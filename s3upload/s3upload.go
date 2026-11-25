// Package s3upload streams a backup archive to S3 (or an S3-compatible store)
// and produces a presigned GET URL. The body is streamed via the SDK's
// multipart manager.Uploader, so a 25-50GB archive never lands on local disk;
// only PartSize*Concurrency bytes are buffered in memory at a time.
//
// NOTE: feature/s3/manager is deprecated upstream in favor of
// feature/s3/transfermanager. manager.Uploader's streaming io.Reader API is
// stable and exactly fits this use case; migrating is a tracked follow-up.
package s3upload

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/spf13/viper"
)

// s3MaxParts is S3's hard cap on multipart parts; part size must leave room for
// the largest expected archive under it.
const s3MaxParts = 10000

// Config is the resolved export.s3 configuration. Credentials are node-local
// (from agent config), never from a job.
type Config struct {
	Endpoint       string
	Region         string
	Bucket         string
	Prefix         string
	AccessKey      string
	SecretKey      string
	ForcePathStyle bool
	PartSizeMB     int
	Concurrency    int
	SSE            string
	DefaultTTL     time.Duration
	MaxTTL         time.Duration
}

// ConfigFromViper reads the backups.export.s3.* keys.
func ConfigFromViper() Config {
	return ConfigFromViperKey("backups.export.s3", "")
}

// ConfigFromViperKey reads <base>.<key> for each setting. For every key except
// prefix, a key unset under base falls back to <fallback>.<key> when fallback
// is non-empty. prefix never falls back.
func ConfigFromViperKey(base, fallback string) Config {
	k := func(key string) string {
		if fallback != "" && key != "prefix" && !viper.IsSet(base+"."+key) {
			return fallback + "." + key
		}
		return base + "." + key
	}
	return Config{
		Endpoint:       viper.GetString(k("endpoint")),
		Region:         viper.GetString(k("region")),
		Bucket:         viper.GetString(k("bucket")),
		Prefix:         viper.GetString(k("prefix")),
		AccessKey:      viper.GetString(k("access_key")),
		SecretKey:      viper.GetString(k("secret_key")),
		ForcePathStyle: viper.GetBool(k("force_path_style")),
		PartSizeMB:     viper.GetInt(k("part_size_mb")),
		Concurrency:    viper.GetInt(k("concurrency")),
		SSE:            viper.GetString(k("sse")),
		DefaultTTL:     time.Duration(viper.GetInt(k("default_ttl_sec"))) * time.Second,
		MaxTTL:         time.Duration(viper.GetInt(k("max_ttl_sec"))) * time.Second,
	}
}

// Enabled reports whether export is configured (a bucket is the activation gate).
func (c Config) Enabled() bool { return c.Bucket != "" }

// Validate checks the config is internally consistent and that the part size
// leaves room for maxArchiveBytes under S3's 10,000-part limit (pass 0 to skip
// that check).
func (c Config) Validate(maxArchiveBytes int64) error {
	if c.Bucket == "" {
		return errors.New("backups.export.s3.bucket is required to enable export")
	}
	if c.Endpoint != "" && !strings.HasPrefix(c.Endpoint, "https://") {
		return fmt.Errorf("backups.export.s3.endpoint must be https, got %q", c.Endpoint)
	}
	if c.PartSizeMB < 5 {
		return fmt.Errorf("backups.export.s3.part_size_mb must be >= 5, got %d", c.PartSizeMB)
	}
	partBytes := int64(c.PartSizeMB) << 20
	if maxArchiveBytes > 0 && partBytes*s3MaxParts < maxArchiveBytes {
		needMB := (maxArchiveBytes + (s3MaxParts<<20 - 1)) / (s3MaxParts << 20)
		return fmt.Errorf("backups.export.s3.part_size_mb=%d too small for a %d-byte archive (S3 %d-part limit); need >= %d", c.PartSizeMB, maxArchiveBytes, s3MaxParts, needMB)
	}
	if c.MaxTTL > 0 && c.DefaultTTL > c.MaxTTL {
		return fmt.Errorf("backups.export.s3.default_ttl_sec (%s) must be <= max_ttl_sec (%s)", c.DefaultTTL, c.MaxTTL)
	}
	return nil
}

// ClampTTL bounds a requested presign TTL: <=0 yields DefaultTTL; anything over
// MaxTTL is capped to MaxTTL.
func (c Config) ClampTTL(req time.Duration) time.Duration {
	if req <= 0 {
		return c.DefaultTTL
	}
	if c.MaxTTL > 0 && req > c.MaxTTL {
		return c.MaxTTL
	}
	return req
}

// Uploader streams objects to a bucket and presigns GETs.
type Uploader struct {
	cfg      Config
	client   *s3.Client
	uploader *manager.Uploader
}

