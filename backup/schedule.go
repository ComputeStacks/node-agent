/*
Durable backup scheduler.

Replaces the old in-RAM robfig runner + Consul schedule mirror. robfig/cron is
kept only as the cron-string PARSER (ParseStandard + Schedule.Next), so the cron
syntax the controller emits is unchanged. Per-volume backup schedules live in
control.db (the `schedules` table): a tick loop fires due schedules by inserting a
volume.backup task and advancing next_fire_at in one transaction (durable
exactly-once). Node maintenance (prune/compact/changelog-prune/task-retention)
runs on the same tick with skip-on-misfire.
*/
package backup

import (
	"context"
	"cs-agent/store"
	"cs-agent/types"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/google/uuid"
	"github.com/spf13/viper"
)

const schedulerTick = 20 * time.Second

// Scheduler owns per-volume backup scheduling + node maintenance, driven off
// control.db. Construct with NewScheduler and run with Run (until ctx is done).
type Scheduler struct {
	st          *store.Store
	hostname    string
	dispatch    func() // poke the dispatcher after enqueuing work (non-blocking)
	tick        time.Duration
	reconcileCh chan struct{}
	maint       []*maintJob
	maintWg     sync.WaitGroup // tracks in-flight maintenance goroutines
}

// maintJob is a node-wide maintenance task on a cron schedule with skip-on-misfire
// (next-fire held in RAM; a node down over a slot simply skips it).
type maintJob struct {
	name    string
	expr    string
	next    time.Time
	run     func(ctx context.Context)
	running atomic.Bool // true while a run is in flight (overlap guard)
}

// NewScheduler builds the scheduler. dispatch is called (non-blocking) after a
// backup/trash task is enqueued so the dispatcher wakes promptly; it may be nil.
func NewScheduler(st *store.Store, dispatch func()) *Scheduler {
	hostname, _ := os.Hostname()
	s := &Scheduler{
		st:          st,
		hostname:    hostname,
		dispatch:    dispatch,
		tick:        schedulerTick,
		reconcileCh: make(chan struct{}, 1),
	}
	s.maint = []*maintJob{
		{name: "prune", expr: viper.GetString("backups.prune_freq"), run: func(ctx context.Context) { prune(ctx, st) }},
		{name: "compact", expr: viper.GetString("backups.compact_freq"), run: func(ctx context.Context) { compact(ctx, st) }},
	}
	// NB: changelog/task-retention housekeeping is NOT a maint job here — it runs
	// unconditionally via backup.Housekeeper (main.go), independent of
	// backups.enabled, so a backups-disabled node still bounds control.db growth.
	return s
}

// ReconcileSignal asks the scheduler to reconcile volume schedules on its next
// loop iteration. Non-blocking + coalescing: a late DOWN handler never blocks.
func (s *Scheduler) ReconcileSignal() {
	select {
	case s.reconcileCh <- struct{}{}:
	default:
	}
}

// Run drives the scheduler until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	backupLogger().Info("Starting backup scheduler")
	// Drain any in-flight maintenance goroutines before returning, so nothing
	// touches the store after main proceeds to store.Close() on shutdown.
	defer s.maintWg.Wait()
	now := time.Now()
	for _, m := range s.maint {
		if m.expr == "" {
			backupLogger().Warn("Maintenance job has no cron; disabled", "job", m.name)
			continue
		}
		m.next = store.NextFire(m.expr, now)
		if m.next.IsZero() {
			backupLogger().Warn("Maintenance job cron unparseable; will not run", "job", m.name, "cron", m.expr)
		} else {
			backupLogger().Info("Maintenance scheduled", "job", m.name, "cron", m.expr)
		}
	}
	s.reconcile(ctx) // boot rebuild

	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			backupLogger().Info("Backup scheduler stopping")
			return
		case <-s.reconcileCh:
			s.reconcile(ctx)
		case <-ticker.C:
			s.fireDue(ctx)
			s.runMaintenance(ctx)
			s.reconcile(ctx) // periodic backstop
		}
	}
}

