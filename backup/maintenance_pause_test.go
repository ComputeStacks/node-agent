package backup

import (
	"context"
	"sync"
	"testing"
	"time"

	"cs-agent/store"
	"cs-agent/types"
)

func pauseNode(t *testing.T, st *store.Store) {
	t.Helper()
	if _, _, err := st.PutLocalHold(context.Background(), "test", "tester"); err != nil {
		t.Fatalf("put local hold: %v", err)
	}
}

func unpauseNode(t *testing.T, st *store.Store) {
	t.Helper()
	if _, err := st.ClearLocalHold(context.Background()); err != nil {
		t.Fatalf("clear local hold: %v", err)
	}
}

func maintMarkers(t *testing.T, st *store.Store) map[string]int64 {
	t.Helper()
	m, err := st.ListMaintJobs(context.Background())
	if err != nil {
		t.Fatalf("list maint jobs: %v", err)
	}
	return m
}

// A due backup slot passed while paused is skipped: no task, next_fire_at moved
// into the future, the skip counted, and nothing fires once the pause ends.
func TestScheduler_FireDueSkipsWhilePaused(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	s := newTestScheduler(t, st)
	putVol(t, st, types.Volume{Name: "v1", Node: "test-node", Backup: true, Freq: "0 2 * * *", ProjectID: 7})
	pauseNode(t, st)
	if err := st.PutSchedule(ctx, "v1", "0 2 * * *", 1); err != nil {
		t.Fatal(err)
	}

	s.fireDue(ctx)

	if pending, _ := st.ListPendingTasks(ctx); len(pending) != 0 {
		t.Fatalf("backup enqueued while paused: %+v", pending)
	}
	sc, _, _ := st.GetSchedule(ctx, "v1")
	if sc.NextFireAt <= time.Now().Unix() {
		t.Fatalf("next_fire_at = %d not advanced past now", sc.NextFireAt)
	}
	ms, err := st.GetMaintenance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ms.SkippedBackups != 1 {
		t.Fatalf("skipped_backups = %d, want 1", ms.SkippedBackups)
	}

	// Exiting maintenance does not replay the skipped slot.
	unpauseNode(t, st)
	s.fireDue(ctx)
	if pending, _ := st.ListPendingTasks(ctx); len(pending) != 0 {
		t.Fatalf("skipped slot fired after the pause ended: %+v", pending)
	}
}

// While paused, BeginMaintJob refuses: the job does not run, its slot is skipped
// (next recomputed into the future) and the overlap guard is left clear. Once
// un-paused the next due slot runs, with the marker present only while it runs.
func TestScheduler_MaintenanceRefusedWhilePaused(t *testing.T) {
	st := testStore(t)
	s := newTestScheduler(t, st)
	pauseNode(t, st)

	release := make(chan struct{})
	started := make(chan struct{}, 1)
	job := &maintJob{
		name: "prune",
		expr: "* * * * *",
		next: time.Now().Add(-time.Minute),
		run: func(ctx context.Context) {
			started <- struct{}{}
			<-release
		},
	}
	s.maint = []*maintJob{job}

	s.runMaintenance(context.Background())
	s.maintWg.Wait()
	select {
	case <-started:
		t.Fatal("maintenance job ran while paused")
	default:
	}
	if !job.next.After(time.Now()) {
		t.Fatalf("refused slot not skipped: next = %v", job.next)
	}
	if job.running.Load() {
		t.Fatal("overlap guard left set after a refused start")
	}
	if m := maintMarkers(t, st); len(m) != 0 {
		t.Fatalf("marker written for a refused job: %v", m)
	}

	unpauseNode(t, st)
	job.next = time.Now().Add(-time.Minute)
	s.runMaintenance(context.Background())
	<-started
	if m := maintMarkers(t, st); len(m) != 1 || m["prune"] == 0 {
		t.Fatalf("markers while running = %v, want prune", m)
	}
	close(release)
	s.maintWg.Wait()
	if m := maintMarkers(t, st); len(m) != 0 {
		t.Fatalf("marker left after the job ended: %v", m)
	}
}

