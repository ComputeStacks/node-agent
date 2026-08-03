package borg

import (
	"reflect"
	"strconv"
	"strings"

	"github.com/getsentry/sentry-go"
)

// execNoContainer is the ExitCode reported when there was no backup container to run
// in. It is a sentinel, not something a command returned — nothing ran at all.
const execNoContainer = 99

// maxReasonBytes caps every reason, whoever composed it. A reason is copied into a
// task's result_json and rides the changelog, so it cannot be unbounded; the full
// output is kept in ExecResult.Response and logged at DEBUG.
const maxReasonBytes = 1024

const reasonTruncated = "...[truncated]"

// borgWarningExit is borg's warning tier: the command reached its normal end but logged
// at WARNING. borg sets this code independently of the --log-json level filter, so it can
// arrive with no record attached at all — which is exactly how a successful backup of a
// busy volume came to be reported as "borg create exited 1: no diagnostic output".
//
// A warning is NOT generically a success. Whether one may be downgraded is a per-command
// question and belongs to the caller: `borg delete` exiting 1 means the archive was not
// found and nothing was deleted, and `borg extract` exiting 1 means nothing was restored.
// Archive.Create is the only caller that downgrades it, and only after checking borg's
// msgid — see wroteCompleteArchive.
const borgWarningExit = 1

// ExecResult is the outcome of one command in the backup container.
type ExecResult struct {
	// ExitCode is the command's exit code, or execNoContainer when there was no
	// container to run it in.
	//
	// Only trustworthy as the command's own verdict when DockerFault is false. Read
	// that field before acting on this one.
	ExitCode int

	// DockerFault reports that the failure was docker-level rather than the command's
	// own verdict: the docker client failed, the container never came online, or the
	// exec could not be inspected.
	//
	// It exists because Container.Exec returns a HARDCODED 1 on every one of those
	// paths, and one of them (the ContainerExecInspect error path) returns the full
	// captured output alongside it. So a docker fault arriving right after `borg create`
	// finished is indistinguishable from borg's warning tier by ExitCode alone: exit 1,
	// with a complete --json payload in Response. borg's real exit code in that case is
	// unknown and may well have been 2.
	//
	// A caller that downgrades a non-zero exit to a success MUST refuse to do so when
	// this is true.
	DockerFault bool

	// Response is everything the command wrote: stdout and stderr merged and
	// \r\n-framed, because Container.Exec allocates a TTY.
	Response string

	// Failure is non-nil IFF the command failed, and carries borg's own record when
	// borg produced one: every field untouched and the message never rewritten, only
	// capped at maxReasonBytes (see classify).
	//
	// A pointer, not a value, deliberately. The gate it replaces was
	// `log != (LogMessage{})` — whole-struct equality — so a response that populated
	// only Time or LevelName flipped every gate in the package, and a response that
	// populated nothing (which is what borg's --json data payloads unmarshal to) left
	// a real failure looking like a success. A nil check cannot be got wrong that way.
	Failure *LogMessage
}

// RunBorg runs a borg command in the backup container. res.Failure is non-nil IFF the
// command failed, and quotes borg's own --log-json record when there is one.
func (r *Repository) RunBorg(label string, cmd []string) ExecResult {
	return r.run(label, true, logEveryFailure, cmd)
}

// RunBorgWarnTolerant is RunBorg for the one command whose caller may legitimately
// downgrade borg's warning tier to a success (Archive.Create, via wroteCompleteArchive).
//
// Classification is IDENTICAL to RunBorg — a non-zero exit is still a failure here, and
// the caller decides. The only difference is the log level: exit borgWarningExit is
// logged at DEBUG rather than WARN, so a healthy backup of a busy volume does not write
// "Command failed" to the node log for a task the agent then reports as completed. Every
// other non-zero exit still logs at WARN, and so does every other command — `borg delete`
// exiting 1 found no archive to delete and must stay visible.
func (r *Repository) RunBorgWarnTolerant(label string, cmd []string) ExecResult {
	return r.run(label, true, quietWarningTier, cmd)
}

// RunShell runs a plain shell command in the backup container. Its output is not borg
// JSON, so the output itself is the reason, verbatim.
func (r *Repository) RunShell(label string, cmd []string) ExecResult {
	return r.run(label, false, logEveryFailure, cmd)
}

