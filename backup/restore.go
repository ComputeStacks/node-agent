package backup

import (
	"context"
	"cs-agent/backup/borg"
	"cs-agent/containermgr"
	"cs-agent/store"
	"cs-agent/types"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/docker/docker/client"
	"github.com/getsentry/sentry-go"
	"github.com/spf13/viper"
)

// strategyIgnoresFilePaths reports the strategies whose restore has always discarded
// file_paths. Their extract has to put back the whole datadir — the promote step that
// follows it in postRestore needs all of it — so restore.go zeroed the list before
// calling into borg, and it did that for every request. Refusing them instead would turn
// a restore that succeeds today into a new failure for no gain, since the dangerous shape
// is precisely the one where the extract WOULD honor the paths.
func strategyIgnoresFilePaths(strategy string) bool {
	switch strategy {
	case "mysql", "mariadb", "postgres":
		return true
	}
	return false
}

// maxRestoreDetailRecords bounds how many of borg's records reach the task result.
//
// Without --error borg emits one record per item it could not extract (see
// borg.extractCommand), so a restore that fails on a whole directory tree produces one
// line per file. Each MESSAGE is already capped by the borg layer at maxReasonBytes; this
// caps the COUNT, because result_json is a control.db column the controller renders, not a
// log file. Twenty is enough to see the pattern — which paths, which errno — and the
// trailing count says how much was left out rather than pretending there was no more.
const maxRestoreDetailRecords = 20

// recordRestoreDetail appends borg's per-item records to the task output. This is the only
// place the individual failed paths are reported: the reason carries one record (the one
// extractFailure picked), and on a partial extract the whole list is the answer to "what
// did not come back".
//
// Every record, including the one already quoted as the reason — so the list is complete on
// its own and an operator reading it is not left wondering whether the reason's file is in
// it. The cost is one repeated line at the top of the output.
//
// Record, not PostEventUpdate: these are potentially maxRestoreDetailRecords lines of
// detail behind a reason that has already been posted at INFO, and PostEventUpdate would
// log every one of them to the node log again.
func recordRestoreDetail(projectEvent *progress, records []borg.LogMessage) {
	for i := range records {
		if i == maxRestoreDetailRecords {
			projectEvent.Record(fmt.Sprintf("and %d more", len(records)-maxRestoreDetailRecords))
			return
		}
		projectEvent.Record(borgFailure(&records[i]))
	}
}

