package store

import (
	"container/list"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ErrProjectDeleting is returned by acquire (and the methods that thread it)
// when a project's DB is being deleted concurrently. Callers treat it as
// "gone" — the project no longer exists.
var ErrProjectDeleting = errors.New("store: project db is being deleted")

// projectConn is one cached per-project DB handle plus its LRU + lease
// bookkeeping.
//
// Lifecycle invariant: a conn is closed exactly once, only when it is removed
// from the pool AND not pinned (refs == 0). evictToCap/sweepIdle/remove mark a
// pinned conn (closeWhenIdle) and defer the close to release(); an unpinned conn
// is closed immediately. This is what prevents a handle from being closed out
// from under an in-flight query (C1).
type projectConn struct {
	projectID string
	db        *sql.DB
	lastUsed  time.Time
	elem      *list.Element // position in lru while pooled; nil once unpooled
	refs      int           // outstanding leases (in-flight queries)

	// closeWhenIdle marks a conn that has been removed from the pool (evicted,
	// idle-swept, or deleted) while still pinned; release() closes it when refs
	// reaches 0.
	closeWhenIdle bool

	// closed is closed (the channel) once db.Close() has run. remove() waits on
	// it before unlinking files, so a delete never pulls files out from under an
	// in-flight query (which would surface as a raw SQLITE_IOERR).
	closed chan struct{}
}

// connPool is an LRU cache of open per-project *sql.DB handles. Per-project DBs
// are opened on demand (and migrated on first open). The number of OPEN handles
// is a SOFT cap: the least-recently-used UNPINNED handle is closed on eviction,
// but a handle with in-flight queries (refs>0) is never closed — the pool
// temporarily exceeds maxOpen rather than yank a live handle. An optional idle
// sweep closes unpinned handles unused for longer than idleTimeout.
//
// acquire/release lease handles under mu; SQLite's own concurrency is per
// *sql.DB. The pool serializes handle lifecycle so a handle is never closed
// mid-query, never opened twice for one project, and never resurrected during a
// delete.
type connPool struct {
	dir         string // <dataDir>/projects
	maxOpen     int
	idleTimeout time.Duration // 0 = no idle sweeping

	mu       sync.Mutex
	conns    map[string]*projectConn
	lru      *list.List      // *projectConn, front = most recently used
	deleting map[string]bool // projects currently being deleted (tombstone)

	// offPool counts in-flight out-of-pool snapshot connections per project.
	// remove() waits on offPoolCond (bound to mu) until a project's count drains
	// before unlinking files.
	offPool     map[string]int
	offPoolCond *sync.Cond

	// snapshotHook is a test seam: called after an out-of-pool snapshot has
	// registered itself and before it opens the file.
	snapshotHook func(projectID string)

	// migrate is injected so the pool stays decoupled from the per-project
	// schema; Store wires it to runMigrations(db, name, projectMigrations).
	migrate func(db *sql.DB, name string) error

	stopSweep chan struct{}
	sweepWG   sync.WaitGroup
	closeOnce sync.Once
}

func newConnPool(dir string, maxOpen int, idleTimeout time.Duration, migrate func(*sql.DB, string) error) *connPool {
	p := &connPool{
		dir:         dir,
		maxOpen:     maxOpen,
		idleTimeout: idleTimeout,
		conns:       make(map[string]*projectConn),
		lru:         list.New(),
		deleting:    make(map[string]bool),
		offPool:     make(map[string]int),
		migrate:     migrate,
	}
	p.offPoolCond = sync.NewCond(&p.mu)
	if idleTimeout > 0 {
		p.stopSweep = make(chan struct{})
		p.sweepWG.Add(1)
		go p.sweepLoop()
	}
	return p
}

// dbPath returns the on-disk path of a project's DB file.
func (p *connPool) dbPath(projectID string) string {
	return filepath.Join(p.dir, projectID+".db")
}

// acquire leases an open, migrated handle for projectID, opening it on demand,
// and returns a release closure the caller MUST invoke (defer) when done. While
// leased (refs>0) the handle will not be closed by eviction/idle-sweep/delete —
// the close is deferred to release(). Returns ErrProjectDeleting if a delete is
// in flight for this project.
func (p *connPool) acquire(projectID string) (*sql.DB, func(), error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.deleting[projectID] {
		return nil, nil, ErrProjectDeleting
	}

	c, ok := p.conns[projectID]
	if !ok {
		db, err := p.openAndMigrate(projectID)
		if err != nil {
			return nil, nil, err
		}
		c = &projectConn{projectID: projectID, db: db, closed: make(chan struct{})}
		c.elem = p.lru.PushFront(c)
		p.conns[projectID] = c
	} else {
		p.lru.MoveToFront(c.elem)
	}

	c.lastUsed = time.Now()
	c.refs++
	db := c.db

	// Try to honor the cap now (skips pinned conns); pinned conns just push us
	// temporarily over.
	p.evictToCap()

	return db, p.releaseFn(c), nil
}

// releaseFn returns the one-shot release closure for a leased conn.
func (p *connPool) releaseFn(c *projectConn) func() {
	return func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		c.refs--
		if c.refs <= 0 && c.closeWhenIdle {
			// Removed from the pool while we held it; close now that it's idle.
			p.closeConn(c)
		}
	}
}

// closeConn closes the conn's handle exactly once and signals its closed
// channel. Caller holds mu. All closes (evict, idle-sweep, delete, shutdown) go
// through here so anyone waiting on c.closed (remove) is reliably woken.
func (p *connPool) closeConn(c *projectConn) {
	select {
	case <-c.closed: // already closed; nothing to do
		return
	default:
	}
	_ = c.db.Close()
	close(c.closed)
}

