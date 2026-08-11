package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// errReset stands in for the mid-stream failure that broke the v3.1.2 publish:
// the body starts fine and dies partway through the copy.
var errReset = errors.New("read tcp 10.0.0.1:443: read: connection reset by peer")

// bodyPlan describes what one GetObject attempt should do.
type bodyPlan struct {
	getErr    error  // fail the GetObject call itself
	data      string // bytes the body yields
	failAfter int    // >0: yield this many bytes then return errReset
	stall     bool   // body blocks forever, like a stream that stops sending
}

// stallBody never yields a byte and never errors on its own; it unblocks only
// when the request context is cancelled, which is how the real SDK's body
// behaves. This is the failure shape that produces no error for retry to see.
type stallBody struct{ ctx context.Context }

func (b *stallBody) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *stallBody) Close() error { return nil }

// truncatedBody yields data[:failAfter] and then fails, modelling a body whose
// read dies after the response headers were already accepted.
type truncatedBody struct {
	r         io.Reader
	remaining int
}

func (b *truncatedBody) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		return 0, errReset
	}
	if len(p) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.r.Read(p)
	b.remaining -= n
	return n, err
}

func (b *truncatedBody) Close() error { return nil }

// fakeS3 serves a scripted sequence of attempts per key.
type fakeS3 struct {
	mu       sync.Mutex
	plans    map[string][]bodyPlan // consumed front to back; last entry repeats
	attempts map[string]int
	listed   []string
	sizes    map[string]int64
	listErr  error
	pageSize int
	puts     []string
	inFlight int32
	maxSeen  int32

	// barrier, when > 0, holds every GetObject until that many are simultaneously
	// in flight. A serial download loop can never satisfy it, so the concurrency
	// test fails loudly instead of relying on goroutines happening to overlap.
	barrier  int
	gate     chan struct{}
	gateOnce sync.Once
}

func (f *fakeS3) GetObject(ctx context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	n := atomic.AddInt32(&f.inFlight, 1)
	for {
		seen := atomic.LoadInt32(&f.maxSeen)
		if n <= seen || atomic.CompareAndSwapInt32(&f.maxSeen, seen, n) {
			break
		}
	}
	defer atomic.AddInt32(&f.inFlight, -1)

	if f.barrier > 0 {
		if int(n) >= f.barrier {
			f.gateOnce.Do(func() { close(f.gate) })
		}
		select {
		case <-f.gate:
		case <-time.After(5 * time.Second):
			return nil, fmt.Errorf("only %d download(s) in flight after 5s; want %d concurrent", n, f.barrier)
		}
	}

	key := aws.ToString(in.Key)
	f.mu.Lock()
	attempt := f.attempts[key]
	f.attempts[key] = attempt + 1
	plans := f.plans[key]
	f.mu.Unlock()

	if len(plans) == 0 {
		return nil, fmt.Errorf("fakeS3: no plan for key %q", key)
	}
	p := plans[min(attempt, len(plans)-1)]
	if p.getErr != nil {
		return nil, p.getErr
	}
	var body io.ReadCloser = io.NopCloser(strings.NewReader(p.data))
	if p.stall {
		return &s3.GetObjectOutput{Body: &stallBody{ctx: ctx}}, nil
	}
	if p.failAfter > 0 {
		body = &truncatedBody{r: strings.NewReader(p.data), remaining: p.failAfter}
	}
	return &s3.GetObjectOutput{Body: body}, nil
}

func (f *fakeS3) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	start := 0
	if tok := aws.ToString(in.ContinuationToken); tok != "" {
		if _, err := fmt.Sscanf(tok, "%d", &start); err != nil {
			return nil, fmt.Errorf("fakeS3: bad continuation token %q", tok)
		}
	}
	size := f.pageSize
	if size <= 0 {
		size = len(f.listed)
	}
	end := min(start+size, len(f.listed))
	out := &s3.ListObjectsV2Output{}
	for _, k := range f.listed[start:end] {
		o := types.Object{Key: aws.String(k)}
		if sz, ok := f.sizes[k]; ok {
			o.Size = aws.Int64(sz)
		}
		out.Contents = append(out.Contents, o)
	}
	if end < len(f.listed) {
		out.IsTruncated = aws.Bool(true)
		out.NextContinuationToken = aws.String(fmt.Sprintf("%d", end))
	}
	return out, nil
}