// fireDue enqueues a volume.backup task for every schedule whose next_fire_at has
// passed, advancing next_fire_at in the same transaction (exactly-once). While the
// node is paused for maintenance a due slot is skipped instead: next_fire_at moves
// to the next slot and the skip is counted, so nothing fires on exit.
func (s *Scheduler) fireDue(ctx context.Context) {
	defer sentry.Recover()
	now := time.Now()
	due, err := s.st.ListDueSchedules(ctx, now.Unix())
	if err != nil {
		backupLogger().Warn("Scheduler: list due schedules", "error", err.Error())
		return
	}
	if len(due) == 0 {
		return
	}
	paused, err := s.st.IsPaused(ctx)
	if err != nil {
		// Unknown pause state: fire nothing this tick; the slots stay due.
		backupLogger().Warn("Scheduler: check maintenance state", "error", err.Error())
		return
	}
	fired := false
	for _, sc := range due {
		next := store.NextFire(sc.CronExpr, now)
		if next.IsZero() {
			// Unparseable/never-firing cron slipped in — drop the schedule so it
			// doesn't re-evaluate as due every tick.
			backupLogger().Warn("Scheduler: dropping schedule with unparseable cron", "volume", sc.VolumeName, "cron", sc.CronExpr)
			_ = s.st.DeleteSchedule(ctx, sc.VolumeName)
			continue
		}
		// Re-check the volume still wants backups (config may have changed since
		// the row was written) and grab its project id for the task/changelog.
		v, found, err := s.st.GetVolume(ctx, sc.VolumeName)
		if err != nil {
			backupLogger().Warn("Scheduler: load volume for due schedule", "volume", sc.VolumeName, "error", err.Error())
			continue
		}
		if !found {
			_ = s.st.DeleteSchedule(ctx, sc.VolumeName)
			continue
		}
		vol, err := types.LoadVolume(v.Config)
		if err != nil || !vol.Backup || vol.Trash {
			_ = s.st.DeleteSchedule(ctx, sc.VolumeName)
			continue
		}
		if paused {
			if err := s.st.SkipDueBackup(ctx, sc.VolumeName, next.Unix()); err != nil {
				backupLogger().Warn("Scheduler: skip due backup", "volume", sc.VolumeName, "error", err.Error())
				continue
			}
			backupLogger().Info("Scheduler: node in maintenance, skipped backup", "volume", sc.VolumeName, "next", next.UTC().Format(time.RFC3339))
			continue
		}
		task := store.Task{
			ID:        uuid.New().String(),
			Name:      "volume.backup",
			Node:      s.hostname,
			Volume:    sc.VolumeName,
			ProjectID: strconv.Itoa(vol.ProjectID),
			Archive:   "auto",
		}
		created, err := s.st.FireDueBackup(ctx, task, next.Unix())
		if err != nil {
			backupLogger().Warn("Scheduler: fire due backup", "volume", sc.VolumeName, "error", err.Error())
			continue
		}
		if !created {
			// The node was paused between the check above and the fire tx; the
			// store skipped the slot.
			backupLogger().Info("Scheduler: backup not fired (node entered maintenance or task already exists)", "volume", sc.VolumeName, "next", next.UTC().Format(time.RFC3339))
			continue
		}
		fired = true
	}
	if fired && s.dispatch != nil {
		s.dispatch()
	}
}

// runMaintenance runs any node maintenance job whose cron slot has passed
// (skip-on-misfire: next is always recomputed from now, never replayed). Each due
// job runs in its OWN goroutine so a long prune/compact (incl. its jitter sleep)
// never blocks backup firing or reconcile on the tick loop; an overlap guard skips
// a job whose prior run is still in flight, and maintWg lets Run drain them on
// shutdown before the store closes. A job starts only once BeginMaintJob has
// written its running marker; while the node is paused it refuses and the slot is
// skipped like any other misfire.
func (s *Scheduler) runMaintenance(ctx context.Context) {
	now := time.Now()
	for _, m := range s.maint {
		if m.expr == "" {
			continue
		}
		if m.next.IsZero() {
			m.next = store.NextFire(m.expr, now)
			continue
		}
		if now.Before(m.next) {
			continue
		}
		m.next = store.NextFire(m.expr, now)
		if !m.running.CompareAndSwap(false, true) {
			backupLogger().Warn("Scheduler: skipping maintenance; previous run still in progress", "job", m.name)
			continue
		}
		started, err := s.st.BeginMaintJob(ctx, m.name)
		if err != nil {
			m.running.Store(false)
			backupLogger().Warn("Scheduler: begin maintenance job", "job", m.name, "error", err.Error())
			continue
		}
		if !started {
			m.running.Store(false)
			backupLogger().Info("Scheduler: skipping maintenance; node paused or job already marked running", "job", m.name)
			continue
		}
		s.maintWg.Add(1)
		go func(m *maintJob) {
			defer s.maintWg.Done()
			defer m.running.Store(false)
			// Deferred so the marker goes on any exit, panic included. Not bound to
			// ctx: on shutdown ctx is already cancelled and the delete must still land.
			defer func() {
				if err := s.st.EndMaintJob(context.WithoutCancel(ctx), m.name); err != nil {
					backupLogger().Warn("Scheduler: end maintenance job", "job", m.name, "error", err.Error())
				}
			}()
			defer sentry.Recover() // report, don't crash the agent
			m.run(ctx)
		}(m)
	}
}

