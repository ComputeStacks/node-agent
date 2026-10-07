package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"cs-agent/store"
)

func TestActions_CreateAccepted(t *testing.T) {
	e := newTestEnv(t)
	e.provisionTenant("proj-a", "tok-a", "active")

	resp := e.do("POST", "/v1/actions", "tok-a", []byte(`{"action_type":"cdn_purge","params":{"paths":["/x"]}}`))
	mustStatus(t, resp, http.StatusAccepted)
	var cr actionCreateResponse
	if err := json.Unmarshal(readBody(t, resp), &cr); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if cr.ID == "" || cr.Status != "pending" {
		t.Fatalf("unexpected create response: %+v", cr)
	}

	// The admin changelog pull shows the action.
	resp = e.do("GET", "/v1/admin/changelog?since=0", e.adminTok, nil)
	mustStatus(t, resp, http.StatusOK)
	var list changelogListResponse
	if err := json.Unmarshal(readBody(t, resp), &list); err != nil {
		t.Fatalf("decode changelog: %v", err)
	}
	if len(list.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(list.Entries))
	}
	ent := list.Entries[0]
	if ent.EntityType != "action_request" || ent.ProjectID != "proj-a" || ent.EntityID != cr.ID {
		t.Fatalf("entry = %+v", ent)
	}
	var snap store.ActionRequest
	if err := json.Unmarshal(ent.Payload, &snap); err != nil {
		t.Fatalf("decode payload snapshot: %v", err)
	}
	if snap.ActionType != "cdn_purge" {
		t.Fatalf("snapshot action_type = %q, want cdn_purge", snap.ActionType)
	}
}

func TestActions_ProjectIDFromTokenNotBody(t *testing.T) {
	e := newTestEnv(t)
	e.provisionTenant("proj-a", "tok-a", "active")

	// A smuggled project_id in the body must be ignored.
	resp := e.do("POST", "/v1/actions", "tok-a", []byte(`{"action_type":"cdn_purge","project_id":"proj-evil","params":{}}`))
	mustStatus(t, resp, http.StatusAccepted)

	resp = e.do("GET", "/v1/admin/changelog?since=0", e.adminTok, nil)
	var list changelogListResponse
	if err := json.Unmarshal(readBody(t, resp), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list.Entries) != 1 || list.Entries[0].ProjectID != "proj-a" {
		t.Fatalf("project_id not stamped from token: %+v", list.Entries)
	}
}

