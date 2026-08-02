package backup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The mysql promote empties /mnt/data and then refills it from the extracted archive's
// dump directory. Every way that can go wrong ends with an empty datadir reported as a
// successful restore, while the only copy of the customer's data sits in /root/.snapshot
// inside an AutoRemove container that is about to be reaped. So, like the snapshot
// harness these share their helpers with (runSh, names, equalNames, writeFile in
// snapshot_command_test.go), these run the real production builder through a real
// /bin/sh against real directories rather than asserting on the string it composes.

// promoteDirs returns an existing data directory and a staging path that does not exist
// yet, laid out at the same depths as production's /mnt/data and /root/.staging.
//
// The depths are load-bearing for every symlink case: `backups -> ..` resolves to the
// parent of the DATA directory before the park and to the parent of the STAGING directory
// after it, which in production is /root — where the snapshot lives. A flat fixture would
// make the two the same and prove nothing. Both paths are free of shell metacharacters,
// which promoteDumpCommand requires.
func promoteDirs(t *testing.T) (data, staging string) {
	t.Helper()
	base := t.TempDir()
	data = filepath.Join(base, "mnt", "data")
	staging = filepath.Join(base, "root", ".staging")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatalf("creating data: %v", err)
	}
	return data, staging
}

func mkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("creating %s: %v", path, err)
	}
}

// mkDump creates a directory that a promote is allowed to promote: a real directory
// carrying a marker, which is how promoteDumpCommand tells a prepared
// xtrabackup/mariabackup dump apart from any other directory called `backups`. Every
// fixture that must SUCCEED has to carry one, and the fixtures that must fail are the ones
// that carry none.
//
// It writes xtrabackupMarker because that is the name shared by Percona xtrabackup and
// MariaDB ≤ 11.0, so a fixture using it exercises the older half of the fleet. The newer
// name is not an also-ran — mariadb:12 writes only `mariadb_backup_checkpoints` — and
// TestPromoteDumpCommandAcceptsEveryMarkerName is what holds the promote to accepting
// each name in dumpMarkers on its own.
func mkDump(t *testing.T, dir string) {
	t.Helper()
	mkDumpMarked(t, dir, xtrabackupMarker)
}

func mkDumpMarked(t *testing.T, dir, marker string) {
	t.Helper()
	mkdirAll(t, dir)
	writeFile(t, filepath.Join(dir, marker), "backup_type = full-prepared\nfrom_lsn = 0\n")
}

// containerRoot fills in the directory that staging sits inside — /root in production —
// with what makes a `backups -> ..` symlink a data-loss bug rather than a curiosity: the
// pre-restore snapshot, which is the ONLY copy of the customer's data while a restore is
// in flight, and the backup container's ssh key. A promote must never move either of them
// into the customer's volume.
func containerRoot(t *testing.T, staging string) string {
	t.Helper()
	root := filepath.Dir(staging)
	mkdirAll(t, filepath.Join(root, ".snapshot"))
	writeFile(t, filepath.Join(root, ".snapshot", "precious.ibd"), "the pre-restore snapshot")
	mkdirAll(t, filepath.Join(root, ".ssh"))
	writeFile(t, filepath.Join(root, ".ssh", "id_ed25519"), "the backup container's key")
	return root
}

