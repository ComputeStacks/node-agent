package borg

import (
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

// TestDecodeArchiveMessage covers what Create does with the output of a `borg create`
// that exited 0. Only a payload carrying an archive id is a decoded backup; everything
// else takes the "succeeded, but the response could not be decoded" path instead of
// logging a completion line for an archive it cannot name.
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
