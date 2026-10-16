package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Maintenance catch-up backups. While the node is paused, due backup slots are
// skipped, and on exit every schedule moves to its next slot after now (no burst).
// A volume that missed a slot inside the window gets ONE catch-up backup after
// the exit, however many slots it missed. The schedule row never leaves its
// grid for this: the catch-up is a separate per-volume marker in control_meta
// (metaCatchupPrefix + volume) that fires its own task.
//
// The marker goes through two states:
//   - {"missed":true}: written when a slot is skipped while paused (an observed
//     miss). Ignored by the consumer.
//   - {"exit":..,"phase":..,"next":..}: written by the exit transaction for every
//     schedule that has a marker (either state) or whose slot came due during
//     the pause. Rewriting an older {exit} marker means repeated pause/exit yields
//     at most one catch-up per volume, judged against the LAST exit.
//
// The daemon scheduler consumes {exit} markers (ConsumeCatchup). Recording
// happens in the store alone, so `cs-agent maintenance off` (a separate process
// without the agent config) records catch-ups like the daemon does.
const metaCatchupPrefix = "catchup."

// catchupMissedFlag is the marker value of an observed miss inside a pause.
const catchupMissedFlag = `{"missed":true}`

type catchupMarker struct {
	Missed bool  `json:"missed,omitempty"`
	Exit   int64 `json:"exit"`
	Phase  int64 `json:"phase"`
	Next   int64 `json:"next"`
}

// Catchup is a pending catch-up backup recorded at a maintenance exit. Exit is
// when maintenance ended, Phase the schedule's spread offset at that time (in
// seconds after its cron instant) and Next the schedule's next_fire_at right after
// the exit.
type Catchup struct {
	Volume string
	Exit   int64
	Phase  int64
	Next   int64

	raw string // the stored value, compared on consume
}

// CatchupOutcome is what ConsumeCatchup did with a marker.
type CatchupOutcome int

const (
	// CatchupKept: nothing written. The marker changed since it was listed, or
	// the node is paused (the next exit rewrites the marker).
	CatchupKept CatchupOutcome = iota
	// CatchupDropped: the marker was deleted without creating a task.
	CatchupDropped
	// CatchupFired: the task was created and the marker deleted.
	CatchupFired
)

func catchupKey(volumeName string) string { return metaCatchupPrefix + volumeName }