// The happy path. A mysql archive carries the datadir as it looked while mysqld was
// writing to it AND the prepared dump inside it; what must survive is the dump, and what
// must NOT survive is the hot copy — starting a server against that is the failure mode
// the whole strategy exists to avoid.
func TestPromoteDumpCommandPromotesTheDump(t *testing.T) {
	data, staging := promoteDirs(t)
	writeFile(t, filepath.Join(data, "ibdata1"), "hot copy, torn")
	mkDump(t, filepath.Join(data, dumpDir))
	writeFile(t, filepath.Join(data, dumpDir, "xtrabackup_info"), "prepared")
	writeFile(t, filepath.Join(data, dumpDir, "data.ibd"), "consistent")

	code, out := runSh(t, promoteDumpCommand(data, staging))

	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if got, want := names(t, data), []string{"data.ibd", xtrabackupMarker, "xtrabackup_info"}; !equalNames(got, want) {
		t.Fatalf("volume = %v, want %v — the volume must end up as exactly the dump's contents", got, want)
	}
	content, err := os.ReadFile(filepath.Join(data, "data.ibd"))
	if err != nil || string(content) != "consistent" {
		t.Errorf("dump file did not survive the promote: %q, %v", content, err)
	}
	// The archived datadir is discarded on purpose: it is the copy of a running
	// database, which is why the dump exists. It stays in staging, which is inside the
	// AutoRemove backup container.
	if _, err := os.Stat(filepath.Join(staging, "ibdata1")); err != nil {
		t.Errorf("the archived datadir should have been left in staging: %v", err)
	}
}

// REGRESSION. Every name in dumpMarkers identifies a dump ON ITS OWN, because in practice
// a dump carries exactly one of them: MariaDB 11.1 renamed mariadb-backup's metadata files
// from `xtrabackup_*` to `mariadb_backup_*`, and a backup taken by one binary never carries
// the other's name.
//
// Testing for a single name is not a cosmetic bug, which is why this runs the WHOLE promote
// per name rather than asserting over the command string. A mariadb:12 volume clone failed
// the marker check on a perfectly good prepared dump; postRestoreMysql returned false,
// restore.go rolled the restore back, and the destination volume was left empty — reported,
// at the time, as a completed clone.
//
// Table-driven over dumpMarkers rather than two hand-written cases, so a name added to that
// set without a working `-f` test for it fails here instead of in production.
func TestPromoteDumpCommandAcceptsEveryMarkerName(t *testing.T) {
	if len(dumpMarkers) == 0 {
		t.Fatal("dumpMarkers is empty; the promote would accept any non-empty directory called backups")
	}
	for _, marker := range dumpMarkers {
		t.Run(marker, func(t *testing.T) {
			data, staging := promoteDirs(t)
			writeFile(t, filepath.Join(data, "ibdata1"), "hot copy, torn")
			mkDumpMarked(t, filepath.Join(data, dumpDir), marker)
			writeFile(t, filepath.Join(data, dumpDir, "data.ibd"), "consistent")

			code, out := runSh(t, promoteDumpCommand(data, staging))

			if code != 0 {
				t.Fatalf("a dump identified by %s must promote, got exit %d: %s", marker, code, out)
			}
			if got, want := names(t, data), []string{"data.ibd", marker}; !equalNames(got, want) {
				t.Fatalf("volume = %v, want %v", got, want)
			}
			if _, err := os.Stat(filepath.Join(staging, "ibdata1")); err != nil {
				t.Errorf("the archived datadir should have been left in staging: %v", err)
			}
		})
	}
}

// The other half of the same invariant: accepting both names must not have widened what
// counts as a dump. A directory carrying NEITHER — an application volume's own backups/
// folder, which is the shape that makes the marker check worth having — must still be
// refused, with the volume left exactly as the extract wrote it.
func TestPromoteDumpCommandUnknownMarkerNameFails(t *testing.T) {
	data, staging := promoteDirs(t)
	writeFile(t, filepath.Join(data, "index.php"), "an application volume")
	mkDumpMarked(t, filepath.Join(data, dumpDir), "percona_backup_checkpoints")

	code, out := runSh(t, promoteDumpCommand(data, staging))

	if code == 0 {
		t.Fatalf("a directory carrying no known marker must fail, got exit 0: %s", out)
	}
	if got, want := names(t, data), []string{dumpDir, "index.php"}; !equalNames(got, want) {
		t.Fatalf("volume = %v, want %v — nothing may move before the dump is identified", got, want)
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Errorf("staging should never have been created: %v", err)
	}
}

