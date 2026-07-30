package backup

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
)

// The snapshot move is the one piece of the restore path where a wrong exit code costs
// customer data: a snapshot that silently did not happen leaves the extract running
// over live data with nothing to roll back to, and a snapshot that reports failure
// where nothing is wrong halts a restore that should have proceeded. So these run the
// command through a real sh against a real directory tree, rather than asserting on the
// string it composes — the string is only correct if these particular shell semantics
// hold.

// runSh runs one shell command and returns its exit code plus merged output.
func runSh(t *testing.T, cmd string) (int, string) {
	t.Helper()
	sh, lookErr := exec.LookPath("sh")
	if lookErr != nil {
		t.Skip("no sh available")
	}
	out, err := exec.Command(sh, "-c", cmd).CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), string(out)
	}
	t.Fatalf("running %q: %v", cmd, err)
	return -1, string(out)
}

// names lists a directory's entries, dotfiles included, sorted.
func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func equalNames(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// snapshotDirs returns an existing src and a dst path that does not exist yet, both
// free of shell metacharacters (the production paths are /mnt/data and /root/.snapshot,
// and snapshotCommand does not quote them).
func snapshotDirs(t *testing.T) (src, dst string) {
	t.Helper()
	base := t.TempDir()
	src = filepath.Join(base, "data")
	dst = filepath.Join(base, "snapshot")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatalf("creating src: %v", err)
	}
	return src, dst
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// An empty source must SUCCEED and move nothing. This is the clone case — the
// controller clones a volume by restoring into a brand-new, empty one — and it is also
// every mysql restore, whose database containers stop before the snapshot runs. A bare
// `mv src/* dst/` fails here, because the glob does not expand and mv is handed the
// literal `src/*`.
func TestSnapshotCommandEmptySource(t *testing.T) {
	src, dst := snapshotDirs(t)

	code, out := runSh(t, snapshotCommand(src, dst))

	if code != 0 {
		t.Fatalf("empty source must succeed, got exit %d: %s", code, out)
	}
	if got := names(t, src); len(got) != 0 {
		t.Errorf("source should still be empty, got %v", got)
	}
	// dst must exist afterwards even though nothing moved: the rollback direction runs
	// `rm -rf dst/*` against it.
	if info, err := os.Stat(dst); err != nil || !info.IsDir() {
		t.Errorf("destination was not created: %v", err)
	}
}

// A source that does not exist at all is the same non-failure. It is reachable: the
// rollback direction globs the snapshot directory, and a restore can fail before
// anything has created it.
func TestSnapshotCommandMissingSource(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "absent")
	dst := filepath.Join(base, "snapshot")

	code, out := runSh(t, snapshotCommand(src, dst))

	if code != 0 {
		t.Fatalf("missing source must succeed, got exit %d: %s", code, out)
	}
}

func TestSnapshotCommandMovesFiles(t *testing.T) {
	src, dst := snapshotDirs(t)
	writeFile(t, filepath.Join(src, "one.txt"), "one")
	if err := os.Mkdir(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatalf("creating subdir: %v", err)
	}
	writeFile(t, filepath.Join(src, "sub", "two.txt"), "two")

	code, out := runSh(t, snapshotCommand(src, dst))

	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if got := names(t, src); len(got) != 0 {
		t.Errorf("source should be empty after the move, got %v", got)
	}
	if got, want := names(t, dst), []string{"one.txt", "sub"}; !equalNames(got, want) {
		t.Errorf("destination = %v, want %v", got, want)
	}
	content, err := os.ReadFile(filepath.Join(dst, "sub", "two.txt"))
	if err != nil || string(content) != "two" {
		t.Errorf("nested file did not survive: %q, %v", content, err)
	}
}

// Quoting "$@" is what makes this work; the unquoted glob it replaces split these into
// separate arguments and moved neither.
func TestSnapshotCommandFilenamesWithSpaces(t *testing.T) {
	src, dst := snapshotDirs(t)
	writeFile(t, filepath.Join(src, "my file.txt"), "spaced")
	writeFile(t, filepath.Join(src, "another one.txt"), "also spaced")

	code, out := runSh(t, snapshotCommand(src, dst))

	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if got, want := names(t, dst), []string{"another one.txt", "my file.txt"}; !equalNames(got, want) {
		t.Errorf("destination = %v, want %v", got, want)
	}
}

