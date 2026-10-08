package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Node maintenance mode. The node carries up to two independent holds: one set
// by the controller over the admin API and one set locally by the CLI. The node
// is paused while ANY hold exists: no new work starts (task claims, backup slots,
// maintenance jobs) while work already running finishes. Each source sets and
// clears only its own hold; a controller override can clear both.
//
// All state lives in control_meta (no schema change). Every hold change appends
// one node_maintenance changelog row in the same tx, so the controller learns
// about a local hold through the replication spine it already polls.
const (
	metaMaintController    = "maintenance.controller"
	metaMaintLocal         = "maintenance.local"
	metaMaintControllerGen = "maintenance.controller_gen"
	metaMaintPausedSince   = "maintenance.paused_since"
	metaMaintSkipped       = "maintenance.skipped_backups"
	metaMaintSeq           = "maintenance.seq"
	metaMaintAdvHighWater  = "maintenance.advertised_high_water"
	metaMaintAdvAt         = "maintenance.advertised_at"
	metaMaintLastSample    = "maintenance.last_sample"
	// metaMaintSampleEmitted is when the last sample-driven entry was appended,
	// in unix nanoseconds (see RecordMaintenanceSample).
	metaMaintSampleEmitted = "maintenance.sample_emitted_at"

	// metaMaintJobPrefix + <job name> marks a node maintenance job (prune,
	// compact) as running; the value is {"started_at":<unix>}.
	metaMaintJobPrefix = "maintjob.running."

	// EntityNodeMaintenance is the changelog entity_type of a maintenance state
	// row (entity_id "node"). It is hidden from changelog readers that did not
	// advertise support for it (see ChangelogPage).
	EntityNodeMaintenance = "node_maintenance"

	taskNameRestore        = "volume.restore"
	restoreCancelledResult = `{"error":"cancelled: node entered maintenance before the restore started"}`
)

// MaintenanceSampleEmitInterval is the minimum gap between two sample-driven
// node_maintenance entries, so a flapping quiesce state cannot flood the
// changelog. A variable so tests can shorten it.
var MaintenanceSampleEmitInterval = 30 * time.Second

// ErrStaleGen is returned by the controller hold methods when the request's
// generation is older than the newest one already applied. Nothing is written.
var ErrStaleGen = errors.New("store: stale maintenance generation")

// MaintenanceHold is one source's hold. By is the unix user (local hold only);
// Gen is the controller generation that last wrote it (controller hold only).
type MaintenanceHold struct {
	Since  int64  `json:"since"`
	Reason string `json:"reason"`
	By     string `json:"by,omitempty"`
	Gen    int64  `json:"gen,omitempty"`
}

// MaintenanceSample is the last observed quiesce sample: whether anything is
// still running, how many things, and how many tasks are queued. While paused,
// a sample that differs from the stored one is published in a changelog entry,
// so the controller can follow the drain without polling the node.
type MaintenanceSample struct {
	Quiesce      string `json:"quiesce"`
	RunningCount int    `json:"running_count"`
	Pending      int    `json:"pending"`
	SampledAt    int64  `json:"sampled_at"`
}

// MaintenanceState is the node's full maintenance state as stored. Seq is the
// changelog seq of the latest node_maintenance entry (0 if none yet).
// InstanceID identifies this control.db (see MetaInstanceID).
type MaintenanceState struct {
	Controller     *MaintenanceHold   `json:"controller"`
	Local          *MaintenanceHold   `json:"local"`
	ControllerGen  int64              `json:"controller_gen"`
	PausedSince    int64              `json:"paused_since,omitempty"`
	SkippedBackups int64              `json:"skipped_backups"`
	Seq            int64              `json:"seq"`
	Sample         *MaintenanceSample `json:"sample"`
	InstanceID     string             `json:"instance_id"`
}

// Paused reports whether any hold exists.
func (m MaintenanceState) Paused() bool {
	return m.Controller != nil || m.Local != nil
}

