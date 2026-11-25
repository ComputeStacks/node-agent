package dbbackup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cs-agent/log"
	"cs-agent/s3upload"

	"github.com/getsentry/sentry-go"
	"github.com/hashicorp/go-hclog"
	"github.com/robfig/cron/v3"
)

const (
	defaultInterval  = 15 * time.Minute
	retentionEvery   = 24 * time.Hour
	tmpDirName       = ".backup-tmp"
	controlDBName    = "control"
	projectDBPrefix  = "projects/"
	deleteBatchLimit = 1000
)

// ObjectStore is the object storage the runner uploads to.
type ObjectStore interface {
	Upload(ctx context.Context, key string, r io.Reader) (int64, error)
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	List(ctx context.Context, prefix string) ([]s3upload.Object, error)
	Delete(ctx context.Context, keys []string) error
}

// Snapshotter produces consistent point-in-time copies of the agent's databases.
type Snapshotter interface {
	DataDir() string
	ProjectIDs() ([]string, error)
	InstanceID(ctx context.Context) (string, error)
	SnapshotControl(ctx context.Context, dst string) error
	SnapshotProject(ctx context.Context, projectID, dst string) error
}

// Runner takes and uploads snapshots on a cron schedule.
type Runner struct {
	cfg Config
	st  Snapshotter
	obj ObjectStore
	now func() time.Time

	mu    sync.Mutex // guards state and serializes runs
	state *stateFile

	// pub is a copy of state.Status published on every change, so Status()
	// never waits behind a run that holds mu for minutes.
	pub atomic.Pointer[Status]
}

func logger() hclog.Logger { return log.New().Named("dbbackup") }

// NewRunner validates cfg and loads persisted state.
func NewRunner(cfg Config, st Snapshotter, obj ObjectStore) (*Runner, error) {
	if st == nil || obj == nil {
		return nil, errors.New("dbbackup: snapshotter and object store are required")
	}
	if cfg.Enabled && len(cfg.Recipients) == 0 {
		return nil, errors.New("store.backup.recipients: at least one age recipient is required")
	}
	if cfg.NodeName == "" {
		h, err := os.Hostname()
		if err != nil || h == "" {
			return nil, errors.New("store.backup.node_name: unset and hostname unavailable")
		}
		cfg.NodeName = h
	}
	if cfg.RetentionDays <= 0 {
		cfg.RetentionDays = 30
	}
	r := &Runner{cfg: cfg, st: st, obj: obj, now: time.Now}
	state, corrupt := loadState(StatePath(st.DataDir()))
	if corrupt != nil {
		logger().Warn("Backup state unreadable; starting empty", "error", corrupt)
	}
	r.state = state
	r.publish()
	return r, nil
}

func nextFire(expr string, from time.Time) time.Time {
	s, err := cron.ParseStandard(expr)
	if err != nil {
		return time.Time{}
	}
	return s.Next(from)
}

// Run drives the backup loop until ctx is canceled.
func (r *Runner) Run(ctx context.Context) {
	if !r.cfg.Enabled {
		return
	}
	if err := r.clearTmp(); err != nil {
		logger().Warn("Could not clear backup temp dir", "error", err)
	}
	if nextFire(r.cfg.Freq, time.Now()).IsZero() {
		logger().Warn("Backup cron empty/unparseable; using default interval", "cron", r.cfg.Freq, "interval", defaultInterval.String())
	} else {
		logger().Info("Starting SQLite backups", "cron", r.cfg.Freq)
	}
	for {
		wait := defaultInterval
		if next := nextFire(r.cfg.Freq, time.Now()); !next.IsZero() {
			wait = time.Until(next)
			if wait < time.Second {
				wait = time.Second
			}
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
			if err := r.RunOnce(ctx); err != nil && ctx.Err() == nil {
				logger().Error("SQLite backup run failed", "error", err)
			}
		}
	}
}

func (r *Runner) tmpDir() string { return filepath.Join(r.st.DataDir(), tmpDirName) }

