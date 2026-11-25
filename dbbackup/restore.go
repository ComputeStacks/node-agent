package dbbackup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"cs-agent/store"

	"filippo.io/age"
)

// Restore modes.
const (
	ModeControl = "control"
	ModeProject = "project"
	ModeNode    = "node"
)

const (
	controlDB      = "control"
	projectsPrefix = "projects/"
	restoreTmpExt  = ".restore-tmp"
)

// ErrAborted is returned when the operator declines the confirmation prompt.
var ErrAborted = errors.New("restore aborted; nothing was changed")

// sqliteSiblings are the files SQLite may keep next to a database.
var sqliteSiblings = []string{"-wal", "-shm", "-journal"}

// LineageInfo summarizes the snapshots of one lineage (one control.db
// instance_id) for a node.
type LineageInfo struct {
	ID       string
	First    time.Time
	Last     time.Time
	Objects  int
	Control  int // number of control.db snapshots
	Projects []string
}

// AmbiguousLineageError is returned when more than one lineage qualifies and
// the operator did not pick one with --lineage.
type AmbiguousLineageError struct {
	What     string
	Lineages []LineageInfo
}

func (e *AmbiguousLineageError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "more than one lineage has %s snapshots; choose one with --lineage:", e.What)
	for _, l := range e.Lineages {
		fmt.Fprintf(&b, "\n  %s  first %s  last %s  objects %d",
			l.ID, l.First.Format(time.RFC3339), l.Last.Format(time.RFC3339), l.Objects)
	}
	return b.String()
}

type snapObj struct {
	key     string
	db      string
	lineage string
	ts      time.Time
	size    int64
}

// validID mirrors the store's project id rules: a bare file name that cannot
// escape the projects directory.
func validID(id string) bool {
	return id != "" && id == filepath.Base(id) && !strings.ContainsAny(id, "/\\\x00") &&
		!strings.Contains(id, "..") && !strings.HasPrefix(id, ".") && !strings.HasPrefix(id, "-")
}

func validNode(node string) bool {
	return node != "" && !strings.ContainsAny(node, "/\x00") && node != "." && node != ".."
}

func listSnaps(ctx context.Context, obj ObjectStore, node string) ([]snapObj, error) {
	if !validNode(node) {
		return nil, fmt.Errorf("invalid node name %q", node)
	}
	objs, err := obj.List(ctx, "nodes/"+node+"/")
	if err != nil {
		return nil, fmt.Errorf("list snapshots for node %s: %w", node, err)
	}
	var out []snapObj
	for _, o := range objs {
		n, db, lineage, ts, ok := ParseKey(o.Key)
		if !ok || n != node {
			continue
		}
		if strings.HasPrefix(db, projectsPrefix) && !validID(strings.TrimPrefix(db, projectsPrefix)) {
			continue
		}
		out = append(out, snapObj{key: o.Key, db: db, lineage: lineage, ts: ts, size: o.Size})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].ts.Equal(out[j].ts) {
			return out[i].ts.Before(out[j].ts)
		}
		return out[i].key < out[j].key
	})
	return out, nil
}

