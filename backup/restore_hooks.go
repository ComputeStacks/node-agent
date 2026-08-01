// Package backup
/*
# Restore Hooks

preRestore will run before the backup is restored to the server, and it is what powers the
service's containers off: it runs the user's PreRestore command against the still-running
service, then stops the service's containers itself, and only then moves /mnt/data aside.
Restore's own stop loop runs after preRestore returns and is a confirmation pass, not the
stop that protects the volume.

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

	"github.com/docker/docker/client"
	"github.com/spf13/viper"
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

// stopServiceContainers stops every container belonging to the volume's service, so that
// the snapshot and the extract which follow it do not run underneath a live writer. It is
// all-or-nothing: if any container refuses to stop, every one of them is started again and
// the restore is halted. A restore that cannot quiesce the service must not begin, and a
// service left half down would be worse than either outcome.
//
// It starts nothing on its success path. Restore restarts the container list it captured
// before preRestore ran, on every way out — success, rollback and failure alike — and does
// so regardless of who stopped them. The mysql strategy has always relied on that; every
// strategy relies on it now.
//
// FindAllByService is called with allowOff: true, so the list includes containers that are
// already stopped, on which Stop() is a harmless no-op. That matters now that this runs for
// every restore, including restores of a service that is already down.
//
// This NARROWS the window between the service's last write and the snapshot. It does not
// close it. Four writers survive it:
//
//   - A bastion container is skipped by label. FindAllByService drops any container whose
//     com.computestacks.role is "backup" or "bastion" (containermgr/containermgr.go).
//     Dropping "backup" is necessary — that is the container this restore is running
//     inside. Dropping "bastion" is a real gap rather than a formality: the skip can only
//     ever affect a container that already matched the com.computestacks.service_id filter,
//     so the exclusion's existence is itself the evidence that a bastion can belong to the
//     service being restored, and write access to that service's volumes is the whole
//     purpose of an SFTP/SSH container. A customer with a transfer in flight keeps writing
//     to /mnt/data through the snapshot and through the extract, and neither this function
//     nor Restore's stop loop touches them.
//   - Zero containers is indistinguishable from success. FindAllByService returns
//     (nil, nil) when nothing matches the filter, the loop body never runs, and this
//     returns true. types.Volume.ServiceID is an unvalidated int decoded straight from the
//     controller's JSON, so a stale service id — or the zero value from a config that never
//     carried one — "stops the service" successfully while every container of the real
//     service stays up and writing.
//   - A container created after this function's own FindAllByService is never stopped.
//     There is one ContainerList call here and no lock held afterwards, so anything that
//     appears in between — a rescheduled container, an operator starting the service back
//     up — is writing to the volume by the time the extract runs.
//   - Containers on another host that share the storage are out of reach entirely. This
//     speaks only to the local docker daemon.
//
// The former name was stopAllMysqlContainers, after its first caller rather than after what
// it does. Nothing in it was ever mysql-specific: it is FindAllByService on the volume's
// service, Stop() on each result, and a restart of all of them if any refuses.
func stopServiceContainers(vol *types.Volume, event *progress) bool {
	cli, cliErr := client.NewClientWithOpts(client.WithVersion(viper.GetString("docker.version")))
	if cliErr != nil {
		backupLogger().Warn("Failed to connect to docker", "error", cliErr.Error(), "function", "stopServiceContainers")
		event.PostEventUpdate("agent-45a73ed06ea34e25", cliErr.Error())
		return false
	}
	containers, findAllErr := containermgr.FindAllByService(cli, strconv.Itoa(vol.ServiceID), true)
	if findAllErr != nil {
		backupLogger().Warn("Failed to retrieve containers", "error", findAllErr.Error(), "function", "stopServiceContainers")
		event.PostEventUpdate("agent-0248a778f49a1eb4", findAllErr.Error())
		return false
	}
	failedToStop := false
	for _, c := range containers {
		if !c.Stop() {
			failedToStop = true
		}
	}
	if failedToStop {
		event.PostEventUpdate("agent-6becd55bb6a584de", "Failed to stop some containers, unable to restore.")
		for _, c := range containers {
			_ = c.Start() // Ignore containers that fail to start
		}
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

	// The strategy hooks keep only their strategy-specific work, and both of the two that
	// exist are now empty: what they used to do — stop the service's containers — is the
	// step below, because it turned out not to be strategy work at all.
	//
	// They stay ahead of that stop as a convention rather than as a live constraint: a
	// pre-restore hook that needs to talk to a running server, the way preBackupPostgres
	// issues `checkpoint;`, has to run while the server is still up, and this switch is
	// the only place left that is. Nothing in the switch depends on it today, so do not
	// read this as protecting anything that currently exists — it is the reason the order
	// is this way round if a hook ever needs it again.
	//
	// mariadb is a strategy the controller accepts alongside mysql. It belongs in this
	// switch and nowhere else: it joins neither postRestore's switch nor
	// rollbackRestore's, because postRestoreMysql rearranges a backups/ dump directory
	// that only a mysql-strategy archive contains — preBackup has no mariadb case either,
	// so nothing ever creates one. A plain extract and a plain snapshot put-back are what
	// a mariadb archive needs.
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

	// Stopping the service is not strategy work, and treating it as such was a
	// data-integrity bug for every strategy that had no case above — which is most
	// volumes. snapshotPath is in the backup container's own filesystem rather than in the
	// volume, so moving /mnt/data aside is a cross-device copy followed by an unlink, not a
	// rename. Run that under any live writer — a database, or an ordinary application
	// rewriting a cache directory — and the copy is torn while the original is deleted out
	// from under it: a torn snapshot, an emptied volume, and a rollback that restores the
	// tear. Until this line existed, a default-strategy volume watched /mnt/data empty
	// beneath it while it was still writing, and Restore's own stop loop was downstream of
	// preRestore and always too late to prevent it.
	//
	// This must stay AFTER the vol.PreRestore block above. That hook reaches its container
	// through containermgr.ServiceExec → FindByService(…, allowOff: false), which collects
	// only containers whose state is "running". Stopping first would break every configured
	// PreRestore command with "no containers found" — the identical bug rollbackRestore's
	// own hook already has, documented at its call site below.
	//
	// And it must stay BEFORE takeRestoreSnapshot, which is the whole point of it.
	if !stopServiceContainers(vol, event) {
		return false
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
