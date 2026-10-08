package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"cs-agent/maintenance"
	"cs-agent/store"
)

// --- Node maintenance hold -----------------------------------------------------
//
// The controller sets and clears its own maintenance hold here. Every write
// carries the controller's generation: a request older than the newest one the
// node has applied is refused with 409 and the current status, so a delayed
// retry can never undo a later decision. Every success returns 200 with the
// full status (the caller needs to know whether the node is still paused, e.g.
// by a local hold) and wakes the dispatcher so an un-pause drains promptly.

// maxMaintenanceReasonBytes caps the hold reason.
const maxMaintenanceReasonBytes = 512

// maintenancePutRequest is the body of PUT /v1/admin/maintenance. Gen is a
// pointer so a missing gen is told apart from 0.
type maintenancePutRequest struct {
	Reason string `json:"reason"`
	Gen    *int64 `json:"gen"`
}

// handleAdminMaintenanceGet returns the node's maintenance status.
func (s *Server) handleAdminMaintenanceGet(w http.ResponseWriter, r *http.Request, _ scope) {
	st, err := maintenance.BuildStatus(r.Context(), s.store, s.cfg.ContainerLister)
	if err != nil {
		s.storeError(w, err, "maintenance status")
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// handleAdminMaintenancePut sets or refreshes the controller hold.
func (s *Server) handleAdminMaintenancePut(w http.ResponseWriter, r *http.Request, _ scope) {
	body, ok := s.readBody(w, r)
	if !ok {
		return
	}
	var req maintenancePutRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Gen == nil || *req.Gen < 0 {
		writeError(w, http.StatusBadRequest, "gen is required and must be >= 0")
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		writeError(w, http.StatusBadRequest, "reason is required")
		return
	}
	if len(reason) > maxMaintenanceReasonBytes {
		writeError(w, http.StatusBadRequest, "reason too long")
		return
	}
	m, err := s.store.PutControllerHold(r.Context(), reason, *req.Gen)
	s.writeMaintenanceResult(w, r, m, err, "put controller hold")
}

// handleAdminMaintenanceDelete clears the controller hold, or with all=1 both
// holds (the controller override for a forgotten local hold). With all=1,
// local_since_max=<unix> makes the override conditional: a local hold placed
// after that time is left alone and the request fails with 412 + status.
func (s *Server) handleAdminMaintenanceDelete(w http.ResponseWriter, r *http.Request, _ scope) {
	q := r.URL.Query()
	raw := q.Get("gen")
	if raw == "" {
		writeError(w, http.StatusBadRequest, "gen is required")
		return
	}
	gen, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || gen < 0 {
		writeError(w, http.StatusBadRequest, "invalid gen")
		return
	}
	var all bool
	switch q.Get("all") {
	case "", "0", "false":
	case "1", "true":
		all = true
	default:
		writeError(w, http.StatusBadRequest, "invalid all")
		return
	}

	var localSinceMax int64
	if raw := q.Get("local_since_max"); raw != "" {
		if !all {
			writeError(w, http.StatusBadRequest, "local_since_max requires all=1")
			return
		}
		localSinceMax, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || localSinceMax <= 0 {
			writeError(w, http.StatusBadRequest, "invalid local_since_max")
			return
		}
	}

	var m store.MaintenanceState
	if all {
		m, err = s.store.ClearAllHolds(r.Context(), gen, localSinceMax)
	} else {
		m, err = s.store.ClearControllerHold(r.Context(), gen)
	}
	s.writeMaintenanceResult(w, r, m, err, "clear maintenance hold")
}

// writeMaintenanceResult answers a hold write: 200 + status on success (and
// wakes the dispatcher), 409 + the current status on a stale generation, 412 +
// the current status when a conditional override found a newer local hold. The
// store returns the state as loaded alongside ErrStaleGen, so the 409 body
// shows the generation that won.
func (s *Server) writeMaintenanceResult(w http.ResponseWriter, r *http.Request, m store.MaintenanceState, err error, op string) {
	status := http.StatusOK
	switch {
	case errors.Is(err, store.ErrStaleGen):
		status = http.StatusConflict
	case errors.Is(err, store.ErrLocalHoldNewer):
		status = http.StatusPreconditionFailed
	case err != nil:
		s.storeError(w, err, op)
		return
	default:
		s.fireHook(s.cfg.OnTaskCreated) // an un-pause should drain promptly
	}
	st, err := maintenance.StatusFromState(r.Context(), s.store, s.cfg.ContainerLister, m)
	if err != nil {
		s.storeError(w, err, "maintenance status")
		return
	}
	writeJSON(w, status, st)
}
