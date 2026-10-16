package backup

import (
	"context"
	"encoding/json"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"cs-agent/store"
	"cs-agent/types"

	"github.com/spf13/viper"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func putVol(t *testing.T, st *store.Store, v types.Volume) {
	t.Helper()
	if err := st.PutVolume(context.Background(), store.Volume{
		Name:      v.Name,
		Node:      v.Node,
		ProjectID: strconv.Itoa(v.ProjectID),
		Config:    json.RawMessage(v.JSONEncode()),
	}); err != nil {
		t.Fatalf("put volume %q: %v", v.Name, err)
	}
}

func newTestScheduler(t *testing.T, st *store.Store) *Scheduler {
	t.Helper()
	s := NewScheduler(st, nil)
	s.hostname = "test-node"
	return s
}

func TestScheduler_Reconcile(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	s := newTestScheduler(t, st)

	// Unpopulated control.db: reconcile must be a no-op (never wipe schedules).
	// (No PutVolume yet, so the volumes-populated sentinel is unset.)
	s.reconcile(ctx)
	if all, _ := st.ListSchedules(ctx); len(all) != 0 {
		t.Fatalf("schedules created while unpopulated: %d", len(all))
	}

	// Backup-enabled volume -> a schedule with a FUTURE next_fire_at (no storm).
	putVol(t, st, types.Volume{Name: "v1", Node: "test-node", Backup: true, Freq: "0 2 * * *", ProjectID: 7})
	s.reconcile(ctx)
	sc, found, _ := st.GetSchedule(ctx, "v1")
	if !found || sc.CronExpr != "0 2 * * *" {
		t.Fatalf("schedule for v1: found=%v %+v", found, sc)
	}
	if sc.NextFireAt <= time.Now().Unix() {
		t.Fatalf("fresh next_fire_at = %d is not in the future (would storm on backfill)", sc.NextFireAt)
	}

	// Backup-disabled volume -> no schedule.
	putVol(t, st, types.Volume{Name: "v2", Node: "test-node", Backup: false, ProjectID: 7})
	s.reconcile(ctx)
	if _, found, _ := st.GetSchedule(ctx, "v2"); found {
		t.Fatal("schedule created for a backup-disabled volume")
	}

	// Trash the volume -> a stable-id volume.trash task + schedule removed.
	putVol(t, st, types.Volume{Name: "v1", Node: "test-node", Backup: true, Freq: "0 2 * * *", Trash: true, ProjectID: 7})
	s.reconcile(ctx)
	if _, found, _ := st.GetSchedule(ctx, "v1"); found {
		t.Fatal("schedule not removed for a trashed volume")
	}
	tk, found, _ := st.GetTask(ctx, "volume.trash:v1")
	if !found || tk.Name != "volume.trash" || tk.Volume != "v1" {
		t.Fatalf("trash task: found=%v %+v", found, tk)
	}
	// Idempotent: a second reconcile does not enqueue a duplicate trash task.
	s.reconcile(ctx)
	if got := countPendingTrash(t, st); got != 1 {
		t.Fatalf("pending trash tasks = %d, want 1 (idempotent)", got)
	}
}

func countPendingTrash(t *testing.T, st *store.Store) int {
	t.Helper()
	pending, err := st.ListPendingTasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, p := range pending {
		if p.Name == "volume.trash" {
			n++
		}
	}
	return n
}

func TestScheduler_FireDue(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	s := newTestScheduler(t, st)
	putVol(t, st, types.Volume{Name: "v1", Node: "test-node", Backup: true, Freq: "0 2 * * *", ProjectID: 7})
	// A schedule already due (next_fire_at in the past).
	if err := st.PutSchedule(ctx, "v1", "0 2 * * *", 1); err != nil {
		t.Fatal(err)
	}
	s.fireDue(ctx)

	pending, _ := st.ListPendingTasks(ctx)
	if len(pending) != 1 || pending[0].Name != "volume.backup" || pending[0].Volume != "v1" {
		t.Fatalf("fireDue pending tasks: %+v", pending)
	}
	// next_fire_at advanced into the future -> no longer due (exactly-once).
	sc, _, _ := st.GetSchedule(ctx, "v1")
	if sc.NextFireAt <= time.Now().Unix() {
		t.Fatalf("next_fire_at = %d not advanced past now", sc.NextFireAt)
	}
}

// setSpreadCap sets backups.schedule_spread_max_sec for one test (the default
// lives in config.ConfigureApp; package tests otherwise see 0 = spreading off).
func setSpreadCap(t *testing.T, sec int) {
	t.Helper()
	prev := viper.Get("backups.schedule_spread_max_sec")
	viper.Set("backups.schedule_spread_max_sec", sec)
	t.Cleanup(func() { viper.Set("backups.schedule_spread_max_sec", prev) })
}

// TestScheduler_ReconcileSpread proves a new schedule lands on its project's
// shifted grid, and that volumes of one project share the slot.
func TestScheduler_ReconcileSpread(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	s := newTestScheduler(t, st)
	setSpreadCap(t, 7200)
	const freq = "0 * * * *"
	off := store.ScheduleOffset(freq, store.OffsetKey(7, ""), 2*time.Hour)
	if off == 0 {
		t.Fatal("test needs a non-zero offset for project 7")
	}

	putVol(t, st, types.Volume{Name: "files", Node: "test-node", Backup: true, Freq: freq, ProjectID: 7})
	putVol(t, st, types.Volume{Name: "db", Node: "test-node", Backup: true, Freq: freq, ProjectID: 7})
	before := time.Now()
	s.reconcile(ctx)
	files, _, _ := st.GetSchedule(ctx, "files")
	db, _, _ := st.GetSchedule(ctx, "db")
	if files.NextFireAt != db.NextFireAt {
		t.Fatalf("volumes of one project: next_fire_at %d != %d", files.NextFireAt, db.NextFireAt)
	}
	if want := store.NextFireOffset(freq, off, before).Unix(); files.NextFireAt != want {
		t.Fatalf("next_fire_at = %d, want %d (offset %v)", files.NextFireAt, want, off)
	}
	if got := store.SchedulePhase(freq, files.NextFireAt); got != int64(off/time.Second) {
		t.Fatalf("phase = %ds, want %v", got, off)
	}
}

// TestScheduler_ReconcileRealigns proves an existing unshifted row (as written
// before spreading) is moved onto its offset once — same slot, so nothing is
// skipped or repeated — and left alone after that; that a smaller cap moves it
// back earlier within the slot; and that cap 0 restores the bare cron instant.
func TestScheduler_ReconcileRealigns(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	s := newTestScheduler(t, st)
	setSpreadCap(t, 7200)
	const freq = "0 2 * * *"
	off := store.ScheduleOffset(freq, store.OffsetKey(7, ""), 2*time.Hour)
	if off == 0 {
		t.Fatal("test needs a non-zero offset for project 7")
	}
	putVol(t, st, types.Volume{Name: "v1", Node: "test-node", Backup: true, Freq: freq, ProjectID: 7})
	base := store.NextFire(freq, time.Now()).Unix()
	if err := st.PutSchedule(ctx, "v1", freq, base); err != nil {
		t.Fatal(err)
	}

	s.reconcile(ctx)
	sc, _, _ := st.GetSchedule(ctx, "v1")
	if want := base + int64(off/time.Second); sc.NextFireAt != want {
		t.Fatalf("realigned next_fire_at = %d, want %d", sc.NextFireAt, want)
	}
	// Idempotent: a second reconcile leaves the aligned row alone.
	s.reconcile(ctx)
	if again, _, _ := st.GetSchedule(ctx, "v1"); again.NextFireAt != sc.NextFireAt {
		t.Fatalf("second reconcile moved next_fire_at %d -> %d", sc.NextFireAt, again.NextFireAt)
	}

	// A smaller cap moves the row EARLIER within the same slot.
	setSpreadCap(t, 60)
	small := store.ScheduleOffset(freq, store.OffsetKey(7, ""), time.Minute)
	s.reconcile(ctx)
	if sc, _, _ := st.GetSchedule(ctx, "v1"); sc.NextFireAt != base+int64(small/time.Second) {
		t.Fatalf("after cap decrease next_fire_at = %d, want %d", sc.NextFireAt, base+int64(small/time.Second))
	}

	// Cap 0 restores the bare instant of the same slot (not the next day's).
	setSpreadCap(t, 0)
	s.reconcile(ctx)
	if sc, _, _ := st.GetSchedule(ctx, "v1"); sc.NextFireAt != base {
		t.Fatalf("after cap 0 next_fire_at = %d, want %d", sc.NextFireAt, base)
	}
}

// TestScheduler_SpreadOff proves cap 0 keeps today's behavior: a new schedule
// gets the exact cron instant and an existing unshifted row is not moved.
func TestScheduler_SpreadOff(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	s := newTestScheduler(t, st)
	setSpreadCap(t, 0)
	const freq = "0 2 * * *"
	putVol(t, st, types.Volume{Name: "new", Node: "test-node", Backup: true, Freq: freq, ProjectID: 7})
	putVol(t, st, types.Volume{Name: "old", Node: "test-node", Backup: true, Freq: freq, ProjectID: 7})
	base := store.NextFire(freq, time.Now()).Unix()
	if err := st.PutSchedule(ctx, "old", freq, base); err != nil {
		t.Fatal(err)
	}
	s.reconcile(ctx)
	for _, name := range []string{"new", "old"} {
		if sc, _, _ := st.GetSchedule(ctx, name); sc.NextFireAt != base {
			t.Fatalf("%s next_fire_at = %d, want exact instant %d", name, sc.NextFireAt, base)
		}
	}
}

// TestScheduler_FireDueSpread proves firing advances to the next SHIFTED slot.
func TestScheduler_FireDueSpread(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	s := newTestScheduler(t, st)
	setSpreadCap(t, 7200)
	const freq = "0 * * * *"
	off := store.ScheduleOffset(freq, store.OffsetKey(7, ""), 2*time.Hour)
	putVol(t, st, types.Volume{Name: "v1", Node: "test-node", Backup: true, Freq: freq, ProjectID: 7})
	if err := st.PutSchedule(ctx, "v1", freq, 1); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	s.fireDue(ctx)
	if pending, _ := st.ListPendingTasks(ctx); len(pending) != 1 {
		t.Fatalf("fireDue pending tasks: %+v", pending)
	}
	sc, _, _ := st.GetSchedule(ctx, "v1")
	if want := store.NextFireOffset(freq, off, before).Unix(); sc.NextFireAt != want {
		t.Fatalf("next_fire_at = %d, want %d (offset %v)", sc.NextFireAt, want, off)
	}
}

// TestScheduler_MaintenanceRunsOffLoop proves the M1 fix: a due maintenance job
// runs in its own goroutine (so a long prune/compact + jitter can't block backup
// firing), and an overlap guard skips a second run while the first is in flight.
func TestScheduler_MaintenanceRunsOffLoop(t *testing.T) {
	st := testStore(t)
	s := newTestScheduler(t, st)

	release := make(chan struct{})
	started := make(chan struct{}, 2)
	var runs atomic.Int32
	job := &maintJob{
		name: "blocker",
		expr: "* * * * *",
		next: time.Now().Add(-time.Minute), // due now
		run: func(ctx context.Context) {
			runs.Add(1)
			started <- struct{}{}
			<-release // hold the job "in flight"
		},
	}
	s.maint = []*maintJob{job}

	// runMaintenance must return promptly even though the job blocks.
	done := make(chan struct{})
	go func() { s.runMaintenance(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runMaintenance blocked on a long maintenance job (not run off-loop)")
	}
	<-started // the job goroutine is running

	// Overlap guard: make it due again; a second pass must not start a 2nd run.
	job.next = time.Now().Add(-time.Minute)
	s.runMaintenance(context.Background())
	select {
	case <-started:
		t.Fatal("overlapping maintenance run started while the previous was in flight")
	case <-time.After(200 * time.Millisecond):
	}

	close(release) // let the first run finish
	s.maintWg.Wait()
	if runs.Load() != 1 {
		t.Fatalf("maintenance ran %d times, want 1 (overlap guarded)", runs.Load())
	}
}

// TestScheduler_ReconcileDoesNotRetryFailedTrash proves the M2/defect-#1 fix: a
// lingering trash:true volume whose teardown task already FAILED is not re-run by
// reconcile every tick (resetFailed=false).
func TestScheduler_ReconcileDoesNotRetryFailedTrash(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	s := newTestScheduler(t, st)

	putVol(t, st, types.Volume{Name: "v1", Node: "test-node", Backup: true, Freq: "0 2 * * *", Trash: true, ProjectID: 7})
	s.reconcile(ctx) // enqueues volume.trash:v1 (pending)
	if _, found, _ := st.GetTask(ctx, "volume.trash:v1"); !found {
		t.Fatal("trash task not enqueued")
	}
	// Simulate the teardown failing.
	if err := st.UpdateTaskStatus(ctx, "volume.trash:v1", store.TaskFailed, nil); err != nil {
		t.Fatal(err)
	}
	// The trash:true row still lingers (controller hasn't DELETEd it) → reconcile
	// must NOT reset the failed task back to pending.
	s.reconcile(ctx)
	tk, _, _ := st.GetTask(ctx, "volume.trash:v1")
	if tk.Status != store.TaskFailed {
		t.Fatalf("status = %q, want failed (reconcile must not retry a failed teardown)", tk.Status)
	}
}

func TestRunTask_UnknownKind(t *testing.T) {
	st := testStore(t)
	result, err := RunTask(context.Background(), st, store.Task{ID: "x", Name: "bogus.kind", Node: "n"})
	if err == nil {
		t.Fatal("unknown kind did not error")
	}
	var m map[string]any
	if uErr := json.Unmarshal(result, &m); uErr != nil {
		t.Fatalf("result not JSON: %v", uErr)
	}
	if m["error"] == nil {
		t.Fatalf("result missing error field: %s", result)
	}
}