// New builds an Uploader from cfg using static credentials. It does not touch
// the network.
func New(cfg Config) (*Uploader, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("s3upload: bucket not configured")
	}
	awsCfg := aws.Config{
		Region:      cfg.Region,
		Credentials: credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.ForcePathStyle
		// S3-compatible stores (e.g. UpCloud/Dell ECS) reject the SDK's default
		// CRC32 request checksums with XAmzContentSHA256Mismatch; only attach a
		// checksum when the operation requires one, matching legacy S3 behavior.
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})
	uploader := manager.NewUploader(client, func(u *manager.Uploader) {
		if cfg.PartSizeMB > 0 {
			u.PartSize = int64(cfg.PartSizeMB) << 20
		}
		if cfg.Concurrency > 0 {
			u.Concurrency = cfg.Concurrency
		}
		// Abort the multipart upload (clean up parts) on any error, so a failed
		// export never leaves a usable object or orphaned parts.
		u.LeavePartsOnError = false
		// The manager has its OWN checksum setting (defaults to WhenSupported),
		// independent of the client option above. Left at the default it adds CRC32
		// part checksums via an aws-chunked trailer that S3-compatible stores like
		// ECS reject with XAmzContentSHA256Mismatch on UploadPart, so force it to
		// WhenRequired here too.
		u.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
	})
	return &Uploader{cfg: cfg, client: client, uploader: uploader}, nil
}

// objectKey joins the configured prefix with key.
func (u *Uploader) objectKey(key string) string {
	return u.cfg.Prefix + key
}

// Upload streams r to <prefix><key> with server-side encryption and returns the
// number of bytes uploaded. On any read or upload error the multipart upload is
// aborted; the caller must NOT presign a URL for a failed upload.
func (u *Uploader) Upload(ctx context.Context, key string, r io.Reader) (int64, error) {
	cr := &countingReader{r: r}
	in := &s3.PutObjectInput{
		Bucket: aws.String(u.cfg.Bucket),
		Key:    aws.String(u.objectKey(key)),
		Body:   cr,
	}
	if u.cfg.SSE != "" {
		in.ServerSideEncryption = s3types.ServerSideEncryption(u.cfg.SSE)
	}
	_, err := u.uploader.Upload(ctx, in)
	return cr.n, err
}

// PresignGet returns a presigned GET URL for <prefix><key> and the time it
// expires. ttl is clamped to [.., MaxTTL]; <=0 uses DefaultTTL.
func (u *Uploader) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, time.Time, error) {
	ttl = u.cfg.ClampTTL(ttl)
	ps := s3.NewPresignClient(u.client)
	req, err := ps.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(u.cfg.Bucket),
		Key:    aws.String(u.objectKey(key)),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", time.Time{}, err
	}
	return req.URL, time.Now().Add(ttl), nil
}

// Get returns the body of <prefix><key>. The caller must close it.
func (u *Uploader) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := u.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(u.cfg.Bucket),
		Key:    aws.String(u.objectKey(key)),
	})
	if err != nil {
		return nil, err
	}
	return out.Body, nil
}

// List returns every object under <cfg.Prefix><prefix>, following pagination.
// Returned keys have cfg.Prefix stripped.
func (u *Uploader) List(ctx context.Context, prefix string) ([]Object, error) {
	var objs []Object
	p := s3.NewListObjectsV2Paginator(u.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(u.cfg.Bucket),
		Prefix: aws.String(u.objectKey(prefix)),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, o := range page.Contents {
			obj := Object{Key: strings.TrimPrefix(aws.ToString(o.Key), u.cfg.Prefix), Size: aws.ToInt64(o.Size)}
			if o.LastModified != nil {
				obj.LastModified = *o.LastModified
			}
			objs = append(objs, obj)
		}
	}
	return objs, nil
}

// deleteBatchSize is S3's DeleteObjects per-request key limit.
const deleteBatchSize = 1000

// Delete removes the given keys (relative to cfg.Prefix) in batches of up to
// 1000. Per-key failures are aggregated into one error naming the keys. If a
// batch request is rejected outright, that batch is retried one key at a time.
func (u *Uploader) Delete(ctx context.Context, keys []string) error {
	var errs []error
	for start := 0; start < len(keys); start += deleteBatchSize {
		end := min(start+deleteBatchSize, len(keys))
		ids := make([]s3types.ObjectIdentifier, 0, end-start)
		for _, k := range keys[start:end] {
			ids = append(ids, s3types.ObjectIdentifier{Key: aws.String(u.objectKey(k))})
		}
		out, err := u.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(u.cfg.Bucket),
			Delete: &s3types.Delete{Objects: ids, Quiet: aws.Bool(true)},
		})
		if err != nil {
			if ctx.Err() != nil {
				return errors.Join(append(errs, err)...)
			}
			// Some S3-compatible stores reject DeleteObjects without a
			// Content-MD5 header, which the SDK no longer sends. Fall back to
			// one DeleteObject per key for this batch.
			for _, k := range keys[start:end] {
				if _, derr := u.client.DeleteObject(ctx, &s3.DeleteObjectInput{
					Bucket: aws.String(u.cfg.Bucket),
					Key:    aws.String(u.objectKey(k)),
				}); derr != nil {
					if ctx.Err() != nil {
						return errors.Join(append(errs, derr)...)
					}
					errs = append(errs, fmt.Errorf("delete %s: %w", k, derr))
				}
			}
			continue
		}
		for _, e := range out.Errors {
			errs = append(errs, fmt.Errorf("delete %s: %s: %s", strings.TrimPrefix(aws.ToString(e.Key), u.cfg.Prefix), aws.ToString(e.Code), aws.ToString(e.Message)))
		}
	}
	return errors.Join(errs...)
}

// countingReader counts bytes read so the caller can record the uploaded size.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// Object is one entry of a List. Key is relative to the configured Prefix,
// the same form Upload takes.
type Object struct {
	Key          string
	Size         int64
	LastModified time.Time
}
