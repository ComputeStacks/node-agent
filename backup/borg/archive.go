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

// benignCreateWarnings are the `borg create` warning msgids that do NOT mean data is
// missing from the archive. Measured on borg 1.4.4:
//
//	FileChangedWarning  "<file>: file changed while we backed it up" — the file IS in
//	                    the archive; its content may be a torn read.
//
// Deliberately an allowlist of one, not a denylist. The measured alternative,
// BackupPermissionError, exits borgWarningExit at the same WARNING severity and the file
// is absent from the archive entirely (verified: `borg list` omits it, nfiles is short) —
// so an unrecognised warning must fail rather than be assumed harmless. When one does,
// the task carries borg's own message, so widening this set is a deliberate, evidenced
// decision rather than a silent default.
var benignCreateWarnings = map[string]bool{"FileChangedWarning": true}

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

	// Never inherit a previous run's warning: Warning is an out-param on the receiver
	// and an Archive can be reused.
	a.Warning = nil

	res := a.Repository.RunBorgWarnTolerant("borg create", createCommand(a.archivePath(), viper.GetString("backups.borg.compression"), lockWait("create")))

	// Decoded first, before the failure check: on a non-zero exit the payload is the
	// only thing that can say whether borg wrote an archive at all.
	borgResponse, decoded := decodeArchiveMessage(res.Response)

	if res.Failure != nil && !wroteCompleteArchive(res, decoded, borgResponse.Archive.ID) {
		return ArchiveMessage{}, a.createFailure(res)
	}

	// Sync reports the repository's observed state (size + archive list) up into
	// control.db, and it is owed for the archive borg just wrote on every path that
	// reports success — including the warned one, and whether or not the --json payload
	// could be read back.
	a.Repository.Sync()

	if res.Failure != nil {
		// borg's warning tier over an archive that holds the volume. Not a failure, so
		// it does not travel in the return value — Create's contract is that a non-nil
		// *LogMessage means the backup failed. The caller publishes a.Warning.
		a.Warning = res.Failure
		borgLogger().Warn("Completed backup with warnings", "archive", borgResponse.Archive.ID,
			"msgid", res.Failure.MsgID, "warning", res.Failure.Message)
		return borgResponse, nil
	}

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

// createFailure picks the reason to report for a create that wroteCompleteArchive
// refused, and logs it — RunBorgWarnTolerant deliberately logs create's warning tier at
// DEBUG only, because only this function knows whether the warning was accepted.
//
// It prefers the record that disqualified the run over failureRecord's severity-based
// pick, for the reason nonBenignCreateWarning explains. Two cases keep res.Failure:
//
//   - a docker fault, where the command may never have run and that fact outranks
//     anything in the output (and where a msgid must not become machine-actionable —
//     see dockerFailure);
//   - any exit other than the warning tier, where borg's ERROR record outranks a WARNING
//     and failureRecord has already chosen correctly.
func (a *Archive) createFailure(res ExecResult) *LogMessage {
	failure := res.Failure
	if !res.DockerFault && res.ExitCode == borgWarningExit {
		if offender := nonBenignCreateWarning(res.Response); offender != nil {
			failure = offender
		}
	}
	borgLogger().Warn("Backup failed", "repo", a.Repository.Name, "exitCode", res.ExitCode,
		"msgid", failure.MsgID, "reason", failure.Message)
	return failure
}

// createCommand builds the `borg create` command.
//
// NO --error, deliberately: it filters the WARNING record that says which file warned
// and why, which is what left a successful backup of a busy volume reporting "borg
// create exited 1: no diagnostic output" and what wroteCompleteArchive needs in order to
// tell a torn file from a missing one. Same call Archive.Delete already makes. Dropping
// it adds nothing to the clean path — borg 1.4.4 emits zero records on an rc 0 create
// either way (measured).
//
// A pure function of its inputs, so its flag set is testable without docker or viper;
// Create reads the configuration and passes it in.
func createCommand(archivePath, compression, lockWaitSecs string) []string {
	cmd := []string{"cd /mnt/data && borg --log-json"}
	cmd = append(cmd, "--lock-wait "+lockWaitSecs)
	cmd = append(cmd, "create --one-file-system --json --numeric-ids --exclude-caches")
	cmd = append(cmd, "--compression "+compression)
	cmd = append(cmd, archivePath)
	cmd = append(cmd, ".")
	return cmd
}