// warnTierLogging selects how run() logs a failure whose exit code is borgWarningExit. A
// named type rather than a bool because it would otherwise be the second of two adjacent
// same-typed parameters, where a silent transposition compiles.
type warnTierLogging int

const (
	// logEveryFailure logs every failure at WARN. The default, for every command whose
	// warning tier is a real failure.
	logEveryFailure warnTierLogging = iota

	// quietWarningTier logs a borgWarningExit failure at DEBUG instead, because the
	// caller owns the verdict and reports it itself. See RunBorgWarnTolerant.
	quietWarningTier
)

func (r *Repository) run(label string, borgJSON bool, warnTier warnTierLogging, cmd []string) ExecResult {
	if reflect.ValueOf(r.Container).IsNil() {
		// DockerFault, because execNoContainer is a sentinel: nothing ran, so ExitCode is
		// not a command's verdict. That keeps the invariant a caller can rely on —
		// DockerFault false IFF ExitCode is the command's own exit code.
		return ExecResult{
			ExitCode:    execNoContainer,
			DockerFault: true,
			Failure:     &LogMessage{Message: "Missing backup container"},
		}
	}

	exitCode, response, err := r.Container.Exec([]string{"sh", "-c", strings.Join(cmd, " ")})

	if err != nil {
		// A docker-level fault: the docker client failed, or the container never came
		// online, so the command may never have run at all. It keeps its existing
		// treatment — logged at Error and reported to Sentry, because it is a fault on
		// this host. A non-zero borg exit does not go to Sentry: it is a fact about the
		// repository, and the ones we expect (missing repo, missing archive, held lock)
		// would bury real infrastructure faults.
		failure := dockerFailure(err, borgJSON, response)
		borgLogger().Error("Fatal exec", "label", label, "repo", r.Name, "error", err.Error())
		borgLogger().Debug("Fatal exec", "label", label, "exitCode", exitCode, "response", response)
		sentry.CaptureException(err)
		return ExecResult{
			ExitCode:    exitCode,
			DockerFault: true,
			Response:    response,
			Failure:     failure,
		}
	}

	res := ExecResult{ExitCode: exitCode, Response: response, Failure: classify(label, borgJSON, exitCode, response)}
	if res.Failure != nil {
		// A downgradeable warning tier is logged at DEBUG: the caller reports the
		// outcome, and a WARN here would say "Command failed" for a backup that
		// completes. Anything else keeps WARN.
		//
		// This is quiet about a warning tier the caller may ACCEPT, not about one it
		// rejects — the caller is responsible for logging at WARN when it decides the
		// warning was a real failure, because only it knows. Archive.Create does.
		if warnTier == quietWarningTier && exitCode == borgWarningExit {
			borgLogger().Debug("Command exited on borg's warning tier", "label", label, "exitCode", exitCode, "msgid", res.Failure.MsgID, "message", res.Failure.Message)
		} else {
			borgLogger().Warn("Command failed", "label", label, "exitCode", exitCode, "msgid", res.Failure.MsgID, "message", res.Failure.Message)
		}
		borgLogger().Debug("Command failed", "label", label, "exitCode", exitCode, "response", response)
	}
	return res
}

