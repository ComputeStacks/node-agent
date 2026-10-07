package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Task is a unit of work for this node — a backup/restore/delete/export/trash.
// It replaces the Consul jobs/<jid> envelope. The controller submits a task DOWN
// (POST /v1/admin/tasks) and the agent reports lifecycle UP via the changelog
// (entity_type "task"). name is one of volume.backup, volume.restore,
// backup.delete, backup.export, volume.trash. Result carries the terminal payload
// (export url/size/expiry/error; a backup's last_backup; failure output).
type Task struct {
	ID        string          `json:"id"`
	ProjectID string          `json:"project_id,omitempty"`
	Name      string          `json:"name"`
	Node      string          `json:"node"`
	Volume    string          `json:"volume,omitempty"`
	Archive   string          `json:"archive,omitempty"`
	AuditID   int64           `json:"audit_id,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Status    string          `json:"status"`
	Result    json.RawMessage `json:"result,omitempty"`
	CreatedAt int64           `json:"created_at"`
	UpdatedAt int64           `json:"updated_at"`
}

// Task status values.
const (
	TaskPending   = "pending"
	TaskRunning   = "running"
	TaskCompleted = "completed"
	TaskFailed    = "failed"
	TaskCancelled = "cancelled"
)

const taskColumns = `id, project_id, name, node, volume, archive, audit_id, params, status, result_json, created_at, updated_at`

// scanTask reads a task row from either *sql.Row or *sql.Rows.
func scanTask(row interface{ Scan(...any) error }) (Task, error) {
	var (
		t       Task
		projID  sql.NullString
		volume  sql.NullString
		archive sql.NullString
		auditID sql.NullInt64
		params  sql.NullString
		result  sql.NullString
	)
	if err := row.Scan(&t.ID, &projID, &t.Name, &t.Node, &volume, &archive, &auditID, &params, &t.Status, &result, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return Task{}, err
	}
	t.ProjectID = projID.String
	t.Volume = volume.String
	t.Archive = archive.String
	t.AuditID = auditID.Int64
	if params.Valid {
		t.Params = json.RawMessage(params.String)
	}
	if result.Valid {
		t.Result = json.RawMessage(result.String)
	}
	return t, nil
}

// getTaskTx reads a task within an existing transaction (for building the
// post-mutation changelog snapshot).
func getTaskTx(ctx context.Context, tx *sql.Tx, id string) (Task, error) {
	return scanTask(tx.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id = ?`, id))
}

