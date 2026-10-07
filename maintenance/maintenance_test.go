package maintenance

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"cs-agent/store"
)

var ctxBG = context.Background()

// fakeLister returns a fixed container list or error.
type fakeLister struct {
	mu  sync.Mutex
	out []InFlight
	err error
}

func (f *fakeLister) RunningBackupContainers(context.Context) ([]InFlight, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]InFlight(nil), f.out...), f.err
}

func (f *fakeLister) set(out []InFlight, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.out, f.err = out, err
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestBuildStatus_Quiesced(t *testing.T) {
	st := openStore(t)
	s, err := BuildStatus(ctxBG, st, &fakeLister{})
	if err != nil {
		t.Fatalf("BuildStatus: %v", err)
	}
	if s.Quiesce != QuiesceQuiesced || s.Paused || s.Pending != 0 {
		t.Fatalf("status = %+v, want quiesced, not paused, 0 pending", s)
	}
	if s.InstanceID == "" {
		t.Fatalf("instance_id empty")
	}
	b, _ := json.Marshal(s)
	var raw map[string]any
	_ = json.Unmarshal(b, &raw)
	if r, ok := raw["running"].([]any); !ok || len(r) != 0 {
		t.Fatalf("running = %v, want []", raw["running"])
	}
	for _, k := range []string{"paused", "controller", "local", "controller_gen", "pending", "skipped_backups", "seq", "quiesce", "sample", "instance_id"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("status JSON missing %q: %s", k, b)
		}
	}
}

func TestBuildStatus_Busy(t *testing.T) {
	st := openStore(t)
	if _, err := st.CreateTask(ctxBG, store.Task{ID: "t1", Name: "volume.backup", Node: "n", Volume: "vol1"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.ClaimTask(ctxBG, "t1"); err != nil || !ok {
		t.Fatalf("ClaimTask = %v, %v", ok, err)
	}
	if _, err := st.CreateTask(ctxBG, store.Task{ID: "t2", Name: "volume.backup", Node: "n", Volume: "vol2"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.BeginMaintJob(ctxBG, "prune"); err != nil || !ok {
		t.Fatalf("BeginMaintJob = %v, %v", ok, err)
	}
	if _, err := st.PutControllerHold(ctxBG, "kernel", 1); err != nil {
		t.Fatal(err)
	}
	cl := &fakeLister{out: []InFlight{{ID: "c1", Kind: KindBorgContainer, Volume: "vol1", StartedAt: 1}}}

	s, err := BuildStatus(ctxBG, st, cl)
	if err != nil {
		t.Fatalf("BuildStatus: %v", err)
	}
	if s.Quiesce != QuiesceBusy {
		t.Fatalf("quiesce = %q, want busy", s.Quiesce)
	}
	if !s.Paused || s.Controller == nil || s.ControllerGen != 1 || s.PausedSince == 0 {
		t.Fatalf("hold fields not carried: %+v", s)
	}
	if s.Pending != 1 {
		t.Fatalf("pending = %d, want 1", s.Pending)
	}
	kinds := map[string]InFlight{}
	for _, r := range s.Running {
		kinds[r.Kind] = r
	}
	if len(s.Running) != 3 {
		t.Fatalf("running = %+v, want 3 entries", s.Running)
	}
	if r := kinds["volume.backup"]; r.ID != "t1" || r.Volume != "vol1" || r.StartedAt == 0 {
		t.Errorf("task entry = %+v", r)
	}
	if r := kinds["maint.prune"]; r.ID != "maint.prune" || r.StartedAt == 0 {
		t.Errorf("maint entry = %+v", r)
	}
	if r := kinds[KindBorgContainer]; r.ID != "c1" {
		t.Errorf("container entry = %+v", r)
	}
}

func TestBuildStatus_Unknown(t *testing.T) {
	st := openStore(t)
	s, err := BuildStatus(ctxBG, st, &fakeLister{err: errors.New("docker down")})
	if err != nil {
		t.Fatalf("BuildStatus: %v", err)
	}
	if s.Quiesce != QuiesceUnknown {
		t.Fatalf("quiesce = %q, want unknown", s.Quiesce)
	}
	if s.Running == nil {
		t.Fatalf("running is nil")
	}
}

func TestWatch_SamplesWhilePaused(t *testing.T) {
	orig := store.MaintenanceSampleEmitInterval
	store.MaintenanceSampleEmitInterval = 0
	t.Cleanup(func() { store.MaintenanceSampleEmitInterval = orig })
	st := openStore(t)
	cl := &fakeLister{out: []InFlight{{ID: "c1", Kind: KindBorgContainer}}}
	ctx, cancel := context.WithCancel(ctxBG)
	done := make(chan struct{})
	go func() {
		defer close(done)
		watch(ctx, st, cl, 0, 10*time.Millisecond, time.Hour)
	}()
	t.Cleanup(func() { cancel(); <-done })

	// Not paused: nothing is sampled.
	time.Sleep(50 * time.Millisecond)
	if m, _ := st.GetMaintenance(ctxBG); m.Sample != nil {
		t.Fatalf("sample recorded while not paused: %+v", m.Sample)
	}

	if _, _, err := st.PutLocalHold(ctxBG, "work", "root"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		m, _ := st.GetMaintenance(ctxBG)
		return m.Sample != nil && m.Sample.Quiesce == QuiesceBusy && m.Sample.RunningCount == 1
	})
	busySeq := latestMaintSeq(t, st)

	cl.set(nil, nil)
	waitFor(t, func() bool {
		m, _ := st.GetMaintenance(ctxBG)
		return m.Sample != nil && m.Sample.Quiesce == QuiesceQuiesced
	})
	if seq := latestMaintSeq(t, st); seq <= busySeq {
		t.Fatalf("quiesced sample not published: seq %d <= %d", seq, busySeq)
	}
}

func latestMaintSeq(t *testing.T, st *store.Store) int64 {
	t.Helper()
	m, err := st.GetMaintenance(ctxBG)
	if err != nil {
		t.Fatal(err)
	}
	return m.Seq
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met within 5s")
}

func TestCheckStale_NoPanicAndSkipsFresh(t *testing.T) {
	st := openStore(t)
	if _, err := st.PutControllerHold(ctxBG, "r", 0); err != nil {
		t.Fatal(err)
	}
	// Both a fresh hold and an old one must run cleanly (log + Sentry no-op
	// without a configured client).
	checkStale(ctxBG, st, time.Hour, time.Now())
	checkStale(ctxBG, st, time.Hour, time.Now().Add(2*time.Hour))
	checkStale(ctxBG, st, 0, time.Now())
}
