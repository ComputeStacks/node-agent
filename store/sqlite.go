package store

import (
	"database/sql"
	"fmt"
	"time"

	// Pure-Go SQLite driver (no cgo, keeps CGO_ENABLED=0). Registered under the
	// driver name "sqlite".
	_ "modernc.org/sqlite"
)

// busyTimeout is how long SQLite waits for a held write lock before returning
// SQLITE_BUSY. A node is single-writer per DB file, so contention is brief; this
// just rides out a concurrent checkpoint/backup rather than failing the caller.
const busyTimeout = 5 * time.Second

// openSQLite opens a SQLite database at path with the durability/concurrency
// PRAGMAs every cs-agent DB shares (control.db and every per-project DB):
//
//   - journal_mode=WAL      one writer + concurrent non-blocking readers
//   - synchronous=FULL      a returned COMMIT is on disk (authoritative data)
//   - busy_timeout=…        wait out a transient lock instead of erroring
//   - foreign_keys=ON       enforce the FK constraints declared in the schema
//
// Open conns are capped low: SQLite is single-writer per file, so a large pool
// buys nothing and just multiplies file handles. WAL still gives readers
// concurrency at the file level.
func openSQLite(path string, txlockImmediate bool) (*sql.DB, error) {
	return openSQLiteMode(path, txlockImmediate, false)
}

// openSQLiteMode is openSQLite with an optional mustExist: when set, the file is
// opened read-write but never created (SQLite URI mode=rw), so a missing DB is an
// open error rather than a fresh empty file.
func openSQLiteMode(path string, txlockImmediate, mustExist bool) (*sql.DB, error) {
	// Driver-level PRAGMAs via the DSN query so they apply to every pooled
	// connection the driver opens, not just the first. busy_timeout is in ms.
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(%d)&_pragma=foreign_keys(ON)",
		path, busyTimeout.Milliseconds(),
	)
	if mustExist {
		dsn += "&mode=rw"
	}
	// control.db runs multi-statement write transactions (withControlTx); open
	// them IMMEDIATE so the write lock is taken at BEGIN, not lazily at the first
	// write. That makes the changelog seq allocation + commit always happen under
	// one held write lock (so the seq stays commit-ordered without relying on a
	// write-first convention) and removes the DEFERRED→write snapshot-upgrade BUSY
	// hazard. Per-project DBs use only single-statement autocommit writes, so they
	// stay DEFERRED.
	if txlockImmediate {
		dsn += "&_txlock=immediate"
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}

	// Single-writer per file. Cap open conns low; a deeper pool only multiplies
	// handles without improving write throughput.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxIdleTime(5 * time.Minute)

	// Force a real connection so a bad path / permission error surfaces here
	// rather than on first query.
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}

	return db, nil
}
