package backup

import (
	"cs-agent/backup/borg"
	"cs-agent/types"
	"strings"
)

// preRestoreMysql has no strategy-specific work left to do.
//
// It once snapshotted /mnt/data and then, later, stopped the database. Both moved out and
// for the same reason: neither was mysql's business. preRestore takes the snapshot once for
// every strategy, and it now stops the service's containers once for every strategy too,
// because an ordinary application volume needed that exactly as much as a database did —
// see stopServiceContainers for what that stop does and does not cover.
//
// It is kept, with its case in preRestore's switch, as the place a genuinely mysql-specific
// pre-restore step would go — and because a hook in that switch still runs while the service
// is up, which a hook below the stop would not. postBackupPostgres, postRestorePostgres and
// rollbackRestorePostgres are the same shape.
func preRestoreMysql(vol *types.Volume, event *progress, repo *borg.Repository) (preRestoreMysqlSuccess bool) {
	return true
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
