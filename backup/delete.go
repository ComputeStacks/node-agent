package backup

import (
	"context"
	"cs-agent/backup/borg"
	"cs-agent/store"
	"cs-agent/types"
	"errors"
	"strings"

	"github.com/getsentry/sentry-go"
)

func DeleteBackup(ctx context.Context, st *store.Store, task store.Task, projectEvent *progress) error {
	params := parseParams(task)

	v, found, err := st.GetVolume(ctx, task.Volume)
	if err != nil {
		backupLogger().Warn("Error loading volume from store", "function", "DeleteBackup()", "error", err.Error())
		return err
	}
	if !found {
		// n1: a controller-POSTed delete against a stale/unknown volume is a
		// truthful failure, not a false "completed".
		backupLogger().Warn("Delete failed: unknown volume", "function", "DeleteBackup()", "volume", task.Volume)
		projectEvent.EventLog.Status = "failed"
		projectEvent.PostEventUpdate("agent-delete-unknown-volume", "volume not found")
		return errors.New("unknown volume: " + task.Volume)
	}

	vol, err := types.LoadVolume(v.Config)
	if err != nil {
		backupLogger().Warn("Fatal error parsing volume", "volume", task.Volume, "error", err.Error())
		sentry.CaptureException(err)
		return err
	}

	repo, findRepoErr := borg.FindRepository(st, &types.Volume{Name: task.Volume}, &types.Volume{Name: params.SourceVolume})

	if findRepoErr != nil {
		projectEvent.EventLog.Status = "failed"
		if findRepoErr.Message == "" {
			projectEvent.PostEventUpdate("agent-18e788c90cc0760d", "Failed to find backup repository.")
		} else {
			projectEvent.PostEventUpdate("agent-55f82aabccdb8643", findRepoErr.Message)
		}
		backupLogger().Warn("Failed to find repository", "name", task.Volume, "archive", task.Archive, "function", "DeleteBackup", "error", findRepoErr.Message)
		return errors.New(borgFailure(findRepoErr))
	}

	archive, findArchiveErr := repo.FindArchive(task.Archive)

	if findArchiveErr != nil {
		backupLogger().Warn("Error deleting repository", "volume", vol.Name, "response", findArchiveErr.Message)
		repo.StopContainer()
		projectEvent.EventLog.Status = "failed"
		// The message, not ToYaml(): the rendered struct is mostly empty fields
		// (levelname/msgid/name blank, time 0) for a message the agent synthesized,
		// which is not a readable reason in result_json.
		projectEvent.PostEventUpdate("agent-e67edd61abe38301", borgFailure(findArchiveErr))
		return errors.New(borgFailure(findArchiveErr))
	}

	deleteResponse, deleteArchiveErr := archive.Delete()
	repo.StopContainer()

	if deleteArchiveErr != nil {
		backupLogger().Warn("Error deleting archive", "volume", vol.Name, "archive", archive.Name, "response", deleteArchiveErr.Message)
		projectEvent.EventLog.Status = "failed"
		projectEvent.PostEventUpdate("agent-a7ae639c559b3088", borgFailure(deleteArchiveErr))
		return errors.New(borgFailure(deleteArchiveErr))
	}

	var output []string
	for _, o := range deleteResponse {
		output = append(output, o.Message)
	}
	// Success: keep borg's full --stats table in result_json for the controller, but
	// don't log it at INFO — the borg layer already logs a concise "Completed
	// Archive Delete event" line (volume + archive), and the multi-line stats table
	// reads like a truncated failure in the node log. The full response is only
	// worth logging at INFO on failure (see deleteArchiveErr above).
	projectEvent.Record(strings.Join(output, "\n"))
	return nil
}