// Dotfiles in BOTH directions. `.rocksdb` is a real dump directory — MyRocks writes it —
// so it has to be promoted; and a dotfile belonging to the archived datadir must be
// cleared out rather than left mixed in among the dump's own, which is exactly what the
// dot-blind `rm -rf /mnt/data/*` this replaced did.
func TestPromoteDumpCommandPromotesDumpDotfiles(t *testing.T) {
	data, staging := promoteDirs(t)
	writeFile(t, filepath.Join(data, "ibdata1"), "hot copy, torn")
	writeFile(t, filepath.Join(data, ".stale"), "from the archived datadir")
	mkDump(t, filepath.Join(data, dumpDir))
	mkdirAll(t, filepath.Join(data, dumpDir, ".rocksdb"))
	writeFile(t, filepath.Join(data, dumpDir, ".rocksdb", "CURRENT"), "MANIFEST-000001")
	writeFile(t, filepath.Join(data, dumpDir, "data.ibd"), "consistent")

	code, out := runSh(t, promoteDumpCommand(data, staging))

	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if got, want := names(t, data), []string{".rocksdb", "data.ibd", xtrabackupMarker}; !equalNames(got, want) {
		t.Fatalf("volume = %v, want %v — the dump's dotfiles must arrive and the datadir's must not", got, want)
	}
	content, err := os.ReadFile(filepath.Join(data, ".rocksdb", "CURRENT"))
	if err != nil || string(content) != "MANIFEST-000001" {
		t.Errorf("the dump's dot-directory did not arrive whole: %q, %v", content, err)
	}
}

// A dump whose PAYLOAD is entirely hidden is a real dump — MyRocks writes `.rocksdb` —
// so it must promote, and it must arrive whole rather than as a bare marker file.
//
// This case used to be the one that proved the precondition asks `ls -A` and not bare
// `ls`. The marker check has taken that argument over: xtrabackupMarker is a visible entry, so
// no dump that passes step 2 can look empty to bare `ls` either. What is left here is the
// promote's own dot-globs, which is why the assertion is on the promoted contents.
func TestPromoteDumpCommandDotfileOnlyDump(t *testing.T) {
	data, staging := promoteDirs(t)
	mkDump(t, filepath.Join(data, dumpDir))
	mkdirAll(t, filepath.Join(data, dumpDir, ".rocksdb"))
	writeFile(t, filepath.Join(data, dumpDir, ".rocksdb", "CURRENT"), "MANIFEST-000001")

	code, out := runSh(t, promoteDumpCommand(data, staging))

	if code != 0 {
		t.Fatalf("a dotfile-only dump must succeed, got exit %d: %s", code, out)
	}
	if got, want := names(t, data), []string{".rocksdb", xtrabackupMarker}; !equalNames(got, want) {
		t.Fatalf("volume = %v, want %v", got, want)
	}
}

// No dump directory at all must FAIL, and must fail before anything moves.
//
// Both halves are the point. moveEntriesCommand is deliberately tolerant of an empty
// source — restoring into a brand-new empty volume is how the controller clones one — so
// without the precondition this exits 0, postRestoreMysql returns true, and the task
// reports a successful restore over an empty datadir while the only copy of the data is
// in /root/.snapshot inside a container about to be reaped. And the command this
// replaced only discovered it at the last step, after it had parked the extract and
// emptied the volume; failing up front leaves the extract where it is.
func TestPromoteDumpCommandMissingDumpFails(t *testing.T) {
	data, staging := promoteDirs(t)
	writeFile(t, filepath.Join(data, "ibdata1"), "an ordinary volume, no dump in it")
	writeFile(t, filepath.Join(data, ".htaccess"), "not a database at all")

	code, out := runSh(t, promoteDumpCommand(data, staging))

	if code == 0 {
		t.Fatalf("a missing dump directory must fail, got exit 0: %s", out)
	}
	if got, want := names(t, data), []string{".htaccess", "ibdata1"}; !equalNames(got, want) {
		t.Fatalf("volume = %v, want %v — nothing may move before the check", got, want)
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Errorf("staging should never have been created: %v", err)
	}
}

