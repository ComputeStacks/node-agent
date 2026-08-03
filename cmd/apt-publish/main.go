// Command apt-publish syncs the cs-agent apt repository to an S3-compatible
// object store, for the GitHub Actions release pipeline.
//
// It deliberately mirrors the exact S3-compatible client options proven in
// s3upload/s3upload.go — path-style addressing, static credentials, and
// request/response checksums calculated only when required (some S3-compatible
// stores reject the SDK's default CRC32 checksums with XAmzContentSHA256Mismatch).
// That is why we build a plain s3.Client here rather than reaching for the AWS CLI
// or deb-s3, whose checksum behaviour against such stores is the unknown we avoid.
//
// Usage:
//
//	apt-publish pull <local-dir> [--allow-empty]   # download <prefix>pool/** into <local-dir>/pool/**
//	apt-publish push <local-dir>                   # upload <local-dir>/** to <prefix>**, signed Release files LAST
//
// Config via env: APT_S3_ENDPOINT, APT_S3_REGION, APT_S3_BUCKET, APT_S3_PREFIX,
// AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, and optionally APT_PULL_CONCURRENCY.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

const (
	// defaultPullConcurrency bounds in-flight GetObjects. The pool is append-only
	// and gains one .deb per architecture per release, so a serial pull grows with
	// release history for no reason — every object is independent. Override with
	// APT_PULL_CONCURRENCY if the store turns out to dislike parallel reads; that
	// way it is a workflow variable rather than a release.
	defaultPullConcurrency = 8

	// downloadAttempts retries the whole GetObject-and-drain for one object.
	// The SDK's own retryer cannot cover this: a "connection reset by peer"
	// while draining out.Body surfaces AFTER GetObject has already returned
	// success, so the request layer never sees the failure. An unretried reset
	// there is what failed the v3.1.2 apt publish.
	downloadAttempts = 4
)

// downloadTimeout bounds ONE download attempt, and exists for the failure the
// retry loop cannot otherwise see: a stalled stream. A store (or a proxy in front
// of it) that keeps the connection open but stops sending body bytes produces no
// error at all, so io.Copy blocks forever and nothing retries. The SDK's default
// transport sets no response-header or overall timeout, and the pipeline passes a
// context with no deadline, so without this a stall hangs the job until the CI
// runner's own limit — with the GitHub Release already cut, the apt publish not
// done, and the shared concurrency group held so the daily reconcile cannot heal
// the repo either.
//
// Pool objects are single-digit MB, so this is generous headroom rather than a
// performance bound: a 6 MB object would have to average under 20 KB/s to trip it,
// and tripping it costs a retry rather than the release. A var, not a const, only
// so tests can shorten it.
var downloadTimeout = 5 * time.Minute

// retryBaseDelay is the first backoff step; it doubles per attempt. A var, not a
// const, only so tests can drop it to zero instead of sleeping.
var retryBaseDelay = 500 * time.Millisecond

