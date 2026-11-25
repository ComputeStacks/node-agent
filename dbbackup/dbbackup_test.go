package dbbackup

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
	"testing"
	"time"

	"cs-agent/s3upload"

	"filippo.io/age"
	"github.com/spf13/viper"
)

type fakeObj struct {
	mu      sync.Mutex
	objs    map[string][]byte
	uploads int
	failUp  error
	delErr  error
	deleted []string
}

func newFakeObj() *fakeObj { return &fakeObj{objs: map[string][]byte{}} }

func (f *fakeObj) Upload(ctx context.Context, key string, r io.Reader) (int64, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failUp != nil {
		return 0, f.failUp
	}
	f.objs[key] = b
	f.uploads++
	return int64(len(b)), nil
}
func (f *fakeObj) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.objs[key]
	if !ok {
		return nil, errors.New("nosuchkey")
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
func (f *fakeObj) List(ctx context.Context, prefix string) ([]s3upload.Object, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []s3upload.Object
	for k, v := range f.objs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, s3upload.Object{Key: k, Size: int64(len(v))})
		}
	}
	return out, nil
}
func (f *fakeObj) Delete(ctx context.Context, keys []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.delErr != nil {
		return f.delErr
	}
	for _, k := range keys {
		delete(f.objs, k)
		f.deleted = append(f.deleted, k)
	}
	return nil
}

var errGone = errors.New("project gone")

type fakeSnap struct {
	dir      string
	projects []string
	instance string
	gone     map[string]bool
	snaps    int
	onSnap   func(db string) // called mid-snapshot
}

func (s *fakeSnap) DataDir() string                                { return s.dir }
func (s *fakeSnap) ProjectIDs() ([]string, error)                  { return s.projects, nil }
func (s *fakeSnap) InstanceID(ctx context.Context) (string, error) { return s.instance, nil }
func (s *fakeSnap) copy(src, dst, db string) error {
	s.snaps++
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if s.onSnap != nil {
		s.onSnap(db)
	}
	return os.WriteFile(dst, b, 0o600)
}
func (s *fakeSnap) SnapshotControl(ctx context.Context, dst string) error {
	return s.copy(filepath.Join(s.dir, "control.db"), dst, "control")
}
func (s *fakeSnap) SnapshotProject(ctx context.Context, id, dst string) error {
	if s.gone[id] {
		return fmt.Errorf("snap %s: %w", id, errGone)
	}
	return s.copy(filepath.Join(s.dir, "projects", id+".db"), dst, "projects/"+id)
}

type env struct {
	t   *testing.T
	dir string
	sn  *fakeSnap
	ob  *fakeObj
	r   *Runner
	id  *age.X25519Identity
	now time.Time
}

func setup(t *testing.T, projects ...string) *env {
	t.Helper()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "projects"), 0o755)
	os.WriteFile(filepath.Join(dir, "control.db"), []byte("control v1"), 0o600)
	for _, p := range projects {
		os.WriteFile(filepath.Join(dir, "projects", p+".db"), []byte("proj "+p), 0o600)
	}
	id, _ := age.GenerateX25519Identity()
	e := &env{t: t, dir: dir, id: id, ob: newFakeObj(), now: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC),
		sn: &fakeSnap{dir: dir, projects: projects, instance: "inst1", gone: map[string]bool{}}}
	e.rebuild()
	return e
}

func (e *env) rebuild() {
	r, err := NewRunner(Config{Enabled: true, Recipients: []age.Recipient{e.id.Recipient()}, NodeName: "n1",
		KeepAll: 48 * time.Hour, RetentionDays: 30, IsProjectGone: func(err error) bool { return errors.Is(err, errGone) }}, e.sn, e.ob)
	if err != nil {
		e.t.Fatal(err)
	}
	r.now = func() time.Time { return e.now }
	e.r = r
}

func (e *env) run() error {
	e.now = e.now.Add(time.Minute)
	return e.r.RunOnce(context.Background())
}

