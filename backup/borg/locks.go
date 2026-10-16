package borg

import "sync"

// Per-repository serialization for operations that must not run concurrently
// against the same borg repository within this agent process.
//
// The registry holds one mutex per repository name, created on first use. The
// outer mutex only guards the map itself; the per-repo mutex is what callers
// actually hold during their borg operation.
var (
	repoLocksMu sync.Mutex
	repoLocks   = map[string]*sync.Mutex{}
)

func repoLock(name string) *sync.Mutex {
	repoLocksMu.Lock()
	defer repoLocksMu.Unlock()
	m, ok := repoLocks[name]
	if !ok {
		m = &sync.Mutex{}
		repoLocks[name] = m
	}
	return m
}

// AcquireRepoLock blocks until the per-repository lock for name is held and
// returns a function that releases it. Typical use:
//
//	defer borg.AcquireRepoLock(vol.Name)()
//
// This serializes the operations that mutate (or, for export, read while
// bypassing borg's own lock) the same repository: export, prune, and compact.
// It is intentionally NOT taken by backup creation (`borg create`): create is
// append-only and safe to run alongside a bypass-lock export, and forcing it to
// wait would risk missing a scheduled backup.
//
// The lock is process-local. It is sufficient only because every repository is
// owned by exactly one node (each node's control.db holds only its own volumes —
// the controller writes each node's desired state to that node's endpoint). If a
// volume could ever be served by two nodes at once this provides no protection and
// a repository-level guard would be required.
func AcquireRepoLock(name string) func() {
	m := repoLock(name)
	m.Lock()
	return m.Unlock
}

// Per-repository serialization of the b-<name> docker volume's lifecycle: the window in
// which this agent inspects that volume, may remove and re-create it, and then starts a
// container that mounts it (InitBackupContainer), and the window in which it destroys it
// (TrashBackupVolumeExists).
//
// Why it exists: ensureCacheVolume re-creates an NFS repository volume whose mount options
// are out of date, and between its remove and its create the volume does not exist. Docker
// auto-creates a missing named volume when a container that mounts it is created — as a
// plain local volume with no driver options — so a second operation on the same repository
// that reached its container create inside that gap would run borg against an empty local
// directory instead of the repository on the NFS server. A backup and an export, for
// example, run in separate pools and nothing else orders them. Once a container is started
// the volume is in use and docker refuses to remove it, so the lock only has to span up to
// the start, not the container's lifetime.
//
// It is a SEPARATE lock from AcquireRepoLock, not a reuse of it. Export, prune and compact
// already hold AcquireRepoLock(name) when they reach InitBackupContainer, and sync.Mutex is
// not reentrant, so taking that lock again there would deadlock the operation against
// itself. Lock order: AcquireRepoLock (outer, where a caller takes it) → the volume
// lifecycle lock (inner). The lifecycle lock is never held while acquiring AcquireRepoLock,
// and it is held only across docker calls and the remote directory delete, never across a
// borg command, so it cannot be the reason a long operation waits.
//
// Process-local for the same reason AcquireRepoLock is: every repository is owned by
// exactly one node.
var (
	volumeLifecycleLocksMu sync.Mutex
	volumeLifecycleLocks   = map[string]*sync.Mutex{}
)

func volumeLifecycleMutex(name string) *sync.Mutex {
	volumeLifecycleLocksMu.Lock()
	defer volumeLifecycleLocksMu.Unlock()
	m, ok := volumeLifecycleLocks[name]
	if !ok {
		m = &sync.Mutex{}
		volumeLifecycleLocks[name] = m
	}
	return m
}

// acquireVolumeLifecycleLock blocks until the volume lifecycle lock for the repository
// name is held and returns a function that releases it. See the comment on
// volumeLifecycleLocks for what it guards and how it orders against AcquireRepoLock.
func acquireVolumeLifecycleLock(name string) func() {
	m := volumeLifecycleMutex(name)
	m.Lock()
	return m.Unlock
}
