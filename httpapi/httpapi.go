// Package httpapi is the agent's customer-metadata HTTP front door. It replaces
// the node's use of Consul as the metadata KV endpoint. It serves three
// audiences over one listener bound at node.primary_ip:8500:
//
//   - Customer containers (tenant Bearer → sha256 → store.TenantByTokenHash):
//     read/write their own /v1/db space and read the platform-managed
//     /v1/managed space. The project is ALWAYS derived from the token, never
//     from the request path — a customer token can only ever touch its own
//     project's data.
//   - The legacy monarx shim: GET /v1/kv/projects/{token}/metadata?raw=true,
//     authenticated by the Bearer (NOT the {token} path segment).
//   - The controller (admin Bearer → sha256 constant-time-equals the configured
//     admin hash): privileged cross-tenant writes to managed_kv and reads/writes
//     of any project's customer_kv, plus tenant provisioning.
//
// TENANT-ISOLATION CONTRACT (security-critical — this is now app code, not
// Consul ACLs):
//   - A request's scope (none/customer(projectID)/admin) is decided once, in
//     authenticate, from the Authorization header alone.
//   - On the customer routes the projectID comes from the resolved tenant scope;
//     the handlers never read a project id from the URL. A customer therefore
//     structurally cannot select another tenant's project.
//   - Admin scope is checked FIRST and takes precedence; a customer token that
//     hits an /v1/admin/* route is rejected (403), it does not fall through to a
//     customer interpretation of an admin route.
//   - The customer write path only ever touches customer_kv; there is no
//     customer-facing route that writes managed_kv.
package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"cs-agent/maintenance"
	"cs-agent/store"

	"github.com/hashicorp/go-hclog"
)

// Store is the subset of *store.Store the HTTP layer depends on. Narrowing it to
// an interface keeps the handlers testable and documents exactly which store
// operations the front door is allowed to perform.
type Store interface {
	TenantByTokenHash(ctx context.Context, tokenHash string) (projectID, status string, found bool, err error)
	UpsertTenant(ctx context.Context, projectID, tokenHash, status string) error
	DeleteTenant(ctx context.Context, projectID string) error
	CreateProjectDB(ctx context.Context, projectID string) error
	DeleteProjectDB(ctx context.Context, projectID string) error

	CustomerGet(ctx context.Context, projectID, path string) (store.KVEntry, bool, error)
	CustomerPut(ctx context.Context, projectID, path, contentType string, value []byte) error
	CustomerDelete(ctx context.Context, projectID, path string) error

	ManagedGet(ctx context.Context, projectID, path string) (store.KVEntry, bool, error)
	ManagedPut(ctx context.Context, projectID, path, contentType string, value []byte) error
	ManagedDelete(ctx context.Context, projectID, path string) error

	CreateActionRequest(ctx context.Context, id, projectID, actionType string, params json.RawMessage) (store.ActionRequest, error)
	ChangelogPage(ctx context.Context, since int64, entityType string, limit int, showMaintenance bool) ([]store.ChangelogEntry, int64, error)
	RecordAdvertisedServe(ctx context.Context, highWater, now int64) error
	SetChangelogAcked(ctx context.Context, seq int64) error
	GetChangelogAcked(ctx context.Context) (int64, error)

	// DOWN desired-state + task dispatch (Consul retirement). The in-process
	// consumers (dispatcher, firewall reconciler, scheduler) read these tables; the
	// DOWN handlers wake them via the Config.On* reconcile hooks.
	CreateTask(ctx context.Context, t store.Task) (created bool, err error)
	EnqueueTeardown(ctx context.Context, t store.Task, resetFailed bool) (enqueued bool, err error)
	CancelPendingTask(ctx context.Context, id string) (cancelled bool, err error)
	PutVolume(ctx context.Context, v store.Volume) error
	DeleteVolume(ctx context.Context, name, projectID string) error
	PutFirewallRules(ctx context.Context, node string, rules json.RawMessage) error
	DeleteFirewallRules(ctx context.Context) error

	// Node maintenance holds (controller side) and the status view's reads.
	maintenance.StatusStore
	PutControllerHold(ctx context.Context, reason string, gen int64) (store.MaintenanceState, error)
	ClearControllerHold(ctx context.Context, gen int64) (store.MaintenanceState, error)
	ClearAllHolds(ctx context.Context, gen, localSinceMax int64) (store.MaintenanceState, error)
}

