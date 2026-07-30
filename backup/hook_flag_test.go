package backup

import (
	"os"
	"strings"
	"testing"
)

// TestBackupPathUsesBackupContinueOnError guards against re-introducing the
// copy-paste error this test exists for: the Archive.Create failure branch in
// Perform gated its postBackup call on vol.RestoreContinueOnError — the flag for
// the RESTORE hooks (restore_error_cont) — instead of vol.BackupContinueOnError
// (backup_error_cont), which the sibling failed-preBackup branch uses. The effect
// was that a mysql/postgres volume whose backup failed skipped postBackup, so
// postBackupMysql never removed /mnt/data/backups and the dump was left on the
// customer's volume until the next successful backup.
//
// It is a source-level guard (like TestTaskHandlersHaveNoRecover) because the
// branch itself is unreachable without docker and a borg repository: everything
// around the condition — Archive.Create, postBackup, containermgr exec — needs a
// live daemon, so nothing short of an integration environment can observe the
// wrong flag being read. What is checkable here is the invariant that makes the
// bug impossible: backup.go is the backup path and has no business naming the
// restore flag at all.
func TestBackupPathUsesBackupContinueOnError(t *testing.T) {
	src, err := os.ReadFile("backup.go")
	if err != nil {
		t.Fatalf("read backup.go: %v", err)
	}
	if strings.Contains(string(src), "RestoreContinueOnError") {
		t.Error("backup.go names RestoreContinueOnError (restore_error_cont); the backup path must " +
			"gate its hooks on BackupContinueOnError (backup_error_cont) — the restore flag belongs " +
			"to restore.go/restore_hooks.go")
	}
	if !strings.Contains(string(src), "vol.BackupContinueOnError") {
		t.Error("backup.go no longer reads vol.BackupContinueOnError; both failure branches in " +
			"Perform must consult it before running postBackup")
	}
}
