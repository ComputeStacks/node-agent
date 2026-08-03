package borg

import (
	"os"
	"strings"
	"testing"
)

// generateName's failure paths that do not need a container: a nil repository, and a
// repository whose Contents() fails. The rest of Archive.Create is not unit-testable
// without a docker fake.
func TestGenerateNameMissingRepository(t *testing.T) {
	a := &Archive{Name: "auto-1"}

	reason := a.generateName()
	if reason == nil {
		t.Fatal("expected a reason for a nil repository, got nil")
	}
	// No MsgID: this reason is synthesized here, and backup.Perform routes an
	// auto-init on borg's msgid alone. A reason we invented must never reach it.
	if reason.MsgID != "" {
		t.Errorf("synthesized reason carries MsgID %q, want empty", reason.MsgID)
	}
	if reason.Message == "" {
		t.Error("reason has no message")
	}
}

// The reason Contents() gives is passed through, not replaced with "Unable to
// generate unique archive name" — that substitution is what hid borg's own
// diagnosis of a failed `borg list` behind a naming error.
func TestGenerateNameCarriesContentsReason(t *testing.T) {
	a := &Archive{Name: "auto-1", Repository: &Repository{Name: "vol"}}

	contentsReason := ""
	if _, err := a.Repository.Contents(); err != nil {
		contentsReason = err.Message
	}
	if contentsReason == "" {
		t.Fatal("Contents() on a container-less repository should fail")
	}

	reason := a.generateName()
	if reason == nil {
		t.Fatal("expected a reason when Contents() fails, got nil")
	}
	if reason.Message != contentsReason {
		t.Errorf("reason = %q, want Contents()'s own reason %q", reason.Message, contentsReason)
	}
}

// TestDecodeArchiveMessage covers what Create does with the output of a `borg create`,
// on a clean exit and on the warning tier alike. Only a payload carrying an archive id is
// a decoded backup; everything else takes the "succeeded, but the response could not be
// decoded" path instead of logging a completion line for an archive it cannot name — and
// on a non-zero exit it is what denies wroteCompleteArchive its evidence of a commit.
// The first seven cases predate the payload extraction and are its regression guard.
func TestDecodeArchiveMessage(t *testing.T) {
	for _, i := range []struct {
		name     string
		response string
		wantOK   bool
	}{
		{
			// The real thing: borg pretty-prints --json, and Container.Exec's TTY
			// frames it \r\n.
			name:     "real create --json payload",
			response: strings.ReplaceAll(createJSONPayload, "\n", "\r\n") + "\r\n",
			wantOK:   true,
		},
		{
			// The case that produced `Completed backup archive= duration=0.00000`: a
			// --log-json record shares no keys with the payload, so it unmarshals
			// cleanly into an all-zero ArchiveMessage.
			name:     "a log record decodes to no archive",
			response: repoDoesNotExistRecord + "\r\n",
			wantOK:   false,
		},
		{
			name:     "stats record decodes to no archive",
			response: statsRecord + "\r\n",
			wantOK:   false,
		},
		{
			// Valid JSON, right shape, no id — the same verdict as the record above,
			// which is the whole point of checking the id rather than the unmarshal.
			name:     "payload with an empty archive id",
			response: `{"archive": {"duration": 0.5, "name": "auto-1"}}` + "\r\n",
			wantOK:   false,
		},
		{name: "multiple records do not unmarshal", response: repoDoesNotExistRecord + "\r\n" + statsRecord + "\r\n", wantOK: false},
		{name: "empty response", response: "", wantOK: false},
		{name: "non-JSON output", response: "sh: borg: not found\r\n", wantOK: false},
		{
			// The warning run's measured shape now that create runs without --error: one
			// record per warned file, records BEFORE the payload. Two concatenated JSON
			// documents, so the whole response cannot be unmarshalled as one — and the
			// archive id in the payload is the only proof borg committed an archive.
			name:     "a warning record before the payload",
			response: fileChangedWarningRecord + "\r\n" + createJSONPayloadTTY + "\r\n",
			wantOK:   true,
		},
		{
			// Order-agnostic on purpose: the measured order is records first, and nothing
			// about borg's logging guarantees it stays that way.
			name:     "a warning record after the payload",
			response: createJSONPayloadTTY + "\r\n" + fileChangedWarningRecord + "\r\n",
			wantOK:   true,
		},
		{
			// One record per warned file, so N of them. Whether these two warnings may be
			// downgraded is wroteCompleteArchive's question, not the decoder's.
			name:     "two warning records before the payload",
			response: fileChangedWarningRecord + "\r\n" + backupPermissionErrorRecord + "\r\n" + createJSONPayloadTTY + "\r\n",
			wantOK:   true,
		},
		{
			// rc 0 with a record present: borg logs some things without setting an exit
			// code, so "exited 0" is not a promise of a record-free response. This is the
			// combination that would silently drop the archive stats if the decoder went
			// back to unmarshalling the whole response.
			name:     "clean exit with a stray record still decodes",
			response: statsRecord + "\r\n" + createJSONPayloadTTY + "\r\n",
			wantOK:   true,
		},
		{
			// Dropping the record leaves nothing to unmarshal, which is the same verdict
			// the old whole-response unmarshal reached by a different route.
			name:     "a warning record with no payload",
			response: fileChangedWarningRecord + "\r\n",
			wantOK:   false,
		},
		{
			name:     "a warning record with an empty-id payload",
			response: fileChangedWarningRecord + "\r\n" + `{"archive": {"duration": 0.5, "name": "auto-1"}}` + "\r\n",
			wantOK:   false,
		},
	} {
		t.Run(i.name, func(t *testing.T) {
			msg, ok := decodeArchiveMessage(i.response)
			if ok != i.wantOK {
				t.Fatalf("Received ok=%v, wanted %v", ok, i.wantOK)
			}
			if !ok {
				if msg != (ArchiveMessage{}) {
					t.Errorf("Received %+v alongside ok=false, wanted the zero value", msg)
				}
				return
			}
			if msg.Archive.ID == "" {
				t.Error("Received ok=true with no archive id")
			}
		})
	}
}

