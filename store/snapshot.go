package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ErrProjectNotFound is returned by SnapshotProject when the project's DB file
// does not exist. Nothing is created.
var ErrProjectNotFound = errors.New("store: project db not found")

// DBKind selects which schema CheckRestorable validates against.
type DBKind int

const (
	DBControl DBKind = iota
	DBProject
)

// DataDir returns the data directory the Store was opened on.
func (s *Store) DataDir() string { return s.dataDir }

// ControlPath is the on-disk path of control.db under dataDir.
func ControlPath(dataDir string) string { return filepath.Join(dataDir, "control.db") }

// ProjectPath is the on-disk path of a project's DB under dataDir.
func ProjectPath(dataDir, projectID string) string {
	return filepath.Join(dataDir, "projects", projectID+".db")
}

// ProjectIDs lists the project ids that have a DB file on disk (sorted). Files
// whose names are not valid project ids are ignored.
func (s *Store) ProjectIDs() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.dataDir, "projects"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: list projects: %w", err)
	}
	var ids []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".db") {
			continue
		}
		id := strings.TrimSuffix(name, ".db")
		if validateProjectID(id) != nil {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

func vacuumInto(ctx context.Context, db *sql.DB, dst string) error {
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("store: snapshot destination %s already exists", dst)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("store: snapshot destination: %w", err)
	}
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, dst); err != nil {
		return fmt.Errorf("store: vacuum into %s: %w", dst, err)
	}
	return nil
}

// SnapshotControl writes a consistent copy of control.db to dst (which must not
// exist) using VACUUM INTO on the store's own handle.
func (s *Store) SnapshotControl(ctx context.Context, dst string) error {
	return vacuumInto(ctx, s.control, dst)
}

// SnapshotProject writes a consistent copy of a project's DB to dst (which must
// not exist). A pooled project is leased from the pool; otherwise a short-lived
// connection outside the pool is used (no pool slot, no eviction, never creates
// the file). Returns ErrProjectNotFound if the file is absent and
// ErrProjectDeleting if a delete is in flight.
func (s *Store) SnapshotProject(ctx context.Context, projectID, dst string) error {
	if err := validateProjectID(projectID); err != nil {
		return err
	}
	p := s.pool
	p.mu.Lock()
	if p.deleting[projectID] {
		p.mu.Unlock()
		return ErrProjectDeleting
	}
	if c, ok := p.conns[projectID]; ok {
		// Pooled: lease exactly as acquire would, without risking a re-open.
		p.lru.MoveToFront(c.elem)
		c.refs++
		p.mu.Unlock()
		release := p.releaseFn(c)
		defer release()
		return vacuumInto(ctx, c.db, dst)
	}
	// Not pooled: register as in-flight so remove() waits for us.
	p.offPool[projectID]++
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.offPool[projectID]--
		if p.offPool[projectID] <= 0 {
			delete(p.offPool, projectID)
		}
		p.offPoolCond.Broadcast()
		p.mu.Unlock()
	}()

	path := p.dbPath(projectID)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return ErrProjectNotFound
		}
		return fmt.Errorf("store: snapshot project %q: %w", projectID, err)
	}
	if p.snapshotHook != nil {
		p.snapshotHook(projectID)
	}
	db, err := openSQLiteMode(path, false, true)
	if err != nil {
		if _, serr := os.Stat(path); os.IsNotExist(serr) {
			return ErrProjectNotFound
		}
		return err
	}
	defer db.Close()
	return vacuumInto(ctx, db, dst)
}

