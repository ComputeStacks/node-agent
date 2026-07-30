package borg

import (
	"strings"
	"testing"
)

// Real borg 1.4.4 --log-json records, captured against the image the node runs.
// Container.Exec allocates a TTY, so records arrive \r\n-framed. Every captured
// failure record is levelname ERROR, name borg.archiver, and carries a msgid.
const (
	repoDoesNotExistRecord    = `{"type": "log_message", "time": 1785448627.6905797, "message": "Repository /mnt/borg/backup does not exist.", "levelname": "ERROR", "name": "borg.archiver", "msgid": "Repository.DoesNotExist"}`
	invalidRepositoryRecord   = `{"type": "log_message", "time": 1785448628.6075294, "message": "/mnt/borg/backup is not a valid repository. Check repo config.", "levelname": "ERROR", "name": "borg.archiver", "msgid": "Repository.InvalidRepository"}`
	repoAlreadyExistsRecord   = `{"type": "log_message", "time": 1785448630.492301, "message": "A repository already exists at /mnt/borg/backup.", "levelname": "ERROR", "name": "borg.archiver", "msgid": "Repository.AlreadyExists"}`
	pathAlreadyExistsRecord   = `{"type": "log_message", "time": 1785449436.8248563, "message": "There is already something at /mnt/borg/backup.", "levelname": "ERROR", "name": "borg.archiver", "msgid": "Repository.PathAlreadyExists"}`
	archiveDoesNotExistRecord = `{"type": "log_message", "time": 1785448632.4613242, "message": "Archive does-not-exist does not exist", "levelname": "ERROR", "name": "borg.archiver", "msgid": "Archive.DoesNotExist"}`
	lockTimeoutRecord         = `{"type": "log_message", "time": 1785449472.4485624, "message": "Failed to create/acquire the lock /mnt/borg/backup/lock.exclusive (timeout).", "levelname": "ERROR", "name": "borg.archiver", "msgid": "LockTimeout"}`
	commandErrorRecord        = `{"type": "log_message", "time": 1785448776.8795714, "message": "Command Error: At least one of the \"keep-within\", \"keep-last\", \"keep-secondly\", \"keep-minutely\", \"keep-hourly\", \"keep-daily\", \"keep-weekly\", \"keep-monthly\", \"keep-13weekly\", \"keep-3monthly\", or \"keep-yearly\" settings must be specified.", "levelname": "ERROR", "name": "borg.archiver", "msgid": "CommandError"}`
)

// Constructed rather than captured: no msgid-less ERROR and no question record turned
// up in the characterization runs, but both shapes are ones failureRecord must handle
// (the msgid tie-break, and the question protocol responder.go already excludes).
const (
	msgIDLessErrorRecord = `{"type": "log_message", "time": 1785448640.1000000, "message": "Local Exception", "levelname": "ERROR", "name": "borg.archiver"}`
	questionPromptRecord = `{"type": "question_prompt", "msgid": "unencrypted_repo_access_is_ok", "message": "Warning: Attempting to access a previously unknown unencrypted repository!", "is_prompt": true}`
)

// A real `borg create --json` data payload. borg pretty-prints --json with indent=4,
// so no line of it is valid JSON on its own — and the whole object unmarshals into
// LogMessage as all-zero fields. Neither the object nor any of its lines may ever be
// reported as a failure.
const createJSONPayload = `{
    "archive": {
        "command_line": [
            "borg",
            "--log-json",
            "create",
            "--json",
            "::manual-1",
            "."
        ],
        "duration": 0.006694,
        "end": "2026-07-29T21:37:07.000000",
        "id": "e244f39f798b298d8e8849033e54f960870504daf34282d5121686e3c56224e6",
        "limits": {
            "max_archive_size": 4.6621141804731045e-06
        },
        "name": "manual-1",
        "start": "2026-07-29T21:37:07.000000",
        "stats": {
            "compressed_size": 98,
            "deduplicated_size": 98,
            "nfiles": 2,
            "original_size": 12
        }
    },
    "cache": {
        "path": "/mnt/borg/cache/6f3bb1a0",
        "stats": {
            "total_chunks": 3,
            "total_csize": 173,
            "total_size": 53,
            "total_unique_chunks": 3,
            "unique_csize": 173,
            "unique_size": 53
        }
    },
    "encryption": {
        "mode": "repokey-blake2"
    },
    "repository": {
        "id": "6f3bb1a0e1cbb0d78f37a2b1d8a1ee0f6f4c1c8a0d2b3e4f5061728394a5b6c7",
        "last_modified": "2026-07-29T21:37:07.000000",
        "location": "/mnt/borg/backup"
    }
}`