// Dropping the records must leave the payload whole, not merely parseable: the caller
// records the archive id and the stats, and a decoder that kept only part of a
// pretty-printed payload would still unmarshal and report a backup with no files.
func TestDecodeArchiveMessageKeepsThePayloadWhole(t *testing.T) {
	msg, ok := decodeArchiveMessage(fileChangedWarningRecord + "\r\n" + createJSONPayloadTTY + "\r\n")
	if !ok {
		t.Fatal("Received ok=false, wanted the payload alongside the warning record")
	}
	if msg.Archive.ID != "e244f39f798b298d8e8849033e54f960870504daf34282d5121686e3c56224e6" {
		t.Errorf("Received id %q, wanted %q", msg.Archive.ID, "e244f39f798b298d8e8849033e54f960870504daf34282d5121686e3c56224e6")
	}
	if msg.Archive.Stats.FileCount != 2 {
		t.Errorf("Received nfiles %d, wanted 2", msg.Archive.Stats.FileCount)
	}
	if msg.Archive.Duration != 0.006694 {
		t.Errorf("Received duration %v, wanted 0.006694", msg.Archive.Duration)
	}
}

// TestWroteCompleteArchive is the false-green guard. A `borg create` that exited non-zero
// may be reported as a success ONLY when borg itself said it committed an archive and
// every warning it logged is one that leaves the archive complete. decoded and the archive
// id are derived from the response here, exactly as Create derives them, so the gate is
// exercised as it is actually composed.
func TestWroteCompleteArchive(t *testing.T) {
	var (
		warned           = fileChangedWarningRecord + "\r\n" + createJSONPayloadTTY + "\r\n"
		twiceWarned      = fileChangedWarningRecord + "\r\n" + fileChangedWarningRecord + "\r\n" + createJSONPayloadTTY + "\r\n"
		permissionDenied = backupPermissionErrorRecord + "\r\n" + createJSONPayloadTTY + "\r\n"
		mixedWarnings    = fileChangedWarningRecord + "\r\n" + backupPermissionErrorRecord + "\r\n" + createJSONPayloadTTY + "\r\n"
	)

	for _, i := range []struct {
		name string
		res  ExecResult
		want bool
	}{
		{
			name: "benign warning over a committed archive",
			res:  ExecResult{ExitCode: borgWarningExit, Response: warned},
			want: true,
		},
		{
			// One record per warned file, and a busy volume warns about several.
			name: "several benign warnings",
			res:  ExecResult{ExitCode: borgWarningExit, Response: twiceWarned},
			want: true,
		},
		{
			// Container.Exec hardcodes 1 on a docker fault and one of those paths returns
			// the full captured output with it, so this response is indistinguishable from
			// the warning tier by exit code alone. borg's real code is unknown.
			name: "docker fault with a perfect payload",
			res:  ExecResult{ExitCode: borgWarningExit, DockerFault: true, Response: warned},
			want: false,
		},
		{
			name: "error tier with a perfect payload",
			res:  ExecResult{ExitCode: 2, Response: warned},
			want: false,
		},
		{
			// Nothing ran at all; there is no borg verdict to downgrade.
			name: "no container sentinel",
			res:  ExecResult{ExitCode: execNoContainer, Failure: &LogMessage{Message: "Missing backup container"}},
			want: false,
		},
		{
			// rc 1 with nothing to inspect: no grounds to call the warning benign. This is
			// also what today's --error behavior looks like, so the fix cannot be shipped
			// without dropping the flag.
			name: "warning tier with no records",
			res:  ExecResult{ExitCode: borgWarningExit, Response: createJSONPayloadTTY},
			want: false,
		},
		{
			// Same exit code, same WARNING severity, and the file is absent from the
			// archive — the case that makes an exit-code-only gate unsafe.
			name: "permission warning alone",
			res:  ExecResult{ExitCode: borgWarningExit, Response: permissionDenied},
			want: false,
		},
		{
			// One benign warning must never vouch for the data-affecting one beside it.
			name: "benign and data-affecting warnings together",
			res:  ExecResult{ExitCode: borgWarningExit, Response: mixedWarnings},
			want: false,
		},
		{
			name: "warning record with no payload",
			res:  ExecResult{ExitCode: borgWarningExit, Response: fileChangedWarningRecord + "\r\n"},
			want: false,
		},
		{
			name: "warning record with an empty-id payload",
			res:  ExecResult{ExitCode: borgWarningExit, Response: fileChangedWarningRecord + "\r\n" + `{"archive": {"duration": 0.5}}` + "\r\n"},
			want: false,
		},
		{
			// Anything the decoder cannot read back is not evidence of a commit.
			name: "warning record with unreadable output",
			res:  ExecResult{ExitCode: borgWarningExit, Response: fileChangedWarningRecord + "\r\nsh: 1: borg: not found\r\n"},
			want: false,
		},
	} {
		t.Run(i.name, func(t *testing.T) {
			msg, decoded := decodeArchiveMessage(i.res.Response)
			if got := wroteCompleteArchive(i.res, decoded, msg.Archive.ID); got != i.want {
				t.Errorf("Received %v, wanted %v", got, i.want)
			}
		})
	}
}