// maintenancePayload is the node_maintenance changelog snapshot.
type maintenancePayload struct {
	Paused         bool               `json:"paused"`
	Controller     *MaintenanceHold   `json:"controller"`
	Local          *MaintenanceHold   `json:"local"`
	PausedSince    *int64             `json:"paused_since"`
	SkippedBackups int64              `json:"skipped_backups"`
	Sample         *MaintenanceSample `json:"sample"`
	InstanceID     string             `json:"instance_id"`
	ControllerGen  int64              `json:"controller_gen"`
}

// queryer is satisfied by both *sql.DB and *sql.Tx.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// readMaintenance loads the maintenance state in a single statement, so a read
// outside a tx still sees one consistent snapshot.
func readMaintenance(ctx context.Context, q queryer) (MaintenanceState, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT key, value FROM control_meta WHERE substr(key, 1, 12) = 'maintenance.' OR key = ?`,
		MetaInstanceID)
	if err != nil {
		return MaintenanceState{}, fmt.Errorf("store: read maintenance state: %w", err)
	}
	defer rows.Close()

	var m MaintenanceState
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return MaintenanceState{}, fmt.Errorf("store: scan maintenance state: %w", err)
		}
		switch key {
		case MetaInstanceID:
			m.InstanceID = value
		case metaMaintLastSample:
			var smp MaintenanceSample
			if err := json.Unmarshal([]byte(value), &smp); err != nil {
				return MaintenanceState{}, fmt.Errorf("store: parse %s: %w", key, err)
			}
			m.Sample = &smp
		case metaMaintController, metaMaintLocal:
			var h MaintenanceHold
			if err := json.Unmarshal([]byte(value), &h); err != nil {
				return MaintenanceState{}, fmt.Errorf("store: parse %s: %w", key, err)
			}
			if key == metaMaintController {
				m.Controller = &h
			} else {
				m.Local = &h
			}
		case metaMaintControllerGen, metaMaintPausedSince, metaMaintSkipped, metaMaintSeq:
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return MaintenanceState{}, fmt.Errorf("store: parse %s %q: %w", key, value, err)
			}
			switch key {
			case metaMaintControllerGen:
				m.ControllerGen = n
			case metaMaintPausedSince:
				m.PausedSince = n
			case metaMaintSkipped:
				m.SkippedBackups = n
			case metaMaintSeq:
				m.Seq = n
			}
		}
	}
	if err := rows.Err(); err != nil {
		return MaintenanceState{}, fmt.Errorf("store: iterate maintenance state: %w", err)
	}
	return m, nil
}

// GetMaintenance returns the node's maintenance state.
func (s *Store) GetMaintenance(ctx context.Context) (MaintenanceState, error) {
	return readMaintenance(ctx, s.control)
}

// IsPaused reports whether any maintenance hold exists.
func (s *Store) IsPaused(ctx context.Context) (bool, error) {
	m, err := readMaintenance(ctx, s.control)
	if err != nil {
		return false, err
	}
	return m.Paused(), nil
}

// isPausedTx is IsPaused inside an existing transaction, so a guard and the
// write it protects see the same state.
func isPausedTx(ctx context.Context, tx *sql.Tx) (bool, error) {
	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM control_meta WHERE key IN (?, ?)`,
		metaMaintController, metaMaintLocal).Scan(&n); err != nil {
		return false, fmt.Errorf("store: check maintenance paused: %w", err)
	}
	return n > 0, nil
}

