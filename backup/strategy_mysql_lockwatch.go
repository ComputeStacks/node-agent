package backup

import (
	"cs-agent/containermgr"
	"strconv"
	"strings"
	"sync"
	"time"
)

// lockWatchInterval is how often watchBackupLock looks at the server's process list.
const lockWatchInterval = 5 * time.Second

// backupLockBlock describes a backup watchBackupLock stopped because it waited too long
// for its backup lock.
type backupLockBlock struct {
	Waited  int    // seconds the BACKUP STAGE statement had waited
	Blocker string // the longest-running other statement, as "<seconds>s: <query>"; may be empty
}

// watchBackupLock guards a running mariadb-backup against waiting forever for its backup
// lock. mariadb-backup takes the lock in BACKUP STAGE steps, and a step waits as long as a
// write to a non-transactional (MyISAM) table is running — mariadb-backup sets its own
// lock_wait_timeout to a year, so nothing on the server bounds that wait, and other writes
// to MyISAM tables queue behind it. SELECTs and InnoDB writes never block it.
//
// Every lockWatchInterval it asks the server whether a BACKUP STAGE statement is waiting
// for the backup lock; once one has waited limit or longer, it notes the longest-running
// other statement and kills the backup's connection. The server then releases every backup
// stage and mariadb-backup exits with an error.
//
// The returned stop ends the watch and reports whether it killed the backup. A limit of
// zero or less disables the watch. Queries that fail (no client binary, refused login) are
// logged and retried; the watch never fails the backup on its own account.
func watchBackupLock(c *containermgr.Container, m *MysqlInstance, limit time.Duration) (stop func() *backupLockBlock) {
	if limit <= 0 {
		return func() *backupLockBlock { return nil }
	}
	done := make(chan struct{})
	var once sync.Once
	var result *backupLockBlock
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(lockWatchInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
			}
			if b := checkBackupLock(c, m, limit); b != nil {
				result = b
				return
			}
		}
	}()
	return func() *backupLockBlock {
		once.Do(func() { close(done) })
		wg.Wait()
		return result
	}
}

// checkBackupLock is one look at the process list; it kills the backup and returns what it
// found when the BACKUP STAGE statement has waited limit or longer.
func checkBackupLock(c *containermgr.Container, m *MysqlInstance, limit time.Duration) *backupLockBlock {
	rows, err := mysqlQuery(c, m, "SELECT id, time FROM information_schema.processlist "+
		"WHERE state = 'Waiting for backup lock' AND info LIKE 'BACKUP STAGE%' ORDER BY time DESC LIMIT 1")
	if err != nil {
		backupLogger().Debug("Backup lock watch: process list query failed", "error", err.Error())
		return nil
	}
	if len(rows) == 0 || len(rows[0]) < 2 {
		return nil
	}
	id := rows[0][0]
	waited, err := strconv.Atoi(rows[0][1])
	if err != nil || time.Duration(waited)*time.Second < limit {
		return nil
	}
	if _, err := strconv.Atoi(id); err != nil {
		return nil
	}

	block := &backupLockBlock{Waited: waited}
	blockers, err := mysqlQuery(c, m, "SELECT time, LEFT(REPLACE(REPLACE(info, '\\n', ' '), '\\t', ' '), 300) FROM information_schema.processlist "+
		"WHERE command = 'Query' AND info IS NOT NULL AND info NOT LIKE 'BACKUP STAGE%' AND id <> CONNECTION_ID() "+
		"ORDER BY time DESC LIMIT 1")
	if err == nil && len(blockers) > 0 && len(blockers[0]) >= 2 {
		block.Blocker = blockers[0][0] + "s: " + blockers[0][1]
	}

	if _, err := mysqlQuery(c, m, "KILL "+id); err != nil {
		backupLogger().Warn("Backup lock watch: failed to kill the waiting backup", "error", err.Error())
		return nil
	}
	backupLogger().Warn("Stopped a backup waiting for its backup lock", "service", m.ServiceID, "waited", waited)
	return block
}

// mysqlQuery runs one statement as root with the mysql client inside c and returns its rows,
// tab-separated fields per row.
func mysqlQuery(c *containermgr.Container, m *MysqlInstance, query string) ([][]string, error) {
	cmd := []string{mysqlClientBinary(m), "-h", m.IPAddress, "-uroot", "-N", "-B", "-e", query}
	exitCode, out, err := c.ExecEnv(cmd, []string{"MYSQL_PWD=" + m.RootPassword})
	if err != nil {
		return nil, err
	}
	if exitCode != 0 {
		return nil, &mysqlQueryError{exitCode: exitCode, out: strings.TrimSpace(out)}
	}
	return parseMysqlRows(out), nil
}

type mysqlQueryError struct {
	exitCode int
	out      string
}

func (e *mysqlQueryError) Error() string {
	return "exit code " + strconv.Itoa(e.exitCode) + ": " + e.out
}

// parseMysqlRows splits `mysql -N -B` output (which arrives with CRLF line ends through a
// TTY exec) into rows of tab-separated fields.
func parseMysqlRows(out string) [][]string {
	var rows [][]string
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		rows = append(rows, strings.Split(line, "\t"))
	}
	return rows
}