// A dump directory that exists but is empty is the same failure. `[ -d ]` alone would
// pass it straight through to a promote that moves nothing.
func TestPromoteDumpCommandEmptyDumpFails(t *testing.T) {
	data, staging := promoteDirs(t)
	writeFile(t, filepath.Join(data, "ibdata1"), "hot copy, torn")
	mkdirAll(t, filepath.Join(data, dumpDir))

	code, out := runSh(t, promoteDumpCommand(data, staging))

	if code == 0 {
		t.Fatalf("an empty dump directory must fail, got exit 0: %s", out)
	}
	if got, want := names(t, data), []string{dumpDir, "ibdata1"}; !equalNames(got, want) {
		t.Fatalf("volume = %v, want %v — nothing may move before the check", got, want)
	}
}

// `[ -d ]` is what makes this fail. Measured against GNU coreutils 9.7: `ls -A` handed a
// REGULAR FILE prints that file's name, so `[ -n "$(ls -A dump)" ]` on its own reads a
// file named `backups` as a populated dump directory and the promote runs to an empty
// datadir.
//
// With `-d` dropped the promote still refuses this shape, because the marker check has
// nowhere to look for a file under a file — so what holds `-d` in place is the DIAGNOSTIC
// asserted below, not the exit code. The two failures are different problems: "no mysql
// dump directory" is true of a regular file named `backups`, while "holds no
// xtrabackup_checkpoints" would tell an operator to go looking inside a directory that
// does not exist. Before the marker check existed, dropping `-d` here cost more than a
// wrong message: the volume was emptied first and the failure landed on rollbackRestore.
//
// It is reachable, not theoretical: restore.go overrides filePaths from a
// `switch vol.Strategy` on the SOURCE volume while every hook is handed &destVol, so an
// ordinary application volume cloned into a mysql-strategy destination runs this over
// content that never had a dump directory — where a stray file named `backups` is
// entirely ordinary.
func TestPromoteDumpCommandDumpAsRegularFileFails(t *testing.T) {
	data, staging := promoteDirs(t)
	writeFile(t, filepath.Join(data, "index.php"), "an application volume")
	writeFile(t, filepath.Join(data, dumpDir), "a file the customer called backups")

	code, out := runSh(t, promoteDumpCommand(data, staging))

	if code == 0 {
		t.Fatalf("a regular file named %s must fail, got exit 0: %s", dumpDir, out)
	}
	if got, want := names(t, data), []string{dumpDir, "index.php"}; !equalNames(got, want) {
		t.Fatalf("volume = %v, want %v — nothing may move before the check", got, want)
	}
	if want := "no mysql dump directory"; !strings.Contains(out, want) {
		t.Errorf("output = %q, want it to contain %q — `[ -d ]` is what makes this shape "+
			"a missing dump rather than a directory with no marker in it", out, want)
	}
}

// `[ -d ]` again, on the other shape it is the only guard against. `ls -A` handed a
// DANGLING SYMLINK prints the link's own path on stdout and exits 0 without writing to
// stderr, so `[ -n "$(ls -A dump)" ]` alone passes here too — and `backups ->
// /mnt/backups` pointing at a mount the restore container does not have is an ordinary
// enough shape for a volume that was never a database.
//
// `[ ! -L ]` refuses it first, so the diagnostic is asserted here for the same reason it
// is on the regular-file case: it is what still distinguishes the two guards from the
// marker check, which would also refuse this shape but call it something else.
func TestPromoteDumpCommandDumpAsDanglingSymlinkFails(t *testing.T) {
	data, staging := promoteDirs(t)
	writeFile(t, filepath.Join(data, "index.php"), "an application volume")
	if err := os.Symlink("/mnt/nowhere", filepath.Join(data, dumpDir)); err != nil {
		t.Fatalf("creating dangling symlink: %v", err)
	}

	code, out := runSh(t, promoteDumpCommand(data, staging))

	if code == 0 {
		t.Fatalf("a dangling symlink named %s must fail, got exit 0: %s", dumpDir, out)
	}
	if got, want := names(t, data), []string{dumpDir, "index.php"}; !equalNames(got, want) {
		t.Fatalf("volume = %v, want %v — nothing may move before the check", got, want)
	}
	if want := "no mysql dump directory"; !strings.Contains(out, want) {
		t.Errorf("output = %q, want it to contain %q", out, want)
	}
}

