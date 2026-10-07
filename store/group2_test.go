package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
)

func TestControlMigrations_V3_Tables(t *testing.T) {
	s := open(t, Options{})
	for _, tbl := range []string{"tasks", "volumes", "firewall_rules", "repositories"} {
		if got := countTable(t, s, tbl); got != 0 {
			t.Fatalf("%s not empty on fresh open: %d", tbl, got)
		}
	}
	// control_meta holds only the instance id Open mints.
	var n int
	if err := s.control.QueryRowContext(ctx,
		`SELECT count(*) FROM control_meta WHERE key != ?`, MetaInstanceID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("control_meta not empty on fresh open: %d (%v)", n, err)
	}
}

func TestCreateTask_AppendsChangelog(t *testing.T) {
	s := open(t, Options{})
	created, err := s.CreateTask(ctx, Task{
		ID: "jid-1", ProjectID: "proj-1", Name: "volume.backup", Node: "node-a",
		Volume: "vol-1", Params: json.RawMessage(`{"download_ttl":3600}`),
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if !created {
		t.Fatal("created = false, want true")
	}
	if got := countTable(t, s, "tasks"); got != 1 {
		t.Fatalf("tasks = %d, want 1", got)
	}

	var entityType, entityID, projectID, op, blob, typ string
	if err := s.control.QueryRowContext(ctx,
		`SELECT entity_type, entity_id, project_id, op, payload, typeof(payload) FROM changelog`,
	).Scan(&entityType, &entityID, &projectID, &op, &blob, &typ); err != nil {
		t.Fatalf("changelog row: %v", err)
	}
	if entityType != "task" || entityID != "jid-1" || projectID != "proj-1" || op != "upsert" || typ != "text" {
		t.Fatalf("changelog = type %q id %q proj %q op %q typ %q", entityType, entityID, projectID, op, typ)
	}
	var snap Task
	if err := json.Unmarshal([]byte(blob), &snap); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if snap.Name != "volume.backup" || snap.Status != TaskPending {
		t.Fatalf("snapshot: %+v", snap)
	}
}

// TestCreateTask_IdempotentOnID proves the DOWN dispatch idempotency the plan
// relies on: a duplicate POST (same id) inserts no second row and — critically —
// appends no second changelog entry, so no re-dispatch. (Contrast
// action_requests, which errors on a duplicate id.)
func TestCreateTask_IdempotentOnID(t *testing.T) {
	s := open(t, Options{})
	if c, err := s.CreateTask(ctx, Task{ID: "dup", Name: "volume.backup", Node: "n"}); err != nil || !c {
		t.Fatalf("first create: created=%v err=%v", c, err)
	}
	c, err := s.CreateTask(ctx, Task{ID: "dup", Name: "volume.backup", Node: "n"})
	if err != nil {
		t.Fatalf("second create err: %v", err)
	}
	if c {
		t.Fatal("second create = true, want false (idempotent)")
	}
	if got := countTable(t, s, "tasks"); got != 1 {
		t.Fatalf("tasks = %d, want 1", got)
	}
	if got := countTable(t, s, "changelog"); got != 1 {
		t.Fatalf("changelog = %d, want 1 (no duplicate append)", got)
	}
}

func TestUpdateTaskStatus(t *testing.T) {
	s := open(t, Options{})
	if _, err := s.CreateTask(ctx, Task{ID: "t1", Name: "backup.export", Node: "n"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateTaskStatus(ctx, "t1", TaskRunning, nil); err != nil {
		t.Fatalf("update running: %v", err)
	}
	result := json.RawMessage(`{"url":"https://x","size":10}`)
	if err := s.UpdateTaskStatus(ctx, "t1", TaskCompleted, result); err != nil {
		t.Fatalf("update completed: %v", err)
	}
	got, found, err := s.GetTask(ctx, "t1")
	if err != nil || !found {
		t.Fatalf("GetTask: found=%v err=%v", found, err)
	}
	if got.Status != TaskCompleted {
		t.Fatalf("status = %q, want completed", got.Status)
	}
	if string(got.Result) != string(result) {
		t.Fatalf("result = %s", got.Result)
	}
	// create + 2 updates = 3 changelog rows.
	if got := countTable(t, s, "changelog"); got != 3 {
		t.Fatalf("changelog = %d, want 3", got)
	}
	if err := s.UpdateTaskStatus(ctx, "nope", TaskFailed, nil); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("update absent = %v, want ErrNoRows", err)
	}
}

func TestCancelPendingTask(t *testing.T) {
	s := open(t, Options{})
	if _, err := s.CreateTask(ctx, Task{ID: "c1", Name: "volume.backup", Node: "n"}); err != nil {
		t.Fatal(err)
	}
	cancelled, err := s.CancelPendingTask(ctx, "c1")
	if err != nil || !cancelled {
		t.Fatalf("cancel: cancelled=%v err=%v", cancelled, err)
	}
	got, _, _ := s.GetTask(ctx, "c1")
	if got.Status != TaskCancelled {
		t.Fatalf("status = %q, want cancelled", got.Status)
	}
	// Cancelling a non-pending task is a no-op and appends no changelog row.
	cancelled, err = s.CancelPendingTask(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if cancelled {
		t.Fatal("second cancel = true, want false")
	}
	if got := countTable(t, s, "changelog"); got != 2 {
		t.Fatalf("changelog = %d, want 2 (create + cancel)", got)
	}
	if c, err := s.CancelPendingTask(ctx, "nope"); err != nil || c {
		t.Fatalf("cancel absent: c=%v err=%v", c, err)
	}
}

func TestListPendingTasks(t *testing.T) {
	s := open(t, Options{})
	for _, id := range []string{"a1", "a2"} {
		if _, err := s.CreateTask(ctx, Task{ID: id, Name: "volume.backup", Node: "node-a"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CreateTask(ctx, Task{ID: "a3", Name: "volume.backup", Node: "node-a"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateTaskStatus(ctx, "a3", TaskRunning, nil); err != nil { // no longer pending
		t.Fatal(err)
	}
	// A task with a different node label is still returned: this node's control.db
	// is the node scope, so ListPendingTasks does not filter by node.
	if _, err := s.CreateTask(ctx, Task{ID: "b1", Name: "volume.backup", Node: "node-b"}); err != nil {
		t.Fatal(err)
	}
	pending, err := s.ListPendingTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 3 {
		t.Fatalf("pending = %d, want 3 (all pending regardless of node label)", len(pending))
	}
	for _, p := range pending {
		if p.Status != TaskPending {
			t.Fatalf("non-pending task returned: %+v", p)
		}
	}
}

func TestPutVolume_ChangelogAndSentinel(t *testing.T) {
	s := open(t, Options{})
	if pop, _ := s.IsPopulated(ctx, MetaVolumesPopulated); pop {
		t.Fatal("volumes reported populated before any PUT")
	}
	cfg := json.RawMessage(`{"name":"vol-1","node":"node-a","freq":"0 2 * * *","backup":true}`)
	if err := s.PutVolume(ctx, Volume{Name: "vol-1", ProjectID: "proj-1", Node: "node-a", Config: cfg}); err != nil {
		t.Fatalf("PutVolume: %v", err)
	}
	v, found, err := s.GetVolume(ctx, "vol-1")
	if err != nil || !found {
		t.Fatalf("GetVolume: found=%v err=%v", found, err)
	}
	if string(v.Config) != string(cfg) || v.Node != "node-a" || v.ProjectID != "proj-1" {
		t.Fatalf("volume: %+v", v)
	}
	if pop, _ := s.IsPopulated(ctx, MetaVolumesPopulated); !pop {
		t.Fatal("volumes-populated sentinel not latched after PUT")
	}

	var et, id, op string
	if err := s.control.QueryRowContext(ctx, `SELECT entity_type, entity_id, op FROM changelog`).Scan(&et, &id, &op); err != nil {
		t.Fatal(err)
	}
	if et != "volume" || id != "vol-1" || op != "upsert" {
		t.Fatalf("changelog = %s %s %s", et, id, op)
	}
	if vols, _ := s.ListVolumes(ctx); len(vols) != 1 {
		t.Fatalf("ListVolumes = %d, want 1", len(vols))
	}

	if err := s.DeleteVolume(ctx, "vol-1", "proj-1"); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}
	if got := countTable(t, s, "volumes"); got != 0 {
		t.Fatalf("volumes = %d, want 0", got)
	}
	if got := countTable(t, s, "changelog"); got != 2 { // upsert + delete
		t.Fatalf("changelog = %d, want 2", got)
	}
	if pop, _ := s.IsPopulated(ctx, MetaVolumesPopulated); !pop {
		t.Fatal("delete cleared the volumes-populated sentinel (should stay latched)")
	}
}

func TestPutFirewallRules_ChangelogAndSentinel(t *testing.T) {
	s := open(t, Options{})
	rules := json.RawMessage(`{"rules":[{"proto":"tcp","port":80}]}`)
	if err := s.PutFirewallRules(ctx, "node-a", rules); err != nil {
		t.Fatalf("PutFirewallRules: %v", err)
	}
	fr, found, err := s.GetFirewallRules(ctx)
	if err != nil || !found {
		t.Fatalf("GetFirewallRules: found=%v err=%v", found, err)
	}
	if string(fr.Rules) != string(rules) {
		t.Fatalf("rules = %s", fr.Rules)
	}
	if pop, _ := s.IsPopulated(ctx, MetaFirewallPopulated); !pop {
		t.Fatal("firewall-populated sentinel not latched")
	}
	var et, id, op string
	if err := s.control.QueryRowContext(ctx, `SELECT entity_type, entity_id, op FROM changelog`).Scan(&et, &id, &op); err != nil {
		t.Fatal(err)
	}
	if et != "firewall_rule" || id != "node-a" || op != "upsert" {
		t.Fatalf("changelog = %s %s %s", et, id, op)
	}
	// An explicit empty rule set is a legitimate "zero published ports" PUT.
	if err := s.PutFirewallRules(ctx, "node-a", json.RawMessage(`{"rules":[]}`)); err != nil {
		t.Fatalf("empty rules PUT: %v", err)
	}
	if err := s.DeleteFirewallRules(ctx); err != nil {
		t.Fatalf("DeleteFirewallRules: %v", err)
	}
	if got := countTable(t, s, "firewall_rules"); got != 0 {
		t.Fatalf("firewall_rules = %d, want 0", got)
	}
}

func TestUpsertRepository_Changelog(t *testing.T) {
	s := open(t, Options{})
	if err := s.UpsertRepository(ctx, Repository{Name: "vol-1", SizeOnDisk: 100, TotalSize: 200, Archives: []string{"a1", "a2"}}); err != nil {
		t.Fatalf("UpsertRepository: %v", err)
	}
	r, found, err := s.GetRepository(ctx, "vol-1")
	if err != nil || !found {
		t.Fatalf("GetRepository: found=%v err=%v", found, err)
	}
	if r.SizeOnDisk != 100 || r.TotalSize != 200 || len(r.Archives) != 2 {
		t.Fatalf("repo: %+v", r)
	}
	// Re-upsert updates in place.
	if err := s.UpsertRepository(ctx, Repository{Name: "vol-1", SizeOnDisk: 150, Archives: []string{"a1"}}); err != nil {
		t.Fatal(err)
	}
	r, _, _ = s.GetRepository(ctx, "vol-1")
	if r.SizeOnDisk != 150 || len(r.Archives) != 1 {
		t.Fatalf("after re-upsert: %+v", r)
	}
	if got := countTable(t, s, "repositories"); got != 1 {
		t.Fatalf("repositories = %d, want 1", got)
	}
	if got := countTable(t, s, "changelog"); got != 2 {
		t.Fatalf("changelog = %d, want 2", got)
	}
	var et string
	if err := s.control.QueryRowContext(ctx, `SELECT entity_type FROM changelog LIMIT 1`).Scan(&et); err != nil {
		t.Fatal(err)
	}
	if et != "repository" {
		t.Fatalf("entity_type = %q, want repository", et)
	}
}

func TestControlMeta_SetGetAndSentinel(t *testing.T) {
	s := open(t, Options{})
	if _, found, err := s.GetMeta(ctx, "k"); err != nil || found {
		t.Fatalf("get missing: found=%v err=%v", found, err)
	}
	if err := s.SetMeta(ctx, "k", "v1"); err != nil {
		t.Fatal(err)
	}
	if v, found, _ := s.GetMeta(ctx, "k"); !found || v != "v1" {
		t.Fatalf("get after set: %q found=%v", v, found)
	}
	if err := s.SetMeta(ctx, "k", "v2"); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := s.GetMeta(ctx, "k"); v != "v2" {
		t.Fatalf("get after re-set: %q", v)
	}
	if pop, _ := s.IsPopulated(ctx, MetaFirewallPopulated); pop {
		t.Fatal("IsPopulated true with no sentinel set")
	}
	// control_meta is consumer state, never changelogged.
	if got := countTable(t, s, "changelog"); got != 0 {
		t.Fatalf("changelog = %d, want 0 (control_meta must not changelog)", got)
	}
}

func TestUpdateTaskStatus_NilResultPreservesPrior(t *testing.T) {
	s := open(t, Options{})
	if _, err := s.CreateTask(ctx, Task{ID: "t1", Name: "backup.export", Node: "n"}); err != nil {
		t.Fatal(err)
	}
	result := json.RawMessage(`{"url":"https://x"}`)
	if err := s.UpdateTaskStatus(ctx, "t1", TaskCompleted, result); err != nil {
		t.Fatal(err)
	}
	// A later status change carrying a nil result must NOT wipe the stored result
	// (the COALESCE(?, result_json) branch).
	if err := s.UpdateTaskStatus(ctx, "t1", TaskFailed, nil); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.GetTask(ctx, "t1")
	if got.Status != TaskFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if string(got.Result) != string(result) {
		t.Fatalf("result = %q, want preserved %q", got.Result, result)
	}
}

func TestDelete_ChangelogShapeAndAbsentNoop(t *testing.T) {
	s := open(t, Options{})
	// Absent deletes are silent no-ops: no error, no changelog row.
	if err := s.DeleteVolume(ctx, "nope", "p"); err != nil {
		t.Fatalf("delete absent volume: %v", err)
	}
	if err := s.DeleteFirewallRules(ctx); err != nil {
		t.Fatalf("delete absent firewall: %v", err)
	}
	if got := countTable(t, s, "changelog"); got != 0 {
		t.Fatalf("changelog = %d after absent deletes, want 0", got)
	}

	// A real delete appends op="delete" with an absent (NULL) payload.
	if err := s.PutVolume(ctx, Volume{Name: "v1", Node: "n", Config: json.RawMessage(`{"node":"n"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteVolume(ctx, "v1", "p"); err != nil {
		t.Fatal(err)
	}
	var (
		op      string
		payload sql.NullString
	)
	if err := s.control.QueryRowContext(ctx,
		`SELECT op, payload FROM changelog WHERE op = 'delete'`).Scan(&op, &payload); err != nil {
		t.Fatalf("delete changelog row: %v", err)
	}
	if op != "delete" || payload.Valid {
		t.Fatalf("delete row: op=%q payloadValid=%v, want op=delete + NULL payload", op, payload.Valid)
	}
}
