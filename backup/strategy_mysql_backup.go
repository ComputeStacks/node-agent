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

// mysqlAccessDeniedExit is the exit code isMysqlReady's script uses for a rejected password.
// It must be one bash never returns itself: 2 is a syntax error, 126/127 a missing command.
const mysqlAccessDeniedExit = 42

func preBackupMysql(vol *types.Volume, event *progress) bool {

	// First, locate a running container
	cli, err := client.NewClientWithOpts(client.WithVersion(viper.GetString("docker.version")))
	if err != nil {
		backupLogger().Warn("Skipping mysql backup due to docker error", "error", err.Error())
		return false
	}
	mysqlMaster, findMysqlErr := loadMysqlMaster(cli, strconv.Itoa(vol.ServiceID), event, false)

	if findMysqlErr != nil {
		return false
	}

	/**
	We build and run the image like a normal container, rather than a "one-shot" command, so that we can re-use our existing
	container exec code and capture logging back to CS.
	*/
	var container *containermgr.Container
	if mysqlMaster.Variant == "mariadb" {
		container = mysqlMaster.Container
	} else {
		var err error
		container, err = buildBackupAgent(cli, mysqlMaster)
		defer container.Stop()

		if err != nil {
			backupLogger().Warn("Failed to create backup container", "error", err.Error())
			event.PostEventUpdate("agent-45b31fb5d2814cd8", "Failed to create backup container: "+err.Error())
			return false
		}
	}

	isReadyResult := isMysqlReady(container, mysqlMaster, event)

	if !isReadyResult {
		return false
	}

	backupMysqlResult := backupMysql(container, mysqlMaster, event)

	if !backupMysqlResult {
		return false
	}

	return prepareMysqlBackup(container, mysqlMaster, event)
}

func postBackupMysql(event *progress, repo *borg.Repository) bool {

	var postBackupCmd []string
	var execCmd []string
	postBackupCmd = append(postBackupCmd, "rm -rf", "/mnt/data/backups")
	execCmd = append(execCmd, "sh", "-c", strings.Join(postBackupCmd, " "))

	exitCode, out, err := repo.Container.Exec(execCmd)

	if err != nil {
		event.PostEventUpdate("agent-eb2b4ef10b08d3d5", withOutput(err.Error(), out))
		return false
	}
	if exitCode > 0 {
		event.PostEventUpdate("agent-eb2b4ef10b08d3d5", withOutput("postBackupMysql cleanup returned a non-zero exit code", out))
		return false
	}

	return true
}

/*
	MySQL Backup Commands
*/

// Determine if MariaDB / MySQL is ready to perform a backup
func isMysqlReady(backupContainer *containermgr.Container, mysqlMaster *MysqlInstance, event *progress) bool {
	var readyCmd []string
	var execCmd []string
	mysqlBinary := "mysql"

	if mysqlMaster.Variant == "mariadb" {
		if mysqlMaster.Version.Major > 10 {
			mysqlBinary = "mariadb"
		}
	}

	// The root password travels in MYSQL_PWD, never on the command line, so no shell quoting
	// can mangle it and no error output can echo it. A rejected password (ERROR 1045) exits
	// mysqlAccessDeniedExit at once rather than retrying: it will not fix itself.
	readyCmd = append(readyCmd, "counter=0; while ! out=$(", mysqlBinary, "-h", mysqlMaster.IPAddress, "-uroot", "-e \"STATUS;\"", "2>&1); do case \"$out\" in *\"ERROR 1045\"*) echo \"$out\"; exit "+strconv.Itoa(mysqlAccessDeniedExit)+";; esac; counter=$((counter+1)); if [ $counter == 11 ]; then echo \"$out\"; echo 'Failed to connect'; exit 1; fi; echo \"Connection attempt $counter\"; sleep 5; done;")
	readyCmd = append(readyCmd, "if [ -d "+mysqlMaster.DataPath+"/backups ]; then rm -rf "+mysqlMaster.DataPath+"/backups; fi") // Ensure the backups dir does not exist, otherwise backups will fail.

	execCmd = append(execCmd, "bash", "-c", strings.Join(readyCmd, " "))

	exitCode, out, err := backupContainer.ExecEnv(execCmd, []string{"MYSQL_PWD=" + mysqlMaster.RootPassword})

	if err != nil {
		backupLogger().Warn("Failed to create backup container exec", "error", err.Error())
		event.PostEventUpdate("agent-a3171394f4f28d20", withOutput("Failed run isMysqlReady check command: "+err.Error(), out))
		return false
	}

	if exitCode == mysqlAccessDeniedExit {
		backupLogger().Warn("MySQL rejected the root password", "service", mysqlMaster.ServiceID)
		event.PostEventUpdate("agent-mysql-access-denied", withOutput("Backup failed: the database rejected the root password configured for this service (MYSQL_ROOT_PASSWORD or MARIADB_ROOT_PASSWORD). If the root password was changed inside the database, update the service's setting to match.", out))
		return false
	}

	if exitCode > 0 {
		backupLogger().Warn("Failed to run isMysqlReady command", "exitCode", exitCode)
		event.PostEventUpdate("agent-b3dae7aba0783df4", withOutput("Failed to complete preBackupMysql Job. Halted during isMysqlReady.", out))
		return false
	}

	return true
}

