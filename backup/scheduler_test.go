package backup

import (
	"context"
	"encoding/json"
	"fmt"
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

// TestScheduler_FireDueUnalignedRowDoesNotRefire covers a row that fires while
// still on an older (smaller) offset: the next slot must come after the slot
// that fired, not be that same slot again at the volume's current offset.
func TestScheduler_FireDueUnalignedRowDoesNotRefire(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	s := newTestScheduler(t, st)
	setSpreadCap(t, 7200)

	// A daily cron whose instant was one minute ago, so this slot is due now
	// and its shifted instant (base + offset) is still ahead.
	base := time.Now().Truncate(time.Minute).Add(-time.Minute)
	freq := fmt.Sprintf("%d %d * * *", base.Minute(), base.Hour())
	project, off := 0, time.Duration(0)
	for id := 1; id < 1000; id++ {
		if o := store.ScheduleOffset(freq, store.OffsetKey(id, ""), 2*time.Hour); o > 5*time.Minute {
			project, off = id, o
			break
		}
	}
	if project == 0 {
		t.Fatal("no project id with an offset above 5m")
	}
	putVol(t, st, types.Volume{Name: "v1", Node: "test-node", Backup: true, Freq: freq, ProjectID: project})
	if err := st.PutSchedule(ctx, "v1", freq, base.Unix()); err != nil { // unshifted, due
		t.Fatal(err)
	}
	s.fireDue(ctx)
	if pending, _ := st.ListPendingTasks(ctx); len(pending) != 1 {
		t.Fatalf("fireDue pending tasks: %+v", pending)
	}
	sc, _, _ := st.GetSchedule(ctx, "v1")
	if want := base.AddDate(0, 0, 1).Add(off).Unix(); sc.NextFireAt != want {
		t.Fatalf("next_fire_at = %s, want tomorrow's slot %s (same-day %s would fire twice)",
			time.Unix(sc.NextFireAt, 0), time.Unix(want, 0), base.Add(off))
	}
}

// --- Catch-up backups after maintenance ---------------------------------------

// setCatchup sets the maintenance catch-up config for one test (package tests
// see no config defaults).
func setCatchup(t *testing.T, enabled bool, skipSec int) {
	t.Helper()
	prevOn, prevSkip := viper.Get("backups.maintenance_catchup"), viper.Get("backups.maintenance_catchup_skip_sec")
	viper.Set("backups.maintenance_catchup", enabled)
	viper.Set("backups.maintenance_catchup_skip_sec", skipSec)
	t.Cleanup(func() {
		viper.Set("backups.maintenance_catchup", prevOn)
		viper.Set("backups.maintenance_catchup_skip_sec", prevSkip)
	})
}

// projectWithOffset finds a project id whose offset for freq (2h cap) is above min.
func projectWithOffset(t *testing.T, freq string, min time.Duration) (int, time.Duration) {
	t.Helper()
	for id := 1; id < 1000; id++ {
		if o := store.ScheduleOffset(freq, store.OffsetKey(id, ""), 2*time.Hour); o > min {
			return id, o
		}
	}
	t.Fatalf("no project id with an offset above %v for %q", min, freq)
	return 0, 0
}

func pause(t *testing.T, st *store.Store) {
	t.Helper()
	if _, _, err := st.PutLocalHold(context.Background(), "test", "root"); err != nil {
		t.Fatal(err)
	}
}

func unpause(t *testing.T, st *store.Store) {
	t.Helper()
	if _, err := st.ClearLocalHold(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type testMarker struct {
	Missed bool  `json:"missed"`
	Exit   int64 `json:"exit"`
	Phase  int64 `json:"phase"`
	Next   int64 `json:"next"`
}

// marker returns vol's stored catch-up marker.
func marker(t *testing.T, st *store.Store, vol string) (testMarker, bool) {
	t.Helper()
	v, found, err := st.GetMeta(context.Background(), "catchup."+vol)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		return testMarker{}, false
	}
	var m testMarker
	if err := json.Unmarshal([]byte(v), &m); err != nil {
		t.Fatalf("marker %q: %v", v, err)
	}
	return m, true
}

// backdateExit moves vol's recorded exit sec seconds into the past, standing in
// for the time that passes after maintenance ends.
func backdateExit(t *testing.T, st *store.Store, vol string, sec int64) {
	t.Helper()
	m, ok := marker(t, st, vol)
	if !ok || m.Exit == 0 {
		t.Fatalf("%s: no exit marker", vol)
	}
	m.Exit -= sec
	b, _ := json.Marshal(map[string]int64{"exit": m.Exit, "phase": m.Phase, "next": m.Next})
	if err := st.SetMeta(context.Background(), "catchup."+vol, string(b)); err != nil {
		t.Fatal(err)
	}
}

func pendingBackups(t *testing.T, st *store.Store, vol string) int {
	t.Helper()
	pending, err := st.ListPendingTasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, p := range pending {
		if p.Name == "volume.backup" && p.Volume == vol {
			n++
		}
	}
	return n
}

// dailyMissed sets up a daily volume whose shifted slot came due 3h ago minus its
// offset, pauses, lets the paused tick skip it, and exits maintenance. It returns
// the offset.
func dailyMissed(t *testing.T, st *store.Store, s *Scheduler, vol string, project int) time.Duration {
	t.Helper()
	base := time.Now().Truncate(time.Minute).Add(-3 * time.Hour)
	freq := fmt.Sprintf("%d %d * * *", base.Minute(), base.Hour())
	off := store.ScheduleOffset(freq, store.OffsetKey(project, ""), 2*time.Hour)
	putVol(t, st, types.Volume{Name: vol, Node: "test-node", Backup: true, Freq: freq, ProjectID: project})
	if err := st.PutSchedule(context.Background(), vol, freq, base.Add(off).Unix()); err != nil {
		t.Fatal(err)
	}
	pause(t, st)
	s.fireDue(context.Background())
	if n := pendingBackups(t, st, vol); n != 0 {
		t.Fatalf("%s: backup fired while paused", vol)
	}
	unpause(t, st)
	return off
}

// A daily slot missed during maintenance gets one catch-up at exit + offset;
// the schedule row is not moved, so its next regular slot still fires once.
func TestScheduler_CatchupDailyMissed(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	s := newTestScheduler(t, st)
	setSpreadCap(t, 7200)
	setCatchup(t, true, 7200)
	project, _ := projectWithOffset(t, "0 3 * * *", 5*time.Minute)

	off := dailyMissed(t, st, s, "v1", project)
	m, ok := marker(t, st, "v1")
	if !ok || m.Exit == 0 || m.Phase != int64(off/time.Second) {
		t.Fatalf("marker = %+v (ok=%v), want phase %v", m, ok, off)
	}
	sc, _, _ := st.GetSchedule(ctx, "v1")
	if m.Next != sc.NextFireAt {
		t.Fatalf("marker next %d != schedule next %d", m.Next, sc.NextFireAt)
	}

	// Not yet: the catch-up waits for exit + offset.
	s.processCatchups(ctx)
	if n := pendingBackups(t, st, "v1"); n != 0 {
		t.Fatalf("catch-up fired at the exit, before its offset (%d tasks)", n)
	}
	backdateExit(t, st, "v1", int64(off/time.Second))
	s.processCatchups(ctx)
	if n := pendingBackups(t, st, "v1"); n != 1 {
		t.Fatalf("catch-up tasks = %d, want 1", n)
	}
	if _, ok := marker(t, st, "v1"); ok {
		t.Fatal("marker left after the catch-up fired")
	}
	s.processCatchups(ctx)
	if n := pendingBackups(t, st, "v1"); n != 1 {
		t.Fatalf("catch-up fired twice (%d tasks)", n)
	}
	if after, _, _ := st.GetSchedule(ctx, "v1"); after.NextFireAt != sc.NextFireAt {
		t.Fatalf("schedule moved by the catch-up: %d -> %d", sc.NextFireAt, after.NextFireAt)
	}
	// Exactly the next regular slot after the exit, which then fires normally.
	if want := sc.NextFireAt; store.SchedulePhase(sc.CronExpr, want) != int64(off/time.Second) || want <= time.Now().Unix() {
		t.Fatalf("next regular slot %d is off its grid or in the past", want)
	}
}

// A slot due just before the pause, skipped by the paused tick, is caught up
// (spreading off: the catch-up is due at the exit itself).
func TestScheduler_CatchupSlotDueBeforePause(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	s := newTestScheduler(t, st)
	setSpreadCap(t, 0)
	setCatchup(t, true, 7200)
	base := time.Now().Truncate(time.Minute).Add(-time.Minute)
	freq := fmt.Sprintf("%d %d * * *", base.Minute(), base.Hour())
	putVol(t, st, types.Volume{Name: "v1", Node: "test-node", Backup: true, Freq: freq, ProjectID: 7})
	if err := st.PutSchedule(ctx, "v1", freq, base.Unix()); err != nil {
		t.Fatal(err)
	}
	pause(t, st)
	s.fireDue(ctx)
	unpause(t, st)
	s.processCatchups(ctx)
	if n := pendingBackups(t, st, "v1"); n != 1 {
		t.Fatalf("catch-up tasks = %d, want 1", n)
	}
}

// hourlyMissed: a volume whose slot after now-lookback was skipped while paused;
// the exit is backdated past the offset so the catch-up is due.
func hourlyMissed(t *testing.T, st *store.Store, s *Scheduler, freq string, project int, lookback time.Duration) {
	t.Helper()
	vol := types.Volume{Name: "v1", Node: "test-node", Backup: true, Freq: freq, ProjectID: project}
	off := spreadOffset(freq, vol)
	putVol(t, st, vol)
	slot := store.NextFireOffset(freq, off, time.Now().Add(-lookback)).Unix()
	if err := st.PutSchedule(context.Background(), "v1", freq, slot); err != nil {
		t.Fatal(err)
	}
	pause(t, st)
	s.fireDue(context.Background())
	unpause(t, st)
	backdateExit(t, st, "v1", int64(off/time.Second)+1)
}

// Hourly: the next slot is within 2h of the exit, so no catch-up.
func TestScheduler_CatchupHourlySkipped(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	s := newTestScheduler(t, st)
	setSpreadCap(t, 7200)
	setCatchup(t, true, 7200)
	hourlyMissed(t, st, s, "0 * * * *", 7, 2*time.Hour)
	s.processCatchups(ctx)
	if n := pendingBackups(t, st, "v1"); n != 0 {
		t.Fatalf("hourly catch-up fired (%d tasks)", n)
	}
	if _, ok := marker(t, st, "v1"); ok {
		t.Fatal("marker left after the catch-up was dropped")
	}
}

// Threshold 0 catches up even when the next slot is near.
func TestScheduler_CatchupThresholdZero(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	s := newTestScheduler(t, st)
	setSpreadCap(t, 0)
	setCatchup(t, true, 0)
	hourlyMissed(t, st, s, "0 * * * *", 7, 2*time.Hour)
	s.processCatchups(ctx)
	if n := pendingBackups(t, st, "v1"); n != 1 {
		t.Fatalf("catch-up tasks = %d, want 1", n)
	}
}

// A 3-hourly schedule whose next slot is 2.5h away is caught up.
func TestScheduler_CatchupThreeHourly(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	s := newTestScheduler(t, st)
	setSpreadCap(t, 600) // offset < 10m, so the next slot stays > 2h after the exit
	setCatchup(t, true, 7200)
	last := time.Now().Truncate(time.Minute).Add(-30 * time.Minute)
	hours := ""
	for k := 0; k < 8; k++ {
		if k > 0 {
			hours += ","
		}
		hours += strconv.Itoa((last.Hour() + 3*k) % 24)
	}
	freq := fmt.Sprintf("%d %s * * *", last.Minute(), hours)
	hourlyMissed(t, st, s, freq, 7, 4*time.Hour)
	s.processCatchups(ctx)
	if n := pendingBackups(t, st, "v1"); n != 1 {
		t.Fatalf("catch-up tasks = %d, want 1", n)
	}
}

// With a small threshold, a catch-up whose time falls after the next regular
// slot is dropped: exit shortly after a cron instant, before its offset. By the
// time the catch-up is due the regular slot is due too; whether or not it has
// fired yet, no extra backup is created.
func TestScheduler_CatchupRegularSlotFirst(t *testing.T) {
	for _, fireFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("fireDue first=%v", fireFirst), func(t *testing.T) {
			ctx := context.Background()
			st := testStore(t)
			s := newTestScheduler(t, st)
			setSpreadCap(t, 7200)
			setCatchup(t, true, 60)
			last := time.Now().Truncate(time.Minute).Add(-time.Minute)
			freq := fmt.Sprintf("%d * * * *", last.Minute())
			project, off := projectWithOffset(t, freq, 5*time.Minute)
			putVol(t, st, types.Volume{Name: "v1", Node: "test-node", Backup: true, Freq: freq, ProjectID: project})
			// The previous hour's slot, missed during the pause.
			if err := st.PutSchedule(ctx, "v1", freq, last.Add(-time.Hour).Add(off).Unix()); err != nil {
				t.Fatal(err)
			}
			pause(t, st)
			s.fireDue(ctx)
			unpause(t, st)
			m, _ := marker(t, st, "v1")
			if m.Next != last.Add(off).Unix() {
				t.Fatalf("next = %d, want this hour's slot %d", m.Next, last.Add(off).Unix())
			}
			// Let time pass until the catch-up is due: shift the exit and the row
			// back by the same amount.
			delta := m.Exit + m.Phase - time.Now().Unix()
			if moved, err := st.MoveScheduleFire(ctx, "v1", m.Next, m.Next-delta); err != nil || !moved {
				t.Fatalf("shift row: %v %v", moved, err)
			}
			if err := st.SetMeta(ctx, "catchup.v1", fmt.Sprintf(`{"exit":%d,"phase":%d,"next":%d}`,
				m.Exit-delta, m.Phase, m.Next-delta)); err != nil {
				t.Fatal(err)
			}
			want := 0
			if fireFirst {
				s.fireDue(ctx)
				want = 1
			}
			s.processCatchups(ctx)
			if n := pendingBackups(t, st, "v1"); n != want {
				t.Fatalf("backups pending = %d, want %d", n, want)
			}
			if _, ok := marker(t, st, "v1"); ok {
				t.Fatal("marker left")
			}
		})
	}
}