// createWarnings must report EVERY record, in order: the gate rules on all of them, and a
// helper that returned only the best one (as failureRecord deliberately does) would let a
// benign warning vouch for a data-affecting one.
func TestCreateWarnings(t *testing.T) {
	for _, i := range []struct {
		name     string
		response string
		want     []string
	}{
		{
			name:     "one record per warned file, in order",
			response: fileChangedWarningRecord + "\r\n" + backupPermissionErrorRecord + "\r\n" + fileChangedWarningRecord + "\r\n",
			want:     []string{"FileChangedWarning", "BackupPermissionError", "FileChangedWarning"},
		},
		{
			// A pretty-printed payload's lines are not records, so the payload around the
			// records contributes nothing.
			name:     "records mixed with the payload",
			response: fileChangedWarningRecord + "\r\n" + createJSONPayloadTTY + "\r\n" + backupPermissionErrorRecord + "\r\n",
			want:     []string{"FileChangedWarning", "BackupPermissionError"},
		},
		{name: "payload only", response: createJSONPayloadTTY, want: nil},
		{name: "compact payload only", response: compactJSONPayload + "\r\n", want: nil},
		{name: "non-JSON output", response: "sh: 1: borg: not found\r\n", want: nil},
		{name: "empty response", response: "", want: nil},
	} {
		t.Run(i.name, func(t *testing.T) {
			got := createWarnings(i.response)
			if len(got) != len(i.want) {
				t.Fatalf("Received %d records (%+v), wanted %d", len(got), got, len(i.want))
			}
			for n, w := range i.want {
				if got[n].MsgID != w {
					t.Errorf("Received msgid %q at %d, wanted %q", got[n].MsgID, n, w)
				}
				if got[n].Message == "" {
					t.Errorf("Received an empty message at %d, wanted borg's own text", n)
				}
			}
		})
	}
}