// s3API is the subset of *s3.Client this command uses, so the retry and
// concurrency logic can be exercised against a fake in tests.
type s3API interface {
	s3.ListObjectsV2APIClient
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

type config struct {
	endpoint    string
	region      string
	bucket      string
	prefix      string
	concurrency int
}

func loadConfig() config {
	c := config{
		endpoint: os.Getenv("APT_S3_ENDPOINT"),
		region:   os.Getenv("APT_S3_REGION"),
		bucket:   os.Getenv("APT_S3_BUCKET"),
		prefix:   os.Getenv("APT_S3_PREFIX"),
	}
	if c.bucket == "" {
		log.Fatal("apt-publish: APT_S3_BUCKET is required")
	}
	if c.region == "" {
		c.region = "us-east-1"
	}
	c.prefix = normalizePrefix(c.prefix)
	c.concurrency = pullConcurrency(os.Getenv("APT_PULL_CONCURRENCY"))
	return c
}

// pullConcurrency parses the APT_PULL_CONCURRENCY override. Anything unset,
// unparseable, or below 1 falls back to the default rather than failing the
// release — a bad value should not be the reason a publish cannot run.
func pullConcurrency(env string) int {
	if env == "" {
		return defaultPullConcurrency
	}
	n, err := strconv.Atoi(strings.TrimSpace(env))
	if err != nil || n < 1 {
		log.Printf("apt-publish: ignoring APT_PULL_CONCURRENCY=%q, using %d", env, defaultPullConcurrency)
		return defaultPullConcurrency
	}
	return n
}

// normalizePrefix strips a leading slash and guarantees a single trailing slash
// when non-empty, so prefix+key concatenation is always well formed.
func normalizePrefix(p string) string {
	p = strings.TrimPrefix(p, "/")
	if p != "" && !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

func newClient(c config) *s3.Client {
	awsCfg := aws.Config{
		Region: c.region,
		Credentials: credentials.NewStaticCredentialsProvider(
			os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY"), ""),
	}
	return s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if c.endpoint != "" {
			o.BaseEndpoint = aws.String(c.endpoint)
		}
		o.UsePathStyle = true
		// Mirror s3upload.New: some S3-compatible stores reject the SDK's default CRC32 request checksums.
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})
}

// invocation is a parsed command line.
type invocation struct {
	cmd        string
	dir        string
	allowEmpty bool
}

// parseArgs parses the arguments after the program name. Every rejection is an
// error rather than a default, so a mistyped release command fails loudly instead
// of doing something adjacent to what was meant.
func parseArgs(argv []string) (invocation, error) {
	if len(argv) < 2 {
		return invocation{}, errors.New("usage: apt-publish <pull|push> <local-dir> [--allow-empty]")
	}
	in := invocation{cmd: argv[0], dir: argv[1]}
	if in.cmd != "pull" && in.cmd != "push" {
		return invocation{}, fmt.Errorf("unknown command %q (want pull|push)", in.cmd)
	}
	for _, arg := range argv[2:] {
		switch {
		case arg == "--allow-empty" && in.cmd == "pull":
			in.allowEmpty = true
		case arg == "--allow-empty":
			// Accepting it for push would let a mistyped command look like it did
			// something it cannot do.
			return invocation{}, fmt.Errorf("--allow-empty applies to pull, not %s", in.cmd)
		default:
			return invocation{}, fmt.Errorf("unknown flag %q", arg)
		}
	}
	return in, nil
}

func main() {
	in, err := parseArgs(os.Args[1:])
	if err != nil {
		log.Fatalf("apt-publish: %v", err)
	}
	c := loadConfig()
	client := newClient(c)
	ctx := context.Background()

	switch in.cmd {
	case "pull":
		if err := pull(ctx, client, c, in.dir, in.allowEmpty); err != nil {
			log.Fatalf("apt-publish pull: %v", err)
		}
	case "push":
		if err := push(ctx, client, c, in.dir); err != nil {
			log.Fatalf("apt-publish push: %v", err)
		}
	}
}

// pull downloads <prefix>pool/** into <dir>/pool/** (the full version history).
func pull(ctx context.Context, client s3API, c config, dir string, allowEmpty bool) error {
	poolPrefix := c.prefix + "pool/"
	objects, err := listPool(ctx, client, c.bucket, poolPrefix)
	if err != nil {
		return err
	}
	// A pull that finds nothing must NOT look like a successful pull of an empty
	// pool. The caller rebuilds the index from whatever ends up on disk, so an
	// empty result would publish an index listing only the release being cut and
	// silently drop every previously published version from the repo.
	if len(objects) == 0 && !allowEmpty {
		return fmt.Errorf("no objects under s3://%s/%s: refusing to continue, because rebuilding "+
			"the index from an empty pool would drop every previously published version "+
			"(pass --allow-empty to bootstrap a genuinely new repo)", c.bucket, poolPrefix)
	}
	if err := downloadAll(ctx, client, c, dir, objects); err != nil {
		return err
	}
	log.Printf("apt-publish: pulled %d object(s) from s3://%s/%s", len(objects), c.bucket, poolPrefix)
	return nil
}

// poolObject is one listed object. Size travels with the key so the download can
// check what it received against what the store said was there.
type poolObject struct {
	Key  string
	Size int64
}

// listPool returns every object under poolPrefix.
func listPool(ctx context.Context, client s3API, bucket, poolPrefix string) ([]poolObject, error) {
	p := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(poolPrefix),
	})
	var objects []poolObject
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, obj := range page.Contents {
			key := aws.ToString(obj.Key)
			// Some S3-compatible stores materialise "directories" as zero-byte keys
			// ending in "/". They are not objects to download, and treating one as a
			// file would try to os.Create a path that has to be a directory.
			if strings.HasSuffix(key, "/") {
				continue
			}
			objects = append(objects, poolObject{Key: key, Size: aws.ToInt64(obj.Size)})
		}
	}
	return objects, nil
}

