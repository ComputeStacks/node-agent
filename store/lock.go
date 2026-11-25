package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// LockFileName is the advisory lock file under the data dir. The daemon holds
// it for its whole lifetime; the restore command holds it while it replaces
// databases, so the two never run at the same time.
const LockFileName = "agent.lock"

// ErrDataDirLocked means another process (normally the running agent) holds
// the data dir lock.
var ErrDataDirLocked = errors.New("store: data dir is locked by another process")

// LockDataDir takes an exclusive, non-blocking flock on <dataDir>/agent.lock,
// creating the directory and the file if needed. It returns ErrDataDirLocked
// (wrapped) when the lock is already held. The lock lasts until release is
// called or the process exits; release is safe to call more than once.
func LockDataDir(dataDir string) (release func(), err error) {
	if dataDir == "" {
		return nil, errors.New("store: LockDataDir requires a non-empty data dir")
	}
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, fmt.Errorf("store: create data dir %q: %w", dataDir, err)
	}
	path := filepath.Join(dataDir, LockFileName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("store: open lock %s: %w", path, err)
	}
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err != unix.EINTR {
			break
		}
	}
	if err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w (%s)", ErrDataDirLocked, path)
		}
		return nil, fmt.Errorf("store: lock %s: %w", path, err)
	}
	done := false
	return func() {
		if done {
			return
		}
		done = true
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
	}, nil
}