// createJSONPayloadTTY reproduces the framing Container.Exec's TTY produces.
var createJSONPayloadTTY = strings.ReplaceAll(createJSONPayload, "\n", "\r\n")

// The same payload on one line. The fragment guard cannot help here — this IS valid
// JSON, and it unmarshals into LogMessage as all-zero fields because none of its keys
// match. Only the Message-or-MsgID test rejects it, which is why that test exists.
const compactJSONPayload = `{"archive": {"duration": 0.006694, "id": "e244f39f798b298d", "name": "manual-1", "stats": {"compressed_size": 98, "nfiles": 2, "original_size": 12}}, "cache": {"path": "/mnt/borg/cache/6f3bb1a0"}, "encryption": {"mode": "repokey-blake2"}, "repository": {"id": "6f3bb1a0e1cbb0d7", "location": "/mnt/borg/backup"}}`

var failureRecordChecks = []struct {
	name      string
	in        string
	wantOK    bool
	wantMsg   string
	wantMsgID string
}{
	{
		name:      "msgid-less warning",
		in:        notFoundRecord + "\r\n",
		wantOK:    true,
		wantMsg:   "Archive does-not-exist-logfix-9999 not found (1/1).",
		wantMsgID: "",
	},
	{
		// The --stats table bypasses borg's level filter, so on a failure it is often
		// the loudest thing present while explaining nothing.
		name:      "diagnosis with stats never quotes the stats table",
		in:        statsRecord + "\r\n" + notFoundRecord + "\r\n" + statsRecord + "\r\n" + statsRecord + "\r\n",
		wantOK:    true,
		wantMsg:   "Archive does-not-exist-logfix-9999 not found (1/1).",
		wantMsgID: "",
	},
	{
		name:   "stats only",
		in:     statsRecord + "\r\n" + statsRecord + "\r\n",
		wantOK: false,
	},
	{
		name:      "non-json shell error",
		in:        "sh: 1: borg: not found\r\n",
		wantOK:    true,
		wantMsg:   "sh: 1: borg: not found",
		wantMsgID: "",
	},
	{
		name:      "record beats a non-json line",
		in:        "sh: 1: borg: not found\r\n" + archiveDoesNotExistRecord + "\r\n",
		wantOK:    true,
		wantMsg:   "Archive does-not-exist does not exist",
		wantMsgID: "Archive.DoesNotExist",
	},
	{
		name:   "empty response",
		in:     "",
		wantOK: false,
	},
	{
		// Severity beats position: borg logs the warning on the way to the error.
		name:      "warning then error returns the error",
		in:        notFoundRecord + "\r\n" + commandErrorRecord + "\r\n",
		wantOK:    true,
		wantMsg:   "Command Error: At least one of the \"keep-within\", \"keep-last\", \"keep-secondly\", \"keep-minutely\", \"keep-hourly\", \"keep-daily\", \"keep-weekly\", \"keep-monthly\", \"keep-13weekly\", \"keep-3monthly\", or \"keep-yearly\" settings must be specified.",
		wantMsgID: "CommandError",
	},
	{
		name:      "two errors returns the first",
		in:        repoDoesNotExistRecord + "\r\n" + invalidRepositoryRecord + "\r\n",
		wantOK:    true,
		wantMsg:   "Repository /mnt/borg/backup does not exist.",
		wantMsgID: "Repository.DoesNotExist",
	},
	{
		// Equal severity, msgid-less first: only the tie-break can pick the second.
		name:      "equal severity prefers the msgid",
		in:        msgIDLessErrorRecord + "\r\n" + archiveDoesNotExistRecord + "\r\n",
		wantOK:    true,
		wantMsg:   "Archive does-not-exist does not exist",
		wantMsgID: "Archive.DoesNotExist",
	},
	{
		name:   "create --json payload only",
		in:     createJSONPayloadTTY,
		wantOK: false,
	},
	{
		name:      "create --json payload then an error",
		in:        createJSONPayloadTTY + "\r\n" + lockTimeoutRecord + "\r\n",
		wantOK:    true,
		wantMsg:   "Failed to create/acquire the lock /mnt/borg/backup/lock.exclusive (timeout).",
		wantMsgID: "LockTimeout",
	},
	{
		// The higher-severity record carries no msgid, so only the severity ordering
		// can pick it over the warning that came first.
		name:      "warning then msgid-less error returns the error",
		in:        notFoundRecord + "\r\n" + msgIDLessErrorRecord + "\r\n",
		wantOK:    true,
		wantMsg:   "Local Exception",
		wantMsgID: "",
	},
	{
		name:   "compact --json payload only",
		in:     compactJSONPayload + "\r\n",
		wantOK: false,
	},
	{
		name:      "compact --json payload then an error",
		in:        compactJSONPayload + "\r\n" + repoDoesNotExistRecord + "\r\n",
		wantOK:    true,
		wantMsg:   "Repository /mnt/borg/backup does not exist.",
		wantMsgID: "Repository.DoesNotExist",
	},
	{
		name:   "question prompt only",
		in:     questionPromptRecord + "\r\n",
		wantOK: false,
	},
}

