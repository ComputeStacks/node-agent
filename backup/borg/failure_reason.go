package borg

import (
	"encoding/json"
	"strings"
)

// failureReason extracts borg's own diagnosis from a --log-json response, dropping
// the `--stats` table (logger name "borg.output.stats"), which explains nothing about
// a failure. Lines that are not valid JSON are kept verbatim — a shell-level error
// (e.g. "sh: borg: not found") is the diagnosis. Records are \r\n-framed because
// Container.Exec allocates a TTY, so lines are trimmed before parsing.
//
// Returns "" when borg emitted no diagnosis at all (only --stats output), so the
// caller can say so concisely instead of dumping the stats table as the reason.
func failureReason(response string) string {
	var reasons []string
	for _, line := range strings.Split(response, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var record LogMessage
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			// Not borg JSON at all (shell error, wrapper output) — it is the diagnosis.
			reasons = append(reasons, line)
			continue
		}
		if record.Name == "borg.output.stats" {
			continue
		}
		if record.Message != "" {
			reasons = append(reasons, record.Message)
		}
	}
	return strings.Join(reasons, "; ")
}
