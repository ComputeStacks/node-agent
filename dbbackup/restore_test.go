package dbbackup

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"cs-agent/store"

	"filippo.io/age"
)

var rctx = context.Background()

const rnode = "n1"

// world is a source store whose snapshots are uploaded as backups.
type world struct {
	t   *testing.T
	st  *store.Store
	obj *fakeObj
	id  *age.X25519Identity
}

func newWorld(t *testing.T, obj *fakeObj, id *age.X25519Identity) *world {
	t.Helper()
	st, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &world{t: t, st: st, obj: obj, id: id}
}

func (w *world) lineage() string {
	id, err := w.st.InstanceID(rctx)
	if err != nil {
		w.t.Fatal(err)
	}
	return id
}

func (w *world) tenant(ids ...string) {
	for _, id := range ids {
		if err := w.st.UpsertTenant(rctx, id, "h-"+id, "active"); err != nil {
			w.t.Fatal(err)
		}
	}
}

func (w *world) project(id, val string) {
	if err := w.st.CustomerPut(rctx, id, "k", "text/plain", []byte(val)); err != nil {
		w.t.Fatal(err)
	}
}

func encryptTo(t *testing.T, obj *fakeObj, key string, plain []byte, r age.Recipient) {
	t.Helper()
	var buf bytes.Buffer
	wc, err := Encrypt(&buf, []age.Recipient{r})
	if err != nil {
		t.Fatal(err)
	}
	wc.Write(plain)
	if err := wc.Close(); err != nil {
		t.Fatal(err)
	}
	obj.objs[key] = buf.Bytes()
}

// upload snapshots db ("control" or "projects/<id>") at ts under lineage.
func (w *world) upload(db, lineage string, ts time.Time) string {
	w.t.Helper()
	dst := filepath.Join(w.t.TempDir(), "snap.db")
	var err error
	if db == "control" {
		err = w.st.SnapshotControl(rctx, dst)
	} else {
		err = w.st.SnapshotProject(rctx, strings.TrimPrefix(db, "projects/"), dst)
	}
	if err != nil {
		w.t.Fatal(err)
	}
	b, _ := os.ReadFile(dst)
	key := KeyFor(rnode, db, lineage, ts)
	encryptTo(w.t, w.obj, key, b, w.id.Recipient())
	return key
}

// target is a node data dir with existing databases.
func newTarget(t *testing.T, tenants []string, projects map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range tenants {
		st.UpsertTenant(rctx, id, "old-"+id, "active")
	}
	for id, v := range projects {
		if err := st.CustomerPut(rctx, id, "k", "text/plain", []byte(v)); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(StatePath(dir), []byte(`{}`), 0o600)
	return dir
}

// tree hashes every regular file under dir (relative path -> sha256), except
// the lock file, which a restore attempt creates.
func tree(t *testing.T, dir string) map[string][32]byte {
	t.Helper()
	out := map[string][32]byte{}
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() && fi.Name() != store.LockFileName {
			b, _ := os.ReadFile(p)
			rel, _ := filepath.Rel(dir, p)
			out[rel] = sha256.Sum256(b)
		}
		return nil
	})
	return out
}

func sameTree(t *testing.T, a, b map[string][32]byte) {
	t.Helper()
	if len(a) != len(b) {
		t.Fatalf("file set changed: before %v after %v", keys(a), keys(b))
	}
	for k, v := range a {
		if b[k] != v {
			t.Fatalf("%s changed", k)
		}
	}
}

func keys(m map[string][32]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func noLeftovers(t *testing.T, dir string) {
	t.Helper()
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && (strings.Contains(p, restoreTmpExt) || strings.HasPrefix(fi.Name(), "pre-restore-")) {
			t.Errorf("leftover %s", p)
		}
		return nil
	})
}

