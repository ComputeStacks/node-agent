package borg

import (
	"context"
	"cs-agent/store"
	"cs-agent/types"
	"encoding/json"
	"testing"

	"github.com/spf13/viper"
)

// TestRepoPathForSSH pins the remote URL field by field. It is the string a
// cross-volume restore got wrong: every part of it but the name comes from
// configuration, so the name is the only thing a caller can put in the wrong place.
func TestRepoPathForSSH(t *testing.T) {
	sshBackendConfig()

	want := "ssh://borg@backup.example.com:2222/backups/node001/b-vol-1/backup"
	if got := repoPathFor("vol-1"); got != want {
		t.Errorf("Received %q, wanted %q", got, want)
	}
}

// TestRepoPathForLocalAndNFS: on both of those backends the docker volume mounted at
// /mnt/borg is the repository, so the path is a constant and the name does not appear
// in it at all.
func TestRepoPathForLocalAndNFS(t *testing.T) {
	for _, i := range []struct {
		name  string
		setup func()
	}{
		{name: "local", setup: func() { viper.Reset() }},
		{
			name: "nfs",
			setup: func() {
				viper.Reset()
				viper.Set("backups.borg.nfs", true)
				viper.Set("backups.borg.nfs_host_path", "/mnt/ams001/node001")
			},
		},
	} {
		t.Run(i.name, func(t *testing.T) {
			i.setup()
			if got := repoPathFor("vol-1"); got != "/mnt/borg/backup" {
				t.Errorf("Received %q, wanted %q", got, "/mnt/borg/backup")
			}
		})
	}
}

// TestResolveRepositoryNamesTheOwner is THE regression guard for this whole change: it
// asserts the decision that was wrong, at the site it was wrong.
//
// A repository is named after the volume that owns it, never the volume the operation is
// pointed at. Naming it after the target is what made a restore of volume A into volume B
// open B's repository on the SSH backend and never find A's archive.
//
// It has to assert here rather than on the container spec. The spec takes the two names as
// separate arguments and routes them correctly whichever way round they are handed over, so
// a spec-level test pins the plumbing and says nothing about the choice — putting the
// original defect back at this line left such a test green.
func TestResolveRepositoryNamesTheOwner(t *testing.T) {
	target := &types.Volume{Name: "vol-target"}
	owner := &types.Volume{Name: "vol-owner"}
	owner.Retention.Daily = 7
	target.Retention.Daily = 99

	r, failure := resolveRepository(nil, target, owner)
	if failure != nil {
		t.Fatalf("Received failure %q, wanted a repository", failure.Message)
	}
	if r.Name != "vol-owner" {
		t.Errorf("Received Name %q, wanted the repository owner's %q", r.Name, "vol-owner")
	}
	if r.Name == target.Name {
		t.Errorf("Received Name %q, which is the TARGET volume — the repository belongs to the owner", r.Name)
	}
	// Retention describes the repository being pruned, so it travels with the owner. Taken
	// from the target, a clone would apply the new volume's policy to the source's archives.
	if r.Retention.Daily != 7 {
		t.Errorf("Received keep-daily %d, wanted the repository owner's %d", r.Retention.Daily, 7)
	}
}

// TestResolveRepositoryDefaultsAnAbsentOwner: an absent owner is DEFAULTED to the target,
// never refused. backup/delete.go passes the task's source_volume straight through and the
// controller only defaults it, so an archive delete that arrives without one succeeds today
// and has to keep succeeding.
func TestResolveRepositoryDefaultsAnAbsentOwner(t *testing.T) {
	r, failure := resolveRepository(nil, &types.Volume{Name: "vol-target"}, &types.Volume{})
	if failure != nil {
		t.Fatalf("Received failure %q — an absent repository owner must be defaulted, not refused", failure.Message)
	}
	if r.Name != "vol-target" {
		t.Errorf("Received Name %q, wanted the target %q it should have defaulted to", r.Name, "vol-target")
	}
}

// TestResolveRepositoryRefusesAnAbsentTarget covers the one input that has no recovery: the
// target supplies the /mnt/data mount and the label saying what the container is for, and an
// empty docker volume name is not a thing to mount.
func TestResolveRepositoryRefusesAnAbsentTarget(t *testing.T) {
	r, failure := resolveRepository(nil, &types.Volume{}, &types.Volume{Name: "vol-owner"})
	if failure == nil {
		t.Fatal("Received a nil failure for an absent target, wanted one")
	}
	if failure.Message != "Missing target volume name" {
		t.Errorf("Received %q, wanted %q", failure.Message, "Missing target volume name")
	}
	if r.Name != "" {
		t.Errorf("Received a named repository (%q) alongside a failure, wanted the zero value", r.Name)
	}
}

// TestFindRepositoryRefusesBeforeBuildingAnything pins that the refusal above happens ahead
// of the docker container build — which is both why it can be tested at all and what stops a
// junk task from creating a b- volume on the way to failing.
func TestFindRepositoryRefusesAnEmptyTargetName(t *testing.T) {
	viper.Reset()

	r, failure := FindRepository(nil, &types.Volume{}, &types.Volume{Name: "vol-owner"})

	if failure == nil {
		t.Fatal("Received a nil failure for an empty target name, wanted one")
	}
	if failure.Message != "Missing target volume name" {
		t.Errorf("Received %q, wanted %q", failure.Message, "Missing target volume name")
	}
	if r != nil {
		t.Errorf("Received a repository (%+v) alongside a failure, wanted nil", r)
	}
}

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

// TestShouldSyncRepository covers the decision that keeps a node from publishing a
// repositories row for a volume it does not own.
//
// It is tested here rather than through Sync because Sync goes straight on to read the
// repository through the borg container, so it cannot be reached without a docker daemon —
// which is what left this decision uncovered while it sat inline. Splitting it out is the
// only reason there is a test at all.
func TestShouldSyncRepository(t *testing.T) {
	st, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()

	if err := st.PutVolume(ctx, store.Volume{
		Name:   "owned-vol",
		Node:   "node001",
		Config: json.RawMessage(`{"name":"owned-vol"}`),
	}); err != nil {
		t.Fatalf("PutVolume: %v", err)
	}

	if !shouldSyncRepository(ctx, st, "owned-vol") {
		t.Error("Received false for a volume this node owns, wanted true")
	}

	// The case the guard exists for: Archive.Delete calls Sync, and an archive delete can
	// name a source volume this node has no desired-state row for.
	if shouldSyncRepository(ctx, st, "someone-elses-vol") {
		t.Error("Received true for a volume this node does not own, wanted false")
	}

	// A store that cannot answer is not the same fact as a volume that is not here: a
	// transient read error must leave the sync alone rather than silently drop it.
	_ = st.Close()
	if !shouldSyncRepository(ctx, st, "owned-vol") {
		t.Error("Received false when the store could not answer, wanted true (fail open)")
	}
}