// classify decides whether a command that ran to completion failed, and if so why.
//
// It is split out from RunBorg/RunShell so the whole contract is testable without
// docker. What remains in those two is a nil check and Container.Exec, which cannot
// be exercised without a docker fake — and a fake detailed enough to be meaningful
// would mostly be testing the fake, so the limitation is stated rather than papered
// over.
//
// exitCode != 0 is a failure HERE, and the caller decides whether it stays one.
//
// This function has no warning tier, and that is a statement about layering rather than
// about borg: exit borgWarningExit means different things per command, so only the caller
// can rule on it. `borg delete` exiting 1 found no archive and deleted nothing; `borg
// extract` exiting 1 matched no include path and restored nothing. Both must fail.
// `borg create` exiting 1 may have written a perfectly good archive — Archive.Create is
// the only caller that downgrades it, and only via wroteCompleteArchive.
//
// An earlier version of this comment claimed create had "no warning tier" as a fact about
// borg, on the strength of five scenarios (files deleted, truncated, grown and removed
// mid-run, a unix socket in the tree) measured as exiting 0. Those five do exit 0 — they
// were re-measured on borg 1.4.4 and reproduce — but they are not the whole warning tier,
// and treating an incomplete measurement as a general rule is what made a successful
// backup report "borg create exited 1: no diagnostic output" on production volumes. Two
// warnings that DO exit 1 were missed:
//
//   - FileChangedWarning, which needs a file large enough that borg's read straddles a
//     concurrent write. It does not reproduce on a small hand-made file and happens
//     constantly on a live customer volume. The archive is complete; the warned file's
//     content may be a torn read.
//   - BackupPermissionError (and its BackupOSError siblings), where borg cannot read a
//     file, logs it, SKIPS it, and commits an archive without it. Same exit code, same
//     WARNING severity, and the file is simply absent — verified with `borg list`.
//
// Those two are why a downgrade must turn on borg's msgid and never on the exit code
// alone.
func classify(label string, borgJSON bool, exitCode int, response string) *LogMessage {
	if exitCode == 0 {
		return nil
	}

	if borgJSON {
		if record, ok := failureRecord(response); ok {
			// Verbatim, precisely: MsgID, LevelName, Name, Type and Time are untouched,
			// and the message is never rewritten or decorated — the msgid decides
			// whether a missing repository gets auto-initialised and prefixes the reason
			// the controller shows, and the exit code is already in ExecResult.ExitCode
			// and the log line, so folding it in would corrupt a reason that is borg's
			// own.
			//
			// The one thing done to the message is capping it, with an explicit
			// truncation marker, and that holds for borg's own records too. Nothing
			// parses Message: backup.go and restore.go branch on MsgID alone and
			// borgFailure only concatenates it, so a cap breaks no caller — while an
			// uncapped message is an unbounded result_json riding the changelog. The
			// untruncated output stays in ExecResult.Response.
			record.Message = truncateReason(record.Message)
			return &record
		}
	} else if reason := failureReason(response); reason != "" {
		return &LogMessage{Message: truncateReason(reason)}
	}

	// Nothing usable: name the command and the code, and carry no MsgID, so a reason
	// the agent invented can never be mistaken for one borg reported.
	return &LogMessage{Message: label + " exited " + strconv.Itoa(exitCode) + ": no diagnostic output"}
}

// dockerFailure composes the reason for a docker-level fault.
//
// The LogMessage carries NO MsgID even when the output contained one. A msgid is
// borg's verdict and the auto-init and already-exists paths act on it; a fault that
// may have stopped the command before it ran is not grounds to act on a verdict we
// cannot trust.
//
// The msgid is still shown to whoever reads the log line or result_json, as plain
// text in the message, because dropping it from the field is about not making it
// machine-actionable — not about hiding it from an operator.
func dockerFailure(err error, borgJSON bool, response string) *LogMessage {
	reason := err.Error()
	if d := diagnosis(borgJSON, response); d != "" {
		// Append rather than replace. Both halves matter: the docker error says the
		// command may not have run, the output says what was seen before it stopped,
		// and discarding either has already produced a misleading failure message.
		reason = reason + ": " + d
	}
	return &LogMessage{Message: truncateReason(reason)}
}

// diagnosis returns whatever the output explains, or "" when it explains nothing. It
// never synthesizes: the caller has a better reason of its own to fall back on.
//
// A record's msgid is included as plain text, since the only caller is the
// docker-error path, which cannot put it in LogMessage.MsgID.
func diagnosis(borgJSON bool, response string) string {
	if borgJSON {
		if record, ok := failureRecord(response); ok {
			if record.MsgID != "" {
				return "(msgid " + record.MsgID + ") " + record.Message
			}
			return record.Message
		}
		return ""
	}
	return failureReason(response)
}

// truncateReason caps a reason at maxReasonBytes, marking that it was cut. The cut is
// made on a rune boundary so the result stays valid UTF-8 in JSON.
func truncateReason(reason string) string {
	if len(reason) <= maxReasonBytes {
		return reason
	}
	return strings.ToValidUTF8(reason[:maxReasonBytes-len(reasonTruncated)], "") + reasonTruncated
}