func readKV(t *testing.T, dir, id string) string {
	t.Helper()
	st, err := store.Open(dir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e, found, err := st.CustomerGet(rctx, id, "k")
	if err != nil || !found {
		t.Fatalf("project %s: found=%v err=%v", id, found, err)
	}
	return string(e.Value)
}

func controlInstanceID(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var id string
	if err := db.QueryRow(`SELECT value FROM control_meta WHERE key='instance_id'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func opts(dir, mode string, id *age.X25519Identity, now time.Time) RestoreOptions {
	return RestoreOptions{Mode: mode, DataDir: dir, Node: rnode, Identities: []age.Identity{id},
		Now: func() time.Time { return now }}
}

var (
	t0   = time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	rnow = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
)

func setupR(t *testing.T) (*fakeObj, *age.X25519Identity, *world) {
	obj := newFakeObj()
	id, _ := age.GenerateX25519Identity()
	return obj, id, newWorld(t, obj, id)
}

func TestRestoreRefusesWhenLocked(t *testing.T) {
	obj, id, w := setupR(t)
	w.tenant("p1")
	w.upload("control", w.lineage(), t0)
	dir := newTarget(t, []string{"old"}, nil)
	release, err := store.LockDataDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	before := tree(t, dir)
	_, err = Restore(rctx, obj, opts(dir, ModeControl, id, rnow))
	if !errors.Is(err, store.ErrDataDirLocked) || !strings.Contains(err.Error(), "systemctl stop cs-agent") {
		t.Fatalf("got %v", err)
	}
	sameTree(t, before, tree(t, dir))
}

func TestRestoreControl(t *testing.T) {
	obj, id, w := setupR(t)
	w.tenant("p1")
	lineage := w.lineage()
	w.upload("control", lineage, t0)
	dir := newTarget(t, []string{"old"}, map[string]string{"old": "keep"})
	oldCtl, _ := os.ReadFile(store.ControlPath(dir))
	projBefore, _ := os.ReadFile(store.ProjectPath(dir, "old"))

	res, err := Restore(rctx, obj, opts(dir, ModeControl, id, rnow))
	if err != nil {
		t.Fatal(err)
	}
	ctl := store.ControlPath(dir)
	for _, s := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(ctl + s); err == nil {
			t.Fatalf("%s%s left next to restored control.db", ctl, s)
		}
	}
	if err := store.CheckRestorable(ctl, store.DBControl); err != nil {
		t.Fatal(err)
	}
	got := controlInstanceID(t, ctl)
	if got == lineage || got != res.Plan.NewInstanceID {
		t.Fatalf("instance_id %q lineage %q plan %q", got, lineage, res.Plan.NewInstanceID)
	}
	if _, err := os.Stat(StatePath(dir)); !os.IsNotExist(err) {
		t.Fatalf("state file not removed: %v", err)
	}
	pre := filepath.Join(dir, "pre-restore-"+rnow.Format(tsLayout))
	if res.PreRestoreDir != pre {
		t.Fatalf("pre-restore dir %q", res.PreRestoreDir)
	}
	moved, err := os.ReadFile(filepath.Join(pre, "control.db"))
	if err != nil || !bytes.Equal(moved, oldCtl) {
		t.Fatalf("original control.db not moved aside intact: %v", err)
	}
	if b, _ := os.ReadFile(store.ProjectPath(dir, "old")); !bytes.Equal(b, projBefore) {
		t.Fatal("control restore touched a project db")
	}
	tenants, _ := store.ProjectsInControlDB(ctl)
	if len(tenants) != 1 || tenants[0] != "p1" {
		t.Fatalf("tenants %v", tenants)
	}
	noTmp(t, dir)

	// The agent opens it fine and keeps the rotated id.
	st, err := store.Open(dir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if iid, _ := st.InstanceID(rctx); iid != got {
		t.Fatalf("store instance_id %q want %q", iid, got)
	}
	st.Close()

	// A second restore in the same second must not reuse the pre-restore dir.
	res2, err := Restore(rctx, obj, opts(dir, ModeControl, id, rnow))
	if err != nil {
		t.Fatal(err)
	}
	if res2.PreRestoreDir != pre+"-2" {
		t.Fatalf("second pre-restore dir %q", res2.PreRestoreDir)
	}
}

func noTmp(t *testing.T, dir string) {
	t.Helper()
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && strings.Contains(p, restoreTmpExt) {
			t.Errorf("leftover %s", p)
		}
		return nil
	})
}

func TestRestoreNode(t *testing.T) {
	obj, id, w := setupR(t)
	lineage := w.lineage()
	w.tenant("p1", "p2")
	w.project("p1", "new1")
	w.project("p2", "new2")
	w.project("p3", "deleted")
	w.upload("projects/p3", lineage, t0) // deleted project's retained object
	w.upload("projects/p1", lineage, t0)
	w.upload("projects/p2", lineage, t0)
	w.upload("control", lineage, t0.Add(time.Minute))

	dir := newTarget(t, []string{"p1", "p4"}, map[string]string{"p1": "old1", "p4": "orphan"})
	p4, _ := os.ReadFile(store.ProjectPath(dir, "p4"))

	var plan *RestorePlan
	o := opts(dir, ModeNode, id, rnow)
	o.Confirm = func(p *RestorePlan) bool { plan = p; return true }
	res, err := Restore(rctx, obj, o)
	if err != nil {
		t.Fatal(err)
	}
	var dbs []string
	for _, d := range plan.DBs {
		dbs = append(dbs, d.DB)
	}
	sort.Strings(dbs)
	if strings.Join(dbs, ",") != "control,projects/p1,projects/p2" {
		t.Fatalf("restored %v", dbs)
	}
	if _, err := os.Stat(store.ProjectPath(dir, "p3")); !os.IsNotExist(err) {
		t.Fatal("deleted project restored")
	}
	if _, err := os.Stat(store.ProjectPath(dir, "p4")); !os.IsNotExist(err) {
		t.Fatal("orphan project left in place")
	}
	if b, err := os.ReadFile(filepath.Join(res.PreRestoreDir, "projects", "p4.db")); err != nil || !bytes.Equal(b, p4) {
		t.Fatalf("orphan not moved aside intact: %v", err)
	}
	if _, err := os.Stat(filepath.Join(res.PreRestoreDir, "projects", "p1.db")); err != nil {
		t.Fatalf("old p1 not moved aside: %v", err)
	}
	if got := controlInstanceID(t, store.ControlPath(dir)); got == lineage {
		t.Fatal("instance_id not rotated")
	}
	if _, err := os.Stat(StatePath(dir)); !os.IsNotExist(err) {
		t.Fatal("state file not removed")
	}
	noTmp(t, dir)
	if v := readKV(t, dir, "p1"); v != "new1" {
		t.Fatalf("p1=%q", v)
	}
	if v := readKV(t, dir, "p2"); v != "new2" {
		t.Fatalf("p2=%q", v)
	}
}

func TestRestoreNodeMissingTenant(t *testing.T) {
	obj, id, w := setupR(t)
	lineage := w.lineage()
	w.tenant("p1", "p2")
	w.project("p1", "new1")
	w.upload("projects/p1", lineage, t0)
	w.upload("control", lineage, t0)

	dir := newTarget(t, []string{"p2"}, map[string]string{"p2": "old2"})
	before := tree(t, dir)
	_, err := Restore(rctx, obj, opts(dir, ModeNode, id, rnow))
	if err == nil || !strings.Contains(err.Error(), "p2") || !strings.Contains(err.Error(), "--allow-missing") {
		t.Fatalf("got %v", err)
	}
	sameTree(t, before, tree(t, dir))
	noLeftovers(t, dir)

	o := opts(dir, ModeNode, id, rnow)
	o.AllowMissing = true
	res, err := Restore(rctx, obj, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Plan.MissingTenants) != 1 || res.Plan.MissingTenants[0] != "p2" {
		t.Fatalf("missing %v", res.Plan.MissingTenants)
	}
	if v := readKV(t, dir, "p2"); v != "old2" {
		t.Fatalf("missing tenant's on-disk db changed: %q", v)
	}
	if v := readKV(t, dir, "p1"); v != "new1" {
		t.Fatalf("p1=%q", v)
	}
}

func TestRestoreValidationFailureChangesNothing(t *testing.T) {
	cases := map[string]func(t *testing.T, w *world, lineage string){
		"garbage plaintext": func(t *testing.T, w *world, lineage string) {
			encryptTo(t, w.obj, KeyFor(rnode, "projects/p2", lineage, t0), []byte("not a database"), w.id.Recipient())
		},
		"wrong recipient": func(t *testing.T, w *world, lineage string) {
			other, _ := age.GenerateX25519Identity()
			b := make([]byte, 4096)
			rand.Read(b)
			encryptTo(t, w.obj, KeyFor(rnode, "projects/p2", lineage, t0), b, other.Recipient())
		},
		"truncated object": func(t *testing.T, w *world, lineage string) {
			key := w.upload("projects/p2", lineage, t0)
			w.obj.objs[key] = w.obj.objs[key][:len(w.obj.objs[key])-20]
		},
		"too-new schema": func(t *testing.T, w *world, lineage string) {
			dst := filepath.Join(t.TempDir(), "s.db")
			if err := w.st.SnapshotProject(rctx, "p2", dst); err != nil {
				t.Fatal(err)
			}
			db, _ := sql.Open("sqlite", "file:"+dst)
			if _, err := db.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (9999, 0)`); err != nil {
				t.Fatal(err)
			}
			db.Close()
			b, _ := os.ReadFile(dst)
			encryptTo(t, w.obj, KeyFor(rnode, "projects/p2", lineage, t0), b, w.id.Recipient())
		},
	}
	for name, corrupt := range cases {
		t.Run(name, func(t *testing.T) {
			obj, id, w := setupR(t)
			lineage := w.lineage()
			w.tenant("p1", "p2")
			w.project("p1", "a")
			w.project("p2", "b")
			w.upload("projects/p1", lineage, t0)
			w.upload("control", lineage, t0)
			corrupt(t, w, lineage)

			dir := newTarget(t, []string{"p1", "p2", "p9"}, map[string]string{"p1": "x", "p2": "y", "p9": "z"})
			before := tree(t, dir)
			_, err := Restore(rctx, obj, opts(dir, ModeNode, id, rnow))
			if err == nil {
				t.Fatal("restore succeeded")
			}
			var se *SwapError
			if errors.As(err, &se) {
				t.Fatalf("failed during swap: %v", err)
			}
			sameTree(t, before, tree(t, dir))
			noLeftovers(t, dir)
		})
	}
}

