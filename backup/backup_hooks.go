package backup

import (
	"cs-agent/backup/borg"
	"cs-agent/containermgr"
	"cs-agent/types"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// withOutput appends captured command output to a message so a hook/exec failure's
// primary diagnostic (the command's own stderr/stdout) survives into the task
// result_json — the controller no longer receives a live csevent stream, so this
// is where that detail must land.
func withOutput(msg, out string) string {
	out = compactOutput(out)
	if out == "" {
		return msg
	}
	return msg + "\n" + out
}

// outputDigits masks the numbers in a line, so lines that differ only in a timestamp,
// counter or duration ("Waiting for query 7 (duration 196 sec)") compare equal.
var outputDigits = regexp.MustCompile(`[0-9]+`)

// compactOutput makes command output readable in an event: a run of three or more
// consecutive lines that differ only in their numbers is cut to its first and last
// line around a count of the lines left out. mariadb-backup, for one, prints the same
// "Waiting for query" line every second while it waits for a lock.
func compactOutput(out string) string {
	out = strings.TrimSpace(strings.ReplaceAll(out, "\r\n", "\n"))
	if out == "" {
		return ""
	}
	lines := strings.Split(out, "\n")
	compacted := make([]string, 0, len(lines))
	for i := 0; i < len(lines); {
		key := outputDigits.ReplaceAllString(lines[i], "#")
		j := i + 1
		for j < len(lines) && outputDigits.ReplaceAllString(lines[j], "#") == key {
			j++
		}
		if n := j - i; n >= 3 {
			compacted = append(compacted, lines[i], fmt.Sprintf("... (%d similar lines omitted) ...", n-2), lines[j-1])
		} else {
			compacted = append(compacted, lines[i:j]...)
		}
		i = j
	}
	return strings.Join(compacted, "\n")
}

func preBackup(vol *types.Volume, event *progress) (preBackupSuccess bool) {
	backupLogger().Info("Running preBackup job", "volume", vol.Name, "strategy", vol.Strategy)
	if vol.Strategy == "" {
		backupLogger().Debug("Strategy is blank for volume, defaulting to files.", "volume", vol.Strategy)
	}
	if len(vol.PreBackup) > 2 {
		success := false
		defer func() (preBackupSuccess bool) {
			if r := recover(); r != nil {
				event.PostEventUpdate("agent-98840575b7f423c4", fmt.Sprintf("%#v", r))
				return false
			}
			return success
		}()
		exitCode, out, err := containermgr.ServiceExec(strconv.Itoa(vol.ServiceID), vol.PreBackup)

		if err != nil {
			event.PostEventUpdate("agent-ce329a15239aeb9d", withOutput(err.Error(), out))
			return false
		}

		if exitCode > 0 {
			if vol.BackupContinueOnError {
				finalMsg := "Pre-Backup command returned a non-zero exit code (" + strconv.Itoa(exitCode) + "): " + strings.Join(vol.PreBackup, " ")
				event.PostEventUpdate("agent-0fe15b3798ec8dfd", withOutput(finalMsg, out))
			} else {
				finalMsg := "Backup halted due to non-zero exit code (" + strconv.Itoa(exitCode) + "): " + strings.Join(vol.PreBackup, " ")
				event.PostEventUpdate("agent-5594abeced0476b3", withOutput(finalMsg, out))
				return false
			}
		}

	}
	switch vol.Strategy {
	case "mysql":
		vol.BackupContinueOnError = true // Force this to be true, otherwise we will have stale backup containers running.
		return preBackupMysql(vol, event)
	case "postgres":
		vol.BackupContinueOnError = true // Force this to be true, otherwise we will have stale backup containers running.
		return preBackupPostgres(vol, event)
	default:
		return true
	}
}

func postBackup(vol *types.Volume, event *progress, repo *borg.Repository) {

	backupLogger().Info("Running postBackup", "volume", vol.Name)

	if len(vol.PostBackup) > 2 {
		defer func() {
			if r := recover(); r != nil {
				event.PostEventUpdate("agent-b400393269184c12", fmt.Sprintf("%#v", r))
				return
			}
		}()
		exitCode, out, err := containermgr.ServiceExec(strconv.Itoa(vol.ServiceID), vol.PostBackup)

		if err != nil {
			event.PostEventUpdate("agent-ee979a773f9b7788", withOutput(err.Error(), out))
			return
		}

		if exitCode > 0 {
			finalMsg := "Post-Backup commands returned a non-zero exit code (" + strconv.Itoa(exitCode) + "): " + strings.Join(vol.PostBackup, " ")
			event.PostEventUpdate("agent-bb7c6f21b16ac27b", withOutput(finalMsg, out))
		}

	}
	switch vol.Strategy {
	case "mysql":
		_ = postBackupMysql(event, repo)
	case "postgres":
		_ = postBackupPostgres(event, repo)
	default:
		return
	}
}
