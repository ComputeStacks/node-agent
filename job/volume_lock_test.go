package job

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"cs-agent/store"
)

// lockHarness runs a real dispatcher (loop + worker pools) whose runner blocks
// each task until the test releases it, so a task can be held "running".
type lockHarness struct {
	t       *testing.T
	d       *Dispatcher
	ctx     context.Context
	started chan string
	start   func()

	mu    sync.Mutex
	gates map[string]chan struct{}
	calls map[string]int
	panic map[string]bool
}

// newLockHarness builds a dispatcher with the given number of backup workers.
// Every lock test uses more workers than it holds running, so a task that waits
// is waiting on its volume, never on a free slot.
func newLockHarness(t *testing.T, backupWorkers int) *lockHarness {
	t.Helper()
	d := newTestDispatcher(t)
	d.backupWorkers = backupWorkers
	d.backupSlots = make(chan struct{}, backupWorkers)
	for i := 0; i < backupWorkers; i++ {
		d.backupSlots <- struct{}{}
	}
	d.backupQ = make(chan store.Task, backupWorkers)

	ctx, cancel := context.WithCancel(context.Background())
	h := &lockHarness{
		t:       t,
		d:       d,
		ctx:     ctx,
		started: make(chan string, 32),
		gates:   map[string]chan struct{}{},
		calls:   map[string]int{},
		panic:   map[string]bool{},
	}
	d.runner = func(ctx context.Context, _ *store.Store, task store.Task) (json.RawMessage, error) {
		h.mu.Lock()
		h.calls[task.ID]++
		boom := h.panic[task.ID]
		h.mu.Unlock()
		if boom {
			panic("boom")
		}
		h.started <- task.ID
		select {
		case <-h.gate(task.ID):
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	var wg sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	h.start = func() { d.Start(ctx, &wg) }
	return h
}

func (h *lockHarness) gate(id string) chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	g, ok := h.gates[id]
	if !ok {
		g = make(chan struct{})
		h.gates[id] = g
	}
	return g
}

func (h *lockHarness) release(id string) { close(h.gate(id)) }

func (h *lockHarness) callCount(id string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls[id]
}

func (h *lockHarness) create(id, kind, volume string) {
	h.t.Helper()
	if _, err := h.d.st.CreateTask(context.Background(), store.Task{ID: id, Name: kind, Node: "test-node", Volume: volume}); err != nil {
		h.t.Fatal(err)
	}
}

// add creates a task and wakes the dispatcher, as the task-create handler does.
func (h *lockHarness) add(id, kind, volume string) {
	h.t.Helper()
	h.create(id, kind, volume)
	h.d.Signal()
}

// expectStarted waits for the runner to report the given task ids (any order).
func (h *lockHarness) expectStarted(ids ...string) {
	h.t.Helper()
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	deadline := time.After(3 * time.Second)
	for len(want) > 0 {
		select {
		case id := <-h.started:
			if !want[id] {
				h.t.Fatalf("task %q started, want one of %v", id, want)
			}
			delete(want, id)
		case <-deadline:
			h.t.Fatalf("tasks %v did not start", want)
		}
	}
}

// expectWaiting asserts the task is still pending (not claimed, not failed)
// after giving the dispatcher time to drain, and that no other task started.
func (h *lockHarness) expectWaiting(id string) {
	h.t.Helper()
	select {
	case got := <-h.started:
		h.t.Fatalf("task %q started while %s should be waiting", got, id)
	case <-time.After(300 * time.Millisecond):
	}
	if s := taskStatus(h.t, h.d.st, id); s != store.TaskPending {
		h.t.Fatalf("%s status = %q, want pending (waiting on its volume)", id, s)
	}
}

// holdRunning starts the harness with one task held running on its volume.
func (h *lockHarness) holdRunning(id, kind, volume string) {
	h.t.Helper()
	h.create(id, kind, volume)
	h.start()
	h.expectStarted(id)
}

// testWaitsBehind holds `first` running on volume V, adds `second` on V, and
// proves it stays pending (consuming no worker slot) until first finishes, then
// runs. The wake comes from the worker's Signal: nothing else drains inside the
// 3s wait, well under the 30s backstop.
func testWaitsBehind(t *testing.T, firstKind, secondKind string) {
	const workers = 3
	h := newLockHarness(t, workers)
	h.holdRunning("t1", firstKind, "vol-a")

	h.add("t2", secondKind, "vol-a")
	h.expectWaiting("t2")
	if free := len(h.d.backupSlots); free != workers-1 {
		t.Fatalf("free backup slots = %d, want %d (a waiting task holds none)", free, workers-1)
	}

	h.release("t1")
	h.expectStarted("t2")
	waitStatus(t, h.d.st, "t1", store.TaskCompleted)
	h.release("t2")
	waitStatus(t, h.d.st, "t2", store.TaskCompleted)
}

// 1: two pending restores of one volume, nothing running -> the first runs, the
// second is failed with the duplicate-restore error and never reaches the runner.
func TestVolumeLock_DuplicateRestoreFails(t *testing.T) {
	h := newLockHarness(t, 3)
	h.create("r1", "volume.restore", "vol-a")
	h.create("r2", "volume.restore", "vol-a")
	h.start()

	h.expectStarted("r1")
	waitStatus(t, h.d.st, "r2", store.TaskFailed)
	tk, _, err := h.d.st.GetTask(context.Background(), "r2")
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]string
	if err := json.Unmarshal(tk.Result, &result); err != nil {
		t.Fatalf("decode r2 result %q: %v", tk.Result, err)
	}
	if result["error"] != "another restore is already in progress on this volume" {
		t.Fatalf("r2 result = %q", tk.Result)
	}

	h.release("r1")
	waitStatus(t, h.d.st, "r1", store.TaskCompleted)
	if n := h.callCount("r2"); n != 0 {
		t.Fatalf("r2 runner calls = %d, want 0", n)
	}
	if n := h.callCount("r1"); n != 1 {
		t.Fatalf("r1 runner calls = %d, want 1", n)
	}
	select {
	case id := <-h.started:
		t.Fatalf("unexpected extra task started: %q", id)
	default:
	}
}

