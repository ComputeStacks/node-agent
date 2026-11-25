package main

import (
	"bytes"
	"context"
	"cs-agent/dbbackup"
	"cs-agent/s3upload"
	"cs-agent/store"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/spf13/viper"
)

type srObj struct{ objs map[string][]byte }

func (f *srObj) Upload(ctx context.Context, key string, r io.Reader) (int64, error) {
	return 0, errors.New("read-only fake")
}
func (f *srObj) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	b, ok := f.objs[key]
	if !ok {
		return nil, errors.New("nosuchkey")
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
func (f *srObj) List(ctx context.Context, prefix string) ([]s3upload.Object, error) {
	var out []s3upload.Object
	for k, v := range f.objs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, s3upload.Object{Key: k, Size: int64(len(v))})
		}
	}
	return out, nil
}
func (f *srObj) Delete(ctx context.Context, keys []string) error { return nil }

type srEnv struct {
	dir     string
	idFile  string
	obj     *srObj
	lineage string
}

// newSREnv makes a backup of a control.db with tenant p1 (node "n1") and a
// target data dir with an existing control.db.
func newSREnv(t *testing.T) *srEnv {
	t.Helper()
	ctx := context.Background()
	id, _ := age.GenerateX25519Identity()
	idFile := filepath.Join(t.TempDir(), "id.txt")
	os.WriteFile(idFile, []byte(id.String()+"\n"), 0o600)

	src, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	src.UpsertTenant(ctx, "p1", "h", "active")
	lineage, _ := src.InstanceID(ctx)
	snap := filepath.Join(t.TempDir(), "c.db")
	if err := src.SnapshotControl(ctx, snap); err != nil {
		t.Fatal(err)
	}
	plain, _ := os.ReadFile(snap)
	var buf bytes.Buffer
	wc, _ := dbbackup.Encrypt(&buf, []age.Recipient{id.Recipient()})
	wc.Write(plain)
	wc.Close()
	obj := &srObj{objs: map[string][]byte{
		dbbackup.KeyFor("n1", "control", lineage, time.Date(2026, 11, 24, 1, 2, 3, 0, time.UTC)): buf.Bytes(),
	}}

	dir := t.TempDir()
	dst, err := store.Open(dir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	dst.Close()

	e := &srEnv{dir: dir, idFile: idFile, obj: obj, lineage: lineage}
	oldEnv, oldHost, oldNow := restoreLoadEnv, restoreHostname, restoreNow
	restoreLoadEnv = func() (restoreEnv, error) { return restoreEnv{dataDir: dir, obj: obj}, nil }
	restoreHostname = func() (string, error) { return "n1", nil }
	restoreNow = func() time.Time { return time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { restoreLoadEnv, restoreHostname, restoreNow = oldEnv, oldHost, oldNow })
	return e
}

func runSR(stdin string, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := storeRestoreCLI(context.Background(), args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func TestStoreRestoreCLI_Usage(t *testing.T) {
	newSREnv(t)
	for _, tc := range []struct {
		args []string
		code int
	}{
		{nil, restoreExitUsage},
		{[]string{"help"}, restoreExitOK},
		{[]string{"bogus"}, restoreExitUsage},
		{[]string{"control"}, restoreExitUsage},                             // no --identity
		{[]string{"project", "--identity", "x"}, restoreExitUsage},          // no --project
		{[]string{"list", "--control", "--project", "p"}, restoreExitUsage}, // exclusive
		{[]string{"control", "--identity", "x", "--at", "yesterday"}, restoreExitUsage},
		{[]string{"list", "--identity", "x"}, restoreExitUsage}, // not a list flag
		{[]string{"control", "--identity", "/nonexistent"}, restoreExitErr},
	} {
		if code, _, _ := runSR("", tc.args...); code != tc.code {
			t.Errorf("%v: exit %d, want %d", tc.args, code, tc.code)
		}
	}
}

func TestStoreRestoreCLI_HelpCoversFlags(t *testing.T) {
	for _, cmd := range storeRestoreCommands {
		if !strings.Contains(storeRestoreUsage, "\n  "+cmd+" ") {
			t.Errorf("usage does not document %q", cmd)
		}
		fs, _ := newStoreRestoreFlagSet(cmd, io.Discard)
		fs.VisitAll(func(f *flag.Flag) {
			if !strings.Contains(storeRestoreUsage, "--"+f.Name) {
				t.Errorf("usage does not document --%s (%s)", f.Name, cmd)
			}
		})
	}
}

func TestStoreRestoreCLI_List(t *testing.T) {
	e := newSREnv(t)
	code, out, errOut := runSR("", "list")
	if code != 0 || !strings.Contains(out, e.lineage) || !strings.Contains(out, "node n1") {
		t.Fatalf("exit %d out %q err %q", code, out, errOut)
	}
	code, out, _ = runSR("", "list", "--node", "other")
	if code != 0 || !strings.Contains(out, "No snapshots") {
		t.Fatalf("exit %d out %q", code, out)
	}
}

func TestStoreRestoreCLI_DeclineChangesNothing(t *testing.T) {
	e := newSREnv(t)
	before, _ := os.ReadFile(store.ControlPath(e.dir))
	code, out, _ := runSR("n\n", "control", "--identity", e.idFile)
	if code != restoreExitErr || !strings.Contains(out, "Proceed? [y/N]") {
		t.Fatalf("exit %d out %q", code, out)
	}
	if !strings.Contains(out, "nodes/n1/control/20261124T010203Z_"+e.lineage) {
		t.Fatalf("plan does not show the object key: %q", out)
	}
	after, _ := os.ReadFile(store.ControlPath(e.dir))
	if !bytes.Equal(before, after) {
		t.Fatal("declined restore changed control.db")
	}
}

func TestStoreRestoreCLI_ControlYes(t *testing.T) {
	e := newSREnv(t)
	code, out, errOut := runSR("", "control", "--identity", e.idFile, "--yes")
	if code != 0 || !strings.Contains(out, "Restore complete") || strings.Contains(out, "Proceed?") {
		t.Fatalf("exit %d out %q err %q", code, out, errOut)
	}
	if !strings.Contains(out, "local maintenance mode") || !strings.Contains(out, "sudo cs-agent maintenance off") {
		t.Fatalf("no maintenance hold notice: %q", out)
	}
	st, err := store.OpenExistingControl(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	m, err := st.GetMaintenance(context.Background())
	st.Close()
	if err != nil || m.Local == nil {
		t.Fatalf("no local hold after restore: %+v %v", m, err)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "pre-restore-20261125T000000Z", "control.db")); err != nil {
		t.Fatal(err)
	}
	tenants, err := store.ProjectsInControlDB(store.ControlPath(e.dir))
	if err != nil || len(tenants) != 1 || tenants[0] != "p1" {
		t.Fatalf("tenants %v err %v", tenants, err)
	}
}

func TestStoreRestoreCLI_RefusesWhileAgentRuns(t *testing.T) {
	e := newSREnv(t)
	release, err := store.LockDataDir(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	code, _, errOut := runSR("", "control", "--identity", e.idFile, "--yes")
	if code != restoreExitErr || !strings.Contains(errOut, "systemctl stop cs-agent") {
		t.Fatalf("exit %d err %q", code, errOut)
	}
}

// The daemon's backup wiring: off is a true nil status source; enabled but
// misconfigured reports enabled with the setup error.
func TestBuildStoreBackupStatus(t *testing.T) {
	t.Cleanup(viper.Reset)
	viper.Set("store.backup.enabled", false)
	if sb := buildStoreBackup(nil).status(); sb != nil {
		t.Fatalf("disabled: got %#v, want nil interface", sb)
	}
	viper.Set("store.backup.enabled", true)
	h := buildStoreBackup(nil)
	if h.runner != nil || h.configErr == nil {
		t.Fatalf("misconfigured: %+v", h)
	}
	st := h.status().Status()
	if !st.Enabled || !strings.HasPrefix(st.LastError, "config: ") || st.LastErrorAt == nil {
		t.Fatalf("misconfigured status %+v", st)
	}
}
