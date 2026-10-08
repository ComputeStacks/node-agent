package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"cs-agent/maintenance"
	"cs-agent/store"
)

// fakeLister is a ContainerLister that returns a fixed result.
type fakeLister struct {
	out []maintenance.InFlight
	err error
}

func (f *fakeLister) RunningBackupContainers(context.Context) ([]maintenance.InFlight, error) {
	return f.out, f.err
}

// newMaintEnv is newTestEnv with a fake container lister and a counter on the
// dispatcher wake hook.
func newMaintEnv(t *testing.T, cl maintenance.ContainerLister) (*testEnv, *atomic.Int32) {
	t.Helper()
	e := newTestEnv(t)
	var wakes atomic.Int32
	e.srv = New(Config{
		AdminTokenHash:  hashToken(adminToken),
		OnTaskCreated:   func() { wakes.Add(1) },
		ContainerLister: cl,
	}, e.st, nil)
	hs := httptest.NewServer(e.srv.Handler())
	t.Cleanup(hs.Close)
	e.httpsrv = hs
	return e, &wakes
}

func decodeStatus(t *testing.T, resp *http.Response) maintenance.Status {
	t.Helper()
	var s maintenance.Status
	if err := json.Unmarshal(readBody(t, resp), &s); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	return s
}

func TestMaintenance_RequiresAdmin(t *testing.T) {
	e, _ := newMaintEnv(t, &fakeLister{})
	e.provisionTenant("p1", "cust-tok", "active")
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/admin/maintenance"},
		{"PUT", "/v1/admin/maintenance"},
		{"DELETE", "/v1/admin/maintenance?gen=1"},
	} {
		resp := e.do(c.method, c.path, "", []byte(`{"reason":"x","gen":1}`))
		mustStatus(t, resp, http.StatusUnauthorized)
		resp.Body.Close()
		resp = e.do(c.method, c.path, "cust-tok", []byte(`{"reason":"x","gen":1}`))
		mustStatus(t, resp, http.StatusForbidden)
		resp.Body.Close()
	}
}

func TestMaintenance_GetDefault(t *testing.T) {
	e, _ := newMaintEnv(t, &fakeLister{})
	resp := e.do("GET", "/v1/admin/maintenance", e.adminTok, nil)
	mustStatus(t, resp, http.StatusOK)
	b := readBody(t, resp)
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"paused", "controller", "local", "controller_gen", "running", "pending", "skipped_backups", "seq", "quiesce", "sample", "instance_id"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("GET status missing %q: %s", k, b)
		}
	}
	if raw["paused"] != false || raw["quiesce"] != "quiesced" || raw["instance_id"] == "" {
		t.Fatalf("unexpected default status: %s", b)
	}
}

func TestMaintenance_PutGetDelete(t *testing.T) {
	e, wakes := newMaintEnv(t, &fakeLister{out: []maintenance.InFlight{{ID: "c1", Kind: maintenance.KindBorgContainer}}})

	resp := e.do("PUT", "/v1/admin/maintenance", e.adminTok, []byte(`{"reason":"kernel update","gen":3}`))
	mustStatus(t, resp, http.StatusOK)
	s := decodeStatus(t, resp)
	if !s.Paused || s.Controller == nil || s.Controller.Reason != "kernel update" || s.ControllerGen != 3 {
		t.Fatalf("PUT status = %+v", s)
	}
	if s.Quiesce != maintenance.QuiesceBusy || len(s.Running) != 1 || s.Seq == 0 {
		t.Fatalf("PUT status in-flight = %+v", s)
	}
	if wakes.Load() != 1 {
		t.Fatalf("wake hook fired %d times, want 1", wakes.Load())
	}

	resp = e.do("GET", "/v1/admin/maintenance", e.adminTok, nil)
	mustStatus(t, resp, http.StatusOK)
	if g := decodeStatus(t, resp); !g.Paused || g.Controller == nil || g.ControllerGen != 3 {
		t.Fatalf("GET status = %+v", g)
	}

	// A local hold keeps the node paused after the controller clears its own.
	if _, _, err := e.st.PutLocalHold(ctxBG, "disk swap", "root"); err != nil {
		t.Fatal(err)
	}
	resp = e.do("DELETE", "/v1/admin/maintenance?gen=4", e.adminTok, nil)
	mustStatus(t, resp, http.StatusOK)
	s = decodeStatus(t, resp)
	if !s.Paused || s.Controller != nil || s.Local == nil || s.ControllerGen != 4 {
		t.Fatalf("DELETE status = %+v", s)
	}
	if wakes.Load() != 2 {
		t.Fatalf("wake hook fired %d times, want 2", wakes.Load())
	}
}

