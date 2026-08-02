package backup

import (
	"cs-agent/backup/borg"
	"cs-agent/types"
	"strings"
)

// stagingPath is where the promote parks the extracted archive while it moves the SQL
// dump into the volume, and dumpDir is the directory inside the volume that the dump
// arrives in.
//
// stagingPath, like snapshotPath, is in the backup container's OWN filesystem rather
// than in the volume, and that container is created with AutoRemove — so whatever is
// left in it when the container is reaped is gone. Here that is deliberate rather than a
// limitation: what is left there is the archived datadir, and discarding it is the whole
// point of the promote. See promoteDumpCommand.
//
// dumpDir is not a name this file chooses. preBackupMysql points
// xtrabackup/mariabackup at `--target-dir=<datadir>/backups`, so that is the name the
// archive carries, and postBackupMysql removes `/mnt/data/backups` from the live volume
// again afterwards (both in strategy_mysql_backup.go). rollbackRestoreMysql still spells
// the same path literally.
//
// The dump markers are the files that tell a prepared dump apart from any other directory
// that happens to be called `backups`. See promoteDumpCommand for why that distinction is
// the difference between a restore and an unrecoverable data loss reported as success, and
// for the evidence that one of these files is always in a dump this agent produced.
//
// There are two of them because the file was RENAMED. MariaDB 11.1 renamed
// mariadb-backup's metadata files from `xtrabackup_*` to `mariadb_backup_*`; Percona
// xtrabackup, and mariabackup up to and including MariaDB 11.0, still write the old name.
// Both must be accepted, and neither can be dropped: a MariaDB 11.1+ server's dumps carry
// only the new name, while archives taken before that upgrade — and every mysql-variant
// volume, which uses Percona xtrabackup — carry only the old one.
const (
	stagingPath = "/root/.staging"
	dumpDir     = "backups"

	// xtrabackupMarker is what Percona xtrabackup and MariaDB ≤ 11.0 write.
	xtrabackupMarker = "xtrabackup_checkpoints"
	// mariadbMarker is what mariadb-backup writes from MariaDB 11.1 on.
	mariadbMarker = "mariadb_backup_checkpoints"
)

// dumpMarkers is the set the promote accepts, most-established name first. Any ONE of them
// identifies the directory as a prepared dump.
var dumpMarkers = []string{xtrabackupMarker, mariadbMarker}

// preRestoreMysql has no strategy-specific work left to do.
//
// It once snapshotted /mnt/data and then, later, stopped the database. Both moved out and
// for the same reason: neither was mysql's business. preRestore takes the snapshot once for
// every strategy, and it now stops the service's containers once for every strategy too,
// because an ordinary application volume needed that exactly as much as a database did —
// see stopServiceContainers for what that stop does and does not cover.
//
// It is kept, with its case in preRestore's switch, as the place a genuinely mysql-specific
// pre-restore step would go — and because a hook in that switch still runs while the service
// is up, which a hook below the stop would not. postBackupPostgres, postRestorePostgres and
// rollbackRestorePostgres are the same shape.
func preRestoreMysql(vol *types.Volume, event *progress, repo *borg.Repository) (preRestoreMysqlSuccess bool) {
	return true
}

