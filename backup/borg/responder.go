package borg

import (
	"encoding/json"
	"strings"

	"github.com/getsentry/sentry-go"
)

func readRepoResponse(response string) (RepositoryResponse, *LogMessage) {
	var logMsg LogMessage
	var repoResponse RepositoryResponse

	// An empty response splits into a single "" element, and item[0:1] below would
	// panic on it (readArchiveRestoreResponse already guards this). Report it as a
	// failure, because an empty response is always a fault on this host and never a
	// verdict about the repository: `borg info --json` on a healthy repo prints a
	// payload, and a missing repo exits 2 WITH an ERROR record. Nothing is left that
	// legitimately writes nothing except a borg that died before it could — an OOM
	// kill or a signal, which Container.Exec reports as (code, "", nil).
	//
	// Returning a nil *LogMessage instead would be worse than the panic it replaces:
	// the zero RepositoryResponse routes into FindRepository's
	// `repoResponse == (RepositoryResponse{})` branch, which synthesises
	// Repository.DoesNotExist and makes backup.Perform run Repository.Setup — so a
	// killed process would silently become an auto-init attempt, with no panic, no
	// Sentry and no log line.
	//
	// No MsgID: this is our diagnosis, not borg's, and it must never be mistaken for
	// borg's verdict — an empty MsgID also cannot reach the auto-init branch.
	if response == "" {
		borgLogger().Warn("Empty response from borg", "function", "readRepoResponse")
		return repoResponse, &LogMessage{Message: "Empty response from borg while reading repository info"}
	}

	for _, item := range strings.Split(response, "\n{") {
		if item[0:1] != "{" {
			item = "{" + item
		}
		var marshalErr error
		if strings.Contains(item, "msgid") {
			marshalErr = json.Unmarshal([]byte(item), &logMsg)
		} else {
			marshalErr = json.Unmarshal([]byte(item), &repoResponse)
			if marshalErr != nil {
				logMsg.Message = item
			}
		}
		if marshalErr != nil {
			borgLogger().Debug("readRepoResponse", "item", item)
			borgLogger().Debug("readRepoResponse", "error", marshalErr.Error())
			sentry.ConfigureScope(func(scope *sentry.Scope) {
				scope.SetExtra("logMsg", item)
			})
			sentry.CaptureException(marshalErr)
			return repoResponse, &logMsg
		}
		if logMsg != (LogMessage{}) {
			if logMsg.Type != "question_env_answer" && logMsg.Type != "question_prompt" {
				borgLogger().Debug("readRepoResponse", "LogMessage", logMsg.ToYaml())
				return repoResponse, &logMsg
			}
		}
	}
	return repoResponse, nil
}

func readRepoContentResponse(response string) (RepositoryContentResponse, *LogMessage) {
	var logMsg LogMessage
	var repoResponse RepositoryContentResponse

	// Same empty-response guard as readRepoResponse, and a failure for the same
	// reason: `borg list --json` on an empty but valid repository still prints
	// {"archives": [], ...}, so no output at all is a fault, not "no archives".
	//
	// Reporting no failure here would also blank the controller's view of this
	// volume: Repository.Sync would upsert a repository row with zero sizes and an
	// empty archive list, and that stands until the next successful sync. Today's
	// panic at least writes nothing.
	if response == "" {
		borgLogger().Warn("Empty response from borg", "function", "readRepoContentResponse")
		return repoResponse, &LogMessage{Message: "Empty response from borg while reading repository contents"}
	}

	for _, item := range strings.Split(response, "\n{") {
		if item[0:1] != "{" {
			item = "{" + item
		}
		var marshalErr error
		if strings.Contains(item, "msgid") {
			marshalErr = json.Unmarshal([]byte(item), &logMsg)
		} else {
			marshalErr = json.Unmarshal([]byte(item), &repoResponse)
			if marshalErr != nil {
				logMsg.Message = item
			}
		}
		if marshalErr != nil {
			borgLogger().Debug("readRepoResponse", "item", item)
			borgLogger().Debug("readRepoResponse", "error", marshalErr.Error())
			sentry.ConfigureScope(func(scope *sentry.Scope) {
				scope.SetExtra("logMsg", item)
			})
			sentry.CaptureException(marshalErr)
			return repoResponse, &logMsg
		}
		if logMsg != (LogMessage{}) {
			if logMsg.Type != "question_env_answer" && logMsg.Type != "question_prompt" {
				borgLogger().Debug("readRepoResponse", "LogMessage", logMsg.ToYaml())
				return repoResponse, &logMsg
			}
		}
	}
	return repoResponse, nil
}

func readArchiveRestoreResponse(response string) *LogMessage {
	if response == "" {
		return nil
	}
	var logMsg LogMessage
	for _, item := range strings.Split(response, "\n{") {
		if item[0:1] != "{" {
			item = "{" + item
		}
		var marshalErr error
		marshalErr = json.Unmarshal([]byte(item), &logMsg)
		if marshalErr != nil {
			borgLogger().Debug("readRepoResponse", "item", item)
			borgLogger().Debug("readRepoResponse", "error", marshalErr.Error())
			sentry.ConfigureScope(func(scope *sentry.Scope) {
				scope.SetExtra("logMsg", item)
			})
			sentry.CaptureException(marshalErr)
			return &logMsg
		}
		if logMsg != (LogMessage{}) {
			if logMsg.Type != "question_env_answer" && logMsg.Type != "question_prompt" {
				borgLogger().Debug("readArchiveRestoreResponse", "LogMessage", logMsg.ToYaml())
				return &logMsg
			}
		}
	}
	return nil
}
