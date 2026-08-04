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
		warned            = fileChangedWarningRecord + "\r\n" + createJSONPayloadTTY + "\r\n"
		twiceWarned       = fileChangedWarningRecord + "\r\n" + fileChangedWarningRecord + "\r\n" + createJSONPayloadTTY + "\r\n"
		permissionDenied  = backupPermissionErrorRecord + "\r\n" + createJSONPayloadTTY + "\r\n"
		mixedWarnings     = fileChangedWarningRecord + "\r\n" + backupPermissionErrorRecord + "\r\n" + createJSONPayloadTTY + "\r\n"
		vanished          = backupFileNotFoundErrorRecord + "\r\n" + createJSONPayloadTTY + "\r\n"
		vanishedAndDenied = backupFileNotFoundErrorRecord + "\r\n" + backupPermissionErrorRecord + "\r\n" + createJSONPayloadTTY + "\r\n"
		bothBenign        = fileChangedWarningRecord + "\r\n" + backupFileNotFoundErrorRecord + "\r\n" + createJSONPayloadTTY + "\r\n"
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
			// The file is gone from the archive because it is gone from the volume, so the
			// archive still matches what was there to back up.
			name: "vanished file over a committed archive",
			res:  ExecResult{ExitCode: borgWarningExit, Response: vanished},
			want: true,
		},
		{
			// Both benign, and for different reasons — neither has to vouch for the other.
			name: "a torn file and a vanished file together",
			res:  ExecResult{ExitCode: borgWarningExit, Response: bothBenign},
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
			// Same rule for the newer benign entry: a file that vanished says nothing about
			// the file borg could not read, which IS still on the volume.
			name: "vanished and data-affecting warnings together",
			res:  ExecResult{ExitCode: borgWarningExit, Response: vanishedAndDenied},
			want: false,
		},
		{
			// borg's error tier is never downgradeable, whatever the records say.
			name: "vanished file at the error tier",
			res:  ExecResult{ExitCode: 2, Response: vanished},
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
			// All three measured create warnings in one response: the gate rules on the whole
			// list, so every kind has to survive the scan.
			name:     "all three warning kinds",
			response: backupFileNotFoundErrorRecord + "\r\n" + fileChangedWarningRecord + "\r\n" + backupPermissionErrorRecord + "\r\n",
			want:     []string{"BackupFileNotFoundError", "FileChangedWarning", "BackupPermissionError"},
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
		{name: "vanished only", response: backupFileNotFoundErrorRecord + "\r\n" + createJSONPayloadTTY, wantMsgID: ""},
		{
			name:      "both benign kinds",
			response:  fileChangedWarningRecord + "\r\n" + backupFileNotFoundErrorRecord + "\r\n" + createJSONPayloadTTY,
			wantMsgID: "",
		},
		{name: "non-benign only", response: backupPermissionErrorRecord + "\r\n", wantMsgID: "BackupPermissionError"},
		{
			// The reason a busy volume must still report the real offender: the vanished
			// files are the loud majority and the unreadable file is the one that matters.
			name:      "vanished before non-benign",
			response:  backupFileNotFoundErrorRecord + "\r\n" + backupFileNotFoundErrorRecord + "\r\n" + backupPermissionErrorRecord + "\r\n" + createJSONPayloadTTY,
			wantMsgID: "BackupPermissionError",
		},
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

// The measured `borg extract` records, captured on borg 1.4.4 against
// ghcr.io/computestacks/cs-docker-borg:latest with the agent's own flag set and NO --error.
// With --error every one of these responses is empty, which is what left a partial restore
// reporting "borg extract exited 1: no diagnostic output".
//
// The first three are one run: an extract into a 2 MiB tmpfs of an archive holding a 5 MB
// file. ENOSPC surfaced first in the per-item write (the WARNING, which names the file and
// the errno) and again in truncate_and_attrs inside the item context's __exit__, where it
// escaped as a top-level exception — so borg exited 2 and logged all three records, the
// ERROR pair saying nothing more than "Local Exception" and a traceback. big.bin was left
// at 2,093,056 of 5,000,000 bytes.
const (
	extractENOSPCWarningRecord   = `{"type": "log_message", "time": 1785772540.0208194, "message": "big.bin: write: [Errno 28] No space left on device", "levelname": "WARNING", "name": "borg.archiver", "msgid": "BackupOSError"}`
	extractLocalExceptionRecord  = `{"type": "log_message", "time": 1785772540.0228317, "message": "Local Exception", "levelname": "ERROR", "name": "borg.archiver", "msgid": "Exception"}`
	extractENOSPCTracebackRecord = `{"type": "log_message", "time": 1785772540.022891, "message": "Traceback (most recent call last):\n  File \"borg/archive.py\", line 858, in extract_item\nOSError: [Errno 28] No space left on device\n\nThe above exception was the direct cause of the following exception:\n\nTraceback (most recent call last):\n  File \"borg/archive.py\", line 850, in extract_item\n  File \"borg/archive.py\", line 214, in __exit__\nborg.helpers.errors.BackupOSError: truncate_and_attrs: [Errno 28] No space left on device\n\nDuring handling of the above exception, another exception occurred:\n\nTraceback (most recent call last):\n  File \"borg/archiver.py\", line 5759, in main\n  File \"borg/archiver.py\", line 5677, in run\n  File \"borg/archiver.py\", line 200, in wrapper\n  File \"borg/archiver.py\", line 215, in wrapper\n  File \"borg/archiver.py\", line 937, in do_extract\n  File \"borg/archive.py\", line 836, in extract_item\nOSError: [Errno 28] No space left on device\n\nPlatform: Linux a1be824fb056 6.12.94+deb13-cloud-amd64 #1 SMP PREEMPT_DYNAMIC Debian 6.12.94-1 (2026-06-20) x86_64\nLinux: Unknown Linux  \nBorg: 1.4.4  Python: CPython 3.11.14 msgpack: 1.1.2 fuse: llfuse 1.5.2 [pyfuse3,llfuse]\nPID: 51  CWD: /small\nsys.argv: ['borg', '--log-json', '--lock-wait', '1', 'extract', '--numeric-ids', '/repo::a1']\nSSH_ORIGINAL_COMMAND: None\n", "levelname": "ERROR", "name": "borg.archiver"}`
)

// The other warning-tier source, from the same image: an extract given an include path
// that matched nothing exits 1 and names the pattern. It is the only extract warning that
// is reachable solely through filePaths.
const includeNeverMatchedRecord = `{"type": "log_message", "time": 1785772537.663821, "message": "Include pattern 'nosuchpath' never matched.", "levelname": "WARNING", "name": "borg.archiver", "msgid": "IncludePatternNeverMatchedWarning"}`

// The rc 0 records. container.go exports BORG_RELOCATED_REPO_ACCESS_IS_OK=yes on every
// backup container, so an extract from a repository whose path has changed answers borg's
// prompt itself and logs this pair — captured verbatim from a repository moved from /repo
// to /repo2, where the extract then succeeded and exited 0. Neither record says anything
// about the restore, and publishing the prompt would tell a customer their successful
// restore had warned about something.
const (
	relocatedRepoQuestionPrompt = `{"type": "question_prompt", "msgid": "BORG_RELOCATED_REPO_ACCESS_IS_OK", "message": "Warning: The repository at location /repo2 was previously located at /repo\nDo you want to continue? [yN] "}`
	relocatedRepoQuestionAnswer = `{"env_var": "BORG_RELOCATED_REPO_ACCESS_IS_OK", "type": "question_env_answer", "msgid": "BORG_RELOCATED_REPO_ACCESS_IS_OK", "message": "yes (from BORG_RELOCATED_REPO_ACCESS_IS_OK)"}`
)

// Container.Exec allocates a TTY, so every record above arrives \r\n-framed.
var (
	// The measured out-of-space response, whole and in borg's own order: the WARNING that
	// names the file FIRST, then the two ERROR records that do not.
	extractENOSPCVariantB = strings.ReplaceAll(
		extractENOSPCWarningRecord+"\n"+extractLocalExceptionRecord+"\n"+extractENOSPCTracebackRecord+"\n", "\n", "\r\n")

	relocatedRepoQuestions = strings.ReplaceAll(
		relocatedRepoQuestionPrompt+"\n"+relocatedRepoQuestionAnswer+"\n", "\n", "\r\n")
)

// TestExtractCommand pins the flag set, and is above all the guard against someone
// re-adding --error to match the other subcommands. On extract that flag is worse than it
// was on create: borg's WARNING tier IS the set of partial restores, so with --error every
// measured partial restore printed nothing at all and reported "borg extract exited 1: no
// diagnostic output".
func TestExtractCommand(t *testing.T) {
	cmd := strings.Join(extractCommand("::auto-1", nil, "120"), " ")

	want := "cd /mnt/data && borg --log-json --lock-wait 120 extract --numeric-ids ::auto-1"
	if cmd != want {
		t.Errorf("Received %q, wanted %q", cmd, want)
	}
	if strings.Contains(cmd, "--error") {
		t.Errorf("Received %q, wanted no --error: it suppresses the only record that can explain a partial restore", cmd)
	}

	// The lock wait is whatever the caller resolved, not a literal in here: Restore reads
	// lockWait("restore") and passes it in, so viper is not needed to test the flag set.
	if got := strings.Join(extractCommand("::auto-1", nil, "7"), " "); !strings.Contains(got, "--lock-wait 7 ") {
		t.Errorf("Received %q, wanted the lock wait passed in", got)
	}

	// An empty slice must be indistinguishable from nil — the orchestration refuses
	// file_paths, so this is the shape every real call has.
	if got := strings.Join(extractCommand("::auto-1", []string{}, "120"), " "); got != want {
		t.Errorf("Received %q for an empty filePaths, wanted %q", got, want)
	}

	// Include paths follow the archive, and in the order given: borg reads the first
	// positional as ARCHIVE, so a path that precedes it is taken for the archive name —
	// measured as `borg extract: error: argument ARCHIVE: "dir1": No archive specified`.
	withPaths := strings.Join(extractCommand("::auto-1", []string{"dir1", "b.txt"}, "120"), " ")
	if withPaths != want+" dir1 b.txt" {
		t.Errorf("Received %q, wanted the archive path before the include paths", withPaths)
	}
}

// extractRecords must report EVERY record borg logged, in order — the per-item records are
// the only thing that names the paths a partial restore did not put back, and a failure
// reason can carry just one of them.
//
// The question-record exclusions are the ones with teeth: BORG_RELOCATED_REPO_ACCESS_IS_OK
// is set on every backup container, so without them every restore from a moved repository
// would publish borg's interactive prompt to the customer as a warning about a restore that
// exited 0.
func TestExtractRecords(t *testing.T) {
	for _, i := range []struct {
		name string
		in   string
		want []string
	}{
		{
			// The measured out-of-space response, in borg's own order.
			name: "every record in order",
			in:   extractENOSPCVariantB,
			want: []string{"BackupOSError", "Exception", ""},
		},
		{
			name: "the warning-tier record on its own",
			in:   strings.ReplaceAll(extractENOSPCWarningRecord+"\n", "\n", "\r\n"),
			want: []string{"BackupOSError"},
		},
		{
			name: "an unmatched include path",
			in:   strings.ReplaceAll(includeNeverMatchedRecord+"\n", "\n", "\r\n"),
			want: []string{"IncludePatternNeverMatchedWarning"},
		},
		{
			// rc 0 on a relocated repository. Nothing here is about the restore.
			name: "the relocated-repo question pair is excluded",
			in:   relocatedRepoQuestions,
			want: nil,
		},
		{
			// A question record must not shield the records around it either.
			name: "questions excluded from a real response",
			in:   relocatedRepoQuestions + extractENOSPCVariantB,
			want: []string{"BackupOSError", "Exception", ""},
		},
		{
			// The --stats table bypasses borg's level filter, so it turns up in responses
			// that have nothing else to say. It explains nothing about a restore.
			name: "the stats table is excluded",
			in:   statsRecord + "\r\n" + extractENOSPCWarningRecord + "\r\n" + statsRecord + "\r\n",
			want: []string{"BackupOSError"},
		},
		{
			// Whatever the shell or a wrapper wrote is not a record. It is still not lost:
			// failureRecord promotes it into the reason, which is where it belongs.
			name: "a non-JSON line contributes nothing",
			in:   "sh: 1: borg: not found\r\n",
			want: nil,
		},
		{name: "empty response", in: "", want: nil},
	} {
		t.Run(i.name, func(t *testing.T) {
			got := extractRecords(i.in)
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

// The regression this exists for: an extract that ran out of space exited 2 having named
// the file and the errno, and the agent reported "(Exception) Local Exception". The
// override must fire for exactly that shape and for nothing else — a real ERROR verdict
// about the whole command must never be displaced by a per-item warning.
func TestExtractFailure(t *testing.T) {
	// Derived from the measured response the way run() derives it, so the fixture itself
	// proves the bug still reproduces rather than being asserted against a hand-built pick.
	quoted, ok := failureRecord(extractENOSPCVariantB)
	if !ok {
		t.Fatal("fixture no longer parses: failureRecord found nothing in the measured response")
	}
	if quoted.MsgID != genericExtractMsgID {
		t.Fatalf("fixture no longer reproduces the bug: failureRecord chose %q, wanted %q", quoted.MsgID, genericExtractMsgID)
	}
	records := extractRecords(extractENOSPCVariantB)

	t.Run("prefers the record that names the file", func(t *testing.T) {
		got := extractFailure(ExecResult{ExitCode: 2, Response: extractENOSPCVariantB, Failure: &quoted}, records)
		if got.MsgID != "BackupOSError" {
			t.Fatalf("Received msgid %q, wanted BackupOSError — the record that names the file", got.MsgID)
		}
		if !strings.Contains(got.Message, "big.bin") || !strings.Contains(got.Message, "Errno 28") {
			t.Errorf("Received %q, wanted borg's own text naming the file and the errno", got.Message)
		}
	})

	// Each of these is a verdict about the whole command, and each arrives with the same
	// per-item records beside it — a restore can run out of space on one file while the
	// repository is also corrupt. The reason must stay the one that stopped borg.
	for _, i := range []struct {
		name    string
		failure *LogMessage
	}{
		{
			// Constructed, not captured: the measured integrity failure came back as a
			// plain traceback line rather than a record. The msgid is what is under test.
			name:    "IntegrityError",
			failure: &LogMessage{Message: "Data integrity error: Invalid segment magic", MsgID: "IntegrityError", LevelName: "ERROR"},
		},
		{
			name:    "LockTimeout",
			failure: mustRecord(t, lockTimeoutRecord),
		},
		{
			name:    "Archive.DoesNotExist",
			failure: mustRecord(t, archiveDoesNotExistRecord),
		},
		{
			// The case that matters most. failureRecord promotes a line that is not borg
			// JSON at all into a msgid-less reason, and that line can be the only
			// diagnosis in the response — `sh: borg: not found` says the command never
			// ran, which no per-item warning may overwrite.
			name:    "no msgid",
			failure: &LogMessage{Message: "sh: 1: borg: not found"},
		},
		{
			// A docker fault always arrives msgid-less, because dockerFailure strips it:
			// a verdict from a command that may never have run must not be
			// machine-actionable. So the override cannot reach this path.
			name:    "docker fault",
			failure: &LogMessage{Message: "Cannot connect to the Docker daemon"},
		},
	} {
		t.Run("keeps "+i.name, func(t *testing.T) {
			got := extractFailure(ExecResult{ExitCode: 2, Response: extractENOSPCVariantB, Failure: i.failure}, records)
			if got != i.failure {
				t.Errorf("Received %+v, wanted the reason borg gave for the command as a whole", got)
			}
		})
	}

	t.Run("keeps Exception when no per-item record is present", func(t *testing.T) {
		// Same msgid, nothing to prefer over it: the generic record is all borg said, so
		// it is still the best reason available.
		response := strings.ReplaceAll(extractLocalExceptionRecord+"\n"+extractENOSPCTracebackRecord+"\n", "\n", "\r\n")
		failure := mustRecord(t, extractLocalExceptionRecord)
		got := extractFailure(ExecResult{ExitCode: 2, Response: response, Failure: failure}, extractRecords(response))
		if got != failure {
			t.Errorf("Received %+v, wanted res.Failure untouched", got)
		}
	})

	t.Run("an unmatched include path is not displaced", func(t *testing.T) {
		// IncludePatternNeverMatchedWarning is not a per-item record — it names a pattern,
		// not a path borg failed to write — so it is not on the allowlist and cannot be
		// promoted over anything.
		response := strings.ReplaceAll(includeNeverMatchedRecord+"\n", "\n", "\r\n")
		failure := &LogMessage{Message: "Local Exception", MsgID: genericExtractMsgID, LevelName: "ERROR"}
		got := extractFailure(ExecResult{ExitCode: borgWarningExit, Response: response, Failure: failure}, extractRecords(response))
		if got != failure {
			t.Errorf("Received %+v, wanted res.Failure untouched", got)
		}
	})

	t.Run("no failure at all", func(t *testing.T) {
		if got := extractFailure(ExecResult{Response: extractENOSPCVariantB}, records); got != nil {
			t.Errorf("Received %+v, wanted nil: a clean exit has no reason to report", got)
		}
	})
}

// mustRecord parses a measured record fixture into the *LogMessage run() would have put in
// ExecResult.Failure, so the identity assertions above compare against borg's own text
// rather than a re-typed copy of it.
func mustRecord(t *testing.T, fixture string) *LogMessage {
	t.Helper()
	record, ok := failureRecord(fixture + "\r\n")
	if !ok {
		t.Fatalf("fixture did not parse as a record: %s", fixture)
	}
	return &record
}
