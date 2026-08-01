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
// controller clones a volume by restoring into a brand-new, empty one — and it is a
// reachable state for a restore of any strategy: preRestore stops the service's containers
// before it takes the snapshot, so what the move sees is whatever a quiesced service left
// in /mnt/data, up to and including nothing. A bare `mv src/* dst/` fails here, because the
// glob does not expand and mv is handed the literal `src/*`.
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
// separate arguments and moved neither. It has to be quoted in every group, so a spaced
// dotfile is here too: unquoted, `mv $@ dst/` in the dotfile group is handed ".dot" and
// "file.txt", neither of which exists, and the move fails.
func TestSnapshotCommandFilenamesWithSpaces(t *testing.T) {
	src, dst := snapshotDirs(t)
	writeFile(t, filepath.Join(src, "my file.txt"), "spaced")
	writeFile(t, filepath.Join(src, "another one.txt"), "also spaced")
	writeFile(t, filepath.Join(src, ".dot file.txt"), "spaced and hidden")

	code, out := runSh(t, snapshotCommand(src, dst))

	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	want := []string{".dot file.txt", "another one.txt", "my file.txt"}
	if got := names(t, dst); !equalNames(got, want) {
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

// Dotfiles MUST be moved. A `dir/*`-only snapshot left them behind in both directions:
// the extract overwrote the volume's `.htaccess` with the archive's, the rollback's rm
// did not clear what the extract wrote, and the put-back had nothing hidden to return —
// so the pre-restore content was gone, in an AutoRemove container, under a rollback that
// reported success. This fleet hosts WordPress; `.htaccess`, `.user.ini` and `.git/` are
// the everyday shape of that loss.
func TestSnapshotCommandMovesDotfiles(t *testing.T) {
	src, dst := snapshotDirs(t)
	writeFile(t, filepath.Join(src, ".htaccess"), "RewriteEngine On")
	writeFile(t, filepath.Join(src, ".user.ini"), "memory_limit = 256M")
	if err := os.Mkdir(filepath.Join(src, ".git"), 0o755); err != nil {
		t.Fatalf("creating .git: %v", err)
	}
	writeFile(t, filepath.Join(src, ".git", "config"), "[core]")
	writeFile(t, filepath.Join(src, "visible.txt"), "not dot")

	code, out := runSh(t, snapshotCommand(src, dst))

	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if got := names(t, src); len(got) != 0 {
		t.Errorf("source should be empty after the move, got %v", got)
	}
	want := []string{".git", ".htaccess", ".user.ini", "visible.txt"}
	if got := names(t, dst); !equalNames(got, want) {
		t.Fatalf("destination = %v, want %v", got, want)
	}
	// A dot-directory has to arrive whole, not just by name: the rollback puts this back
	// as the volume's only copy.
	content, err := os.ReadFile(filepath.Join(dst, ".git", "config"))
	if err != nil || string(content) != "[core]" {
		t.Errorf("nested dotfile did not survive: %q, %v", content, err)
	}
}

// Names beginning with two dots are what the third glob is for. `.[!.]*` cannot match
// them — it requires the character after the dot not to be a dot — so without `..?*` they
// are exactly as invisible to the snapshot as every dotfile used to be.
func TestSnapshotCommandMovesDoubleDotNames(t *testing.T) {
	src, dst := snapshotDirs(t)
	writeFile(t, filepath.Join(src, "..stray"), "two dots")
	writeFile(t, filepath.Join(src, "...triple"), "three dots")
	writeFile(t, filepath.Join(src, "visible.txt"), "not dot")

	code, out := runSh(t, snapshotCommand(src, dst))

	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if got := names(t, src); len(got) != 0 {
		t.Errorf("source should be empty after the move, got %v", got)
	}
	want := []string{"...triple", "..stray", "visible.txt"}
	if got := names(t, dst); !equalNames(got, want) {
		t.Fatalf("destination = %v, want %v", got, want)
	}
}

// A source holding nothing but dotfiles must succeed. Two of the three globs match
// nothing here, and dash leaves an unmatched glob as a literal, so this is the case that
// fails outright under any rewrite where a non-matching pattern is handed straight to mv
// or where one group's non-match decides the command's exit status.
func TestSnapshotCommandDotfileOnlySource(t *testing.T) {
	src, dst := snapshotDirs(t)
	writeFile(t, filepath.Join(src, ".env"), "DB_PASSWORD=hunter2")

	code, out := runSh(t, snapshotCommand(src, dst))

	if code != 0 {
		t.Fatalf("a dotfile-only source must succeed, got exit %d: %s", code, out)
	}
	if got := names(t, src); len(got) != 0 {
		t.Errorf("source should be empty after the move, got %v", got)
	}
	if got, want := names(t, dst), []string{".env"}; !equalNames(got, want) {
		t.Fatalf("destination = %v, want %v", got, want)
	}
}

// The `|| [ -L "$1" ]` guard again, in the DOTTED group. The visible-group case above
// cannot see this one: each group binds its own "$1", so dropping the guard from the
// dotfile group alone leaves that test green while `.htaccess` stops being snapshotted.
// `.current -> releases/gone` is the same ordinary application-volume shape as its
// visible sibling, wearing a dot.
func TestSnapshotCommandDanglingDotSymlinkFirst(t *testing.T) {
	src, dst := snapshotDirs(t)
	// ".0-current" sorts ahead of ".htaccess", so it is what "$1" binds to.
	if err := os.Symlink("releases/gone", filepath.Join(src, ".0-current")); err != nil {
		t.Fatalf("creating dangling symlink: %v", err)
	}
	writeFile(t, filepath.Join(src, ".htaccess"), "precious")

	code, out := runSh(t, snapshotCommand(src, dst))

	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if got := names(t, src); len(got) != 0 {
		t.Errorf("source should be empty after the move, got %v", got)
	}
	if got, want := names(t, dst), []string{".0-current", ".htaccess"}; !equalNames(got, want) {
		t.Fatalf("destination = %v, want %v — the dangling dot-symlink made a non-empty directory read as empty", got, want)
	}
	content, err := os.ReadFile(filepath.Join(dst, ".htaccess"))
	if err != nil || string(content) != "precious" {
		t.Errorf("dotfile did not reach the snapshot: %q, %v", content, err)
	}
}

// `.` and `..` must never be operands. `src/.*` would match both, which hands the move
// the volume's own directory and its PARENT — the docker volume's parent, in production.
// GNU mv refuses, so what this asserts is the refusal's own consequence as well: a
// snapshot that reports failure and halts the restore.
func TestSnapshotCommandNeverMovesDotOrDotDot(t *testing.T) {
	src, dst := snapshotDirs(t)
	parent := filepath.Dir(src)
	writeFile(t, filepath.Join(parent, "sibling.txt"), "outside the volume")
	writeFile(t, filepath.Join(src, "visible.txt"), "inside")
	writeFile(t, filepath.Join(src, ".htaccess"), "inside, hidden")

	code, out := runSh(t, snapshotCommand(src, dst))

	if code != 0 {
		t.Fatalf("exit %d: %s — a glob matching `.` or `..` is the only way this fails", code, out)
	}
	if got, want := names(t, parent), []string{"data", "sibling.txt", "snapshot"}; !equalNames(got, want) {
		t.Errorf("parent of the source = %v, want %v", got, want)
	}
	info, err := os.Stat(src)
	if err != nil || !info.IsDir() {
		t.Fatalf("the source directory itself should still be there: %v", err)
	}
	if got := names(t, src); len(got) != 0 {
		t.Errorf("source should be empty after the move, got %v", got)
	}
}

// A failed move of a DOTFILE has to be reported, for the same reason a failed move of a
// visible file does: a restore whose rollback is not really available must not begin.
//
// `mv file dst/` where dst/file is a directory fails for every uid — "cannot overwrite
// directory … with non-directory" — which is what makes this provable in CI, where the
// suite runs as uid 0. Permission tricks do not work there; see the uid check that makes
// TestSnapshotCommandUncreatableDestinationFails skip.
func TestSnapshotCommandDotfileMoveFailureFails(t *testing.T) {
	src, dst := snapshotDirs(t)
	writeFile(t, filepath.Join(src, ".htaccess"), "pre-restore")
	if err := os.MkdirAll(filepath.Join(dst, ".htaccess"), 0o755); err != nil {
		t.Fatalf("creating the blocking directory: %v", err)
	}

	code, out := runSh(t, snapshotCommand(src, dst))

	if code == 0 {
		t.Fatalf("a failed dotfile move must be reported, got exit 0: %s", out)
	}
	if got, want := names(t, src), []string{".htaccess"}; !equalNames(got, want) {
		t.Errorf("source = %v, want %v — the dotfile should not have moved", got, want)
	}
}

// The groups are joined with `&&`, not `;`. With `;` the exit status is the last group's,
// and the `..?*` group matches nothing on virtually every volume — so a failed move of
// the visible entries, which is nearly all the data, would report a snapshot that worked
// and the extract would run believing it could be undone.
//
// Both halves of that matter, so both are asserted: the failure is reported, and the
// later groups did not run.
func TestSnapshotCommandVisibleMoveFailureStopsTheChain(t *testing.T) {
	src, dst := snapshotDirs(t)
	writeFile(t, filepath.Join(src, "visible.txt"), "pre-restore")
	writeFile(t, filepath.Join(src, ".htaccess"), "pre-restore, hidden")
	if err := os.MkdirAll(filepath.Join(dst, "visible.txt"), 0o755); err != nil {
		t.Fatalf("creating the blocking directory: %v", err)
	}

	code, out := runSh(t, snapshotCommand(src, dst))

	if code == 0 {
		t.Fatalf("a failed visible move must be reported, got exit 0: %s", out)
	}
	if got, want := names(t, src), []string{".htaccess", "visible.txt"}; !equalNames(got, want) {
		t.Errorf("source = %v, want %v — the dotfile group ran after the visible group failed", got, want)
	}
	if got, want := names(t, dst), []string{"visible.txt"}; !equalNames(got, want) {
		t.Errorf("destination = %v, want %v — only the blocking directory should be there", got, want)
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

// The same real failure, provable as root — which is how this suite is actually run: the
// project's test command is `go test ./...` inside a golang container, as uid 0. The test
// above skips there, because root ignores directory permissions, and that left every case
// that does execute asserting a success scenario: appending `|| true` to the snapshot
// command passed the entire file.
//
// A path component that is a regular file is a failure no uid can walk past. `mkdir -p`
// gets ENOTDIR, the `&&` short-circuits, and nothing moves.
func TestSnapshotCommandDestinationUnderFileFails(t *testing.T) {
	src, _ := snapshotDirs(t)
	writeFile(t, filepath.Join(src, "one.txt"), "one")

	blocker := filepath.Join(t.TempDir(), "blocker")
	writeFile(t, blocker, "a regular file, not a directory")

	code, out := runSh(t, snapshotCommand(src, filepath.Join(blocker, "snapshot")))

	if code == 0 {
		t.Fatalf("a destination beneath a regular file must fail, got exit 0: %s", out)
	}
	if got, want := names(t, src), []string{"one.txt"}; !equalNames(got, want) {
		t.Errorf("source = %v, want %v — nothing should have moved", got, want)
	}
}

// The rollback direction, through rollbackCommand — the same function rollbackRestoreSnapshot
// runs, not a copy of the string it produces. Composing it here as well is what let the
// production command drift: a lost `&&` or a changed path would have kept these tests
// green. A marker file placed in the volume before the restore must come back.
func TestSnapshotCommandRollbackRoundTrip(t *testing.T) {
	data, snapshot := snapshotDirs(t)
	writeFile(t, filepath.Join(data, "marker.txt"), "customer data")

	if code, out := runSh(t, snapshotCommand(data, snapshot)); code != 0 {
		t.Fatalf("snapshot: exit %d: %s", code, out)
	}
	// Stand in for what a failed extract leaves behind.
	writeFile(t, filepath.Join(data, "half-restored.txt"), "partial")

	if code, out := runSh(t, rollbackCommand(snapshot, data)); code != 0 {
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

	if code, out := runSh(t, rollbackCommand(snapshot, data)); code != 0 {
		t.Fatalf("rollback: exit %d: %s", code, out)
	}

	if got := names(t, data); len(got) != 0 {
		t.Errorf("volume should be empty again, got %v", got)
	}
}

// The headline regression. A dot-blind snapshot lost the pre-restore content of every
// dotfile a partial extract touched, permanently: the snapshot never held the volume's
// `.htaccess`, the extract overwrote it with the archive's copy, the rollback's
// `rm -rf dst/*` did not clear what the extract had written, and the put-back had nothing
// hidden to return. The backup container is AutoRemove, so there was no second copy — and
// the rollback reported success.
//
// So this asserts the full round trip through both directions on a dotfile: the volume's
// own content comes back, and a dotfile the failed extract introduced is removed rather
// than left mixed in among the snapshot's.
func TestRollbackCommandRestoresDotfiles(t *testing.T) {
	data, snapshot := snapshotDirs(t)
	writeFile(t, filepath.Join(data, ".htaccess"), "pre-restore")
	writeFile(t, filepath.Join(data, "marker.txt"), "customer data")

	if code, out := runSh(t, snapshotCommand(data, snapshot)); code != 0 {
		t.Fatalf("snapshot: exit %d: %s", code, out)
	}

	// Stand in for a partial extract: the archive's own .htaccess over the volume's, a
	// dotfile the volume never had, and a half-written visible file.
	writeFile(t, filepath.Join(data, ".htaccess"), "from the archive")
	writeFile(t, filepath.Join(data, ".env"), "DB_PASSWORD=from-the-archive")
	writeFile(t, filepath.Join(data, "half-restored.txt"), "partial")

	if code, out := runSh(t, rollbackCommand(snapshot, data)); code != 0 {
		t.Fatalf("rollback: exit %d: %s", code, out)
	}

	if got, want := names(t, data), []string{".htaccess", "marker.txt"}; !equalNames(got, want) {
		t.Fatalf("volume = %v, want %v — the extract's .env must be gone, not left behind", got, want)
	}
	content, err := os.ReadFile(filepath.Join(data, ".htaccess"))
	if err != nil || string(content) != "pre-restore" {
		t.Errorf(".htaccess did not round-trip: %q, %v", content, err)
	}
	if got := names(t, snapshot); len(got) != 0 {
		t.Errorf("snapshot should be drained, got %v", got)
	}
}

// The rollback's rm is the one command here that deletes, and `dst/.*` would hand it
// `dst/..` — the parent of the docker volume. GNU coreutils refuse, but the borg image tag
// floats and an rm that does not refuse takes the parent directory with it, so the pattern
// is asserted against rather than trusted to the tool.
func TestRollbackCommandNeverRemovesTheParent(t *testing.T) {
	data, snapshot := snapshotDirs(t)
	parent := filepath.Dir(data)
	writeFile(t, filepath.Join(parent, "sibling.txt"), "outside the volume")
	writeFile(t, filepath.Join(data, ".htaccess"), "pre-restore")

	if code, out := runSh(t, snapshotCommand(data, snapshot)); code != 0 {
		t.Fatalf("snapshot: exit %d: %s", code, out)
	}
	writeFile(t, filepath.Join(data, "half-restored.txt"), "partial")

	code, out := runSh(t, rollbackCommand(snapshot, data))

	if code != 0 {
		t.Fatalf("rollback: exit %d: %s — a glob matching `.` or `..` is the only way this fails", code, out)
	}
	if got, want := names(t, parent), []string{"data", "sibling.txt", "snapshot"}; !equalNames(got, want) {
		t.Errorf("parent of the volume = %v, want %v", got, want)
	}
	content, err := os.ReadFile(filepath.Join(parent, "sibling.txt"))
	if err != nil || string(content) != "outside the volume" {
		t.Errorf("the sibling outside the volume did not survive: %q, %v", content, err)
	}
}
