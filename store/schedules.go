package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
)

// Schedule is a volume's durable backup schedule: the cron expression and the
// next unix-second time a volume.backup task is due. It replaces the in-RAM
// robfig runner and the Consul schedule mirror. Node-local scheduler state — it
// is NOT changelogged (the controller already holds the schedule as the volume's
// freq).
type Schedule struct {
	VolumeName string `json:"volume_name"`
	CronExpr   string `json:"cron_expr"`
	NextFireAt int64  `json:"next_fire_at"`
	UpdatedAt  int64  `json:"updated_at"`
}

// NextFire parses a standard 5-field cron expression (robfig, parser only) and
// returns the next fire time after `from`; a zero time signals an unparseable or
// never-firing expression.
func NextFire(expr string, from time.Time) time.Time {
	sched, err := cron.ParseStandard(expr)
	if err != nil {
		return time.Time{}
	}
	return sched.Next(from)
}

func scanSchedule(row interface{ Scan(...any) error }) (Schedule, error) {
	var sc Schedule
	if err := row.Scan(&sc.VolumeName, &sc.CronExpr, &sc.NextFireAt, &sc.UpdatedAt); err != nil {
		return Schedule{}, err
	}
	return sc, nil
}

// PutSchedule upserts a volume's backup schedule. The scheduler's reconcile calls
// it only when creating a new schedule or when the cron expression changed, so
// the caller-supplied nextFireAt (always a future time, computed as Next(now)) is
// authoritative. It must NOT be called on an unchanged schedule — that would push
// next_fire_at forward every reconcile and starve the backup. Not changelogged.
func (s *Store) PutSchedule(ctx context.Context, volumeName, cronExpr string, nextFireAt int64) error {
	if volumeName == "" || cronExpr == "" {
		return errors.New("store: PutSchedule requires volume_name and cron_expr")
	}
	now := time.Now().Unix()
	_, err := s.control.ExecContext(ctx, `
		INSERT INTO schedules (volume_name, cron_expr, next_fire_at, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(volume_name) DO UPDATE SET
			cron_expr    = excluded.cron_expr,
			next_fire_at = excluded.next_fire_at,
			updated_at   = excluded.updated_at
	`, volumeName, cronExpr, nextFireAt, now)
	if err != nil {
		return fmt.Errorf("store: put schedule %q: %w", volumeName, err)
	}
	return nil
}

// GetSchedule returns a volume's schedule. found=false on a miss (not an error).
func (s *Store) GetSchedule(ctx context.Context, volumeName string) (Schedule, bool, error) {
	sc, err := scanSchedule(s.control.QueryRowContext(ctx,
		`SELECT volume_name, cron_expr, next_fire_at, updated_at FROM schedules WHERE volume_name = ?`, volumeName))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Schedule{}, false, nil
	case err != nil:
		return Schedule{}, false, fmt.Errorf("store: get schedule %q: %w", volumeName, err)
	default:
		return sc, true, nil
	}
}

// DeleteSchedule removes a volume's schedule (backups disabled / volume trashed
// or gone). Deleting an absent schedule is a no-op (no error).
func (s *Store) DeleteSchedule(ctx context.Context, volumeName string) error {
	if _, err := s.control.ExecContext(ctx, `DELETE FROM schedules WHERE volume_name = ?`, volumeName); err != nil {
		return fmt.Errorf("store: delete schedule %q: %w", volumeName, err)
	}
	return nil
}

// ListSchedules returns every schedule (the scheduler's boot rebuild / reconcile
// source), ordered by volume name.
func (s *Store) ListSchedules(ctx context.Context) ([]Schedule, error) {
	return s.querySchedules(ctx,
		`SELECT volume_name, cron_expr, next_fire_at, updated_at FROM schedules ORDER BY volume_name`)
}

// ListDueSchedules returns schedules whose next_fire_at is at or before asOf
// (the tick loop's due set), oldest-due first.
func (s *Store) ListDueSchedules(ctx context.Context, asOf int64) ([]Schedule, error) {
	return s.querySchedules(ctx,
		`SELECT volume_name, cron_expr, next_fire_at, updated_at FROM schedules WHERE next_fire_at <= ? ORDER BY next_fire_at, volume_name`,
		asOf)
}

func (s *Store) querySchedules(ctx context.Context, query string, args ...any) ([]Schedule, error) {
	rows, err := s.control.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query schedules: %w", err)
	}
	defer rows.Close()

	var out []Schedule
	for rows.Next() {
		sc, err := scanSchedule(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan schedule row: %w", err)
		}
		out = append(out, sc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate schedules: %w", err)
	}
	return out, nil
}

// FireDueBackup fires a due backup schedule exactly once: in ONE control.db
// transaction it inserts the volume.backup task (idempotent on task.ID) with its
// changelog row AND advances the schedule's next_fire_at. Because both commit
// together, a crash cannot create the task without advancing (a double fire) or
// advance without creating (a missed fire) — the SQLite payoff over the old
// in-RAM-cron + separate Consul-KV job write. The caller builds `task` with a
// fresh unique ID and computes nextFireAt = Next(now). A vanished schedule row
// (volume removed mid-tick) just advances nothing; the task is still created.
// If the node is paused for maintenance when the tx runs, no task is created
// (created=false): the slot is skipped as SkipDueBackup does, advancing
// next_fire_at and counting the skip.
func (s *Store) FireDueBackup(ctx context.Context, task Task, nextFireAt int64) (created bool, err error) {
	if task.ID == "" || task.Name == "" || task.Node == "" || task.Volume == "" {
		return false, errors.New("store: FireDueBackup requires task id, name, node, volume")
	}
	now := time.Now().Unix()
	if task.Status == "" {
		task.Status = TaskPending
	}
	task.CreatedAt = now
	task.UpdatedAt = now
	snapshot, err := json.Marshal(task)
	if err != nil {
		return false, fmt.Errorf("store: marshal task %q: %w", task.ID, err)
	}
	err = s.withControlTx(ctx, func(tx *sql.Tx) error {
		paused, err := isPausedTx(ctx, tx)
		if err != nil {
			return err
		}
		if paused {
			return skipDueBackupTx(ctx, tx, task.Volume, nextFireAt, now)
		}
		created, err = insertTaskTx(ctx, tx, task, snapshot)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE schedules SET next_fire_at = ?, updated_at = ? WHERE volume_name = ?`,
			nextFireAt, now, task.Volume); err != nil {
			return fmt.Errorf("store: advance schedule %q: %w", task.Volume, err)
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return created, nil
}
