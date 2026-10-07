package backup

import (
	"context"
	"cs-agent/store"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/spf13/viper"
)

// defaultHousekeepingInterval is the fallback cadence when changelog.prune_freq is
// empty or unparseable — control.db retention must never be silently disabled.
const defaultHousekeepingInterval = 15 * time.Minute

// Housekeeper prunes acked/aged changelog rows and reaps terminal task rows. It
// runs UNCONDITIONALLY (independent of backups.enabled): retention is a control.db
// concern, not a backup concern, so a backups-disabled node must still bound
// changelog/task growth.
type Housekeeper struct {
	st   *store.Store
	expr string
}

// NewHousekeeper builds the housekeeper from the changelog.prune_freq cron.
func NewHousekeeper(st *store.Store) *Housekeeper {
	return &Housekeeper{st: st, expr: viper.GetString("changelog.prune_freq")}
}

// Run drives housekeeping until ctx is cancelled, scheduled off the
// changelog.prune_freq CRON (not a fixed ticker). If the cron is empty or
// unparseable it falls back to a fixed interval so retention can't be silently
// turned off.
func (h *Housekeeper) Run(ctx context.Context) {
	if store.NextFire(h.expr, time.Now()).IsZero() {
		backupLogger().Warn("Housekeeping cron empty/unparseable; using default interval",
			"cron", h.expr, "interval", defaultHousekeepingInterval.String())
	} else {
		backupLogger().Info("Starting housekeeping", "cron", h.expr)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(h.untilNext(time.Now())):
			h.runOnce(ctx)
		}
	}
}

// untilNext returns the delay to the next run: the cron's next fire, or the
// default interval when the cron is empty/unparseable.
func (h *Housekeeper) untilNext(now time.Time) time.Duration {
	next := store.NextFire(h.expr, now)
	if next.IsZero() {
		return defaultHousekeepingInterval
	}
	if d := time.Until(next); d > 0 {
		return d
	}
	return time.Second // due now/in the past — fire promptly
}

func (h *Housekeeper) runOnce(ctx context.Context) {
	defer sentry.Recover()
	now := time.Now().Unix()
	minAge := int64(viper.GetInt("changelog.prune_min_age_sec"))
	maxAge := int64(viper.GetInt("changelog.prune_max_age_sec"))
	if n, err := h.st.PruneChangelog(ctx, now, minAge, maxAge); err != nil {
		backupLogger().Warn("Housekeeping: changelog prune", "error", err.Error())
	} else if n > 0 {
		backupLogger().Info("Pruned changelog rows", "count", n)
	}
	if retention := int64(viper.GetInt("tasks.retention_sec")); retention > 0 {
		if n, err := h.st.DeleteTerminalTasksBefore(ctx, now-retention); err != nil {
			backupLogger().Warn("Housekeeping: task retention", "error", err.Error())
		} else if n > 0 {
			backupLogger().Info("Reaped terminal tasks", "count", n)
		}
	}
}