// Config configures the metadata HTTP server. Populate from viper in main.go.
type Config struct {
	// ListenAddr is the address to bind, e.g. "10.0.0.5:8500". The provisioner
	// sets node.primary_ip:8500 (the port is baked into customer containers via
	// metadata.internal:8500, so it must be 8500).
	ListenAddr string

	// AdminTokenHash is the hex sha256 of the per-node admin Bearer token. Empty
	// disables the admin scope entirely (no admin route can authenticate) — a
	// safe default rather than an accidentally-open admin surface.
	AdminTokenHash string

	// MaxBodyBytes caps a single request body (http.MaxBytesReader → 413 on
	// exceed). There is intentionally NO cap on the stored value size beyond
	// this transport-level body limit — killing Consul's 512 KB ceiling is a
	// goal. <=0 falls back to defaultMaxBodyBytes.
	MaxBodyBytes int64

	// Reconcile hooks let the DOWN admin handlers wake the in-process consumers
	// after a successful control.db write, so a controller submission is acted on
	// promptly instead of waiting for the next backstop tick. main wires them to
	// the dispatcher / scheduler / firewall reconciler; all are optional (nil-safe)
	// and MUST be non-blocking (they are called on the request goroutine).
	OnTaskCreated     func()
	OnVolumesChanged  func()
	OnFirewallChanged func()

	// ContainerLister lists running backup containers for the maintenance status
	// view. nil uses the local docker daemon (maintenance.DockerLister).
	ContainerLister maintenance.ContainerLister
}

// fireHook invokes an optional reconcile hook if set.
func (s *Server) fireHook(hook func()) {
	if hook != nil {
		hook()
	}
}

const defaultMaxBodyBytes = 10 << 20 // 10 MiB

// scope is the authorization decision for a request, made once in authenticate
// from the Authorization header alone.
type scopeKind int

const (
	scopeNone     scopeKind = iota // no/garbage/unknown token
	scopeCustomer                  // active tenant; projectID is set
	scopeAdmin                     // matched the configured admin hash
)

type scope struct {
	kind scopeKind
	// projectID is the tenant's project for scopeCustomer. For scopeAdmin and
	// scopeNone it is empty — admin handlers take the project id from the URL
	// path explicitly; a customer handler never reads it from the URL.
	projectID string
}

// authError carries the HTTP status an auth failure should produce, so the
// distinction between 401 (who are you) and 403 (known, not allowed) is decided
// in one place.
type authError struct {
	status int
	msg    string
}

func (e *authError) Error() string { return e.msg }

// Server is the metadata HTTP front door. Build with New, run with Start, stop
// with Shutdown.
type Server struct {
	cfg     Config
	store   Store
	log     hclog.Logger
	mux     *http.ServeMux
	http    *http.Server
	limiter *rateLimiter
}

// New builds the server and wires its routes. It does not bind a socket; call
// Start (which binds + serves) afterward.
func New(cfg Config, st Store, logger hclog.Logger) *Server {
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = defaultMaxBodyBytes
	}
	if logger == nil {
		logger = hclog.NewNullLogger()
	}
	if cfg.ContainerLister == nil {
		cfg.ContainerLister = maintenance.DockerLister()
	}
	s := &Server{
		cfg:     cfg,
		store:   st,
		log:     logger,
		mux:     http.NewServeMux(),
		limiter: newRateLimiter(actionsBurst, actionsRefillPerSec),
	}
	s.routes()
	s.http = &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: s.mux,
		// Slowloris defense on a customer-reachable listener: bound how long a
		// client may dribble request headers and how long an idle keep-alive
		// connection lingers. MaxBytesReader caps body SIZE, not TIME, so these
		// are the time bounds. WriteTimeout is intentionally left unset — a large
		// (uncapped) customer value read is a legitimate slow response and must
		// not be cut off.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	return s
}

// Handler exposes the wired mux (for httptest in the same process and tests).
func (s *Server) Handler() http.Handler { return s.mux }

// Start binds ListenAddr and serves in the calling goroutine. main.go runs it in
// its own goroutine. It returns http.ErrServerClosed on a graceful Shutdown.
func (s *Server) Start() error {
	s.log.Info("metadata HTTP server listening", "addr", s.cfg.ListenAddr)
	return s.http.ListenAndServe()
}

