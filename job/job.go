package job

import (
	"context"
	"cs-agent/log"
	"cs-agent/store"
	"encoding/json"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/spf13/viper"
)

// backstopInterval is how often the dispatcher re-drains ListPendingTasks even
// without a wake signal — a safety net for a task committed just before a missed
// signal, or one left pending by an export revert.
const backstopInterval = 30 * time.Second

// slotRecheckInterval is how often the dispatcher, while waiting for a free
// backup worker, re-checks whether the node has entered maintenance. A var so
// tests can shorten it.
var slotRecheckInterval = 5 * time.Second

// Dispatcher is the in-process replacement for the old Consul jobs/ long-poll. A
// SINGLE goroutine drains this node's pending tasks and dispatches each into a
// worker pool; being single-goroutine is load-bearing — it (with the ClaimTask
// CAS) guarantees a task is dispatched at most once even when a wake signal and
// the backstop coincide.
type Dispatcher struct {
	st            *store.Store
	backupQ       chan store.Task
	exportQ       chan store.Task
	signal        chan struct{}
	backupWorkers int
	exportWorkers int
	// runner executes a task; nil means backup.RunTask (the production path).
	// Overridable in tests to exercise the worker's terminal guard directly.
	runner func(context.Context, *store.Store, store.Task) (json.RawMessage, error)
	// backupSlots holds one token per idle backup worker. The dispatcher takes a
	// token BEFORE claiming a backup task and the worker returns it when the task
	// ends, so a task is only moved to running once a worker is free to start it.
	backupSlots chan struct{}
}

// NewDispatcher builds the dispatcher (worker pools sized by config). The backup
// queue is buffered to the pool size so a claimed task is handed off without
// blocking; the slot tokens keep it from ever holding more than the idle workers.
func NewDispatcher(st *store.Store) *Dispatcher {
	backupWorkers := viper.GetInt("queue.numworkers") + 1
	if backupWorkers < 1 {
		backupWorkers = 1
	}
	exportWorkers := viper.GetInt("backups.export.workers")
	if exportWorkers < 1 {
		exportWorkers = 1
	}
	slots := make(chan struct{}, backupWorkers)
	for i := 0; i < backupWorkers; i++ {
		slots <- struct{}{}
	}
	return &Dispatcher{
		st:            st,
		backupQ:       make(chan store.Task, backupWorkers),
		exportQ:       make(chan store.Task),
		backupSlots:   slots,
		signal:        make(chan struct{}, 1),
		backupWorkers: backupWorkers,
		exportWorkers: exportWorkers,
	}
}

// Signal wakes the dispatcher to drain pending tasks. Non-blocking + coalescing:
// callers (the task-create HTTP handler, the scheduler) never block, and bursts
// collapse into a single drain.
func (d *Dispatcher) Signal() {
	select {
	case d.signal <- struct{}{}:
	default:
	}
}

// Start runs the boot crash-reconcile, starts the worker pools, and runs the
// single dispatch loop until ctx is done. Call in its own goroutine. wg tracks
// the worker pools + the dispatch loop so main can bound the shutdown drain.
func (d *Dispatcher) Start(ctx context.Context, wg *sync.WaitGroup) {
	// Fail any task left "running" by a crashed process BEFORE accepting new
	// work, so a re-drain can't race a reconcile of the same task.
	d.bootReconcile(ctx)

	d.startWorkers(ctx, wg, "backup", d.backupWorkers, d.backupQ, d.backupSlots)
	d.startWorkers(ctx, wg, "export", d.exportWorkers, d.exportQ, nil)

	wg.Add(1)
	go d.loop(ctx, wg)
}

func (d *Dispatcher) startWorkers(ctx context.Context, wg *sync.WaitGroup, name string, count int, q <-chan store.Task, slots chan<- struct{}) {
	wg.Add(count)
	for i := 1; i <= count; i++ {
		jobEvent().Info("Starting worker process", "queue", name, "worker-process", i)
		go d.worker(ctx, wg, name, q, slots)
	}
}

// loop is the single dispatch goroutine.
func (d *Dispatcher) loop(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	defer func() { jobEvent().Info("Dispatcher stopping") }()

	d.drain(ctx) // drain whatever is already pending at boot
	backstop := time.NewTicker(backstopInterval)
	defer backstop.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.signal:
			d.drain(ctx)
		case <-backstop.C:
			d.drain(ctx)
		}
	}
}

