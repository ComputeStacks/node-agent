package borg

import (
	"encoding/json"
	"math/rand"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/hashicorp/go-hclog"
	"github.com/spf13/viper"
)

// lockWait returns the borg --lock-wait value for an operation, falling back to
// the global backups.borg.lock_wait when no per-op override is configured. A
// scheduled `create` uses a longer wait so it rides out an in-agent compact/prune
// (both hold borg's exclusive lock) instead of failing fast and missing a backup.
func lockWait(op string) string {
	if v := viper.GetString("backups.borg.lock_wait_" + op); v != "" {
		return v
	}
	return viper.GetString("backups.borg.lock_wait")
}

func (a *Archive) Create() (ArchiveMessage, *LogMessage) {
	var borgResponse ArchiveMessage
	var log LogMessage
	if a.Repository == nil {
		log.Message = "Missing Repository"
		return borgResponse, &log
	}

	if reflect.ValueOf(a.Repository.Container).IsNil() {
		return borgResponse, &LogMessage{Message: "Missing backup container"}
	}

	if !a.generateName() {
		log.Message = "Unable to generate unique archive name"
		return borgResponse, &log
	}

	backupCmd := []string{"cd /mnt/data && borg --log-json"}
	backupCmd = append(backupCmd, "--lock-wait "+lockWait("create"))
	backupCmd = append(backupCmd, "create --error --one-file-system --json --numeric-ids --exclude-caches")
	backupCmd = append(backupCmd, "--compression "+viper.GetString("backups.borg.compression"))
	backupCmd = append(backupCmd, a.archivePath())
	backupCmd = append(backupCmd, ".")

	_, response, log := a.Repository.ExecWithLog(backupCmd)

	if log != (LogMessage{}) {
		return ArchiveMessage{}, &log
	}
	a.Repository.Sync()
	marshalErr := json.Unmarshal([]byte(response), &borgResponse)
	if marshalErr == nil {
		borgLogger().Info("Completed backup", "archive", borgResponse.Archive.ID, "duration", hclog.Fmt("%.5f", borgResponse.Archive.Duration))
		return borgResponse, nil
	} else {
		borgLogger().Debug("Unmarshal Error on Borg Backup Response", "error", marshalErr.Error(), "raw", response)
	}
	borgLogger().Warn("Backup appears to have succeeded, but there was an error decoding the response data from borg.")
	return ArchiveMessage{}, nil
}

/**
 * Restore an archive to a volume
 *
 * (DEPRECATED) You can optionally specify specific files (including their path) to restore. Otherwise, it will restore the entire directory.
 */
func (a *Archive) Restore(filePaths []string) *LogMessage {
	if reflect.ValueOf(a.Repository.Container).IsNil() {
		return &LogMessage{Message: "Missing backup container"}
	}

	// Move current structure to snapshot
	preRestore := []string{"mkdir", "-p /root/.snapshot"}
	preRestore = append(preRestore, "&&", "mv", "/mnt/data/* /root/.snapshot/")

	_, _, preRestoreLog := a.Repository.ExecWithLog(preRestore)

	if preRestoreLog != (LogMessage{}) {
		return &preRestoreLog
	}

	// Perform Restore
	cmd := []string{"cd /mnt/data && borg --log-json"}
	cmd = append(cmd, "--lock-wait "+viper.GetString("backups.borg.lock_wait"))
	cmd = append(cmd, "extract --error --numeric-ids")
	cmd = append(cmd, a.archivePath())
	for _, p := range filePaths {
		cmd = append(cmd, p)
	}
	_, response, log := a.Repository.ExecWithLog(cmd)

	borgLogger().Debug("Restore Response", "output", response)

	if log != (LogMessage{}) {

		// Failed, so we roll back
		rollbackCmd := []string{"rm", "-rf /mnt/data/*"}
		rollbackCmd = append(rollbackCmd, "&&", "mv /root/.snapshot/* /mnt/data/")
		if _, rollbackResponse, rollbackLog := a.Repository.ExecWithLog(rollbackCmd); rollbackLog != (LogMessage{}) {
			borgLogger().Warn("Fatal error performing rollback on restore", "response", rollbackResponse, "error", rollbackLog.Message)
		}
		return &log
	}
	return readArchiveRestoreResponse(response)
}