func TestRestoreLineageAndAt(t *testing.T) {
	obj, id, w := setupR(t)
	a := w.lineage()
	w.tenant("t1")
	w.upload("control", a, t0)
	w.tenant("t2")
	w.upload("control", a, t0.Add(time.Hour))
	w.tenant("t3")
	w.upload("control", "lineageB", t0.Add(2*time.Hour))

	dir := newTarget(t, nil, nil)
	plan := func(o RestoreOptions) (*RestorePlan, error) {
		var p *RestorePlan
		o.Confirm = func(rp *RestorePlan) bool { p = rp; return false }
		_, err := Restore(rctx, obj, o)
		if err != nil && !errors.Is(err, ErrAborted) {
			return nil, err
		}
		return p, nil
	}
	before := tree(t, dir)

	// Two lineages, no --lineage: refuse and name both.
	_, err := plan(opts(dir, ModeControl, id, rnow))
	var amb *AmbiguousLineageError
	if !errors.As(err, &amb) || len(amb.Lineages) != 2 || !strings.Contains(err.Error(), a) || !strings.Contains(err.Error(), "lineageB") {
		t.Fatalf("got %v", err)
	}
	if _, err := plan(opts(dir, ModeNode, id, rnow)); !errors.As(err, &amb) {
		t.Fatalf("node: got %v", err)
	}

	// --lineage picks the newest in that lineage.
	o := opts(dir, ModeControl, id, rnow)
	o.Lineage = a
	p, err := plan(o)
	if err != nil || p.DBs[0].Time != t0.Add(time.Hour) || p.Lineage != a {
		t.Fatalf("plan %+v err %v", p, err)
	}
	// --at between the two picks the older one.
	o.At = t0.Add(30 * time.Minute)
	if p, err = plan(o); err != nil || p.DBs[0].Time != t0 {
		t.Fatalf("plan %+v err %v", p, err)
	}
	// --at before lineage B exists: unambiguous without --lineage.
	o = opts(dir, ModeControl, id, rnow)
	o.At = t0.Add(90 * time.Minute)
	if p, err = plan(o); err != nil || p.Lineage != a || p.DBs[0].Time != t0.Add(time.Hour) {
		t.Fatalf("plan %+v err %v", p, err)
	}
	// --lineage with nothing at or before --at.
	o = opts(dir, ModeControl, id, rnow)
	o.Lineage, o.At = "lineageB", t0
	if _, err = plan(o); err == nil {
		t.Fatal("expected error")
	}
	// Declined confirmations and refusals left nothing behind.
	sameTree(t, before, tree(t, dir))
	noLeftovers(t, dir)

	ls, err := ListLineages(rctx, obj, rnode, "")
	if err != nil || len(ls) != 2 || ls[0].ID != a || ls[0].Objects != 2 || ls[0].Control != 2 {
		t.Fatalf("list %+v err %v", ls, err)
	}
}