// localPath maps an S3 key to its destination under dir, dropping the configured
// prefix so the layout on disk starts at pool/ (what apt-ftparchive indexes).
//
// It rejects a key that would resolve outside dir. Object keys are data from the
// store, not input we generate, and the pool is planned to be shared with other
// packages' CI — so a key carrying ".." segments must fail the pull rather than
// write somewhere in the runner's filesystem.
func localPath(dir, prefix, key string) (string, error) {
	rel := strings.TrimPrefix(key, prefix) // e.g. pool/main/c/cs-agent/...
	dst := filepath.Join(dir, filepath.FromSlash(rel))
	cleanDir := filepath.Clean(dir)
	if dst != cleanDir && !strings.HasPrefix(dst, cleanDir+string(filepath.Separator)) {
		return "", fmt.Errorf("key %q resolves outside %s", key, cleanDir)
	}
	return dst, nil
}

// downloadAll fetches objects with bounded concurrency. The first failure cancels
// the rest and is returned: a partial pool must never reach the index builder.
func downloadAll(ctx context.Context, client s3API, c config, dir string, objects []poolObject) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	limit := c.concurrency
	if limit < 1 {
		limit = defaultPullConcurrency
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	fail := func(key string, err error) {
		mu.Lock()
		defer mu.Unlock()
		if firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", key, err)
			cancel() // stop the siblings; the whole pull is doomed either way
		}
	}

	for _, obj := range objects {
		if ctx.Err() != nil {
			break
		}
		dst, err := localPath(dir, c.prefix, obj.Key)
		if err != nil {
			fail(obj.Key, err)
			break
		}
		sem <- struct{}{} // every goroutine releases its slot, so this cannot deadlock
		wg.Add(1)
		go func(obj poolObject, dst string) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := downloadWithRetry(ctx, client, c.bucket, obj, dst); err != nil {
				fail(obj.Key, err)
			}
		}(obj, dst)
	}
	wg.Wait()
	return firstErr
}

// downloadWithRetry retries downloadOne with exponential backoff. See
// downloadAttempts for why the SDK's retryer is not enough here, and
// downloadTimeout for why each attempt gets its own deadline.
func downloadWithRetry(ctx context.Context, client s3API, bucket string, obj poolObject, dst string) error {
	var lastErr error
	for attempt := 1; attempt <= downloadAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		lastErr = func() error {
			attemptCtx, cancelAttempt := context.WithTimeout(ctx, downloadTimeout)
			defer cancelAttempt()
			return downloadOne(attemptCtx, client, bucket, obj, dst)
		}()
		if lastErr == nil {
			return nil
		}
		// A rotated key or a genuinely missing object cannot be fixed by waiting;
		// retrying it four times only delays a failure the operator has to act on.
		if permanent, why := permanentFailure(lastErr); permanent {
			return fmt.Errorf("not retryable (%s): %w", why, lastErr)
		}
		if attempt == downloadAttempts {
			break
		}
		delay := jitter(backoffDelay(attempt))
		log.Printf("apt-publish: %s: attempt %d/%d failed (%v); retrying in %s",
			obj.Key, attempt, downloadAttempts, lastErr, delay)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return fmt.Errorf("after %d attempt(s): %w", downloadAttempts, lastErr)
}