// markCatchupMissedTx records an observed miss for a volume (skipDueBackupTx).
// An existing marker is kept: a {exit} marker from an earlier exit is rewritten
// by the next exit anyway.
func markCatchupMissedTx(ctx context.Context, tx *sql.Tx, volumeName string) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO control_meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO NOTHING
	`, catchupKey(volumeName), catchupMissedFlag); err != nil {
		return fmt.Errorf("store: mark catch-up %q: %w", volumeName, err)
	}
	return nil
}

// takeCatchupMarkersTx deletes every catch-up marker and returns the volumes
// that had one. Used by the exit transaction, which rewrites the markers of the
// schedules that qualify; markers of volumes without a schedule go away.
func takeCatchupMarkersTx(ctx context.Context, tx *sql.Tx) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT key FROM control_meta WHERE substr(key, 1, ?) = ?`,
		len(metaCatchupPrefix), metaCatchupPrefix)
	if err != nil {
		return nil, fmt.Errorf("store: list catch-up markers: %w", err)
	}
	had := map[string]bool{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scan catch-up marker: %w", err)
		}
		had[strings.TrimPrefix(key, metaCatchupPrefix)] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("store: iterate catch-up markers: %w", err)
	}
	rows.Close()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM control_meta WHERE substr(key, 1, ?) = ?`,
		len(metaCatchupPrefix), metaCatchupPrefix); err != nil {
		return nil, fmt.Errorf("store: clear catch-up markers: %w", err)
	}
	return had, nil
}

// putCatchupTx writes a volume's {exit} marker.
func putCatchupTx(ctx context.Context, tx *sql.Tx, volumeName string, exit, phase, next int64) error {
	b, err := json.Marshal(catchupMarker{Exit: exit, Phase: phase, Next: next})
	if err != nil {
		return fmt.Errorf("store: marshal catch-up %q: %w", volumeName, err)
	}
	return setMetaTx(ctx, tx, catchupKey(volumeName), string(b))
}

// ListCatchups returns the catch-ups recorded at a maintenance exit, ordered by
// volume name. Observed-miss flags (still inside a pause) are not returned, nor
// is a marker that does not parse (the next exit replaces it).
func (s *Store) ListCatchups(ctx context.Context) ([]Catchup, error) {
	rows, err := s.control.QueryContext(ctx,
		`SELECT key, value FROM control_meta WHERE substr(key, 1, ?) = ? ORDER BY key`,
		len(metaCatchupPrefix), metaCatchupPrefix)
	if err != nil {
		return nil, fmt.Errorf("store: list catch-ups: %w", err)
	}
	defer rows.Close()

	var out []Catchup
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, fmt.Errorf("store: scan catch-up: %w", err)
		}
		var mk catchupMarker
		if err := json.Unmarshal([]byte(value), &mk); err != nil || mk.Missed || mk.Exit == 0 {
			continue
		}
		out = append(out, Catchup{
			Volume: strings.TrimPrefix(key, metaCatchupPrefix),
			Exit:   mk.Exit,
			Phase:  mk.Phase,
			Next:   mk.Next,
			raw:    value,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate catch-ups: %w", err)
	}
	return out, nil
}

// ConsumeCatchup resolves a due catch-up in ONE control.db transaction, so a
// crash either leaves the marker to be retried or has created the task and
// deleted the marker together. The marker must still hold the value c was listed
// with (another exit may have rewritten it); otherwise, or while the node is
// paused, nothing is written (CatchupKept).
//
// A nil task drops the marker: the caller found the feature disabled or the
// volume no longer backed up. Otherwise the marker is dropped without a task
// when the volume's schedule is gone; when its post-exit slot has already passed
// (it fired, so that backup covers the window); when that slot is due within
// threshold seconds of the exit (threshold > 0); when fireAt is not before it
// (the regular slot comes first); or when a volume.backup for the volume is
// already pending. Else the task is created, with its changelog row, exactly as
// FireDueBackup creates a scheduled one. reason says why it was dropped or kept.
func (s *Store) ConsumeCatchup(ctx context.Context, c Catchup, fireAt, threshold int64, task *Task) (outcome CatchupOutcome, reason string, err error) {
	if c.Volume == "" || c.raw == "" {
		return CatchupKept, "", errors.New("store: ConsumeCatchup requires a listed catch-up")
	}
	var snapshot []byte
	if task != nil {
		if task.ID == "" || task.Name == "" || task.Node == "" || task.Volume != c.Volume {
			return CatchupKept, "", errors.New("store: ConsumeCatchup requires task id, name, node and the catch-up's volume")
		}
		t := *task
		now := time.Now().Unix()
		if t.Status == "" {
			t.Status = TaskPending
		}
		t.CreatedAt = now
		t.UpdatedAt = now
		task = &t
		if snapshot, err = json.Marshal(t); err != nil {
			return CatchupKept, "", fmt.Errorf("store: marshal task %q: %w", t.ID, err)
		}
	}
	key := catchupKey(c.Volume)
	err = s.withControlTx(ctx, func(tx *sql.Tx) error {
		outcome, reason = CatchupKept, ""
		var cur string
		switch err := tx.QueryRowContext(ctx,
			`SELECT value FROM control_meta WHERE key = ?`, key).Scan(&cur); {
		case errors.Is(err, sql.ErrNoRows):
			reason = "marker gone"
			return nil
		case err != nil:
			return fmt.Errorf("store: read catch-up %q: %w", c.Volume, err)
		}
		if cur != c.raw {
			reason = "marker changed"
			return nil
		}
		paused, err := isPausedTx(ctx, tx)
		if err != nil {
			return err
		}
		if paused {
			reason = "node paused"
			return nil
		}

		reason, err = catchupDropReasonTx(ctx, tx, c, fireAt, threshold, task)
		if err != nil {
			return err
		}
		if reason == "" {
			created, err := insertTaskTx(ctx, tx, *task, snapshot)
			if err != nil {
				return err
			}
			if !created {
				reason = "task id already exists"
			}
		}
		if err := deleteMetaTx(ctx, tx, key); err != nil {
			return err
		}
		if reason == "" {
			outcome = CatchupFired
		} else {
			outcome = CatchupDropped
		}
		return nil
	})
	if err != nil {
		return CatchupKept, "", err
	}
	return outcome, reason, nil
}

// catchupDropReasonTx is why ConsumeCatchup drops c without a task ("" = fire).
func catchupDropReasonTx(ctx context.Context, tx *sql.Tx, c Catchup, fireAt, threshold int64, task *Task) (string, error) {
	if task == nil {
		return "catch-up disabled or volume not backed up", nil
	}
	var expr string
	var next int64
	switch err := tx.QueryRowContext(ctx,
		`SELECT cron_expr, next_fire_at FROM schedules WHERE volume_name = ?`, c.Volume).Scan(&expr, &next); {
	case errors.Is(err, sql.ErrNoRows):
		return "no schedule", nil
	case err != nil:
		return "", fmt.Errorf("store: read schedule %q: %w", c.Volume, err)
	}
	if next > c.Next && !sameSlot(expr, next, c.Next) {
		return "schedule moved past the slot after maintenance (it ran, or its cron changed)", nil
	}
	if threshold > 0 && next-c.Exit <= threshold {
		return "next scheduled backup is due soon after maintenance", nil
	}
	if fireAt >= next {
		return "next scheduled backup comes first", nil
	}
	waiting, err := pendingBackupTx(ctx, tx, c.Volume)
	if err != nil {
		return "", err
	}
	if waiting {
		return "a backup is already pending", nil
	}
	return "", nil
}

// sameSlot reports whether next_fire_at values a and b stand for the same cron
// instant of expr (a realignment to another offset moves a row within its slot).
func sameSlot(expr string, a, b int64) bool {
	ba, okA := BaseInstant(expr, a)
	bb, okB := BaseInstant(expr, b)
	return okA && okB && ba == bb
}