func TestFailureRecord(t *testing.T) {
	for _, i := range failureRecordChecks {
		t.Run(i.name, func(t *testing.T) {
			got, ok := failureRecord(i.in)
			if ok != i.wantOK {
				t.Fatalf("Received ok=%v (%+v), wanted ok=%v", ok, got, i.wantOK)
			}
			if !ok {
				if got != (LogMessage{}) {
					t.Errorf("Received %+v, wanted the zero LogMessage when ok is false", got)
				}
				return
			}
			if got.Message != i.wantMsg {
				t.Errorf("Received message %q, wanted %q", got.Message, i.wantMsg)
			}
			if got.MsgID != i.wantMsgID {
				t.Errorf("Received msgid %q, wanted %q", got.MsgID, i.wantMsgID)
			}
		})
	}
}

// TestFailureRecordVerbatim is the auto-init regression guard: backup.Perform branches
// on MsgID == "Repository.DoesNotExist" to initialise a new volume's repository, and
// the failure reason rendered for the controller is built from MsgID plus Message. Any
// rewriting of borg's record breaks one of those, so all six fields are asserted.
func TestFailureRecordVerbatim(t *testing.T) {
	got, ok := failureRecord(repoDoesNotExistRecord + "\r\n")
	if !ok {
		t.Fatal("Received ok=false, wanted the record")
	}
	if got.Time != 1785448627.6905797 {
		t.Errorf("Received time %v, wanted 1785448627.6905797", got.Time)
	}
	if got.Type != "log_message" {
		t.Errorf("Received type %q, wanted %q", got.Type, "log_message")
	}
	if got.Message != "Repository /mnt/borg/backup does not exist." {
		t.Errorf("Received message %q, wanted %q", got.Message, "Repository /mnt/borg/backup does not exist.")
	}
	if got.MsgID != "Repository.DoesNotExist" {
		t.Errorf("Received msgid %q, wanted %q", got.MsgID, "Repository.DoesNotExist")
	}
	if got.LevelName != "ERROR" {
		t.Errorf("Received levelname %q, wanted %q", got.LevelName, "ERROR")
	}
	if got.Name != "borg.archiver" {
		t.Errorf("Received name %q, wanted %q", got.Name, "borg.archiver")
	}
}

// A pretty-printed payload's lines are not valid JSON, so without the fragment guard
// one of them becomes "the diagnosis" — and the first one that is not a record wins,
// which means the real error that follows is never reported.
func TestFailureRecordNeverQuotesAPayloadFragment(t *testing.T) {
	got, ok := failureRecord(createJSONPayloadTTY + "\r\n" + commandErrorRecord + "\r\n")
	if !ok {
		t.Fatal("Received ok=false, wanted the error record")
	}
	if got.MsgID != "CommandError" {
		t.Errorf("Received msgid %q, wanted %q", got.MsgID, "CommandError")
	}
	for _, line := range strings.Split(createJSONPayload, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" && got.Message == trimmed {
			t.Fatalf("Received a payload fragment as the reason: %q", got.Message)
		}
	}
}

