package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"cs-agent/store"

	"github.com/google/uuid"
)

// --- Customer routes (project comes from sc.projectID, NEVER the URL) --------

// handleCustomerDBGet reads customer_kv[path] for the authenticated tenant.
func (s *Server) handleCustomerDBGet(w http.ResponseWriter, r *http.Request, sc scope) {
	path := r.PathValue("path")
	e, found, err := s.store.CustomerGet(r.Context(), sc.projectID, path)
	if err != nil {
		s.storeError(w, err, "customer get")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	writeValue(w, e)
}

// handleCustomerDBPut upserts customer_kv[path] for the authenticated tenant.
// Full-value replace. The stored value is NOT size-capped (that is the point of
// the cutover); only the transport body is bounded via http.MaxBytesReader.
func (s *Server) handleCustomerDBPut(w http.ResponseWriter, r *http.Request, sc scope) {
	path := r.PathValue("path")
	body, ok := s.readBody(w, r)
	if !ok {
		return
	}
	if err := s.store.CustomerPut(r.Context(), sc.projectID, path, r.Header.Get("Content-Type"), body); err != nil {
		s.storeError(w, err, "customer put")
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleCustomerDBDelete removes customer_kv[path] for the authenticated tenant.
func (s *Server) handleCustomerDBDelete(w http.ResponseWriter, r *http.Request, sc scope) {
	path := r.PathValue("path")
	if err := s.store.CustomerDelete(r.Context(), sc.projectID, path); err != nil {
		s.storeError(w, err, "customer delete")
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleCustomerManagedGet reads managed_kv[path] for the authenticated tenant.
// Read-only: there is intentionally no customer-facing managed WRITE route —
// managed_kv is platform-written, customer-read-only.
func (s *Server) handleCustomerManagedGet(w http.ResponseWriter, r *http.Request, sc scope) {
	path := r.PathValue("path")
	e, found, err := s.store.ManagedGet(r.Context(), sc.projectID, path)
	if err != nil {
		s.storeError(w, err, "managed get")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	writeValue(w, e)
}

// handleShimMetadata is the single legacy compatibility path: the old
// monarx entrypoint does GET …/v1/kv/projects/{token}/metadata?raw=true.
//
// Authoritative identity is the Bearer (requireCustomer already scoped us to one
// project). The {token} path segment is IGNORED for authorization — a token can
// only ever read its OWN managed metadata regardless of what {token} says. We
// serve managed_kv["metadata"] (the controller-pushed minimal blob) as raw
// JSON. Only ?raw=true is handled (monarx always sends it); anything else is a
// 400 (we never emit Consul's base64-wrapped envelope).
func (s *Server) handleShimMetadata(w http.ResponseWriter, r *http.Request, sc scope) {
	if r.URL.Query().Get("raw") != "true" {
		writeError(w, http.StatusBadRequest, "only ?raw=true is supported by this endpoint")
		return
	}
	e, found, err := s.store.ManagedGet(r.Context(), sc.projectID, "metadata")
	if err != nil {
		s.storeError(w, err, "shim managed get")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "no metadata for this project")
		return
	}
	// monarx pipes the body straight into jq; serve the stored blob verbatim as
	// JSON. The content type the controller pushed wins if set, else default to
	// application/json (the shim only ever serves the JSON metadata blob).
	ct := e.ContentType
	if ct == "" {
		ct = "application/json"
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(e.Value)
}

// --- Admin routes (project_id is EXPLICIT in the URL path) -------------------
//
// NOTE: the admin DATA routes (managed/* and db/*) assume the project was
// provisioned via PUT /v1/admin/tenants/{project_id}. They do not gate on the
// tenant row existing: a write to a never-provisioned (but path-valid) id will
// lazily create a stray <project_id>.db. That is intentional at this stage and
// not a security concern — admin is a trusted, per-node scope and the project_id
// is validated by the store (ErrInvalidProjectID → 400 before any file is
// touched for an unsafe id). No gating logic is added here yet.

// handleAdminManagedPut writes managed_kv[path] for the path's project. This is
// the controller pushing the minimal managed blob (e.g. path "metadata").
func (s *Server) handleAdminManagedPut(w http.ResponseWriter, r *http.Request, _ scope) {
	projectID := r.PathValue("project_id")
	path := r.PathValue("path")
	body, ok := s.readBody(w, r)
	if !ok {
		return
	}
	if err := s.store.ManagedPut(r.Context(), projectID, path, r.Header.Get("Content-Type"), body); err != nil {
		s.storeError(w, err, "admin managed put")
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleAdminManagedDelete removes managed_kv[path] for the path's project.
func (s *Server) handleAdminManagedDelete(w http.ResponseWriter, r *http.Request, _ scope) {
	projectID := r.PathValue("project_id")
	path := r.PathValue("path")
	if err := s.store.ManagedDelete(r.Context(), projectID, path); err != nil {
		s.storeError(w, err, "admin managed delete")
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleAdminDBGet reads customer_kv[path] for any project (the UI's
// cross-tenant reads).
func (s *Server) handleAdminDBGet(w http.ResponseWriter, r *http.Request, _ scope) {
	projectID := r.PathValue("project_id")
	path := r.PathValue("path")
	e, found, err := s.store.CustomerGet(r.Context(), projectID, path)
	if err != nil {
		s.storeError(w, err, "admin db get")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	writeValue(w, e)
}

// handleAdminDBPut upserts customer_kv[path] for any project.
func (s *Server) handleAdminDBPut(w http.ResponseWriter, r *http.Request, _ scope) {
	projectID := r.PathValue("project_id")
	path := r.PathValue("path")
	body, ok := s.readBody(w, r)
	if !ok {
		return
	}
	if err := s.store.CustomerPut(r.Context(), projectID, path, r.Header.Get("Content-Type"), body); err != nil {
		s.storeError(w, err, "admin db put")
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleAdminDBDelete removes customer_kv[path] for any project.
func (s *Server) handleAdminDBDelete(w http.ResponseWriter, r *http.Request, _ scope) {
	projectID := r.PathValue("project_id")
	path := r.PathValue("path")
	if err := s.store.CustomerDelete(r.Context(), projectID, path); err != nil {
		s.storeError(w, err, "admin db delete")
		return
	}
	w.WriteHeader(http.StatusOK)
}

// tenantBody is the JSON body of PUT /v1/admin/tenants/{project_id}.
type tenantBody struct {
	TokenHash string `json:"token_hash"`
	Status    string `json:"status"`
}

// handleAdminTenantPut provisions/re-provisions a tenant: UpsertTenant then
// CreateProjectDB. The project_id is taken from the path; the token_hash + status
// from the JSON body. (token_hash, never a plaintext token, is what the store
// records.)
func (s *Server) handleAdminTenantPut(w http.ResponseWriter, r *http.Request, _ scope) {
	projectID := r.PathValue("project_id")
	body, ok := s.readBody(w, r)
	if !ok {
		return
	}
	var tb tenantBody
	if err := json.Unmarshal(body, &tb); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if tb.TokenHash == "" {
		writeError(w, http.StatusBadRequest, "token_hash is required")
		return
	}
	if err := s.store.UpsertTenant(r.Context(), projectID, tb.TokenHash, tb.Status); err != nil {
		if errors.Is(err, store.ErrTenantExists) {
			writeError(w, http.StatusConflict, "token_hash already mapped to a different project")
			return
		}
		s.storeError(w, err, "upsert tenant")
		return
	}
	if err := s.store.CreateProjectDB(r.Context(), projectID); err != nil {
		s.storeError(w, err, "create project db")
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleAdminTenantDelete de-provisions a tenant: DeleteTenant then
// DeleteProjectDB. Both are idempotent no-ops on an absent project.
func (s *Server) handleAdminTenantDelete(w http.ResponseWriter, r *http.Request, _ scope) {
	projectID := r.PathValue("project_id")
	if err := s.store.DeleteTenant(r.Context(), projectID); err != nil {
		s.storeError(w, err, "delete tenant")
		return
	}
	if err := s.store.DeleteProjectDB(r.Context(), projectID); err != nil {
		s.storeError(w, err, "delete project db")
		return
	}
	s.limiter.forget(projectID) // drop the de-provisioned tenant's rate-limit bucket
	w.WriteHeader(http.StatusOK)
}

// --- Container actions + changelog -------------------------------------------

// actionCreateRequest is the body of POST /v1/actions. project_id is NEVER read
// from the body — it is stamped from the authenticated tenant scope (there is no
// project_id field here, so a smuggled one is silently ignored). params is
// opaque JSON the agent does not interpret.
type actionCreateRequest struct {
	ActionType string          `json:"action_type"`
	Params     json.RawMessage `json:"params"`
}

// actionCreateResponse is the 202 body: the agent-generated id + initial status.
type actionCreateResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// changelogListResponse is the GET /v1/admin/changelog body.
type changelogListResponse struct {
	Entries []store.ChangelogEntry `json:"entries"`
	// HighWater is the last seq this request scanned (or since when nothing was
	// scanned). Rows hidden from this reader count as scanned, so a reader should
	// advance its cursor to HighWater, not to the last entry it received.
	HighWater int64 `json:"high_water"`
}

// changelogTypesHeader lists (comma-separated) the optional changelog entity
// types the reader understands. Only node_maintenance is optional today.
const changelogTypesHeader = "X-CS-Changelog-Types"

const (
	maxActionTypeLen   = 128       // action_type sanity bound
	maxActionBodyBytes = 128 << 10 // tight cap on the whole /v1/actions body
	maxParamsBytes     = 64 << 10  // cap on the opaque params blob

	defaultChangelogLimit = 100
	maxChangelogLimit     = 1000
)

// handleActionCreate records a container's fire-and-forget action request. The
// agent is generic: it validates only that action_type is present and of sane
// length and that params is within cap — it never interprets the action (that is
// the controller's job). The project is stamped from sc.projectID, never the
// body. The outbox row + its changelog row are written atomically; a controller
// pulls it later. Rate-limited per tenant. Returns 202 with the new id.
func (s *Server) handleActionCreate(w http.ResponseWriter, r *http.Request, sc scope) {
	// Rate-limit before reading/parsing the body so the limiter bounds CPU/alloc,
	// not just the DB write; a 429 need not consume the body.
	if !s.limiter.allow(sc.projectID) {
		writeError(w, http.StatusTooManyRequests, "too many action requests; slow down")
		return
	}
	body, ok := s.readBodyLimited(w, r, maxActionBodyBytes)
	if !ok {
		return
	}
	var req actionCreateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.ActionType == "" {
		writeError(w, http.StatusBadRequest, "action_type is required")
		return
	}
	if len(req.ActionType) > maxActionTypeLen {
		writeError(w, http.StatusBadRequest, "action_type too long")
		return
	}
	if len(req.Params) > maxParamsBytes {
		writeError(w, http.StatusBadRequest, "params too large")
		return
	}
	// Normalize "no params": both an omitted params and an explicit JSON null
	// become SQL NULL. (A bare `null` RawMessage would otherwise store the literal
	// text "null", differing from the omitted case.)
	params := req.Params
	if len(params) == 0 || string(params) == "null" {
		params = nil
	}
	ar, err := s.store.CreateActionRequest(r.Context(), uuid.New().String(), sc.projectID, req.ActionType, params)
	if err != nil {
		s.storeError(w, err, "create action request")
		return
	}
	writeJSON(w, http.StatusAccepted, actionCreateResponse{ID: ar.ID, Status: ar.Status})
}

// handleAdminChangelogList is the controller's pull channel: changelog rows with
// seq > since, ordered by seq, capped by limit (default 100, max 1000),
// optionally filtered by entity_type. Reuses the per-node admin Bearer.
func (s *Server) handleAdminChangelogList(w http.ResponseWriter, r *http.Request, _ scope) {
	q := r.URL.Query()

	var since int64
	if raw := q.Get("since"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid since")
			return
		}
		since = n
	}

	limit := defaultChangelogLimit
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		if n > maxChangelogLimit {
			n = maxChangelogLimit
		}
		limit = n
	}

	entityType := q.Get("entity_type")
	showMaint := advertisesType(r, store.EntityNodeMaintenance)
	entries, highWater, err := s.store.ChangelogPage(r.Context(), since, entityType, limit, showMaint)
	if err != nil {
		s.storeError(w, err, "changelog since")
		return
	}
	// Record that a maintenance-aware reader was served up to highWater; the
	// local CLI uses it to tell that the controller has seen its hold. A page
	// filtered to another entity type proves nothing about node_maintenance rows,
	// so only an unfiltered (or node_maintenance-filtered) page counts. A failure
	// here does not fail the read: the CLI just keeps waiting.
	if showMaint && (entityType == "" || entityType == store.EntityNodeMaintenance) {
		if err := s.store.RecordAdvertisedServe(r.Context(), highWater, time.Now().Unix()); err != nil {
			s.log.Warn("record advertised changelog serve failed", "error", err)
		}
	}
	if entries == nil {
		entries = []store.ChangelogEntry{} // encode [] not null
	}
	writeJSON(w, http.StatusOK, changelogListResponse{Entries: entries, HighWater: highWater})
}

// advertisesType reports whether the request's X-CS-Changelog-Types header
// lists entityType.
func advertisesType(r *http.Request, entityType string) bool {
	for _, v := range r.Header.Values(changelogTypesHeader) {
		for _, t := range strings.Split(v, ",") {
			if strings.TrimSpace(t) == entityType {
				return true
			}
		}
	}
	return false
}

// changelogAckRequest is the body of POST /v1/admin/changelog/ack: the highest seq
// the single per-node consumer has durably projected. The watermark is monotonic —
// a seq at or below the current value is accepted but does not rewind it.
type changelogAckRequest struct {
	Seq int64 `json:"seq"`
}

// changelogAckResponse echoes the resulting (monotonic) watermark, which may be
// higher than the requested seq if a lower/duplicate ack was ignored.
type changelogAckResponse struct {
	Acked int64 `json:"acked"`
}

// handleAdminChangelogAck advances the controller's acked changelog watermark, the
// low-water mark below which the prune janitor may drop rows (subject to an age
// floor). It is how the controller reports durable projection progress so the node
// can reclaim changelog space.
func (s *Server) handleAdminChangelogAck(w http.ResponseWriter, r *http.Request, _ scope) {
	body, ok := s.readBody(w, r)
	if !ok {
		return
	}
	var req changelogAckRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Seq < 0 {
		writeError(w, http.StatusBadRequest, "seq must be >= 0")
		return
	}
	if err := s.store.SetChangelogAcked(r.Context(), req.Seq); err != nil {
		s.storeError(w, err, "set changelog acked")
		return
	}
	acked, err := s.store.GetChangelogAcked(r.Context())
	if err != nil {
		s.storeError(w, err, "get changelog acked")
		return
	}
	writeJSON(w, http.StatusOK, changelogAckResponse{Acked: acked})
}

// --- DOWN desired-state + task dispatch (Consul retirement) -------------------
//
// These admin endpoints let the controller submit node desired-state (firewall
// rules, volume config) and tasks DOWN. Each persists to control.db + the
// changelog and wakes the matching in-process consumer via a reconcile hook
// (Config.On*): a POSTed task wakes the dispatcher; a firewall/volume write wakes
// the firewall reconciler / the scheduler.

// taskCreateRequest is the body of POST /v1/admin/tasks. The controller supplies
// the task id (the jid) so a retried POST is idempotent — CreateTask dedups on it
// and re-dispatch never happens. node identifies the owning node.
type taskCreateRequest struct {
	ID        string          `json:"id"`
	ProjectID string          `json:"project_id"`
	Name      string          `json:"name"`
	Node      string          `json:"node"`
	Volume    string          `json:"volume"`
	Archive   string          `json:"archive"`
	AuditID   int64           `json:"audit_id"`
	Params    json.RawMessage `json:"params"`
}

// taskCreateResponse is the 202 body. created=false means the id already existed
// (a duplicate/retried POST) — the row and any dispatch are untouched.
type taskCreateResponse struct {
	ID      string `json:"id"`
	Created bool   `json:"created"`
}

// handleAdminTaskCreate records a controller-submitted task (row + changelog) and
// wakes the in-process dispatcher to claim + run it. Idempotent on the task id, so
// a retried POST never re-dispatches.
func (s *Server) handleAdminTaskCreate(w http.ResponseWriter, r *http.Request, _ scope) {
	body, ok := s.readBody(w, r)
	if !ok {
		return
	}
	var req taskCreateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.ID == "" || req.Name == "" || req.Node == "" {
		writeError(w, http.StatusBadRequest, "id, name and node are required")
		return
	}
	params := req.Params
	if len(params) == 0 || string(params) == "null" {
		params = nil
	}
	created, err := s.store.CreateTask(r.Context(), store.Task{
		ID:        req.ID,
		ProjectID: req.ProjectID,
		Name:      req.Name,
		Node:      req.Node,
		Volume:    req.Volume,
		Archive:   req.Archive,
		AuditID:   req.AuditID,
		Params:    params,
	})
	if err != nil {
		s.storeError(w, err, "create task")
		return
	}
	s.fireHook(s.cfg.OnTaskCreated) // wake the dispatcher
	writeJSON(w, http.StatusAccepted, taskCreateResponse{ID: req.ID, Created: created})
}

// taskCancelResponse is the body of DELETE /v1/admin/tasks/{id}. cancelled=false
// means the task was not pending (already dispatched/terminal, or absent).
type taskCancelResponse struct {
	ID        string `json:"id"`
	Cancelled bool   `json:"cancelled"`
}

// handleAdminTaskCancel cancels a still-pending task (best-effort; a running task
// is left to finish).
func (s *Server) handleAdminTaskCancel(w http.ResponseWriter, r *http.Request, _ scope) {
	id := r.PathValue("id")
	cancelled, err := s.store.CancelPendingTask(r.Context(), id)
	if err != nil {
		s.storeError(w, err, "cancel task")
		return
	}
	writeJSON(w, http.StatusOK, taskCancelResponse{ID: id, Cancelled: cancelled})
}

// handleAdminFirewallPut stores a node's published-port NAT desired-state. The
// request body IS the firewall.NatRules JSON (an explicit empty rule set is a
// valid "zero published ports" — distinct from never having PUT, which the
// populated sentinel records). Latches the firewall-populated sentinel.
func (s *Server) handleAdminFirewallPut(w http.ResponseWriter, r *http.Request, _ scope) {
	node := r.PathValue("host")
	body, ok := s.readBody(w, r)
	if !ok {
		return
	}
	if !json.Valid(body) {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := s.store.PutFirewallRules(r.Context(), node, body); err != nil {
		s.storeError(w, err, "put firewall_rules")
		return
	}
	s.fireHook(s.cfg.OnFirewallChanged) // wake the firewall reconciler
	w.WriteHeader(http.StatusOK)
}

// handleAdminFirewallDelete clears this node's rule set (idempotent no-op if
// absent). The firewall-populated sentinel stays latched. The {host} path segment
// is a cosmetic label — this node's control.db holds only its own rules.
func (s *Server) handleAdminFirewallDelete(w http.ResponseWriter, r *http.Request, _ scope) {
	if err := s.store.DeleteFirewallRules(r.Context()); err != nil {
		s.storeError(w, err, "delete firewall_rules")
		return
	}
	s.fireHook(s.cfg.OnFirewallChanged) // wake the firewall reconciler
	w.WriteHeader(http.StatusOK)
}

// volumePeek pulls the owning node out of a volume config body so the store can
// record it as a label (cosmetic in v3.0.0 — the DB is the node scope) without
// interpreting the rest of the blob.
type volumePeek struct {
	Node string `json:"node"`
}

// handleAdminVolumePut stores a volume's desired-state. project_id + name come
// from the path; the body is the full types.Volume config JSON (carries node,
// freq, retention, …). Latches the volumes-populated sentinel.
func (s *Server) handleAdminVolumePut(w http.ResponseWriter, r *http.Request, _ scope) {
	projectID := r.PathValue("project_id")
	name := r.PathValue("name")
	body, ok := s.readBody(w, r)
	if !ok {
		return
	}
	if !json.Valid(body) {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	var peek volumePeek
	if err := json.Unmarshal(body, &peek); err != nil || peek.Node == "" {
		writeError(w, http.StatusBadRequest, "volume config must include node")
		return
	}
	if err := s.store.PutVolume(r.Context(), store.Volume{
		Name:      name,
		ProjectID: projectID,
		Node:      peek.Node,
		Config:    body,
	}); err != nil {
		s.storeError(w, err, "put volume")
		return
	}
	s.fireHook(s.cfg.OnVolumesChanged) // wake the scheduler reconciler
	w.WriteHeader(http.StatusOK)
}

// handleAdminVolumeDelete removes a volume's desired-state. It FIRST enqueues a
// volume.trash teardown task (destroy the borg repository + stop the backup
// container), then removes the row — so a DELETE can never orphan the repository
// on disk, even if the controller DELETEs directly instead of first PUTting the
// volume with trash=true. EnqueueTeardown with resetFailed=true means a re-DELETE
// after a transiently-failed teardown retries it (a still-pending/running one is
// not duplicated; a completed one is a no-op). Idempotent no-op on the row if
// already absent.
func (s *Server) handleAdminVolumeDelete(w http.ResponseWriter, r *http.Request, _ scope) {
	projectID := r.PathValue("project_id")
	name := r.PathValue("name")
	hostname, _ := os.Hostname()
	if _, err := s.store.EnqueueTeardown(r.Context(), store.Task{
		ID:        "volume.trash:" + name,
		Name:      "volume.trash",
		Node:      hostname,
		Volume:    name,
		ProjectID: projectID,
	}, true); err != nil {
		s.storeError(w, err, "enqueue volume teardown")
		return
	}
	if err := s.store.DeleteVolume(r.Context(), name, projectID); err != nil {
		s.storeError(w, err, "delete volume")
		return
	}
	s.fireHook(s.cfg.OnTaskCreated)    // wake the dispatcher to run the teardown
	s.fireHook(s.cfg.OnVolumesChanged) // wake the scheduler reconciler
	w.WriteHeader(http.StatusOK)
}

// --- helpers -----------------------------------------------------------------

// readBody reads the request body under the server's configured cap
// (MaxBodyBytes). See readBodyLimited for the semantics.
func (s *Server) readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	return s.readBodyLimited(w, r, s.cfg.MaxBodyBytes)
}

// readBodyLimited reads the request body under an explicit byte cap. On a body
// that exceeds limit, http.MaxBytesReader surfaces a *http.MaxBytesError; we
// translate that to 413 and return ok=false (the handler must stop); any other
// read error is a 400. The stored VALUE has no size cap — only this
// transport-level body limit applies. /v1/actions passes a tighter cap than the
// KV routes so a flood can't buffer large bodies into the shared control.db path.
func (s *Server) readBodyLimited(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return nil, false
		}
		writeError(w, http.StatusBadRequest, "error reading request body")
		return nil, false
	}
	return body, true
}

// storeError maps a store error onto an HTTP status:
//   - ErrProjectDeleting   → 404 (the project is gone);
//   - ErrInvalidPath /
//     ErrInvalidProjectID  → 400 (client error: empty {path...} on a PUT, or a
//     malformed {project_id} on an admin route — surfaced via the store
//     sentinels rather than duplicating validation in the handlers);
//   - anything else        → logged 500.
//
// Auth-time ErrProjectDeleting is handled in authenticate; this covers the
// per-request data path.
func (s *Server) storeError(w http.ResponseWriter, err error, op string) {
	switch {
	case errors.Is(err, store.ErrProjectDeleting):
		writeError(w, http.StatusNotFound, "project is gone")
	case errors.Is(err, store.ErrInvalidPath):
		writeError(w, http.StatusBadRequest, "invalid path")
	case errors.Is(err, store.ErrInvalidProjectID):
		writeError(w, http.StatusBadRequest, "invalid project_id")
	default:
		s.log.Error("store operation failed", "op", op, "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// writeValue writes a stored KV entry back to the client, preserving the stored
// content type when present.
func writeValue(w http.ResponseWriter, e store.KVEntry) {
	if e.ContentType != "" {
		w.Header().Set("Content-Type", e.ContentType)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(e.Value)
}

// writeError writes a small JSON error body with the given status.
func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// writeJSON writes v as a JSON body with the given status: the success-path
// analog of writeError. (The KV routes stream raw stored bytes via writeValue;
// the actions + changelog routes return structured JSON.)
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
