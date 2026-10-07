package main

import (
	"bytes"
	"context"
	"cs-agent/maintenance"
	"cs-agent/store"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// cliLister is a fake ContainerLister whose result the test can change.
type cliLister struct {
	mu  sync.Mutex
	out []maintenance.InFlight
}

func (l *cliLister) RunningBackupContainers(context.Context) ([]maintenance.InFlight, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]maintenance.InFlight(nil), l.out...), nil
}

func (l *cliLister) set(out []maintenance.InFlight) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.out = out
}

// cliEnv is one CLI test fixture: a control.db created by store.Open (then
// closed), an agent.yml pointing at it, a fake lister and a fake clock whose
// sleeps advance time instantly and run onSleep first.
type cliEnv struct {
	dataDir string
	lister  *cliLister
	now     time.Time
	onSleep func(d time.Duration)
	sleeps  int
}

func newCLIEnv(t *testing.T) *cliEnv {
	t.Helper()
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	st, err := store.Open(dataDir, store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "agent.yml")
	if err := os.WriteFile(cfg, []byte("store:\n  data_dir: "+dataDir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CS_AGENT_CONFIG", cfg)

	e := &cliEnv{dataDir: dataDir, lister: &cliLister{}, now: time.Unix(1_800_000_000, 0)}
	origLister, origActive, origNow, origSleep := maintLister, maintAgentActive, maintNow, maintSleep
	maintLister = func() maintenance.ContainerLister { return e.lister }
	maintAgentActive = func(context.Context) bool { return true }
	maintNow = func() time.Time { return e.now }
	maintSleep = func(ctx context.Context, d time.Duration) error {
		e.sleeps++
		if e.onSleep != nil {
			e.onSleep(d)
		}
		e.now = e.now.Add(d)
		return ctx.Err()
	}
	t.Cleanup(func() {
		maintLister, maintAgentActive, maintNow, maintSleep = origLister, origActive, origNow, origSleep
	})
	return e
}

// side opens a second control.db handle, as the agent would hold beside the CLI.
func (e *cliEnv) side(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.OpenExistingControl(e.dataDir)
	if err != nil {
		t.Fatalf("OpenExistingControl: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func runCLI(args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = maintenanceCLI(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

func decodeOne(t *testing.T, stdout string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(stdout))
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		t.Fatalf("decode stdout %q: %v", stdout, err)
	}
	if dec.More() {
		t.Fatalf("stdout holds more than one JSON value: %q", stdout)
	}
	return obj
}

func TestMaintenanceCLI_Usage(t *testing.T) {
	newCLIEnv(t)
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"no command", nil, exitUsage},
		{"unknown command", []string{"frobnicate"}, exitUsage},
		{"help", []string{"help"}, exitOK},
		{"flag help", []string{"status", "-h"}, exitOK},
		{"unknown flag", []string{"status", "--bogus"}, exitUsage},
		{"stray argument", []string{"off", "extra"}, exitUsage},
		{"on without reason", []string{"on"}, exitUsage},
		{"on blank reason", []string{"on", "--reason", "  "}, exitUsage},
		{"on long reason", []string{"on", "--reason", strings.Repeat("x", maxReasonBytes+1)}, exitUsage},
		{"wait without timeout", []string{"on", "--reason", "r", "--wait"}, exitUsage},
		{"timeout without wait", []string{"on", "--reason", "r", "--timeout", "1m"}, exitUsage},
		{"no-controller without wait", []string{"on", "--reason", "r", "--no-controller"}, exitUsage},
		{"negative settle", []string{"on", "--reason", "r", "--wait", "--timeout", "1m", "--settle", "-1s"}, exitUsage},
		{"reason on off", []string{"off", "--reason", "r"}, exitUsage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := runCLI(tc.args...)
			if code != tc.want {
				t.Fatalf("exit = %d, want %d (stderr %q)", code, tc.want, stderr)
			}
			if tc.want == exitUsage && stdout != "" {
				t.Fatalf("usage error wrote stdout: %q", stdout)
			}
		})
	}
}

func TestMaintenanceCLI_OnOffStatus(t *testing.T) {
	e := newCLIEnv(t)
	t.Setenv("SUDO_USER", "alice")

	code, stdout, stderr := runCLI("on", "--reason", "kernel update", "--json")
	if code != exitOK {
		t.Fatalf("on exit = %d, stderr %q", code, stderr)
	}
	if want := filepath.Join(e.dataDir, "control.db"); !strings.Contains(stderr, want) {
		t.Fatalf("stderr %q does not name %s", stderr, want)
	}
	obj := decodeOne(t, stdout)
	local, _ := obj["local"].(map[string]any)
	if obj["paused"] != true || local == nil || local["reason"] != "kernel update" || local["by"] != "alice" {
		t.Fatalf("on output = %v", obj)
	}

	m, err := e.side(t).GetMaintenance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m.Local == nil || m.Local.By != "alice" || !m.Paused() {
		t.Fatalf("stored state after on = %+v", m)
	}

	code, stdout, _ = runCLI("status")
	if code != exitOK || !strings.Contains(stdout, `"kernel update"`) || !strings.Contains(stdout, "paused:           yes") {
		t.Fatalf("status exit %d, stdout %q", code, stdout)
	}

	code, stdout, _ = runCLI("off", "--json")
	if code != exitOK {
		t.Fatalf("off exit = %d", code)
	}
	obj = decodeOne(t, stdout)
	if obj["paused"] != false || obj["local"] != nil {
		t.Fatalf("off output = %v", obj)
	}
}

