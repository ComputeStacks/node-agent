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
// never overlaps compact/export of the same repo.
func prune(ctx context.Context, st *store.Store) {
	defer sentry.Recover()

	vols, err := st.ListVolumes(ctx)
	if err != nil {
		backupLogger().Warn("Prune error listing volumes", "error", err.Error())
		sentry.CaptureException(err)
		return
	}

	for _, sv := range vols {
		if ctx.Err() != nil { // stop the sweep promptly on shutdown
			return
		}
		vol, err := types.LoadVolume(sv.Config)
		if err != nil {
			backupLogger().Warn("Prune: error parsing volume", "volume", sv.Name, "error", err.Error())
			sentry.CaptureException(err)
			continue
		}
		if vol.Backup {
			// Serialize against compact/export of the same repo. Scoped to a
			// closure so the lock releases each iteration (and on panic).
			func() {
				defer borg.AcquireRepoLock(vol.Name)()
				repo, repoErr := borg.FindRepository(st, &vol, &vol)
				if repoErr != nil {
					backupLogger().Warn("Prune Volume Error, error loading repo", "volume", vol.Name, "error", repoErr.Message)
					return
				}
				if pruneErr := repo.Prune(); pruneErr != nil {
					// Carry the reason. Prune could not report one before — a
					// non-zero `borg prune` exit did not reach here at all — so
					// this line logged that something went wrong and dropped
					// what it was.
					backupLogger().Warn("Prune Volume Error", "volume", vol.Name, "error", borgFailure(pruneErr))
				}
				repo.Container.Stop()
			}()
		}
	}
}