// `backups -> ..` is the shape that turns the promote into a tool for emptying the backup
// container's /root into the customer's volume, and `[ ! -L ]` is the only thing that
// refuses it before anything moves.
//
// Measured against the previous command, which had `[ -d ]` but no `[ ! -L ]`: `-d`
// follows the link and passes, `ls -A` lists the link's target and passes, the park moves
// the LINK into staging, and the promote then globs `staging/backups/*`, which now
// resolves to `staging/..` — /root. The volume ended up holding `.snapshot`, `.ssh` and
// `.staging`, with the snapshot — the only copy of the customer's pre-restore data —
// inside the very volume the restore was supposed to fill, and the whole command exited
// 0. postRestoreMysql returns true on that, so restore.go logs a completed restore, runs
// no rollback, and reaps the AutoRemove container with the snapshot in the wrong place.
//
// So the assertions that matter are the ones about what did NOT move. The exit code alone
// would pass against a guard that fails after the damage.
func TestPromoteDumpCommandDumpAsSymlinkToParentFails(t *testing.T) {
	data, staging := promoteDirs(t)
	root := containerRoot(t, staging)
	writeFile(t, filepath.Join(data, "index.php"), "an application volume")
	if err := os.Symlink("..", filepath.Join(data, dumpDir)); err != nil {
		t.Fatalf("creating parent symlink: %v", err)
	}

	code, out := runSh(t, promoteDumpCommand(data, staging))

	if code == 0 {
		t.Fatalf("a %s symlink to its parent must fail, got exit 0: %s", dumpDir, out)
	}
	if got, want := names(t, data), []string{dumpDir, "index.php"}; !equalNames(got, want) {
		t.Fatalf("volume = %v, want %v — nothing may move before the check", got, want)
	}
	if got, want := names(t, root), []string{".snapshot", ".ssh"}; !equalNames(got, want) {
		t.Fatalf("the backup container's /root = %v, want %v — the promote swept it", got, want)
	}
	if _, err := os.Stat(filepath.Join(root, ".snapshot", "precious.ibd")); err != nil {
		t.Errorf("the pre-restore snapshot must stay in /root/.snapshot: %v", err)
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Errorf("staging should never have been created: %v", err)
	}
}

// `backups -> .` is the same guard on the shape that needs no traversal at all: the link
// resolves to the datadir itself, so `-d` and `ls -A` both pass on whatever the volume
// happens to contain, and the promote would park the volume and then try to promote it
// back out of a link that no longer points anywhere useful.
func TestPromoteDumpCommandDumpAsSelfSymlinkFails(t *testing.T) {
	data, staging := promoteDirs(t)
	writeFile(t, filepath.Join(data, "index.php"), "an application volume")
	if err := os.Symlink(".", filepath.Join(data, dumpDir)); err != nil {
		t.Fatalf("creating self symlink: %v", err)
	}

	code, out := runSh(t, promoteDumpCommand(data, staging))

	if code == 0 {
		t.Fatalf("a %s symlink to the datadir itself must fail, got exit 0: %s", dumpDir, out)
	}
	if got, want := names(t, data), []string{dumpDir, "index.php"}; !equalNames(got, want) {
		t.Fatalf("volume = %v, want %v — nothing may move before the check", got, want)
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Errorf("staging should never have been created: %v", err)
	}
}

