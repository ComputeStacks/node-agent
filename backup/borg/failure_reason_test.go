package borg

import "testing"

// Real borg 1.4.4 --log-json records, captured from a `delete --stats --force` of a
// non-existent archive. Container.Exec allocates a TTY, so records arrive \r\n-framed.
const (
	notFoundRecord = `{"type": "log_message", "time": 1785442534.2241745, "message": "Archive does-not-exist-logfix-9999 not found (1/1).", "levelname": "WARNING", "name": "borg.archiver"}`
	statsRecord    = `{"type": "log_message", "time": 1785442533.3912702, "message": "Deleted data:                    0 B                  0 B                  0 B", "levelname": "INFO", "name": "borg.output.stats"}`
)

var failureReasonChecks = []struct {
	name string
	in   string
	out  string
}{
	{
		name: "diagnosis with stats",
		in:   notFoundRecord + "\r\n" + statsRecord + "\r\n" + statsRecord + "\r\n" + statsRecord + "\r\n",
		out:  "Archive does-not-exist-logfix-9999 not found (1/1).",
	},
	{
		name: "stats only",
		in:   statsRecord + "\r\n" + statsRecord + "\r\n",
		out:  "",
	},
	{
		name: "non-json line",
		in:   "sh: 1: borg: not found\r\n",
		out:  "sh: 1: borg: not found",
	},
	{
		name: "non-json line with stats",
		in:   "sh: 1: borg: not found\r\n" + statsRecord + "\r\n",
		out:  "sh: 1: borg: not found",
	},
	{
		name: "empty response",
		in:   "",
		out:  "",
	},
}

func TestFailureReason(t *testing.T) {
	for _, i := range failureReasonChecks {
		t.Run(i.name, func(t *testing.T) {
			result := failureReason(i.in)
			if result != i.out {
				t.Errorf("Received %q, wanted %q", result, i.out)
			}
		})
	}
}
