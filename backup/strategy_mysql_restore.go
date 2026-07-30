package backup

import (
	"cs-agent/backup/borg"
	"cs-agent/containermgr"
	"cs-agent/types"
	"strconv"
	"strings"

	"github.com/docker/docker/client"
	"github.com/spf13/viper"
)

// preRestoreMysql stops the database before the volume underneath it is disturbed.
//
// It no longer snapshots /mnt/data. preRestore does that for every strategy, right
// after this returns, so the snapshot is taken once by one owner instead of here and
// again inside Archive.Restore — where the second attempt ran against the directory
// this hook had already emptied, and reported the unexpanded glob as a failure.
func preRestoreMysql(vol *types.Volume, event *progress, repo *borg.Repository) (preRestoreMysqlSuccess bool) {
	return stopAllMysqlContainers(vol, event)
}

func postRestoreMysql(event *progress, repo *borg.Repository) bool {

	var postRestoreCmd []string
	var execCmd []string
	postRestoreCmd = append(postRestoreCmd, "mkdir", "-p /root/.staging")
	postRestoreCmd = append(postRestoreCmd, "&&", "mv", "/mnt/data/* /root/.staging/")
	postRestoreCmd = append(postRestoreCmd, "&&", "rm", "-rf /mnt/data/*")
	postRestoreCmd = append(postRestoreCmd, "&&", "mv", "/root/.staging/backups/* /mnt/data/")

	execCmd = append(execCmd, "sh", "-c", strings.Join(postRestoreCmd, " "))

	exitCode, out, err := repo.Container.Exec(execCmd)

	if err != nil {
		backupLogger().Warn("Failed to execute mysql cleanup on restore", "error", err.Error())
		event.PostEventUpdate("agent-c24b0abcd88acdff", withOutput(err.Error(), out))
		return false
	}
	if exitCode > 0 {
		event.PostEventUpdate("agent-c24b0abcd88acdff", withOutput("postRestoreMysql cleanup returned a non-zero exit code", out))
		return false
	}

	return true

}

// rollbackRestoreMysql removes the SQL dump directory that a mysql-strategy backup
// leaves in the volume.
//
// rollbackRestore has already emptied /mnt/data and moved the snapshot back by the time
// this runs, so the `rm -rf /mnt/data/*` and the `mv` that used to be here are gone.
// Running them here as well was the data-loss path: the borg layer's own rollback moved
// the snapshot back into /mnt/data, and then this function's rm deleted it while its
// unguarded mv failed against the now-empty snapshot, leaving the volume empty and the
// snapshot empty in an AutoRemove container.
func rollbackRestoreMysql(event *progress, repo *borg.Repository) bool {

	var rollbackCmd []string
	var execCmd []string
	rollbackCmd = append(rollbackCmd, "rm", "-rf /mnt/data/backups")

	execCmd = append(execCmd, "sh", "-c", strings.Join(rollbackCmd, " "))

	exitCode, out, err := repo.Container.Exec(execCmd)

	if err != nil {
		backupLogger().Warn("Failed to store database backup", "error", err.Error())
		event.PostEventUpdate("agent-af1b0badd5d9b9f6", withOutput(err.Error(), out))
		return false
	}
	if exitCode > 0 {
		event.PostEventUpdate("agent-af1b0badd5d9b9f6", withOutput("rollbackRestoreMysql returned a non-zero exit code", out))
		return false
	}

	return true

}

func stopAllMysqlContainers(vol *types.Volume, event *progress) bool {
	cli, cliErr := client.NewClientWithOpts(client.WithVersion(viper.GetString("docker.version")))
	if cliErr != nil {
		backupLogger().Warn("Failed to connect to docker", "error", cliErr.Error(), "function", "stopAllMysqlContainers")
		event.PostEventUpdate("agent-45a73ed06ea34e25", cliErr.Error())
		return false
	}
	containers, findAllErr := containermgr.FindAllByService(cli, strconv.Itoa(vol.ServiceID), true)
	if findAllErr != nil {
		backupLogger().Warn("Failed to retrieve containers", "error", findAllErr.Error(), "function", "stopAllMysqlContainers")
		event.PostEventUpdate("agent-0248a778f49a1eb4", findAllErr.Error())
		return false
	}
	failedToStop := false
	for _, c := range containers {
		if !c.Stop() {
			failedToStop = true
		}
	}
	if failedToStop {
		event.PostEventUpdate("agent-6becd55bb6a584de", "Failed to stop some containers, unable to restore.")
		for _, c := range containers {
			_ = c.Start() // Ignore containers that fail to start
		}
		return false
	}
	return true
}
