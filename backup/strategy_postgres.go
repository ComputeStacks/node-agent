package backup

import (
	"cs-agent/backup/borg"
	"cs-agent/containermgr"
	"cs-agent/types"
	"strconv"

	"github.com/docker/docker/client"
	"github.com/spf13/viper"
)

func preBackupPostgres(vol *types.Volume, event *progress) (preBackupPostgresSuccess bool) {
	cli, err := client.NewClientWithOpts(client.WithVersion(viper.GetString("docker.version")))
	if err != nil {
		backupLogger().Warn("Docker error preBackupPostgres", "error", err.Error())
		event.PostEventUpdate("agent-88a614c7c22772e5", "Fatal error connecting to docker for preBackupPostgres Job")
		return false
	}
	c, err := containermgr.FindByService(cli, strconv.Itoa(vol.ServiceID), false)

	// If container is offline, then we don't need to do anything! allow normal file-level backup to continue
	if err != nil {
		return true
	}
	exitCode, out, err := c.Exec([]string{"psql", "-U", "postgres", "-c", "checkpoint;"})
	if err != nil {
		backupLogger().Warn("Failed to run preBackupPostgres exec", "error", err.Error())
		event.PostEventUpdate("agent-dc7bfd8aa3bb1042", withOutput("Failed to run preBackupPostgres Job: "+err.Error(), out))
		return false
	}

	if exitCode > 0 {
		backupLogger().Warn("Failed to run preBackupPostgres Job", "exitCode", exitCode)
		event.PostEventUpdate("agent-dc7bfd8aa3bb1042", withOutput("Failed to run preBackupPostgres Job", out))
		return false
	}

	return true
}

func postBackupPostgres(event *progress, repo *borg.Repository) bool {
	return true
}

// preRestorePostgres has no strategy-specific work left to do.
//
// It briefly stopped the service's containers, because preRestore had started taking the
// /mnt/data snapshot ahead of Restore's own stop loop and postgres was getting its data
// directory moved out from under a running server. That was never a postgres problem: the
// snapshot is a cross-device copy followed by an unlink for every strategy, so every
// strategy needed the stop. preRestore does it once now, for all of them — see
// stopServiceContainers, which also records the writers that stop does not reach and the
// fact that Restore, not this hook, starts the containers again.
//
// It is kept, with its case in preRestore's switch, as the place a genuinely
// postgres-specific pre-restore step would go — and because that switch runs before the
// stop, so a hook there can still reach a live server the way preBackupPostgres above does
// when it issues `checkpoint;`. postBackupPostgres, postRestorePostgres and
// rollbackRestorePostgres are the same shape.
func preRestorePostgres(vol *types.Volume, event *progress, repo *borg.Repository) (preRestorePostgresSuccess bool) {
	return true
}

func postRestorePostgres(event *progress, repo *borg.Repository) bool {
	return true
}

func rollbackRestorePostgres(event *progress, repo *borg.Repository) bool {
	return true
}
