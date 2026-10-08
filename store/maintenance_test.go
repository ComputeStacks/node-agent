package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// maintEntries returns every node_maintenance changelog row, in seq order.
func maintEntries(t *testing.T, s *Store) []ChangelogEntry {
	t.Helper()
	all, _, err := s.ChangelogPage(ctx, 0, EntityNodeMaintenance, 1000, true)
	if err != nil {
		t.Fatalf("ChangelogPage: %v", err)
	}
	return all
}

// lastMaintPayload decodes the newest node_maintenance payload into a generic map.
func lastMaintPayload(t *testing.T, s *Store) map[string]any {
	t.Helper()
	es := maintEntries(t, s)
	if len(es) == 0 {
		t.Fatal("no node_maintenance entries")
	}
	var p map[string]any
	if err := json.Unmarshal(es[len(es)-1].Payload, &p); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	return p
}

func mustMaint(t *testing.T, s *Store) MaintenanceState {
	t.Helper()
	m, err := s.GetMaintenance(ctx)
	if err != nil {
		t.Fatalf("GetMaintenance: %v", err)
	}
	return m
}

func TestMaintenance_HoldsPerSource(t *testing.T) {
	s := open(t, Options{})
	if p, err := s.IsPaused(ctx); err != nil || p {
		t.Fatalf("fresh IsPaused = %v, %v", p, err)
	}

	m, err := s.PutControllerHold(ctx, "kernel upgrade", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Paused() || m.Controller == nil || m.Controller.Reason != "kernel upgrade" || m.Controller.Gen != 1 || m.Local != nil {
		t.Fatalf("after controller put: %+v", m)
	}
	if m.PausedSince == 0 || m.Seq == 0 {
		t.Fatalf("paused_since/seq not set: %+v", m)
	}

	m, _, err = s.PutLocalHold(ctx, "disk swap", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if m.Local == nil || m.Local.By != "alice" || m.Controller == nil {
		t.Fatalf("after local put: %+v", m)
	}

	// Clearing the local hold leaves the controller hold, so still paused.
	if m, err = s.ClearLocalHold(ctx); err != nil || m.Local != nil || !m.Paused() {
		t.Fatalf("clear local: %+v %v", m, err)
	}
	if m, err = s.ClearControllerHold(ctx, 2); err != nil || m.Paused() || m.PausedSince != 0 {
		t.Fatalf("clear controller: %+v %v", m, err)
	}
	if got := mustMaint(t, s); got.Paused() || got.PausedSince != 0 || got.ControllerGen != 2 {
		t.Fatalf("stored after clears: %+v", got)
	}
	// put ctrl, put local, clear local, clear ctrl = 4 entries.
	if n := len(maintEntries(t, s)); n != 4 {
		t.Fatalf("entries = %d, want 4", n)
	}

	// Clearing an absent hold appends nothing.
	if _, err := s.ClearLocalHold(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClearControllerHold(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if n := len(maintEntries(t, s)); n != 4 {
		t.Fatalf("entries after no-op clears = %d, want 4", n)
	}
}

func TestMaintenance_ClearAllHolds(t *testing.T) {
	s := open(t, Options{})
	if _, err := s.PutControllerHold(ctx, "r", 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PutLocalHold(ctx, "l", "bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClearAllHolds(ctx, 0); !errors.Is(err, ErrStaleGen) {
		t.Fatalf("stale ClearAllHolds err = %v", err)
	}
	m, err := s.ClearAllHolds(ctx, 1)
	if err != nil || m.Paused() || m.Controller != nil || m.Local != nil {
		t.Fatalf("ClearAllHolds: %+v %v", m, err)
	}
}

func TestMaintenance_GenRules(t *testing.T) {
	s := open(t, Options{})
	if _, err := s.PutControllerHold(ctx, "r", 5); err != nil {
		t.Fatal(err)
	}
	before := len(maintEntries(t, s))

	m, err := s.PutControllerHold(ctx, "other", 4)
	if !errors.Is(err, ErrStaleGen) {
		t.Fatalf("stale put err = %v", err)
	}
	if m.ControllerGen != 5 || m.Controller == nil || m.Controller.Reason != "r" {
		t.Fatalf("stale put should return current state: %+v", m)
	}
	if _, err := s.ClearControllerHold(ctx, 4); !errors.Is(err, ErrStaleGen) {
		t.Fatalf("stale clear err = %v", err)
	}
	if got := mustMaint(t, s); got.Controller == nil || got.Controller.Reason != "r" {
		t.Fatalf("stale ops must write nothing: %+v", got)
	}
	if n := len(maintEntries(t, s)); n != before {
		t.Fatalf("stale ops appended entries: %d -> %d", before, n)
	}

	// Equal gen is accepted (an idempotent retry).
	if _, err := s.PutControllerHold(ctx, "r", 5); err != nil {
		t.Fatalf("equal gen put: %v", err)
	}
	// Clear at gen 7; the gen survives the hold's absence.
	if _, err := s.ClearControllerHold(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if got := mustMaint(t, s); got.Controller != nil || got.ControllerGen != 7 {
		t.Fatalf("after clear: %+v", got)
	}
	if _, err := s.PutControllerHold(ctx, "r", 6); !errors.Is(err, ErrStaleGen) {
		t.Fatalf("put below kept gen err = %v", err)
	}
	if _, err := s.PutControllerHold(ctx, "r", -1); err == nil {
		t.Fatal("negative gen accepted")
	}
}

func TestMaintenance_IdempotentControllerPut(t *testing.T) {
	s := open(t, Options{})
	if _, err := s.PutControllerHold(ctx, "r", 1); err != nil {
		t.Fatal(err)
	}
	// Backdate since so "kept" is distinguishable from "re-stamped".
	if err := s.SetMeta(ctx, metaMaintController, `{"since":100,"reason":"r","gen":1}`); err != nil {
		t.Fatal(err)
	}
	seqBefore := mustMaint(t, s).Seq
	n := len(maintEntries(t, s))

	m, err := s.PutControllerHold(ctx, "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if m.Controller.Since != 100 || m.Seq != seqBefore {
		t.Fatalf("idempotent put: %+v", m)
	}
	if got := len(maintEntries(t, s)); got != n {
		t.Fatalf("idempotent put appended: %d -> %d", n, got)
	}

	// A reason change keeps since but appends.
	m, err = s.PutControllerHold(ctx, "new reason", 2)
	if err != nil {
		t.Fatal(err)
	}
	if m.Controller.Since != 100 || m.Controller.Reason != "new reason" || m.Seq <= seqBefore {
		t.Fatalf("reason change: %+v", m)
	}
	if got := len(maintEntries(t, s)); got != n+1 {
		t.Fatalf("reason change entries = %d, want %d", got, n+1)
	}
}

func TestMaintenance_PutLocalHoldAlwaysAppends(t *testing.T) {
	s := open(t, Options{})
	var last int64
	for i := 0; i < 3; i++ {
		m, seq, err := s.PutLocalHold(ctx, "same", "root")
		if err != nil {
			t.Fatal(err)
		}
		if seq <= last || m.Seq != seq {
			t.Fatalf("iteration %d: seq=%d last=%d state.Seq=%d", i, seq, last, m.Seq)
		}
		last = seq
	}
	if n := len(maintEntries(t, s)); n != 3 {
		t.Fatalf("entries = %d, want 3", n)
	}
	if v, _, _ := s.GetMeta(ctx, metaMaintSeq); v == "" {
		t.Fatal("maintenance.seq not stored")
	}
}

func TestMaintenance_PayloadShape(t *testing.T) {
	s := open(t, Options{})
	if _, err := s.PutControllerHold(ctx, "r", 3); err != nil {
		t.Fatal(err)
	}
	p := lastMaintPayload(t, s)
	if p["paused"] != true || p["controller"] == nil || p["local"] != nil || p["paused_since"] == nil {
		t.Fatalf("payload: %v", p)
	}
	if gen, ok := p["controller_gen"].(float64); !ok || gen != 3 {
		t.Fatalf("payload controller_gen = %v", p["controller_gen"])
	}
	if id, ok := p["instance_id"].(string); !ok || len(id) != 32 {
		t.Fatalf("payload instance_id = %v", p["instance_id"])
	}
	if _, ok := p["sample"]; !ok {
		t.Fatalf("payload has no sample key: %v", p)
	}
	if _, ok := p["skipped_backups"]; !ok {
		t.Fatalf("payload has no skipped_backups key: %v", p)
	}
	es := maintEntries(t, s)
	if e := es[len(es)-1]; e.EntityID != "node" || e.Op != "upsert" {
		t.Fatalf("entry: %+v", e)
	}

	if _, err := s.ClearControllerHold(ctx, 3); err != nil {
		t.Fatal(err)
	}
	p = lastMaintPayload(t, s)
	if p["paused"] != false || p["paused_since"] != nil || p["controller"] != nil {
		t.Fatalf("cleared payload: %v", p)
	}
	if gen, _ := p["controller_gen"].(float64); gen != 3 {
		t.Fatalf("cleared payload controller_gen = %v", p["controller_gen"])
	}
}

func TestMaintenance_CancelRestoresOnEntryOnly(t *testing.T) {
	s := open(t, Options{})
	for _, tk := range []Task{
		{ID: "r1", Name: "volume.restore", Node: "n"},
		{ID: "r2", Name: "volume.restore", Node: "n"},
		{ID: "b1", Name: "volume.backup", Node: "n"},
	} {
		if _, err := s.CreateTask(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}
	// r2 is already running: not touched.
	if c, err := s.ClaimTask(ctx, "r2"); err != nil || !c {
		t.Fatalf("claim r2: %v %v", c, err)
	}

	if _, err := s.PutControllerHold(ctx, "r", 1); err != nil {
		t.Fatal(err)
	}
	r1, _, _ := s.GetTask(ctx, "r1")
	if r1.Status != TaskCancelled || !strings.Contains(string(r1.Result), "node entered maintenance") {
		t.Fatalf("r1 = %+v", r1)
	}
	if r2, _, _ := s.GetTask(ctx, "r2"); r2.Status != TaskRunning {
		t.Fatalf("r2 = %q, want running", r2.Status)
	}
	if b1, _, _ := s.GetTask(ctx, "b1"); b1.Status != TaskPending {
		t.Fatalf("b1 = %q, want pending", b1.Status)
	}
	// The cancel was changelogged as a task snapshot.
	tasks, _, err := s.ChangelogPage(ctx, 0, "task", 100, false)
	if err != nil {
		t.Fatal(err)
	}
	var sawCancel bool
	for _, e := range tasks {
		if e.EntityID == "r1" && strings.Contains(string(e.Payload), `"cancelled"`) {
			sawCancel = true
		}
	}
	if !sawCancel {
		t.Fatal("no changelog snapshot for the cancelled restore")
	}

	// A pending restore that exists while ALREADY paused is not cancelled by a
	// further hold change (only the not-paused -> paused transition cancels).
	if err := s.withControlTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO tasks (id, name, node, status, created_at, updated_at) VALUES ('r3', 'volume.restore', 'n', 'pending', 1, 1)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PutLocalHold(ctx, "l", "root"); err != nil {
		t.Fatal(err)
	}
	if r3, _, _ := s.GetTask(ctx, "r3"); r3.Status != TaskPending {
		t.Fatalf("r3 = %q, want pending (no transition)", r3.Status)
	}
}

func TestMaintenance_CreateRestoreWhilePaused(t *testing.T) {
	s := open(t, Options{})
	if _, _, err := s.PutLocalHold(ctx, "l", "root"); err != nil {
		t.Fatal(err)
	}
	created, err := s.CreateTask(ctx, Task{ID: "r1", Name: "volume.restore", Node: "n"})
	if err != nil || !created {
		t.Fatalf("create: %v %v", created, err)
	}
	r1, _, _ := s.GetTask(ctx, "r1")
	if r1.Status != TaskCancelled || !strings.Contains(string(r1.Result), "node entered maintenance") {
		t.Fatalf("r1 = %+v", r1)
	}
	// Other task kinds are still queued while paused.
	if _, err := s.CreateTask(ctx, Task{ID: "b1", Name: "volume.backup", Node: "n"}); err != nil {
		t.Fatal(err)
	}
	if b1, _, _ := s.GetTask(ctx, "b1"); b1.Status != TaskPending {
		t.Fatalf("b1 = %q, want pending", b1.Status)
	}
	if n, err := s.CountPendingTasks(ctx); err != nil || n != 1 {
		t.Fatalf("CountPendingTasks = %d, %v", n, err)
	}
}

func TestMaintenance_ClaimRefusedWhilePaused(t *testing.T) {
	s := open(t, Options{})
	if _, err := s.CreateTask(ctx, Task{ID: "b1", Name: "volume.backup", Node: "n"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutControllerHold(ctx, "r", 1); err != nil {
		t.Fatal(err)
	}
	before := countTable(t, s, "changelog")
	if c, err := s.ClaimTask(ctx, "b1"); err != nil || c {
		t.Fatalf("claim while paused: %v %v", c, err)
	}
	if b1, _, _ := s.GetTask(ctx, "b1"); b1.Status != TaskPending {
		t.Fatalf("b1 = %q, want pending", b1.Status)
	}
	if got := countTable(t, s, "changelog"); got != before {
		t.Fatalf("refused claim wrote changelog: %d -> %d", before, got)
	}
	if _, err := s.ClearControllerHold(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if c, err := s.ClaimTask(ctx, "b1"); err != nil || !c {
		t.Fatalf("claim after clear: %v %v", c, err)
	}
}

func TestMaintenance_SchedulesAdvanceOnExit(t *testing.T) {
	s := open(t, Options{})
	now := time.Now().Unix()
	if err := s.PutSchedule(ctx, "v1", "* * * * *", now+3600); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSchedule(ctx, "bad", "not a cron", 42); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PutLocalHold(ctx, "l", "root"); err != nil {
		t.Fatal(err)
	}
	// A slot passes during the window.
	if _, err := s.control.ExecContext(ctx, `UPDATE schedules SET next_fire_at = ? WHERE volume_name = 'v1'`, now-600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClearLocalHold(ctx); err != nil {
		t.Fatal(err)
	}
	sc, _, _ := s.GetSchedule(ctx, "v1")
	if sc.NextFireAt <= now {
		t.Fatalf("v1 next_fire_at = %d, want > %d", sc.NextFireAt, now)
	}
	if bad, _, _ := s.GetSchedule(ctx, "bad"); bad.NextFireAt != 42 {
		t.Fatalf("unparseable schedule touched: %+v", bad)
	}
}

func TestMaintenance_SkipDueBackup(t *testing.T) {
	s := open(t, Options{})
	if err := s.PutSchedule(ctx, "v1", "0 2 * * *", 10); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PutLocalHold(ctx, "l", "root"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.SkipDueBackup(ctx, "v1", 500); err != nil {
			t.Fatal(err)
		}
	}
	if sc, _, _ := s.GetSchedule(ctx, "v1"); sc.NextFireAt != 500 {
		t.Fatalf("next_fire_at = %d", sc.NextFireAt)
	}
	if m := mustMaint(t, s); m.SkippedBackups != 2 {
		t.Fatalf("skipped = %d, want 2", m.SkippedBackups)
	}
	// Re-entry resets the counter.
	if _, err := s.ClearLocalHold(ctx); err != nil {
		t.Fatal(err)
	}
	if m, err := s.PutControllerHold(ctx, "r", 0); err != nil || m.SkippedBackups != 0 {
		t.Fatalf("re-entry skipped = %d, %v", m.SkippedBackups, err)
	}
}

// FireDueBackup re-checks the pause inside its tx: a hold placed after the
// scheduler's own check still stops the task.
func TestMaintenance_FireDueBackupWhilePaused(t *testing.T) {
	s := open(t, Options{})
	if err := s.PutSchedule(ctx, "v1", "0 2 * * *", 10); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PutLocalHold(ctx, "l", "root"); err != nil {
		t.Fatal(err)
	}
	created, err := s.FireDueBackup(ctx, Task{
		ID: "auto-1", Name: "volume.backup", Node: "node-a", Volume: "v1", ProjectID: "proj-1",
	}, 500)
	if err != nil || created {
		t.Fatalf("FireDueBackup while paused: created=%v err=%v", created, err)
	}
	if _, found, _ := s.GetTask(ctx, "auto-1"); found {
		t.Fatal("task created while paused")
	}
	if sc, _, _ := s.GetSchedule(ctx, "v1"); sc.NextFireAt != 500 {
		t.Fatalf("next_fire_at = %d, want 500", sc.NextFireAt)
	}
	if m := mustMaint(t, s); m.SkippedBackups != 1 {
		t.Fatalf("skipped = %d, want 1", m.SkippedBackups)
	}
}

func TestChangelogPage_MaintenanceVisibility(t *testing.T) {
	s := open(t, Options{})
	if _, err := s.CreateActionRequest(ctx, "a-1", "proj-1", "cdn_purge", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PutLocalHold(ctx, "l", "root"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PutLocalHold(ctx, "l", "root"); err != nil {
		t.Fatal(err)
	}
	// seqs: 1 action_request, 2-3 node_maintenance.

	hidden, hw, err := s.ChangelogPage(ctx, 0, "", 100, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hidden) != 1 || hidden[0].EntityType != "action_request" || hw != 3 {
		t.Fatalf("hidden page: %d entries, hw=%d", len(hidden), hw)
	}
	shown, hw, err := s.ChangelogPage(ctx, 0, "", 100, true)
	if err != nil || len(shown) != 3 || hw != 3 {
		t.Fatalf("shown page: %d entries, hw=%d err=%v", len(shown), hw, err)
	}
	// A page whose scan is all hidden still advances the high water.
	empty, hw, err := s.ChangelogPage(ctx, 1, "", 100, false)
	if err != nil || len(empty) != 0 || hw != 3 {
		t.Fatalf("hidden tail: %d entries, hw=%d err=%v", len(empty), hw, err)
	}
	// An advertising scan is bounded by limit: high water never passes an
	// unscanned row.
	_, hw, err = s.ChangelogPage(ctx, 0, "", 2, true)
	if err != nil || hw != 2 {
		t.Fatalf("limit bound: hw=%d err=%v", hw, err)
	}
	// A hiding scan stops right after the row that fills the page.
	if got, hw, err := s.ChangelogPage(ctx, 0, "", 1, false); err != nil || len(got) != 1 || hw != 1 {
		t.Fatalf("full hiding page: %d entries, hw=%d err=%v", len(got), hw, err)
	}
	// Nothing scanned: high water is since.
	if _, hw, _ := s.ChangelogPage(ctx, 9, "", 100, false); hw != 9 {
		t.Fatalf("empty scan hw = %d, want 9", hw)
	}
	// ChangelogSince hides node_maintenance.
	if got := mustSince(t, s, 0, "", 100); len(got) != 1 {
		t.Fatalf("ChangelogSince = %d entries, want 1", len(got))
	}
}

// appendHidden appends n node_maintenance changelog rows directly.
func appendHidden(t *testing.T, s *Store, n int) {
	t.Helper()
	err := s.withControlTx(ctx, func(tx *sql.Tx) error {
		for i := 0; i < n; i++ {
			if err := appendChangelogTx(ctx, tx, EntityNodeMaintenance, "node", "", "upsert", []byte(`{}`), 1); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A reader that does not advertise node_maintenance support must not be handed
// an empty page just because hidden rows filled the first limit rows: the scan
// continues to the next visible row.
func TestChangelogPage_HiddenRunSkipsToVisible(t *testing.T) {
	s := open(t, Options{})
	appendHidden(t, s, 250)
	if _, err := s.CreateActionRequest(ctx, "a-1", "proj-1", "cdn_purge", nil); err != nil {
		t.Fatal(err)
	}
	got, hw, err := s.ChangelogPage(ctx, 0, "", 100, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].EntityType != "action_request" || got[0].Seq != 251 || hw != 251 {
		t.Fatalf("page: %d entries %+v, hw=%d", len(got), got, hw)
	}
	// ChangelogSince (which ignores the high water) also reaches it.
	if since := mustSince(t, s, 0, "", 100); len(since) != 1 || since[0].Seq != 251 {
		t.Fatalf("ChangelogSince = %+v", since)
	}
	// An advertising scan is unchanged: bounded by limit.
	shown, hw, err := s.ChangelogPage(ctx, 0, "", 100, true)
	if err != nil || len(shown) != 100 || hw != 100 {
		t.Fatalf("advertising page: %d entries, hw=%d err=%v", len(shown), hw, err)
	}
}

// The hiding scan is capped at changelogHiddenScanFactor*limit rows.
func TestChangelogPage_HiddenScanCap(t *testing.T) {
	s := open(t, Options{})
	const limit = 10
	appendHidden(t, s, changelogHiddenScanFactor*limit+5)
	if _, err := s.CreateActionRequest(ctx, "a-1", "proj-1", "cdn_purge", nil); err != nil {
		t.Fatal(err)
	}
	got, hw, err := s.ChangelogPage(ctx, 0, "", limit, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 || hw != changelogHiddenScanFactor*limit {
		t.Fatalf("capped page: %d entries, hw=%d, want 0 and %d", len(got), hw, changelogHiddenScanFactor*limit)
	}
	// Resuming from the high water reaches the visible row.
	got, hw, err = s.ChangelogPage(ctx, hw, "", limit, false)
	if want := int64(changelogHiddenScanFactor*limit + 6); err != nil || len(got) != 1 || got[0].Seq != want || hw != want {
		t.Fatalf("resumed page: %+v, hw=%d err=%v", got, hw, err)
	}
}

func TestRecordAdvertisedServe_Monotonic(t *testing.T) {
	s := open(t, Options{})
	if hw, at, err := s.GetAdvertised(ctx); err != nil || hw != 0 || at != 0 {
		t.Fatalf("fresh: %d %d %v", hw, at, err)
	}
	if err := s.RecordAdvertisedServe(ctx, 10, 1000); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordAdvertisedServe(ctx, 7, 2000); err != nil {
		t.Fatal(err)
	}
	hw, at, err := s.GetAdvertised(ctx)
	if err != nil || hw != 10 || at != 2000 {
		t.Fatalf("after rewind: hw=%d at=%d err=%v", hw, at, err)
	}
	if err := s.RecordAdvertisedServe(ctx, 12, 3000); err != nil {
		t.Fatal(err)
	}
	if hw, at, _ := s.GetAdvertised(ctx); hw != 12 || at != 3000 {
		t.Fatalf("after advance: hw=%d at=%d", hw, at)
	}
}

func TestMaintJobMarkers(t *testing.T) {
	s := open(t, Options{})
	if ok, err := s.BeginMaintJob(ctx, "prune"); err != nil || !ok {
		t.Fatalf("begin prune: %v %v", ok, err)
	}
	if ok, err := s.BeginMaintJob(ctx, "prune"); err != nil || ok {
		t.Fatalf("second begin prune: %v %v", ok, err)
	}
	jobs, err := s.ListMaintJobs(ctx)
	if err != nil || len(jobs) != 1 || jobs["prune"] == 0 {
		t.Fatalf("ListMaintJobs = %v, %v", jobs, err)
	}
	// Markers are not maintenance.* state and must not perturb it.
	if m := mustMaint(t, s); m.Paused() {
		t.Fatal("marker made the node paused")
	}

	if _, _, err := s.PutLocalHold(ctx, "l", "root"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.BeginMaintJob(ctx, "compact"); err != nil || ok {
		t.Fatalf("begin while paused: %v %v", ok, err)
	}
	if err := s.EndMaintJob(ctx, "prune"); err != nil {
		t.Fatal(err)
	}
	if jobs, _ := s.ListMaintJobs(ctx); len(jobs) != 0 {
		t.Fatalf("after end: %v", jobs)
	}
	if _, err := s.ClearLocalHold(ctx); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"prune", "compact"} {
		if ok, err := s.BeginMaintJob(ctx, n); err != nil || !ok {
			t.Fatalf("begin %s: %v %v", n, ok, err)
		}
	}
	if err := s.ClearMaintJobMarkers(ctx); err != nil {
		t.Fatal(err)
	}
	if jobs, _ := s.ListMaintJobs(ctx); len(jobs) != 0 {
		t.Fatalf("after clear: %v", jobs)
	}
}

// setSampleEmitInterval overrides the sample rate limit for one test.
func setSampleEmitInterval(t *testing.T, d time.Duration) {
	t.Helper()
	orig := MaintenanceSampleEmitInterval
	MaintenanceSampleEmitInterval = d
	t.Cleanup(func() { MaintenanceSampleEmitInterval = orig })
}

func TestRecordMaintenanceSample(t *testing.T) {
	setSampleEmitInterval(t, 0)
	s := open(t, Options{})
	busy := MaintenanceSample{Quiesce: "busy", RunningCount: 2, Pending: 1, SampledAt: 100}

	// Not paused: stored silently.
	if app, err := s.RecordMaintenanceSample(ctx, busy); err != nil || app {
		t.Fatalf("unpaused sample: %v %v", app, err)
	}
	if m := mustMaint(t, s); m.Sample == nil || m.Sample.RunningCount != 2 {
		t.Fatalf("unpaused sample not stored: %+v", m.Sample)
	}
	if n := len(maintEntries(t, s)); n != 0 {
		t.Fatalf("unpaused sample appended %d entries", n)
	}

	// Entry clears the stored sample, so the first sample after entry emits even
	// though it matches the one stored before.
	if _, _, err := s.PutLocalHold(ctx, "l", "root"); err != nil {
		t.Fatal(err)
	}
	if m := mustMaint(t, s); m.Sample != nil {
		t.Fatalf("sample not cleared on entry: %+v", m.Sample)
	}
	if p := lastMaintPayload(t, s); p["sample"] != nil {
		t.Fatalf("entry payload sample = %v, want null", p["sample"])
	}
	n := len(maintEntries(t, s))
	if app, err := s.RecordMaintenanceSample(ctx, busy); err != nil || !app {
		t.Fatalf("first sample after entry: %v %v", app, err)
	}
	if got := len(maintEntries(t, s)); got != n+1 {
		t.Fatalf("entries = %d, want %d", got, n+1)
	}
	m := mustMaint(t, s)
	if es := maintEntries(t, s); m.Seq != es[len(es)-1].Seq {
		t.Fatalf("maintenance.seq = %d, want %d", m.Seq, es[len(es)-1].Seq)
	}
	p := lastMaintPayload(t, s)
	if smp, ok := p["sample"].(map[string]any); !ok || smp["quiesce"] != "busy" || smp["running_count"] != float64(2) {
		t.Fatalf("payload sample = %v", p["sample"])
	}

	// Unchanged counts: only sampled_at is refreshed, no entry.
	again := busy
	again.SampledAt = 200
	if app, err := s.RecordMaintenanceSample(ctx, again); err != nil || app {
		t.Fatalf("unchanged sample: %v %v", app, err)
	}
	if got := len(maintEntries(t, s)); got != n+1 {
		t.Fatalf("unchanged sample appended: %d", got)
	}
	if m := mustMaint(t, s); m.Sample.SampledAt != 200 {
		t.Fatalf("sampled_at = %d, want 200", m.Sample.SampledAt)
	}

	// A change emits.
	quiet := MaintenanceSample{Quiesce: "quiesced", SampledAt: 300}
	if app, err := s.RecordMaintenanceSample(ctx, quiet); err != nil || !app {
		t.Fatalf("changed sample: %v %v", app, err)
	}
	if got := len(maintEntries(t, s)); got != n+2 {
		t.Fatalf("entries = %d, want %d", got, n+2)
	}
}

// A flapping sample emits at most one entry per interval, and the state it
// settles on is still emitted once the interval has passed.
func TestRecordMaintenanceSample_RateLimited(t *testing.T) {
	const window = time.Second
	setSampleEmitInterval(t, window)
	s := open(t, Options{})
	if _, _, err := s.PutLocalHold(ctx, "l", "root"); err != nil {
		t.Fatal(err)
	}
	busy := MaintenanceSample{Quiesce: "busy", RunningCount: 1, SampledAt: 100}
	quiet := MaintenanceSample{Quiesce: "quiesced", SampledAt: 101}

	start := time.Now()
	if app, err := s.RecordMaintenanceSample(ctx, busy); err != nil || !app {
		t.Fatalf("first sample after entry: %v %v", app, err)
	}
	n := len(maintEntries(t, s))
	for i := 0; i < 10; i++ {
		smp := quiet
		if i%2 == 1 {
			smp = busy
		}
		smp.SampledAt = int64(200 + i)
		app, err := s.RecordMaintenanceSample(ctx, smp)
		if err != nil {
			t.Fatal(err)
		}
		if app {
			t.Fatalf("flap %d emitted inside the window", i)
		}
	}
	if time.Since(start) >= window {
		t.Skip("flapping took longer than the window; timing assertions do not hold")
	}
	if got := len(maintEntries(t, s)); got != n {
		t.Fatalf("entries = %d, want %d", got, n)
	}
	// A held-back sample does not replace the published one; sampled_at moves only
	// for a sample that matches it (the last flap is busy again).
	if m := mustMaint(t, s); m.Sample == nil || m.Sample.Quiesce != "busy" || m.Sample.SampledAt != 209 {
		t.Fatalf("stored sample = %+v", m.Sample)
	}
	// The node settles while the limit still holds the change back.
	if app, err := s.RecordMaintenanceSample(ctx, quiet); err != nil || app {
		t.Fatalf("held-back settle: %v %v", app, err)
	}

	time.Sleep(window - time.Since(start) + 50*time.Millisecond)
	if app, err := s.RecordMaintenanceSample(ctx, quiet); err != nil || !app {
		t.Fatalf("settled sample after the window: %v %v", app, err)
	}
	if got := len(maintEntries(t, s)); got != n+1 {
		t.Fatalf("entries = %d, want %d", got, n+1)
	}
	if p := lastMaintPayload(t, s); p["sample"].(map[string]any)["quiesce"] != "quiesced" {
		t.Fatalf("payload sample = %v", p["sample"])
	}
	// The window restarts: an immediate change is held back again.
	if app, err := s.RecordMaintenanceSample(ctx, busy); err != nil || app {
		t.Fatalf("change right after an emit: %v %v", app, err)
	}

	// Hold changes are not rate limited, and re-entry clears the emit time so
	// the first sample after it emits at once.
	if _, err := s.ClearLocalHold(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PutLocalHold(ctx, "again", "root"); err != nil {
		t.Fatal(err)
	}
	if app, err := s.RecordMaintenanceSample(ctx, busy); err != nil || !app {
		t.Fatalf("first sample after re-entry: %v %v", app, err)
	}
}

func TestInstanceID(t *testing.T) {
	dir := t.TempDir()
	s1, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	id1, err := s1.InstanceID(ctx)
	if err != nil || len(id1) != 32 {
		t.Fatalf("instance id = %q, %v", id1, err)
	}
	if m := mustMaint(t, s1); m.InstanceID != id1 {
		t.Fatalf("GetMaintenance instance_id = %q, want %q", m.InstanceID, id1)
	}
	_ = s1.Close()

	// Stable across reopen.
	s2, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if id2, _ := s2.InstanceID(ctx); id2 != id1 {
		t.Fatalf("reopen instance id = %q, want %q", id2, id1)
	}
	// An existing DB without one (written by an older binary) gets one minted.
	if _, err := s2.control.ExecContext(ctx, `DELETE FROM control_meta WHERE key = ?`, MetaInstanceID); err != nil {
		t.Fatal(err)
	}
	_ = s2.Close()
	s3, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()
	id3, _ := s3.InstanceID(ctx)
	if len(id3) != 32 || id3 == id1 {
		t.Fatalf("minted instance id = %q (old %q)", id3, id1)
	}
}

func TestOpenExistingControl(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "absent")
		if _, err := OpenExistingControl(dir); err == nil {
			t.Fatal("opened a missing control.db")
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("data dir was created: %v", err)
		}
		// Existing dir, no DB: still refused, and no file created.
		dir2 := t.TempDir()
		if _, err := OpenExistingControl(dir2); err == nil {
			t.Fatal("opened a missing control.db")
		}
		if ents, _ := os.ReadDir(dir2); len(ents) != 0 {
			t.Fatalf("files created: %v", ents)
		}
	})

	t.Run("ok", func(t *testing.T) {
		dir := t.TempDir()
		s, err := Open(dir, Options{})
		if err != nil {
			t.Fatal(err)
		}
		id, _ := s.InstanceID(ctx)
		_ = s.Close()

		c, err := OpenExistingControl(dir)
		if err != nil {
			t.Fatalf("OpenExistingControl: %v", err)
		}
		if _, _, err := c.PutLocalHold(ctx, "l", "root"); err != nil {
			t.Fatalf("control write via CLI store: %v", err)
		}
		if m := mustMaint(t, c); m.InstanceID != id || !m.Paused() {
			t.Fatalf("state via CLI store: %+v", m)
		}
		if err := c.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "projects")); !os.IsNotExist(err) {
			t.Fatalf("projects dir created: %v", err)
		}
	})

	t.Run("newer schema", func(t *testing.T) {
		dir := t.TempDir()
		s, err := Open(dir, Options{})
		if err != nil {
			t.Fatal(err)
		}
		_ = s.Close()
		latest := controlMigrations[len(controlMigrations)-1].version
		writeAppliedVersion(t, filepath.Join(dir, "control.db"), latest+1)
		if _, err := OpenExistingControl(dir); err == nil {
			t.Fatal("opened a newer schema")
		}
	})

	t.Run("older schema", func(t *testing.T) {
		dir := t.TempDir()
		s, err := Open(dir, Options{})
		if err != nil {
			t.Fatal(err)
		}
		_ = s.Close()
		latest := controlMigrations[len(controlMigrations)-1].version
		db := openRaw(t, filepath.Join(dir, "control.db"))
		if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version = ?`, latest); err != nil {
			t.Fatal(err)
		}
		_ = db.Close()
		if _, err := OpenExistingControl(dir); err == nil {
			t.Fatal("opened an older schema")
		}
		// And it did not migrate it forward.
		if got := maxAppliedVersion(t, filepath.Join(dir, "control.db")); got != latest-1 {
			t.Fatalf("schema version = %d, want %d (no migration)", got, latest-1)
		}
	})
}

func TestNextFire(t *testing.T) {
	from := time.Date(2026, 1, 1, 1, 30, 0, 0, time.UTC)
	if got := NextFire("0 2 * * *", from); !got.Equal(time.Date(2026, 1, 1, 2, 0, 0, 0, time.UTC)) {
		t.Fatalf("NextFire = %v", got)
	}
	if got := NextFire("garbage", from); !got.IsZero() {
		t.Fatalf("unparseable NextFire = %v, want zero", got)
	}
}
