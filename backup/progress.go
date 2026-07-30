package backup

import (
	"encoding/json"
	"strings"
	"sync"
)

// progress collects a task's step messages and terminal outcome LOCALLY, replacing
// the old csevent HTTP push to the controller. Per the v3.0.0 contract the
// controller learns a task's outcome from its changelog `result_json` (terminal
// status + accumulated output), not a live per-step stream — so this just
// accumulates: PostEventUpdate appends a line (and logs it at INFO), Record
// appends a line (and logs it at DEBUG), Set records a structured success field
// (export url/size/…, a backup's last_backup), and Result() renders the JSON the
// worker stores via UpdateTaskStatus.
//
// It deliberately mirrors the old *csevent.ProjectEvent surface (PostEventUpdate,
// CloseEvent, EventLog.Status) so the many handler/hook call sites are unchanged.
type progress struct {
	// EventLog.Status mirrors the old field; handlers set it to "failed" on a
	// soft failure that doesn't return an error. Starts "running".
	EventLog struct{ Status string }

	mu     sync.Mutex
	lines  []string
	fields map[string]any
}

func newProgress() *progress {
	p := &progress{fields: map[string]any{}}
	p.EventLog.Status = "running"
	return p
}

// PostEventUpdate records a step message AND logs it at INFO. Safe for concurrent
// use (some hooks call it from a goroutine). code is the legacy op code — logged
// for correlation, not stored in the result. Use this for failures/anomalies,
// where the full borg/hook response belongs in the node log; for verbose SUCCESS
// output that the controller reads from result_json but that would flood the log,
// use Record instead.
func (p *progress) PostEventUpdate(code, msg string) {
	if p == nil {
		return
	}
	p.Record(msg)
	backupLogger().Info("task step", "code", code, "msg", msg)
}

// Record appends a step message to the task result AND logs it at DEBUG. Use for
// verbose success output (e.g. borg's per-archive stats) that the controller
// reads from result_json but that would flood the node log at INFO on every
// backup. The concise success line is logged separately by the borg layer; the
// DEBUG line keeps the full payload reachable on the node with log.level=debug
// instead of only via result_json.
func (p *progress) Record(msg string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.lines = append(p.lines, msg)
	p.mu.Unlock()
	backupLogger().Debug("task step", "msg", msg)
}

// CloseEvent is retained for call-site compatibility; the worker owns finalizing
// the task (UpdateTaskStatus) now, so this is a no-op.
func (p *progress) CloseEvent() {}

// Set records a structured success field for the task result (e.g. an export's
// url/object_key/size/expiry, or a backup's last_backup).
func (p *progress) Set(key string, value any) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.fields[key] = value
	p.mu.Unlock()
}

// Failed reports a soft failure recorded via EventLog.Status without a returned
// error. "cancelled" (e.g. a mysql master offline, which the old csevent path set)
// maps to a failed task too — both mean the backup did not happen.
func (p *progress) Failed() bool {
	return p != nil && (p.EventLog.Status == "failed" || p.EventLog.Status == "cancelled")
}

// Result renders the task's result_json: the structured success fields, the
// accumulated output, and — on failure — the error message.
func (p *progress) Result(err error) json.RawMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := make(map[string]any, len(p.fields)+2)
	for k, v := range p.fields {
		m[k] = v
	}
	if out := strings.TrimSpace(strings.Join(p.lines, "\n")); out != "" {
		m["output"] = out
	}
	if err != nil {
		m["error"] = err.Error()
	}
	if len(m) == 0 {
		return nil
	}
	b, mErr := json.Marshal(m)
	if mErr != nil {
		return nil
	}
	return b
}
