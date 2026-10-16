package backup

import (
	"cs-agent/backup/borg"
	"cs-agent/containermgr"
	"cs-agent/types"
	"strconv"
	"strings"
	"time"

	"github.com/coreos/go-semver/semver"
	"github.com/docker/docker/client"
	"github.com/spf13/viper"
)

// mysqlAccessDeniedExit is the exit code isMysqlReady's script uses for a rejected password.
// It must be one bash never returns itself: 2 is a syntax error, 126/127 a missing command.
const mysqlAccessDeniedExit = 42

// mysqlClientBinary is the name of the mysql command-line client in the container that
// runs the backup: MariaDB 11 dropped the `mysql` name.
func mysqlClientBinary(mysqlMaster *MysqlInstance) string {
	if mysqlMaster.Variant == "mariadb" && mysqlMaster.Version.Major > 10 {
		return "mariadb"
	}
	return "mysql"
}

// mysqlReadyScript is the bash script isMysqlReady runs: wait up to ~50s for the server to
// accept a root login (password from MYSQL_PWD), then clear any stale backups dir. A refused
// login (ERROR 1045) exits mysqlAccessDeniedExit at once rather than retrying: it will not fix
// itself. Other failures retry, and the last one's output is echoed.
func mysqlReadyScript(mysqlBinary, host, dataPath string) string {
	return "counter=0; while ! out=$(" + mysqlBinary + " -h " + host + " -uroot -e \"STATUS;\" 2>&1); do " +
		"case \"$out\" in *\"ERROR 1045\"*) echo \"$out\"; exit " + strconv.Itoa(mysqlAccessDeniedExit) + ";; esac; " +
		"counter=$((counter+1)); if [ $counter == 11 ]; then echo \"$out\"; echo 'Failed to connect'; exit 1; fi; " +
		"echo \"Connection attempt $counter\"; sleep 5; done; " +
		"if [ -d " + dataPath + "/backups ]; then rm -rf " + dataPath + "/backups; fi" // Ensure the backups dir does not exist, otherwise backups will fail.
}

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
	mysqlBinary := mysqlClientBinary(mysqlMaster)

	// The root password travels in MYSQL_PWD, never on the command line, so no shell quoting
	// can mangle it and no error output can echo it.
	execCmd := []string{"bash", "-c", mysqlReadyScript(mysqlBinary, mysqlMaster.IPAddress, mysqlMaster.DataPath)}

	exitCode, out, err := backupContainer.ExecEnv(execCmd, []string{"MYSQL_PWD=" + mysqlMaster.RootPassword})

	if err != nil {
		backupLogger().Warn("Failed to create backup container exec", "error", err.Error())
		event.PostEventUpdate("agent-a3171394f4f28d20", withOutput("Failed run isMysqlReady check command: "+err.Error(), out))
		return false
	}

	if exitCode == mysqlAccessDeniedExit {
		backupLogger().Warn("MySQL refused the root login", "service", mysqlMaster.ServiceID)
		msg := "Backup failed: the database refused the root login. If the root password was changed inside the database, update the service's root password setting (MYSQL_ROOT_PASSWORD or MARIADB_ROOT_PASSWORD) to match."
		if mysqlMaster.RootPassword == "" {
			msg = "Backup failed: the database refused the root login, and this service has no root password setting (MYSQL_ROOT_PASSWORD or MARIADB_ROOT_PASSWORD). Backups need the database's root password in one of these settings."
		}
		event.PostEventUpdate("agent-mysql-access-denied", withOutput(msg, out))
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

	backupCmd = append(backupCmd, backupBinary, "--backup", "--datadir="+mysqlMaster.DataPath, "--port=3306")
	backupCmd = append(backupCmd, "--target-dir="+mysqlMaster.DataPath+"/backups")
	backupCmd = append(backupCmd, "--user="+mysqlMaster.Username)
	backupCmd = append(backupCmd, "--password="+mysqlMaster.Password)
	backupCmd = append(backupCmd, "--host="+mysqlMaster.IPAddress)

	stopWatch := func() *backupLockBlock { return nil }
	if mysqlMaster.Variant == "mariadb" {
		lockWait := viper.GetInt("mariadb.lock_wait.timeout")
		if usesBackupStage(mysqlMaster) {
			stopWatch = watchBackupLock(backupContainer, mysqlMaster, time.Duration(lockWait)*time.Second)
		} else {
			backupCmd = append(backupCmd, ftwrlOptions(lockWait)...)
		}
	}

	exitCode, out, err := backupContainer.Exec(backupCmd)
	// A backup that exited 0 succeeded, even if the watch killed a connection just as it finished.
	if block := stopWatch(); block != nil && (err != nil || exitCode != 0) {
		msg := "Backup stopped: it waited " + strconv.Itoa(block.Waited) + " seconds for the database's backup lock. " +
			"The lock waits for long-running DDL (such as ALTER TABLE or OPTIMIZE TABLE) and for writes to " +
			"non-transactional (such as MyISAM) tables to finish."
		if block.Blocker != "" {
			msg += "\nLongest-running query: " + block.Blocker
		}
		event.PostEventUpdate("agent-mysql-backup-lock-wait", withOutput(msg, out))
		return false
	}

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

// usesBackupStage reports whether a mariadb-variant server takes its backup lock with BACKUP
// STAGE (MariaDB 10.4+) rather than FLUSH TABLES WITH READ LOCK.
//
// With BACKUP STAGE the agent passes no --ftwrl-wait-* / --kill-long-* options: since MariaDB
// 10.11.8 mariadb-backup ignores all of them but --ftwrl-wait-timeout, which now waits for
// every running statement — SELECTs included — and fails the backup if any outlasts it. BACKUP
// STAGE itself never waits for a SELECT or an InnoDB write; watchBackupLock bounds the waits it
// does have.
func usesBackupStage(mysqlMaster *MysqlInstance) bool {
	return !mysqlMaster.Version.LessThan(semver.Version{Major: 10, Minor: 4})
}

// ftwrlOptions keeps a pre-10.4 server's FLUSH TABLES WITH READ LOCK from being requested
// while a long statement runs. That lock waits for running SELECTs and blocks every write while
// it waits, so mariabackup first waits up to lockWait seconds for running statements to finish,
// and fails the backup rather than take it if they don't.
func ftwrlOptions(lockWait int) []string {
	return []string{
		"--ftwrl-wait-query-type=ALL",
		"--ftwrl-wait-timeout=" + strconv.Itoa(lockWait),
		"--kill-long-query-type=SELECT",
		"--kill-long-queries-timeout=20",
	}
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