// TestCreateCommand pins the flag set, and is the guard against someone re-adding
// --error to match the other subcommands: --error filters the WARNING record that says
// which file warned and why, which is both the reason a successful backup reported "no
// diagnostic output" and the input wroteCompleteArchive rules on.
func TestCreateCommand(t *testing.T) {
	cmd := strings.Join(createCommand("::auto-1", "zstd", "600"), " ")

	want := "cd /mnt/data && borg --log-json --lock-wait 600 " +
		"create --one-file-system --json --numeric-ids --exclude-caches --compression zstd ::auto-1 ."
	if cmd != want {
		t.Errorf("Received %q, wanted %q", cmd, want)
	}
	if strings.Contains(cmd, "--error") {
		t.Errorf("Received %q, wanted no --error: it filters the warning record the success gate reads", cmd)
	}
	for _, flag := range []string{"--json", "--log-json", "--one-file-system", "--numeric-ids", "--exclude-caches"} {
		if !strings.Contains(cmd, flag) {
			t.Errorf("Received %q, wanted it to carry %q", cmd, flag)
		}
	}
}

// nonBenignCreateWarning must find the disqualifying record wherever it sits in the
// response, because borg emits one record per warned file in the order it walked them.
func TestNonBenignCreateWarning(t *testing.T) {
	for _, i := range []struct {
		name      string
		response  string
		wantMsgID string
	}{
		{name: "no records", response: createJSONPayloadTTY, wantMsgID: ""},
		{name: "benign only", response: fileChangedWarningRecord + "\r\n" + createJSONPayloadTTY, wantMsgID: ""},
		{
			name:      "two benign",
			response:  fileChangedWarningRecord + "\r\n" + fileChangedWarningRecord + "\r\n" + createJSONPayloadTTY,
			wantMsgID: "",
		},
		{name: "non-benign only", response: backupPermissionErrorRecord + "\r\n", wantMsgID: "BackupPermissionError"},
		{
			// The order that matters: the benign record comes FIRST, which is what
			// failureRecord would have quoted.
			name:      "benign before non-benign",
			response:  fileChangedWarningRecord + "\r\n" + backupPermissionErrorRecord + "\r\n" + createJSONPayloadTTY,
			wantMsgID: "BackupPermissionError",
		},
		{
			name:      "non-benign before benign",
			response:  backupPermissionErrorRecord + "\r\n" + fileChangedWarningRecord + "\r\n" + createJSONPayloadTTY,
			wantMsgID: "BackupPermissionError",
		},
		{
			// A record with no msgid is not on the allowlist, so it disqualifies.
			name:      "msgid-less record disqualifies",
			response:  msgIDLessErrorRecord + "\r\n",
			wantMsgID: "",
		},
	} {
		t.Run(i.name, func(t *testing.T) {
			got := nonBenignCreateWarning(i.response)
			if i.name == "msgid-less record disqualifies" {
				if got == nil {
					t.Fatal("Received nil, wanted the msgid-less record to disqualify")
				}
				return
			}
			if i.wantMsgID == "" {
				if got != nil {
					t.Fatalf("Received %+v, wanted nil", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("Received nil, wanted msgid %q", i.wantMsgID)
			}
			if got.MsgID != i.wantMsgID {
				t.Errorf("Received msgid %q, wanted %q", got.MsgID, i.wantMsgID)
			}
		})
	}
}

// The regression this exists for: a create refused by wroteCompleteArchive must report the
// warning that DISQUALIFIED it, not failureRecord's severity-based pick. Every create
// warning is levelname WARNING with a msgid, so failureRecord's tie-break never fires and
// the first record in the response wins — whichever file borg walked first. On a volume
// with a busy file and an unreadable one, that reported "file changed while we backed it
// up" for a backup that failed because a different file was missing from the archive.
func TestCreateFailureReportsTheDisqualifyingWarning(t *testing.T) {
	a := &Archive{Name: "auto-1", Repository: &Repository{Name: "vol"}}

	// Benign record first, so a severity-based pick would choose it.
	response := fileChangedWarningRecord + "\r\n" + backupPermissionErrorRecord + "\r\n" + createJSONPayloadTTY

	t.Run("prefers the disqualifying record over failureRecord's pick", func(t *testing.T) {
		quoted, _ := failureRecord(response)
		if quoted.MsgID != "FileChangedWarning" {
			t.Fatalf("fixture no longer reproduces the bug: failureRecord chose %q, wanted FileChangedWarning", quoted.MsgID)
		}
		got := a.createFailure(ExecResult{ExitCode: borgWarningExit, Response: response, Failure: &quoted})
		if got.MsgID != "BackupPermissionError" {
			t.Errorf("Received msgid %q, wanted BackupPermissionError — the warning that failed the allowlist", got.MsgID)
		}
	})

	t.Run("a docker fault keeps its own reason", func(t *testing.T) {
		dockerReason := &LogMessage{Message: "Cannot connect to the Docker daemon"}
		got := a.createFailure(ExecResult{ExitCode: borgWarningExit, DockerFault: true, Response: response, Failure: dockerReason})
		if got != dockerReason {
			t.Errorf("Received %+v, wanted the docker reason: the command may never have run", got)
		}
	})

	t.Run("borg's error tier keeps failureRecord's pick", func(t *testing.T) {
		errorReason := &LogMessage{Message: "Repository does not exist.", MsgID: "Repository.DoesNotExist", LevelName: "ERROR"}
		got := a.createFailure(ExecResult{ExitCode: 2, Response: response, Failure: errorReason})
		if got != errorReason {
			t.Errorf("Received %+v, wanted failureRecord's pick: an ERROR record outranks a WARNING", got)
		}
	})
}

// A source-level guard, in the style of TestBackupPathUsesBackupContinueOnError: the
// branch itself needs docker and a borg repository, so nothing short of an integration
// environment can observe Create reporting success on a non-zero exit. What is checkable
// here is that the one place it may do so is still gated on wroteCompleteArchive — the
// whole of the false-green protection — and that create still asks for the records the
// gate reads.
func TestCreateGatesSuccessOnWroteCompleteArchive(t *testing.T) {
	src, err := os.ReadFile("archive.go")
	if err != nil {
		t.Fatalf("read archive.go: %v", err)
	}
	if !strings.Contains(string(src), "res.Failure != nil && !wroteCompleteArchive(res, decoded, borgResponse.Archive.ID)") {
		t.Error("Create no longer gates its non-zero-exit return on wroteCompleteArchive; a create that " +
			"exited non-zero may only be reported as a success through that gate")
	}
	if strings.Contains(string(src), "create --error") {
		t.Error("archive.go passes --error to borg create; it filters the warning record wroteCompleteArchive reads")
	}
}
