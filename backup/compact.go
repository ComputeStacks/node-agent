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
// Stops between volumes if the node enters maintenance; a volume already being
// compacted finishes.
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

	for i, sv := range vols {
		if ctx.Err() != nil { // stop the sweep promptly on shutdown
			return
		}
		// Checked here, after the jitter sleep, so a pause taken during the sleep
		// stops the sweep before its first volume.
		if sweepPaused(ctx, st, "compact", len(vols)-i) {
			return
		}
		vol, err := types.LoadVolume(sv.Config)
		if err != nil {
			backupLogger().Warn("Compact: error parsing volume", "volume", sv.Name, "error", err.Error())
			continue
		}
		if vol.Backup {
			switch action, err := compactActionFor(ctx, st, vol.Name); action {
			case compactSkipStoreError:
				backupLogger().Warn("Compact: error loading repository", "volume", vol.Name, "error", err.Error())
				sentry.CaptureException(err)
				continue
			case compactSkipNoRepo:
				backupLogger().Debug("Compact: skipping volume with no repository", "volume", vol.Name)
				continue
			}

			compactVolume(st, vol)
		}
	}
}

// compactVolume compacts one volume's repository. A variable so tests can stand in
// for the borg run.
//
// The lock releases on return (and on panic), so one repo blocked behind an
// in-flight export doesn't stall the rest of the sweep. vol.Name is the
// repository owner: the sweep only ever compacts this node's own volumes, so the
// Repository below is built with Name set to the same value the lock is keyed on.
var compactVolume = func(st *store.Store, vol types.Volume) {
	defer borg.AcquireRepoLock(vol.Name)()
	repo := borg.Repository{Name: vol.Name, Store: st}
	if log := repo.Compact(); log != nil {
		backupLogger().Warn("Compact Volume Error", "volume", vol.Name, "error", log.Message)
	}
	repo.StopContainer() // no-op for the NFS backend (no container)
}

// compactAction is what the store says a compact sweep should do with one volume.
type compactAction int

const (
	compactRun            compactAction = iota // a repositories row exists: compact it
	compactSkipNoRepo                          // no row: there is no repository to compact
	compactSkipStoreError                      // the store could not answer: skip this sweep only
)

// compactActionFor decides whether the sweep should compact a volume, returning the
// store's error alongside compactSkipStoreError so the caller can report it.
//
// The sweep builds its borg.Repository directly rather than through FindRepository,
// so nothing has established that the repository exists: compacting one that was
// never initialized exits 2 (measured on borg 1.4.4), and InitBackupContainer leaves
// a stray b-<volume> docker volume behind on the way. A repositories row is the
// cheapest available proof, and it costs no container to read.
//
// Not routed through FindRepository on purpose: Compact dispatches to compactNFS for
// the NFS backend, which deliberately builds no container, and FindRepository would
// force one.
//
// The three outcomes are kept distinct because a store that could not answer is not
// the same fact as a volume with no repository. Collapsing them would skip a healthy
// volume's compaction silently, for as long as the store kept failing; kept apart, a
// store error is reported and costs only the current sweep.
//
// Accepted limitation: rows are written only by a successful Sync, so after a
// migration or a lost row an existing repository is skipped until its next successful
// backup or prune — normally one cycle.
func compactActionFor(ctx context.Context, st *store.Store, volume string) (compactAction, error) {
	_, found, err := st.GetRepository(ctx, volume)
	switch {
	case err != nil:
		return compactSkipStoreError, err
	case !found:
		return compactSkipNoRepo, nil
	default:
		return compactRun, nil
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
