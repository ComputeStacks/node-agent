// Package backup
/*
# Restore Hooks

preRestore will run before the backup is restored to the server, and before the container is
powered off.

postRestore will run after the backup is transferred to the container, but before the container
is booted back up.
*/
package backup

import (
	"cs-agent/backup/borg"
	"cs-agent/containermgr"
	"cs-agent/types"
	"fmt"
	"strconv"
	"strings"
)

// dataPath is the volume being restored and snapshotPath is where its current contents
// are set aside for the duration of the restore, both as seen inside the backup
// container.
//
// Known limitation, deliberately not addressed here: snapshotPath is in the backup
// container's own filesystem rather than in the volume, and that container is created
// with AutoRemove, so the snapshot exists only for as long as the container does. A
// rollback is therefore possible only before repo.StopContainer().
//
// Restore's deferred StopContainer does keep every rollback call site inside that
// window, but the window is the only thing standing between a failed restore and
// permanent data loss: any path that leaves rollbackRestore without putting the
// snapshot back has destroyed it, because the container goes away moments later and
// takes the snapshot with it. That is why the put-back is the first thing
// rollbackRestore does, ahead of every step that can fail.
const (
	dataPath     = "/mnt/data"
	snapshotPath = "/root/.snapshot"
)

// snapshotCommand builds a shell command that moves the contents of src into dst,
// creating dst first. It is the single implementation of that move: preRestore uses it
// to set the volume aside, rollbackRestore uses it with src and dst swapped to put the
// volume back.
//
// `set --` and the test that follows are what make an empty src a success. A bare
// `mv src/* dst/` exits non-zero when src is empty, because the glob does not expand
// and mv is handed the literal string `src/*` — and restoring into an empty destination
// is a first-class flow: cloning a volume restores into a brand-new one. Now that a
// non-zero exit fails the restore, an empty source had to stop looking like a failure.
//
// `|| [ -L "$1" ]` is mandatory, not defensive. `[ -e ]` follows symlinks, so a
// dangling symlink that happens to sort first — `current -> releases/gone`, an ordinary
// shape for an application volume — would make a directory full of data read as empty.
// The move would be skipped, the command would exit 0, and the extract would then run
// over live data with nothing to roll back to: exactly the accident the guard exists to
// prevent, wearing a success exit code.
//
// Glob semantics are otherwise unchanged on purpose. `set -- src/*` still skips
// dotfiles, exactly as `mv src/*` did, so dotfiles are still neither snapshotted nor
// put back by a rollback — a pre-existing gap that needs its own reasoning about `.`
// and `..`, and changing it here would quietly change what a rollback restores.
// Quoting "$@" is a free fix: filenames containing spaces now survive, where the old
// unquoted glob broke them.
func snapshotCommand(src, dst string) string {
	return "mkdir -p " + dst + " && set -- " + src + "/*" +
		` && if [ -e "$1" ] || [ -L "$1" ]; then mv "$@" ` + dst + `/; fi`
}

// rollbackCommand builds the shell command that puts the snapshot back: empty the volume
// of whatever the failed restore left in it, then run the same move in the other
// direction.
//
// It is a function rather than a string composed at its one call site so that the tests
// exercise the command production actually runs. While rollbackRestoreSnapshot built it
// inline and the rollback tests rebuilt the same string themselves, a lost `&&` or a
// changed path in production would have left every one of those tests passing.
//
// `rm -rf dst/*` needs no guard of its own — `rm -f` exits 0 on an unexpanded glob — and
// it cannot destroy anything unrecoverable: src is empty only when dst was empty when
// the snapshot was taken, so what this removes is either nothing or what the failed
// restore itself just wrote there.
func rollbackCommand(src, dst string) string {
	return "rm -rf " + dst + "/* && " + snapshotCommand(src, dst)
}