// A marker that already exists (e.g. a run held elsewhere) refuses the start the
// same way a pause does.
func TestScheduler_MaintenanceRefusedOnExistingMarker(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	s := newTestScheduler(t, st)
	if ok, err := st.BeginMaintJob(ctx, "compact"); err != nil || !ok {
		t.Fatalf("seed marker: ok=%v err=%v", ok, err)
	}
	ran := false
	job := &maintJob{
		name: "compact",
		expr: "* * * * *",
		next: time.Now().Add(-time.Minute),
		run:  func(ctx context.Context) { ran = true },
	}
	s.maint = []*maintJob{job}

	s.runMaintenance(ctx)
	s.maintWg.Wait()
	if ran {
		t.Fatal("job ran although its marker already existed")
	}
	if job.running.Load() {
		t.Fatal("overlap guard left set after a refused start")
	}
	if !job.next.After(time.Now()) {
		t.Fatalf("refused slot not skipped: next = %v", job.next)
	}
}

// A panicking job still clears its marker and the overlap guard, so the next slot
// can start.
func TestScheduler_MaintenanceEndsOnPanic(t *testing.T) {
	st := testStore(t)
	s := newTestScheduler(t, st)
	runs := 0
	job := &maintJob{
		name: "prune",
		expr: "* * * * *",
		next: time.Now().Add(-time.Minute),
		run: func(ctx context.Context) {
			runs++
			panic("boom")
		},
	}
	s.maint = []*maintJob{job}

	s.runMaintenance(context.Background())
	s.maintWg.Wait()
	if m := maintMarkers(t, st); len(m) != 0 {
		t.Fatalf("marker left after a panic: %v", m)
	}
	if job.running.Load() {
		t.Fatal("overlap guard left set after a panic")
	}

	job.next = time.Now().Add(-time.Minute)
	s.runMaintenance(context.Background())
	s.maintWg.Wait()
	if runs != 2 {
		t.Fatalf("runs = %d, want 2 (a panic must not block the next slot)", runs)
	}
}

// stubSweep replaces fn (pruneVolume or compactVolume) for the test, recording the
// volumes reached and calling onVolume after each.
func stubSweep(t *testing.T, fn *func(*store.Store, types.Volume), onVolume func()) *[]string {
	t.Helper()
	orig := *fn
	t.Cleanup(func() { *fn = orig })
	var mu sync.Mutex
	var got []string
	*fn = func(_ *store.Store, vol types.Volume) {
		mu.Lock()
		got = append(got, vol.Name)
		mu.Unlock()
		if onVolume != nil {
			onVolume()
		}
	}
	return &got
}

func putSweepVols(t *testing.T, st *store.Store) {
	t.Helper()
	for _, name := range []string{"v1", "v2", "v3"} {
		putVol(t, st, types.Volume{Name: name, Node: "test-node", Backup: true, Freq: "0 2 * * *", ProjectID: 7})
		if err := st.UpsertRepository(context.Background(), store.Repository{Name: name, Archives: []string{"a1"}}); err != nil {
			t.Fatalf("upsert repository: %v", err)
		}
	}
}

// prune and compact check the pause before every volume: a pause taken while one
// volume is being processed lets that volume finish and stops the sweep before
// the next; a node already paused processes nothing.
func TestSweeps_StopWhenPaused(t *testing.T) {
	sweeps := []struct {
		name string
		fn   *func(*store.Store, types.Volume)
		run  func(context.Context, *store.Store)
	}{
		{"prune", &pruneVolume, prune},
		{"compact", &compactVolume, compact},
	}
	for _, sw := range sweeps {
		t.Run(sw.name+"/not paused", func(t *testing.T) {
			st := testStore(t)
			putSweepVols(t, st)
			got := stubSweep(t, sw.fn, nil)
			sw.run(context.Background(), st)
			if len(*got) != 3 {
				t.Fatalf("volumes processed = %v, want all 3", *got)
			}
		})
		t.Run(sw.name+"/already paused", func(t *testing.T) {
			st := testStore(t)
			putSweepVols(t, st)
			pauseNode(t, st)
			got := stubSweep(t, sw.fn, nil)
			sw.run(context.Background(), st)
			if len(*got) != 0 {
				t.Fatalf("volumes processed while paused = %v, want none", *got)
			}
		})
		t.Run(sw.name+"/paused mid-sweep", func(t *testing.T) {
			st := testStore(t)
			putSweepVols(t, st)
			var once sync.Once
			got := stubSweep(t, sw.fn, func() { once.Do(func() { pauseNode(t, st) }) })
			sw.run(context.Background(), st)
			if len(*got) != 1 {
				t.Fatalf("volumes processed = %v, want only the one in progress when paused", *got)
			}
		})
	}
}