// insertTaskTx inserts a task and, in the same transaction, appends its changelog
// row (entity_type "task", op "upsert"). It is idempotent on the task id
// (ON CONFLICT(id) DO NOTHING): a duplicate id inserts no row and appends no
// changelog entry, returning created=false. Callers must pre-set t.Status,
// t.CreatedAt, t.UpdatedAt and pass snapshot = json.Marshal(t). Shared by
// CreateTask (controller/DOWN) and FireDueBackup (scheduler).
func insertTaskTx(ctx context.Context, tx *sql.Tx, t Task, snapshot []byte) (bool, error) {
	res, err := tx.ExecContext(ctx, `
		INSERT INTO tasks (id, project_id, name, node, volume, archive, audit_id, params, status, result_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING
	`, t.ID, nullable(t.ProjectID), t.Name, t.Node, nullable(t.Volume), nullable(t.Archive), t.AuditID, nullableJSON(t.Params), t.Status, nullableJSON(t.Result), t.CreatedAt, t.UpdatedAt)
	if err != nil {
		return false, fmt.Errorf("store: insert task %q: %w", t.ID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: task %q rows affected: %w", t.ID, err)
	}
	if n == 0 {
		return false, nil // duplicate id: no row inserted -> no changelog append
	}
	if err := appendChangelogTx(ctx, tx, "task", t.ID, t.ProjectID, "upsert", snapshot, t.CreatedAt); err != nil {
		return false, err
	}
	return true, nil
}

// CreateTask inserts a task and, in the SAME control.db transaction, appends its
// changelog row. It is idempotent on the task id: a duplicate POST (same id) is a
// no-op — no second row, no second changelog entry, no re-dispatch — and returns
// created=false. This is what makes at-least-once controller delivery safe.
// Dispatch to the worker pool is the dispatcher's job, not this method's.
func (s *Store) CreateTask(ctx context.Context, t Task) (created bool, err error) {
	if t.ID == "" || t.Name == "" || t.Node == "" {
		return false, errors.New("store: CreateTask requires id, name, node")
	}
	now := time.Now().Unix()
	if t.Status == "" {
		t.Status = TaskPending
	}
	t.CreatedAt = now
	t.UpdatedAt = now

	err = s.withControlTx(ctx, func(tx *sql.Tx) error {
		// A restore that arrives while the node is paused is recorded as
		// cancelled rather than queued, matching the restores cancelled on entry.
		if t.Name == taskNameRestore && t.Status == TaskPending {
			paused, err := isPausedTx(ctx, tx)
			if err != nil {
				return err
			}
			if paused {
				t.Status = TaskCancelled
				t.Result = json.RawMessage(restoreCancelledResult)
			}
		}
		snapshot, err := json.Marshal(t)
		if err != nil {
			return fmt.Errorf("store: marshal task %q: %w", t.ID, err)
		}
		created, err = insertTaskTx(ctx, tx, t, snapshot)
		return err
	})
	if err != nil {
		return false, err // the insert/append rolled back: never report created on error
	}
	return created, nil
}

// UpdateTaskStatus transitions a task's status and, if result is non-nil,
// overwrites result_json (a nil result leaves the existing value). It re-reads
// the row inside the tx and appends the full updated snapshot to the changelog
// (op "upsert"). Updating an absent task returns sql.ErrNoRows.
func (s *Store) UpdateTaskStatus(ctx context.Context, id, status string, result json.RawMessage) error {
	if id == "" || status == "" {
		return errors.New("store: UpdateTaskStatus requires id and status")
	}
	now := time.Now().Unix()
	return s.withControlTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE tasks SET status = ?, result_json = COALESCE(?, result_json), updated_at = ?
			WHERE id = ?
		`, status, nullableJSON(result), now, id)
		if err != nil {
			return fmt.Errorf("store: update task %q: %w", id, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: task %q rows affected: %w", id, err)
		}
		if n == 0 {
			return fmt.Errorf("store: update task %q: %w", id, sql.ErrNoRows)
		}
		t, err := getTaskTx(ctx, tx, id)
		if err != nil {
			return fmt.Errorf("store: reload task %q: %w", id, err)
		}
		snapshot, err := json.Marshal(t)
		if err != nil {
			return fmt.Errorf("store: marshal task %q: %w", id, err)
		}
		return appendChangelogTx(ctx, tx, "task", t.ID, t.ProjectID, "upsert", snapshot, now)
	})
}

// ClaimTask atomically transitions a task pending -> running and appends the
// snapshot, only while it is still pending. Returns claimed=false if the task was
// not pending (already claimed/terminal/cancelled/absent). The dispatcher is the
// only caller; this CAS is what guarantees a task dispatches at most once even if
// the in-process wake signal and the backstop drain race on the same row.
//
// While the node is paused for maintenance it refuses (claimed=false, no write),
// checked inside the same tx as the CAS so a hold set concurrently is honored.
func (s *Store) ClaimTask(ctx context.Context, id string) (claimed bool, err error) {
	if id == "" {
		return false, errors.New("store: ClaimTask requires id")
	}
	now := time.Now().Unix()
	err = s.withControlTx(ctx, func(tx *sql.Tx) error {
		paused, err := isPausedTx(ctx, tx)
		if err != nil {
			return err
		}
		if paused {
			return nil
		}
		claimed, err = casTaskStatusTx(ctx, tx, id, TaskPending, TaskRunning, now)
		return err
	})
	if err != nil {
		return false, err
	}
	return claimed, nil
}

// UnclaimTask reverts a task running -> pending. It is used only when the
// dispatcher claimed a task (typically an export) but could not hand it to a
// worker (pool full): the row goes back to pending so a later wake retries it. The
// CAS on running means it can never stomp a task a worker has already moved
// terminal. A crash between claim and this revert leaves the task running, which
// the boot reconcile then fails (an export is never blindly re-run).
func (s *Store) UnclaimTask(ctx context.Context, id string) (unclaimed bool, err error) {
	unclaimed, err = s.casTaskStatus(ctx, id, TaskRunning, TaskPending)
	return unclaimed, err
}

// casTaskStatus flips a task from -> to only while it is currently in `from`,
// appending the resulting snapshot in the same tx. Returns false (no changelog
// append) when the row was not in `from` (or absent).
func (s *Store) casTaskStatus(ctx context.Context, id, from, to string) (bool, error) {
	if id == "" {
		return false, errors.New("store: casTaskStatus requires id")
	}
	now := time.Now().Unix()
	var changed bool
	err := s.withControlTx(ctx, func(tx *sql.Tx) error {
		var err error
		changed, err = casTaskStatusTx(ctx, tx, id, from, to, now)
		return err
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// casTaskStatusTx is the body of casTaskStatus inside an existing transaction.
func casTaskStatusTx(ctx context.Context, tx *sql.Tx, id, from, to string, now int64) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE tasks SET status = ?, updated_at = ? WHERE id = ? AND status = ?`,
		to, now, id, from)
	if err != nil {
		return false, fmt.Errorf("store: cas task %q %s->%s: %w", id, from, to, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: task %q rows affected: %w", id, err)
	}
	if n == 0 {
		return false, nil // not in `from`: no-op, not changelogged
	}
	t, err := getTaskTx(ctx, tx, id)
	if err != nil {
		return false, fmt.Errorf("store: reload task %q: %w", id, err)
	}
	snapshot, err := json.Marshal(t)
	if err != nil {
		return false, fmt.Errorf("store: marshal task %q: %w", id, err)
	}
	if err := appendChangelogTx(ctx, tx, "task", t.ID, t.ProjectID, "upsert", snapshot, now); err != nil {
		return false, err
	}
	return true, nil
}

// CancelPendingTask flips a task pending -> cancelled (and appends the snapshot)
// only while it is still pending; a task already dispatched/terminal is left
// untouched. Returns cancelled=false if the task was not pending (or absent).
func (s *Store) CancelPendingTask(ctx context.Context, id string) (cancelled bool, err error) {
	cancelled, err = s.casTaskStatus(ctx, id, TaskPending, TaskCancelled)
	return cancelled, err
}

// EnqueueTeardown idempotently enqueues a volume.trash teardown task keyed by the
// caller-supplied stable id ("volume.trash:<name>"). Plain CreateTask (ON CONFLICT
// DO NOTHING) would tombstone a prior terminal attempt, so this inspects the
// existing row:
//   - absent          -> insert a pending task (enqueued=true)
//   - pending/running -> no-op (a teardown is already in flight)
//   - completed       -> no-op (already torn down)
//   - failed          -> if resetFailed, reset to pending + re-dispatch (enqueued=true);
//                        otherwise no-op.
//
// resetFailed=true is for the controller-driven DELETE path (an explicit re-request
// retries a transiently-failed teardown). resetFailed=false is for the scheduler
// reconcile path: a lingering trash:true row must NOT re-run a failed teardown on
// every tick (that would be a tick-rate retry loop).
func (s *Store) EnqueueTeardown(ctx context.Context, t Task, resetFailed bool) (enqueued bool, err error) {
	if t.ID == "" || t.Name == "" || t.Node == "" {
		return false, errors.New("store: EnqueueTeardown requires id, name, node")
	}
	now := time.Now().Unix()
	err = s.withControlTx(ctx, func(tx *sql.Tx) error {
		existing, gErr := getTaskTx(ctx, tx, t.ID)
		switch {
		case errors.Is(gErr, sql.ErrNoRows):
			t.Status = TaskPending
			t.CreatedAt = now
			t.UpdatedAt = now
			snapshot, mErr := json.Marshal(t)
			if mErr != nil {
				return fmt.Errorf("store: marshal task %q: %w", t.ID, mErr)
			}
			enqueued, err = insertTaskTx(ctx, tx, t, snapshot)
			return err
		case gErr != nil:
			return fmt.Errorf("store: get task %q: %w", t.ID, gErr)
		}
		// A row exists: only a failed row with resetFailed is re-activated.
		if !(resetFailed && existing.Status == TaskFailed) {
			return nil
		}
		res, uErr := tx.ExecContext(ctx,
			`UPDATE tasks SET status = ?, result_json = NULL, updated_at = ? WHERE id = ? AND status = ?`,
			TaskPending, now, t.ID, TaskFailed)
		if uErr != nil {
			return fmt.Errorf("store: reset teardown %q: %w", t.ID, uErr)
		}
		n, aErr := res.RowsAffected()
		if aErr != nil {
			return fmt.Errorf("store: task %q rows affected: %w", t.ID, aErr)
		}
		if n == 0 {
			return nil // raced to non-failed: leave it
		}
		reloaded, rErr := getTaskTx(ctx, tx, t.ID)
		if rErr != nil {
			return fmt.Errorf("store: reload task %q: %w", t.ID, rErr)
		}
		snapshot, mErr := json.Marshal(reloaded)
		if mErr != nil {
			return fmt.Errorf("store: marshal task %q: %w", t.ID, mErr)
		}
		if cErr := appendChangelogTx(ctx, tx, "task", reloaded.ID, reloaded.ProjectID, "upsert", snapshot, now); cErr != nil {
			return cErr
		}
		enqueued = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return enqueued, nil
}

// GetTask returns a task by id. found=false on a miss (not an error).
func (s *Store) GetTask(ctx context.Context, id string) (Task, bool, error) {
	t, err := scanTask(s.control.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id = ?`, id))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Task{}, false, nil
	case err != nil:
		return Task{}, false, fmt.Errorf("store: get task %q: %w", id, err)
	default:
		return t, true, nil
	}
}

// CountPendingTasks returns how many of this node's tasks are pending.
func (s *Store) CountPendingTasks(ctx context.Context) (int, error) {
	var n int
	if err := s.control.QueryRowContext(ctx,
		`SELECT count(*) FROM tasks WHERE status = ?`, TaskPending).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count pending tasks: %w", err)
	}
	return n, nil
}

// ListPendingTasks returns this node's pending tasks in creation order. It is
// the dispatcher's boot/backstop drain source. There is no node filter: this
// node's control.db holds only this node's tasks (the controller writes each
// node's desired state to that node's endpoint), so the DB IS the node scope.
func (s *Store) ListPendingTasks(ctx context.Context) ([]Task, error) {
	return s.listTasksByStatus(ctx, TaskPending)
}

// ListRunningTasks returns this node's running tasks in creation order. It is the
// boot crash-reconcile source: a task left running across a restart is dead work
// and must be failed (never auto-replayed for destructive kinds).
func (s *Store) ListRunningTasks(ctx context.Context) ([]Task, error) {
	return s.listTasksByStatus(ctx, TaskRunning)
}

func (s *Store) listTasksByStatus(ctx context.Context, status string) ([]Task, error) {
	rows, err := s.control.QueryContext(ctx,
		`SELECT `+taskColumns+` FROM tasks WHERE status = ? ORDER BY created_at, id`,
		status)
	if err != nil {
		return nil, fmt.Errorf("store: list %s tasks: %w", status, err)
	}
	defer rows.Close()

	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan task row: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate tasks: %w", err)
	}
	return out, nil
}