func Restore(ctx context.Context, st *store.Store, task store.Task, projectEvent *progress) error {
	// No handler-level sentry.Recover(): let a panic reach the worker terminal
	// guard so a crashed restore is FAILED (never a false "completed").
	params := parseParams(task)

	destData, found, destVolErr := st.GetVolume(ctx, task.Volume)
	if destVolErr != nil {
		sentry.CaptureException(destVolErr)
		return destVolErr
	}
	if !found {
		// n1: a controller-POSTed restore against a stale/unknown target is a
		// truthful failure, not a false "completed".
		backupLogger().Warn("Restore failed: unknown destination volume", "volume", task.Volume, "source_volume", params.SourceVolume)
		projectEvent.EventLog.Status = "failed"
		reason := "destination volume not found"
		projectEvent.PostEventUpdate("agent-restore-unknown-dest", reason)
		// Every failure branch below returns its reason as well as posting it, so
		// result_json.error carries the diagnosis instead of runner.go's synthesized
		// "task reported failure" — the same change createErr made on the backup path.
		// It changes the error TEXT only, not how the task is finalized: RunTask already
		// turns a "failed" EventLog.Status into an error, and job/worker.go derives
		// TaskFailed from a non-nil error either way.
		return errors.New(reason)
	}
	srcData, found, err := st.GetVolume(ctx, params.SourceVolume)
	if err != nil {
		sentry.CaptureException(err)
		return err
	}
	if !found {
		backupLogger().Warn("Restore failed: unknown source volume", "volume", task.Volume, "source_volume", params.SourceVolume)
		projectEvent.EventLog.Status = "failed"
		reason := "source volume not found"
		projectEvent.PostEventUpdate("agent-restore-unknown-src", reason)
		return errors.New(reason)
	}
	backupLogger().Info("Performing volume restore", "volume", task.Volume, "source_volume", params.SourceVolume)

	// Load Source Volume
	vol, err := types.LoadVolume(srcData.Config)
	if err != nil {
		backupLogger().Warn("Fatal error parsing source volume", "source_volume", params.SourceVolume, "error", err.Error())
		sentry.CaptureException(err)
		return err
	}

	// Load Destination Volume
	destVol, err := types.LoadVolume(destData.Config)
	if err != nil {
		backupLogger().Warn("Fatal error parsing destination volume", "volume", task.Volume, "error", err.Error())
		sentry.CaptureException(err)
		return err
	}

	// Start restore
	backupLogger().Info("Preparing to restore volume", "volume", task.Volume, "source_volume", params.SourceVolume)

	if task.Archive == "" {
		backupLogger().Warn("Error restoring volume, missing archive name", "volume", task.Volume, "source_volume", params.SourceVolume)
		projectEvent.EventLog.Status = "failed"
		reason := "Failed to restore volume due to missing backup name."
		projectEvent.PostEventUpdate("agent-548b1d752057add0", reason)
		return errors.New(reason)
	}

	// file_paths is refused, and it is refused HERE: the source volume is loaded, so the
	// strategy is known, and nothing has been moved — no repository, no backup container,
	// and preRestore (which takes the snapshot) is still far below.
	//
	// A named-path restore does not restore part of a volume, it destroys the rest of it.
	// takeRestoreSnapshot moves the WHOLE of /mnt/data into /root/.snapshot inside the
	// backup container, the extract then puts back only the named paths, and a
	// fully-matching include-path extract exits 0 — so postRestore runs, the task
	// completes, and the deferred repo.StopContainer() reaps the AutoRemove container with
	// everything that was not named still inside it. Failing loudly before the move is the
	// only outcome here that keeps the volume.
	//
	// The borg layer keeps the capability and its tests (borg.Archive.Restore still takes
	// filePaths); refusing it is a policy, and it belongs to the orchestration that owns
	// the snapshot.
	if len(params.FilePaths) > 0 && !strategyIgnoresFilePaths(vol.Strategy) {
		backupLogger().Warn("Restore failed: file_paths is not supported", "volume", task.Volume, "source_volume", params.SourceVolume, "strategy", vol.Strategy, "file_paths", len(params.FilePaths))
		projectEvent.EventLog.Status = "failed"
		reason := "restoring individual file paths is not supported; a restore replaces the whole volume"
		projectEvent.PostEventUpdate("agent-restore-file-paths-unsupported", reason)
		return errors.New(reason)
	}

	// vol is the SOURCE and it is the second argument, because the repository being read
	// belongs to the source volume — destVol only receives the extract. A restore never
	// creates a repository: whatever comes back here is final, and the two branches below
	// differ only in the id the failure is reported under.
	repo, findRepoErr := borg.FindRepository(st, &destVol, &vol)

	if findRepoErr != nil {
		// Can't restore from a repository that isn't there. The same-volume case reports the
		// identical outcome as the branch below it, under its own id: a restore of a volume
		// onto itself and a restore from another volume's repository are different things to
		// an operator reading the task result, and the ids are what downstream consumers
		// have to tell them apart with.
		if destVol.Name == vol.Name {
			backupLogger().Warn("Error Restoring volume", "volume", task.Volume, "source_volume", params.SourceVolume, "error", findRepoErr.Message)
			projectEvent.EventLog.Status = "failed"
			// borgFailure, not ToYaml: ToYaml renders the whole LogMessage struct, so a
			// one-line reason reached the controller as five empty fields around it.
			// borgFailure renders "(msgid) reason", as the backup path and every other
			// borg failure site already do.
			reason := borgFailure(findRepoErr)
			projectEvent.PostEventUpdate("agent-81023b3bc0541171", reason)
			return errors.New(reason)
		}

		// No auto-init here, deliberately. This used to run repo.Setup on an SSH backend
		// when borg said Repository.DoesNotExist, on the belief that the missing repository
		// was the DESTINATION's — which it never was, and now demonstrably is not: the
		// repository FindRepository opens belongs to the source, so that verdict means the
		// volume being restored FROM has no repository and there is nothing to extract. A
		// `borg init` produced an empty repository, the archive lookup that followed failed
		// anyway, and on the way it created a repository directory on the backup server for
		// a volume that had no backups. Restore reads; only backup.Perform creates.
		backupLogger().Warn("Error Restoring volume", "volume", task.Volume, "source_volume", params.SourceVolume, "error", findRepoErr.Message)
		projectEvent.EventLog.Status = "failed"
		reason := borgFailure(findRepoErr)
		projectEvent.PostEventUpdate("agent-2e2a3156b8e2ffd2", reason)
		return errors.New(reason)
	}

	defer repo.StopContainer()

	archive, findArchiveErr := repo.FindArchive(task.Archive)

	if findArchiveErr != nil {
		backupLogger().Warn("Error Restoring volume", "volume", task.Volume, "source_volume", params.SourceVolume, "error", findArchiveErr.Message)
		projectEvent.EventLog.Status = "failed"
		reason := borgFailure(findArchiveErr)
		projectEvent.PostEventUpdate("agent-7d32bd2230b39408", reason)
		repo.StopContainer()
		return errors.New(reason)
	}

	cli, restoreDockerErr := client.NewClientWithOpts(client.WithVersion(viper.GetString("docker.version")))
	if restoreDockerErr != nil {
		backupLogger().Warn("Failed to connect to docker", "error", restoreDockerErr.Error(), "function", "Restore")
		projectEvent.EventLog.Status = "failed"
		projectEvent.PostEventUpdate("agent-05297a0a0438a5bf", restoreDockerErr.Error())
		repo.StopContainer()
		return restoreDockerErr
	}
	containers, findAllErr := containermgr.FindAllByService(cli, strconv.Itoa(destVol.ServiceID), true)

	if findAllErr != nil {
		backupLogger().Warn("Failed to retrieve containers", "error", findAllErr.Error(), "function", "Restore")
		projectEvent.EventLog.Status = "failed"
		projectEvent.PostEventUpdate("agent-dc1ec275fcf91a6c", findAllErr.Error())
		repo.StopContainer()
		return findAllErr
	}

	backupLogger().Debug("Running PreRestore hook", "volume", vol.Name)

	preRestoreSuccess := preRestore(&destVol, projectEvent, repo)

	if !preRestoreSuccess {
		repo.StopContainer()
		backupLogger().Warn("Failed to restore volume", "volume", vol.Name, "archive", archive.Name, "error", "PreRestore hook failed.")
		projectEvent.EventLog.Status = "failed"
		// No PostEventUpdate: preRestore and the hooks it calls post their own detail
		// (rollbackRestoreSnapshot, ServiceExec, the per-strategy hooks), so this is the
		// one reason the task result would otherwise be missing.
		return errors.New("pre-restore hook failed")
	}

	// restoreFailure carries the reason out to the return for the three branches below
	// that cannot return where they discover it: each of them has to roll the snapshot
	// back and restart the service's containers first. Everything above returns its reason
	// directly.
	var restoreFailure error

	// A confirmation pass, not the stop that protects the volume. preRestore has already
	// stopped this same set of containers — it has to, because it moves /mnt/data aside and
	// that move is a cross-device copy, not a rename — so this loop is normally re-stopping
	// containers that are already down, which Stop() treats as a no-op.
	//
	// It is not redundant for that. It still catches a container that came back up in
	// between (the orchestrator rescheduling one, an operator starting the service), and its
	// failure path is the one that HAS somewhere to fall back to: the snapshot exists by the
	// time control reaches here, so failedToStop below can call rollbackRestore and put the
	// volume back. A failure inside preRestore's stop cannot and does not need to — nothing
	// has been moved yet at that point.
	failedToStop := false
	for _, c := range containers {
		backupLogger().Debug("Start Restore: Stop Container", "volume", vol.Name, "container", c.ID)
		if !c.Stop() {
			failedToStop = true
			projectEvent.PostEventUpdate("agent-f3ccb16e88d10a80", "Failed to stop container, halting restore process.")
			break
		}
	}

	if failedToStop {
		projectEvent.EventLog.Status = "failed"
		// Assigned before the rollback, for the same reason the status is: the reason the
		// restore failed does not depend on how the rollback goes. The rollback-failure
		// branch below then overwrites it, because that outranks this.
		restoreFailure = errors.New("Failed to stop container, halting restore process.")
		// Only a LOST SNAPSHOT escalates the reported reason; see rollbackOutcome. The
		// three call sites below spell this switch out rather than sharing a helper,
		// because rollbackRestore has to be called from Restore's own body for the AST
		// guards in restore_status_test.go to see the branch it sits in.
		switch rollbackRestore(&destVol, projectEvent, repo) {
		case rollbackComplete:
			backupLogger().Info("Completed restore rollback", "volume", destVol.Name)
		case rollbackSnapshotLost:
			projectEvent.PostEventUpdate("agent-0b33976078a50679", "Restore rollback failed.")
			restoreFailure = rollbackFailure(restoreFailure)
		case rollbackCleanupFailed:
			projectEvent.PostEventUpdate("agent-restore-rollback-cleanup", "The volume was put back, but a step after the rollback failed.")
		}
	} else {
		backupLogger().Info("Restoring volume", "source_volume", vol.Name, "volume", destVol.Name, "archive", archive.Name)
		// nil, never params.FilePaths: a request carrying file_paths was refused above,
		// before the snapshot was taken, so the whole archive is the only restore there is.
		records, restoreErr := archive.Restore(nil)
		if restoreErr != nil {
			reason := borgFailure(restoreErr)
			projectEvent.PostEventUpdate("agent-6dfe4e7b471fdd4c", reason)
			// The reason can only carry one record. On a partial extract borg emits one
			// per item it could not write, and those name the paths that did not come
			// back, so they go into the task output alongside it.
			recordRestoreDetail(projectEvent, records)
			projectEvent.EventLog.Status = "failed"
			restoreFailure = errors.New(reason)
			backupLogger().Warn("Failed to restore volume", "source_volume", vol.Name, "volume", destVol.Name, "archive", archive.Name, "error", restoreErr.Message)
			switch rollbackRestore(&destVol, projectEvent, repo) {
			case rollbackComplete:
				backupLogger().Info("Completed restore rollback", "volume", destVol.Name)
			case rollbackSnapshotLost:
				projectEvent.PostEventUpdate("agent-0b33976078a50679", "Restore rollback failed.")
				restoreFailure = rollbackFailure(restoreFailure)
			case rollbackCleanupFailed:
				projectEvent.PostEventUpdate("agent-restore-rollback-cleanup", "The volume was put back, but a step after the rollback failed.")
			}
		} else {
			if !postRestore(&destVol, projectEvent, repo) {
				// A rolled-back restore is a FAILED restore, and saying so is this
				// line's whole job. The task's terminal status comes from
				// EventLog.Status via progress.Failed() (backup/runner.go), so with it
				// left at "running" this branch reported COMPLETED: the controller
				// recorded a successful restore — and, for a clone, a completed clone
				// job with an empty destination volume — over data that had just been
				// rolled back. Every other failure branch in Restore sets it; only this
				// one did not, and under the pre-v3.0.0 csevent stream the omission was
				// invisible because the controller watched the event rather than the
				// task result.
				//
				// It is set BEFORE the rollback so the status does not depend on how the
				// rollback goes: a rollback that succeeds still leaves the restore failed.
				projectEvent.EventLog.Status = "failed"
				reason := "postRestore failed, executing rollback."
				projectEvent.PostEventUpdate("agent-12b99684cb30d029", reason)
				restoreFailure = errors.New(reason)
				switch rollbackRestore(&destVol, projectEvent, repo) {
				case rollbackComplete:
					backupLogger().Info("Completed restore rollback", "volume", destVol.Name)
				case rollbackSnapshotLost:
					projectEvent.PostEventUpdate("agent-b9f3171f4182ee92", "Restore rollback failed.")
					restoreFailure = rollbackFailure(restoreFailure)
				case rollbackCleanupFailed:
					projectEvent.PostEventUpdate("agent-restore-rollback-cleanup", "The volume was put back, but a step after the rollback failed.")
				}
			} else {
				// rc 0 with records is a restore that stands but logged something — borg's
				// exit code is what settles that (see borg.Archive.Restore). Set, not
				// Record, so it is its own result_json key rather than a line buried in the
				// output of a task that completed green, exactly as backup_warning is on the
				// create path.
				//
				// Published HERE, after postRestore, and not next to the extract that
				// produced it: this is the only branch that completes green. Setting it
				// earlier put a "warning about your restore" field on the result of a task
				// that then failed and rolled the volume back, which reads as a restore that
				// half worked.
				if len(records) > 0 {
					projectEvent.Set("restore_warning", borgFailure(&records[0]))
				}

				// Success: log a concise completion line (symmetric with the borg
				// layer's "Completed backup"). The controller learns success from the
				// task's terminal status; this is the operator-facing node-log signal,
				// which was previously absent on a successful restore.
				backupLogger().Info("Completed volume restore", "volume", destVol.Name, "source_volume", vol.Name, "archive", archive.Name)
			}
		}
	}

	for _, c := range containers {
		backupLogger().Debug("Finalize Restore: Start Container", "volume", destVol.Name, "container", c.ID)
		if !c.Start() {
			backupLogger().Warn("Failed to start container", "function", "Restore")
			projectEvent.PostEventUpdate("agent-a15b6d18583615a1", "Failed to start container")
		}
		time.Sleep(time.Second) // give each container a second to boot to avoid thrashing the disk
	}

	return restoreFailure
}

// rollbackFailure wraps the reason a restore failed with the fact that the volume's
// contents were not put back, which is the one thing in a restore's result that needs a
// human immediately — the deferred repo.StopContainer() is moments away from reaping the
// AutoRemove backup container, and /root/.snapshot lives inside it.
//
// It names the state rather than hedging, because it is reached for rollbackSnapshotLost
// alone. A rollback whose put-back worked and whose cleanup afterwards did not is reported
// where it happens and does NOT come through here: for any volume with a PostRestore
// command that is the outcome of every rollback (see rollbackOutcome), so escalating it
// would make this message the routine one and train an operator to ignore it.
//
// The original reason is wrapped rather than replaced, so the result still says what failed
// the restore in the first place.
func rollbackFailure(cause error) error {
	return fmt.Errorf("restore rollback failed, the volume's contents were not put back: %w", cause)
}
