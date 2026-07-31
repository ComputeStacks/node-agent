package borg

import "testing"

// recordMessage pulls borg's own text out of one of the captured --log-json records in
// failure_record_test.go, so these cases match production wording by construction
// rather than by a retyped copy of it.
func recordMessage(t *testing.T, raw string) string {
	t.Helper()
	record, ok := failureRecord(raw + "\r\n")
	if !ok {
		t.Fatalf("Received no record for %q, wanted borg's own", raw)
	}
	if record.Message == "" {
		t.Fatalf("Received an empty message for %q", raw)
	}
	return record.Message
}

// fullRecord pulls a whole captured --log-json record out of the fixtures in
// failure_record_test.go, so a decision made on a record's fields is tested against the
// same record production would hand it.
func fullRecord(t *testing.T, raw string) *LogMessage {
	t.Helper()
	record, ok := failureRecord(raw + "\r\n")
	if !ok {
		t.Fatalf("Received no record for %q, wanted borg's own", raw)
	}
	return &record
}

// emptyResponseMessage is what group 1's guard reports for a response with nothing in
// it. Read from the parser rather than copied, so the wording cannot drift apart from
// the thing this test says must not be mistaken for a missing repository.
func emptyResponseMessage(t *testing.T) string {
	t.Helper()
	_, failure := readRepoResponse("")
	if failure == nil {
		t.Fatal("Received nil from readRepoResponse(\"\"), wanted a failure")
	}
	return failure.Message
}

// TestLooksLikeMissingRepository pins the wording match on its own, because the
// consequence of a false positive is borg init running over a directory that already
// has content in it.
func TestLooksLikeMissingRepository(t *testing.T) {
	for _, i := range []struct {
		name string
		in   string
		want bool
	}{
		{name: "borg's does-not-exist wording", in: recordMessage(t, repoDoesNotExistRecord), want: true},
		{name: "ssh backend path", in: "Repository ssh://borg@backup.example.com:22/backups/b-vol/backup does not exist.", want: true},
		{name: "invalid repository is a different condition", in: recordMessage(t, invalidRepositoryRecord), want: false},
		{name: "path already has something at it", in: recordMessage(t, pathAlreadyExistsRecord), want: false},
		{name: "a missing archive is not a missing repository", in: recordMessage(t, archiveDoesNotExistRecord), want: false},
		{name: "empty response is a fault on this host", in: emptyResponseMessage(t), want: false},
		{name: "synthesized no-output reason", in: "borg info exited 2: no diagnostic output", want: false},
		{name: "empty message", in: "", want: false},
	} {
		t.Run(i.name, func(t *testing.T) {
			if got := looksLikeMissingRepository(i.in); got != i.want {
				t.Errorf("Received %v for %q, wanted %v", got, i.in, i.want)
			}
		})
	}
}

// TestStampMissingRepository covers the decision FindRepository makes on a failure from
// Repository.Info: only a msgid-less failure whose text is borg's does-not-exist wording
// becomes the auto-init verdict, and the message is never rewritten.
func TestStampMissingRepository(t *testing.T) {
	for _, i := range []struct {
		name      string
		in        LogMessage
		wantMsgID string
	}{
		{
			// The insurance path: borg said the repository is not there but the record
			// arrived without its msgid.
			name:      "does-not-exist wording without a msgid",
			in:        LogMessage{Message: recordMessage(t, repoDoesNotExistRecord)},
			wantMsgID: missingRepositoryMsgID,
		},
		{
			// A path that exists and holds something that is not a repository. Stamping
			// this would init over its contents.
			name:      "invalid repository wording without a msgid",
			in:        LogMessage{Message: recordMessage(t, invalidRepositoryRecord)},
			wantMsgID: "",
		},
		{
			name:      "empty response from borg",
			in:        LogMessage{Message: emptyResponseMessage(t)},
			wantMsgID: "",
		},
		{
			// Constructed to isolate the MsgID guard: borg's verdict wins over the
			// wording, so even a message that matches cannot overwrite a msgid borg
			// supplied.
			name:      "borg's own msgid is never overwritten",
			in:        LogMessage{MsgID: "Repository.InvalidRepository", Message: recordMessage(t, repoDoesNotExistRecord)},
			wantMsgID: "Repository.InvalidRepository",
		},
		{
			name:      "borg's invalid-repository record untouched",
			in:        LogMessage{MsgID: "Repository.InvalidRepository", Message: recordMessage(t, invalidRepositoryRecord)},
			wantMsgID: "Repository.InvalidRepository",
		},
		{
			name:      "a lock timeout is not a missing repository",
			in:        LogMessage{MsgID: "LockTimeout", Message: recordMessage(t, lockTimeoutRecord)},
			wantMsgID: "LockTimeout",
		},
		{
			name:      "empty message",
			in:        LogMessage{},
			wantMsgID: "",
		},
	} {
		t.Run(i.name, func(t *testing.T) {
			got := i.in
			stampMissingRepository(&got)
			if got.MsgID != i.wantMsgID {
				t.Errorf("Received msgid %q, wanted %q", got.MsgID, i.wantMsgID)
			}
			if got.Message != i.in.Message {
				t.Errorf("Received message %q, wanted it left as %q", got.Message, i.in.Message)
			}
		})
	}
}

// A nil failure is not a case FindRepository can produce (it stamps only inside its
// err != nil branch), but the helper must be safe on its own.
func TestStampMissingRepositoryNil(t *testing.T) {
	stampMissingRepository(nil)
}

// TestRepositoryAlreadyExists pins the one `borg init` failure Setup treats as success.
// Both directions cost something real: rejecting Repository.AlreadyExists fails a backup
// that lost a harmless init race, and accepting anything else — Repository.PathAlreadyExists
// above all — returns a repository that is not there.
func TestRepositoryAlreadyExists(t *testing.T) {
	for _, i := range []struct {
		name string
		in   *LogMessage
		want bool
	}{
		{
			// The race this tolerance exists for: another task initialized the
			// repository first, so the one the caller wanted does exist.
			name: "borg's already-exists record",
			in:   fullRecord(t, repoAlreadyExistsRecord),
			want: true,
		},
		{
			// A different measured condition: the path holds content that is not a
			// repository. Tolerating it would report a phantom repository.
			name: "path already holds non-repository content",
			in:   fullRecord(t, pathAlreadyExistsRecord),
			want: false,
		},
		{
			name: "invalid repository",
			in:   fullRecord(t, invalidRepositoryRecord),
			want: false,
		},
		{
			name: "lock timeout",
			in:   fullRecord(t, lockTimeoutRecord),
			want: false,
		},
		{
			// Constructed to isolate the msgid: borg's own already-exists wording with
			// no msgid on the record must NOT be tolerated, because the decision is
			// borg's verdict and never our reading of its prose.
			name: "already-exists wording without a msgid",
			in:   &LogMessage{Message: recordMessage(t, repoAlreadyExistsRecord)},
			want: false,
		},
		{
			// What classify synthesizes when borg printed nothing usable. It carries no
			// msgid precisely so it cannot reach a msgid-driven decision like this one.
			name: "synthesized no-output reason",
			in:   &LogMessage{Message: "borg init exited 2: no diagnostic output"},
			want: false,
		},
		{name: "empty failure", in: &LogMessage{}, want: false},
		{name: "nil failure", in: nil, want: false},
	} {
		t.Run(i.name, func(t *testing.T) {
			if got := repositoryAlreadyExists(i.in); got != i.want {
				t.Errorf("Received %v, wanted %v", got, i.want)
			}
		})
	}
}
