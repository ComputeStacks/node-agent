package store

import (
	"encoding/json"
	"testing"
	"time"
)

// rawCatchup returns a volume's stored catch-up marker ("" if none).
func rawCatchup(t *testing.T, s *Store, vol string) string {
	t.Helper()
	v, _, err := s.GetMeta(ctx, catchupKey(vol))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func decodeCatchup(t *testing.T, raw string) catchupMarker {
	t.Helper()
	var mk catchupMarker
	if err := json.Unmarshal([]byte(raw), &mk); err != nil {
		t.Fatalf("decode marker %q: %v", raw, err)
	}
	return mk
}

func pauseLocal(t *testing.T, s *Store) {
	t.Helper()
	if _, _, err := s.PutLocalHold(ctx, "l", "root"); err != nil {
		t.Fatal(err)
	}
}

func exitLocal(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.ClearLocalHold(ctx); err != nil {
		t.Fatal(err)
	}
}

// listedCatchup returns vol's catch-up as ListCatchups reports it.
func listedCatchup(t *testing.T, s *Store, vol string) (Catchup, bool) {
	t.Helper()
	all, err := s.ListCatchups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range all {
		if c.Volume == vol {
			return c, true
		}
	}
	return Catchup{}, false
}

// Both paths that skip a slot while paused record the miss; a miss flag is not
// a catch-up yet, and an existing exit marker is left in place.
func TestCatchup_SkipMarksMissed(t *testing.T) {
	s := open(t, Options{})
	for _, vol := range []string{"a", "b"} {
		if err := s.PutSchedule(ctx, vol, "0 2 * * *", 10); err != nil {
			t.Fatal(err)
		}
	}
	pauseLocal(t, s)
	if err := s.SkipDueBackup(ctx, "a", 500); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FireDueBackup(ctx, Task{ID: "x", Name: "volume.backup", Node: "n", Volume: "b"}, 500); err != nil {
		t.Fatal(err)
	}
	for _, vol := range []string{"a", "b"} {
		if got := rawCatchup(t, s, vol); got != catchupMissedFlag {
			t.Fatalf("%s marker = %q, want the miss flag", vol, got)
		}
	}
	if all, _ := s.ListCatchups(ctx); len(all) != 0 {
		t.Fatalf("miss flags listed as catch-ups: %+v", all)
	}

	const exitMarker = `{"exit":100,"phase":0,"next":200}`
	if err := s.SetMeta(ctx, catchupKey("a"), exitMarker); err != nil {
		t.Fatal(err)
	}
	if err := s.SkipDueBackup(ctx, "a", 600); err != nil {
		t.Fatal(err)
	}
	if got := rawCatchup(t, s, "a"); got != exitMarker {
		t.Fatalf("skip replaced an exit marker: %q", got)
	}
	// The maintenance snapshot does not see the markers.
	if _, err := s.GetMaintenance(ctx); err != nil {
		t.Fatalf("GetMaintenance with catch-up markers: %v", err)
	}
}

// The exit (through the same API the CLI uses) turns flagged and overdue rows
// into catch-ups carrying the row's phase and its new next slot; other rows get
// none, and a flag for a volume without a schedule is removed.
func TestCatchup_ExitRecords(t *testing.T) {
	s := open(t, Options{})
	const expr = "0 * * * *"
	const phase = 7 * 60
	base := NextFire(expr, time.Now().Add(-3*time.Hour)).Unix()
	for vol, next := range map[string]int64{
		"flagged": base + 3600 + phase, // skipped below
		"overdue": base + phase,        // came due, not skipped yet
		"clean":   time.Now().Unix() + 3600,
	} {
		if err := s.PutSchedule(ctx, vol, expr, next); err != nil {
			t.Fatal(err)
		}
	}
	pauseLocal(t, s)
	if err := s.SkipDueBackup(ctx, "flagged", base+2*3600+phase); err != nil {
		t.Fatal(err)
	}
	if err := s.SkipDueBackup(ctx, "gone", 1); err != nil { // no schedule row
		t.Fatal(err)
	}
	// Push the flagged row into the future again so only its flag qualifies it.
	if _, err := s.control.ExecContext(ctx,
		`UPDATE schedules SET next_fire_at = ? WHERE volume_name = 'flagged'`, time.Now().Unix()+600); err != nil {
		t.Fatal(err)
	}
	before := time.Now().Unix()
	exitLocal(t, s)
	after := time.Now().Unix()

	for _, vol := range []string{"flagged", "overdue"} {
		c, ok := listedCatchup(t, s, vol)
		if !ok {
			t.Fatalf("%s: no catch-up recorded", vol)
		}
		sc, _, _ := s.GetSchedule(ctx, vol)
		if c.Exit < before || c.Exit > after || c.Next != sc.NextFireAt {
			t.Fatalf("%s catch-up = %+v, schedule next %d", vol, c, sc.NextFireAt)
		}
		if vol == "overdue" && c.Phase != phase {
			t.Fatalf("overdue phase = %d, want %d", c.Phase, phase)
		}
	}
	for _, vol := range []string{"clean", "gone"} {
		if got := rawCatchup(t, s, vol); got != "" {
			t.Fatalf("%s marker = %q, want none", vol, got)
		}
	}
	if m := mustMaint(t, s); m.Paused() {
		t.Fatalf("still paused: %+v", m)
	}
}

// Repeated pause/exit: an unconsumed catch-up is rewritten to the latest exit,
// so it is judged and spread from there; on/off with nothing missed records
// nothing.
func TestCatchup_RepeatedExit(t *testing.T) {
	s := open(t, Options{})
	if err := s.PutSchedule(ctx, "v1", "0 2 * * *", time.Now().Unix()+3600); err != nil {
		t.Fatal(err)
	}
	pauseLocal(t, s)
	exitLocal(t, s)
	if all, _ := s.ListCatchups(ctx); len(all) != 0 || rawCatchup(t, s, "v1") != "" {
		t.Fatalf("on/off with nothing missed recorded %+v", all)
	}

	if err := s.SetMeta(ctx, catchupKey("v1"), `{"exit":100,"phase":30,"next":200}`); err != nil {
		t.Fatal(err)
	}
	pauseLocal(t, s)
	before := time.Now().Unix()
	exitLocal(t, s)
	c, ok := listedCatchup(t, s, "v1")
	if !ok || c.Exit < before {
		t.Fatalf("catch-up not rewritten to the latest exit: %+v (ok=%v)", c, ok)
	}
	sc, _, _ := s.GetSchedule(ctx, "v1")
	if c.Next != sc.NextFireAt || c.Phase != 0 {
		t.Fatalf("rewritten catch-up = %+v, schedule %+v", c, sc)
	}
}

// consumeSetup writes an exit marker for v1 with a daily schedule whose next
// slot is next, and returns the listed catch-up.
func consumeSetup(t *testing.T, s *Store, exit, phase, next int64) Catchup {
	t.Helper()
	if err := s.PutSchedule(ctx, "v1", "0 2 * * *", next); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(catchupMarker{Exit: exit, Phase: phase, Next: next})
	if err := s.SetMeta(ctx, catchupKey("v1"), string(b)); err != nil {
		t.Fatal(err)
	}
	c, ok := listedCatchup(t, s, "v1")
	if !ok {
		t.Fatal("catch-up not listed")
	}
	return c
}

func catchupTask(id string) *Task {
	return &Task{ID: id, Name: "volume.backup", Node: "n", Volume: "v1", ProjectID: "7", Archive: "auto"}
}

func TestConsumeCatchup_Fires(t *testing.T) {
	s := open(t, Options{})
	next := NextFire("0 2 * * *", time.Now().Add(3*time.Hour)).Unix()
	exit := time.Now().Unix() - 60
	c := consumeSetup(t, s, exit, 30, next)

	out, reason, err := s.ConsumeCatchup(ctx, c, exit+30, 7200, catchupTask("c1"))
	if err != nil || out != CatchupFired {
		t.Fatalf("consume = %v %q %v, want fired", out, reason, err)
	}
	tk, found, _ := s.GetTask(ctx, "c1")
	if !found || tk.Status != TaskPending || tk.Volume != "v1" || tk.CreatedAt == 0 {
		t.Fatalf("catch-up task: found=%v %+v", found, tk)
	}
	es, _, err := s.ChangelogPage(ctx, 0, "task", 100, true)
	if err != nil || len(es) != 1 || es[0].EntityID != "c1" {
		t.Fatalf("task changelog = %+v %v", es, err)
	}
	if rawCatchup(t, s, "v1") != "" {
		t.Fatal("marker left after firing")
	}
	if sc, _, _ := s.GetSchedule(ctx, "v1"); sc.NextFireAt != next {
		t.Fatalf("schedule moved: %d, want %d", sc.NextFireAt, next)
	}
	// Re-running the same consume (e.g. after a crash before the caller saw the
	// result) is a no-op.
	if out, _, err := s.ConsumeCatchup(ctx, c, exit+30, 7200, catchupTask("c2")); err != nil || out != CatchupKept {
		t.Fatalf("second consume = %v %v, want kept", out, err)
	}
	if _, found, _ := s.GetTask(ctx, "c2"); found {
		t.Fatal("second consume created a task")
	}
}

func TestConsumeCatchup_KeptAndDropped(t *testing.T) {
	now := time.Now().Unix()
	far := NextFire("0 2 * * *", time.Now().Add(3*time.Hour)).Unix()
	cases := []struct {
		name      string
		next      int64 // schedule next_fire_at (and the marker's next)
		fireAt    int64
		threshold int64
		prep      func(t *testing.T, s *Store)
		nilTask   bool
		want      CatchupOutcome
	}{
		{name: "marker changed", next: far, fireAt: now, threshold: 7200, want: CatchupKept,
			prep: func(t *testing.T, s *Store) {
				if err := s.SetMeta(ctx, catchupKey("v1"), `{"exit":1,"phase":0,"next":2}`); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "paused", next: far, fireAt: now, threshold: 7200, want: CatchupKept,
			prep: func(t *testing.T, s *Store) { pauseLocal(t, s) }},
		{name: "nil task", next: far, fireAt: now, threshold: 7200, nilTask: true, want: CatchupDropped},
		{name: "no schedule", next: far, fireAt: now, threshold: 7200, want: CatchupDropped,
			prep: func(t *testing.T, s *Store) { _ = s.DeleteSchedule(ctx, "v1") }},
		{name: "slot already ran", next: far, fireAt: now, threshold: 7200, want: CatchupDropped,
			prep: func(t *testing.T, s *Store) {
				if err := s.PutSchedule(ctx, "v1", "0 2 * * *", far+86400); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "within threshold", next: now + 3600, fireAt: now, threshold: 7200, want: CatchupDropped},
		{name: "regular slot first", next: now + 3600, fireAt: now + 3600, threshold: 0, want: CatchupDropped},
		{name: "backup pending", next: far, fireAt: now, threshold: 7200, want: CatchupDropped,
			prep: func(t *testing.T, s *Store) {
				if _, err := s.CreateTask(ctx, Task{ID: "p", Name: "volume.backup", Node: "n", Volume: "v1"}); err != nil {
					t.Fatal(err)
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := open(t, Options{})
			c := consumeSetup(t, s, now, 0, tc.next)
			if tc.prep != nil {
				tc.prep(t, s)
			}
			task := catchupTask("c1")
			if tc.nilTask {
				task = nil
			}
			out, reason, err := s.ConsumeCatchup(ctx, c, tc.fireAt, tc.threshold, task)
			if err != nil || out != tc.want {
				t.Fatalf("consume = %v %q %v, want %v", out, reason, err, tc.want)
			}
			if _, found, _ := s.GetTask(ctx, "c1"); found {
				t.Fatal("task created")
			}
			if left := rawCatchup(t, s, "v1"); (left != "") != (tc.want == CatchupKept) {
				t.Fatalf("marker after %v = %q", out, left)
			}
		})
	}
}

// A realignment to another offset keeps the row in the same slot; that is not
// "the scheduled backup already ran".
func TestConsumeCatchup_RealignedRowStillFires(t *testing.T) {
	s := open(t, Options{})
	next := NextFire("0 2 * * *", time.Now().Add(3*time.Hour)).Unix() + 60
	exit := time.Now().Unix()
	c := consumeSetup(t, s, exit, 0, next)
	if moved, err := s.MoveScheduleFire(ctx, "v1", next, next+600); err != nil || !moved {
		t.Fatalf("move: %v %v", moved, err)
	}
	if out, reason, err := s.ConsumeCatchup(ctx, c, exit, 7200, catchupTask("c1")); err != nil || out != CatchupFired {
		t.Fatalf("consume = %v %q %v, want fired", out, reason, err)
	}
}