func TestRestoreProject(t *testing.T) {
	obj, id, w := setupR(t)
	lineage := w.lineage()
	w.project("p1", "restored")
	w.upload("projects/p1", lineage, t0)

	dir := newTarget(t, []string{"p1"}, map[string]string{"p1": "current"})
	ctlBefore := tree(t, dir)["control.db"]
	var plan *RestorePlan
	o := opts(dir, ModeProject, id, rnow)
	o.ProjectID = "p1"
	o.Confirm = func(p *RestorePlan) bool { plan = p; return true }
	res, err := Restore(rctx, obj, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Warnings) != 0 || plan.RemovesState || plan.NewInstanceID != "" {
		t.Fatalf("plan %+v", plan)
	}
	after := tree(t, dir)
	if after["control.db"] != ctlBefore {
		t.Fatal("project restore changed control.db")
	}
	if _, ok := after["backup-state.json"]; !ok {
		t.Fatal("project restore removed the state file")
	}
	if _, err := os.Stat(filepath.Join(res.PreRestoreDir, "projects", "p1.db")); err != nil {
		t.Fatal(err)
	}
	if v := readKV(t, dir, "p1"); v != "restored" {
		t.Fatalf("p1=%q", v)
	}

	// No tenant row in the current control.db: warn.
	w.project("p7", "x")
	w.upload("projects/p7", lineage, t0)
	o.ProjectID = "p7"
	if _, err := Restore(rctx, obj, o); err != nil {
		t.Fatal(err)
	}
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "p7") {
		t.Fatalf("warnings %v", plan.Warnings)
	}

	// Two lineages for the project, no --lineage: refuse.
	w.upload("projects/p1", "otherLineage", t0.Add(time.Hour))
	o.ProjectID = "p1"
	before := tree(t, dir)
	var amb *AmbiguousLineageError
	if _, err := Restore(rctx, obj, o); !errors.As(err, &amb) {
		t.Fatalf("got %v", err)
	}
	sameTree(t, before, tree(t, dir))
}

