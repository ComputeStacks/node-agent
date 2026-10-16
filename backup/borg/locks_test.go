package borg

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Same key must serialize: at most one holder at a time. Run with -race.
func TestRepoLockSameKeySerializes(t *testing.T) {
	const name = "vol-a"
	var concurrent, violated int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer AcquireRepoLock(name)()
			if atomic.AddInt32(&concurrent, 1) > 1 {
				atomic.StoreInt32(&violated, 1)
			}
			time.Sleep(2 * time.Millisecond)
			atomic.AddInt32(&concurrent, -1)
		}()
	}
	wg.Wait()
	if violated != 0 {
		t.Error("same-key lock allowed more than one concurrent holder")
	}
}

// A different key must not block while another key is held.
func TestRepoLockDistinctKeysParallel(t *testing.T) {
	release := AcquireRepoLock("alpha")
	defer release()

	done := make(chan struct{})
	go func() {
		AcquireRepoLock("beta")()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("distinct-key lock blocked while a different key was held")
	}
}

func TestRepoLockRegistry(t *testing.T) {
	if repoLock("same") != repoLock("same") {
		t.Error("repoLock should return the same mutex for the same name")
	}
	if repoLock("one") == repoLock("two") {
		t.Error("repoLock should return distinct mutexes for distinct names")
	}
}

// The volume lifecycle lock must serialize holders of the same repository name.
func TestVolumeLifecycleLockSameKeySerializes(t *testing.T) {
	const name = "vol-lifecycle-a"
	var concurrent, violated int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer acquireVolumeLifecycleLock(name)()
			if atomic.AddInt32(&concurrent, 1) > 1 {
				atomic.StoreInt32(&violated, 1)
			}
			time.Sleep(2 * time.Millisecond)
			atomic.AddInt32(&concurrent, -1)
		}()
	}
	wg.Wait()
	if violated != 0 {
		t.Error("same-key volume lifecycle lock allowed more than one concurrent holder")
	}
}

// A different repository's lifecycle lock must not block while another is held.
func TestVolumeLifecycleLockDistinctKeysParallel(t *testing.T) {
	release := acquireVolumeLifecycleLock("lifecycle-alpha")
	defer release()

	done := make(chan struct{})
	go func() {
		acquireVolumeLifecycleLock("lifecycle-beta")()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("distinct-key volume lifecycle lock blocked while a different key was held")
	}
}

// Export, prune and compact hold AcquireRepoLock(name) when they reach InitBackupContainer,
// which then takes the volume lifecycle lock for the same name. If the two were the same
// lock (or shared a registry entry) that would deadlock the operation against itself.
func TestVolumeLifecycleLockDoesNotBlockUnderRepoLock(t *testing.T) {
	const name = "vol-lifecycle-nested"
	releaseRepo := AcquireRepoLock(name)
	defer releaseRepo()

	done := make(chan struct{})
	go func() {
		acquireVolumeLifecycleLock(name)()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("volume lifecycle lock blocked while AcquireRepoLock held the same name")
	}

	if volumeLifecycleMutex(name) == repoLock(name) {
		t.Error("volume lifecycle lock and repository lock share a mutex")
	}
}