func TestUploadAndGateSkip(t *testing.T) {
	e := setup(t, "p1")
	if err := e.run(); err != nil {
		t.Fatal(err)
	}
	if e.ob.uploads != 2 || e.sn.snaps != 2 {
		t.Fatalf("uploads=%d snaps=%d", e.ob.uploads, e.sn.snaps)
	}
	st := e.r.Status()
	if st.Uploaded != 2 || st.DBs != 2 || st.LastSuccessAt == nil || st.Lineage != "inst1" {
		t.Fatalf("%+v", st)
	}
	// decrypt one
	for k, v := range e.ob.objs {
		rd, err := Decrypt(bytes.NewReader(v), []age.Identity{e.id})
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rd)
		if strings.Contains(k, "control") && string(b) != "control v1" {
			t.Fatalf("got %q", b)
		}
	}
	if err := e.run(); err != nil {
		t.Fatal(err)
	}
	if e.ob.uploads != 2 || e.sn.snaps != 2 {
		t.Fatalf("gate should skip: uploads=%d snaps=%d", e.ob.uploads, e.sn.snaps)
	}
	if st := e.r.Status(); st.Skipped != 2 {
		t.Fatalf("%+v", st)
	}
	if entries, _ := os.ReadDir(filepath.Join(e.dir, tmpDirName)); len(entries) != 0 {
		t.Fatalf("temp not cleaned: %v", entries)
	}
}

func TestHashSkipAfterMtimeChurn(t *testing.T) {
	e := setup(t)
	e.run()
	later := e.now.Add(time.Hour)
	os.Chtimes(filepath.Join(e.dir, "control.db"), later, later)
	e.run()
	if e.sn.snaps != 2 || e.ob.uploads != 1 {
		t.Fatalf("snaps=%d uploads=%d", e.sn.snaps, e.ob.uploads)
	}
	e.run() // marker now recorded -> gate skip
	if e.sn.snaps != 2 {
		t.Fatalf("snaps=%d", e.sn.snaps)
	}
}

func TestWriteDuringSnapshotResnapshots(t *testing.T) {
	e := setup(t)
	p := filepath.Join(e.dir, "control.db")
	e.sn.onSnap = func(string) {
		f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
		f.WriteString("more")
		f.Close()
		e.sn.onSnap = nil
	}
	e.run()
	e.run()
	if e.sn.snaps != 2 {
		t.Fatalf("expected a second snapshot, got %d", e.sn.snaps)
	}
	if e.ob.uploads != 2 { // first snapshot read pre-write content, second sees the append
		t.Fatalf("uploads=%d", e.ob.uploads)
	}
}

func TestUploadFailure(t *testing.T) {
	e := setup(t)
	e.run()
	prev := *e.r.Status().LastSuccessAt
	os.WriteFile(filepath.Join(e.dir, "control.db"), []byte("changed"), 0o600)
	e.ob.failUp = errors.New("boom")
	if err := e.run(); err == nil {
		t.Fatal("expected error")
	}
	st := e.r.Status()
	if st.Failed != 1 || st.LastError == "" || st.LastErrorAt == nil || !st.LastSuccessAt.Equal(prev) {
		t.Fatalf("%+v", st)
	}
	e.ob.failUp = nil
	if err := e.run(); err != nil {
		t.Fatal(err)
	}
	if e.ob.uploads != 2 {
		t.Fatalf("uploads=%d", e.ob.uploads)
	}
}

func TestProjectVanishedSkipped(t *testing.T) {
	e := setup(t, "p1")
	e.sn.gone["p1"] = true
	if err := e.run(); err != nil {
		t.Fatal(err)
	}
	st := e.r.Status()
	if st.Failed != 0 || st.Skipped != 1 || st.LastSuccessAt == nil {
		t.Fatalf("%+v", st)
	}
}

func TestCorruptStateRecovers(t *testing.T) {
	e := setup(t)
	e.run()
	os.WriteFile(StatePath(e.dir), []byte("{not json"), 0o600)
	e.rebuild()
	if e.r.Status().LastSuccessAt != nil {
		t.Fatal("status should be empty")
	}
	if err := e.run(); err != nil {
		t.Fatal(err)
	}
	if e.ob.uploads != 2 { // state lost: uploads again even though content is unchanged
		t.Fatalf("uploads=%d", e.ob.uploads)
	}
}

func TestStatePersistsAcrossRestart(t *testing.T) {
	e := setup(t)
	e.run()
	e.rebuild()
	if e.r.Status().LastSuccessAt == nil {
		t.Fatal("status not reloaded")
	}
	e.run()
	if e.sn.snaps != 1 {
		t.Fatalf("snaps=%d", e.sn.snaps)
	}
}

