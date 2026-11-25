//go:build e2e

// End-to-end test against a real S3-compatible server. Run with:
//
//	CS_AGENT_E2E_S3_ENDPOINT=http://127.0.0.1:18333 CS_AGENT_E2E_S3_BUCKET=e2e \
//	  go test -tags e2e -run TestE2E ./dbbackup/
package dbbackup

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"cs-agent/s3upload"
	"cs-agent/store"

	"filippo.io/age"
)

func TestE2EBackupAndNodeRestore(t *testing.T) {
	endpoint := os.Getenv("CS_AGENT_E2E_S3_ENDPOINT")
	bucket := os.Getenv("CS_AGENT_E2E_S3_BUCKET")
	if endpoint == "" || bucket == "" {
		t.Skip("CS_AGENT_E2E_S3_ENDPOINT / CS_AGENT_E2E_S3_BUCKET not set")
	}
	ctx := context.Background()
	up, err := s3upload.New(s3upload.Config{
		Endpoint: endpoint, Region: "us-east-1", Bucket: bucket,
		Prefix:    "sqlite-" + time.Now().UTC().Format("150405.000") + "/",
		AccessKey: envOr("CS_AGENT_E2E_S3_ACCESS_KEY", "e2e"), SecretKey: envOr("CS_AGENT_E2E_S3_SECRET_KEY", "e2e"),
		ForcePathStyle: true, PartSizeMB: 5, Concurrency: 2,
	})
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	st, err := store.Open(dir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"p1", "p2"} {
		must(t, st.UpsertTenant(ctx, p, "hash-"+p, "active"))
		must(t, st.CreateProjectDB(ctx, p))
		must(t, st.CustomerPut(ctx, p, "k", "text/plain", []byte("v1-"+p)))
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewRunner(Config{
		Enabled: true, Recipients: []age.Recipient{id.Recipient()}, NodeName: "e2e-node",
		KeepAll: 48 * time.Hour, RetentionDays: 30,
		IsProjectGone: func(err error) bool { return errors.Is(err, store.ErrProjectNotFound) },
	}, st, up)
	if err != nil {
		t.Fatal(err)
	}
	run := func(label string) Status {
		if err := r.RunOnce(ctx); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		s := r.Status()
		t.Logf("%s: uploaded=%d skipped=%d failed=%d dbs=%d", label, s.Uploaded, s.Skipped, s.Failed, s.DBs)
		return s
	}
	if s := run("first"); s.Uploaded != 3 {
		t.Fatalf("first run uploaded %d, want 3", s.Uploaded)
	}
	if s := run("second (no writes)"); s.Uploaded > 1 {
		t.Fatalf("second run uploaded %d project DBs; skip-unchanged is not working", s.Uploaded)
	}
	must(t, st.CustomerPut(ctx, "p1", "k", "text/plain", []byte("v2-p1")))
	run("third (p1 changed)")
	must(t, st.DeleteTenant(ctx, "p2"))
	must(t, st.DeleteProjectDB(ctx, "p2"))
	run("fourth (p2 deleted)")
	oldID, _ := st.InstanceID(ctx)
	must(t, st.Close())

	objs, err := up.List(ctx, "nodes/e2e-node/")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d objects in S3", len(objs))

	// Restore onto an empty disk.
	dir2 := t.TempDir()
	res, err := Restore(ctx, up, RestoreOptions{Mode: ModeNode, DataDir: dir2, Node: "e2e-node", Identities: []age.Identity{id}})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("installed: %v", res.Installed)
	st2, err := store.Open(dir2, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	newID, _ := st2.InstanceID(ctx)
	if newID == "" || newID == oldID {
		t.Fatalf("instance_id not rotated: old %s new %s", oldID, newID)
	}
	e, found, err := st2.CustomerGet(ctx, "p1", "k")
	if err != nil || !found || string(e.Value) != "v2-p1" {
		t.Fatalf("p1 after restore: %q found=%v err=%v", e.Value, found, err)
	}
	if _, found, _ := st2.TenantByProjectID(ctx, "p2"); found {
		t.Fatal("deleted project p2 came back as a tenant")
	}
	if _, err := os.Stat(store.ProjectPath(dir2, "p2")); !os.IsNotExist(err) {
		t.Fatal("deleted project p2's DB came back")
	}

	// Real-store delete (retention path).
	var keys []string
	for _, o := range objs {
		keys = append(keys, o.Key)
	}
	must(t, up.Delete(ctx, keys))
	left, err := up.List(ctx, "nodes/e2e-node/")
	if err != nil || len(left) != 0 {
		t.Fatalf("after delete: %d objects left, err %v", len(left), err)
	}
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