// Every measured failure carries a msgid, and each one has a caller that reads it.
func TestFailureRecordMeasuredMsgIDs(t *testing.T) {
	for _, i := range []struct {
		record string
		msgID  string
	}{
		{repoDoesNotExistRecord, "Repository.DoesNotExist"},
		{invalidRepositoryRecord, "Repository.InvalidRepository"},
		{repoAlreadyExistsRecord, "Repository.AlreadyExists"},
		{pathAlreadyExistsRecord, "Repository.PathAlreadyExists"},
		{archiveDoesNotExistRecord, "Archive.DoesNotExist"},
		{lockTimeoutRecord, "LockTimeout"},
		{commandErrorRecord, "CommandError"},
	} {
		t.Run(i.msgID, func(t *testing.T) {
			got, ok := failureRecord(i.record + "\r\n")
			if !ok {
				t.Fatal("Received ok=false, wanted the record")
			}
			if got.MsgID != i.msgID {
				t.Errorf("Received msgid %q, wanted %q", got.MsgID, i.msgID)
			}
			if got.Message == "" {
				t.Error("Received an empty message, wanted borg's own text")
			}
		})
	}
}

// failureReason quotes a line verbatim when it is not JSON at all, because a shell
// error is the diagnosis. Valid JSON that is not a log record is a --json data
// payload and must contribute nothing — the distinction the two flags on borgLine
// exist to keep. It lives here rather than in failure_reason_test.go so that file
// stays exactly as it was, as the refactor's regression guard.
func TestFailureReasonIgnoresAJSONPayload(t *testing.T) {
	if got := failureReason(compactJSONPayload + "\r\n"); got != "" {
		t.Errorf("Received %q, wanted no reason for a data payload", got)
	}
}

var looksLikeJSONFragmentChecks = []struct {
	name string
	in   string
	want bool
}{
	{name: "opening brace", in: "{", want: true},
	{name: "closing brace with comma", in: "        },", want: true},
	{name: "key", in: `        "name": "manual-1"`, want: true},
	{name: "array element", in: `            "borg",`, want: true},
	{name: "array close", in: "        ],", want: true},
	{name: "blank", in: "   ", want: true},
	{name: "shell error", in: "sh: 1: borg: not found", want: false},
	{name: "mv error", in: "mv: cannot stat '/mnt/data/*': No such file or directory", want: false},
}

func TestLooksLikeJSONFragment(t *testing.T) {
	for _, i := range looksLikeJSONFragmentChecks {
		t.Run(i.name, func(t *testing.T) {
			if got := looksLikeJSONFragment(i.in); got != i.want {
				t.Errorf("Received %v, wanted %v", got, i.want)
			}
		})
	}
}

// Every line of a real payload must be recognised as a fragment; a single unrecognised
// line is enough to become a bogus failure reason.
func TestLooksLikeJSONFragmentCoversTheWholePayload(t *testing.T) {
	for _, line := range strings.Split(createJSONPayload, "\n") {
		if !looksLikeJSONFragment(line) {
			t.Errorf("Received false for a real payload line: %q", line)
		}
	}
}

var severityRankChecks = []struct {
	in   string
	want int
}{
	{in: "CRITICAL", want: 3},
	{in: "FATAL", want: 3},
	{in: "critical", want: 3},
	{in: "ERROR", want: 2},
	{in: "error", want: 2},
	{in: "WARNING", want: 1},
	{in: "WARN", want: 1},
	{in: "warning", want: 1},
	{in: "INFO", want: 0},
	{in: "", want: 0},
	{in: "nonsense", want: 0},
}

func TestSeverityRank(t *testing.T) {
	for _, i := range severityRankChecks {
		t.Run(i.in, func(t *testing.T) {
			if got := severityRank(i.in); got != i.want {
				t.Errorf("Received %d, wanted %d", got, i.want)
			}
		})
	}
}