// takeRestoreSnapshot moves the volume's current contents aside so that a restore which
// fails partway can be undone.
//
// It runs for every strategy, exactly once, and a failure halts the restore: a restore
// whose rollback would not be available must not begin. That is the whole reason it
// lives in preRestore rather than in the borg layer — preRestore is gated on success
// before anything touches the archive, so by the time any rollback can be reached the
// snapshot has already been taken.
func takeRestoreSnapshot(event *progress, repo *borg.Repository) bool {
	res := repo.RunShell("restore snapshot", []string{snapshotCommand(dataPath, snapshotPath)})
	if res.Failure != nil {
		backupLogger().Warn("Failed to snapshot existing data", "volume", repo.Name, "exitCode", res.ExitCode, "error", res.Failure.Message)
		event.PostEventUpdate("agent-82c8d22caa01995d", withOutput("Failed to move the existing volume data aside, halting restore: "+res.Failure.Message, res.Response))
		return false
	}
	return true
}

// rollbackRestoreSnapshot puts the snapshot back, over whatever the failed restore left
// behind. See rollbackCommand for why the command it runs lives in its own function.
func rollbackRestoreSnapshot(event *progress, repo *borg.Repository) bool {
	res := repo.RunShell("restore rollback", []string{rollbackCommand(snapshotPath, dataPath)})
	if res.Failure != nil {
		backupLogger().Warn("Failed to roll back restore snapshot", "volume", repo.Name, "exitCode", res.ExitCode, "error", res.Failure.Message)
		event.PostEventUpdate("agent-af1b0badd5d9b9f6", withOutput("Failed to move the snapshot back into the volume: "+res.Failure.Message, res.Response))
		return false
	}
	return true
}

func preRestore(vol *types.Volume, event *progress, repo *borg.Repository) (preRestoreSuccess bool) {

	if len(vol.PreRestore) > 2 {
		success := false
		defer func() (preRestoreSuccess bool) {
			if r := recover(); r != nil {
				event.PostEventUpdate("agent-cd090c1cc5c19617", fmt.Sprintf("%#v", r))
				return false
			}
			return success
		}()
		exitCode, out, err := containermgr.ServiceExec(strconv.Itoa(vol.ServiceID), vol.PreRestore)

		if err != nil {
			event.PostEventUpdate("agent-b98d45dff8fd639b", withOutput(err.Error(), out))
			return false
		}

		if exitCode > 0 {
			if vol.RestoreContinueOnError {
				finalMsg := "Pre-Restore command returned a non-zero exit code (" + strconv.Itoa(exitCode) + "): " + strings.Join(vol.PreRestore, " ")
				event.PostEventUpdate("agent-17a5e40308439ab3", withOutput(finalMsg, out))
			} else {
				finalMsg := "Restored halted due to non-zero exit code (" + strconv.Itoa(exitCode) + "): " + strings.Join(vol.PreRestore, " ")
				event.PostEventUpdate("agent-059e93c9920612b8", withOutput(finalMsg, out))
				return false
			}
		}

	}

	// The strategy hooks run first and keep only their strategy-specific work: for every
	// database strategy that is stopping the database containers. The snapshot then runs
	// once, for every strategy, and this order is not interchangeable — snapshotPath is
	// in the backup container's own filesystem, not the volume, so the move is a
	// cross-device copy-and-delete rather than a rename. Copying a running database's
	// data directory that way and then deleting the original would put a torn copy in
	// the snapshot and nothing in the volume, which is why the database has to stop
	// first. That makes a missing case here a data-integrity bug, not a missed
	// optimisation.
	//
	// mariadb is a strategy the controller accepts alongside mysql, and it took the
	// no-op default until now. It belongs here, with mysql, and nowhere else: it joins
	// neither postRestore's switch nor rollbackRestore's, because postRestoreMysql
	// rearranges a backups/ dump directory that only a mysql-strategy archive contains
	// — preBackup has no mariadb case either, so nothing ever creates one. A plain
	// extract and a plain snapshot put-back are what a mariadb archive needs.
	switch vol.Strategy {
	case "mysql", "mariadb":
		if !preRestoreMysql(vol, event, repo) {
			return false
		}
	case "postgres":
		if !preRestorePostgres(vol, event, repo) {
			return false
		}
	}

	return takeRestoreSnapshot(event, repo)
}

