package dbbackup

import "time"

// Status is the runner's last-known state, also persisted in the state file.
type Status struct {
	Enabled       bool       `json:"enabled"`
	LastSuccessAt *time.Time `json:"last_success_at"`
	LastAttemptAt *time.Time `json:"last_attempt_at"`
	LastError     string     `json:"last_error,omitempty"`
	LastErrorAt   *time.Time `json:"last_error_at,omitempty"`
	Uploaded      int        `json:"uploaded"`
	Skipped       int        `json:"skipped"`
	Failed        int        `json:"failed"`
	DBs           int        `json:"dbs"`
	Lineage       string     `json:"lineage"`
}

// Status returns a copy of the current status.
func (r *Runner) Status() Status {
	var s Status
	if p := r.pub.Load(); p != nil {
		s = *p
	}
	s.Enabled = r.cfg.Enabled
	return s
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}
