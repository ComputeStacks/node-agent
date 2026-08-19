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
		if findRepoMsg.MsgID == borg.MsgIDRepositoryMissing {
			repo = &borg.Repository{Name: vol.Name, Store: st}
			// Build backup container
			repoErr := repo.Setup(&vol)
			if repoErr != nil {
				reason := borgFailure(repoErr)
				projectEvent.EventLog.Status = "failed"
				projectEvent.PostEventUpdate("agent-d4c34f1d89c20aa6", reason)
				return errors.New(reason)
			}
		} else if findRepoMsg.MsgID == borg.MsgIDRepositoryInvalid && viper.GetBool("backups.borg.ssh.enabled") {
			// An SSH repository whose directory exists but was never initialized: borg
			// reports Repository.InvalidRepository rather than Repository.DoesNotExist, so
			// it still needs a `borg init`.
			//
			// The comparison is against the exported msgid because it used to be against
			// the literal "InvalidRepository", which borg never emits — the record is
			// spelled Repository.InvalidRepository (captured in the borg package's
			// failure_record_test.go). This branch was therefore dead, and such a
			// repository failed every backup forever with no way out.
			repo = &borg.Repository{Name: vol.Name, Store: st}
			// Build backup container
			repoErr := repo.Setup(&vol)
			if repoErr != nil {
				reason := borgFailure(repoErr)
				projectEvent.EventLog.Status = "failed"
				projectEvent.PostEventUpdate("agent-7fad20a06cbd26a2", reason)
				return errors.New(reason)
			}
		} else {
			// borgFailure, not ToYaml(), and the SAME value in both places. ToYaml renders
			// the whole LogMessage, so a docker-level failure — which carries no msgid or
			// levelname — reached the controller as five empty fields wrapped around one
			// line of text. The controller reads the accumulated output ahead of the task
			// error, so the struct dump was what an operator actually saw; posting the
			// reason and returning it keeps those two the same, as the create path below
			// and the delete path already do.
			reason := borgFailure(findRepoMsg)
			projectEvent.EventLog.Status = "failed"
			projectEvent.PostEventUpdate("agent-c4087f229d50d4dc", reason)
			return errors.New(reason)
		}
	}

	archive := borg.Archive{
		Name:       task.Archive,
		Repository: repo,
	}

	preBackupSuccess := preBackup(&vol, projectEvent)

	backupSucceeded := false

	// createErr carries the create failure's reason out to the return, so result_json
	// gets borg's own diagnosis instead of runner.go's synthesized "task reported
	// failure". It cannot be returned where it is discovered: postBackup still has to
	// run first (see BackupContinueOnError below).
	var createErr error

	if preBackupSuccess {
		archiveMsg, archiveErr := archive.Create()
		if archiveErr != nil {
			// borgFailure, not ToYaml: ToYaml renders the whole LogMessage struct, so a
			// reason the agent synthesized itself reached the controller as five empty
			// fields around one line of text. borgFailure renders "(msgid) reason", or
			// the bare reason when there is no msgid, exactly as the repo-lookup path
			// above already does.
			reason := borgFailure(archiveErr)
			projectEvent.PostEventUpdate("agent-d894f86c71d0db7b", reason)
			createErr = errors.New(reason)
			if projectEvent.EventLog.Status == "running" {
				projectEvent.EventLog.Status = "failed"
			}

			// BackupContinueOnError (backup_error_cont), never the restore flag: this
			// is the backup path, and the decision is whether to still run the BACKUP
			// hooks after a failed create. It matches the failed-preBackup branch
			// below, and it is what the mysql/postgres strategies force to true in
			// preBackup so their postBackup cleanup always runs.
			if vol.BackupContinueOnError {
				postBackup(&vol, projectEvent, repo)
			}
		} else {
			// Success: keep the full borg archive stats in result_json for the
			// controller, but don't log the YAML at INFO — the borg layer already
			// logs a concise "Completed backup" line (archive id + duration). The
			// full response is only worth logging on failure (see archiveErr above).
			projectEvent.Record(archiveMsg.ToYaml())

			// A benign warning is a success with something the customer should see: borg
			// wrote the archive, but a file changed while it was being read, so that
			// file's copy in the archive may be a torn read. Set, not Record, so it is
			// its own key in result_json next to last_backup rather than a line buried
			// in the accumulated output of a task that completed green.
			if archive.Warning != nil {
				projectEvent.Set("backup_warning", borgFailure(archive.Warning))
			}

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
	//
	// createErr is returned rather than nil so result_json.error carries borg's own
	// diagnosis. It changes the error TEXT only, not how the task is finalized:
	// runner.go already synthesizes errors.New("task reported failure") whenever
	// EventLog.Status is failed, and job/worker.go already derives TaskFailed from a
	// non-nil error either way. It is nil on the preBackup-failure path, which keeps
	// its existing reporting (preBackup posts its own messages).
	if !backupSucceeded {
		return createErr
	}

	projectEvent.Set("last_backup", time.Now().Unix())
	return nil
}