func TestRestoreMidSwapFailureReports(t *testing.T) {
	obj, id, w := setupR(t)
	lineage := w.lineage()
	w.tenant("p1")
	w.project("p1", "new1")
	w.upload("projects/p1", lineage, t0)
	w.upload("control", lineage, t0)
	dir := newTarget(t, []string{"p1"}, map[string]string{"p1": "old1"})
	oldCtl, _ := os.ReadFile(store.ControlPath(dir))

	o := opts(dir, ModeNode, id, rnow)
	// Sabotage the control.db install after validation: projects are swapped
	// first, then installing control.db fails.
	o.Confirm = func(p *RestorePlan) bool {
		os.Remove(store.ControlPath(dir) + restoreTmpExt)
		return true
	}
	_, err := Restore(rctx, obj, o)
	var se *SwapError
	if !errors.As(err, &se) {
		t.Fatalf("got %v", err)
	}
	if len(se.Installed) != 1 || se.Installed[0] != store.ProjectPath(dir, "p1") {
		t.Fatalf("installed %v", se.Installed)
	}
	msg := err.Error()
	if !strings.Contains(msg, "installed: "+store.ProjectPath(dir, "p1")) || !strings.Contains(msg, se.PreRestoreDir) {
		t.Fatalf("report: %s", msg)
	}
	if b, err := os.ReadFile(filepath.Join(se.PreRestoreDir, "control.db")); err != nil || !bytes.Equal(b, oldCtl) {
		t.Fatalf("original control.db not in pre-restore dir: %v", err)
	}
	if _, err := os.Stat(StatePath(dir)); err != nil {
		t.Fatal("state file removed despite failed control install")
	}
	noTmp(t, dir)
}