func summarize(snaps []snapObj) []LineageInfo {
	by := map[string]*LineageInfo{}
	projects := map[string]map[string]bool{}
	for _, s := range snaps {
		l := by[s.lineage]
		if l == nil {
			l = &LineageInfo{ID: s.lineage, First: s.ts, Last: s.ts}
			by[s.lineage] = l
			projects[s.lineage] = map[string]bool{}
		}
		if s.ts.Before(l.First) {
			l.First = s.ts
		}
		if s.ts.After(l.Last) {
			l.Last = s.ts
		}
		l.Objects++
		if s.db == controlDB {
			l.Control++
		} else if id := strings.TrimPrefix(s.db, projectsPrefix); !projects[s.lineage][id] {
			projects[s.lineage][id] = true
			l.Projects = append(l.Projects, id)
		}
	}
	out := make([]LineageInfo, 0, len(by))
	for _, l := range by {
		sort.Strings(l.Projects)
		out = append(out, *l)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].First.Equal(out[j].First) {
			return out[i].First.Before(out[j].First)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// ListLineages groups a node's snapshots by lineage. db filters to one
// database ("control" or "projects/<id>"); empty means all.
func ListLineages(ctx context.Context, obj ObjectStore, node, db string) ([]LineageInfo, error) {
	snaps, err := listSnaps(ctx, obj, node)
	if err != nil {
		return nil, err
	}
	if db != "" {
		snaps = filterSnaps(snaps, func(s snapObj) bool { return s.db == db })
	}
	return summarize(snaps), nil
}

func filterSnaps(snaps []snapObj, keep func(snapObj) bool) []snapObj {
	var out []snapObj
	for _, s := range snaps {
		if keep(s) {
			out = append(out, s)
		}
	}
	return out
}

// newest returns the newest snapshot of db in lineage at or before at.
func newest(snaps []snapObj, db, lineage string, at time.Time) *snapObj {
	var best *snapObj
	for i := range snaps {
		s := &snaps[i]
		if s.db != db || s.lineage != lineage || s.ts.After(at) {
			continue
		}
		if best == nil || s.ts.After(best.ts) || (s.ts.Equal(best.ts) && s.key > best.key) {
			best = s
		}
	}
	return best
}

// pickLineage chooses the lineage for db: the explicit one if given (which
// must have a snapshot of db at or before at), else the only lineage that has
// one; more than one is an AmbiguousLineageError.
func pickLineage(snaps []snapObj, db, explicit string, at time.Time) (string, error) {
	inWindow := filterSnaps(snaps, func(s snapObj) bool { return s.db == db && !s.ts.After(at) })
	if explicit != "" {
		for _, s := range inWindow {
			if s.lineage == explicit {
				return explicit, nil
			}
		}
		return "", fmt.Errorf("lineage %s has no %s snapshot at or before %s", explicit, db, at.Format(time.RFC3339))
	}
	ls := summarize(inWindow)
	switch len(ls) {
	case 0:
		return "", fmt.Errorf("no %s snapshot at or before %s", db, at.Format(time.RFC3339))
	case 1:
		return ls[0].ID, nil
	default:
		return "", &AmbiguousLineageError{What: db, Lineages: ls}
	}
}

// RestoreOptions configures Restore.
type RestoreOptions struct {
	Mode         string // ModeControl, ModeProject or ModeNode
	DataDir      string
	Node         string
	ProjectID    string    // ModeProject only
	At           time.Time // zero means Now()
	Lineage      string    // empty: must be unambiguous
	AllowMissing bool      // ModeNode: tolerate tenants with no project snapshot
	Identities   []age.Identity
	HoldBy       string           // control/node: recorded as the maintenance hold's "by"
	Now          func() time.Time // defaults to time.Now
	// Confirm is called once every selected DB has been downloaded and
	// validated, before anything on disk is changed. Returning false aborts
	// with ErrAborted. Nil means proceed.
	Confirm func(*RestorePlan) bool
}

// PlannedDB is one database the restore will install.
type PlannedDB struct {
	DB         string // "control" or "projects/<id>"
	Key        string
	ObjectSize int64 // encrypted object size
	Size       int64 // restored file size
	Lineage    string
	Time       time.Time
	Target     string   // final path
	MoveAside  []string // existing files at Target that will be moved aside
	tmp        string
	kind       store.DBKind
}

// RestorePlan is what Restore will do once confirmed.
type RestorePlan struct {
	Mode           string
	Node           string
	Lineage        string
	At             time.Time
	DataDir        string
	PreRestoreDir  string // where originals go (a suffix is added if it exists)
	DBs            []PlannedDB
	Orphans        []string // ModeNode: on-disk project files not in the restored tenants, moved aside
	MissingTenants []string // ModeNode with AllowMissing: tenants left as they are on disk
	NewInstanceID  string   // control restores: the rotated instance_id
	// Control restores install the control.db with a local maintenance hold
	// (MaintenanceReason), so nothing it holds runs until an operator runs
	// `cs-agent maintenance off`. OpenTasks are its pending and running
	// tasks as installed; CanceledTasks are the pending volume.restore tasks
	// that placing the hold canceled.
	MaintenanceHold bool
	OpenTasks       []store.Task
	CanceledTasks   []store.Task
	RemovesState    bool
	Warnings        []string
}

// RestoreResult reports a completed restore.
type RestoreResult struct {
	Plan          *RestorePlan
	PreRestoreDir string   // empty when nothing needed moving
	Moved         []string // "src -> dst"
	Installed     []string // final paths
}

// SwapError is returned when the restore failed after it started changing
// files. Nothing is rolled back; it says exactly what was done.
type SwapError struct {
	PreRestoreDir string
	Moved         []string
	Installed     []string
	Err           error
}

func (e *SwapError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "restore stopped part-way: %v", e.Err)
	if len(e.Installed) == 0 {
		b.WriteString("\n  no database was installed")
	}
	for _, p := range e.Installed {
		fmt.Fprintf(&b, "\n  installed: %s", p)
	}
	for _, m := range e.Moved {
		fmt.Fprintf(&b, "\n  moved aside: %s", m)
	}
	if e.PreRestoreDir != "" {
		fmt.Fprintf(&b, "\n  originals are in %s", e.PreRestoreDir)
	}
	return b.String()
}

func (e *SwapError) Unwrap() error { return e.Err }

// Restore downloads, validates and installs snapshots according to opts. It
// holds the data dir lock for the whole operation, validates every selected DB
// before touching anything, and never deletes existing databases (they are
// moved into <data_dir>/pre-restore-<ts>/).
func Restore(ctx context.Context, obj ObjectStore, opts RestoreOptions) (*RestoreResult, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.DataDir == "" {
		return nil, errors.New("data dir is not set")
	}
	if len(opts.Identities) == 0 {
		return nil, errors.New("no age identity given")
	}
	switch opts.Mode {
	case ModeControl, ModeNode:
	case ModeProject:
		if !validID(opts.ProjectID) {
			return nil, fmt.Errorf("invalid project id %q", opts.ProjectID)
		}
	default:
		return nil, fmt.Errorf("unknown restore mode %q", opts.Mode)
	}
	at := opts.At
	if at.IsZero() {
		at = opts.Now()
	}
	at = at.UTC()

	release, err := store.LockDataDir(opts.DataDir)
	if err != nil {
		if errors.Is(err, store.ErrDataDirLocked) {
			return nil, fmt.Errorf("%w: the agent must be stopped first (systemctl stop cs-agent)", err)
		}
		return nil, err
	}
	defer release()

	snaps, err := listSnaps(ctx, obj, opts.Node)
	if err != nil {
		return nil, err
	}

	r := &restorer{ctx: ctx, obj: obj, opts: opts, snaps: snaps}
	plan := &RestorePlan{Mode: opts.Mode, Node: opts.Node, At: at, DataDir: opts.DataDir,
		PreRestoreDir: filepath.Join(opts.DataDir, "pre-restore-"+opts.Now().UTC().Format(tsLayout))}
	r.plan = plan
	defer r.cleanupTmp()

	if err := r.prepare(at); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.Confirm != nil && !opts.Confirm(plan) {
		return nil, ErrAborted
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return r.swap()
}

type restorer struct {
	ctx   context.Context
	obj   ObjectStore
	opts  RestoreOptions
	snaps []snapObj
	plan  *RestorePlan
	tmps  []string // tmp files to remove unless installed
}

func (r *restorer) prepare(at time.Time) error {
	dataDir := r.opts.DataDir
	switch r.opts.Mode {
	case ModeProject:
		db := projectsPrefix + r.opts.ProjectID
		lineage := ""
		if r.opts.Lineage == "" {
			// Default to the lineage of the control.db in place, when it has
			// a snapshot of this project in the window: after a control
			// restore or a reimage, older lineages would otherwise make
			// every project restore ambiguous.
			if cur, err := store.InstanceIDInControlDB(store.ControlPath(dataDir)); err == nil && cur != "" &&
				newest(r.snaps, db, cur, at) != nil {
				lineage = cur
			}
		}
		if lineage == "" {
			var err error
			lineage, err = pickLineage(r.snaps, db, r.opts.Lineage, at)
			if err != nil {
				return err
			}
		}
		r.plan.Lineage = lineage
		if err := r.fetch(db, lineage, at); err != nil {
			return err
		}
		// Warn when the current control.db does not know this project.
		tenants, err := store.ProjectsInControlDB(store.ControlPath(dataDir))
		switch {
		case err != nil:
			r.plan.Warnings = append(r.plan.Warnings,
				fmt.Sprintf("could not read the current control.db tenants (%v); the project may not be served", err))
		case !contains(tenants, r.opts.ProjectID):
			r.plan.Warnings = append(r.plan.Warnings,
				fmt.Sprintf("the current control.db has no tenant row for project %s; its data will not be served until the controller re-provisions it", r.opts.ProjectID))
		}
		return nil

	case ModeControl, ModeNode:
		lineage, err := pickLineage(r.snaps, controlDB, r.opts.Lineage, at)
		if err != nil {
			return err
		}
		r.plan.Lineage = lineage
		if err := r.fetch(controlDB, lineage, at); err != nil {
			return err
		}
		ctlTmp := r.plan.DBs[0].tmp // fetch(control) is always DBs[0]
		if r.opts.Mode == ModeNode {
			tenants, err := store.ProjectsInControlDB(ctlTmp)
			if err != nil {
				return fmt.Errorf("read tenants from restored control.db: %w", err)
			}
			restoring := map[string]bool{}
			var missing []string
			for _, id := range tenants {
				if !validID(id) {
					return fmt.Errorf("restored control.db has an invalid project id %q", id)
				}
				if newest(r.snaps, projectsPrefix+id, lineage, at) == nil {
					missing = append(missing, id)
					continue
				}
				restoring[id] = true
			}
			if len(missing) > 0 && !r.opts.AllowMissing {
				return fmt.Errorf("lineage %s has no snapshot at or before %s for tenant project(s): %s (use --allow-missing to restore without them)",
					lineage, at.Format(time.RFC3339), strings.Join(missing, ", "))
			}
			r.plan.MissingTenants = missing
			for _, id := range tenants {
				if restoring[id] {
					if err := r.fetch(projectsPrefix+id, lineage, at); err != nil {
						return err
					}
				}
			}
			keep := map[string]bool{}
			for _, id := range tenants {
				keep[id] = true
			}
			orphans, err := orphanProjectFiles(dataDir, keep)
			if err != nil {
				return err
			}
			r.plan.Orphans = orphans
		}
		// Rotate the lineage in the restored control.db before it is
		// installed, then make the file self-contained and re-check it.
		id, err := store.RotateInstanceIDAt(ctlTmp)
		if err != nil {
			return fmt.Errorf("rotate instance_id: %w", err)
		}
		if id == "" || id == lineage {
			return fmt.Errorf("rotate instance_id: got %q", id)
		}
		r.plan.NewInstanceID = id
		if err := r.holdMaintenance(ctlTmp); err != nil {
			return err
		}
		if err := finalizeTmp(ctlTmp, store.DBControl); err != nil {
			return fmt.Errorf("control.db after rotation: %w", err)
		}
		if fi, err := os.Stat(ctlTmp); err == nil {
			r.plan.DBs[0].Size = fi.Size()
		}
		r.plan.RemovesState = true
		return nil
	}
	return fmt.Errorf("unknown restore mode %q", r.opts.Mode)
}

// MaintenanceReason is the reason recorded on the local maintenance hold a
// control restore leaves in place.
const MaintenanceReason = "store-restore: review restored tasks, then run cs-agent maintenance off"

// holdMaintenance places the local maintenance hold in the restored control.db
// at path and records its open tasks in the plan. The restored file can hold
// tasks that already ran after the snapshot was taken (a volume.restore or
// volume.trash, say); the hold keeps the dispatcher from running them until an
// operator has looked.
func (r *restorer) holdMaintenance(path string) error {
	before, err := store.OpenTasksInControlDB(path)
	if err != nil {
		return fmt.Errorf("read tasks from restored control.db: %w", err)
	}
	if _, err := store.PutLocalHoldAt(r.ctx, path, MaintenanceReason, r.opts.HoldBy); err != nil {
		return fmt.Errorf("place maintenance hold in restored control.db: %w", err)
	}
	after, err := store.OpenTasksInControlDB(path)
	if err != nil {
		return fmt.Errorf("read tasks from restored control.db: %w", err)
	}
	still := map[string]bool{}
	for _, t := range after {
		still[t.ID] = true
	}
	for _, t := range before {
		if !still[t.ID] {
			r.plan.CanceledTasks = append(r.plan.CanceledTasks, t)
		}
	}
	r.plan.OpenTasks = after
	r.plan.MaintenanceHold = true
	return nil
}

// fetch downloads, decrypts and validates the newest snapshot of db in lineage
// at or before at into <target>.restore-tmp and adds it to the plan.
func (r *restorer) fetch(db, lineage string, at time.Time) error {
	s := newest(r.snaps, db, lineage, at)
	if s == nil {
		return fmt.Errorf("lineage %s has no %s snapshot at or before %s", lineage, db, at.Format(time.RFC3339))
	}
	if err := r.ctx.Err(); err != nil {
		return err
	}
	p := PlannedDB{DB: db, Key: s.key, ObjectSize: s.size, Lineage: lineage, Time: s.ts}
	if db == controlDB {
		p.Target, p.kind = store.ControlPath(r.opts.DataDir), store.DBControl
	} else {
		id := strings.TrimPrefix(db, projectsPrefix)
		p.Target, p.kind = store.ProjectPath(r.opts.DataDir, id), store.DBProject
		if err := os.MkdirAll(filepath.Dir(p.Target), 0o750); err != nil {
			return fmt.Errorf("create projects dir: %w", err)
		}
	}
	p.tmp = p.Target + restoreTmpExt
	removeWithSiblings(p.tmp)
	r.tmps = append(r.tmps, p.tmp)

	n, err := download(r.ctx, r.obj, s.key, r.opts.Identities, p.tmp)
	if err != nil {
		return fmt.Errorf("%s (%s): %w", db, s.key, err)
	}
	p.Size = n
	if err := finalizeTmp(p.tmp, p.kind); err != nil {
		return fmt.Errorf("%s (%s): %w", db, s.key, err)
	}
	p.MoveAside = existingFiles(p.Target)
	r.plan.DBs = append(r.plan.DBs, p)
	return nil
}

func download(ctx context.Context, obj ObjectStore, key string, ids []age.Identity, dst string) (int64, error) {
	rc, err := obj.Get(ctx, key)
	if err != nil {
		return 0, fmt.Errorf("download: %w", err)
	}
	defer rc.Close()
	plain, err := Decrypt(&ctxReader{ctx: ctx, r: rc}, ids)
	if err != nil {
		return 0, fmt.Errorf("decrypt: %w", err)
	}
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, plain)
	if err != nil {
		f.Close()
		return n, fmt.Errorf("decrypt: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return n, err
	}
	return n, f.Close()
}

// finalizeTmp checks a downloaded DB, converts it to a self-contained
// rollback-journal file (no -wal/-shm needed or left behind; the daemon
// switches it back to WAL on open), re-checks it and fsyncs it.
func finalizeTmp(path string, kind store.DBKind) error {
	if err := store.CheckRestorable(path, kind); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=rw")
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		db.Close()
		return fmt.Errorf("checkpoint: %w", err)
	}
	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode=DELETE`).Scan(&mode); err != nil {
		db.Close()
		return fmt.Errorf("set journal_mode: %w", err)
	}
	if err := db.Close(); err != nil {
		return err
	}
	if mode != "delete" {
		return fmt.Errorf("journal_mode is %q, want delete", mode)
	}
	if err := store.CheckRestorable(path, kind); err != nil {
		return err
	}
	for _, s := range sqliteSiblings {
		if _, err := os.Lstat(path + s); err == nil {
			return fmt.Errorf("%s%s still exists after checkpoint", path, s)
		}
	}
	return syncPath(path)
}

func syncPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func existingFiles(dbPath string) []string {
	var out []string
	for _, p := range append([]string{dbPath}, dbPath+"-wal", dbPath+"-shm") {
		if _, err := os.Lstat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

func removeWithSiblings(p string) {
	_ = os.Remove(p)
	for _, s := range sqliteSiblings {
		_ = os.Remove(p + s)
	}
}

func (r *restorer) cleanupTmp() {
	for _, p := range r.tmps {
		removeWithSiblings(p)
	}
}

// orphanProjectFiles lists project DB files (and their -wal/-shm) in
// <dataDir>/projects whose id is not in keep.
func orphanProjectFiles(dataDir string, keep map[string]bool) ([]string, error) {
	dir := filepath.Join(dataDir, "projects")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list %s: %w", dir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		base := strings.TrimSuffix(strings.TrimSuffix(name, "-wal"), "-shm")
		if !strings.HasSuffix(base, ".db") {
			continue
		}
		id := strings.TrimSuffix(base, ".db")
		if !validID(id) || keep[id] {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	sort.Strings(out)
	return out, nil
}

// swap moves the existing files aside and installs the validated tmp files.
// Projects go first and control.db last.
func (r *restorer) swap() (*RestoreResult, error) {
	dataDir := r.opts.DataDir
	res := &RestoreResult{Plan: r.plan}
	fail := func(err error) (*RestoreResult, error) {
		r.syncDirs(res.PreRestoreDir)
		return res, &SwapError{PreRestoreDir: res.PreRestoreDir, Moved: res.Moved, Installed: res.Installed, Err: err}
	}

	order := make([]int, 0, len(r.plan.DBs))
	for i, d := range r.plan.DBs {
		if d.DB != controlDB {
			order = append(order, i)
		}
	}
	for i, d := range r.plan.DBs {
		if d.DB == controlDB {
			order = append(order, i)
		}
	}

	moveAside := func(src string) error {
		if res.PreRestoreDir == "" {
			dir, err := makePreRestoreDir(r.plan.PreRestoreDir)
			if err != nil {
				return err
			}
			res.PreRestoreDir = dir
		}
		rel, err := filepath.Rel(dataDir, src)
		if err != nil || strings.HasPrefix(rel, "..") {
			return fmt.Errorf("%s is outside the data dir", src)
		}
		dst := filepath.Join(res.PreRestoreDir, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			return err
		}
		if _, err := os.Lstat(dst); err == nil {
			return fmt.Errorf("move %s: %s already exists", src, dst)
		}
		if err := os.Rename(src, dst); err != nil {
			return fmt.Errorf("move %s aside: %w", src, err)
		}
		res.Moved = append(res.Moved, src+" -> "+dst)
		return nil
	}

	for _, i := range order {
		d := r.plan.DBs[i]
		for _, src := range existingFiles(d.Target) {
			if err := moveAside(src); err != nil {
				return fail(err)
			}
		}
		if err := os.Rename(d.tmp, d.Target); err != nil {
			return fail(fmt.Errorf("install %s: %w", d.Target, err))
		}
		res.Installed = append(res.Installed, d.Target)
		if d.DB == controlDB {
			// Before moving on, make the next steps see the new file.
			r.syncDirs(res.PreRestoreDir)
		}
	}
	for _, src := range r.plan.Orphans {
		if _, err := os.Lstat(src); os.IsNotExist(err) {
			continue
		}
		if err := moveAside(src); err != nil {
			return fail(err)
		}
	}
	if r.plan.RemovesState {
		if err := os.Remove(StatePath(dataDir)); err != nil && !os.IsNotExist(err) {
			return fail(fmt.Errorf("remove backup state file: %w", err))
		}
	}
	if err := r.syncDirs(res.PreRestoreDir); err != nil {
		return fail(fmt.Errorf("fsync directories: %w", err))
	}
	return res, nil
}

func (r *restorer) syncDirs(pre string) error {
	dirs := []string{r.opts.DataDir, filepath.Join(r.opts.DataDir, "projects")}
	if pre != "" {
		dirs = append(dirs, pre, filepath.Join(pre, "projects"))
	}
	var first error
	for _, d := range dirs {
		if err := syncPath(d); err != nil && !os.IsNotExist(err) && first == nil {
			first = err
		}
	}
	return first
}

// makePreRestoreDir creates base, or base-2, base-3, ... if it exists, so
// earlier originals are never overwritten.
func makePreRestoreDir(base string) (string, error) {
	for i := 1; i < 1000; i++ {
		dir := base
		if i > 1 {
			dir = fmt.Sprintf("%s-%d", base, i)
		}
		err := os.Mkdir(dir, 0o750)
		if err == nil {
			return dir, nil
		}
		if !os.IsExist(err) {
			return "", fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return "", fmt.Errorf("create %s: too many existing pre-restore dirs", base)
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