func TestMaintenanceCLI_OffKeepsControllerHold(t *testing.T) {
	e := newCLIEnv(t)
	if _, err := e.side(t).PutControllerHold(context.Background(), "migration", 1); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLI("off", "--json")
	if code != exitOK {
		t.Fatalf("off exit = %d", code)
	}
	if obj := decodeOne(t, stdout); obj["paused"] != true || obj["controller"] == nil {
		t.Fatalf("off output = %v", obj)
	}
	if !strings.Contains(stderr, "controller hold") {
		t.Fatalf("stderr %q does not mention the remaining controller hold", stderr)
	}
}

func TestMaintenanceCLI_MissingDB(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "agent.yml")
	dataDir := filepath.Join(dir, "nope")
	if err := os.WriteFile(cfg, []byte("store:\n  data_dir: "+dataDir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CS_AGENT_CONFIG", cfg)
	for _, args := range [][]string{{"status"}, {"off"}, {"on", "--reason", "r"}} {
		if code, _, stderr := runCLI(args...); code != exitControlDB {
			t.Fatalf("%v exit = %d, want %d (stderr %q)", args, code, exitControlDB, stderr)
		}
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Fatalf("data dir was created: %v", err)
	}
}

func TestMaintenanceCLI_WaitFailsFastWhenNeverAdvertised(t *testing.T) {
	e := newCLIEnv(t)
	code, stdout, stderr := runCLI("on", "--reason", "r", "--wait", "--timeout", "10m", "--json")
	if code != exitNoController {
		t.Fatalf("exit = %d, want %d (stderr %q)", code, exitNoController, stderr)
	}
	if e.sleeps != 0 {
		t.Fatalf("slept %d times before failing fast", e.sleeps)
	}
	if !strings.Contains(stderr, "has not polled with maintenance support") {
		t.Fatalf("stderr %q", stderr)
	}
	if obj := decodeOne(t, stdout); obj["local"] == nil {
		t.Fatalf("local hold not kept: %v", obj)
	}
}

func TestMaintenanceCLI_WaitFailsFastWhenAdvertisedLongAgo(t *testing.T) {
	e := newCLIEnv(t)
	if err := e.side(t).RecordAdvertisedServe(context.Background(), 0, e.now.Add(-121*time.Second).Unix()); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runCLI("on", "--reason", "r", "--wait", "--timeout", "10m"); code != exitNoController {
		t.Fatalf("exit = %d, want %d (stderr %q)", code, exitNoController, stderr)
	}
}

func TestMaintenanceCLI_WaitNoController(t *testing.T) {
	e := newCLIEnv(t)
	e.lister.set([]maintenance.InFlight{{ID: "c1", Kind: maintenance.KindBorgContainer, Volume: "vol1", StartedAt: 1}})
	e.onSleep = func(time.Duration) {
		if e.sleeps == 2 {
			e.lister.set(nil)
		}
	}
	code, stdout, stderr := runCLI("on", "--reason", "r", "--wait", "--timeout", "1m", "--settle", "5s", "--no-controller", "--json")
	if code != exitOK {
		t.Fatalf("exit = %d, stderr %q", code, stderr)
	}
	obj := decodeOne(t, stdout)
	if obj["quiesce"] != maintenance.QuiesceQuiesced || obj["controller_acked"] != false {
		t.Fatalf("output = %v", obj)
	}
	if !strings.Contains(stderr, "borg.container vol1") || !strings.Contains(stderr, "settling") {
		t.Fatalf("stderr %q lacks progress", stderr)
	}
	// Two polls while busy, then the settle sleep.
	if e.sleeps != 3 {
		t.Fatalf("sleeps = %d, want 3", e.sleeps)
	}
}

func TestMaintenanceCLI_WaitSettleRechecks(t *testing.T) {
	e := newCLIEnv(t)
	// Work appears during the settle sleep and is gone a poll later.
	e.onSleep = func(d time.Duration) {
		switch e.sleeps {
		case 1:
			e.lister.set([]maintenance.InFlight{{ID: "c1", Kind: maintenance.KindBorgContainer}})
		case 2:
			e.lister.set(nil)
		}
	}
	code, _, stderr := runCLI("on", "--reason", "r", "--wait", "--timeout", "1m", "--no-controller")
	if code != exitOK {
		t.Fatalf("exit = %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stderr, "during the settle period") || e.sleeps != 3 {
		t.Fatalf("sleeps = %d, stderr %q", e.sleeps, stderr)
	}
}

func TestMaintenanceCLI_WaitControllerAck(t *testing.T) {
	e := newCLIEnv(t)
	side := e.side(t)
	ctx := context.Background()
	if err := side.RecordAdvertisedServe(ctx, 0, e.now.Unix()); err != nil {
		t.Fatal(err)
	}
	// The controller keeps polling; on the second poll it has seen and acked
	// the CLI's entry.
	e.onSleep = func(time.Duration) {
		hw := int64(0)
		if e.sleeps >= 2 {
			m, err := side.GetMaintenance(ctx)
			if err != nil {
				t.Error(err)
			}
			hw = m.Seq
			if err := side.SetChangelogAcked(ctx, hw); err != nil {
				t.Error(err)
			}
		}
		if err := side.RecordAdvertisedServe(ctx, hw, e.now.Unix()); err != nil {
			t.Error(err)
		}
	}
	code, stdout, stderr := runCLI("on", "--reason", "r", "--wait", "--timeout", "1m", "--settle", "0s", "--json")
	if code != exitOK {
		t.Fatalf("exit = %d, stderr %q", code, stderr)
	}
	if obj := decodeOne(t, stdout); obj["controller_acked"] != true {
		t.Fatalf("output = %v", obj)
	}
	if !strings.Contains(stderr, "waiting on controller acknowledgement") || e.sleeps != 2 {
		t.Fatalf("sleeps = %d, stderr %q", e.sleeps, stderr)
	}
}

func TestMaintenanceCLI_WaitTimeoutBusy(t *testing.T) {
	e := newCLIEnv(t)
	e.lister.set([]maintenance.InFlight{{ID: "c1", Kind: maintenance.KindBorgContainer}})
	code, stdout, stderr := runCLI("on", "--reason", "r", "--wait", "--timeout", "7s", "--no-controller", "--json")
	if code != exitQuiesceTimeout {
		t.Fatalf("exit = %d, want %d (stderr %q)", code, exitQuiesceTimeout, stderr)
	}
	if obj := decodeOne(t, stdout); obj["quiesce"] != maintenance.QuiesceBusy {
		t.Fatalf("output = %v", obj)
	}
	// Polls at 0, 2, 4, 6 and a final one at 7s.
	if e.sleeps != 4 {
		t.Fatalf("sleeps = %d, want 4", e.sleeps)
	}
}

func TestMaintenanceCLI_WaitTimeoutUnacked(t *testing.T) {
	e := newCLIEnv(t)
	side := e.side(t)
	e.onSleep = func(time.Duration) {
		_ = side.RecordAdvertisedServe(context.Background(), 0, e.now.Unix())
	}
	if err := side.RecordAdvertisedServe(context.Background(), 0, e.now.Unix()); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runCLI("on", "--reason", "r", "--wait", "--timeout", "5s"); code != exitNoController {
		t.Fatalf("exit = %d, want %d (stderr %q)", code, exitNoController, stderr)
	}
}

func TestMaintenanceCLI_WaitLocalHoldCleared(t *testing.T) {
	e := newCLIEnv(t)
	side := e.side(t)
	e.lister.set([]maintenance.InFlight{{ID: "c1", Kind: maintenance.KindBorgContainer}})
	e.onSleep = func(time.Duration) {
		if _, err := side.ClearLocalHold(context.Background()); err != nil {
			t.Error(err)
		}
	}
	code, _, stderr := runCLI("on", "--reason", "r", "--wait", "--timeout", "1m", "--no-controller")
	if code != exitUsage || !strings.Contains(stderr, "cleared while waiting") {
		t.Fatalf("exit = %d, stderr %q", code, stderr)
	}
}

func TestMaintenanceCLI_JSONShape(t *testing.T) {
	newCLIEnv(t)
	code, stdout, _ := runCLI("status", "--json")
	if code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	obj := decodeOne(t, stdout)
	for _, k := range []string{
		"paused", "controller", "local", "controller_gen", "running", "pending",
		"skipped_backups", "seq", "quiesce", "sample", "instance_id",
		"controller_acked", "agent_active",
	} {
		if _, ok := obj[k]; !ok {
			t.Errorf("JSON missing %q: %s", k, stdout)
		}
	}
	if r, ok := obj["running"].([]any); !ok || len(r) != 0 {
		t.Errorf("running = %v, want []", obj["running"])
	}
	if obj["agent_active"] != true || obj["controller_acked"] != true {
		t.Errorf("agent_active/controller_acked = %v/%v", obj["agent_active"], obj["controller_acked"])
	}
}
