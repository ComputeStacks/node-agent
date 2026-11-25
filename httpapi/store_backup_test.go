package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"cs-agent/dbbackup"
)

type fakeStoreBackup struct{ st dbbackup.Status }

func (f fakeStoreBackup) Status() dbbackup.Status { return f.st }

func newStoreBackupEnv(t *testing.T, sb StoreBackupStatus) *testEnv {
	t.Helper()
	e := newTestEnv(t)
	e.srv = New(Config{AdminTokenHash: hashToken(adminToken), StoreBackup: sb}, e.st, nil)
	hs := httptest.NewServer(e.srv.Handler())
	t.Cleanup(hs.Close)
	e.httpsrv = hs
	return e
}

func TestStoreBackup_Status(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	e := newStoreBackupEnv(t, fakeStoreBackup{dbbackup.Status{
		Enabled: true, LastSuccessAt: &now, Uploaded: 3, DBs: 4, Lineage: "abc",
	}})
	resp := e.do("GET", "/v1/admin/store-backup", e.adminTok, nil)
	mustStatus(t, resp, http.StatusOK)
	var got dbbackup.Status
	if err := json.Unmarshal(readBody(t, resp), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || got.Uploaded != 3 || got.DBs != 4 || got.Lineage != "abc" || got.LastSuccessAt == nil {
		t.Fatalf("unexpected status: %+v", got)
	}
}

func TestStoreBackup_NilRunnerDisabled(t *testing.T) {
	e := newStoreBackupEnv(t, nil)
	resp := e.do("GET", "/v1/admin/store-backup", e.adminTok, nil)
	mustStatus(t, resp, http.StatusOK)
	var got map[string]any
	if err := json.Unmarshal(readBody(t, resp), &got); err != nil {
		t.Fatal(err)
	}
	if got["enabled"] != false {
		t.Fatalf("want enabled=false, got %v", got)
	}
}

func TestStoreBackup_RequiresAdmin(t *testing.T) {
	e := newStoreBackupEnv(t, nil)
	for _, tok := range []string{"", "wrong-token"} {
		resp := e.do("GET", "/v1/admin/store-backup", tok, nil)
		mustStatus(t, resp, http.StatusUnauthorized)
		resp.Body.Close()
	}
}