func TestMaintenance_PutTrimsReason(t *testing.T) {
	e, _ := newMaintEnv(t, &fakeLister{})
	resp := e.do("PUT", "/v1/admin/maintenance", e.adminTok, []byte(`{"reason":"  kernel update \n","gen":1}`))
	mustStatus(t, resp, http.StatusOK)
	if s := decodeStatus(t, resp); s.Controller == nil || s.Controller.Reason != "kernel update" {
		t.Fatalf("PUT status controller = %+v", s.Controller)
	}
	m, err := e.st.GetMaintenance(ctxBG)
	if err != nil {
		t.Fatal(err)
	}
	if m.Controller == nil || m.Controller.Reason != "kernel update" {
		t.Fatalf("stored controller hold = %+v", m.Controller)
	}
}

func TestMaintenance_DeleteAll(t *testing.T) {
	e, _ := newMaintEnv(t, &fakeLister{})
	if _, err := e.st.PutControllerHold(ctxBG, "r", 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.st.PutLocalHold(ctxBG, "l", "root"); err != nil {
		t.Fatal(err)
	}
	resp := e.do("DELETE", "/v1/admin/maintenance?gen=1&all=1", e.adminTok, nil)
	mustStatus(t, resp, http.StatusOK)
	s := decodeStatus(t, resp)
	if s.Paused || s.Controller != nil || s.Local != nil {
		t.Fatalf("all=1 status = %+v", s)
	}
}

func TestMaintenance_DeleteAllConditional(t *testing.T) {
	e, wakes := newMaintEnv(t, &fakeLister{})
	if _, err := e.st.PutControllerHold(ctxBG, "r", 1); err != nil {
		t.Fatal(err)
	}
	m, _, err := e.st.PutLocalHold(ctxBG, "l", "root")
	if err != nil {
		t.Fatal(err)
	}
	before := wakes.Load()

	resp := e.do("DELETE", "/v1/admin/maintenance?gen=1&all=1&local_since_max="+strconv.FormatInt(m.Local.Since-1, 10), e.adminTok, nil)
	mustStatus(t, resp, http.StatusPreconditionFailed)
	if s := decodeStatus(t, resp); !s.Paused || s.Local == nil || s.Controller == nil {
		t.Fatalf("412 body = %+v", s)
	}
	if wakes.Load() != before {
		t.Fatalf("wake hook fired on 412")
	}

	resp = e.do("DELETE", "/v1/admin/maintenance?gen=1&local_since_max=5", e.adminTok, nil)
	mustStatus(t, resp, http.StatusBadRequest)
	resp = e.do("DELETE", "/v1/admin/maintenance?gen=1&all=1&local_since_max=x", e.adminTok, nil)
	mustStatus(t, resp, http.StatusBadRequest)

	resp = e.do("DELETE", "/v1/admin/maintenance?gen=1&all=1&local_since_max="+strconv.FormatInt(m.Local.Since, 10), e.adminTok, nil)
	mustStatus(t, resp, http.StatusOK)
	if s := decodeStatus(t, resp); s.Paused {
		t.Fatalf("conditional all=1 status = %+v", s)
	}
}

func TestMaintenance_StaleGen409(t *testing.T) {
	e, wakes := newMaintEnv(t, &fakeLister{})
	if _, err := e.st.PutControllerHold(ctxBG, "newer", 5); err != nil {
		t.Fatal(err)
	}
	resp := e.do("PUT", "/v1/admin/maintenance", e.adminTok, []byte(`{"reason":"older","gen":4}`))
	mustStatus(t, resp, http.StatusConflict)
	s := decodeStatus(t, resp)
	if s.ControllerGen != 5 || s.Controller == nil || s.Controller.Reason != "newer" || s.InstanceID == "" {
		t.Fatalf("409 PUT body = %+v", s)
	}

	resp = e.do("DELETE", "/v1/admin/maintenance?gen=4", e.adminTok, nil)
	mustStatus(t, resp, http.StatusConflict)
	if s := decodeStatus(t, resp); s.ControllerGen != 5 || !s.Paused {
		t.Fatalf("409 DELETE body = %+v", s)
	}
	if wakes.Load() != 0 {
		t.Fatalf("wake hook fired on a 409")
	}
}

func TestMaintenance_Validation(t *testing.T) {
	e, _ := newMaintEnv(t, &fakeLister{})
	long := strings.Repeat("x", 513)
	for _, body := range []string{
		`not json`,
		`{"reason":"r"}`,
		`{"reason":"r","gen":-1}`,
		`{"gen":1}`,
		`{"reason":"  ","gen":1}`,
		`{"reason":"` + long + `","gen":1}`,
	} {
		resp := e.do("PUT", "/v1/admin/maintenance", e.adminTok, []byte(body))
		mustStatus(t, resp, http.StatusBadRequest)
		resp.Body.Close()
	}
	resp := e.do("PUT", "/v1/admin/maintenance", e.adminTok, []byte(`{"reason":"`+strings.Repeat("x", 512)+`","gen":0}`))
	mustStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	for _, q := range []string{"", "?gen=", "?gen=x", "?gen=-1", "?gen=1&all=maybe"} {
		resp := e.do("DELETE", "/v1/admin/maintenance"+q, e.adminTok, nil)
		mustStatus(t, resp, http.StatusBadRequest)
		resp.Body.Close()
	}
}

func TestMaintenance_UnknownQuiesce(t *testing.T) {
	e, _ := newMaintEnv(t, &fakeLister{err: errors.New("docker down")})
	resp := e.do("GET", "/v1/admin/maintenance", e.adminTok, nil)
	mustStatus(t, resp, http.StatusOK)
	if s := decodeStatus(t, resp); s.Quiesce != maintenance.QuiesceUnknown {
		t.Fatalf("quiesce = %q, want unknown", s.Quiesce)
	}
}

// changelogPage is the decoded GET /v1/admin/changelog body.
type changelogPage struct {
	Entries   []store.ChangelogEntry `json:"entries"`
	HighWater int64                  `json:"high_water"`
}

func (e *testEnv) changelog(path, types string) changelogPage {
	e.t.Helper()
	req, err := http.NewRequest("GET", e.httpsrv.URL+path, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+e.adminTok)
	if types != "" {
		req.Header.Set("X-CS-Changelog-Types", types)
	}
	resp, err := e.httpsrv.Client().Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	mustStatus(e.t, resp, http.StatusOK)
	var p changelogPage
	if err := json.Unmarshal(readBody(e.t, resp), &p); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func TestChangelog_MaintenanceHeaderGating(t *testing.T) {
	e, _ := newMaintEnv(t, &fakeLister{})
	if _, err := e.st.CreateTask(ctxBG, store.Task{ID: "t1", Name: "volume.backup", Node: "n"}); err != nil {
		t.Fatal(err)
	}
	m, seq, err := e.st.PutLocalHold(ctxBG, "work", "root")
	if err != nil {
		t.Fatal(err)
	}
	if m.Seq != seq || seq == 0 {
		t.Fatalf("PutLocalHold seq = %d, state seq %d", seq, m.Seq)
	}

	// Not advertised: the maintenance row is hidden but high_water passes it,
	// and nothing is recorded as advertised.
	p := e.changelog("/v1/admin/changelog", "")
	for _, en := range p.Entries {
		if en.EntityType == store.EntityNodeMaintenance {
			t.Fatalf("node_maintenance served without the header")
		}
	}
	if p.HighWater != seq {
		t.Fatalf("high_water = %d, want %d", p.HighWater, seq)
	}
	if hw, at, err := e.st.GetAdvertised(ctxBG); err != nil || hw != 0 || at != 0 {
		t.Fatalf("advertised = %d/%d/%v, want none", hw, at, err)
	}

	// A filtered page with the header proves nothing about maintenance rows.
	e.changelog("/v1/admin/changelog?entity_type=task", "node_maintenance")
	if hw, _, _ := e.st.GetAdvertised(ctxBG); hw != 0 {
		t.Fatalf("filtered page recorded advertised high water %d", hw)
	}

	// Advertised (among other types, with spaces): row served, serve recorded.
	p = e.changelog("/v1/admin/changelog", "foo, node_maintenance")
	var found bool
	for _, en := range p.Entries {
		if en.EntityType == store.EntityNodeMaintenance && en.Seq == seq {
			found = true
		}
	}
	if !found || p.HighWater != seq {
		t.Fatalf("advertised page = %+v, want node_maintenance seq %d", p, seq)
	}
	if hw, at, err := e.st.GetAdvertised(ctxBG); err != nil || hw != seq || at == 0 {
		t.Fatalf("advertised = %d/%d/%v, want %d/now", hw, at, err, seq)
	}

	// high_water is bounded by limit: rows past the limit are not passed.
	p = e.changelog("/v1/admin/changelog?limit=1", "node_maintenance")
	if len(p.Entries) != 1 || p.HighWater != p.Entries[0].Seq || p.HighWater >= seq {
		t.Fatalf("limit=1 page = %+v", p)
	}
	// The advertised high water is monotonic: the smaller page did not lower it.
	if hw, _, _ := e.st.GetAdvertised(ctxBG); hw != seq {
		t.Fatalf("advertised high water dropped to %d", hw)
	}
}