// openAndMigrate opens the file (creating the projects dir as needed) and brings
// its schema current. A schema-version-guard failure (binary too old for the
// DB) closes the just-opened handle and propagates the clear error. Caller holds
// mu.
func (p *connPool) openAndMigrate(projectID string) (*sql.DB, error) {
	if err := os.MkdirAll(p.dir, 0o750); err != nil {
		return nil, fmt.Errorf("create projects dir: %w", err)
	}
	db, err := openSQLite(p.dbPath(projectID), false)
	if err != nil {
		return nil, err
	}
	if err := p.migrate(db, "projects/"+projectID+".db"); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// evictToCap unpools least-recently-used UNPINNED handles until at most maxOpen
// remain (or no more can be evicted because the rest are pinned). Caller holds
// mu.
func (p *connPool) evictToCap() {
	// Walk from the back (LRU); skip pinned conns. Stop once we're at/under cap
	// or there's nothing left to evict.
	e := p.lru.Back()
	for p.lru.Len() > p.maxOpen && e != nil {
		c := e.Value.(*projectConn)
		prev := e.Prev()
		if c.refs == 0 {
			p.unpool(c)
			p.closeConn(c)
		}
		// Pinned conns are left in place (soft cap); just move on.
		e = prev
	}
}

// unpool removes c from the LRU + conns map without closing it. Caller holds mu.
func (p *connPool) unpool(c *projectConn) {
	if c.elem != nil {
		p.lru.Remove(c.elem)
		c.elem = nil
	}
	delete(p.conns, c.projectID)
}

// remove deletes a project's DB. It plants a deleting tombstone (so a concurrent
// acquire can't resurrect the file), unpools the handle, then removes the
// .db/-wal/-shm files. If the handle is pinned (an in-flight query holds a
// lease), it marks the conn close-on-last-release and WAITS for that close
// before unlinking the files — so a delete never pulls files out from under a
// live query (which SQLite would surface as a raw I/O error). The tombstone is
// held for the whole operation and lifted at the end, so the id is reusable
// afterward. Absent files are ignored; removing an absent project is a no-op.
func (p *connPool) remove(projectID string) error {
	p.mu.Lock()
	// Tombstone first: blocks any acquire from (re)opening during the rm.
	p.deleting[projectID] = true

	var wait chan struct{}
	if c, ok := p.conns[projectID]; ok {
		p.unpool(c)
		if c.refs == 0 {
			p.closeConn(c)
		} else {
			// In use: defer the close to the last release, and wait for it
			// below before unlinking files.
			c.closeWhenIdle = true
			wait = c.closed
		}
	}
	p.mu.Unlock()

	// Block (lock released) until any in-flight lease drains and the handle is
	// closed. New acquires are refused by the tombstone meanwhile.
	if wait != nil {
		<-wait
	}

	// Likewise wait for any out-of-pool snapshot connection on this project
	// (new ones are refused by the tombstone).
	p.mu.Lock()
	for p.offPool[projectID] > 0 {
		p.offPoolCond.Wait()
	}
	p.mu.Unlock()

	base := p.dbPath(projectID)
	var firstErr error
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(base + suffix); err != nil && !os.IsNotExist(err) && firstErr == nil {
			firstErr = err
		}
	}

	// Lift the tombstone: the project id is reusable (a later CreateProjectDB
	// recreates a fresh file).
	p.mu.Lock()
	delete(p.deleting, projectID)
	p.mu.Unlock()

	return firstErr
}

// sweepLoop closes unpinned handles idle longer than idleTimeout. Runs on a
// ticker at idleTimeout cadence; stops when close is called.
func (p *connPool) sweepLoop() {
	defer p.sweepWG.Done()
	t := time.NewTicker(p.idleTimeout)
	defer t.Stop()
	for {
		select {
		case <-p.stopSweep:
			return
		case <-t.C:
			p.sweepIdle()
		}
	}
}

func (p *connPool) sweepIdle() {
	p.mu.Lock()
	defer p.mu.Unlock()
	cutoff := time.Now().Add(-p.idleTimeout)
	// Walk from the back (least recently used); stop at the first non-idle one.
	for e := p.lru.Back(); e != nil; {
		c := e.Value.(*projectConn)
		prev := e.Prev()
		if c.lastUsed.After(cutoff) {
			break
		}
		if c.refs == 0 { // never sweep a pinned handle
			p.unpool(c)
			p.closeConn(c)
		}
		e = prev
	}
}

// close stops the idle sweeper and closes every cached handle. Idempotent: a
// second call is a safe no-op (guarded by closeOnce). Assumes no leases are
// outstanding at shutdown.
func (p *connPool) close() error {
	var firstErr error
	p.closeOnce.Do(func() {
		if p.stopSweep != nil {
			close(p.stopSweep)
			p.sweepWG.Wait()
		}

		p.mu.Lock()
		defer p.mu.Unlock()
		for _, c := range p.conns {
			select {
			case <-c.closed: // already closed
			default:
				if err := c.db.Close(); err != nil && firstErr == nil {
					firstErr = err
				}
				close(c.closed)
			}
		}
		p.conns = make(map[string]*projectConn)
		p.lru.Init()
	})
	return firstErr
}

// openCount returns the number of currently-pooled handles (test/observability).
func (p *connPool) openCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.conns)
}