func deleteMetaTx(ctx context.Context, tx *sql.Tx, key string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM control_meta WHERE key = ?`, key); err != nil {
		return fmt.Errorf("store: delete control_meta %q: %w", key, err)
	}
	return nil
}

// putHoldTx writes (or, for nil, deletes) one hold key.
func putHoldTx(ctx context.Context, tx *sql.Tx, key string, h *MaintenanceHold) error {
	if h == nil {
		return deleteMetaTx(ctx, tx, key)
	}
	b, err := json.Marshal(h)
	if err != nil {
		return fmt.Errorf("store: marshal %s: %w", key, err)
	}
	return setMetaTx(ctx, tx, key, string(b))
}

// mutateMaintenance runs one maintenance write in a single control.db tx. fn
// edits the loaded state in place and reports whether a hold changed; returning
// an error (e.g. ErrStaleGen) rolls everything back. The helper then persists
// the holds and controller gen, applies any paused transition, and appends a
// node_maintenance entry when a hold changed or alwaysAppend is set. It returns
// the new state and the seq of the appended entry (0 if none). On error the
// returned state is the state as loaded, so a caller can report it.
func (s *Store) mutateMaintenance(ctx context.Context, alwaysAppend bool, fn func(m *MaintenanceState, now int64) (bool, error)) (MaintenanceState, int64, error) {
	now := time.Now().Unix()
	var (
		out      MaintenanceState
		loaded   MaintenanceState
		entrySeq int64
	)
	err := s.withControlTx(ctx, func(tx *sql.Tx) error {
		before, err := readMaintenance(ctx, tx)
		if err != nil {
			return err
		}
		loaded = before
		m := before
		changed, err := fn(&m, now)
		if err != nil {
			return err
		}
		if err := putHoldTx(ctx, tx, metaMaintController, m.Controller); err != nil {
			return err
		}
		if err := putHoldTx(ctx, tx, metaMaintLocal, m.Local); err != nil {
			return err
		}
		if m.ControllerGen != before.ControllerGen {
			if err := setMetaTx(ctx, tx, metaMaintControllerGen, strconv.FormatInt(m.ControllerGen, 10)); err != nil {
				return err
			}
		}

		switch {
		case !before.Paused() && m.Paused():
			m.PausedSince = now
			m.SkippedBackups = 0
			m.Sample = nil // so the first sample after entry always emits
			if err := deleteMetaTx(ctx, tx, metaMaintLastSample); err != nil {
				return err
			}
			if err := deleteMetaTx(ctx, tx, metaMaintSampleEmitted); err != nil {
				return err
			}
			if err := setMetaTx(ctx, tx, metaMaintPausedSince, strconv.FormatInt(now, 10)); err != nil {
				return err
			}
			if err := setMetaTx(ctx, tx, metaMaintSkipped, "0"); err != nil {
				return err
			}
			if err := cancelPendingRestoresTx(ctx, tx, now); err != nil {
				return err
			}
		case before.Paused() && !m.Paused():
			m.PausedSince = 0
			if err := deleteMetaTx(ctx, tx, metaMaintPausedSince); err != nil {
				return err
			}
			if err := advanceSchedulesTx(ctx, tx, now); err != nil {
				return err
			}
		}

		if changed || alwaysAppend {
			entrySeq, err = appendMaintenanceTx(ctx, tx, m, now)
			if err != nil {
				return err
			}
			m.Seq = entrySeq
		}
		out = m
		return nil
	})
	if err != nil {
		return loaded, 0, err
	}
	return out, entrySeq, nil
}

// appendMaintenanceTx appends the node_maintenance snapshot of m and records its
// seq in maintenance.seq.
func appendMaintenanceTx(ctx context.Context, tx *sql.Tx, m MaintenanceState, now int64) (int64, error) {
	p := maintenancePayload{
		Paused:         m.Paused(),
		Controller:     m.Controller,
		Local:          m.Local,
		SkippedBackups: m.SkippedBackups,
		Sample:         m.Sample,
		InstanceID:     m.InstanceID,
		ControllerGen:  m.ControllerGen,
	}
	if m.PausedSince != 0 {
		ps := m.PausedSince
		p.PausedSince = &ps
	}
	b, err := json.Marshal(p)
	if err != nil {
		return 0, fmt.Errorf("store: marshal maintenance entry: %w", err)
	}
	seq, err := appendChangelogSeqTx(ctx, tx, EntityNodeMaintenance, "node", "", "upsert", b, now)
	if err != nil {
		return 0, err
	}
	if err := setMetaTx(ctx, tx, metaMaintSeq, strconv.FormatInt(seq, 10)); err != nil {
		return 0, err
	}
	return seq, nil
}

// cancelPendingRestoresTx moves every pending volume.restore to cancelled on
// entry into maintenance, appending each snapshot exactly as casTaskStatus does.
// A restore overwrites live data, so it must not start late, after an operator
// has finished the work the node was paused for.
func cancelPendingRestoresTx(ctx context.Context, tx *sql.Tx, now int64) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM tasks WHERE status = ? AND name = ? ORDER BY created_at, id`,
		TaskPending, taskNameRestore)
	if err != nil {
		return fmt.Errorf("store: list pending restores: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("store: scan pending restore: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("store: iterate pending restores: %w", err)
	}
	rows.Close()

	for _, id := range ids {
		res, err := tx.ExecContext(ctx,
			`UPDATE tasks SET status = ?, result_json = ?, updated_at = ? WHERE id = ? AND status = ?`,
			TaskCancelled, restoreCancelledResult, now, id, TaskPending)
		if err != nil {
			return fmt.Errorf("store: cancel restore %q: %w", id, err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("store: task %q rows affected: %w", id, err)
		} else if n == 0 {
			continue
		}
		t, err := getTaskTx(ctx, tx, id)
		if err != nil {
			return fmt.Errorf("store: reload task %q: %w", id, err)
		}
		snapshot, err := json.Marshal(t)
		if err != nil {
			return fmt.Errorf("store: marshal task %q: %w", id, err)
		}
		if err := appendChangelogTx(ctx, tx, "task", t.ID, t.ProjectID, "upsert", snapshot, now); err != nil {
			return err
		}
	}
	return nil
}

// advanceSchedulesTx recomputes every schedule's next_fire_at from now on exit
// from maintenance, so slots missed during the window are skipped rather than
// all firing at once. A row whose cron no longer parses is left as is.
func advanceSchedulesTx(ctx context.Context, tx *sql.Tx, now int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT volume_name, cron_expr FROM schedules`)
	if err != nil {
		return fmt.Errorf("store: list schedules: %w", err)
	}
	type sched struct{ vol, expr string }
	var all []sched
	for rows.Next() {
		var sc sched
		if err := rows.Scan(&sc.vol, &sc.expr); err != nil {
			rows.Close()
			return fmt.Errorf("store: scan schedule row: %w", err)
		}
		all = append(all, sc)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("store: iterate schedules: %w", err)
	}
	rows.Close()

	from := time.Unix(now, 0)
	for _, sc := range all {
		next := NextFire(sc.expr, from)
		if next.IsZero() {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE schedules SET next_fire_at = ?, updated_at = ? WHERE volume_name = ?`,
			next.Unix(), now, sc.vol); err != nil {
			return fmt.Errorf("store: advance schedule %q: %w", sc.vol, err)
		}
	}
	return nil
}