// promoteDumpCommand builds the shell command that turns a freshly extracted
// mysql-strategy archive into a datadir a server can actually be started against.
//
// Such an archive holds the database twice. `borg create` archives the whole volume, so
// it carries the datadir exactly as it looked while mysqld was writing to it — a hot,
// torn copy — and, sitting inside that datadir, the prepared xtrabackup/mariabackup dump
// in dumpDir, which is the crash-consistent copy and the entire reason this strategy
// exists. `borg extract` puts both back. Six steps promote the second over the first:
//
//  1. refuse to go any further unless the dump is really there;
//  2. refuse to go any further unless it is really a DUMP and not just a directory of
//     that name;
//  3. move everything the extract wrote into staging;
//  4. empty the volume;
//  5. refuse to go any further unless the dump is STILL really there, at the staged path
//     the next step is about to glob;
//  6. move the dump's own contents out of staging and into the volume.
//
// The archived datadir stays in staging and is never put back. That is not a loss being
// tolerated, it is the objective: it is the copy taken from a running database, which is
// precisely why the dump exists alongside it. staging is inside the AutoRemove backup
// container, so it goes away with the container.
//
// The precondition runs FIRST, and that is a behaviour change. The command this replaced
// discovered a missing dump at its LAST step, where `mv /root/.staging/backups/* …` was
// handed an unexpanded literal and failed — after it had already parked the extract and
// emptied the volume, leaving rollbackRestore to undo all of it. Checking up front means
// a mysql archive with no dump in it fails with the extract still sitting in /mnt/data
// and nothing moved.
//
// Checking at all is now mandatory rather than tidy. moveEntriesCommand is deliberately
// tolerant of an empty source — restoring into a brand-new empty volume is how the
// controller clones one — so the naive port of the old command exits 0 when there is no
// dump to promote. postRestoreMysql would return true, the task would report success,
// and the volume would be empty while the only copy of the customer's data sat in
// /root/.snapshot inside a container about to be reaped.
//
// `[ ! -L dump ]` comes FIRST, and it is what stops this promote from being a way to
// empty the backup container's own /root into the customer's volume. dumpDir is a name
// the EXTRACT supplies, not one this agent controls, and borg restores a symlink as a
// symlink — so an archive can carry `backups -> ..`, and every other test here follows
// symlinks. Without `-L`, measured against the exact string this function builds:
// `[ -d ]` follows the link and passes, `ls -A` lists the link's TARGET and passes, step
// 3 moves the link itself into staging, and at step 6 `staging/backups` resolves to
// `staging/..` — /root, which is where snapshotPath lives, i.e. the only copy of the
// customer's pre-restore data. Step 6 sweeps /root into the volume, the postcondition
// finds a non-empty datadir and passes, postRestoreMysql returns true, restore.go logs a
// completed restore, no rollback runs, and the AutoRemove container is reaped with the
// snapshot now sitting inside the volume as `.snapshot`. Exit 0 throughout.
//
// entryGlobs is what made that reachable. The dot-blind `mv staging/backups/* data/`
// this replaced was handed an unexpanded literal, because every entry of /root begins
// with a dot, and so failed by accident — the same accident that used to catch a missing
// dump directory.
//
// Step 5 repeats `[ ! -L ] && [ -d ]` on the STAGED path, because that is the path step 6
// actually globs and it is NOT the path the precondition looked at: the link moves
// between the two, and a relative target resolves against wherever the link now sits.
// It is brace-wrapped so its `||` binds inside its own group rather than catching a
// failure from an earlier step in the `&&` chain. No shape an extract can produce is
// known to reach it while the precondition stands — they check the same inode — which is
// exactly what it is for: it is the layer that still holds if a later edit reorders the
// steps, relaxes the check up front, or gives staging a `backups` entry of its own.
//
// `[ -d dump ]` AND `[ -n "$(ls -A dump)" ]`, and neither alone. Measured in dash
// against GNU coreutils 9.7:
//
//   - `-n "$(ls -A dump)"` alone passes for a REGULAR FILE named `backups`, because
//     `ls -A` handed a file prints that file's name, and for a DANGLING SYMLINK named
//     `backups`, because `ls -A` prints the link's own path on stdout and exits 0
//     without writing to stderr. Both shapes reach step 6 with nothing to promote.
//   - `-d dump` alone passes for a dump directory that exists but is empty.
//
// Dropping `-d` was measured, and the trailing postcondition below does still catch both
// shapes — but only after the volume has been emptied, with the failure handed to
// rollbackRestore. The two are layers, not duplicates: `-d` decides whether anything
// moves at all, the postcondition decides whether an empty datadir can be reported as a
// successful restore.
//
// None of these shapes is theoretical. restore.go overrides filePaths from a
// `switch vol.Strategy` on the SOURCE volume while every hook here is handed &destVol,
// so an ordinary application volume cloned into a mysql-strategy destination runs this
// promote over content that never had a dump directory in it.
//
// Which brings up the shape none of those tests can see, and step 2. A real, non-empty
// DIRECTORY named `backups` is the MOST ordinary thing on an application volume — a
// WordPress site's own backups/ folder — and to `[ ! -L ]`, `[ -d ]` and `[ -n ]` it is
// indistinguishable from a prepared dump. Left uncovered, that volume's restore parks the
// whole site in staging, replaces the volume with the contents of the site's own backups/
// folder, passes the staged check and the postcondition, and exits 0. postRestore returns
// true, so no rollback runs, and the AutoRemove container is reaped with both the parked
// site and the snapshot inside it. Unrecoverable, and reported to the controller as a
// successful restore.
//
// So the promote asks for identification: a checkpoints file, under EITHER of the two
// names the backup binaries write (dumpMarkers). That is not a guess about the format, it
// is the file this agent's own backups always leave there, and three facts are what make
// it safe to insist on:
//
//   - backupMysql runs `xtrabackup`/`mariabackup`/`mariadb-backup --backup` with
//     `--target-dir=<datadir>/backups`, and prepareMysqlBackup then runs `--prepare`
//     against that same directory (both in strategy_mysql_backup.go). Every one of those
//     binaries writes a checkpoints file into the target directory; it is core to the
//     format rather than incidental — `--prepare` reads it, rewrites it, and incremental
//     backups chain off the LSNs in it — so it is there both before and after the prepare
//     that this strategy always performs.
//   - What it is CALLED depends on the binary, which is the whole reason dumpMarkers is a
//     set. MariaDB 11.1 renamed mariadb-backup's metadata files from `xtrabackup_*` to
//     `mariadb_backup_*`; the old names are still read as a fallback when preparing an
//     older backup, but a fresh backup writes only the new ones. Percona xtrabackup, and
//     mariabackup up to MariaDB 11.0, write only the old ones. Testing for a single name
//     therefore rejects valid dumps this agent produced itself — which is exactly what
//     happened: a MariaDB 12 volume clone failed this check on a good prepared dump, the
//     restore rolled back, and the destination volume was left empty.
//   - No other dump shape has ever existed here to be broken by the requirement.
//     `mysqldump` appears nowhere under backup/ in this repository's whole history
//     (`git log --all -S mysqldump -- backup/` is empty), and
//     `--target-dir=<datadir>/backups` dates to the initial commit, 613c594.
//
// An archive that predates any of this therefore still carries one of the markers, and the
// shapes that carry neither are exactly the ones that must not be promoted. Adding a name
// here is safe and dropping one is not: an old archive is restorable for as long as it is
// retained, so `xtrabackup_checkpoints` cannot be retired once every server has moved to
// MariaDB 11.1+.
//
// When the marker is the thing missing the operator is told precisely that, in its own
// diagnostic naming both accepted files: a directory that exists but is not a dump is a
// different problem from no directory at all, and it usually means the volume is not a
// database and the restore was aimed at the wrong strategy.
//
// What the marker does not buy, and no check at this layer can: a directory named
// `backups` that happens to contain a file called `xtrabackup_checkpoints` without being
// a dump this agent produced is still promoted, with the consequences described above.
// The identification is of the format, not of the provenance.
//
// The marker check makes `[ -n "$(ls -A dump)" ]` redundant — a directory holding a
// regular file is not empty — and with it, `ls -A` versus bare `ls` no longer changes any
// outcome either, because a marker is itself a visible entry. Both stay: the emptiness
// test is the layer that survives someone relaxing the marker check, and it owns the
// distinct "no mysql dump directory" diagnostic for an empty one. What the `-A` was there
// for in the first place — a dump whose only content is hidden, as MyRocks' `.rocksdb`
// makes possible — the marker now covers outright.
//
// EVERY check here echoes an explicit diagnostic to stderr rather than being left as a
// bare failing `[ … ]`, the postcondition included. postRestoreMysql calls
// repo.Container.Exec directly and not repo.RunShell, so nothing on this path runs
// classify/failureReason (backup/borg/exec.go): a silent test failure surfaces to the
// controller as "postRestoreMysql cleanup returned a non-zero exit code" with an empty
// body — strictly less diagnostic than the `mv: cannot stat
// '/root/.staging/backups/*'` the old command produced by accident.
//
// The trailing `[ -n "$(ls -A data)" ]` is a postcondition on the invariant every check
// above can only approximate: a promote must not leave an empty datadir, whatever the
// cause. It is brace-wrapped for the same reason step 5 is — unwrapped, its `||` would
// fire on a failure from any earlier step in the chain and blame an empty datadir for a
// failed `mv`. Its known trigger used to be a relative `backups -> ../dump`, which
// resolved before the move and dangled after it; `[ ! -L ]` now refuses that shape up
// front, and no shape an extract can produce is known to reach this line any more. It
// stays because it is the only guard phrased in terms of the invariant itself rather
// than of a shape someone thought of, and because failing here is safe rather than
// merely loud: postRestore returning false sends restore.go's postRestore branch through
// rollbackRestore, which puts the snapshot back.
//
// Nothing is quoted, so data and staging must stay free of shell metacharacters, and of
// `[` and `]` as well since entryGlobs splices them into a bracket expression.
// Production passes /mnt/data and /root/.staging.
func promoteDumpCommand(data, staging string) string {
	dump := data + "/" + dumpDir
	staged := staging + "/" + dumpDir

	// One `-f` test per accepted marker name, joined with `||`: any one of them identifies
	// the directory as a prepared dump. Built from dumpMarkers rather than spelled out, so
	// the test and the diagnostic below cannot name different sets.
	markerTests := make([]string, 0, len(dumpMarkers))
	for _, marker := range dumpMarkers {
		markerTests = append(markerTests, "[ -f "+dump+"/"+marker+" ]")
	}

	return "{ [ ! -L " + dump + " ] && [ -d " + dump + ` ] && [ -n "$(ls -A ` + dump + `)" ]; } || ` +
		`{ echo "no mysql dump directory at ` + dump + `" >&2; exit 1; }` +
		" && { " + strings.Join(markerTests, " || ") + " || " +
		`{ echo "` + dump + ` holds no ` + strings.Join(dumpMarkers, " or ") +
		`, so it is not a prepared mysql dump" >&2; exit 1; }; }` +
		" && " + moveEntriesCommand(data, staging) +
		" && " + clearEntriesCommand(data) +
		" && { [ ! -L " + staged + " ] && [ -d " + staged + " ] || " +
		`{ echo "staged mysql dump directory ` + staged + ` is a symlink or is missing" >&2; exit 1; }; }` +
		" && " + moveEntriesCommand(staged, data) +
		` && { [ -n "$(ls -A ` + data + `)" ] || ` +
		`{ echo "mysql promote left an empty datadir at ` + data + `" >&2; exit 1; }; }`
}

