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

// preRestorePostgres stops the database before the volume underneath it is disturbed.
//
// It used to return true and stop nothing, which was survivable only while nothing
// touched the volume before Restore's own container stop loop. preRestore now takes the
// /mnt/data snapshot, and preRestore runs ahead of that loop, so postgres was getting
// its data directory moved out from under a running server. Not a rename either: the
// snapshot lives in the backup container's filesystem rather than in the volume, so the
// move is a per-file copy followed by an unlink — a torn copy in the snapshot and an
// empty volume, with the database still writing.
//
// stopAllMysqlContainers is named after its first caller rather than after what it does:
// FindAllByService on the volume's service, Stop() on each container, and a restart of
// all of them if any refuses to stop. There is nothing mysql-specific in it. Renaming it
// would touch the mysql hooks and their comments for no change in behaviour, so the name
// stays and this comment carries the explanation.
//
// The containers stopped here are started again by Restore, which restarts the list it
// captured before any of this ran, regardless of who stopped them — the mysql strategy
// has always relied on that.
func preRestorePostgres(vol *types.Volume, event *progress, repo *borg.Repository) (preRestorePostgresSuccess bool) {
	return stopAllMysqlContainers(vol, event)
}

func postRestorePostgres(event *progress, repo *borg.Repository) bool {
	return true
}

func rollbackRestorePostgres(event *progress, repo *borg.Repository) bool {
	return true
}
