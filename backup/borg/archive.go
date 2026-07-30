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

	// generateName reads the repository (borg list), so it can fail for borg's own
	// reasons — a held lock, a missing repository — not only because no name could be
	// composed. Report whichever it was.
	if nameErr := a.generateName(); nameErr != nil {
		return borgResponse, nameErr
	}

	backupCmd := []string{"cd /mnt/data && borg --log-json"}
	backupCmd = append(backupCmd, "--lock-wait "+lockWait("create"))
	backupCmd = append(backupCmd, "create --error --one-file-system --json --numeric-ids --exclude-caches")
	backupCmd = append(backupCmd, "--compression "+viper.GetString("backups.borg.compression"))
	backupCmd = append(backupCmd, a.archivePath())
	backupCmd = append(backupCmd, ".")

	res := a.Repository.RunBorg("borg create", backupCmd)

	if res.Failure != nil {
		return ArchiveMessage{}, res.Failure
	}
	// Before the decode, deliberately: Sync reports the repository's observed state
	// (size + archive list) up into control.db, and it is owed for the archive borg
	// just wrote whether or not its --json payload can be read back.
	a.Repository.Sync()

	borgResponse, decoded := decodeArchiveMessage(res.Response)
	if decoded {
		borgLogger().Info("Completed backup", "archive", borgResponse.Archive.ID, "duration", hclog.Fmt("%.5f", borgResponse.Archive.Duration))
		return borgResponse, nil
	}
	// Still a success, and still a nil error: the exit code is what says whether borg
	// wrote the archive, and it exited 0. The caller records the backup and advances
	// last_backup, which is correct — what was broken was reporting success on a
	// NON-zero exit, and the funnel above now prevents that.
	borgLogger().Warn("Backup appears to have succeeded, but there was an error decoding the response data from borg.")
	return ArchiveMessage{}, nil
}

// decodeArchiveMessage reads `borg create --json`'s payload out of the output of a run
// that exited 0. ok is false when nothing usable came back.
//
// An archive id that decoded empty counts as "nothing usable", exactly like a response
// that would not unmarshal at all. Go ignores unknown fields, so anything shaped
// unlike the payload — a --log-json record, a stats line — unmarshals cleanly into an
// all-zero ArchiveMessage; the old code took that for a decoded payload and logged
// `Completed backup archive= duration=0.00000`, a completion line naming no archive and
// claiming a zero duration. There is nothing for a caller to do with an ArchiveMessage
// that has no id, so it takes the same "succeeded, but the response could not be
// decoded" path.
func decodeArchiveMessage(response string) (ArchiveMessage, bool) {
	var msg ArchiveMessage
	if err := json.Unmarshal([]byte(response), &msg); err != nil {
		borgLogger().Debug("Unmarshal Error on Borg Backup Response", "error", err.Error(), "raw", response)
		return ArchiveMessage{}, false
	}
	if msg.Archive.ID == "" {
		borgLogger().Debug("Borg Backup Response carried no archive id", "raw", response)
		return ArchiveMessage{}, false
	}
	return msg, true
}

// Restore extracts an archive over /mnt/data, restoring the whole of it unless
// filePaths names specific files to pick out (DEPRECATED).
//
// It does NOT move the volume's existing contents aside, and it does not roll anything
// back: the restore hooks own the snapshot and its rollback, in one place each.
//
// It used to do both, in parallel with the hooks doing the same thing, and the overlap
// was unreachable rather than harmless. Two consequences, both fatal once a non-zero
// exit is honoured:
//
//   - It re-ran the mysql strategy's snapshot command against a /mnt/data that the hook
//     had already emptied, where the glob does not expand and mv exits non-zero with
//     nothing wrong — failing every mysql restore.
//   - When both rollbacks ran, this one moved the snapshot back into /mnt/data and then
//     rollbackRestoreMysql's `rm -rf /mnt/data/*` deleted it, leaving nothing in either
//     place. The backup container is AutoRemove, so there was nothing left to recover
//     from.
//
// Removing it also fixes an asymmetry: the internal rollback only ever ran on the
// docker-fault path, so a borg failure — the path that actually fires — rolled back
// nothing at all. Every failure now returns non-nil to one caller, restore.Restore,
// which calls rollbackRestore for all of them.
func (a *Archive) Restore(filePaths []string) *LogMessage {
	if reflect.ValueOf(a.Repository.Container).IsNil() {
		return &LogMessage{Message: "Missing backup container"}
	}

	// Perform Restore
	cmd := []string{"cd /mnt/data && borg --log-json"}
	cmd = append(cmd, "--lock-wait "+viper.GetString("backups.borg.lock_wait"))
	cmd = append(cmd, "extract --error --numeric-ids")
	cmd = append(cmd, a.archivePath())
	for _, p := range filePaths {
		cmd = append(cmd, p)
	}

	res := a.Repository.RunBorg("borg extract", cmd)

	borgLogger().Debug("Restore Response", "output", res.Response)

	if res.Failure != nil {
		return res.Failure
	}

	// Belt and braces on a clean exit: extract is silent on success with --error, so
	// anything here is a record borg emitted without failing.
	return readArchiveRestoreResponse(res.Response)
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

	res := a.Repository.RunBorg("borg delete", cmd)

	// This site alone used to hand-roll the non-zero-exit check, because it could not
	// rely on the old gate; that check was exactly the funnel's rule, so it collapses
	// to this. The reason is borg's own diagnosis rather than the --stats table (which
	// says only "Deleted data: 0 B" and reads like the cause when it isn't), the exit
	// code is in res.ExitCode and the funnel's log line, and the full response is at
	// DEBUG there.
	if res.Failure != nil {
		return results, res.Failure
	}

	// A clean exit is a successful delete even when nothing was printed: the lone ""
	// element below fails to unmarshal and is skipped, leaving results empty. (The
	// gate this replaces returned a pointer to a ZERO LogMessage in that case, failing
	// the task with the reason "() " and skipping Sync.)
	list := strings.Split(res.Response, "\n")
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

// generateName gives the archive a name that does not collide with one already in
// the repository. It returns nil on success, otherwise the reason it could not.
//
// A bare bool was indistinguishable: a failed `borg list` and a genuinely
// un-nameable archive both surfaced as "Unable to generate unique archive name",
// hiding borg's own diagnosis of the first. The two synthesized reasons below carry
// no MsgID, so they can never be mistaken for a verdict borg reported; only the
// Contents() path returns borg's record.
func (a *Archive) generateName() *LogMessage {
	if a.Repository == nil {
		// Unreachable from Create, which checks this first; kept so the method is
		// safe on its own.
		return &LogMessage{Message: "Missing Repository"}
	}
	contents, contentsErr := a.Repository.Contents()
	if contentsErr != nil {
		return contentsErr
	}
	rand.New(rand.NewSource(time.Now().UnixNano()))
	randNum := rand.Intn(10000-10) + 10
	for _, i := range contents.Archives {
		if i.Name == a.Name {
			a.Name = a.Name + "-" + strconv.Itoa(randNum)
			break
		}
	}
	if a.Name == "" {
		return &LogMessage{Message: "Unable to generate unique archive name"}
	}
	return nil
}

func (a *Archive) archivePath() string {
	return "::" + a.Name
	//return a.Repository.repoPath() + "::" + a.Name
}
