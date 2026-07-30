package borg

import (
	"errors"
	"strings"
	"testing"
)

// These tests exercise classify, which is where the whole contract lives. RunBorg and
// RunShell add only a nil-container check and a call to Container.Exec, and neither the
// docker-error branch nor a real exec can be reached without a docker fake; a fake
// elaborate enough to be meaningful would mostly be testing itself, so that gap is
// stated rather than faked.

var classifyChecks = []struct {
	name      string
	label     string
	borgJSON  bool
	exitCode  int
	response  string
	wantNil   bool
	wantMsg   string
	wantMsgID string
}{
	{
		name:     "exit zero is success even with output",
		label:    "borg create",
		borgJSON: true,
		exitCode: 0,
		response: createJSONPayloadTTY,
		wantNil:  true,
	},
	{
		// borg's own severity calls this a warning; for us it means the delete did not
		// happen, and the uniform rule reports it.
		name:      "exit one with the delete not-found warning",
		label:     "borg delete",
		borgJSON:  true,
		exitCode:  1,
		response:  notFoundRecord + "\r\n" + statsRecord + "\r\n",
		wantMsg:   "Archive does-not-exist-logfix-9999 not found (1/1).",
		wantMsgID: "",
	},
	{
		name:      "stats only",
		label:     "borg delete",
		borgJSON:  true,
		exitCode:  2,
		response:  statsRecord + "\r\n" + statsRecord + "\r\n",
		wantMsg:   "borg delete exited 2: no diagnostic output",
		wantMsgID: "",
	},
	{
		name:      "empty response",
		label:     "borg prune",
		borgJSON:  true,
		exitCode:  2,
		response:  "",
		wantMsg:   "borg prune exited 2: no diagnostic output",
		wantMsgID: "",
	},
	{
		name:      "json payload only",
		label:     "borg create",
		borgJSON:  true,
		exitCode:  2,
		response:  createJSONPayloadTTY,
		wantMsg:   "borg create exited 2: no diagnostic output",
		wantMsgID: "",
	},
	{
		name:      "compact json payload only",
		label:     "borg create",
		borgJSON:  true,
		exitCode:  2,
		response:  compactJSONPayload + "\r\n",
		wantMsg:   "borg create exited 2: no diagnostic output",
		wantMsgID: "",
	},
	{
		name:      "non-json shell line",
		label:     "borg info",
		borgJSON:  true,
		exitCode:  2,
		response:  "sh: 1: borg: not found\r\n",
		wantMsg:   "sh: 1: borg: not found",
		wantMsgID: "",
	},
	{
		// The nil-container sentinel: nothing ran, and RunBorg supplies the reason
		// itself, but a code of 99 arriving here must still be a failure.
		name:      "no container sentinel",
		label:     "borg info",
		borgJSON:  true,
		exitCode:  execNoContainer,
		response:  "",
		wantMsg:   "borg info exited 99: no diagnostic output",
		wantMsgID: "",
	},
	{
		name:      "shell mode reports the output verbatim",
		label:     "restore snapshot",
		borgJSON:  false,
		exitCode:  1,
		response:  "mv: cannot stat '/mnt/data/*': No such file or directory\r\n",
		wantMsg:   "mv: cannot stat '/mnt/data/*': No such file or directory",
		wantMsgID: "",
	},
	{
		// Shell mode keeps every line: the rollback move is a chain, and a later line
		// can be the one that explains the failure. borg mode quotes one record, so
		// this is the case that tells the two modes apart.
		name:     "shell mode joins every line of output",
		label:    "restore rollback",
		borgJSON: false,
		exitCode: 1,
		response: "mv: cannot stat '/mnt/data/a': No such file or directory\r\n" +
			"mv: cannot stat '/mnt/data/b': No such file or directory\r\n",
		wantMsg: "mv: cannot stat '/mnt/data/a': No such file or directory; " +
			"mv: cannot stat '/mnt/data/b': No such file or directory",
		wantMsgID: "",
	},
	{
		name:      "shell mode with no output",
		label:     "restore rollback",
		borgJSON:  false,
		exitCode:  1,
		response:  "",
		wantMsg:   "restore rollback exited 1: no diagnostic output",
		wantMsgID: "",
	},
}

