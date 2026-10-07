package maintenance

import (
	"context"
	"fmt"
	"time"

	"cs-agent/store"

	"github.com/getsentry/sentry-go"
)

const (
	// sampleEvery is how often the watcher samples quiesce while paused.
	sampleEvery = 10 * time.Second
	// staleCheckEvery is how often the watcher looks for holds left too long.
	staleCheckEvery = time.Hour
)

// WatchStore is the subset of *store.Store the watcher uses.
type WatchStore interface {
	StatusStore
	IsPaused(ctx context.Context) (bool, error)
	RecordMaintenanceSample(ctx context.Context, s store.MaintenanceSample) (bool, error)
}

// Watch runs until ctx is done. While the node is paused it samples the status
// every 10 s and records it (RecordMaintenanceSample publishes a changed sample
// as a node_maintenance changelog entry, so the controller can follow the
// drain). At start and then hourly it logs a warning, and reports to Sentry,
// for each hold older than staleAfter; staleAfter <= 0 disables that check.
func Watch(ctx context.Context, st WatchStore, staleAfter time.Duration) {
	watch(ctx, st, DockerLister(), staleAfter, sampleEvery, staleCheckEvery)
}

func watch(ctx context.Context, st WatchStore, cl ContainerLister, staleAfter, sampleInterval, staleInterval time.Duration) {
	sample := time.NewTicker(sampleInterval)
	defer sample.Stop()
	stale := time.NewTicker(staleInterval)
	defer stale.Stop()

	checkStale(ctx, st, staleAfter, time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case <-sample.C:
			sampleOnce(ctx, st, cl)
		case now := <-stale.C:
			checkStale(ctx, st, staleAfter, now)
		}
	}
}

// sampleOnce records one quiesce sample if the node is paused. Nothing is
// sampled while not paused: the docker query is skipped and the stored sample
// is cleared anyway on the next entry into maintenance.
func sampleOnce(ctx context.Context, st WatchStore, cl ContainerLister) {
	paused, err := st.IsPaused(ctx)
	if err != nil {
		if ctx.Err() == nil {
			maintenanceLog().Warn("maintenance sample: read paused state", "error", err.Error())
		}
		return
	}
	if !paused {
		return
	}
	s, err := BuildStatus(ctx, st, cl)
	if err != nil {
		if ctx.Err() == nil {
			maintenanceLog().Warn("maintenance sample: build status", "error", err.Error())
		}
		return
	}
	appended, err := st.RecordMaintenanceSample(ctx, store.MaintenanceSample{
		Quiesce:      s.Quiesce,
		RunningCount: len(s.Running),
		Pending:      s.Pending,
		SampledAt:    time.Now().Unix(),
	})
	if err != nil {
		if ctx.Err() == nil {
			maintenanceLog().Warn("maintenance sample: record", "error", err.Error())
		}
		return
	}
	if appended {
		maintenanceLog().Info("maintenance quiesce sample changed", "quiesce", s.Quiesce, "running", len(s.Running), "pending", s.Pending)
	}
}

// checkStale warns about each hold older than staleAfter.
func checkStale(ctx context.Context, st WatchStore, staleAfter time.Duration, now time.Time) {
	if staleAfter <= 0 {
		return
	}
	m, err := st.GetMaintenance(ctx)
	if err != nil {
		if ctx.Err() == nil {
			maintenanceLog().Warn("maintenance stale check: read state", "error", err.Error())
		}
		return
	}
	for _, h := range []struct {
		source string
		hold   *store.MaintenanceHold
	}{{"controller", m.Controller}, {"local", m.Local}} {
		if h.hold == nil {
			continue
		}
		age := now.Sub(time.Unix(h.hold.Since, 0))
		if age < staleAfter {
			continue
		}
		maintenanceLog().Warn("Maintenance hold has been in place a long time; the node starts no new work while it is held",
			"source", h.source, "reason", h.hold.Reason, "by", h.hold.By, "age", age.Truncate(time.Minute).String())
		sentry.CaptureMessage(fmt.Sprintf("node maintenance %s hold older than %s", h.source, staleAfter))
	}
}
