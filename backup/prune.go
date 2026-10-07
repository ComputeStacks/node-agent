package backup

import (
	"context"
	"cs-agent/backup/borg"
	"cs-agent/store"
	"cs-agent/types"

	"github.com/getsentry/sentry-go"
)

// prune applies each backup-enabled volume's borg retention policy. Reads volume
// desired-state from this node's control.db; runs under the per-repo lock so it
// never overlaps compact/export of the same repo. Stops between volumes if the
// node enters maintenance; a volume already being pruned finishes.
func prune(ctx context.Context, st *store.Store) {
	defer sentry.Recover()

	vols, err := st.ListVolumes(ctx)
	if err != nil {
		backupLogger().Warn("Prune error listing volumes", "error", err.Error())
		sentry.CaptureException(err)
		return
	}

	for i, sv := range vols {
		if ctx.Err() != nil { // stop the sweep promptly on shutdown
			return
		}
		if sweepPaused(ctx, st, "prune", len(vols)-i) {
			return
		}
		vol, err := types.LoadVolume(sv.Config)
		if err != nil {
			backupLogger().Warn("Prune: error parsing volume", "volume", sv.Name, "error", err.Error())
			sentry.CaptureException(err)
			continue
		}
		if vol.Backup {
			pruneVolume(st, vol)
		}
	}
}

// pruneVolume prunes one volume's repository. A variable so tests can stand in
// for the borg run.
//
// Serialized against compact/export of the same repo; the lock releases on return
// (and on panic). vol.Name is the repository owner here — a prune is a
// same-volume operation and passes vol as both of FindRepository's arguments — so
// the key names the same repository the lock is protecting.
var pruneVolume = func(st *store.Store, vol types.Volume) {
	defer borg.AcquireRepoLock(vol.Name)()
	repo, repoErr := borg.FindRepository(st, &vol, &vol)
	if repoErr != nil {
		backupLogger().Warn("Prune Volume Error, error loading repo", "volume", vol.Name, "error", repoErr.Message)
		return
	}
	if pruneErr := repo.Prune(); pruneErr != nil {
		// Carry the reason. Prune could not report one before — a non-zero
		// `borg prune` exit did not reach here at all — so this line logged that
		// something went wrong and dropped what it was.
		backupLogger().Warn("Prune Volume Error", "volume", vol.Name, "error", borgFailure(pruneErr))
	}
	repo.Container.Stop()
}

// sweepPaused reports whether a prune/compact sweep must stop before its next
// volume because the node is paused for maintenance (or its state can't be read).
// left is the number of volumes the sweep has not yet reached.
func sweepPaused(ctx context.Context, st *store.Store, job string, left int) bool {
	paused, err := st.IsPaused(ctx)
	if err != nil {
		backupLogger().Warn("Maintenance sweep stopped; cannot read maintenance state", "job", job, "volumes_left", left, "error", err.Error())
		return true
	}
	if paused {
		backupLogger().Info("Maintenance sweep stopped; node in maintenance", "job", job, "volumes_left", left)
	}
	return paused
}