// createWarnings returns every log record in a create response, in order.
//
// Separate from failureRecord, which returns the single best record to quote as a
// failure reason: the gate needs ALL of them, because one benign warning alongside one
// data-affecting warning must not read as benign. borg emits one record per warned file.
//
// It reuses scanBorgOutput's definition of a record, so a --json data payload — pretty-
// printed or compact — contributes nothing, and a non-JSON line contributes nothing
// either. Nothing is filtered out beyond that: an unexpected record makes
// wroteCompleteArchive refuse the downgrade, which is the safe direction.
func createWarnings(response string) []LogMessage {
	var warnings []LogMessage
	for _, l := range scanBorgOutput(response) {
		if !l.isRecord {
			continue
		}
		warnings = append(warnings, l.record)
	}
	return warnings
}

// nonBenignCreateWarning returns the first record that is NOT in benignCreateWarnings, or
// nil when every record is benign (or there are none).
//
// It exists so that a create refused by wroteCompleteArchive reports the warning that
// disqualified it. failureRecord cannot do that job: it picks by severity, and every
// create warning is levelname WARNING with a msgid, so its tie-break never fires and the
// FIRST record wins — whichever file borg happened to walk first. On a volume with both a
// busy file and an unreadable one, that reported "file changed while we backed it up" (the
// harmless one) for a backup that failed because a different file was missing from the
// archive entirely, telling the operator the opposite of what happened.
func nonBenignCreateWarning(response string) *LogMessage {
	for _, w := range createWarnings(response) {
		if !benignCreateWarnings[w.MsgID] {
			offender := w
			return &offender
		}
	}
	return nil
}

// wroteCompleteArchive reports whether a non-zero `borg create` nonetheless wrote an
// archive that holds the volume, and may therefore be reported as a success.
//
// Every term is load-bearing:
//
//   - DockerFault: Container.Exec returns a HARDCODED 1 on a docker-level fault, and one
//     of those paths returns the full captured output with it. borg's real exit code is
//     then unknown and may have been 2, so the exit code below means nothing.
//   - borgWarningExit: anything else is borg's error tier, never downgradeable.
//   - decoded and a non-empty archive id: borg's own statement that it committed an
//     archive. Nothing else in the response proves it.
//   - at least one record: an rc 1 with nothing to inspect gives no grounds to call the
//     warning benign, which is also exactly the old behavior if --error ever comes back.
//   - every record benign: see benignCreateWarnings. BackupPermissionError exits with
//     the same code at the same severity and means a file is missing from the archive.
func wroteCompleteArchive(res ExecResult, decoded bool, archiveID string) bool {
	if res.DockerFault || res.ExitCode != borgWarningExit {
		return false
	}
	if !decoded || archiveID == "" {
		return false
	}
	warnings := createWarnings(res.Response)
	if len(warnings) == 0 {
		return false
	}
	for _, w := range warnings {
		if !benignCreateWarnings[w.MsgID] {
			return false
		}
	}
	return true
}

// decodeArchiveMessage reads `borg create --json`'s payload out of a create response.
// ok is false when nothing usable came back.
//
// The payload is extracted rather than unmarshalled whole, because create runs without
// --error: a warning run's response is one --log-json record PER WARNED FILE followed by
// the payload — several concatenated JSON documents, which will not unmarshal as one.
// Dropping the records leaves the payload, and the archive id in it is what proves borg
// committed an archive, which is what wroteCompleteArchive turns on.
//
// scanBorgOutput's isRecord is reused so there is ONE definition of "is a log record"
// for both the failure path and this one, rather than a second that can drift from it. A
// pretty-printed payload's lines are individually unparseable and so are individually
// not records, and a compact payload parses but carries no Message or MsgID, so both
// survive intact.
//
// An archive id that decoded empty counts as "nothing usable", exactly like a response
// that would not unmarshal at all. Go ignores unknown fields, so anything shaped
// unlike the payload — a --log-json record, a stats line — unmarshals cleanly into an
// all-zero ArchiveMessage; the old code took that for a decoded payload and logged
// `Completed backup archive= duration=0.00000`, a completion line naming no archive and
// claiming a zero duration. There is nothing for a caller to do with an ArchiveMessage
// that has no id, so it takes the same "succeeded, but the response could not be
// decoded" path.
//
// It fails safe: a non-record line that is not part of the payload makes the rejoin
// invalid JSON, ok is false, and the gate then fails the run.
func decodeArchiveMessage(response string) (ArchiveMessage, bool) {
	var payload []string
	for _, l := range scanBorgOutput(response) {
		if l.isRecord {
			continue
		}
		payload = append(payload, l.raw)
	}
	if len(payload) == 0 {
		return ArchiveMessage{}, false
	}
	var msg ArchiveMessage
	if err := json.Unmarshal([]byte(strings.Join(payload, "\n")), &msg); err != nil {
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
