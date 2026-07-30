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

	switch vol.Strategy {
	case "mysql":
		return preRestoreMysql(vol, event, repo)
	case "postgres":
		return preRestorePostgres(vol, event, repo)
	default:
		return true
	}

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
	switch vol.Strategy {
	case "mysql":
		return rollbackRestoreMysql(event, repo)
	case "postgres":
		return rollbackRestorePostgres(event, repo)
	default:
		return true
	}
}