// postRestoreMysql promotes the extracted archive's SQL dump to be the volume's
// contents. See promoteDumpCommand for what that means and for every guard in the
// command it runs.
func postRestoreMysql(event *progress, repo *borg.Repository) bool {

	exitCode, out, err := repo.Container.Exec([]string{"sh", "-c", promoteDumpCommand(dataPath, stagingPath)})

	if err != nil {
		backupLogger().Warn("Failed to execute mysql cleanup on restore", "error", err.Error())
		event.PostEventUpdate("agent-c24b0abcd88acdff", withOutput(err.Error(), out))
		return false
	}
	if exitCode > 0 {
		event.PostEventUpdate("agent-c24b0abcd88acdff", withOutput("postRestoreMysql cleanup returned a non-zero exit code", out))
		return false
	}

	return true

}

// rollbackRestoreMysql removes the SQL dump directory that a mysql-strategy backup
// leaves in the volume.
//
// rollbackRestore has already emptied /mnt/data and moved the snapshot back by the time
// this runs, so the `rm -rf /mnt/data/*` and the `mv` that used to be here are gone.
// Running them here as well was the data-loss path: the borg layer's own rollback moved
// the snapshot back into /mnt/data, and then this function's rm deleted it while its
// unguarded mv failed against the now-empty snapshot, leaving the volume empty and the
// snapshot empty in an AutoRemove container.
func rollbackRestoreMysql(event *progress, repo *borg.Repository) bool {

	var rollbackCmd []string
	var execCmd []string
	rollbackCmd = append(rollbackCmd, "rm", "-rf /mnt/data/backups")

	execCmd = append(execCmd, "sh", "-c", strings.Join(rollbackCmd, " "))

	exitCode, out, err := repo.Container.Exec(execCmd)

	if err != nil {
		backupLogger().Warn("Failed to store database backup", "error", err.Error())
		event.PostEventUpdate("agent-af1b0badd5d9b9f6", withOutput(err.Error(), out))
		return false
	}
	if exitCode > 0 {
		event.PostEventUpdate("agent-af1b0badd5d9b9f6", withOutput("rollbackRestoreMysql returned a non-zero exit code", out))
		return false
	}

	return true

}