// reconcile brings the schedules table in line with volume desired-state. Gated by
// the volumes-populated sentinel so an unpopulated control.db never wipes every
// schedule. A trashed volume gets a (stable-id, idempotent) volume.trash task and
// its schedule removed; a backup-disabled or vanished volume just loses its
// schedule; a new/changed cron gets Next(now) (no catch-up on first schedule, so a
// fleet backfill can't trigger a backup storm).
func (s *Scheduler) reconcile(ctx context.Context) {
	defer sentry.Recover()
	populated, err := s.st.IsPopulated(ctx, store.MetaVolumesPopulated)
	if err != nil {
		backupLogger().Warn("Scheduler: check volumes populated", "error", err.Error())
		return
	}
	if !populated {
		backupLogger().Debug("Scheduler: volumes not populated, skipping reconcile")
		return
	}

	vols, err := s.st.ListVolumes(ctx)
	if err != nil {
		backupLogger().Warn("Scheduler: list volumes", "error", err.Error())
		return
	}

	now := time.Now()
	seen := map[string]bool{}
	trashed := false
	for _, sv := range vols {
		vol, err := types.LoadVolume(sv.Config)
		if err != nil {
			backupLogger().Warn("Scheduler: parse volume", "volume", sv.Name, "error", err.Error())
			continue
		}
		seen[vol.Name] = true

		if vol.Trash {
			// Enqueue teardown once and stop scheduling. resetFailed=false: a
			// lingering trash:true row (the controller hasn't DELETEd it yet) must
			// NOT re-run a failed teardown every tick — only an explicit controller
			// re-DELETE retries. EnqueueTeardown reads before writing, so a
			// pending/running/completed/failed row is a no-op that writes no rows/
			// changelog and doesn't poke the dispatcher (it still opens a short
			// IMMEDIATE tx for the read — fine on a single-node control.db).
			enq, err := s.st.EnqueueTeardown(ctx, store.Task{
				ID:        "volume.trash:" + vol.Name,
				Name:      "volume.trash",
				Node:      s.hostname,
				Volume:    vol.Name,
				ProjectID: strconv.Itoa(vol.ProjectID),
			}, false)
			if err != nil {
				backupLogger().Warn("Scheduler: enqueue trash", "volume", vol.Name, "error", err.Error())
			} else if enq {
				trashed = true
			}
			_ = s.st.DeleteSchedule(ctx, vol.Name)
			continue
		}

		if !vol.Backup || vol.Freq == "" {
			_ = s.st.DeleteSchedule(ctx, vol.Name)
			continue
		}

		existing, found, _ := s.st.GetSchedule(ctx, vol.Name)
		if !found || existing.CronExpr != vol.Freq {
			next := store.NextFire(vol.Freq, now)
			if next.IsZero() {
				backupLogger().Warn("Scheduler: invalid cron for volume", "volume", vol.Name, "cron", vol.Freq)
				continue
			}
			if err := s.st.PutSchedule(ctx, vol.Name, vol.Freq, next.Unix()); err != nil {
				backupLogger().Warn("Scheduler: put schedule", "volume", vol.Name, "error", err.Error())
			}
		}
		// Unchanged cron: leave next_fire_at untouched (never push it forward).
	}

	// Drop schedules for volumes no longer present/owned here.
	all, err := s.st.ListSchedules(ctx)
	if err == nil {
		for _, sc := range all {
			if !seen[sc.VolumeName] {
				_ = s.st.DeleteSchedule(ctx, sc.VolumeName)
			}
		}
	}

	if trashed && s.dispatch != nil {
		s.dispatch()
	}
}