// checkGen applies the controller generation rule: an older gen is refused, an
// equal or newer one is recorded (the stored gen survives a cleared hold).
func checkGen(m *MaintenanceState, gen int64) error {
	if gen < 0 {
		return errors.New("store: maintenance generation must be >= 0")
	}
	if gen < m.ControllerGen {
		return ErrStaleGen
	}
	m.ControllerGen = gen
	return nil
}

// PutControllerHold sets or refreshes the controller hold. A gen older than the
// newest seen returns ErrStaleGen with nothing written. An existing hold keeps
// its original since (the reason is updated); an entry is appended only when the
// hold is new or its reason changed, so an idempotent retry writes no entry.
func (s *Store) PutControllerHold(ctx context.Context, reason string, gen int64) (MaintenanceState, error) {
	m, _, err := s.mutateMaintenance(ctx, false, func(m *MaintenanceState, now int64) (bool, error) {
		if err := checkGen(m, gen); err != nil {
			return false, err
		}
		if m.Controller == nil {
			m.Controller = &MaintenanceHold{Since: now, Reason: reason, Gen: gen}
			return true, nil
		}
		h := *m.Controller
		changed := h.Reason != reason
		h.Reason = reason
		h.Gen = gen
		m.Controller = &h
		return changed, nil
	})
	return m, err
}