// Shutdown gracefully drains the server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}

// routes registers every handler using Go 1.22 method+path patterns. {path...}
// is a trailing wildcard captured with r.PathValue("path").
func (s *Server) routes() {
	// --- Customer (tenant Bearer; project derived from the token) ---
	s.mux.HandleFunc("GET /v1/db/{path...}", s.requireCustomer(s.handleCustomerDBGet))
	s.mux.HandleFunc("PUT /v1/db/{path...}", s.requireCustomer(s.handleCustomerDBPut))
	s.mux.HandleFunc("DELETE /v1/db/{path...}", s.requireCustomer(s.handleCustomerDBDelete))
	s.mux.HandleFunc("GET /v1/managed/{path...}", s.requireCustomer(s.handleCustomerManagedGet))

	// --- Container actions (tenant Bearer; project stamped from the token) ---
	s.mux.HandleFunc("POST /v1/actions", s.requireCustomer(s.handleActionCreate))

	// --- Legacy monarx shim; identity is the Bearer, not {token} ---
	s.mux.HandleFunc("GET /v1/kv/projects/{token}/metadata", s.requireCustomer(s.handleShimMetadata))

	// --- Admin (per-node admin Bearer; project_id explicit in the path) ---
	s.mux.HandleFunc("PUT /v1/admin/projects/{project_id}/managed/{path...}", s.requireAdmin(s.handleAdminManagedPut))
	s.mux.HandleFunc("DELETE /v1/admin/projects/{project_id}/managed/{path...}", s.requireAdmin(s.handleAdminManagedDelete))
	s.mux.HandleFunc("GET /v1/admin/projects/{project_id}/db/{path...}", s.requireAdmin(s.handleAdminDBGet))
	s.mux.HandleFunc("PUT /v1/admin/projects/{project_id}/db/{path...}", s.requireAdmin(s.handleAdminDBPut))
	s.mux.HandleFunc("DELETE /v1/admin/projects/{project_id}/db/{path...}", s.requireAdmin(s.handleAdminDBDelete))

	s.mux.HandleFunc("PUT /v1/admin/tenants/{project_id}", s.requireAdmin(s.handleAdminTenantPut))
	s.mux.HandleFunc("DELETE /v1/admin/tenants/{project_id}", s.requireAdmin(s.handleAdminTenantDelete))

	// --- Controller pull channel for the changelog (per-node admin Bearer) ---
	s.mux.HandleFunc("GET /v1/admin/changelog", s.requireAdmin(s.handleAdminChangelogList))
	s.mux.HandleFunc("POST /v1/admin/changelog/ack", s.requireAdmin(s.handleAdminChangelogAck))

	// --- Controller DOWN desired-state + task dispatch (per-node admin Bearer) ---
	// Each persists to control.db + the changelog and wakes the matching in-process
	// consumer (dispatcher / firewall reconciler / scheduler) via a reconcile hook.
	s.mux.HandleFunc("POST /v1/admin/tasks", s.requireAdmin(s.handleAdminTaskCreate))
	s.mux.HandleFunc("DELETE /v1/admin/tasks/{id}", s.requireAdmin(s.handleAdminTaskCancel))
	s.mux.HandleFunc("PUT /v1/admin/nodes/{host}/firewall_rules", s.requireAdmin(s.handleAdminFirewallPut))
	s.mux.HandleFunc("DELETE /v1/admin/nodes/{host}/firewall_rules", s.requireAdmin(s.handleAdminFirewallDelete))
	s.mux.HandleFunc("PUT /v1/admin/projects/{project_id}/volumes/{name}", s.requireAdmin(s.handleAdminVolumePut))
	s.mux.HandleFunc("DELETE /v1/admin/projects/{project_id}/volumes/{name}", s.requireAdmin(s.handleAdminVolumeDelete))

	// --- Node maintenance hold (per-node admin Bearer) ---
	s.mux.HandleFunc("GET /v1/admin/maintenance", s.requireAdmin(s.handleAdminMaintenanceGet))
	s.mux.HandleFunc("PUT /v1/admin/maintenance", s.requireAdmin(s.handleAdminMaintenancePut))
	s.mux.HandleFunc("DELETE /v1/admin/maintenance", s.requireAdmin(s.handleAdminMaintenanceDelete))
}