// A volume created during maintenance never missed a slot: no catch-up.
func TestScheduler_CatchupNewVolumeDuringMaintenance(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	s := newTestScheduler(t, st)
	setCatchup(t, true, 7200)
	pause(t, st)
	putVol(t, st, types.Volume{Name: "v1", Node: "test-node", Backup: true, Freq: "0 3 * * *", ProjectID: 7})
	s.reconcile(ctx)
	s.fireDue(ctx)
	unpause(t, st)
	if _, ok := marker(t, st, "v1"); ok {
		t.Fatal("catch-up recorded for a volume that missed nothing")
	}
}

// Disabled: the catch-up is dropped, nothing fires, the row is not moved.
func TestScheduler_CatchupDisabled(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	s := newTestScheduler(t, st)
	setSpreadCap(t, 0)
	setCatchup(t, false, 7200)
	dailyMissed(t, st, s, "v1", 7)
	sc, _, _ := st.GetSchedule(ctx, "v1")
	s.processCatchups(ctx)
	if n := pendingBackups(t, st, "v1"); n != 0 {
		t.Fatalf("catch-up fired while disabled (%d tasks)", n)
	}
	if _, ok := marker(t, st, "v1"); ok {
		t.Fatal("marker left while disabled")
	}
	if after, _, _ := st.GetSchedule(ctx, "v1"); after.NextFireAt != sc.NextFireAt {
		t.Fatal("schedule moved while disabled")
	}
}