func TestActions_Validation(t *testing.T) {
	e := newTestEnv(t)
	e.provisionTenant("proj-a", "tok-a", "active")

	cases := []struct {
		name string
		body string
		want int
	}{
		{"bad json", `{not json`, http.StatusBadRequest},
		{"missing action_type", `{}`, http.StatusBadRequest},
		{"empty action_type", `{"action_type":""}`, http.StatusBadRequest},
		{"long action_type", `{"action_type":"` + strings.Repeat("x", maxActionTypeLen+1) + `"}`, http.StatusBadRequest},
		{"no params ok", `{"action_type":"cdn_purge"}`, http.StatusAccepted},
		{"explicit null params ok", `{"action_type":"cdn_purge","params":null}`, http.StatusAccepted},
		{"params just under cap ok", `{"action_type":"cdn_purge","params":"` + strings.Repeat("y", maxParamsBytes-16) + `"}`, http.StatusAccepted},
		{"params too large", `{"action_type":"cdn_purge","params":"` + strings.Repeat("y", maxParamsBytes+1) + `"}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := e.do("POST", "/v1/actions", "tok-a", []byte(tc.body))
			mustStatus(t, resp, tc.want)
		})
	}
}

func TestActions_BodyTooLarge(t *testing.T) {
	e := newTestEnv(t)
	e.provisionTenant("proj-a", "tok-a", "active")
	// Whole body over the tight actions cap → 413 (below the server's MaxBodyBytes).
	big := `{"action_type":"cdn_purge","params":"` + strings.Repeat("z", maxActionBodyBytes) + `"}`
	resp := e.do("POST", "/v1/actions", "tok-a", []byte(big))
	mustStatus(t, resp, http.StatusRequestEntityTooLarge)
}

func TestActions_ExplicitNullStoredAsNull(t *testing.T) {
	e := newTestEnv(t)
	e.provisionTenant("proj-a", "tok-a", "active")
	resp := e.do("POST", "/v1/actions", "tok-a", []byte(`{"action_type":"cdn_purge","params":null}`))
	mustStatus(t, resp, http.StatusAccepted)

	// The stored snapshot omits params (nil → SQL NULL → omitempty), same as if it
	// had been left out entirely.
	resp = e.do("GET", "/v1/admin/changelog?since=0", e.adminTok, nil)
	var list changelogListResponse
	if err := json.Unmarshal(readBody(t, resp), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(list.Entries))
	}
	var snap store.ActionRequest
	if err := json.Unmarshal(list.Entries[0].Payload, &snap); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if snap.Params != nil {
		t.Fatalf("params = %s, want nil", snap.Params)
	}
}

func TestActions_RateLimited(t *testing.T) {
	e := newTestEnv(t)
	e.provisionTenant("proj-a", "tok-a", "active")
	e.provisionTenant("proj-b", "tok-b", "active")

	got429 := false
	for i := 0; i < actionsBurst+5; i++ {
		resp := e.do("POST", "/v1/actions", "tok-a", []byte(`{"action_type":"cdn_purge"}`))
		if resp.StatusCode == http.StatusTooManyRequests {
			got429 = true
		}
		resp.Body.Close()
	}
	if !got429 {
		t.Fatalf("expected a 429 after exhausting tenant a's burst of %d", actionsBurst)
	}

	// A different tenant has its own bucket and is unaffected.
	resp := e.do("POST", "/v1/actions", "tok-b", []byte(`{"action_type":"cdn_purge"}`))
	mustStatus(t, resp, http.StatusAccepted)
}

func TestActions_CustomerScopeRequired(t *testing.T) {
	e := newTestEnv(t)
	e.provisionTenant("proj-a", "tok-a", "active")

	// Admin token on a customer route → 403 (there is no project to scope to).
	resp := e.do("POST", "/v1/actions", e.adminTok, []byte(`{"action_type":"cdn_purge"}`))
	mustStatus(t, resp, http.StatusForbidden)

	// No token → 401.
	resp = e.do("POST", "/v1/actions", "", []byte(`{"action_type":"cdn_purge"}`))
	mustStatus(t, resp, http.StatusUnauthorized)
}

func TestChangelog_AdminScopeRequired(t *testing.T) {
	e := newTestEnv(t)
	e.provisionTenant("proj-a", "tok-a", "active")

	// Customer token on the admin route → 403.
	resp := e.do("GET", "/v1/admin/changelog", "tok-a", nil)
	mustStatus(t, resp, http.StatusForbidden)

	// No token → 401.
	resp = e.do("GET", "/v1/admin/changelog", "", nil)
	mustStatus(t, resp, http.StatusUnauthorized)
}

func TestChangelog_EmptyReturnsArray(t *testing.T) {
	e := newTestEnv(t)
	resp := e.do("GET", "/v1/admin/changelog", e.adminTok, nil)
	mustStatus(t, resp, http.StatusOK)
	if body := strings.TrimSpace(string(readBody(t, resp))); body != `{"entries":[],"high_water":0}` {
		t.Fatalf("empty changelog body = %q, want {\"entries\":[],\"high_water\":0}", body)
	}
}

func TestChangelog_BadParams(t *testing.T) {
	e := newTestEnv(t)
	for _, q := range []string{"?limit=abc", "?limit=0", "?limit=-1", "?since=abc", "?since=-1"} {
		t.Run(q, func(t *testing.T) {
			resp := e.do("GET", "/v1/admin/changelog"+q, e.adminTok, nil)
			mustStatus(t, resp, http.StatusBadRequest)
		})
	}
}
