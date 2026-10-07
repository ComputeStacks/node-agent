package job

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"cs-agent/store"
)

func taskStatus(t *testing.T, st *store.Store, id string) string {
	t.Helper()
	tk, found, err := st.GetTask(context.Background(), id)
	if err != nil || !found {
		t.Fatalf("get task %s: found=%v err=%v", id, found, err)
	}
	return tk.Status
}

// waitStatus polls until the task reaches want or the deadline passes.
func waitStatus(t *testing.T, st *store.Store, id, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if taskStatus(t, st, id) == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("task %s status = %q, want %q", id, taskStatus(t, st, id), want)
}

// TestDispatcher_PausedClaimsNothing proves a drain while the node is in
// maintenance claims nothing (backups and exports stay pending), and that a drain
// after the hold is cleared dispatches them.
func TestDispatcher_PausedClaimsNothing(t *testing.T) {
	d := newTestDispatcher(t)
	ctx := context.Background()
	for _, tk := range []store.Task{
		{ID: "b1", Name: "volume.backup", Node: "test-node"},
		{ID: "e1", Name: "backup.export", Node: "test-node"},
	} {
		if _, err := d.st.CreateTask(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := d.st.PutLocalHold(ctx, "test", "tester"); err != nil {
		t.Fatal(err)
	}

	d.drain(ctx)
	for _, id := range []string{"b1", "e1"} {
		if s := taskStatus(t, d.st, id); s != store.TaskPending {
			t.Fatalf("%s status while paused = %q, want pending", id, s)
		}
	}
	select {
	case task := <-d.backupQ:
		t.Fatalf("task dispatched while paused: %q", task.ID)
	default:
	}
	if len(d.backupSlots) != d.backupWorkers {
		t.Fatalf("free backup slots = %d, want %d (none held while paused)", len(d.backupSlots), d.backupWorkers)
	}

	if _, err := d.st.ClearLocalHold(ctx); err != nil {
		t.Fatal(err)
	}
	d.drain(ctx)
	select {
	case task := <-d.backupQ:
		if task.ID != "b1" {
			t.Fatalf("dispatched %q, want b1", task.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("backup not dispatched after un-pause")
	}
	if s := taskStatus(t, d.st, "b1"); s != store.TaskRunning {
		t.Fatalf("b1 status after un-pause = %q, want running", s)
	}
}

// TestDispatcher_NoClaimUntilWorkerFree proves slot-then-claim: with every backup
// worker busy, a second pending task is NOT moved to running; it is claimed only
// once a worker frees up.
func TestDispatcher_NoClaimUntilWorkerFree(t *testing.T) {
	d := newTestDispatcher(t)
	d.backupWorkers = 1
	d.backupSlots = make(chan struct{}, 1)
	d.backupSlots <- struct{}{}
	d.backupQ = make(chan store.Task, 1)

	started := make(chan string, 2)
	release := make(chan struct{})
	d.runner = func(_ context.Context, _ *store.Store, task store.Task) (json.RawMessage, error) {
		started <- task.ID
		<-release
		return nil, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	for _, id := range []string{"b1", "b2"} {
		if _, err := d.st.CreateTask(ctx, store.Task{ID: id, Name: "volume.backup", Node: "test-node"}); err != nil {
			t.Fatal(err)
		}
	}
	d.Start(ctx, &wg)

	var first string
	select {
	case first = <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("no task started")
	}
	second := "b2"
	if first == "b2" {
		second = "b1"
	}
	// The only worker is busy: the other task must stay pending, not running.
	time.Sleep(300 * time.Millisecond)
	if s := taskStatus(t, d.st, second); s != store.TaskPending {
		t.Fatalf("%s status with all workers busy = %q, want pending", second, s)
	}

	close(release)
	select {
	case id := <-started:
		if id != second {
			t.Fatalf("started %q, want %q", id, second)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second task not started after the worker freed")
	}
	waitStatus(t, d.st, first, store.TaskCompleted)
	waitStatus(t, d.st, second, store.TaskCompleted)
}

// TestDispatcher_PauseWhileWaitingForSlot proves the dispatcher, blocked waiting
// for a busy backup pool, gives up when the node enters maintenance rather than
// claiming the task once a worker frees.
func TestDispatcher_PauseWhileWaitingForSlot(t *testing.T) {
	old := slotRecheckInterval
	slotRecheckInterval = 50 * time.Millisecond
	t.Cleanup(func() { slotRecheckInterval = old })

	d := newTestDispatcher(t)
	ctx := context.Background()
	if _, err := d.st.CreateTask(ctx, store.Task{ID: "b1", Name: "volume.backup", Node: "test-node"}); err != nil {
		t.Fatal(err)
	}
	// Occupy every slot, as if all workers were busy.
	for i := 0; i < d.backupWorkers; i++ {
		<-d.backupSlots
	}

	done := make(chan struct{})
	go func() {
		d.drain(ctx)
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	if _, _, err := d.st.PutLocalHold(ctx, "test", "tester"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("drain did not return after the node entered maintenance")
	}
	if s := taskStatus(t, d.st, "b1"); s != store.TaskPending {
		t.Fatalf("b1 status = %q, want pending", s)
	}
}