// A volume trashed or with backups turned off after the exit gets no catch-up.
func TestScheduler_CatchupVolumeNoLongerBackedUp(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	s := newTestScheduler(t, st)
	setSpreadCap(t, 0)
	setCatchup(t, true, 7200)
	dailyMissed(t, st, s, "trashed", 7)
	dailyMissed(t, st, s, "off", 8)
	putVol(t, st, types.Volume{Name: "trashed", Node: "test-node", Backup: true, Trash: true, ProjectID: 7})
	putVol(t, st, types.Volume{Name: "off", Node: "test-node", Backup: false, ProjectID: 8})
	s.processCatchups(ctx)
	for _, vol := range []string{"trashed", "off"} {
		if n := pendingBackups(t, st, vol); n != 0 {
			t.Fatalf("%s: catch-up fired (%d tasks)", vol, n)
		}
		if _, ok := marker(t, st, vol); ok {
			t.Fatalf("%s: marker left", vol)
		}
	}
}

// Paused again before the catch-up runs: it is kept, and the next exit rewrites
// it to that exit, so it fires at exit2 + offset — not at once — and only once.
// A later window that misses a slot while the catch-up is still pending adds no
// second task.
func TestScheduler_CatchupRepeatedPause(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	s := newTestScheduler(t, st)
	setSpreadCap(t, 7200)
	setCatchup(t, true, 7200)
	project, _ := projectWithOffset(t, "0 3 * * *", 5*time.Minute)
	off := dailyMissed(t, st, s, "v1", project)
	backdateExit(t, st, "v1", 86400) // exit1 + offset is long past

	pause(t, st)
	s.processCatchups(ctx)
	if n := pendingBackups(t, st, "v1"); n != 0 {
		t.Fatal("catch-up fired while paused")
	}
	if _, ok := marker(t, st, "v1"); !ok {
		t.Fatal("catch-up dropped while paused")
	}
	before := time.Now().Unix()
	unpause(t, st)
	if m, _ := marker(t, st, "v1"); m.Exit < before {
		t.Fatalf("exit not rewritten: %d < %d", m.Exit, before)
	}
	s.processCatchups(ctx)
	if n := pendingBackups(t, st, "v1"); n != 0 {
		t.Fatal("catch-up fired at exit2 instead of exit2 + offset")
	}
	backdateExit(t, st, "v1", int64(off/time.Second))
	s.processCatchups(ctx)
	s.processCatchups(ctx)
	if n := pendingBackups(t, st, "v1"); n != 1 {
		t.Fatalf("catch-up tasks = %d, want 1", n)
	}

	// Another window misses a slot while that backup is still pending.
	sc, _, _ := st.GetSchedule(ctx, "v1")
	if _, err := st.MoveScheduleFire(ctx, "v1", sc.NextFireAt, sc.NextFireAt-86400); err != nil {
		t.Fatal(err)
	}
	pause(t, st)
	s.fireDue(ctx)
	unpause(t, st)
	backdateExit(t, st, "v1", int64(off/time.Second))
	s.processCatchups(ctx)
	if n := pendingBackups(t, st, "v1"); n != 1 {
		t.Fatalf("backups pending = %d, want 1 (no second catch-up)", n)
	}
}

