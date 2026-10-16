package store

import (
	"testing"
	"time"
)

func TestControlMigrations_V4(t *testing.T) {
	s := open(t, Options{})
	if got := countTable(t, s, "schedules"); got != 0 {
		t.Fatalf("schedules not empty on fresh open: %d", got)
	}
	// The convergence bake-in columns must exist (query errors if a column is absent).
	for _, q := range []string{
		`SELECT count(generation), count(applied_generation) FROM volumes`,
		`SELECT count(generation), count(applied_generation) FROM firewall_rules`,
	} {
		var a, b int
		if err := s.control.QueryRowContext(ctx, q).Scan(&a, &b); err != nil {
			t.Fatalf("convergence columns missing (%s): %v", q, err)
		}
	}
}

func TestScheduleCRUD(t *testing.T) {
	s := open(t, Options{})
	if _, found, err := s.GetSchedule(ctx, "vol-1"); err != nil || found {
		t.Fatalf("get missing: found=%v err=%v", found, err)
	}
	if err := s.PutSchedule(ctx, "vol-1", "0 2 * * *", 1000); err != nil {
		t.Fatalf("PutSchedule: %v", err)
	}
	sc, found, err := s.GetSchedule(ctx, "vol-1")
	if err != nil || !found {
		t.Fatalf("GetSchedule: found=%v err=%v", found, err)
	}
	if sc.CronExpr != "0 2 * * *" || sc.NextFireAt != 1000 {
		t.Fatalf("schedule: %+v", sc)
	}
	// A reschedule (cron changed) overwrites cron_expr + next_fire_at.
	if err := s.PutSchedule(ctx, "vol-1", "0 5 * * *", 2000); err != nil {
		t.Fatal(err)
	}
	sc, _, _ = s.GetSchedule(ctx, "vol-1")
	if sc.CronExpr != "0 5 * * *" || sc.NextFireAt != 2000 {
		t.Fatalf("after reschedule: %+v", sc)
	}
	// Schedules are node-local state, never changelogged.
	if got := countTable(t, s, "changelog"); got != 0 {
		t.Fatalf("changelog = %d, want 0 (schedules must not changelog)", got)
	}
	if err := s.DeleteSchedule(ctx, "vol-1"); err != nil {
		t.Fatalf("DeleteSchedule: %v", err)
	}
	if got := countTable(t, s, "schedules"); got != 0 {
		t.Fatalf("schedules = %d, want 0", got)
	}
	// Deleting an absent schedule is a no-op.
	if err := s.DeleteSchedule(ctx, "gone"); err != nil {
		t.Fatalf("delete absent: %v", err)
	}
}