// A RELATIVE symlink to a real prepared dump elsewhere. borg restores symlinks as
// symlinks, so this is a shape an extract can produce.
//
// This is the case that isolates `[ ! -L ]` from every other guard: the target really is
// a prepared dump, carrying xtrabackupMarker, so the precondition's `-d`, `ls -A` and marker
// tests would ALL pass through the link. Only `-L` refuses it. It used to be this file's
// postcondition case — the link resolved from /mnt/data, then dangled once the park had
// moved it to /root/.staging, so the promote moved nothing and left an empty datadir for
// the trailing `[ -n "$(ls -A data)" ]` to catch. Catching it up front is strictly better:
// the extract is still in the volume and there is nothing for rollbackRestore to undo.
func TestPromoteDumpCommandDumpAsRelativeSymlinkFails(t *testing.T) {
	data, staging := promoteDirs(t)
	// A sibling of the data directory, so the link resolves from /mnt/data and not
	// from /root/.staging.
	sibling := filepath.Join(filepath.Dir(data), "dump")
	mkDump(t, sibling)
	writeFile(t, filepath.Join(sibling, "data.ibd"), "consistent, but out of reach after the move")

	if err := os.Symlink("../dump", filepath.Join(data, dumpDir)); err != nil {
		t.Fatalf("creating relative symlink: %v", err)
	}

	code, out := runSh(t, promoteDumpCommand(data, staging))

	if code == 0 {
		t.Fatalf("a relative %s symlink must fail, got exit 0: %s", dumpDir, out)
	}
	if got, want := names(t, data), []string{dumpDir}; !equalNames(got, want) {
		t.Fatalf("volume = %v, want %v — nothing may move before the check", got, want)
	}
	if got, want := names(t, sibling), []string{"data.ibd", xtrabackupMarker}; !equalNames(got, want) {
		t.Fatalf("the link's target = %v, want %v — nothing may move before the check", got, want)
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Errorf("staging should never have been created: %v", err)
	}
}

// An ABSOLUTE symlink to a real prepared dump inside the volume. It isolates `[ ! -L ]`
// the same way the relative case does — every other test passes through the link — and it
// is the shape that would still resolve after the park, so the promote would run to
// completion: the volume would be replaced by a dump that the park had already moved into
// staging, out from under the link.
func TestPromoteDumpCommandDumpAsAbsoluteSymlinkFails(t *testing.T) {
	data, staging := promoteDirs(t)
	target := filepath.Join(data, "realdump")
	mkDump(t, target)
	writeFile(t, filepath.Join(target, "data.ibd"), "consistent")

	if err := os.Symlink(target, filepath.Join(data, dumpDir)); err != nil {
		t.Fatalf("creating absolute symlink: %v", err)
	}

	code, out := runSh(t, promoteDumpCommand(data, staging))

	if code == 0 {
		t.Fatalf("an absolute %s symlink must fail, got exit 0: %s", dumpDir, out)
	}
	if got, want := names(t, data), []string{dumpDir, "realdump"}; !equalNames(got, want) {
		t.Fatalf("volume = %v, want %v — nothing may move before the check", got, want)
	}
	if got, want := names(t, target), []string{"data.ibd", xtrabackupMarker}; !equalNames(got, want) {
		t.Fatalf("the link's target = %v, want %v — nothing may move before the check", got, want)
	}
}

