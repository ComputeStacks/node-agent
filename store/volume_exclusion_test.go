package store

import (
	"encoding/json"
	"testing"
)

func TestFailPendingTask(t *testing.T) {
	s := open(t, Options{})
	if _, err := s.CreateTask(ctx, Task{ID: "r1", Name: "volume.restore", Node: "n", Volume: "v1"}); err != nil {
		t.Fatal(err)
	}
	before := countTable(t, s, "changelog")
	result := json.RawMessage(`{"error":"duplicate restore"}`)
	failed, err := s.FailPendingTask(ctx, "r1", result)
	if err != nil || !failed {
		t.Fatalf("fail pending: failed=%v err=%v", failed, err)
	}
	got, _, _ := s.GetTask(ctx, "r1")
	if got.Status != TaskFailed || string(got.Result) != string(result) {
		t.Fatalf("after fail: %+v", got)
	}
	if n := countTable(t, s, "changelog"); n != before+1 {
		t.Fatalf("changelog = %d, want %d", n, before+1)
	}

	// Already terminal (failed): no-op.
	if f, err := s.FailPendingTask(ctx, "r1", result); err != nil || f {
		t.Fatalf("fail failed: %v %v", f, err)
	}

	// Running and completed are left untouched.
	for _, id := range []string{"run", "done"} {
		if _, err := s.CreateTask(ctx, Task{ID: id, Name: "volume.restore", Node: "n", Volume: "v1"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ClaimTask(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.UpdateTaskStatus(ctx, "done", TaskCompleted, nil); err != nil {
		t.Fatal(err)
	}
	before = countTable(t, s, "changelog")
	for id, want := range map[string]string{"run": TaskRunning, "done": TaskCompleted} {
		if f, err := s.FailPendingTask(ctx, id, result); err != nil || f {
			t.Fatalf("fail %s: %v %v", id, f, err)
		}
		if got, _, _ := s.GetTask(ctx, id); got.Status != want || got.Result != nil {
			t.Fatalf("%s changed: %+v", id, got)
		}
	}
	if f, err := s.FailPendingTask(ctx, "absent", result); err != nil || f {
		t.Fatalf("fail absent: %v %v", f, err)
	}
	if n := countTable(t, s, "changelog"); n != before {
		t.Fatalf("no-op fails wrote changelog: %d -> %d", before, n)
	}

	if _, err := s.FailPendingTask(ctx, "", result); err == nil {
		t.Fatal("empty id: want error")
	}
}

func TestFailPendingTask_RefusedWhilePaused(t *testing.T) {
	s := open(t, Options{})
	if _, err := s.CreateTask(ctx, Task{ID: "b1", Name: "volume.backup", Node: "n", Volume: "v1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutControllerHold(ctx, "r", 1); err != nil {
		t.Fatal(err)
	}
	before := countTable(t, s, "changelog")
	if f, err := s.FailPendingTask(ctx, "b1", json.RawMessage(`{}`)); err != nil || f {
		t.Fatalf("fail while paused: %v %v", f, err)
	}
	if got, _, _ := s.GetTask(ctx, "b1"); got.Status != TaskPending || got.Result != nil {
		t.Fatalf("b1 changed while paused: %+v", got)
	}
	if n := countTable(t, s, "changelog"); n != before {
		t.Fatalf("refused fail wrote changelog: %d -> %d", before, n)
	}
}

// fireBackup fires a due backup for v1 with the given task id, advancing the
// schedule to next.
func fireBackup(t *testing.T, s *Store, id string, next int64) bool {
	t.Helper()
	created, err := s.FireDueBackup(ctx, Task{
		ID: id, Name: "volume.backup", Node: "node-a", Volume: "v1", ProjectID: "proj-1",
	}, next)
	if err != nil {
		t.Fatalf("FireDueBackup %s: %v", id, err)
	}
	return created
}

func TestFireDueBackup_PendingBackupSuppressesInsert(t *testing.T) {
	s := open(t, Options{})
	if err := s.PutSchedule(ctx, "v1", "0 * * * *", 100); err != nil {
		t.Fatal(err)
	}
	if !fireBackup(t, s, "auto-1", 200) {
		t.Fatal("first fire not created")
	}
	tasks := countTable(t, s, "tasks")
	changes := countTable(t, s, "changelog")
	if fireBackup(t, s, "auto-2", 300) {
		t.Fatal("second fire created while a backup is pending")
	}
	if _, found, _ := s.GetTask(ctx, "auto-2"); found {
		t.Fatal("auto-2 inserted")
	}
	if n := countTable(t, s, "tasks"); n != tasks {
		t.Fatalf("tasks = %d, want %d", n, tasks)
	}
	if n := countTable(t, s, "changelog"); n != changes {
		t.Fatalf("changelog = %d, want %d", n, changes)
	}
	if sc, _, _ := s.GetSchedule(ctx, "v1"); sc.NextFireAt != 300 {
		t.Fatalf("next_fire_at = %d, want 300", sc.NextFireAt)
	}
	if m := mustMaint(t, s); m.SkippedBackups != 0 {
		t.Fatalf("skipped = %d, want 0", m.SkippedBackups)
	}
}

func TestFireDueBackup_InsertsDespiteOtherTasks(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, s *Store)
	}{
		{"running backup same volume", func(t *testing.T, s *Store) {
			if _, err := s.CreateTask(ctx, Task{ID: "x", Name: "volume.backup", Node: "n", Volume: "v1"}); err != nil {
				t.Fatal(err)
			}
			if c, err := s.ClaimTask(ctx, "x"); err != nil || !c {
				t.Fatalf("claim: %v %v", c, err)
			}
		}},
		{"pending backup other volume", func(t *testing.T, s *Store) {
			if _, err := s.CreateTask(ctx, Task{ID: "x", Name: "volume.backup", Node: "n", Volume: "v2"}); err != nil {
				t.Fatal(err)
			}
		}},
		{"pending restore same volume", func(t *testing.T, s *Store) {
			if _, err := s.CreateTask(ctx, Task{ID: "x", Name: "volume.restore", Node: "n", Volume: "v1"}); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := open(t, Options{})
			if err := s.PutSchedule(ctx, "v1", "0 * * * *", 100); err != nil {
				t.Fatal(err)
			}
			tc.setup(t, s)
			if !fireBackup(t, s, "auto-1", 200) {
				t.Fatal("fire not created")
			}
			if tk, found, _ := s.GetTask(ctx, "auto-1"); !found || tk.Status != TaskPending {
				t.Fatalf("auto-1: found=%v %+v", found, tk)
			}
			if sc, _, _ := s.GetSchedule(ctx, "v1"); sc.NextFireAt != 200 {
				t.Fatalf("next_fire_at = %d, want 200", sc.NextFireAt)
			}
		})
	}
}
