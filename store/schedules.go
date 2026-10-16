package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"sync"
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

// Backup schedule spreading. Every volume used to fire exactly on its cron
// instant, so every backup on every node started in the same minute. A schedule
// now fires at a fixed offset AFTER each cron instant (never earlier). The offset
// is a pure function of the cron expression and a key — the project, so all of a
// project's volumes share it and their archives stay close together — and is
// recomputed rather than stored: every node, restart and computation agrees.

// minIntervalFires is how many fires after minIntervalRef the shortest gap is
// measured over: enough for weekly and monthly expressions to show a real gap.
const minIntervalFires = 64

// minIntervalCacheMax bounds the memo; past it intervals are computed uncached.
const minIntervalCacheMax = 1024

var (
	minIntervalMu    sync.Mutex
	minIntervalCache = map[string]time.Duration{}
)

// minInterval is the shortest gap between consecutive fires of expr among the
// first minIntervalFires fires after a FIXED reference time, so the result never
// depends on the wall clock (no offset churn for irregular expressions). Zero for
// an unparseable expression or one with fewer than two fires. Memoized: the
// scheduler asks for it for every volume on every reconcile.
func minInterval(expr string) time.Duration {
	minIntervalMu.Lock()
	d, ok := minIntervalCache[expr]
	minIntervalMu.Unlock()
	if ok {
		return d
	}
	d = computeMinInterval(expr)
	minIntervalMu.Lock()
	if len(minIntervalCache) < minIntervalCacheMax {
		minIntervalCache[expr] = d
	}
	minIntervalMu.Unlock()
	return d
}

func computeMinInterval(expr string) time.Duration {
	sched, err := cron.ParseStandard(expr)
	if err != nil {
		return 0
	}
	var shortest time.Duration
	prev := sched.Next(time.Date(2024, 1, 1, 0, 0, 0, 0, time.Local))
	if prev.IsZero() {
		return 0
	}
	for i := 1; i < minIntervalFires; i++ {
		next := sched.Next(prev)
		if next.IsZero() {
			break
		}
		if gap := next.Sub(prev); gap > 0 && (shortest == 0 || gap < shortest) {
			shortest = gap
		}
		prev = next
	}
	return shortest
}

// spreadWindowSec is the largest offset window, in whole seconds, any cap can
// produce for expr: a quarter of its shortest interval, so a shifted slot always
// stays well clear of the next cron instant. Zero means no spreading.
func spreadWindowSec(expr string) int64 {
	return int64(minInterval(expr)/time.Second) / 4
}

// OffsetKey is the spread key of a volume's backup schedule: its project when it
// has one (all of a project's volumes share an offset, even across nodes), else
// the volume itself.
func OffsetKey(projectID int, volumeName string) string {
	if projectID > 0 {
		return fmt.Sprintf("project:%d", projectID)
	}
	return "volume:" + volumeName
}

// ScheduleOffset is the fixed delay after each cron instant at which a schedule
// keyed by key fires: hash(key) mod window, in whole seconds, where window is a
// quarter of expr's shortest interval capped at maxWindow. maxWindow <= 0 turns
// spreading off; an unparseable or rarely-firing expression gets 0.
func ScheduleOffset(expr, key string, maxWindow time.Duration) time.Duration {
	if maxWindow <= 0 {
		return 0
	}
	window := spreadWindowSec(expr)
	if c := int64(maxWindow / time.Second); c < window {
		window = c
	}
	if window <= 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	return time.Duration(h.Sum64()%uint64(window)) * time.Second
}

// NextFireOffset is the first shifted slot (cron instant + offset) strictly after
// from; zero when expr is unparseable or never fires.
func NextFireOffset(expr string, offset time.Duration, from time.Time) time.Time {
	next := NextFire(expr, from.Add(-offset))
	if next.IsZero() {
		return next
	}
	return next.Add(offset)
}

// BaseInstant returns the cron instant a next_fire_at of `at` belongs to: the
// latest instant of expr in [at-W, at], W being spreadWindowSec(expr) — the most
// any offset can delay a slot. ok=false when no instant lies there (the row is
// not on any offset grid of expr) or expr is unparseable.
func BaseInstant(expr string, at int64) (base int64, ok bool) {
	sched, err := cron.ParseStandard(expr)
	if err != nil {
		return 0, false
	}
	b := sched.Next(time.Unix(at-spreadWindowSec(expr)-1, 0))
	if b.IsZero() || b.Unix() > at {
		return 0, false
	}
	// Normally exactly one instant lies in the window; if a clock change packs
	// in more, the latest is the slot `at` stands for (earlier ones have fired).
	for i := 0; i < minIntervalFires; i++ {
		n := sched.Next(b)
		if n.IsZero() || n.Unix() > at {
			break
		}
		b = n
	}
	return b.Unix(), true
}

// SchedulePhase is how far `at` sits after its cron instant (see BaseInstant);
// 0 when it is on no grid of expr.
func SchedulePhase(expr string, at int64) int64 {
	if base, ok := BaseInstant(expr, at); ok {
		return at - base
	}
	return 0
}

// AlignNextFire moves an existing next_fire_at onto the grid of offset without
// skipping or repeating a slot. When existing belongs to a cron instant (it may
// already carry a different offset), the result is that instant + offset — which
// may be EARLIER than existing: that slot has not fired yet. Otherwise it is the
// first shifted slot at or after existing, but only if that slot is itself on the
// grid; else existing is returned unchanged, so an expression with no fixed grid
// (e.g. @every) is never pushed forward on every reconcile.
func AlignNextFire(expr string, offset time.Duration, existing int64) int64 {
	off := int64(offset / time.Second)
	if base, ok := BaseInstant(expr, existing); ok {
		return base + off
	}
	next := NextFireOffset(expr, offset, time.Unix(existing-1, 0))
	if next.IsZero() {
		return existing
	}
	if base, ok := BaseInstant(expr, next.Unix()); !ok || next.Unix()-base != off {
		return existing
	}
	return next.Unix()
}

func scanSchedule(row interface{ Scan(...any) error }) (Schedule, error) {
	var sc Schedule
	if err := row.Scan(&sc.VolumeName, &sc.CronExpr, &sc.NextFireAt, &sc.UpdatedAt); err != nil {
		return Schedule{}, err
	}
	return sc, nil
}

// PutSchedule upserts a volume's backup schedule. The scheduler's reconcile calls
// it when creating a new schedule or when the cron expression changed (nextFireAt
// is then the next shifted slot after now), and once to move an unchanged
// schedule onto its spread offset (AlignNextFire). It must NOT otherwise be called
// on an unchanged schedule — recomputing from now would push next_fire_at forward
// every reconcile and starve the backup. Not changelogged.
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
//
// If a volume.backup for the same volume is already pending, no task is created
// either (created=false): the schedule still advances, but the maintenance
// skipped-backups counter is not touched. Per-volume task exclusion makes a backup
// wait behind a running task on the same volume, so without this an hourly
// schedule behind a long restore would stack several backups that then run back to
// back; at most one scheduled backup now waits. A RUNNING backup does not suppress
// the insert — the new one queues behind it as usual.
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
		var waiting int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM tasks WHERE name = 'volume.backup' AND status = ? AND volume = ?`,
			TaskPending, task.Volume).Scan(&waiting); err != nil {
			return fmt.Errorf("store: check pending backup %q: %w", task.Volume, err)
		}
		if waiting == 0 {
			created, err = insertTaskTx(ctx, tx, task, snapshot)
			if err != nil {
				return err
			}
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