func (f *fakeS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts = append(f.puts, aws.ToString(in.Key))
	return &s3.PutObjectOutput{}, nil
}

func newFake() *fakeS3 {
	return &fakeS3{plans: map[string][]bodyPlan{}, attempts: map[string]int{}, sizes: map[string]int64{}, gate: make(chan struct{})}
}

// fastTimeout shrinks the per-attempt stall ceiling so a stall test finishes fast.
func fastTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	orig := downloadTimeout
	downloadTimeout = d
	t.Cleanup(func() { downloadTimeout = orig })
}

// fastRetries shrinks the backoff so the retry tests don't sleep for seconds.
func fastRetries(t *testing.T) {
	t.Helper()
	orig := retryBaseDelay
	retryBaseDelay = 0
	t.Cleanup(func() { retryBaseDelay = orig })
}

func TestNormalizePrefix(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"public", "public/"},
		{"public/", "public/"},
		{"/public", "public/"},
		{"/public/", "public/"},
		{"/a/b", "a/b/"},
	} {
		if got := normalizePrefix(tc.in); got != tc.want {
			t.Errorf("normalizePrefix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestLocalPathStripsThePrefix(t *testing.T) {
	got, err := localPath("/work/aptrepo", "public/", "public/pool/main/c/cs-agent/cs-agent_3.1.2_amd64.deb")
	if err != nil {
		t.Fatalf("localPath: %v", err)
	}
	want := filepath.Join("/work/aptrepo", "pool", "main", "c", "cs-agent", "cs-agent_3.1.2_amd64.deb")
	if got != want {
		t.Errorf("localPath = %q, want %q", got, want)
	}
	// An empty prefix must still land under dir at pool/.
	got, err = localPath("/work/aptrepo", "", "pool/main/c/cs-agent/x.deb")
	if err != nil {
		t.Fatalf("localPath: %v", err)
	}
	want = filepath.Join("/work/aptrepo", "pool", "main", "c", "cs-agent", "x.deb")
	if got != want {
		t.Errorf("localPath(empty prefix) = %q, want %q", got, want)
	}
}

// "apt-publish pull ." is the natural local invocation, and a bare relative dir is
// the case a joined-path prefix test gets wrong: filepath.Join cleans the "./" away,
// so every ordinary key looked like an escape and the whole pull failed.
func TestLocalPathAcceptsARelativeDirectory(t *testing.T) {
	for _, dir := range []string{".", "", "./", "aptrepo", "./aptrepo"} {
		got, err := localPath(dir, "public/", "public/pool/main/c/cs-agent/cs-agent_3.1.2_amd64.deb")
		if err != nil {
			t.Errorf("localPath(%q): %v", dir, err)
			continue
		}
		want := filepath.Join(dir, "pool", "main", "c", "cs-agent", "cs-agent_3.1.2_amd64.deb")
		if got != want {
			t.Errorf("localPath(%q) = %q, want %q", dir, got, want)
		}
	}
}

// Object keys come from the store, so a key with ".." must fail the pull rather
// than write outside the working directory.
func TestLocalPathRejectsAKeyThatEscapesTheDirectory(t *testing.T) {
	// Every dir shape, including the relative ones: the containment check must not
	// have been loosened into an accept-anything test to make "." work.
	for _, dir := range []string{"/work/aptrepo", ".", "", "./", "aptrepo", "./aptrepo"} {
		for _, key := range []string{
			"public/pool/../../../etc/cron.d/x",
			"public/pool/../../outside.deb",
			"public/../secrets",
			"public/..",
		} {
			if got, err := localPath(dir, "public/", key); err == nil {
				t.Errorf("localPath(%q, %q) = %q, want an error", dir, key, got)
			}
		}
	}
	// A key that merely contains ".." inside a segment name is fine.
	for _, dir := range []string{"/work/aptrepo", ".", "aptrepo"} {
		if _, err := localPath(dir, "public/", "public/pool/a..b.deb"); err != nil {
			t.Errorf("localPath(%q) on a harmless '..' substring: %v", dir, err)
		}
	}
}

// Pinned to literal durations, not to retryBaseDelay: an assertion written in
// terms of the constant passes even if the constant became 0, which is exactly the
// regression that would silently turn the backoff into a hot retry loop.
func TestBackoffDelayDoubles(t *testing.T) {
	for _, tc := range []struct {
		attempt int
		want    time.Duration
	}{
		{0, 500 * time.Millisecond}, // guard the shift against 0 or less
		{1, 500 * time.Millisecond},
		{2, 1 * time.Second},
		{3, 2 * time.Second},
		{4, 4 * time.Second},
	} {
		if got := backoffDelay(tc.attempt); got != tc.want {
			t.Errorf("backoffDelay(%d) = %v, want %v", tc.attempt, got, tc.want)
		}
	}
}

// The retry budget and the stall ceiling are both load-bearing values agreed with
// the pipeline's behaviour; pin them so a casual edit has to be deliberate.
func TestRetryConstants(t *testing.T) {
	if downloadAttempts != 4 {
		t.Errorf("downloadAttempts = %d, want 4", downloadAttempts)
	}
	if retryBaseDelay != 500*time.Millisecond {
		t.Errorf("retryBaseDelay = %v, want 500ms", retryBaseDelay)
	}
	if downloadTimeout != 5*time.Minute {
		t.Errorf("downloadTimeout = %v, want 5m", downloadTimeout)
	}
}

func TestOrderForUploadPutsReleaseMetaLast(t *testing.T) {
	in := []string{
		"d/dists/stable/InRelease",
		"d/pool/main/c/cs-agent/b.deb",
		"d/dists/stable/Release",
		"d/dists/stable/main/binary-amd64/Packages",
		"d/dists/stable/Release.gpg",
		"d/pool/main/c/cs-agent/a.deb",
	}
	got := orderForUpload(in)

	firstMeta := len(got)
	for i, p := range got {
		if isReleaseMeta(p) {
			firstMeta = i
			break
		}
	}
	for i, p := range got {
		if i < firstMeta && isReleaseMeta(p) {
			t.Fatalf("release meta %q at %d precedes a non-meta file", p, i)
		}
		if i >= firstMeta && !isReleaseMeta(p) {
			t.Fatalf("non-meta %q at %d follows release meta", p, i)
		}
	}
	// Packages must be uploaded before the Release that hashes it.
	if firstMeta != 3 {
		t.Fatalf("expected 3 non-meta files first, got %d: %v", firstMeta, got)
	}
	if !sort.StringsAreSorted(got[:firstMeta]) || !sort.StringsAreSorted(got[firstMeta:]) {
		t.Errorf("each group should be lexical: %v", got)
	}
}

func TestIsReleaseMetaMatchesOnlyTheSignedIndexFiles(t *testing.T) {
	for _, p := range []string{"d/Release", "d/Release.gpg", "d/InRelease"} {
		if !isReleaseMeta(p) {
			t.Errorf("isReleaseMeta(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"d/Packages", "d/Packages.gz", "d/a.deb", "d/Releases", "d/InRelease.old"} {
		if isReleaseMeta(p) {
			t.Errorf("isReleaseMeta(%q) = true, want false", p)
		}
	}
}

func TestDownloadOneRenamesOnlyAfterACleanCopy(t *testing.T) {
	dir := t.TempDir()
	f := newFake()
	f.plans["k"] = []bodyPlan{{data: "the whole deb"}}
	dst := filepath.Join(dir, "sub", "out.deb")

	if err := downloadOne(context.Background(), f, "b", poolObject{Key: "k"}, dst); err != nil {
		t.Fatalf("downloadOne: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("reading dst: %v", err)
	}
	if string(got) != "the whole deb" {
		t.Errorf("dst = %q, want %q", got, "the whole deb")
	}
	if _, err := os.Stat(dst + ".part"); !os.IsNotExist(err) {
		t.Errorf(".part file should be gone, stat err = %v", err)
	}
}

// The critical property: a body that dies partway must NOT leave a short file
// where the index builder would find it, because apt-ftparchive would publish
// its size and hashes as if it were the real .deb.
func TestDownloadOneLeavesNoFileWhenTheBodyDies(t *testing.T) {
	dir := t.TempDir()
	f := newFake()
	f.plans["k"] = []bodyPlan{{data: "0123456789", failAfter: 4}}
	dst := filepath.Join(dir, "out.deb")

	err := downloadOne(context.Background(), f, "b", poolObject{Key: "k"}, dst)
	if !errors.Is(err, errReset) {
		t.Fatalf("downloadOne err = %v, want errReset", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("dst must not exist after a torn read, stat err = %v", err)
	}
	if _, err := os.Stat(dst + ".part"); !os.IsNotExist(err) {
		t.Errorf(".part must be cleaned up, stat err = %v", err)
	}
}

func TestDownloadWithRetryRecoversFromATornBody(t *testing.T) {
	fastRetries(t)
	dir := t.TempDir()
	f := newFake()
	// Attempt 1 dies mid-stream (the v3.1.2 failure), attempt 2 succeeds.
	f.plans["k"] = []bodyPlan{
		{data: "complete contents", failAfter: 5},
		{data: "complete contents"},
	}
	dst := filepath.Join(dir, "out.deb")

	if err := downloadWithRetry(context.Background(), f, "b", poolObject{Key: "k"}, dst); err != nil {
		t.Fatalf("downloadWithRetry: %v", err)
	}
	if f.attempts["k"] != 2 {
		t.Errorf("attempts = %d, want 2", f.attempts["k"])
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("reading dst: %v", err)
	}
	if string(got) != "complete contents" {
		t.Errorf("dst = %q, want the full contents", got)
	}
}

func TestDownloadWithRetryAlsoRetriesAFailedGetObject(t *testing.T) {
	fastRetries(t)
	dir := t.TempDir()
	f := newFake()
	f.plans["k"] = []bodyPlan{{getErr: errReset}, {data: "ok"}}

	if err := downloadWithRetry(context.Background(), f, "b", poolObject{Key: "k"}, filepath.Join(dir, "o")); err != nil {
		t.Fatalf("downloadWithRetry: %v", err)
	}
	if f.attempts["k"] != 2 {
		t.Errorf("attempts = %d, want 2", f.attempts["k"])
	}
}

func TestDownloadWithRetryGivesUpAndReportsTheLastError(t *testing.T) {
	fastRetries(t)
	dir := t.TempDir()
	f := newFake()
	f.plans["k"] = []bodyPlan{{data: "x", failAfter: 0, getErr: errReset}}

	err := downloadWithRetry(context.Background(), f, "b", poolObject{Key: "k"}, filepath.Join(dir, "o"))
	if err == nil {
		t.Fatal("expected an error after exhausting attempts")
	}
	if !errors.Is(err, errReset) {
		t.Errorf("err should wrap the underlying failure, got %v", err)
	}
	if f.attempts["k"] != downloadAttempts {
		t.Errorf("attempts = %d, want %d", f.attempts["k"], downloadAttempts)
	}
}

func TestDownloadWithRetryStopsOnACancelledContext(t *testing.T) {
	fastRetries(t)
	dir := t.TempDir()
	f := newFake()
	f.plans["k"] = []bodyPlan{{getErr: errReset}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := downloadWithRetry(ctx, f, "b", poolObject{Key: "k"}, filepath.Join(dir, "o")); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if f.attempts["k"] != 0 {
		t.Errorf("attempts = %d, want 0 (must not start work under a dead context)", f.attempts["k"])
	}
}

// The guard that matters most: an empty listing must abort, not silently rebuild
// the index from a pool holding only the release being cut.
func TestPullRefusesAnEmptyPool(t *testing.T) {
	f := newFake()
	err := pull(context.Background(), f, config{bucket: "b", prefix: "public/"}, t.TempDir(), false)
	if err == nil {
		t.Fatal("pull must fail on an empty pool")
	}
	if !strings.Contains(err.Error(), "refusing to continue") {
		t.Errorf("err should explain the refusal, got %v", err)
	}
}

func TestPullAllowsAnEmptyPoolWhenBootstrapping(t *testing.T) {
	f := newFake()
	if err := pull(context.Background(), f, config{bucket: "b", prefix: "public/"}, t.TempDir(), true); err != nil {
		t.Errorf("pull --allow-empty on an empty pool: %v", err)
	}
}

func TestPullDownloadsEveryListedObject(t *testing.T) {
	fastRetries(t) // if the barrier ever trips, fail promptly instead of after 4 backoffs
	dir := t.TempDir()
	f := newFake()
	f.pageSize = 3                     // force pagination
	f.barrier = defaultPullConcurrency // prove the downloads really run in parallel
	for i := 0; i < 10; i++ {
		key := fmt.Sprintf("public/pool/main/c/cs-agent/v%d.deb", i)
		f.listed = append(f.listed, key)
		f.plans[key] = []bodyPlan{{data: fmt.Sprintf("deb-%d", i)}}
	}

	if err := pull(context.Background(), f, config{bucket: "b", prefix: "public/"}, dir, false); err != nil {
		t.Fatalf("pull: %v", err)
	}
	for i := 0; i < 10; i++ {
		p := filepath.Join(dir, "pool", "main", "c", "cs-agent", fmt.Sprintf("v%d.deb", i))
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("missing %s: %v", p, err)
		}
		if want := fmt.Sprintf("deb-%d", i); string(got) != want {
			t.Errorf("%s = %q, want %q", p, got, want)
		}
	}
	if f.maxSeen != defaultPullConcurrency {
		t.Errorf("peak concurrency = %d, want exactly the %d-slot cap", f.maxSeen, defaultPullConcurrency)
	}
}

func TestPullFailsWhenAnyObjectCannotBeFetched(t *testing.T) {
	fastRetries(t)
	dir := t.TempDir()
	f := newFake()
	for i := 0; i < 4; i++ {
		key := fmt.Sprintf("public/pool/v%d.deb", i)
		f.listed = append(f.listed, key)
		f.plans[key] = []bodyPlan{{data: "fine"}}
	}
	f.plans["public/pool/v2.deb"] = []bodyPlan{{getErr: errReset}}

	err := pull(context.Background(), f, config{bucket: "b", prefix: "public/"}, dir, false)
	if err == nil {
		t.Fatal("pull must fail when an object cannot be fetched")
	}
	if !strings.Contains(err.Error(), "v2.deb") {
		t.Errorf("err should name the failing key, got %v", err)
	}
	// The doomed object must not be left on disk in any form.
	if _, statErr := os.Stat(filepath.Join(dir, "pool", "v2.deb")); !os.IsNotExist(statErr) {
		t.Errorf("failed object should not exist, stat err = %v", statErr)
	}
}

func TestPullSkipsDirectoryMarkerKeys(t *testing.T) {
	dir := t.TempDir()
	f := newFake()
	f.listed = []string{"public/pool/", "public/pool/main/", "public/pool/main/a.deb"}
	f.plans["public/pool/main/a.deb"] = []bodyPlan{{data: "deb"}}

	if err := pull(context.Background(), f, config{bucket: "b", prefix: "public/"}, dir, false); err != nil {
		t.Fatalf("pull: %v", err)
	}
	if got := f.attempts["public/pool/"]; got != 0 {
		t.Errorf("marker key was fetched %d time(s), want 0", got)
	}
	if _, err := os.ReadFile(filepath.Join(dir, "pool", "main", "a.deb")); err != nil {
		t.Errorf("the real object should still be downloaded: %v", err)
	}
}

// A pool holding nothing but directory markers is empty for indexing purposes,
// so the guard must still fire.
func TestPullRefusesAPoolOfOnlyMarkers(t *testing.T) {
	f := newFake()
	f.listed = []string{"public/pool/", "public/pool/main/"}
	err := pull(context.Background(), f, config{bucket: "b", prefix: "public/"}, t.TempDir(), false)
	if err == nil || !strings.Contains(err.Error(), "refusing to continue") {
		t.Errorf("err = %v, want the empty-pool refusal", err)
	}
}

func TestPullPropagatesAListingError(t *testing.T) {
	f := newFake()
	f.listErr = errors.New("boom")
	if err := pull(context.Background(), f, config{bucket: "b"}, t.TempDir(), false); err == nil {
		t.Fatal("expected the listing error to surface")
	}
}

func TestPushUploadsEverythingWithReleaseMetaLast(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("pool/main/c/cs-agent/a.deb", "a")
	write("dists/stable/main/binary-amd64/Packages", "pkgs")
	write("dists/stable/Release", "rel")
	write("dists/stable/InRelease", "inrel")

	f := newFake()
	if err := push(context.Background(), f, config{bucket: "b", prefix: "public/"}, dir); err != nil {
		t.Fatalf("push: %v", err)
	}
	want := []string{
		"public/dists/stable/main/binary-amd64/Packages",
		"public/pool/main/c/cs-agent/a.deb",
		"public/dists/stable/InRelease",
		"public/dists/stable/Release",
	}
	if len(f.puts) != len(want) {
		t.Fatalf("pushed %v, want %v", f.puts, want)
	}
	for i := range want {
		if f.puts[i] != want[i] {
			t.Errorf("put[%d] = %q, want %q (full order %v)", i, f.puts[i], want[i], f.puts)
		}
	}
}

func TestUploadOneSendsTheFileContents(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x")
	if err := os.WriteFile(p, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	capture := &capturingS3{onPut: func(in *s3.PutObjectInput) { io.Copy(&got, in.Body) }}
	if err := uploadOne(context.Background(), capture, "b", "k", p); err != nil {
		t.Fatalf("uploadOne: %v", err)
	}
	if got.String() != "payload" {
		t.Errorf("uploaded %q, want %q", got.String(), "payload")
	}
}

// capturingS3 is a fake that only implements PutObject behaviour of interest.
type capturingS3 struct {
	fakeS3
	onPut func(*s3.PutObjectInput)
}

func (c *capturingS3) PutObject(ctx context.Context, in *s3.PutObjectInput, o ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	if c.onPut != nil {
		c.onPut(in)
	}
	return &s3.PutObjectOutput{}, nil
}

func TestPullConcurrencyOverride(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want int
	}{
		{"", defaultPullConcurrency},
		{"3", 3},
		{" 3 ", 3},
		{"1", 1},
		{"32", 32},
		// A bad value must not be the reason a release cannot publish.
		{"0", defaultPullConcurrency},
		{"-4", defaultPullConcurrency},
		{"lots", defaultPullConcurrency},
	} {
		if got := pullConcurrency(tc.env); got != tc.want {
			t.Errorf("pullConcurrency(%q) = %d, want %d", tc.env, got, tc.want)
		}
	}
}

func TestDownloadAllHonoursTheConfiguredConcurrency(t *testing.T) {
	dir := t.TempDir()
	f := newFake()
	f.barrier = 3 // deadlocks unless exactly 3 run at once
	var objs []poolObject
	for i := 0; i < 9; i++ {
		key := fmt.Sprintf("public/pool/v%d.deb", i)
		objs = append(objs, poolObject{Key: key})
		f.plans[key] = []bodyPlan{{data: "x"}}
	}

	c := config{bucket: "b", prefix: "public/", concurrency: 3}
	if err := downloadAll(context.Background(), f, c, dir, objs); err != nil {
		t.Fatalf("downloadAll: %v", err)
	}
	if f.maxSeen != 3 {
		t.Errorf("peak concurrency = %d, want 3", f.maxSeen)
	}
}

// A config built without going through loadConfig (concurrency 0) must still
// download rather than block forever on a zero-capacity semaphore.
func TestDownloadAllFallsBackWhenConcurrencyIsUnset(t *testing.T) {
	dir := t.TempDir()
	f := newFake()
	f.plans["public/pool/a.deb"] = []bodyPlan{{data: "x"}}

	c := config{bucket: "b", prefix: "public/"} // concurrency left at 0
	if err := downloadAll(context.Background(), f, c, dir, []poolObject{{Key: "public/pool/a.deb"}}); err != nil {
		t.Fatalf("downloadAll: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "pool", "a.deb")); err != nil {
		t.Errorf("object should have been downloaded: %v", err)
	}
}

// The stall is the failure shape the retry loop cannot see on its own: no error
// ever arrives, so without a per-attempt deadline io.Copy blocks forever and the
// unattended job hangs until the CI runner kills it.
func TestDownloadWithRetryConvertsAStallIntoARetriedFailure(t *testing.T) {
	fastRetries(t)
	fastTimeout(t, 50*time.Millisecond)
	dir := t.TempDir()
	f := newFake()
	f.plans["k"] = []bodyPlan{{stall: true}, {data: "arrived on the second try"}}

	done := make(chan error, 1)
	go func() {
		done <- downloadWithRetry(context.Background(), f, "b", poolObject{Key: "k"}, filepath.Join(dir, "o"))
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("downloadWithRetry: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("downloadWithRetry hung on a stalled body — the per-attempt deadline is not working")
	}
	if f.attempts["k"] != 2 {
		t.Errorf("attempts = %d, want 2", f.attempts["k"])
	}
	got, err := os.ReadFile(filepath.Join(dir, "o"))
	if err != nil || string(got) != "arrived on the second try" {
		t.Errorf("file = %q (err %v), want the second attempt's contents", got, err)
	}
}

func TestDownloadWithRetryGivesUpOnAPermanentlyStalledObject(t *testing.T) {
	fastRetries(t)
	fastTimeout(t, 50*time.Millisecond)
	dir := t.TempDir()
	f := newFake()
	f.plans["k"] = []bodyPlan{{stall: true}}

	done := make(chan error, 1)
	go func() {
		done <- downloadWithRetry(context.Background(), f, "b", poolObject{Key: "k"}, filepath.Join(dir, "o"))
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a failure, not a hang and not a success")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("downloadWithRetry hung instead of exhausting its attempts")
	}
	if _, err := os.Stat(filepath.Join(dir, "o")); !os.IsNotExist(err) {
		t.Errorf("no file should be left behind, stat err = %v", err)
	}
}

// A body whose length disagrees with the listing must be rejected even though the
// transport reported no error: the index would otherwise publish the truncated
// file's own hashes, and the daily reconcile cannot detect that.
func TestDownloadOneRejectsAShortObject(t *testing.T) {
	dir := t.TempDir()
	f := newFake()
	f.plans["k"] = []bodyPlan{{data: "only 13 bytes"}}
	dst := filepath.Join(dir, "out.deb")

	err := downloadOne(context.Background(), f, "b", poolObject{Key: "k", Size: 4096}, dst)
	if err == nil {
		t.Fatal("expected a short-object error")
	}
	if !strings.Contains(err.Error(), "short object") {
		t.Errorf("err = %v, want it to name the mismatch", err)
	}
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Errorf("short object must not be left in the pool, stat err = %v", statErr)
	}
}

func TestDownloadOneAcceptsAnObjectMatchingTheListedSize(t *testing.T) {
	dir := t.TempDir()
	f := newFake()
	f.plans["k"] = []bodyPlan{{data: "exact"}}
	dst := filepath.Join(dir, "out.deb")

	if err := downloadOne(context.Background(), f, "b", poolObject{Key: "k", Size: 5}, dst); err != nil {
		t.Fatalf("downloadOne: %v", err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "exact" {
		t.Errorf("dst = %q, want %q", got, "exact")
	}
}

func TestPullCarriesTheListedSizeIntoTheSizeCheck(t *testing.T) {
	fastRetries(t)
	dir := t.TempDir()
	f := newFake()
	key := "public/pool/main/c/cs-agent/a.deb"
	f.listed = []string{key}
	f.sizes[key] = 999 // the store says 999 bytes...
	f.plans[key] = []bodyPlan{{data: "but only this arrives"}}

	err := pull(context.Background(), f, config{bucket: "b", prefix: "public/"}, dir, false)
	if err == nil {
		t.Fatal("pull must fail when an object arrives shorter than the listing says")
	}
	if !strings.Contains(err.Error(), "short object") {
		t.Errorf("err = %v, want the short-object diagnosis", err)
	}
}

// statusError models an SDK error carrying an HTTP status code.
type statusError struct {
	code int
	msg  string
}

func (e *statusError) Error() string       { return e.msg }
func (e *statusError) HTTPStatusCode() int { return e.code }

func TestPermanentFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		permanent bool
	}{
		{"403 rotated key", &statusError{403, "Forbidden"}, true},
		{"404 missing object", &statusError{404, "NoSuchKey"}, true},
		{"400 bad request", &statusError{400, "Bad Request"}, true},
		// These two 4xx codes explicitly mean "try again".
		{"408 request timeout", &statusError{408, "Request Timeout"}, false},
		{"429 slow down", &statusError{429, "SlowDown"}, false},
		{"500 server error", &statusError{500, "Internal Error"}, false},
		{"503 unavailable", &statusError{503, "Slow Down"}, false},
		{"plain transport reset", errReset, false},
		{"wrapped 403", fmt.Errorf("get: %w", &statusError{403, "Forbidden"}), true},
	} {
		got, why := permanentFailure(tc.err)
		if got != tc.permanent {
			t.Errorf("%s: permanentFailure = %v, want %v", tc.name, got, tc.permanent)
		}
		if got && why == "" {
			t.Errorf("%s: a permanent failure should say why", tc.name)
		}
	}
}

func TestDownloadWithRetryDoesNotRetryAPermanentFailure(t *testing.T) {
	fastRetries(t)
	dir := t.TempDir()
	f := newFake()
	f.plans["k"] = []bodyPlan{{getErr: &statusError{403, "Forbidden"}}}

	err := downloadWithRetry(context.Background(), f, "b", poolObject{Key: "k"}, filepath.Join(dir, "o"))
	if err == nil {
		t.Fatal("expected the 403 to fail")
	}
	if !strings.Contains(err.Error(), "not retryable") {
		t.Errorf("err = %v, want it flagged as not retryable", err)
	}
	if f.attempts["k"] != 1 {
		t.Errorf("attempts = %d, want 1 — a 403 cannot be fixed by waiting", f.attempts["k"])
	}
}

func TestPullRejectsAKeyThatEscapesTheWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	f := newFake()
	f.listed = []string{"public/pool/../../../escaped.deb"}
	f.plans["public/pool/../../../escaped.deb"] = []bodyPlan{{data: "x"}}

	err := pull(context.Background(), f, config{bucket: "b", prefix: "public/"}, dir, false)
	if err == nil {
		t.Fatal("pull must reject a key resolving outside the working directory")
	}
	if f.attempts["public/pool/../../../escaped.deb"] != 0 {
		t.Error("the object must not be fetched at all")
	}
}

func TestParseArgs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		argv    []string
		want    invocation
		wantErr string
	}{
		{name: "pull", argv: []string{"pull", "d"}, want: invocation{cmd: "pull", dir: "d"}},
		{name: "push", argv: []string{"push", "d"}, want: invocation{cmd: "push", dir: "d"}},
		{name: "pull allow-empty", argv: []string{"pull", "d", "--allow-empty"},
			want: invocation{cmd: "pull", dir: "d", allowEmpty: true}},
		// Silently accepting this for push would make a mistyped command look like
		// it did something it cannot do.
		{name: "push allow-empty is rejected", argv: []string{"push", "d", "--allow-empty"},
			wantErr: "applies to pull"},
		{name: "unknown flag", argv: []string{"pull", "d", "--force"}, wantErr: "unknown flag"},
		{name: "unknown command", argv: []string{"sync", "d"}, wantErr: "unknown command"},
		{name: "missing dir", argv: []string{"pull"}, wantErr: "usage"},
		{name: "no args", argv: nil, wantErr: "usage"},
	} {
		got, err := parseArgs(tc.argv)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s: err = %v, want it to contain %q", tc.name, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected err %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: parseArgs = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// Concurrent downloads fail together, so an unjittered backoff would have all of
// them retry in lockstep and re-create the burst that caused the failure.
func TestJitterSpreadsTheDelay(t *testing.T) {
	const d = time.Second
	seen := map[time.Duration]bool{}
	for i := 0; i < 500; i++ {
		got := jitter(d)
		if got < d/2 || got > d {
			t.Fatalf("jitter(%v) = %v, want within [%v, %v]", d, got, d/2, d)
		}
		seen[got] = true
	}
	// Distinct values are the whole point; a constant would pass the bounds check.
	if len(seen) < 100 {
		t.Errorf("jitter produced only %d distinct values across 500 calls — it is not spreading", len(seen))
	}
}

func TestJitterHandlesZeroAndNegative(t *testing.T) {
	if got := jitter(0); got != 0 {
		t.Errorf("jitter(0) = %v, want 0 (fastRetries relies on this)", got)
	}
	if got := jitter(-5); got != -5 {
		t.Errorf("jitter(-5) = %v, want it returned unchanged rather than panicking", got)
	}
	// One nanosecond must not panic on rand.Int64N(0).
	if got := jitter(time.Nanosecond); got < 0 || got > time.Nanosecond {
		t.Errorf("jitter(1ns) = %v, out of range", got)
	}
}
