package backup

import (
	"context"
	"cs-agent/backup/borg"
	"cs-agent/containermgr"
	"cs-agent/store"
	"cs-agent/types"
	"strconv"
	"time"

	"github.com/docker/docker/client"
	"github.com/getsentry/sentry-go"
	"github.com/spf13/viper"
)

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
		projectEvent.PostEventUpdate("agent-restore-unknown-dest", "destination volume not found")
		return nil
	}
	srcData, found, err := st.GetVolume(ctx, params.SourceVolume)
	if err != nil {
		sentry.CaptureException(err)
		return err
	}
	if !found {
		backupLogger().Warn("Restore failed: unknown source volume", "volume", task.Volume, "source_volume", params.SourceVolume)
		projectEvent.EventLog.Status = "failed"
		projectEvent.PostEventUpdate("agent-restore-unknown-src", "source volume not found")
		return nil
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
		projectEvent.PostEventUpdate("agent-548b1d752057add0", "Failed to restore volume due to missing backup name.")
		return nil
	}

	repo, findRepoErr := borg.FindRepository(st, &destVol, &vol)

	if findRepoErr != nil {
		// Can't restore from an empty repository. (A same-volume restore — source
		// == destination — has no other repo to fall back to, so fail rather than
		// try to create one. This was historically a pointer compare that never
		// fired; it is a value compare now.)
		if destVol.Name == vol.Name {
			backupLogger().Warn("Error Restoring volume", "volume", task.Volume, "source_volume", params.SourceVolume, "error", findRepoErr.Message)
			projectEvent.EventLog.Status = "failed"
			projectEvent.PostEventUpdate("agent-81023b3bc0541171", findRepoErr.ToYaml())
			return nil
		}

		// For SSH-backed repositories, we may need to first create the repository.
		if findRepoErr.MsgID == "Repository.DoesNotExist" && viper.GetBool("backups.borg.ssh.enabled") {
			// Empty SSH repos return 'InvalidRepository' rather than 'DoesNotExist'.
			repo = &borg.Repository{Name: vol.Name, Store: st}
			// Build backup container
			repoErr := repo.Setup(&destVol, &vol)
			if repoErr != nil {
				backupLogger().Warn("Error Setting up repo for volume restore", "volume", task.Volume, "source_volume", params.SourceVolume, "error", repoErr.Message)
				projectEvent.EventLog.Status = "failed"
				projectEvent.PostEventUpdate("agent-ea3613609e732d68", repoErr.ToYaml())
				return nil
			}
		} else {
			backupLogger().Warn("Error Restoring volume", "volume", task.Volume, "source_volume", params.SourceVolume, "error", findRepoErr.Message)
			projectEvent.EventLog.Status = "failed"
			projectEvent.PostEventUpdate("agent-2e2a3156b8e2ffd2", findRepoErr.ToYaml())
			return nil
		}
	}

	defer repo.StopContainer()

	archive, findArchiveErr := repo.FindArchive(task.Archive)

	if findArchiveErr != nil {
		backupLogger().Warn("Error Restoring volume", "volume", task.Volume, "source_volume", params.SourceVolume, "error", findArchiveErr.Message)
		projectEvent.EventLog.Status = "failed"
		projectEvent.PostEventUpdate("agent-7d32bd2230b39408", findArchiveErr.ToYaml())
		repo.StopContainer()
		return nil
	}

	cli, restoreDockerErr := client.NewClientWithOpts(client.WithVersion(viper.GetString("docker.version")))
	if restoreDockerErr != nil {
		backupLogger().Warn("Failed to connect to docker", "error", restoreDockerErr.Error(), "function", "Restore")
		projectEvent.EventLog.Status = "failed"
		projectEvent.PostEventUpdate("agent-05297a0a0438a5bf", restoreDockerErr.Error())
		repo.StopContainer()
		return nil
	}
	containers, findAllErr := containermgr.FindAllByService(cli, strconv.Itoa(destVol.ServiceID), true)

	if findAllErr != nil {
		backupLogger().Warn("Failed to retrieve containers", "error", findAllErr.Error(), "function", "Restore")
		projectEvent.EventLog.Status = "failed"
		projectEvent.PostEventUpdate("agent-dc1ec275fcf91a6c", findAllErr.Error())
		repo.StopContainer()
		return nil
	}

	backupLogger().Debug("Running PreRestore hook", "volume", vol.Name)

	preRestoreSuccess := preRestore(&destVol, projectEvent, repo)

	if !preRestoreSuccess {
		repo.StopContainer()
		backupLogger().Warn("Failed to restore volume", "volume", vol.Name, "archive", archive.Name, "error", "PreRestore hook failed.")
		projectEvent.EventLog.Status = "failed"
		return nil
	}

	// Override file paths for our custom strategies.
	filePaths := params.FilePaths

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
		if rollbackRestore(&destVol, projectEvent, repo) {
			backupLogger().Info("Completed restore rollback", "volume", destVol.Name)
		} else {
			projectEvent.PostEventUpdate("agent-0b33976078a50679", "Restore rollback failed.")
		}
	} else {
		switch vol.Strategy {
		case "mysql", "mariadb":
			filePaths = []string{}
		case "postgres":
			filePaths = []string{}
		}

		backupLogger().Info("Restoring volume", "source_volume", vol.Name, "volume", destVol.Name, "archive", archive.Name)
		restoreErr := archive.Restore(filePaths)
		if restoreErr != nil {
			projectEvent.PostEventUpdate("agent-6dfe4e7b471fdd4c", restoreErr.ToYaml())
			projectEvent.EventLog.Status = "failed"
			backupLogger().Warn("Failed to restore volume", "source_volume", vol.Name, "volume", destVol.Name, "archive", archive.Name, "error", restoreErr.Message)
			if rollbackRestore(&destVol, projectEvent, repo) {
				backupLogger().Info("Completed restore rollback", "volume", destVol.Name)
			} else {
				projectEvent.PostEventUpdate("agent-0b33976078a50679", "Restore rollback failed.")
			}
		} else {
			if !postRestore(&destVol, projectEvent, repo) {
				projectEvent.PostEventUpdate("agent-12b99684cb30d029", "postRestore failed, executing rollback.")
				if rollbackRestore(&destVol, projectEvent, repo) {
					backupLogger().Info("Completed restore rollback", "volume", destVol.Name)
				} else {
					projectEvent.PostEventUpdate("agent-b9f3171f4182ee92", "Restore rollback failed.")
				}
			} else {
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

	return nil
}