// 2: restore running on V; a backup on V waits, then runs promptly after.
func TestVolumeLock_BackupWaitsBehindRestore(t *testing.T) {
	testWaitsBehind(t, "volume.restore", "volume.backup")
}

// 3: backup running on V; a restore on V waits (is NOT failed), then runs.
func TestVolumeLock_RestoreWaitsBehindBackup(t *testing.T) {
	testWaitsBehind(t, "volume.backup", "volume.restore")
}

// 4: backup running on V; a second backup on V waits, then runs.
func TestVolumeLock_BackupWaitsBehindBackup(t *testing.T) {
	testWaitsBehind(t, "volume.backup", "volume.backup")
}

// 5: trash running on V; a backup on V waits, then runs.
func TestVolumeLock_BackupWaitsBehindTrash(t *testing.T) {
	testWaitsBehind(t, "volume.trash", "volume.backup")
}

// 6: tasks on different volumes run at the same time.
func TestVolumeLock_DifferentVolumesConcurrent(t *testing.T) {
	h := newLockHarness(t, 3)
	h.holdRunning("r1", "volume.restore", "vol-a")
	h.add("b1", "volume.backup", "vol-b")
	h.expectStarted("b1") // r1 is still held in the runner
	h.release("r1")
	h.release("b1")
	waitStatus(t, h.d.st, "r1", store.TaskCompleted)
	waitStatus(t, h.d.st, "b1", store.TaskCompleted)
}

// 7: an export of V is not held back by a restore running on V.
func TestVolumeLock_ExportUnaffected(t *testing.T) {
	h := newLockHarness(t, 3)
	h.holdRunning("r1", "volume.restore", "vol-a")
	h.add("e1", "backup.export", "vol-a")
	h.expectStarted("e1")
	h.release("e1")
	waitStatus(t, h.d.st, "e1", store.TaskCompleted)
	if s := taskStatus(t, h.d.st, "r1"); s != store.TaskRunning {
		t.Fatalf("r1 status = %q, want running", s)
	}
	h.release("r1")
	waitStatus(t, h.d.st, "r1", store.TaskCompleted)
}

// 8: a panicking task still releases its volume, so the next task on V runs.
func TestVolumeLock_ReleasedAfterPanic(t *testing.T) {
	h := newLockHarness(t, 3)
	h.panic["p1"] = true
	h.create("p1", "volume.restore", "vol-a")
	h.create("p2", "volume.backup", "vol-a")
	h.start()

	waitStatus(t, h.d.st, "p1", store.TaskFailed)
	h.expectStarted("p2")
	h.release("p2")
	waitStatus(t, h.d.st, "p2", store.TaskCompleted)
}

// 9: participating tasks with no volume take no lock and run concurrently.
func TestVolumeLock_EmptyVolumeNotLocked(t *testing.T) {
	h := newLockHarness(t, 3)
	h.create("r1", "volume.restore", "")
	h.create("r2", "volume.restore", "")
	h.start()
	h.expectStarted("r1", "r2")
	h.release("r1")
	h.release("r2")
	waitStatus(t, h.d.st, "r1", store.TaskCompleted)
	waitStatus(t, h.d.st, "r2", store.TaskCompleted)
}