func TestRestoreControlHoldsMaintenance(t *testing.T) {
	obj, id, w := setupR(t)
	w.tenant("p1")
	for _, tk := range []store.Task{
		{ID: "t-restore", Name: "volume.restore", Node: rnode, Volume: "v1", Status: store.TaskPending},
		{ID: "t-trash", Name: "volume.trash", Node: rnode, Volume: "v2", Status: store.TaskPending},
		{ID: "t-run", Name: "volume.backup", Node: rnode, Volume: "v3", Status: store.TaskPending},
		{ID: "t-done", Name: "volume.backup", Node: rnode, Volume: "v4", Status: store.TaskPending},
	} {
		if _, err := w.st.CreateTask(rctx, tk); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := w.st.ClaimTask(rctx, "t-run"); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if err := w.st.UpdateTaskStatus(rctx, "t-done", store.TaskCompleted, nil); err != nil {
		t.Fatal(err)
	}
	w.upload("control", w.lineage(), t0)
	dir := newTarget(t, nil, nil)

	o := opts(dir, ModeControl, id, rnow)
	o.HoldBy = "alice"
	var plan *RestorePlan
	o.Confirm = func(p *RestorePlan) bool { plan = p; return true }
	if _, err := Restore(rctx, obj, o); err != nil {
		t.Fatal(err)
	}
	ids := func(ts []store.Task) string {
		var s []string
		for _, t := range ts {
			s = append(s, t.ID+":"+t.Status)
		}
		return strings.Join(s, ",")
	}
	if !plan.MaintenanceHold {
		t.Fatal("plan has no maintenance hold")
	}
	if got := ids(plan.OpenTasks); got != "t-run:running,t-trash:pending" {
		t.Fatalf("open tasks %s", got)
	}
	if got := ids(plan.CanceledTasks); got != "t-restore:pending" {
		t.Fatalf("canceled tasks %s", got)
	}

	st, err := store.Open(dir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m, err := st.GetMaintenance(rctx)
	if err != nil {
		t.Fatal(err)
	}
	if m.Local == nil || m.Local.Reason != MaintenanceReason || m.Local.By != "alice" || m.Controller != nil {
		t.Fatalf("maintenance %+v", m)
	}
	if m.InstanceID != plan.NewInstanceID {
		t.Fatalf("hold recorded under instance %q want %q", m.InstanceID, plan.NewInstanceID)
	}
	// The dispatcher's claim is refused while held.
	if ok, err := st.ClaimTask(rctx, "t-trash"); err != nil || ok {
		t.Fatalf("claim under hold: %v %v", ok, err)
	}
	if tk, ok, err := st.GetTask(rctx, "t-restore"); err != nil || !ok || tk.Status != store.TaskCancelled {
		t.Fatalf("restore task %+v %v %v", tk, ok, err)
	}
}

func TestRestoreProjectDefaultsToCurrentLineage(t *testing.T) {
	obj, id, w := setupR(t)
	dir := newTarget(t, []string{"p1"}, map[string]string{"p1": "current"})
	cur, err := store.InstanceIDInControlDB(store.ControlPath(dir))
	if err != nil || cur == "" {
		t.Fatalf("instance id %q %v", cur, err)
	}
	w.project("p1", "from-current")
	w.upload("projects/p1", cur, t0)
	w.project("p1", "from-other")
	w.upload("projects/p1", "otherLineage", t0.Add(time.Hour))

	var plan *RestorePlan
	o := opts(dir, ModeProject, id, rnow)
	o.ProjectID = "p1"
	o.Confirm = func(p *RestorePlan) bool { plan = p; return true }
	if _, err := Restore(rctx, obj, o); err != nil {
		t.Fatal(err)
	}
	if plan.Lineage != cur || readKV(t, dir, "p1") != "from-current" {
		t.Fatalf("lineage %q value %q", plan.Lineage, readKV(t, dir, "p1"))
	}
	if !strings.Contains(strings.Join(plan.Warnings, "\n"), "otherLineage") {
		t.Fatalf("expected a warning naming the other lineage, got %q", plan.Warnings)
	}

	// The current lineage has nothing at or before --at: the ambiguity rule
	// applies to the others (here only one, so it is chosen).
	o.At = t0.Add(-time.Minute)
	w.upload("projects/p1", "olderLineage", t0.Add(-time.Hour))
	if _, err := Restore(rctx, obj, o); err != nil {
		t.Fatal(err)
	}
	if plan.Lineage != "olderLineage" {
		t.Fatalf("fallback lineage %q", plan.Lineage)
	}
	w.upload("projects/p1", "olderLineage2", t0.Add(-2*time.Hour))
	var amb *AmbiguousLineageError
	if _, err := Restore(rctx, obj, o); !errors.As(err, &amb) {
		t.Fatalf("got %v", err)
	}
}
