package backup

import (
	"context"
	"cs-agent/backup/borg"
	"cs-agent/store"
	"cs-agent/types"
	"hash/fnv"
	"os"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/spf13/viper"
)

// compact reclaims space freed by prune/delete for every backup-enabled volume in
// this node's control.db. It runs borg compact under the per-repo lock so it never
// overlaps an export of the same repo. (hostname is used only for the jitter seed.)
func compact(ctx context.Context, st *store.Store) {
	defer sentry.Recover()
	hostname, _ := os.Hostname()

	// Per-node jitter so many nodes sharing one backup server don't all start
	// compacting at the same cron minute. Deterministic (hostname-derived) for
	// even spread and reproducibility.
	if jitter := viper.GetInt("backups.compact_jitter_sec"); jitter > 0 {
		select {
		case <-time.After(jitterDelay(hostname, jitter)):
		case <-ctx.Done(): // ctx-aware: don't hold up shutdown during the jitter sleep
			return
		}
	}

	vols, err := st.ListVolumes(ctx)
	if err != nil {
		backupLogger().Warn("Compact error listing volumes", "error", err.Error())
		sentry.CaptureException(err)
		return
	}

	for _, sv := range vols {
		if ctx.Err() != nil { // stop the sweep promptly on shutdown
			return
		}
		vol, err := types.LoadVolume(sv.Config)
		if err != nil {
			backupLogger().Warn("Compact: error parsing volume", "volume", sv.Name, "error", err.Error())
			continue
		}
		if vol.Backup {
			// Skip a volume that has no repository yet. This sweep builds its
			// borg.Repository directly rather than through FindRepository, so
			// nothing here has established that the repository exists: compacting
			// one that was never initialized exits 2 (measured on borg 1.4.4), and
			// InitBackupContainer leaves a stray b-<volume> docker volume behind on
			// the way. A repositories row is the cheapest available proof, and it
			// costs no container to read.
			//
			// Not routed through FindRepository on purpose: Compact dispatches to
			// compactNFS for the NFS backend, which deliberately builds no
			// container, and FindRepository would force one.
			//
			// Accepted limitation: rows are written only by a successful Sync, so
			// after a migration or a lost row an existing repository is skipped
			// until its next successful backup or prune — normally one cycle.
			if _, found, err := st.GetRepository(ctx, vol.Name); err != nil {
				backupLogger().Warn("Compact: error loading repository", "volume", vol.Name, "error", err.Error())
				sentry.CaptureException(err)
				continue
			} else if !found {
				backupLogger().Debug("Compact: skipping volume with no repository", "volume", vol.Name)
				continue
			}

			// Scoped closure so the lock releases each iteration (and on panic),
			// and so one repo blocked behind an in-flight export doesn't stall
			// the rest of the sweep.
			func() {
				defer borg.AcquireRepoLock(vol.Name)()
				repo := borg.Repository{Name: vol.Name, SourceVolumeName: vol.Name, Store: st}
				if log := repo.Compact(); log != nil {
					backupLogger().Warn("Compact Volume Error", "volume", vol.Name, "error", log.Message)
				}
				repo.StopContainer() // no-op for the NFS backend (no container)
			}()
		}
	}
}

// jitterDelay maps a hostname to a stable delay in [0, maxSec).
func jitterDelay(hostname string, maxSec int) time.Duration {
	if maxSec <= 0 {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(hostname))
	return time.Duration(int(h.Sum32()%uint32(maxSec))) * time.Second
}