// ClearControllerHold removes the controller hold (same gen rule as
// PutControllerHold). Clearing an absent hold records the gen and appends no
// entry.
func (s *Store) ClearControllerHold(ctx context.Context, gen int64) (MaintenanceState, error) {
	m, _, err := s.mutateMaintenance(ctx, false, func(m *MaintenanceState, _ int64) (bool, error) {
		if err := checkGen(m, gen); err != nil {
			return false, err
		}
		changed := m.Controller != nil
		m.Controller = nil
		return changed, nil
	})
	return m, err
}

// ClearAllHolds is the controller override: it removes both the controller and
// the local hold (same gen rule as PutControllerHold).
func (s *Store) ClearAllHolds(ctx context.Context, gen int64) (MaintenanceState, error) {
	m, _, err := s.mutateMaintenance(ctx, false, func(m *MaintenanceState, _ int64) (bool, error) {
		if err := checkGen(m, gen); err != nil {
			return false, err
		}
		changed := m.Controller != nil || m.Local != nil
		m.Controller = nil
		m.Local = nil
		return changed, nil
	})
	return m, err
}

// PutLocalHold sets or refreshes the local hold (an existing hold keeps its
// since; reason and by are updated). It ALWAYS appends a fresh entry and returns
// its seq, even when nothing changed: the CLI waits for the controller to have
// consumed an entry written after its own request.
func (s *Store) PutLocalHold(ctx context.Context, reason, by string) (MaintenanceState, int64, error) {
	return s.mutateMaintenance(ctx, true, func(m *MaintenanceState, now int64) (bool, error) {
		since := now
		if m.Local != nil {
			since = m.Local.Since
		}
		m.Local = &MaintenanceHold{Since: since, Reason: reason, By: by}
		return true, nil
	})
}

// ClearLocalHold removes the local hold; an entry is appended only if it existed.
func (s *Store) ClearLocalHold(ctx context.Context) (MaintenanceState, error) {
	m, _, err := s.mutateMaintenance(ctx, false, func(m *MaintenanceState, _ int64) (bool, error) {
		changed := m.Local != nil
		m.Local = nil
		return changed, nil
	})
	return m, err
}

// RecordMaintenanceSample stores the latest quiesce sample in one tx. While not
// paused it is stored silently. While paused, a sample whose quiesce, running
// count or pending count differs from the stored one is stored AND published
// as a fresh node_maintenance entry (appended=true); an unchanged one only
// refreshes the stored sampled_at.
//
// Sample-driven entries are rate limited to one per
// MaintenanceSampleEmitInterval (hold changes are not). A differing sample that
// the limit holds back is NOT stored (sampled_at is not refreshed either), so the
// stored sample stays the last one published and the first sample allowed
// after the interval still differs and is published. The first sample after
// entry into maintenance always publishes: entry clears the stored sample and
// the emit time together.
func (s *Store) RecordMaintenanceSample(ctx context.Context, smp MaintenanceSample) (appended bool, err error) {
	now := time.Now()
	err = s.withControlTx(ctx, func(tx *sql.Tx) error {
		m, err := readMaintenance(ctx, tx)
		if err != nil {
			return err
		}
		prev := m.Sample
		next := smp
		if m.Paused() {
			differs := prev == nil || prev.Quiesce != smp.Quiesce ||
				prev.RunningCount != smp.RunningCount || prev.Pending != smp.Pending
			if differs {
				allowed, err := sampleEmitAllowedTx(ctx, tx, now)
				if err != nil {
					return err
				}
				appended = allowed || prev == nil
			}
			if !appended {
				next = *prev
				// sampled_at says when the stored values were last observed, so it
				// moves only when they still hold; a held-back change leaves it.
				if !differs {
					next.SampledAt = smp.SampledAt
				}
			}
		}
		b, err := json.Marshal(next)
		if err != nil {
			return fmt.Errorf("store: marshal maintenance sample: %w", err)
		}
		if err := setMetaTx(ctx, tx, metaMaintLastSample, string(b)); err != nil {
			return err
		}
		if !appended {
			return nil
		}
		if err := setMetaTx(ctx, tx, metaMaintSampleEmitted, strconv.FormatInt(now.UnixNano(), 10)); err != nil {
			return err
		}
		m.Sample = &next
		_, err = appendMaintenanceTx(ctx, tx, m, now.Unix())
		return err
	})
	if err != nil {
		return false, err
	}
	return appended, nil
}