func (r *Runner) clearTmp() error {
	if err := os.RemoveAll(r.tmpDir()); err != nil {
		return err
	}
	return os.MkdirAll(r.tmpDir(), 0o700)
}

type outcome int

const (
	outUploaded outcome = iota
	outSkipped
)

// RunOnce performs one full pass over every database, then retention if due.
// It returns a summary error when any database failed, or ctx.Err() if canceled.
func (r *Runner) RunOnce(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now().UTC()
	r.state.Status.LastAttemptAt = &now
	r.publish()
	var failures []string
	var uploaded, skipped, failed int
	fail := func(what string, err error) {
		failed++
		failures = append(failures, fmt.Sprintf("%s: %v", what, err))
		logger().Error("SQLite backup failed", "db", what, "error", err)
	}

	lineage, err := r.st.InstanceID(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return r.finish(0, 0, 1, 0, []string{"instance id: " + err.Error()})
	}
	ids, idsErr := r.st.ProjectIDs()
	if idsErr != nil {
		fail("project list", idsErr)
	}
	dbs := append([]string{controlDBName}, prefixed(ids)...)
	r.state.Status.Lineage = lineage

	if err := os.MkdirAll(r.tmpDir(), 0o700); err != nil {
		return r.finish(0, 0, 1, len(dbs), []string{"temp dir: " + err.Error()})
	}
	dirty := false
	for _, db := range dbs {
		if ctx.Err() != nil {
			r.save()
			return ctx.Err()
		}
		out, d, err := r.backupOne(ctx, db, lineage, now)
		dirty = dirty || d
		if ctx.Err() != nil {
			r.save()
			return ctx.Err()
		}
		switch {
		case err != nil:
			fail(db, err)
		case out == outUploaded:
			uploaded++
		default:
			skipped++
		}
	}
	// forget state of projects that no longer exist
	if idsErr == nil {
		live := map[string]bool{controlDBName: true}
		for _, d := range dbs {
			live[d] = true
		}
		for k := range r.state.DBs {
			if !live[k] {
				delete(r.state.DBs, k)
				dirty = true
			}
		}
	}
	if dirty {
		r.save()
	}
	if idsErr == nil && now.Sub(time.Unix(r.state.LastRetentionAt, 0)) >= retentionEvery {
		_ = r.retention(ctx, now, ids)
	}
	return r.finish(uploaded, skipped, failed, len(dbs), failures)
}

func prefixed(ids []string) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = projectDBPrefix + id
	}
	return out
}

// finish records the run result, persists state and reports failures once.
func (r *Runner) finish(up, sk, fl, dbs int, failures []string) error {
	s := &r.state.Status
	s.Uploaded, s.Skipped, s.Failed, s.DBs = up, sk, fl, dbs
	var err error
	if fl > 0 {
		err = errors.New(strings.Join(failures, "; "))
		s.LastError = err.Error()
		t := r.now().UTC()
		s.LastErrorAt = &t
		sentry.CaptureException(fmt.Errorf("sqlite backup: %w", err))
	} else {
		t := r.now().UTC()
		s.LastSuccessAt = &t
	}
	r.save()
	return err
}

// publish copies the current status for Status(). Caller holds mu (or is
// the constructor).
func (r *Runner) publish() {
	s := r.state.Status
	s.LastSuccessAt = cloneTime(s.LastSuccessAt)
	s.LastAttemptAt = cloneTime(s.LastAttemptAt)
	s.LastErrorAt = cloneTime(s.LastErrorAt)
	r.pub.Store(&s)
}

func (r *Runner) save() {
	r.publish()
	if err := saveState(StatePath(r.st.DataDir()), r.state); err != nil {
		logger().Warn("Could not persist backup state", "error", err)
	}
}

func (r *Runner) dbPath(db string) string {
	if db == controlDBName {
		return filepath.Join(r.st.DataDir(), "control.db")
	}
	return filepath.Join(r.st.DataDir(), "projects", strings.TrimPrefix(db, projectDBPrefix)+".db")
}

