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

// ExecResult is the outcome of one command in the backup container.
type ExecResult struct {
	// ExitCode is the command's exit code, or execNoContainer when there was no
	// container to run it in.
	ExitCode int

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
	return r.run(label, true, cmd)
}

// RunShell runs a plain shell command in the backup container. Its output is not borg
// JSON, so the output itself is the reason, verbatim.
func (r *Repository) RunShell(label string, cmd []string) ExecResult {
	return r.run(label, false, cmd)
}

func (r *Repository) run(label string, borgJSON bool, cmd []string) ExecResult {
	if reflect.ValueOf(r.Container).IsNil() {
		return ExecResult{
			ExitCode: execNoContainer,
			Failure:  &LogMessage{Message: "Missing backup container"},
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
			ExitCode: exitCode,
			Response: response,
			Failure:  failure,
		}
	}

	res := ExecResult{ExitCode: exitCode, Response: response, Failure: classify(label, borgJSON, exitCode, response)}
	if res.Failure != nil {
		borgLogger().Warn("Command failed", "label", label, "exitCode", exitCode, "msgid", res.Failure.MsgID, "message", res.Failure.Message)
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
// exitCode != 0 is a failure. There is no warning tier: `borg create` was measured
// through five deterministic warning scenarios (files deleted, truncated, grown and
// removed mid-run, a unix socket in the tree) and exited 0 with no records every
// time, while both commands observed exiting 1 are ones where failing is the right
// answer — a delete of a missing archive did not delete anything, and an extract with
// an unmatched include path restored nothing.
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