func backupMysql(backupContainer *containermgr.Container, mysqlMaster *MysqlInstance, event *progress) bool {
	var backupCmd []string

	backupBinary := "xtrabackup"

	if mysqlMaster.Variant == "mariadb" || mysqlMaster.Variant == "bitnami-mariadb" {
		if mysqlMaster.Version.Major > 10 {
			backupBinary = "mariadb-backup"
		} else {
			backupBinary = "mariabackup"
		}
	}

	mariaLongTimeout := viper.GetString("mariadb.long_queries.timeout")
	mariaLongType := viper.GetString("mariadb.long_queries.query_type")
	mariaWaitQueryType := viper.GetString("mariadb.lock_wait.query_type")
	mariaWaitTimeout := viper.GetString("mariadb.lock_wait.timeout")

	backupCmd = append(backupCmd, backupBinary, "--backup", "--datadir="+mysqlMaster.DataPath, "--port=3306")
	backupCmd = append(backupCmd, "--target-dir="+mysqlMaster.DataPath+"/backups")
	backupCmd = append(backupCmd, "--user="+mysqlMaster.Username)
	backupCmd = append(backupCmd, "--password="+mysqlMaster.Password)
	backupCmd = append(backupCmd, "--host="+mysqlMaster.IPAddress)

	if mysqlMaster.Variant == "mariadb" {
		backupCmd = append(backupCmd, "--ftwrl-wait-query-type="+mariaWaitQueryType)
		backupCmd = append(backupCmd, "--ftwrl-wait-timeout="+mariaWaitTimeout)
		backupCmd = append(backupCmd, "--kill-long-query-type="+mariaLongType)
		backupCmd = append(backupCmd, "--kill-long-queries-timeout="+mariaLongTimeout)
	}

	exitCode, out, err := backupContainer.Exec(backupCmd)

	if err != nil {
		backupLogger().Warn("Failed to run backupMysql", "error", err.Error())
		event.PostEventUpdate("agent-2d2e814ec98a57d4", withOutput("Failed to run backup mysql: "+err.Error(), out))
		return false
	}

	if exitCode > 0 {
		backupLogger().Warn("Failed to run backupMysql Job", "exitCode", exitCode)
		event.PostEventUpdate("agent-40840727428848c7", withOutput("Failed to run preBackupMysql Job due to backupMysql error.", out))
		return false
	}

	return true
}

func prepareMysqlBackup(backupContainer *containermgr.Container, mysqlMaster *MysqlInstance, event *progress) bool {
	var backupPrepareCmd []string

	backupBinary := "xtrabackup"

	if mysqlMaster.Variant == "mariadb" || mysqlMaster.Variant == "bitnami-mariadb" {
		if mysqlMaster.Version.Major > 10 {
			backupBinary = "mariadb-backup"
		} else {
			backupBinary = "mariabackup"
		}
	}

	backupPrepareCmd = append(backupPrepareCmd, backupBinary, "--prepare", "--target-dir="+mysqlMaster.DataPath+"/backups")

	exitCode, out, err := backupContainer.Exec(backupPrepareCmd)

	if err != nil {
		backupLogger().Warn("Failed to run prepareMysqlBackup", "error", err.Error())
		event.PostEventUpdate("agent-7dfa6f8829fb8c7e", withOutput("Failed to prepare mysql backup: "+err.Error(), out))
		return false
	}

	if exitCode > 0 {
		backupLogger().Warn("Failed to run backupMysql Job", "exitCode", exitCode, "commands", strings.Join(backupPrepareCmd, " "))
		event.PostEventUpdate("agent-2fe3bce7adbb0f58", withOutput("Failed to run prepareMysqlBackup Job due to backupMysql error.", out))
		return false
	}

	return true
}
