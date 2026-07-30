/*
*
Volume backup execution.
*/
package backup

import (
	"context"
	"cs-agent/backup/borg"
	"cs-agent/store"
	"cs-agent/types"
	"errors"
	"time"

	"github.com/getsentry/sentry-go"

	"github.com/spf13/viper"
)

// Perform runs a volume.backup task: create a borg archive of the volume and, on
// success, record the backup time in the task result (the controller reads
// last_backup from the completed task's result_json — there is no volumes
// writeback). Soft failures are reported via projectEvent.EventLog.Status; the
// worker marks the task failed and stores the accumulated output.
func Perform(ctx context.Context, st *store.Store, task store.Task, projectEvent *progress) error {
	// NOTE: no handler-level sentry.Recover() here — a panic must propagate to the
	// worker's terminal guard (job/worker.go) so the task is marked FAILED, not
	// silently recovered into a false "completed". The guard reports to Sentry.
	v, found, err := st.GetVolume(ctx, task.Volume)
	if err != nil {
		backupLogger().Warn("Fatal error loading volume from store", "volume", task.Volume, "error", err.Error())
		sentry.CaptureException(err)
		return err
	}
	if !found {
		backupLogger().Warn("Skipping backup job for unknown volume", "volume", task.Volume)
		return nil
	}
	vol, err := types.LoadVolume(v.Config)
	if err != nil {
		backupLogger().Warn("Fatal error loading volume", "volume", task.Volume, "error", err.Error())
		sentry.CaptureException(err)
		return err
	}

	backupLogger().Info("Backing up volume", "volume", task.Volume)

	repo, findRepoMsg := borg.FindRepository(st, &vol, &vol)

	defer func() {
		// Stop borg container
		if !repo.StopContainer() {
			projectEvent.PostEventUpdate("agent-fe161b668b0756df", "Failed to stop backup container")
		}
	}()

	if findRepoMsg != nil {
		if findRepoMsg.MsgID == "Repository.DoesNotExist" {
			repo = &borg.Repository{Name: vol.Name, Store: st}
			// Build backup container
			repoErr := repo.Setup(&vol, &vol)
			if repoErr != nil {
				projectEvent.EventLog.Status = "failed"
				projectEvent.PostEventUpdate("agent-d4c34f1d89c20aa6", repoErr.ToYaml())
				return errors.New(repoErr.Message)
			}
		} else if findRepoMsg.MsgID == "InvalidRepository" && viper.GetBool("backups.borg.ssh.enabled") {
			// Empty SSH repos return 'InvalidRepository' rather than 'DoesNotExist'.
			repo = &borg.Repository{Name: vol.Name, Store: st}
			// Build backup container
			repoErr := repo.Setup(&vol, &vol)
			if repoErr != nil {
				projectEvent.EventLog.Status = "failed"
				projectEvent.PostEventUpdate("agent-7fad20a06cbd26a2", repoErr.ToYaml())
				return errors.New(repoErr.Message)
			}
		} else {
			projectEvent.EventLog.Status = "failed"
			projectEvent.PostEventUpdate("agent-c4087f229d50d4dc", findRepoMsg.ToYaml())
			return errors.New(borgFailure(findRepoMsg))
		}
	}

	archive := borg.Archive{
		Name:       task.Archive,
		Repository: repo,
	}

	preBackupSuccess := preBackup(&vol, projectEvent)

	backupSucceeded := false

	if preBackupSuccess {
		archiveMsg, archiveErr := archive.Create()
		if archiveErr != nil {
			projectEvent.PostEventUpdate("agent-d894f86c71d0db7b", archiveErr.ToYaml())
			if projectEvent.EventLog.Status == "running" {
				projectEvent.EventLog.Status = "failed"
			}

			if vol.RestoreContinueOnError {
				postBackup(&vol, projectEvent, repo)
			}
		} else {
			// Success: keep the full borg archive stats in result_json for the
			// controller, but don't log the YAML at INFO — the borg layer already
			// logs a concise "Completed backup" line (archive id + duration). The
			// full response is only worth logging on failure (see archiveErr above).
			projectEvent.Record(archiveMsg.ToYaml())
			postBackup(&vol, projectEvent, repo)
			backupSucceeded = true
		}
	} else {
		if projectEvent.EventLog.Status == "running" {
			projectEvent.EventLog.Status = "failed"
		}

		if vol.BackupContinueOnError {
			postBackup(&vol, projectEvent, repo)
		}
	}

	// Only advance last_backup on a successful archive creation. On failure the
	// task is marked failed (via EventLog.Status) and carries no last_backup, so
	// the controller — which reads last_backup from the completed task result —
	// correctly sees the volume as still overdue.
	if !backupSucceeded {
		return nil
	}

	projectEvent.Set("last_backup", time.Now().Unix())
	return nil
}