func postRestore(vol *types.Volume, event *progress, repo *borg.Repository) bool {

	if len(vol.PostRestore) > 2 {
		defer func() bool {
			if r := recover(); r != nil {
				event.PostEventUpdate("agent-b5117962943e98cb", fmt.Sprintf("%#v", r))
				return false
			}
			return true
		}()
		exitCode, out, err := containermgr.ServiceExec(strconv.Itoa(vol.ServiceID), vol.PostRestore)

		if err != nil {
			event.PostEventUpdate("agent-1b5d010969199a18", withOutput(err.Error(), out))
			return false
		}

		if exitCode > 0 {
			finalMsg := "Post-Backup commands returned a non-zero exit code (" + strconv.Itoa(exitCode) + "): " + strings.Join(vol.PostRestore, " ")
			event.PostEventUpdate("agent-9330f19b388b4428", withOutput(finalMsg, out))
			return false
		}

	}
	switch vol.Strategy {
	case "mysql":
		return postRestoreMysql(event, repo)
	case "postgres":
		return postRestorePostgres(event, repo)
	default:
		return true
	}
}

func rollbackRestore(vol *types.Volume, event *progress, repo *borg.Repository) bool {

	// The snapshot goes back first: once, for every strategy — including the default
	// one, which had no rollback at all before and simply left the volume as the failed
	// restore had left it — and before anything that can return early.
	//
	// The order is not a preference. Nothing below is a precondition for the put-back:
	// it is two shell commands inside the backup container, independent of the service's
	// own containers and of whatever the user's hook does. The reverse order was
	// actively unsafe. The user hook below ran first and returned false on error, so for
	// any volume with a PostRestore command configured the put-back was never reached,
	// and the deferred repo.StopContainer() then took the only copy of the customer's
	// data with the AutoRemove container.
	if !rollbackRestoreSnapshot(event, repo) {
		return false
	}

	// This hook cannot currently succeed on this path, and is kept only because removing
	// it is a separate decision: every rollbackRestore call site is downstream of
	// Restore's container stop loop, and ServiceExec resolves containers with
	// FindByService(…, allowOff: false), which collects only containers whose state is
	// "running". With none running it returns "no containers found" and this block
	// reports a rollback failure. It no longer costs the snapshot, which is the part
	// that mattered.
	if len(vol.PostRestore) > 0 {
		// A recovered panic leaves rollbackRestore's unnamed bool return at its zero
		// value false, i.e. "rollback failed" — which is what the caller reports.
		// This closure's own return value was never consumed, so it has none.
		defer func() {
			if r := recover(); r != nil {
				event.PostEventUpdate("agent-c290fcc106e4f78a", fmt.Sprintf("%#v", r))
			}
		}()
		exitCode, out, err := containermgr.ServiceExec(strconv.Itoa(vol.ServiceID), vol.PostRestore)

		if err != nil {
			event.PostEventUpdate("agent-9393516879f411ea", withOutput(err.Error(), out))
			return false
		}

		if exitCode > 0 {
			finalMsg := "Post-Backup commands returned a non-zero exit code (" + strconv.Itoa(exitCode) + "): " + strings.Join(vol.PostRestore, " ")
			event.PostEventUpdate("agent-cf02d05dd4d77905", withOutput(finalMsg, out))
			return false
		}

	}

	// The strategy hooks clean up after the put-back, keeping only their
	// strategy-specific work.
	switch vol.Strategy {
	case "mysql":
		return rollbackRestoreMysql(event, repo)
	case "postgres":
		return rollbackRestorePostgres(event, repo)
	default:
		return true
	}
}
