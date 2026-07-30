package borg

import "testing"

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