// authenticate decides the request scope from the Authorization header ALONE.
// This is the single tenant-isolation gate; get it exactly right:
//
//  1. Parse the Bearer token. No/garbage header → scopeNone (401).
//  2. ADMIN FIRST, and it takes precedence: constant-time-compare sha256(token)
//     against the configured admin hash. A match → scopeAdmin regardless of any
//     tenant row, so an admin token is never reinterpreted as a customer.
//  3. Otherwise tenant: sha256(token) → store.TenantByTokenHash.
//     - found & status=="active"      → scopeCustomer(projectID)
//     - found & suspended/trashed/etc → 403 (known principal, not allowed)
//     - not found                     → 401 (unknown principal)
//
// A nil error means the returned scope is authoritative.
func (s *Server) authenticate(ctx context.Context, r *http.Request) (scope, *authError) {
	token, ok := bearerToken(r)
	if !ok || token == "" {
		return scope{}, &authError{status: http.StatusUnauthorized, msg: "missing or malformed Authorization Bearer token"}
	}

	sum := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(sum[:])

	// (2) Admin first — precedence over any tenant interpretation.
	if s.cfg.AdminTokenHash != "" &&
		subtle.ConstantTimeCompare([]byte(tokenHash), []byte(s.cfg.AdminTokenHash)) == 1 {
		return scope{kind: scopeAdmin}, nil
	}

	// (3) Tenant.
	projectID, status, found, err := s.store.TenantByTokenHash(ctx, tokenHash)
	if err != nil {
		// Treat a project mid-delete as gone; everything else is a 500.
		if errors.Is(err, store.ErrProjectDeleting) {
			return scope{}, &authError{status: http.StatusNotFound, msg: "project is gone"}
		}
		s.log.Error("tenant lookup failed", "error", err)
		return scope{}, &authError{status: http.StatusInternalServerError, msg: "auth backend error"}
	}
	if !found {
		// A project is served exactly when its tenant row exists (provisioned via
		// UpsertTenant); an unknown token is terminal.
		return scope{}, &authError{status: http.StatusUnauthorized, msg: "unknown token"}
	}
	if status != "active" {
		// Known principal, but suspended/trashed → forbidden, not unauthorized.
		return scope{}, &authError{status: http.StatusForbidden, msg: "tenant is not active"}
	}
	return scope{kind: scopeCustomer, projectID: projectID}, nil
}

// requireCustomer wraps a handler so it only runs for an active tenant. Admin
// scope is allowed too ONLY where it makes sense; here we deliberately require a
// CUSTOMER scope for the /v1/db, /v1/managed, and shim routes — those are the
// customer's own data, addressed by the token. (Admins use the explicit
// /v1/admin/* cross-tenant routes instead.) The resolved scope (and thus the
// projectID) is passed in, never re-derived from the URL.
func (s *Server) requireCustomer(h func(http.ResponseWriter, *http.Request, scope)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sc, aerr := s.authenticate(r.Context(), r)
		if aerr != nil {
			writeError(w, aerr.status, aerr.msg)
			return
		}
		if sc.kind != scopeCustomer {
			// An admin token on a customer route: there is no project to scope
			// to (the URL carries none), so reject rather than guess.
			writeError(w, http.StatusForbidden, "this route requires a customer (tenant) token")
			return
		}
		h(w, r, sc)
	}
}

// requireAdmin wraps a handler so it only runs for the admin scope. Admin
// precedence is enforced in authenticate (admin is matched before tenant), so a
// customer token reaching here resolves to scopeCustomer and is rejected 403 —
// it never falls through to a customer interpretation of an admin route.
func (s *Server) requireAdmin(h func(http.ResponseWriter, *http.Request, scope)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sc, aerr := s.authenticate(r.Context(), r)
		if aerr != nil {
			writeError(w, aerr.status, aerr.msg)
			return
		}
		if sc.kind != scopeAdmin {
			writeError(w, http.StatusForbidden, "admin token required")
			return
		}
		h(w, r, sc)
	}
}

// bearerToken extracts the token from an "Authorization: Bearer <token>" header.
// The scheme match is case-insensitive per RFC 7235; the token is returned
// verbatim (trimmed of surrounding spaces).
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", false
	}
	const prefix = "bearer "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(h[len(prefix):]), true
}