// The shape every other guard here is blind to, and the reason step 2 exists: an ordinary
// application volume with an ordinary directory called `backups` in it. A WordPress site
// keeps its own backups there. It is a real, non-symlink, non-empty directory, so
// `[ ! -L ]`, `[ -d ]` and `[ -n "$(ls -A …)" ]` all pass, and the postcondition passes
// too, because the promote does move something.
//
// What it moves is the disaster. The site is parked in staging, the volume is emptied and
// refilled with the contents of the site's own backups/ folder, and the command exits 0 —
// so postRestore returns true, restore.go logs a completed restore, no rollback runs, and
// the AutoRemove container is reaped with the parked site and the pre-restore snapshot
// inside it. The customer's site is gone and the task says it succeeded.
//
// xtrabackupMarker is what tells the two apart: backupMysql and prepareMysqlBackup
// (strategy_mysql_backup.go) run xtrabackup/mariabackup against
// `--target-dir=<datadir>/backups`, and those binaries always write it there. A site's own
// backups/ folder does not have one.
func TestPromoteDumpCommandOrdinaryBackupsDirectoryFails(t *testing.T) {
	data, staging := promoteDirs(t)
	writeFile(t, filepath.Join(data, "index.php"), "an application volume")
	writeFile(t, filepath.Join(data, "wp-config.php"), "the site's config")
	writeFile(t, filepath.Join(data, ".htaccess"), "not a database at all")
	mkdirAll(t, filepath.Join(data, dumpDir))
	writeFile(t, filepath.Join(data, dumpDir, "site-2026-07-31.tar.gz"), "the customer's own backup")

	code, out := runSh(t, promoteDumpCommand(data, staging))

	if code == 0 {
		t.Fatalf("an ordinary directory named %s must fail, got exit 0: %s", dumpDir, out)
	}
	if got, want := names(t, data), []string{".htaccess", dumpDir, "index.php", "wp-config.php"}; !equalNames(got, want) {
		t.Fatalf("volume = %v, want %v — the site must be exactly where the extract left it", got, want)
	}
	if got, want := names(t, filepath.Join(data, dumpDir)), []string{"site-2026-07-31.tar.gz"}; !equalNames(got, want) {
		t.Fatalf("%s = %v, want %v", dumpDir, got, want)
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Errorf("staging should never have been created: %v", err)
	}
}

// The marker failure gets its own diagnostic, and it has to be a different one. "No mysql
// dump directory" is false and actively misleading for a directory that is sitting right
// there: what the operator needs to know is that it exists and is not a prepared dump,
// which nearly always means the restore was aimed at a volume that is not a database —
// see restore.go's filePaths switch on the SOURCE volume's strategy.
//
// It names EVERY accepted marker, and that is not decoration. The message is the whole of
// what an operator gets — postRestoreMysql calls repo.Container.Exec directly, so the
// controller sees this text and nothing else — and the reader's next move is to look in the
// directory for the file it names. Naming only one sends someone hunting for
// `xtrabackup_checkpoints` in a MariaDB 11.1+ dump that was never going to contain it.
//
// The subshell wrapper with stdout discarded is what makes this an assertion about stderr
// specifically; runSh merges the two streams.
func TestPromoteDumpCommandMarkerDiagnosticReachesStderr(t *testing.T) {
	data, staging := promoteDirs(t)
	mkdirAll(t, filepath.Join(data, dumpDir))
	writeFile(t, filepath.Join(data, dumpDir, "site-2026-07-31.tar.gz"), "the customer's own backup")

	code, out := runSh(t, "( "+promoteDumpCommand(data, staging)+" ) 1>/dev/null")

	if code == 0 {
		t.Fatalf("a directory with no marker must fail, got exit 0: %s", out)
	}
	if want := filepath.Join(data, dumpDir) + " holds no "; !strings.Contains(out, want) {
		t.Errorf("stderr = %q, want it to contain %q", out, want)
	}
	for _, marker := range dumpMarkers {
		if !strings.Contains(out, marker) {
			t.Errorf("stderr = %q, want it to name %q — an operator cannot look for a file the "+
				"diagnostic never mentions", out, marker)
		}
	}
	if notWant := "no mysql dump directory"; strings.Contains(out, notWant) {
		t.Errorf("stderr = %q, must not contain %q — the directory is there, it is just not a dump", out, notWant)
	}
}