// An exit while the agent was down: catch-ups spread from the scheduler's start
// instead of all firing at boot.
func TestScheduler_CatchupSpreadFromStart(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	s := newTestScheduler(t, st)
	setSpreadCap(t, 7200)
	setCatchup(t, true, 7200)
	// Two projects with clearly different daily offsets.
	freq := "0 3 * * *"
	pa, offA := projectWithOffset(t, freq, 0)
	pb, offB := 0, time.Duration(0)
	for id := pa + 1; id < 1000; id++ {
		if o := store.ScheduleOffset(freq, store.OffsetKey(id, ""), 2*time.Hour); o > offA+10*time.Minute {
			pb, offB = id, o
			break
		}
	}
	if pb == 0 {
		t.Fatal("no second project with a larger offset")
	}
	dailyMissed(t, st, s, "a", pa)
	dailyMissed(t, st, s, "b", pb)
	// Both exits were 10h ago; the daemon started just over offA ago.
	ma, _ := marker(t, st, "a")
	mb, _ := marker(t, st, "b")
	backdateExit(t, st, "a", 10*3600)
	backdateExit(t, st, "b", 10*3600)
	if ma.Phase != int64(offA/time.Second) || mb.Phase != int64(offB/time.Second) {
		t.Fatalf("phases %d/%d, want %v/%v", ma.Phase, mb.Phase, offA, offB)
	}
	s.started = time.Now().Add(-offA - time.Second)
	s.processCatchups(ctx)
	if n := pendingBackups(t, st, "a"); n != 1 {
		t.Fatalf("a: catch-up tasks = %d, want 1", n)
	}
	if n := pendingBackups(t, st, "b"); n != 0 {
		t.Fatal("b: catch-up fired at boot instead of start + its offset")
	}
}
