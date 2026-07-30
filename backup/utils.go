package backup

import (
	"cs-agent/backup/borg"
	"cs-agent/log"
	"strings"

	"github.com/hashicorp/go-hclog"
)

func backupLogger() hclog.Logger {
	return log.New().Named("backup")
}

// borgFailure renders a borg LogMessage as a single-line failure reason for a task
// error or an event update. MsgID is only populated when the message came from borg's
// own JSON output, so the "(msgid) " prefix is included only when there is one —
// otherwise every message the agent synthesizes itself carried a bare "() " prefix
// into the controller's logs.
func borgFailure(msg *borg.LogMessage) string {
	if msg == nil {
		return "unknown backup failure"
	}
	reason := strings.TrimSpace(msg.Message)
	if reason == "" {
		reason = "no failure detail reported"
	}
	if msg.MsgID == "" {
		return reason
	}
	return "(" + msg.MsgID + ") " + reason
}