// sampleEmitAllowedTx reports whether the sample rate limit lets an entry be
// appended at now: no sample entry yet, the interval has passed, or the clock
// stepped back behind the last emit (so a clock change cannot block it).
func sampleEmitAllowedTx(ctx context.Context, tx *sql.Tx, now time.Time) (bool, error) {
	var v string
	switch err := tx.QueryRowContext(ctx,
		`SELECT value FROM control_meta WHERE key = ?`, metaMaintSampleEmitted).Scan(&v); {
	case errors.Is(err, sql.ErrNoRows):
		return true, nil
	case err != nil:
		return false, fmt.Errorf("store: read %s: %w", metaMaintSampleEmitted, err)
	}
	ns, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return false, fmt.Errorf("store: parse %s %q: %w", metaMaintSampleEmitted, v, err)
	}
	last := time.Unix(0, ns)
	return now.Before(last) || now.Sub(last) >= MaintenanceSampleEmitInterval, nil
}

// SkipDueBackup records a backup slot skipped because the node is paused: in one
// tx it advances the volume's next_fire_at and increments
// maintenance.skipped_backups. It appends no changelog entry.
func (s *Store) SkipDueBackup(ctx context.Context, volumeName string, nextFireAt int64) error {
	if volumeName == "" {
		return errors.New("store: SkipDueBackup requires volume_name")
	}
	now := time.Now().Unix()
	return s.withControlTx(ctx, func(tx *sql.Tx) error {
		return skipDueBackupTx(ctx, tx, volumeName, nextFireAt, now)
	})
}

// skipDueBackupTx is SkipDueBackup inside an existing transaction.
func skipDueBackupTx(ctx context.Context, tx *sql.Tx, volumeName string, nextFireAt, now int64) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE schedules SET next_fire_at = ?, updated_at = ? WHERE volume_name = ?`,
		nextFireAt, now, volumeName); err != nil {
		return fmt.Errorf("store: skip schedule %q: %w", volumeName, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO control_meta (key, value) VALUES (?, '1')
		ON CONFLICT(key) DO UPDATE SET value = CAST(CAST(control_meta.value AS INTEGER) + 1 AS TEXT)
	`, metaMaintSkipped); err != nil {
		return fmt.Errorf("store: count skipped backup: %w", err)
	}
	return nil
}

