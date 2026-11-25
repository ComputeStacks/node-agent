package s3upload

import (
	"context"
	"fmt"
	"github.com/spf13/viper"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConfigValidate(t *testing.T) {
	base := Config{Bucket: "b", PartSizeMB: 64, DefaultTTL: 6 * time.Hour, MaxTTL: 24 * time.Hour}
	if err := base.Validate(50 << 30); err != nil { // 50 GB fits in 64MB*10000
		t.Errorf("valid config rejected: %v", err)
	}

	c := base
	c.Bucket = ""
	if err := c.Validate(0); err == nil {
		t.Error("expected error when bucket is empty")
	}

	c = base
	c.Endpoint = "http://minio:9000"
	if err := c.Validate(0); err == nil {
		t.Error("expected error for non-https endpoint")
	}
	c.Endpoint = "https://minio:9000"
	if err := c.Validate(0); err != nil {
		t.Errorf("https endpoint should be accepted: %v", err)
	}

	c = base
	c.PartSizeMB = 5 // 5MiB*10000 ≈ 48.8GiB < 60GiB
	if err := c.Validate(60 << 30); err == nil {
		t.Error("expected part-size-floor error for a 60GB archive at 5MB parts")
	}

	c = base
	c.PartSizeMB = 4
	if err := c.Validate(0); err == nil {
		t.Error("expected error for part_size_mb < 5")
	}

	c = base
	c.DefaultTTL = 48 * time.Hour
	if err := c.Validate(0); err == nil {
		t.Error("expected error when default_ttl > max_ttl")
	}
}

func TestClampTTL(t *testing.T) {
	c := Config{DefaultTTL: 6 * time.Hour, MaxTTL: 24 * time.Hour}
	if got := c.ClampTTL(0); got != 6*time.Hour {
		t.Errorf("unset ttl: got %s, want 6h", got)
	}
	if got := c.ClampTTL(-time.Second); got != 6*time.Hour {
		t.Errorf("negative ttl: got %s, want 6h", got)
	}
	if got := c.ClampTTL(2 * time.Hour); got != 2*time.Hour {
		t.Errorf("in-range ttl: got %s, want 2h", got)
	}
	if got := c.ClampTTL(48 * time.Hour); got != 24*time.Hour {
		t.Errorf("over-max ttl: got %s, want 24h (capped)", got)
	}
}

// PresignGet signs locally (no network), so we can assert the URL shape.
func TestPresignGet(t *testing.T) {
	u, err := New(Config{
		Endpoint:       "https://s3.example.com",
		Region:         "us-east-1",
		Bucket:         "mybucket",
		Prefix:         "exports/",
		AccessKey:      "AKIAEXAMPLE",
		SecretKey:      "secret",
		ForcePathStyle: true,
		PartSizeMB:     64,
		Concurrency:    4,
		DefaultTTL:     6 * time.Hour,
		MaxTTL:         24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	url, expiry, err := u.PresignGet(context.Background(), "vol-1/archive.tar", time.Hour)
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}
	if !strings.Contains(url, "mybucket") || !strings.Contains(url, "exports/vol-1/archive.tar") {
		t.Errorf("presigned URL missing bucket/key: %s", url)
	}
	if !strings.Contains(url, "X-Amz-Expires=3600") {
		t.Errorf("presigned URL missing 1h expiry: %s", url)
	}
	if expiry.IsZero() {
		t.Error("expected a non-zero expiry time")
	}
}

func TestConfigFromViperKeyFallback(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	viper.Set("backups.export.s3.endpoint", "https://e")
	viper.Set("backups.export.s3.bucket", "exp")
	viper.Set("backups.export.s3.prefix", "exp/")
	viper.Set("backups.export.s3.force_path_style", true)
	viper.Set("backups.export.s3.part_size_mb", 64)
	viper.Set("store.backup.s3.bucket", "own")
	viper.Set("store.backup.s3.force_path_style", false)

	c := ConfigFromViperKey("store.backup.s3", "backups.export.s3")
	if c.Bucket != "own" {
		t.Errorf("set key overridden: %q", c.Bucket)
	}
	if c.Endpoint != "https://e" || c.PartSizeMB != 64 {
		t.Errorf("unset keys did not fall back: %+v", c)
	}
	if c.ForcePathStyle {
		t.Error("explicit false force_path_style was overridden by fallback")
	}
	if c.Prefix != "" {
		t.Errorf("prefix fell back: %q", c.Prefix)
	}

	e := ConfigFromViper()
	if e.Bucket != "exp" || e.Prefix != "exp/" || !e.ForcePathStyle || e.Endpoint != "https://e" {
		t.Errorf("ConfigFromViper changed: %+v", e)
	}
}

type fakeS3 struct {
	mu          sync.Mutex
	gotKeys     []string
	listQ       []url.Values
	delBodies   []string
	delHdrs     []http.Header
	rejectMulti bool
	singleDel   []string
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/bkt/") && !q.Has("list-type"):
		f.gotKeys = append(f.gotKeys, strings.TrimPrefix(r.URL.Path, "/bkt/"))
		_, _ = w.Write([]byte("hello"))
	case r.Method == http.MethodGet && q.Get("list-type") == "2":
		f.listQ = append(f.listQ, q)
		w.Header().Set("Content-Type", "application/xml")
		if q.Get("continuation-token") == "" {
			_, _ = w.Write([]byte(`<ListBucketResult><IsTruncated>true</IsTruncated><NextContinuationToken>tok2</NextContinuationToken>` +
				`<Contents><Key>pfx/a/1</Key><Size>3</Size></Contents></ListBucketResult>`))
		} else {
			_, _ = w.Write([]byte(`<ListBucketResult><IsTruncated>false</IsTruncated>` +
				`<Contents><Key>pfx/a/2</Key><Size>5</Size></Contents></ListBucketResult>`))
		}
	case r.Method == http.MethodPost && q.Has("delete") && f.rejectMulti:
		http.Error(w, "<Error><Code>InvalidRequest</Code><Message>Missing required header: Content-MD5</Message></Error>", 400)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/bkt/"):
		f.singleDel = append(f.singleDel, strings.TrimPrefix(r.URL.Path, "/bkt/"))
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && q.Has("delete"):
		b, _ := io.ReadAll(r.Body)
		f.delBodies = append(f.delBodies, string(b))
		f.delHdrs = append(f.delHdrs, r.Header.Clone())
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<DeleteResult></DeleteResult>`))
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.String(), 400)
	}
}

func newFake(t *testing.T) (*Uploader, *fakeS3) {
	t.Helper()
	f := &fakeS3{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	u, err := New(Config{Endpoint: srv.URL, Region: "us-east-1", Bucket: "bkt", Prefix: "pfx/", AccessKey: "a", SecretKey: "s", ForcePathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	return u, f
}

func TestGet(t *testing.T) {
	u, f := newFake(t)
	rc, err := u.Get(context.Background(), "x/y")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	if string(b) != "hello" || len(f.gotKeys) != 1 || f.gotKeys[0] != "pfx/x/y" {
		t.Errorf("body=%q keys=%v", b, f.gotKeys)
	}
}

func TestListPaginationAndStrip(t *testing.T) {
	u, f := newFake(t)
	objs, err := u.List(context.Background(), "a/")
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 2 || objs[0].Key != "a/1" || objs[1].Key != "a/2" || objs[1].Size != 5 {
		t.Errorf("objs=%+v", objs)
	}
	if len(f.listQ) != 2 || f.listQ[0].Get("prefix") != "pfx/a/" || f.listQ[1].Get("continuation-token") != "tok2" {
		t.Errorf("queries=%v", f.listQ)
	}
}

func TestDeleteBatching(t *testing.T) {
	u, f := newFake(t)
	keys := make([]string, 2500)
	for i := range keys {
		keys[i] = fmt.Sprintf("k/%d", i)
	}
	if err := u.Delete(context.Background(), keys); err != nil {
		t.Fatal(err)
	}
	if len(f.delBodies) != 3 {
		t.Fatalf("batches=%d", len(f.delBodies))
	}
	if n := strings.Count(f.delBodies[0], "<Key>"); n != 1000 {
		t.Errorf("first batch %d keys", n)
	}
	if n := strings.Count(f.delBodies[2], "<Key>"); n != 500 {
		t.Errorf("last batch %d keys", n)
	}
	if !strings.Contains(f.delBodies[0], "<Key>pfx/k/0</Key>") {
		t.Error("key not prefixed")
	}
	h := f.delHdrs[0]
	t.Logf("delete checksum headers: md5=%q crc32=%q sha256=%q", h.Get("Content-Md5"), h.Get("X-Amz-Checksum-Crc32"), h.Get("X-Amz-Checksum-Sha256"))
	if h.Get("Content-Md5") == "" && h.Get("X-Amz-Checksum-Crc32") == "" {
		t.Error("DeleteObjects sent no checksum")
	}
	if err := u.Delete(context.Background(), nil); err != nil || len(f.delBodies) != 3 {
		t.Error("empty delete should be a no-op")
	}
}

func TestDeleteFallsBackToSingleDeletes(t *testing.T) {
	u, f := newFake(t)
	f.rejectMulti = true
	if err := u.Delete(context.Background(), []string{"a/1", "a/2"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.singleDel, ","); got != "pfx/a/1,pfx/a/2" {
		t.Fatalf("single deletes = %q", got)
	}
}
