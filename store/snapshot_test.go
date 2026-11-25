package store

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSnapshotControl_ConcurrentWrites(t *testing.T) {
	s := open(t, Options{})
	if err := s.UpsertTenant(ctx, "p-seed", hash("seed"), "active"); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = s.UpsertTenant(ctx, "p-w"+string(rune('a'+i%20)), hash("w"+string(rune('a'+i%20))), "active")
		}
	}()
	dst := filepath.Join(t.TempDir(), "c.db")
	err := s.SnapshotControl(ctx, dst)
	close(stop)
	<-done
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckRestorable(dst, DBControl); err != nil {
		t.Fatal(err)
	}
	ids, err := ProjectsInControlDB(dst)
	if err != nil || len(ids) == 0 || ids[0] != "p-seed" && ids[len(ids)-1] != "p-seed" {
		// sorted: "p-seed" sorts after "p-w..."? 's' < 'w', so it is first.
		t.Fatalf("ids=%v err=%v", ids, err)
	}
	if err := s.SnapshotControl(ctx, dst); err == nil {
		t.Fatal("expected error when dst exists")
	}
}

func TestSnapshotProject_PooledAndUnpooled(t *testing.T) {
	s := open(t, Options{})
	if err := s.CreateProjectDB(ctx, "pa"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProjectDB(ctx, "pb"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := s.SnapshotProject(ctx, "pa", filepath.Join(dir, "pa.db")); err != nil {
		t.Fatal(err)
	}
	if err := CheckRestorable(filepath.Join(dir, "pa.db"), DBProject); err != nil {
		t.Fatal(err)
	}
	// Unpool pb.
	s.pool.mu.Lock()
	c := s.pool.conns["pb"]
	s.pool.unpool(c)
	s.pool.closeConn(c)
	s.pool.mu.Unlock()
	before := s.pool.openCount()
	if err := s.SnapshotProject(ctx, "pb", filepath.Join(dir, "pb.db")); err != nil {
		t.Fatal(err)
	}
	if after := s.pool.openCount(); after != before {
		t.Fatalf("openCount %d -> %d", before, after)
	}
	if err := CheckRestorable(filepath.Join(dir, "pb.db"), DBProject); err != nil {
		t.Fatal(err)
	}
	ids, err := s.ProjectIDs()
	if err != nil || len(ids) != 2 || ids[0] != "pa" || ids[1] != "pb" {
		t.Fatalf("ids=%v err=%v", ids, err)
	}
}

func TestSnapshotProject_Missing(t *testing.T) {
	s := open(t, Options{})
	dst := filepath.Join(t.TempDir(), "x.db")
	if err := s.SnapshotProject(ctx, "nope", dst); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("err=%v", err)
	}
	if _, err := os.Stat(dst); err == nil {
		t.Fatal("dst created")
	}
	if _, err := os.Stat(ProjectPath(s.DataDir(), "nope")); err == nil {
		t.Fatal("project file created")
	}
}

func TestRemoveWaitsForOutOfPoolSnapshot(t *testing.T) {
	s := open(t, Options{})
	if err := s.CreateProjectDB(ctx, "pw"); err != nil {
		t.Fatal(err)
	}
	s.pool.mu.Lock()
	c := s.pool.conns["pw"]
	s.pool.unpool(c)
	s.pool.closeConn(c)
	s.pool.mu.Unlock()

	reached := make(chan struct{})
	release := make(chan struct{})
	s.pool.snapshotHook = func(string) { close(reached); <-release }

	snapErr := make(chan error, 1)
	dst := filepath.Join(t.TempDir(), "pw.db")
	go func() { snapErr <- s.SnapshotProject(ctx, "pw", dst) }()
	<-reached

	removed := make(chan error, 1)
	go func() { removed <- s.pool.remove("pw") }()
	select {
	case <-removed:
		t.Fatal("remove did not wait for snapshot")
	case <-time.After(200 * time.Millisecond):
	}
	// Snapshot must not start while deleting.
	if err := s.SnapshotProject(ctx, "pw", filepath.Join(t.TempDir(), "z.db")); !errors.Is(err, ErrProjectDeleting) {
		t.Fatalf("err=%v", err)
	}
	close(release)
	if err := <-snapErr; err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if err := <-removed; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ProjectPath(s.DataDir(), "pw")); err == nil {
		t.Fatal("file not removed")
	}
}

func TestRotateInstanceIDAt(t *testing.T) {
	s := open(t, Options{})
	old, err := s.InstanceID(ctx)
	if err != nil || old == "" {
		t.Fatalf("old=%q err=%v", old, err)
	}
	dst := filepath.Join(t.TempDir(), "c.db")
	if err := s.SnapshotControl(ctx, dst); err != nil {
		t.Fatal(err)
	}
	id, err := RotateInstanceIDAt(dst)
	if err != nil || id == old || len(id) != len(old) {
		t.Fatalf("id=%q old=%q err=%v", id, old, err)
	}
	db, _ := sql.Open("sqlite", "file:"+dst+"?mode=ro")
	defer db.Close()
	var got string
	if err := db.QueryRow(`SELECT value FROM control_meta WHERE key='instance_id'`).Scan(&got); err != nil || got != id {
		t.Fatalf("got=%q err=%v", got, err)
	}
}

func TestCheckRestorable_Rejects(t *testing.T) {
	s := open(t, Options{})
	dir := t.TempDir()
	good := filepath.Join(dir, "good.db")
	if err := s.SnapshotControl(ctx, good); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(good)

	garbage := filepath.Join(dir, "garbage.db")
	_ = os.WriteFile(garbage, []byte("this is not a sqlite database at all, nope"), 0o600)
	if err := CheckRestorable(garbage, DBControl); err == nil {
		t.Fatal("garbage accepted")
	}
	trunc := filepath.Join(dir, "trunc.db")
	_ = os.WriteFile(trunc, data[:len(data)/2], 0o600)
	if err := CheckRestorable(trunc, DBControl); err == nil {
		t.Fatal("truncated accepted")
	}
	if err := CheckRestorable(filepath.Join(dir, "missing.db"), DBControl); err == nil {
		t.Fatal("missing accepted")
	}

	newer := filepath.Join(dir, "newer.db")
	if err := s.SnapshotControl(ctx, newer); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+newer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (9999, 0)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := CheckRestorable(newer, DBControl); err == nil {
		t.Fatal("too-new schema accepted")
	}

	nomig := filepath.Join(dir, "nomig.db")
	db, _ = sql.Open("sqlite", "file:"+nomig)
	_, _ = db.Exec(`CREATE TABLE x (a)`)
	db.Close()
	if err := CheckRestorable(nomig, DBControl); err == nil {
		t.Fatal("missing schema_migrations accepted")
	}
	if err := CheckRestorable(good, DBControl); err != nil {
		t.Fatal(err)
	}
}