// The diagnostic has to be explicit, and it has to be on stderr.
//
// postRestoreMysql calls repo.Container.Exec directly rather than repo.RunShell, so
// nothing on this path runs classify/failureReason (backup/borg/exec.go). A bare failing
// `[ … ]` would reach the controller as "postRestoreMysql cleanup returned a non-zero
// exit code" with an empty body — strictly LESS diagnostic than the accidental
// `mv: cannot stat '/root/.staging/backups/*'` the old command produced.
//
// The subshell wrapper with stdout discarded is what makes this an assertion about
// stderr specifically; runSh merges the two streams, so without it the test would pass
// on a message echoed to stdout, which Container.Exec's TTY would merge but a caller
// reading only stderr would not see.
func TestPromoteDumpCommandDiagnosticReachesStderr(t *testing.T) {
	data, staging := promoteDirs(t)
	writeFile(t, filepath.Join(data, "ibdata1"), "no dump beside it")

	code, out := runSh(t, "( "+promoteDumpCommand(data, staging)+" ) 1>/dev/null")

	if code == 0 {
		t.Fatalf("a missing dump directory must fail, got exit 0: %s", out)
	}
	want := "no mysql dump directory at " + filepath.Join(data, dumpDir)
	if !strings.Contains(out, want) {
		t.Errorf("stderr = %q, want it to contain %q", out, want)
	}
}

// The staged check and the postcondition are brace-wrapped, and this is what that buys.
//
// Both are written `{ test || { echo …; exit 1; }; }` rather than `test || { … }`,
// because the whole command is one `&&` chain: an unwrapped `||` binds to the chain, not
// to its own test, so it fires when ANY earlier step has failed. A failed `mv` in the park
// would then be reported to the operator as "mysql promote left an empty datadir", which
// is a different problem with a different cause and would send whoever reads it looking in
// the wrong place — and, since these are the only strings this path produces (see
// TestPromoteDumpCommandDiagnosticReachesStderr for why), it is all they would have.
//
// The park is made to fail the way it can fail in production: a `mv` that cannot overwrite
// what is already at the destination. Its own diagnostic must be what survives.
//
// There is no case here for the postcondition FIRING, and that is deliberate rather than
// an omission: with `[ ! -L ]` refusing a symlinked dump up front and the staged check
// re-asserting on the globbed path, no shape an extract can produce is known to reach a
// promote that moves nothing. The postcondition stays as the one guard phrased in terms of
// the invariant rather than of a shape someone thought of; what is testable about it is
// that it does not speak out of turn.
func TestPromoteDumpCommandFailedParkDoesNotBlameTheDatadir(t *testing.T) {
	data, staging := promoteDirs(t)
	writeFile(t, filepath.Join(data, "ibdata1"), "hot copy, torn")
	mkDump(t, filepath.Join(data, dumpDir))
	// A directory at the destination that the park's `mv` cannot overwrite with a file.
	mkdirAll(t, filepath.Join(staging, "ibdata1"))

	code, out := runSh(t, "( "+promoteDumpCommand(data, staging)+" ) 1>/dev/null")

	if code == 0 {
		t.Fatalf("a park that could not move the volume aside must fail, got exit 0: %s", out)
	}
	if !strings.Contains(out, "ibdata1") || !strings.Contains(out, "mv:") {
		t.Errorf("stderr = %q, want mv's own diagnostic about ibdata1", out)
	}
	if notWant := "left an empty datadir"; strings.Contains(out, notWant) {
		t.Errorf("stderr = %q, must not contain %q — the postcondition's `||` is binding to the "+
			"whole chain instead of to its own test", out, notWant)
	}
	if notWant := "staged mysql dump directory"; strings.Contains(out, notWant) {
		t.Errorf("stderr = %q, must not contain %q — the staged check's `||` is binding to the "+
			"whole chain instead of to its own test", out, notWant)
	}
}