func TestLineageChangeForcesUpload(t *testing.T) {
	e := setup(t)
	e.run()
	e.sn.instance = "inst2"
	e.run()
	if e.ob.uploads != 2 {
		t.Fatalf("uploads=%d", e.ob.uploads)
	}
}

func TestRetentionRunsOncePerDay(t *testing.T) {
	e := setup(t)
	// The newest object beyond the horizon is kept as the window-start
	// state; the one before it is deleted.
	e.ob.objs[KeyFor("n1", "control", "inst1", e.now.Add(-40*24*time.Hour))] = []byte("x")
	old := KeyFor("n1", "control", "inst1", e.now.Add(-45*24*time.Hour))
	e.ob.objs[old] = []byte("x")
	e.run()
	if len(e.ob.deleted) != 1 || e.ob.deleted[0] != old {
		t.Fatalf("deleted=%v", e.ob.deleted)
	}
	e.ob.objs[old] = []byte("x")
	e.run()
	if len(e.ob.deleted) != 1 {
		t.Fatal("retention ran twice within 24h")
	}
	e.now = e.now.Add(25 * time.Hour)
	e.run()
	if len(e.ob.deleted) != 2 {
		t.Fatalf("deleted=%v", e.ob.deleted)
	}
}

func TestRetentionDeleteFailureNotFatal(t *testing.T) {
	e := setup(t)
	e.ob.objs[KeyFor("n1", "control", "inst1", e.now.Add(-40*24*time.Hour))] = []byte("x")
	e.ob.delErr = errors.New("nope")
	if err := e.run(); err != nil {
		t.Fatal(err)
	}
}

func TestContextCancel(t *testing.T) {
	e := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := e.r.RunOnce(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestEncryptDecrypt(t *testing.T) {
	a, _ := age.GenerateX25519Identity()
	b, _ := age.GenerateX25519Identity()
	var buf bytes.Buffer
	w, err := Encrypt(&buf, []age.Recipient{a.Recipient()})
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("hello sqlite "), 10000)
	w.Write(payload)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := Decrypt(bytes.NewReader(buf.Bytes()), []age.Identity{a})
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("round trip failed: %v", err)
	}
	if _, err := Decrypt(bytes.NewReader(buf.Bytes()), []age.Identity{b}); err == nil {
		t.Fatal("wrong identity should fail")
	}
}