func (a *Archive) Info() (*ArchiveResponse, *LogMessage) {
	if reflect.ValueOf(a.Repository.Container).IsNil() {
		return &ArchiveResponse{}, &LogMessage{Message: "Missing backup container"}
	}
	var archiveResponse ArchiveResponse

	// --bypass-lock is required here, not an optimization. backups.borg.lock_wait
	// defaults to 1 second (only lock_wait_create is longer) and a held exclusive
	// lock makes `info` exit 2 with msgid LockTimeout — so now that a non-zero exit
	// is honoured, without the bypass every export, delete and restore that overlaps
	// a backup of the same volume would fail at this existence check. Exports are
	// designed to coexist with backups: backup creation deliberately does not take
	// the per-repo mutex, and the export itself reads with --bypass-lock. That only
	// worked because the LockTimeout record unmarshals into ArchiveResponse as
	// all-zero fields, so this function reported success and hid the timeout.
	// `info ::archive --bypass-lock` under a held exclusive lock exits 0 with no
	// record; `info` is read-only and the mutation that follows takes its own lock,
	// so the bypass preserves today's concurrency rather than trading it away.
	// --lock-wait stays, and this ordering (globals before the subcommand) is what
	// was measured working.
	cmd := []string{"borg --log-json --bypass-lock"}
	cmd = append(cmd, "--lock-wait "+viper.GetString("backups.borg.lock_wait"))
	cmd = append(cmd, "info --error --json")
	cmd = append(cmd, a.archivePath())

	res := a.Repository.RunBorg("borg info", cmd)

	if res.Failure != nil {
		return nil, res.Failure
	}

	marshalErr := json.Unmarshal([]byte(res.Response), &archiveResponse)

	if marshalErr != nil {
		sentry.CaptureException(marshalErr)
		borgLogger().Error("Error unmarshaling json", "function", "Archive.Info", "error", marshalErr.Error())
		return nil, &LogMessage{Message: marshalErr.Error()}
	}

	return &archiveResponse, nil
}

func (a *Archive) Delete() ([]LogMessage, *LogMessage) {
	var results []LogMessage

	if reflect.ValueOf(a.Repository.Container).IsNil() {
		return results, &LogMessage{Message: "Missing backup container"}
	}

	// No --error here: it suppresses the WARNING-level record that carries borg's own
	// diagnosis of a failure (e.g. "Archive X not found (1/1).") while the --stats
	// records bypass the level filter, so a failed delete reported nothing but the
	// stats table. Dropping --error costs nothing on success — borg 1.4.4 emits the
	// same eight INFO stats records either way.
	cmd := []string{"borg --log-json"}
	cmd = append(cmd, "--lock-wait "+viper.GetString("backups.borg.lock_wait"))
	cmd = append(cmd, "delete --stats --force")
	cmd = append(cmd, a.archivePath())

	borgLogger().Debug("Raw Delete Command", "cmd", strings.Join(cmd, " "))

	exitCode, response, log := a.Repository.ExecWithLog(cmd)

	if exitCode > 0 {
		// The full response is kept at DEBUG so nothing is lost when troubleshooting;
		// the reported message is borg's own diagnosis, never the --stats table (which
		// says only "Deleted data: 0 B" and reads like the cause when it isn't).
		borgLogger().Debug("Archive delete exited non-zero", "exitCode", exitCode, "response", response)
		reason := failureReason(response)
		if reason == "" {
			// An exec-level failure (docker client error, container never came online)
			// arrives with an empty response and the reason already in log.Message —
			// keep it rather than claiming borg ran and said nothing.
			reason = strings.TrimSpace(log.Message)
		}
		if reason == "" {
			reason = "no diagnostic output from borg"
		}
		log.Message = "borg delete exited " + strconv.Itoa(exitCode) + ": " + reason
		return results, &log
	}

	// Gate on the borg error only. &log is never nil, so the old `response == "" ||`
	// clause turned exit code 0 with no borg error but empty output into a non-nil
	// pointer to a ZERO LogMessage — failing the task with the reason "() ", skipping
	// Sync() and logging no completion line. An empty response with a clean exit is a
	// successful delete: it falls through to the loop below, where the single ""
	// element fails to unmarshal and is skipped, leaving results empty.
	if log != (LogMessage{}) {
		return results, &log
	}

	list := strings.Split(response, "\n")
	for _, d := range list {
		var result LogMessage
		if jErr := json.Unmarshal([]byte(d), &result); jErr != nil {
			continue
		}
		results = append(results, result)
	}
	a.Repository.Sync()

	borgLogger().Info("Completed Archive Delete event", "volume", a.Repository.Name, "archive", a.Name)
	return results, nil
}

func (a *Archive) generateName() bool {
	if a.Repository == nil {
		return false
	}
	contents, contentsErr := a.Repository.Contents()
	if contentsErr != nil {
		return false
	}
	rand.New(rand.NewSource(time.Now().UnixNano()))
	randNum := rand.Intn(10000-10) + 10
	for _, i := range contents.Archives {
		if i.Name == a.Name {
			a.Name = a.Name + "-" + strconv.Itoa(randNum)
			break
		}
	}
	return a.Name != ""
}

func (a *Archive) archivePath() string {
	return "::" + a.Name
	//return a.Repository.repoPath() + "::" + a.Name
}