// RecordAdvertisedServe notes that a changelog page was served to a reader that
// advertised node_maintenance support: maintenance.advertised_high_water is a
// monotonic max of the served high water, and maintenance.advertised_at is now.
// The CLI uses both to tell whether the controller has seen its local hold.
func (s *Store) RecordAdvertisedServe(ctx context.Context, highWater, now int64) error {
	return s.withControlTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO control_meta (key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value
			WHERE CAST(excluded.value AS INTEGER) > CAST(control_meta.value AS INTEGER)
		`, metaMaintAdvHighWater, strconv.FormatInt(highWater, 10)); err != nil {
			return fmt.Errorf("store: record advertised high water: %w", err)
		}
		return setMetaTx(ctx, tx, metaMaintAdvAt, strconv.FormatInt(now, 10))
	})
}

// GetAdvertised returns the advertised high water and when it was last served
// (both 0 if never).
func (s *Store) GetAdvertised(ctx context.Context) (highWater, at int64, err error) {
	if highWater, err = s.metaInt(ctx, metaMaintAdvHighWater); err != nil {
		return 0, 0, err
	}
	if at, err = s.metaInt(ctx, metaMaintAdvAt); err != nil {
		return 0, 0, err
	}
	return highWater, at, nil
}

// metaInt reads an integer control_meta value (0 if absent).
func (s *Store) metaInt(ctx context.Context, key string) (int64, error) {
	v, found, err := s.GetMeta(ctx, key)
	if err != nil || !found || v == "" {
		return 0, err
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("store: parse %s %q: %w", key, v, err)
	}
	return n, nil
}

type maintJobMarker struct {
	StartedAt int64 `json:"started_at"`
}

// BeginMaintJob marks a node maintenance job (prune, compact) as running. In one
// tx it refuses (started=false) while the node is paused or while the job's
// marker already exists; otherwise it writes the marker. The markers let the
// status view report a job in flight from outside the agent process.
func (s *Store) BeginMaintJob(ctx context.Context, name string) (started bool, err error) {
	if name == "" {
		return false, errors.New("store: BeginMaintJob requires name")
	}
	key := metaMaintJobPrefix + name
	b, err := json.Marshal(maintJobMarker{StartedAt: time.Now().Unix()})
	if err != nil {
		return false, fmt.Errorf("store: marshal maint job marker: %w", err)
	}
	err = s.withControlTx(ctx, func(tx *sql.Tx) error {
		paused, err := isPausedTx(ctx, tx)
		if err != nil || paused {
			return err
		}
		var n int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM control_meta WHERE key = ?`, key).Scan(&n); err != nil {
			return fmt.Errorf("store: check maint job %q: %w", name, err)
		}
		if n > 0 {
			return nil
		}
		if err := setMetaTx(ctx, tx, key, string(b)); err != nil {
			return err
		}
		started = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return started, nil
}

// EndMaintJob removes a maintenance job's running marker (absent is a no-op).
func (s *Store) EndMaintJob(ctx context.Context, name string) error {
	if _, err := s.control.ExecContext(ctx,
		`DELETE FROM control_meta WHERE key = ?`, metaMaintJobPrefix+name); err != nil {
		return fmt.Errorf("store: end maint job %q: %w", name, err)
	}
	return nil
}

// ListMaintJobs returns the running maintenance jobs as name -> started_at.
func (s *Store) ListMaintJobs(ctx context.Context) (map[string]int64, error) {
	rows, err := s.control.QueryContext(ctx,
		`SELECT key, value FROM control_meta WHERE substr(key, 1, ?) = ?`,
		len(metaMaintJobPrefix), metaMaintJobPrefix)
	if err != nil {
		return nil, fmt.Errorf("store: list maint jobs: %w", err)
	}
	defer rows.Close()

	out := map[string]int64{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, fmt.Errorf("store: scan maint job: %w", err)
		}
		var mk maintJobMarker
		if err := json.Unmarshal([]byte(value), &mk); err != nil {
			return nil, fmt.Errorf("store: parse maint job %q: %w", key, err)
		}
		out[strings.TrimPrefix(key, metaMaintJobPrefix)] = mk.StartedAt
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate maint jobs: %w", err)
	}
	return out, nil
}

// ClearMaintJobMarkers removes every maintenance job marker. Called at boot: no
// job survives a restart, so any marker left is stale.
func (s *Store) ClearMaintJobMarkers(ctx context.Context) error {
	if _, err := s.control.ExecContext(ctx,
		`DELETE FROM control_meta WHERE substr(key, 1, ?) = ?`,
		len(metaMaintJobPrefix), metaMaintJobPrefix); err != nil {
		return fmt.Errorf("store: clear maint job markers: %w", err)
	}
	return nil
}