// The `|| [ -L "$1" ]` guard. `[ -e ]` follows symlinks, so a dangling symlink that
// sorts first reads as "nothing here" — and a directory full of data would then be
// skipped, exit 0, and be overwritten by the extract with no rollback available.
// `current -> releases/gone` is an ordinary shape for an application volume.
func TestSnapshotCommandDanglingSymlinkFirst(t *testing.T) {
	src, dst := snapshotDirs(t)
	// "0-current" sorts ahead of "data.txt", so it is what "$1" binds to.
	if err := os.Symlink("releases/gone", filepath.Join(src, "0-current")); err != nil {
		t.Fatalf("creating dangling symlink: %v", err)
	}
	writeFile(t, filepath.Join(src, "data.txt"), "precious")

	code, out := runSh(t, snapshotCommand(src, dst))

	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if got := names(t, src); len(got) != 0 {
		t.Errorf("source should be empty after the move, got %v", got)
	}
	if got, want := names(t, dst), []string{"0-current", "data.txt"}; !equalNames(got, want) {
		t.Fatalf("destination = %v, want %v — the dangling symlink made a non-empty directory read as empty", got, want)
	}
	content, err := os.ReadFile(filepath.Join(dst, "data.txt"))
	if err != nil || string(content) != "precious" {
		t.Errorf("data file did not reach the snapshot: %q, %v", content, err)
	}
}

// Dotfiles are NOT moved, exactly as `mv src/*` did not move them. This documents
// today's semantics as intentional rather than accidental: they are not snapshotted and
// so not put back by a rollback, and changing that is a separate behavioural change
// with its own reasoning to do. If this test starts failing, the change was deliberate.
func TestSnapshotCommandSkipsDotfiles(t *testing.T) {
	src, dst := snapshotDirs(t)
	writeFile(t, filepath.Join(src, ".hidden"), "dot")
	writeFile(t, filepath.Join(src, "visible.txt"), "not dot")

	code, out := runSh(t, snapshotCommand(src, dst))

	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if got, want := names(t, src), []string{".hidden"}; !equalNames(got, want) {
		t.Errorf("source = %v, want %v", got, want)
	}
	if got, want := names(t, dst), []string{"visible.txt"}; !equalNames(got, want) {
		t.Errorf("destination = %v, want %v", got, want)
	}
}

// A real failure must still be reported, or the guard above would have traded one silent
// data loss for another: an undoable restore that believed it had a snapshot.
func TestSnapshotCommandUncreatableDestinationFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	src, _ := snapshotDirs(t)
	writeFile(t, filepath.Join(src, "one.txt"), "one")

	parent := filepath.Join(t.TempDir(), "readonly")
	if err := os.Mkdir(parent, 0o500); err != nil {
		t.Fatalf("creating read-only parent: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

	code, _ := runSh(t, snapshotCommand(src, filepath.Join(parent, "snapshot")))

	if code == 0 {
		t.Fatal("an uncreatable destination must fail, got exit 0")
	}
	if got, want := names(t, src), []string{"one.txt"}; !equalNames(got, want) {
		t.Errorf("source = %v, want %v — nothing should have moved", got, want)
	}
}

// The rollback direction, as rollbackRestoreSnapshot composes it: empty the volume, then
// run the same helper with src and dst swapped. A marker file placed in the volume
// before the restore must come back.
func TestSnapshotCommandRollbackRoundTrip(t *testing.T) {
	data, snapshot := snapshotDirs(t)
	writeFile(t, filepath.Join(data, "marker.txt"), "customer data")

	if code, out := runSh(t, snapshotCommand(data, snapshot)); code != 0 {
		t.Fatalf("snapshot: exit %d: %s", code, out)
	}
	// Stand in for what a failed extract leaves behind.
	writeFile(t, filepath.Join(data, "half-restored.txt"), "partial")

	rollback := "rm -rf " + data + "/* && " + snapshotCommand(snapshot, data)
	if code, out := runSh(t, rollback); code != 0 {
		t.Fatalf("rollback: exit %d: %s", code, out)
	}

	if got, want := names(t, data), []string{"marker.txt"}; !equalNames(got, want) {
		t.Fatalf("volume = %v, want %v", got, want)
	}
	content, err := os.ReadFile(filepath.Join(data, "marker.txt"))
	if err != nil || string(content) != "customer data" {
		t.Errorf("marker did not round-trip: %q, %v", content, err)
	}
	if got := names(t, snapshot); len(got) != 0 {
		t.Errorf("snapshot should be drained, got %v", got)
	}
}

// The rollback of a restore into an empty volume: the snapshot is empty, so `rm -rf
// dst/*` runs with nothing to put back — and must still succeed, removing only what the
// failed restore itself wrote. The snapshot can only be empty when the volume was empty
// when it was taken, which is why that rm needs no guard of its own.
func TestSnapshotCommandRollbackFromEmptySnapshot(t *testing.T) {
	data, snapshot := snapshotDirs(t)

	if code, out := runSh(t, snapshotCommand(data, snapshot)); code != 0 {
		t.Fatalf("snapshot: exit %d: %s", code, out)
	}
	writeFile(t, filepath.Join(data, "half-restored.txt"), "partial")

	rollback := "rm -rf " + data + "/* && " + snapshotCommand(snapshot, data)
	if code, out := runSh(t, rollback); code != 0 {
		t.Fatalf("rollback: exit %d: %s", code, out)
	}

	if got := names(t, data); len(got) != 0 {
		t.Errorf("volume should be empty again, got %v", got)
	}
}