func TestListDueSchedules(t *testing.T) {
	s := open(t, Options{})
	if err := s.PutSchedule(ctx, "due-1", "* * * * *", 100); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSchedule(ctx, "due-2", "* * * * *", 150); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSchedule(ctx, "future", "* * * * *", 500); err != nil {
		t.Fatal(err)
	}
	due, err := s.ListDueSchedules(ctx, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 2 {
		t.Fatalf("due = %d, want 2", len(due))
	}
	// Oldest-due first.
	if due[0].VolumeName != "due-1" || due[1].VolumeName != "due-2" {
		t.Fatalf("due order: %+v", due)
	}
	if all, _ := s.ListSchedules(ctx); len(all) != 3 {
		t.Fatalf("ListSchedules = %d, want 3", len(all))
	}
}

// TestFireDueBackup_ExactlyOnce proves the SQLite payoff: firing a due schedule
// creates the task AND advances next_fire_at in one transaction, so the same
// schedule is no longer due afterward (no double fire) — and the schedule advance
// is not changelogged while the task creation is.
func TestFireDueBackup_ExactlyOnce(t *testing.T) {
	s := open(t, Options{})
	if err := s.PutSchedule(ctx, "vol-1", "0 2 * * *", 100); err != nil {
		t.Fatal(err)
	}
	created, err := s.FireDueBackup(ctx, Task{
		ID: "auto-1", Name: "volume.backup", Node: "node-a", Volume: "vol-1", ProjectID: "proj-1",
	}, 3700)
	if err != nil || !created {
		t.Fatalf("FireDueBackup: created=%v err=%v", created, err)
	}
	// Task exists, pending.
	tk, found, _ := s.GetTask(ctx, "auto-1")
	if !found || tk.Status != TaskPending || tk.Volume != "vol-1" {
		t.Fatalf("fired task: found=%v %+v", found, tk)
	}
	// next_fire_at advanced past the old due time -> no longer due at t=200.
	sc, _, _ := s.GetSchedule(ctx, "vol-1")
	if sc.NextFireAt != 3700 {
		t.Fatalf("next_fire_at = %d, want 3700", sc.NextFireAt)
	}
	if due, _ := s.ListDueSchedules(ctx, 200); len(due) != 0 {
		t.Fatalf("still due after fire: %d", len(due))
	}
	// The task creation is changelogged; the schedule advance is not (1 row).
	if got := countTable(t, s, "changelog"); got != 1 {
		t.Fatalf("changelog = %d, want 1 (task upsert only)", got)
	}
}

func TestClaimTask_CAS(t *testing.T) {
	s := open(t, Options{})
	if _, err := s.CreateTask(ctx, Task{ID: "j1", Name: "volume.backup", Node: "n"}); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimTask(ctx, "j1")
	if err != nil || !claimed {
		t.Fatalf("first claim: claimed=%v err=%v", claimed, err)
	}
	got, _, _ := s.GetTask(ctx, "j1")
	if got.Status != TaskRunning {
		t.Fatalf("status = %q, want running", got.Status)
	}
	// A second claim (already running) is a no-op — this is what prevents a wake
	// signal and the backstop drain from double-dispatching one task.
	claimed, err = s.ClaimTask(ctx, "j1")
	if err != nil || claimed {
		t.Fatalf("second claim: claimed=%v err=%v, want false", claimed, err)
	}
	// create + claim = 2 changelog rows (the no-op second claim adds none).
	if got := countTable(t, s, "changelog"); got != 2 {
		t.Fatalf("changelog = %d, want 2", got)
	}
	if c, err := s.ClaimTask(ctx, "absent"); err != nil || c {
		t.Fatalf("claim absent: c=%v err=%v", c, err)
	}
}

func TestUnclaimTask_RevertsRunningOnly(t *testing.T) {
	s := open(t, Options{})
	if _, err := s.CreateTask(ctx, Task{ID: "e1", Name: "backup.export", Node: "n"}); err != nil {
		t.Fatal(err)
	}
	// Cannot unclaim a pending task (only running).
	if u, err := s.UnclaimTask(ctx, "e1"); err != nil || u {
		t.Fatalf("unclaim pending: u=%v err=%v, want false", u, err)
	}
	if _, err := s.ClaimTask(ctx, "e1"); err != nil {
		t.Fatal(err)
	}
	// Now running -> unclaim reverts to pending (export pool was full).
	u, err := s.UnclaimTask(ctx, "e1")
	if err != nil || !u {
		t.Fatalf("unclaim running: u=%v err=%v", u, err)
	}
	got, _, _ := s.GetTask(ctx, "e1")
	if got.Status != TaskPending {
		t.Fatalf("status = %q, want pending", got.Status)
	}
	// A terminal task can't be unclaimed (guards against stomping a finished export).
	if _, err := s.ClaimTask(ctx, "e1"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateTaskStatus(ctx, "e1", TaskCompleted, nil); err != nil {
		t.Fatal(err)
	}
	if u, err := s.UnclaimTask(ctx, "e1"); err != nil || u {
		t.Fatalf("unclaim completed: u=%v err=%v, want false", u, err)
	}
}

func TestListRunningTasks(t *testing.T) {
	s := open(t, Options{})
	for _, id := range []string{"r1", "r2", "r3"} {
		if _, err := s.CreateTask(ctx, Task{ID: id, Name: "volume.backup", Node: "node-a"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.ClaimTask(ctx, "r1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimTask(ctx, "r2"); err != nil {
		t.Fatal(err)
	}
	// A different node label is still returned: the DB is the node scope, so
	// ListRunningTasks does not filter by node.
	if _, err := s.CreateTask(ctx, Task{ID: "x1", Name: "volume.backup", Node: "node-b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimTask(ctx, "x1"); err != nil {
		t.Fatal(err)
	}
	running, err := s.ListRunningTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(running) != 3 {
		t.Fatalf("running = %d, want 3 (all running regardless of node label)", len(running))
	}
}

func TestMinInterval(t *testing.T) {
	for _, tc := range []struct {
		expr string
		want time.Duration
	}{
		{"0 * * * *", time.Hour},
		{"*/30 * * * *", 30 * time.Minute},
		{"0 2 * * *", 24 * time.Hour},
		{"0 2,3 * * *", time.Hour},
		{"0 2 * * 0", 7 * 24 * time.Hour},
		{"@every 90m", 90 * time.Minute},
		{"garbage", 0},
	} {
		if got := minInterval(tc.expr); got != tc.want {
			t.Errorf("minInterval(%q) = %v, want %v", tc.expr, got, tc.want)
		}
	}
	// Monthly: the shortest month bounds it (DST may shave an hour off).
	if got := minInterval("0 2 1 * *"); got < 27*24*time.Hour || got > 28*24*time.Hour {
		t.Errorf("monthly minInterval = %v, want ~28d", got)
	}
}

func TestOffsetKey(t *testing.T) {
	if got := OffsetKey(7, "vol-a"); got != "project:7" {
		t.Fatalf("OffsetKey(7) = %q", got)
	}
	if OffsetKey(7, "vol-a") != OffsetKey(7, "vol-b") {
		t.Fatal("volumes of one project got different keys")
	}
	if got := OffsetKey(0, "vol-a"); got != "volume:vol-a" {
		t.Fatalf("OffsetKey(0) = %q", got)
	}
}

func TestScheduleOffset_Window(t *testing.T) {
	const cap2h = 2 * time.Hour
	for _, tc := range []struct {
		expr      string
		maxWindow time.Duration
		window    time.Duration
	}{
		{"0 * * * *", cap2h, 15 * time.Minute},
		{"*/30 * * * *", cap2h, 450 * time.Second},
		{"0 2 * * *", cap2h, cap2h},
		{"0 */6 * * *", cap2h, 90 * time.Minute},
		{"0 2,3 * * *", cap2h, 15 * time.Minute},
		// Weekly/monthly get a real interval: only the cap bounds them.
		{"0 2 * * 0", cap2h, cap2h},
		{"0 2 * * 0", 1000 * time.Hour, 42 * time.Hour},
		{"0 2 1 * *", 1000 * time.Hour, 7 * 24 * time.Hour},
	} {
		var widest time.Duration
		for i := 0; i < 200; i++ {
			off := ScheduleOffset(tc.expr, OffsetKey(i+1, ""), tc.maxWindow)
			if off < 0 || off >= tc.window {
				t.Fatalf("%q cap %v: offset %v outside [0,%v)", tc.expr, tc.maxWindow, off, tc.window)
			}
			if off%time.Second != 0 {
				t.Fatalf("%q: offset %v is not whole seconds", tc.expr, off)
			}
			widest = max(widest, off)
		}
		// 200 keys spread over the window, not bunched at its start.
		if widest < tc.window/2 {
			t.Fatalf("%q cap %v: widest offset %v of window %v", tc.expr, tc.maxWindow, widest, tc.window)
		}
	}
	for _, tc := range []struct {
		expr      string
		maxWindow time.Duration
	}{
		{"0 * * * *", 0},
		{"0 * * * *", -time.Hour},
		{"garbage", time.Hour},
		{"0 0 30 2 *", time.Hour}, // never fires
	} {
		if off := ScheduleOffset(tc.expr, "project:1", tc.maxWindow); off != 0 {
			t.Fatalf("ScheduleOffset(%q, cap %v) = %v, want 0", tc.expr, tc.maxWindow, off)
		}
	}
}

// TestScheduleOffset_Deterministic pins offsets to precomputed values: they
// depend only on the expression, key and cap — never on the wall clock — so every
// node and every restart computes the same offset.
func TestScheduleOffset_Deterministic(t *testing.T) {
	want := map[string]time.Duration{
		"project:1": 3111 * time.Second,
		"project:2": 5722 * time.Second,
		"project:3": 1133 * time.Second,
		"volume:a":  874 * time.Second,
	}
	seen := map[time.Duration]string{}
	for key, w := range want {
		if got := ScheduleOffset("0 2 * * *", key, 2*time.Hour); got != w {
			t.Fatalf("ScheduleOffset(%q) = %v, want %v", key, got, w)
		}
		if other, dup := seen[w]; dup {
			t.Fatalf("keys %q and %q share offset %v", key, other, w)
		}
		seen[w] = key
	}
	if a, b := ScheduleOffset("0 * * * *", OffsetKey(5, "files"), time.Hour),
		ScheduleOffset("0 * * * *", OffsetKey(5, "db"), time.Hour); a != b {
		t.Fatalf("volumes of one project: offsets %v != %v", a, b)
	}
}

func TestNextFireOffset(t *testing.T) {
	at := func(h, m, s int) time.Time { return time.Date(2026, 1, 5, h, m, s, 0, time.Local) }
	const off = 7 * time.Minute
	for _, tc := range []struct {
		from, want time.Time
	}{
		{at(10, 0, 0), at(10, 7, 0)},
		{at(10, 6, 59), at(10, 7, 0)},
		{at(10, 7, 0), at(11, 7, 0)},
	} {
		if got := NextFireOffset("0 * * * *", off, tc.from); !got.Equal(tc.want) {
			t.Fatalf("NextFireOffset(from %v) = %v, want %v", tc.from, got, tc.want)
		}
	}
	if got := NextFireOffset("0 * * * *", 0, at(10, 0, 0)); !got.Equal(NextFire("0 * * * *", at(10, 0, 0))) {
		t.Fatalf("offset 0 = %v, want NextFire", got)
	}
	if got := NextFireOffset("garbage", off, at(10, 0, 0)); !got.IsZero() {
		t.Fatalf("unparseable = %v, want zero", got)
	}
}

func TestAlignNextFire(t *testing.T) {
	at := func(d, h, m int) int64 { return time.Date(2026, 1, d, h, m, 0, 0, time.Local).Unix() }
	const daily = "0 2 * * *"
	for _, tc := range []struct {
		name     string
		expr     string
		off      time.Duration
		existing int64
		want     int64
	}{
		{"unshifted moves onto offset", daily, 40 * time.Minute, at(5, 2, 0), at(5, 2, 40)},
		{"already aligned is unchanged", daily, 40 * time.Minute, at(5, 2, 40), at(5, 2, 40)},
		{"offset increase", daily, 90 * time.Minute, at(5, 2, 40), at(5, 3, 30)},
		{"offset decrease moves earlier", daily, 10 * time.Minute, at(5, 2, 40), at(5, 2, 10)},
		{"cap 0 on shifted row lands on the same-day instant", daily, 0, at(5, 2, 40), at(5, 2, 0)},
		{"offset 0 on unshifted row is unchanged", daily, 0, at(5, 2, 0), at(5, 2, 0)},
		{"past row stays in its own slot", daily, 40 * time.Minute, at(1, 2, 0), at(1, 2, 40)},
		// Off grid (more than a quarter interval past an instant): first shifted
		// slot at or after existing.
		{"fallback to next shifted slot", "0 * * * *", 7 * time.Minute, at(5, 10, 30), at(5, 11, 7)},
		{"row just past an instant belongs to it", "0 * * * *", 7 * time.Minute, at(5, 10, 0) + 1, at(5, 10, 7)},
	} {
		if got := AlignNextFire(tc.expr, tc.off, tc.existing); got != tc.want {
			t.Errorf("%s: AlignNextFire = %s, want %s", tc.name, time.Unix(got, 0), time.Unix(tc.want, 0))
			continue
		}
		// Idempotent: an aligned row never moves again.
		if again := AlignNextFire(tc.expr, tc.off, tc.want); again != tc.want {
			t.Errorf("%s: not idempotent: %s -> %s", tc.name, time.Unix(tc.want, 0), time.Unix(again, 0))
		}
	}
	// No fixed grid (@every) and unparseable: never moved.
	for _, expr := range []string{"@every 90m", "garbage"} {
		if got := AlignNextFire(expr, 5*time.Minute, at(5, 10, 3)); got != at(5, 10, 3) {
			t.Errorf("AlignNextFire(%q) moved the row to %s", expr, time.Unix(got, 0))
		}
	}
}

func TestSchedulePhase(t *testing.T) {
	base := time.Date(2026, 1, 5, 2, 0, 0, 0, time.Local).Unix()
	if got := SchedulePhase("0 2 * * *", base+40*60); got != 40*60 {
		t.Fatalf("phase = %d, want 2400", got)
	}
	if got := SchedulePhase("0 2 * * *", base); got != 0 {
		t.Fatalf("phase on instant = %d, want 0", got)
	}
	if got := SchedulePhase("0 2 * * *", base+12*3600); got != 0 {
		t.Fatalf("off-grid phase = %d, want 0", got)
	}
	if got := SchedulePhase("garbage", base); got != 0 {
		t.Fatalf("unparseable phase = %d, want 0", got)
	}
}