func TestKeyRoundTrip(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, db := range []string{"control", "projects/abc-1"} {
		k := KeyFor("host", db, "lin-x", ts)
		for _, key := range []string{k, "sqlite/" + k} {
			n, d, l, got, ok := ParseKey(key)
			if !ok || n != "host" || d != db || l != "lin-x" || !got.Equal(ts) {
				t.Fatalf("%s: %v %v %v %v %v", key, n, d, l, got, ok)
			}
		}
	}
	if KeyFor("h", "control", "l", ts) != "nodes/h/control/20260102T030405Z_l.db.gz.age" {
		t.Fatal(KeyFor("h", "control", "l", ts))
	}
	for _, bad := range []string{"", "junk", "nodes/h/control/x.db.gz.age", "nodes/h/control/20260102T030405Z_.db.gz.age",
		"nodes/h/control/20260102T030405Z_l.db", "nodes/h/other/20260102T030405Z_l.db.gz.age",
		"nodes/h/control/2026010XT030405Z_l.db.gz.age", "nodes/h/projects/20260102T030405Z_l.db.gz.age"} {
		if _, _, _, _, ok := ParseKey(bad); ok {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestKeep(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	h := func(hrs float64) time.Time { return now.Add(-time.Duration(hrs * float64(time.Hour))) }
	mk := func(lin string, ts time.Time) s3upload.Object {
		return s3upload.Object{Key: KeyFor("n", "control", lin, ts)}
	}
	keys := func(os ...s3upload.Object) []string {
		var r []string
		for _, o := range os {
			r = append(r, o.Key)
		}
		sort.Strings(r)
		return r
	}
	a1 := mk("A", h(1))
	day := func(d int, hr int) time.Time { return time.Date(2026, 11, d, hr, 0, 0, 0, time.UTC) }
	cases := []struct {
		name   string
		objs   []s3upload.Object
		exists bool
		days   int
		want   []string
	}{
		{"all recent kept", []s3upload.Object{a1, mk("A", h(2))}, true, 30, nil},
		{"day bucketing keeps newest per UTC day", []s3upload.Object{mk("A", day(20, 23)), mk("A", day(20, 1)), mk("A", day(19, 23)), mk("A", day(19, 0)), a1},
			true, 30, keys(mk("A", day(20, 1)), mk("A", day(19, 0)))},
		{"older than days deleted, newest of lineage kept", []s3upload.Object{mk("A", h(24*40)), mk("A", h(24*50))}, true, 30, keys(mk("A", h(24*50)))},
		{"old lineage ages out, current keeps window-start anchor", []s3upload.Object{mk("A", h(24*40)), mk("A", h(24*41)), mk("B", h(1)), mk("B", h(24*35)), mk("B", h(24*36))}, true, 30,
			keys(mk("A", h(24*40)), mk("A", h(24*41)), mk("B", h(24*36)))},
		{"rarely-changing database keeps the object live at window start", []s3upload.Object{mk("A", h(24*40)), mk("A", h(24*1))}, true, 30, nil},
		{"dead old lineage fully beyond horizon is all deleted", []s3upload.Object{mk("A", h(24*40)), mk("A", h(24*45)), mk("A", h(24*50)), mk("B", h(1)), mk("B", h(24*5))}, true, 30,
			keys(mk("A", h(24*40)), mk("A", h(24*45)), mk("A", h(24*50)))},
		{"old lineage within days keeps its daily objects", []s3upload.Object{mk("A", h(24*10)), mk("A", h(24*10+1)), mk("B", h(1))}, true, 30,
			keys(mk("A", h(24*10+1)))},
		{"deleted project keeps window-start anchor", []s3upload.Object{mk("A", h(24*50)), mk("A", h(24*40)), mk("A", h(24*5))}, false, 30, keys(mk("A", h(24*50)))},
		{"deleted project fully beyond horizon ages out", []s3upload.Object{mk("A", h(24*40)), mk("A", h(24*50))}, false, 30, keys(mk("A", h(24*40)), mk("A", h(24*50)))},
		{"deleted project newest recent still kept", []s3upload.Object{mk("A", h(1))}, false, 30, nil},
		{"junk keys untouched", []s3upload.Object{{Key: "nodes/n/control/readme.txt"}, a1}, true, 30, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Keep(c.objs, now, 48*time.Hour, c.days, c.exists)
			sort.Strings(got)
			if fmt.Sprint(got) != fmt.Sprint(c.want) {
				t.Fatalf("got %v want %v", got, c.want)
			}
		})
	}
}

func TestConfigFromViper(t *testing.T) {
	id, _ := age.GenerateX25519Identity()
	reset := func() { viper.Reset(); t.Cleanup(viper.Reset) }
	reset()
	if c, err := ConfigFromViper(); err != nil || c.Enabled {
		t.Fatalf("disabled: %+v %v", c, err)
	}
	viper.Set("store.backup.enabled", true)
	if _, err := ConfigFromViper(); err == nil || !strings.Contains(err.Error(), "store.backup.recipients") {
		t.Fatalf("no recipients: %v", err)
	}
	viper.Set("store.backup.recipients", []string{"bogus"})
	if _, err := ConfigFromViper(); err == nil || !strings.Contains(err.Error(), "store.backup.recipients") {
		t.Fatalf("bad recipient: %v", err)
	}
	viper.Set("store.backup.recipients", []string{id.Recipient().String()})
	c, err := ConfigFromViper()
	if err != nil {
		t.Fatal(err)
	}
	host, _ := os.Hostname()
	if !c.Enabled || len(c.Recipients) != 1 || c.NodeName != host || c.KeepAll != 48*time.Hour || c.RetentionDays != 30 {
		t.Fatalf("%+v", c)
	}
	viper.Set("store.backup.node_name", "x")
	viper.Set("store.backup.keep_all_hours", 6)
	viper.Set("store.backup.retention_days", 7)
	c, _ = ConfigFromViper()
	if c.NodeName != "x" || c.KeepAll != 6*time.Hour || c.RetentionDays != 7 {
		t.Fatalf("%+v", c)
	}
	viper.Set("store.backup.retention_days", 0)
	if _, err := ConfigFromViper(); err == nil {
		t.Fatal("retention_days 0 should fail")
	}
}