// RotateInstanceIDAt replaces control_meta.instance_id in a standalone control
// DB file with a fresh random id (same format as ensureInstanceID) and returns it.
func RotateInstanceIDAt(path string) (string, error) {
	db, err := openSQLiteMode(path, true, true)
	if err != nil {
		return "", err
	}
	defer db.Close()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("store: mint instance id: %w", err)
	}
	id := hex.EncodeToString(b[:])
	if _, err := db.Exec(`
		INSERT INTO control_meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, MetaInstanceID, id); err != nil {
		return "", fmt.Errorf("store: rotate instance id in %s: %w", path, err)
	}
	return id, nil
}

// openReadOnly opens an existing SQLite file read-only; it never creates it.
func openReadOnly(path string) (*sql.DB, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(%d)", path, busyTimeout.Milliseconds()))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// CheckRestorable verifies a snapshot file is safe to install: it passes
// PRAGMA integrity_check and its schema version is not newer than this binary.
func CheckRestorable(path string, kind DBKind) error {
	db, err := openReadOnly(path)
	if err != nil {
		return fmt.Errorf("store: check %s: %w", path, err)
	}
	defer db.Close()

	rows, err := db.Query(`PRAGMA integrity_check`)
	if err != nil {
		return fmt.Errorf("store: integrity_check %s: %w", path, err)
	}
	var results []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			rows.Close()
			return fmt.Errorf("store: integrity_check %s: %w", path, err)
		}
		results = append(results, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: integrity_check %s: %w", path, err)
	}
	if len(results) != 1 || results[0] != "ok" {
		if len(results) > 5 {
			results = results[:5]
		}
		return fmt.Errorf("store: %s failed integrity_check: %s", path, strings.Join(results, "; "))
	}

	_, onDisk, err := appliedVersions(db)
	if err != nil {
		return fmt.Errorf("store: %s has no readable schema_migrations: %w", path, err)
	}
	migs := controlMigrations
	if kind == DBProject {
		migs = projectMigrations
	}
	supported := 0
	if n := len(migs); n > 0 {
		supported = migs[n-1].version
	}
	if onDisk > supported {
		return fmt.Errorf("store: %s is schema v%d; this binary supports v%d", path, onDisk, supported)
	}
	return nil
}

// ProjectsInControlDB returns the project ids in the tenants table of a
// standalone control.db file (sorted, de-duplicated).
func ProjectsInControlDB(path string) ([]string, error) {
	db, err := openReadOnly(path)
	if err != nil {
		return nil, fmt.Errorf("store: read tenants from %s: %w", path, err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT DISTINCT project_id FROM tenants ORDER BY project_id`)
	if err != nil {
		return nil, fmt.Errorf("store: read tenants from %s: %w", path, err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(ids)
	return ids, nil
}

// OpenTasksInControlDB reads, read-only, the tasks in the control.db file at
// path that are not in a terminal state (pending or running), oldest first.
// The restore tool shows them before installing a restored control.db.
func OpenTasksInControlDB(path string) ([]Task, error) {
	db, err := openReadOnly(path)
	if err != nil {
		return nil, fmt.Errorf("store: read tasks from %s: %w", path, err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT `+taskColumns+` FROM tasks WHERE status IN (?, ?) ORDER BY created_at, id`,
		TaskPending, TaskRunning)
	if err != nil {
		return nil, fmt.Errorf("store: read tasks from %s: %w", path, err)
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// PutLocalHoldAt places the local maintenance hold (as `cs-agent maintenance
// on` does) in the control.db file at path, which no agent may have open. The
// file is first migrated to this binary's schema, as the agent would on start.
// Entering maintenance cancels pending volume.restore tasks, exactly as it does
// on a live node.
func PutLocalHoldAt(ctx context.Context, path, reason, by string) (MaintenanceState, error) {
	db, err := openSQLiteMode(path, true, true)
	if err != nil {
		return MaintenanceState{}, err
	}
	s := &Store{dataDir: filepath.Dir(path), control: db}
	defer s.Close()
	if err := runMigrations(db, "control.db", controlMigrations); err != nil {
		return MaintenanceState{}, err
	}
	m, _, err := s.PutLocalHold(ctx, reason, by)
	return m, err
}

// InstanceIDInControlDB reads, read-only, the instance_id recorded in the
// control.db file at path ("" if none is recorded).
func InstanceIDInControlDB(path string) (string, error) {
	db, err := openReadOnly(path)
	if err != nil {
		return "", fmt.Errorf("store: read instance id from %s: %w", path, err)
	}
	defer db.Close()
	var id string
	err = db.QueryRow(`SELECT value FROM control_meta WHERE key = ?`, MetaInstanceID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: read instance id from %s: %w", path, err)
	}
	return id, nil
}