// permanentFailure reports whether an error is one that retrying cannot fix, and
// why. Only client errors are treated as permanent, and 408/429 are excluded
// because those two 4xx codes DO mean "try again".
//
// The status code is read through an anonymous interface rather than the SDK's
// concrete error type, so this file needs no direct dependency on smithy-go.
// GoReleaser runs `go mod tidy` before building, and promoting an indirect
// dependency here would leave go.mod modified mid-release — a dirty tree, which
// GoReleaser refuses to build from.
func permanentFailure(err error) (bool, string) {
	var statusErr interface{ HTTPStatusCode() int }
	if !errors.As(err, &statusErr) {
		return false, ""
	}
	code := statusErr.HTTPStatusCode()
	if code < 400 || code >= 500 {
		return false, ""
	}
	switch code {
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return false, ""
	}
	return true, fmt.Sprintf("HTTP %d", code)
}

// backoffDelay doubles retryBaseDelay per attempt: 500ms, 1s, 2s, ...
func backoffDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	return retryBaseDelay << (attempt - 1)
}

// jitter spreads a delay across [d/2, d].
//
// This matters specifically because downloads now run concurrently: whatever
// makes one of them fail — the store shedding load, a connection cap, a network
// blip — tends to hit all of the in-flight ones at the same moment. An unjittered
// backoff would then have every worker sleep for an identical interval and retry
// in lockstep, reproducing the very burst that caused the failure, on a schedule.
// Half-width rather than full-width jitter, so the backoff still grows.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
}

// downloadOne writes to a sibling .part file and renames only after a complete
// copy, so any file present under dir is a whole object. The index publishes each
// pool file's own size and hashes, so a truncated .deb would otherwise be indexed
// as if it were the real thing — self-consistent, hash-valid, and broken on
// install, in a way the daily reconcile cannot detect because it re-derives the
// index from the same bad bytes.
//
// "Complete" is checked against the size the listing reported rather than trusted
// from io.Copy returning nil, so a short body that arrives without a transport
// error is still caught.
func downloadOne(ctx context.Context, client s3API, bucket string, obj poolObject, dst string) error {
	out, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(obj.Key),
	})
	if err != nil {
		return err
	}
	defer out.Body.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, out.Body)
	if err == nil && obj.Size > 0 && n != obj.Size {
		err = fmt.Errorf("short object: copied %d byte(s), listing reported %d", n, obj.Size)
	}
	if err == nil {
		err = f.Sync() // the rename must not publish a name whose bytes aren't on disk
	}
	if err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// push uploads <dir>/** to <prefix>**. The signed Release files (Release,
// Release.gpg, InRelease) go LAST so a client mid-update never sees a Release
// referencing a Packages index that hasn't been uploaded yet. (For fully atomic
// updates, enable by-hash in build-apt-repo.sh — tracked follow-up.)
func push(ctx context.Context, client s3API, c config, dir string) error {
	var files []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	files = orderForUpload(files)

	for _, path := range files {
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		key := c.prefix + filepath.ToSlash(rel)
		if err := uploadOne(ctx, client, c.bucket, key, path); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
	}
	log.Printf("apt-publish: pushed %d file(s) to s3://%s/%s", len(files), c.bucket, c.prefix)
	return nil
}

func isReleaseMeta(p string) bool {
	switch filepath.Base(p) {
	case "Release", "Release.gpg", "InRelease":
		return true
	}
	return false
}

// orderForUpload sorts non-Release-meta files first and the signed Release files
// last, lexically within each group. Returns the same slice, reordered.
func orderForUpload(files []string) []string {
	sort.SliceStable(files, func(i, j int) bool {
		ri, rj := isReleaseMeta(files[i]), isReleaseMeta(files[j])
		if ri != rj {
			return !ri
		}
		return files[i] < files[j]
	})
	return files
}

// uploadOne PUTs a single file. Unlike the download path this needs no retry
// wrapper: the body is a seekable *os.File, so the SDK's retryer can rewind it
// and does cover a reset mid-upload.
func uploadOne(ctx context.Context, client s3API, bucket, key, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   f, // *os.File is seekable, so the SDK sets Content-Length without buffering
	})
	return err
}
