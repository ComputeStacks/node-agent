package backup

import (
	"encoding/json"
	"testing"
)

func TestProgressFailureReasonIsFirstPostedLine(t *testing.T) {
	p := newProgress()
	if got := p.FailureReason(); got != "" {
		t.Fatalf("FailureReason with nothing posted = %q, want empty", got)
	}
	p.Record("borg stats")
	p.PostEventUpdate("a", "Unable to find an IP address for database container db-1\nmore output")
	p.PostEventUpdate("b", "postBackupMysql cleanup returned a non-zero exit code")
	if got, want := p.FailureReason(), "Unable to find an IP address for database container db-1"; got != want {
		t.Fatalf("FailureReason = %q, want %q", got, want)
	}
}

func TestRunTaskSoftFailureCarriesReason(t *testing.T) {
	// A soft failure (EventLog.Status set, no error returned) must surface the
	// posted reason in result_json.error, not the generic fallback.
	for _, tc := range []struct {
		name   string
		posted string
		want   string
	}{
		{"with reason", "Unable to inspect the database container: boom", "Unable to inspect the database container: boom"},
		{"no reason", "", "task reported failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newProgress()
			if tc.posted != "" {
				p.PostEventUpdate("x", tc.posted)
			}
			p.EventLog.Status = "failed"
			err := softFailureError(p)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			var res map[string]any
			if jErr := json.Unmarshal(p.Result(err), &res); jErr != nil {
				t.Fatal(jErr)
			}
			if res["error"] != tc.want {
				t.Fatalf("result error = %v, want %q", res["error"], tc.want)
			}
		})
	}
}