// drain claims and dispatches every pending task for this node. It is the ONLY
// claimer/dispatcher. Exports are dispatched first (non-blocking) so a busy backup
// pool can't head-of-line-block an export; backups then wait for a free worker
// (the workers are the throughput limiter). While the node is in maintenance
// nothing is claimed; an un-pause wakes the dispatcher to drain again.
func (d *Dispatcher) drain(ctx context.Context) {
	if paused, err := d.st.IsPaused(ctx); err != nil {
		// ClaimTask still refuses while paused, so carry on.
		jobEvent().Warn("dispatch: check maintenance", "error", err.Error())
	} else if paused {
		jobEvent().Debug("dispatch: node in maintenance; not claiming tasks")
		return
	}
	pending, err := d.st.ListPendingTasks(ctx)
	if err != nil {
		jobEvent().Warn("dispatch: list pending tasks", "error", err.Error())
		return
	}
	for _, task := range pending {
		if ctx.Err() != nil {
			return
		}
		if task.Name == "backup.export" {
			d.dispatchExport(ctx, task)
		}
	}
	for _, task := range pending {
		if ctx.Err() != nil {
			return
		}
		if task.Name != "backup.export" {
			if !d.dispatchBackup(ctx, task) {
				return
			}
		}
	}
}

// dispatchBackup waits for a free backup worker, THEN claims (CAS
// pending->running) and hands the task off, so a task is never running while
// still waiting for a worker. Only a task we won the CAS on is dispatched, so a
// signal + backstop can't double-run one task. It returns false when drain should
// stop: ctx is done or the node entered maintenance while we waited.
func (d *Dispatcher) dispatchBackup(ctx context.Context, task store.Task) bool {
	if !d.acquireBackupSlot(ctx) {
		return false
	}
	claimed, err := d.st.ClaimTask(ctx, task.ID)
	if err != nil {
		d.backupSlots <- struct{}{}
		jobEvent().Warn("dispatch: claim task", "task", task.ID, "error", err.Error())
		return true
	}
	if !claimed {
		d.backupSlots <- struct{}{}
		return true // already claimed/terminal, or paused (ClaimTask refuses)
	}
	// Never blocks: the queue is sized to the pool and we hold a slot.
	d.backupQ <- task
	return true
}

// acquireBackupSlot blocks until a backup worker is free. While waiting it wakes
// every slotRecheckInterval and gives up (false) if the node has entered
// maintenance, so a paused node doesn't sit holding a pending task; it also gives
// up when ctx is done.
func (d *Dispatcher) acquireBackupSlot(ctx context.Context) bool {
	select {
	case <-d.backupSlots:
		return true
	default:
	}
	recheck := time.NewTicker(slotRecheckInterval)
	defer recheck.Stop()
	for {
		select {
		case <-d.backupSlots:
			return true
		case <-ctx.Done():
			return false
		case <-recheck.C:
			paused, err := d.st.IsPaused(ctx)
			if err != nil {
				jobEvent().Warn("dispatch: check maintenance", "error", err.Error())
				continue
			}
			if paused {
				jobEvent().Debug("dispatch: node entered maintenance while waiting for a backup worker")
				return false
			}
		}
	}
}

// dispatchExport claims then NON-BLOCKING sends to the export pool; if the pool is
// full the claim is reverted (running->pending) so a later wake retries it. A
// crash between claim and revert leaves the task running, which the boot reconcile
// then fails — an export is never blindly re-run.
func (d *Dispatcher) dispatchExport(ctx context.Context, task store.Task) {
	claimed, err := d.st.ClaimTask(ctx, task.ID)
	if err != nil {
		jobEvent().Warn("dispatch: claim export", "task", task.ID, "error", err.Error())
		return
	}
	if !claimed {
		return
	}
	select {
	case d.exportQ <- task:
	default:
		if _, uErr := d.st.UnclaimTask(ctx, task.ID); uErr != nil {
			jobEvent().Warn("dispatch: unclaim export (pool full)", "task", task.ID, "error", uErr.Error())
		}
	}
}

// bootReconcile fails every task left "running" by a crashed process. On boot no
// task is truly in flight, so a "running" row is orphaned work. NONE are
// auto-replayed — destructive kinds (restore/delete/trash) must never re-run
// unbidden, and export must never blindly re-upload; the scheduler re-fires
// backups on their next slot; the controller re-requests the rest.
func (d *Dispatcher) bootReconcile(ctx context.Context) {
	running, err := d.st.ListRunningTasks(ctx)
	if err != nil {
		jobEvent().Warn("boot reconcile: list running tasks", "error", err.Error())
		return
	}
	result, _ := json.Marshal(map[string]string{"error": "agent restarted while task was running; not auto-replayed"})
	for _, task := range running {
		if err := d.st.UpdateTaskStatus(ctx, task.ID, store.TaskFailed, result); err != nil {
			jobEvent().Warn("boot reconcile: mark failed", "task", task.ID, "error", err.Error())
			continue
		}
		jobEvent().Warn("boot reconcile: failed orphaned running task", "task", task.ID, "kind", task.Name)
	}
}

func jobEvent() hclog.Logger {
	return log.New().Named("worker")
}