// backupOne handles one database. dirty reports whether state changed.
func (r *Runner) backupOne(ctx context.Context, db, lineage string, now time.Time) (out outcome, dirty bool, err error) {
	path := r.dbPath(db)
	prev := r.state.DBs[db]
	m := takeMarker(path) // BEFORE the snapshot: a concurrent write re-triggers next run
	sameLineage := prev != nil && prev.Lineage == lineage && prev.Hash != ""
	if sameLineage && m.Valid && prev.Marker.Valid && prev.Marker == m {
		return outSkipped, false, nil
	}

	tmp := filepath.Join(r.tmpDir(), strings.ReplaceAll(db, "/", "_")+".db")
	os.Remove(tmp)
	defer os.Remove(tmp)

	if db == controlDBName {
		err = r.st.SnapshotControl(ctx, tmp)
	} else {
		err = r.st.SnapshotProject(ctx, strings.TrimPrefix(db, projectDBPrefix), tmp)
		if err != nil && r.cfg.IsProjectGone != nil && r.cfg.IsProjectGone(err) {
			logger().Info("Project vanished before backup; skipping", "db", db)
			if r.state.DBs[db] != nil {
				delete(r.state.DBs, db)
				dirty = true
			}
			return outSkipped, dirty, nil
		}
	}
	if err != nil {
		return 0, false, fmt.Errorf("snapshot: %w", err)
	}

	hash, err := hashFile(tmp)
	if err != nil {
		return 0, false, fmt.Errorf("hash: %w", err)
	}
	if sameLineage && prev.Hash == hash {
		prev.Marker = m
		return outSkipped, true, nil
	}

	key := KeyFor(r.cfg.NodeName, db, lineage, now)
	if err := r.upload(ctx, tmp, key); err != nil {
		return 0, false, fmt.Errorf("upload: %w", err)
	}
	r.state.DBs[db] = &dbState{Marker: m, Hash: hash, Key: key, Lineage: lineage, UploadedAt: now.Unix()}
	return outUploaded, true, nil
}

func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// upload streams file -> gzip -> age -> object store through a pipe.
func (r *Runner) upload(ctx context.Context, path, key string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		enc, err := Encrypt(pw, r.cfg.Recipients)
		if err == nil {
			_, err = io.Copy(enc, &ctxReader{ctx: ctx, r: f})
			if cerr := enc.Close(); err == nil {
				err = cerr
			}
		}
		pw.CloseWithError(err) // nil -> EOF
		done <- err
	}()
	_, uerr := r.obj.Upload(ctx, key, pr)
	if uerr != nil {
		pr.CloseWithError(uerr)
	} else {
		pr.Close()
	}
	werr := <-done
	if uerr != nil {
		return uerr
	}
	if werr != nil && !errors.Is(werr, io.ErrClosedPipe) {
		return werr
	}
	return ctx.Err()
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// retention lists this node's objects once and deletes what Keep says to.
// Errors are logged, never fatal.
func (r *Runner) retention(ctx context.Context, now time.Time, projectIDs []string) error {
	objs, err := r.obj.List(ctx, "nodes/"+r.cfg.NodeName+"/")
	if err != nil {
		logger().Warn("Backup retention: list failed", "error", err)
		return err
	}
	exists := map[string]bool{controlDBName: true}
	for _, id := range projectIDs {
		exists[projectDBPrefix+id] = true
	}
	byDB := map[string][]s3upload.Object{}
	for _, o := range objs {
		if _, db, _, _, ok := ParseKey(o.Key); ok {
			byDB[db] = append(byDB[db], o)
		}
	}
	var del []string
	for db, list := range byDB {
		del = append(del, Keep(list, now, r.cfg.KeepAll, r.cfg.RetentionDays, exists[db])...)
	}
	var derr error
	for len(del) > 0 {
		n := min(len(del), deleteBatchLimit)
		if err := r.obj.Delete(ctx, del[:n]); err != nil {
			logger().Warn("Backup retention: delete failed", "error", err)
			derr = err
			break
		}
		del = del[n:]
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	r.state.LastRetentionAt = now.Unix()
	return derr
}