func TestClassify(t *testing.T) {
	for _, i := range classifyChecks {
		t.Run(i.name, func(t *testing.T) {
			got := classify(i.label, i.borgJSON, i.exitCode, i.response)
			if i.wantNil {
				if got != nil {
					t.Fatalf("Received %+v, wanted nil", got)
				}
				return
			}
			if got == nil {
				t.Fatal("Received nil, wanted a failure")
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

// Every measured borg failure carries a msgid, and callers act on it:
// Repository.DoesNotExist initialises a new volume's repository,
// Repository.AlreadyExists makes init idempotent. classify must pass both the msgid
// and borg's own wording through untouched.
func TestClassifyPreservesMeasuredMsgIDs(t *testing.T) {
	for _, i := range []struct {
		record  string
		msgID   string
		message string
	}{
		{repoDoesNotExistRecord, "Repository.DoesNotExist", "Repository /mnt/borg/backup does not exist."},
		{invalidRepositoryRecord, "Repository.InvalidRepository", "/mnt/borg/backup is not a valid repository. Check repo config."},
		{repoAlreadyExistsRecord, "Repository.AlreadyExists", "A repository already exists at /mnt/borg/backup."},
		{pathAlreadyExistsRecord, "Repository.PathAlreadyExists", "There is already something at /mnt/borg/backup."},
		{archiveDoesNotExistRecord, "Archive.DoesNotExist", "Archive does-not-exist does not exist"},
		{lockTimeoutRecord, "LockTimeout", "Failed to create/acquire the lock /mnt/borg/backup/lock.exclusive (timeout)."},
	} {
		t.Run(i.msgID, func(t *testing.T) {
			got := classify("borg info", true, 2, i.record+"\r\n")
			if got == nil {
				t.Fatal("Received nil, wanted a failure")
			}
			if got.MsgID != i.msgID {
				t.Errorf("Received msgid %q, wanted %q", got.MsgID, i.msgID)
			}
			if got.Message != i.message {
				t.Errorf("Received message %q, wanted %q", got.Message, i.message)
			}
			if got.LevelName != "ERROR" {
				t.Errorf("Received levelname %q, wanted %q", got.LevelName, "ERROR")
			}
		})
	}
}

// The reason is copied into a task's result_json and rides the changelog, so it is
// capped; the untruncated output stays in ExecResult.Response.
func TestClassifyTruncatesAnOversizedReason(t *testing.T) {
	long := strings.Repeat("mv: cannot stat '/mnt/data/x': No such file or directory ", 100)
	got := classify("restore snapshot", false, 1, long)
	if got == nil {
		t.Fatal("Received nil, wanted a failure")
	}
	if len(got.Message) > maxReasonBytes {
		t.Errorf("Received %d bytes, wanted at most %d", len(got.Message), maxReasonBytes)
	}
	if !strings.HasSuffix(got.Message, reasonTruncated) {
		t.Errorf("Received %q, wanted it to end with the truncation marker", got.Message)
	}
	if len(got.Message) >= len(long) {
		t.Errorf("Received %d bytes, wanted less than the %d-byte response", len(got.Message), len(long))
	}
}

// A reason that fits is not marked and not otherwise touched.
func TestClassifyLeavesAShortReasonAlone(t *testing.T) {
	got := classify("restore snapshot", false, 1, "mv: target is not a directory\r\n")
	if got == nil {
		t.Fatal("Received nil, wanted a failure")
	}
	if got.Message != "mv: target is not a directory" {
		t.Errorf("Received %q, wanted the output verbatim", got.Message)
	}
}

// The cap applies to borg's own msgid-bearing records too. Nothing parses Message —
// backup.go and restore.go branch on MsgID alone and borgFailure only concatenates it
// — so capping breaks no caller, while an uncapped message is an unbounded
// result_json riding the changelog. What must survive the cap are the fields callers
// do act on, and borg's own wording up to the cut.
func TestClassifyCapsABorgRecordAndKeepsItsFields(t *testing.T) {
	long := strings.Repeat("Repository /mnt/borg/backup does not exist. ", 100)
	record := `{"type": "log_message", "time": 1785448627.6905797, "message": "` + long +
		`", "levelname": "ERROR", "name": "borg.archiver", "msgid": "Repository.DoesNotExist"}`

	got := classify("borg info", true, 2, record+"\r\n")
	if got == nil {
		t.Fatal("Received nil, wanted a failure")
	}
	if len(got.Message) > maxReasonBytes {
		t.Errorf("Received %d bytes, wanted at most %d", len(got.Message), maxReasonBytes)
	}
	if !strings.HasSuffix(got.Message, reasonTruncated) {
		t.Errorf("Received %q, wanted it to end with the truncation marker", got.Message)
	}
	if !strings.HasPrefix(got.Message, "Repository /mnt/borg/backup does not exist.") {
		t.Errorf("Received %q, wanted borg's own wording up to the cut", got.Message)
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
	if got.Type != "log_message" {
		t.Errorf("Received type %q, wanted %q", got.Type, "log_message")
	}
	if got.Time != 1785448627.6905797 {
		t.Errorf("Received time %v, wanted 1785448627.6905797", got.Time)
	}
}

// The docker-error path drops the msgid from the FIELD on purpose: a fault that may
// have stopped the command before it ran is not grounds to auto-initialise a
// repository or to call an init a no-op. It is not a reason to hide the msgid from the
// operator, so it stays in the message as plain text.
func TestDockerFailure(t *testing.T) {
	dockerErr := errors.New("Error response from daemon: No such container: b-vol")

	t.Run("borg's msgid stays visible as plain text", func(t *testing.T) {
		got := dockerFailure(dockerErr, true, repoAlreadyExistsRecord+"\r\n")
		if got == nil {
			t.Fatal("Received nil, wanted a failure")
		}
		if got.MsgID != "" {
			t.Errorf("Received msgid %q, wanted no msgid on a docker fault", got.MsgID)
		}
		if !strings.Contains(got.Message, "(msgid Repository.AlreadyExists)") {
			t.Errorf("Received %q, wanted the msgid quoted as plain text", got.Message)
		}
		if !strings.Contains(got.Message, "A repository already exists at /mnt/borg/backup.") {
			t.Errorf("Received %q, wanted borg's own wording", got.Message)
		}
		if !strings.HasPrefix(got.Message, dockerErr.Error()+": ") {
			t.Errorf("Received %q, wanted the docker error kept as well", got.Message)
		}
	})

	t.Run("a record with no msgid adds no msgid text", func(t *testing.T) {
		got := dockerFailure(dockerErr, true, msgIDLessErrorRecord+"\r\n")
		if got == nil {
			t.Fatal("Received nil, wanted a failure")
		}
		if got.Message != dockerErr.Error()+": Local Exception" {
			t.Errorf("Received %q, wanted the docker error plus borg's message", got.Message)
		}
	})

	t.Run("shell output is appended as-is", func(t *testing.T) {
		got := dockerFailure(dockerErr, false, "mv: target is not a directory\r\n")
		if got == nil {
			t.Fatal("Received nil, wanted a failure")
		}
		if got.Message != dockerErr.Error()+": mv: target is not a directory" {
			t.Errorf("Received %q, wanted the docker error plus the output", got.Message)
		}
	})

	t.Run("no output leaves the docker error alone", func(t *testing.T) {
		got := dockerFailure(dockerErr, true, "")
		if got == nil {
			t.Fatal("Received nil, wanted a failure")
		}
		if got.Message != dockerErr.Error() {
			t.Errorf("Received %q, wanted just the docker error", got.Message)
		}
	})
}
