package job

import (
	"context"
	"cs-agent/backup"
	"cs-agent/store"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/getsentry/sentry-go"
)

// worker pulls claimed tasks off its queue and runs them until ctx is cancelled.
// When slots is non-nil (the backup pool) the worker returns its slot token after
// each task so the dispatcher can claim the next one.
func (d *Dispatcher) worker(ctx context.Context, wg *sync.WaitGroup, name string, queue <-chan store.Task, slots chan<- struct{}) {
	defer wg.Done()
	defer func() { jobEvent().Info("Worker stopping", "queue", name) }()
	for {
		select {
		case <-ctx.Done():
			jobEvent().Info("[" + name + "] Shutting down")
			return
		case task := <-queue:
			d.runTask(ctx, task)
			if slots != nil {
				slots <- struct{}{}
			}
			if ctx.Err() != nil {
				jobEvent().Info("[" + name + "] Shutdown")
				return
			}
		}
	}
}

// runTask executes a claimed (running) task and records its terminal status +
// result via the store. A deferred TERMINAL GUARD ensures a task never stays
// "running": a recovered panic (or any exit without a terminal write) marks the
// task failed — the csevent CloseEvent + finalizeStuckExport that used to own this
// are gone, so the worker owns it now.
func (d *Dispatcher) runTask(ctx context.Context, task store.Task) {
	completed := false
	defer func() {
		if r := recover(); r != nil {
			hub := sentry.CurrentHub().Clone()
			hub.Recover(r)
			hub.Flush(2 * time.Second)
			jobEvent().Error("task panicked", "task", task.ID, "kind", task.Name, "panic", fmt.Sprintf("%v", r))
			d.markFailed(task.ID, "task panicked")
			return
		}
		if !completed {
			// Handler returned without us recording a terminal status (should not
			// happen) — fail closed rather than leave the task running forever.
			d.markFailed(task.ID, "task exited without a terminal result")
		}
	}()

	start := time.Now()
	jobEvent().Info("Processing task", "task", task.ID, "kind", task.Name)
	run := d.runner
	if run == nil {
		run = backup.RunTask
	}
	result, err := run(ctx, d.st, task)
	status := store.TaskCompleted
	if err != nil {
		status = store.TaskFailed
		jobEvent().Warn("task failed", "task", task.ID, "kind", task.Name, "error", err.Error())
	}
	// Record the terminal status on a fresh context, not the worker ctx: a task
	// that finished right as shutdown cancelled ctx must still be recorded with its
	// true outcome (not failed by the guard) — important for the never-replayed
	// kinds (restore/delete/export/trash) where a false failure needs a manual
	// re-request.
	writeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	uErr := d.st.UpdateTaskStatus(writeCtx, task.ID, status, result)
	if uErr != nil {
		// One retry: a transient control.db write failure must not turn a real
		// success into a false failure on the never-replay kinds (restore/export/
		// delete/trash), where the guard's fallback would need a manual re-request.
		jobEvent().Warn("record task status failed; retrying once", "task", task.ID, "error", uErr.Error())
		uErr = d.st.UpdateTaskStatus(writeCtx, task.ID, status, result)
	}
	if uErr != nil {
		jobEvent().Warn("failed to record task status", "task", task.ID, "error", uErr.Error())
		return // leave completed=false so the guard marks it failed
	}
	// Terminal success line, so a task UUID can be traced start→finish in the node
	// log (previously only abnormal exits logged anything). Gated on status because
	// a failure already logged "task failed" above with the error text — an
	// unconditional line here would double-log every failure. It sits AFTER the
	// successful UpdateTaskStatus so the line means "finished AND recorded"; the
	// early return on write failure above correctly emits no success line.
	//
	// hclog's standard formatter renders a time.Duration via %v (duration=15.023s).
	// Under JSONFormat it would marshal as an integer nanosecond count instead;
	// log/log.go sets no JSONFormat today, so this is fine as-is.
	if status == store.TaskCompleted {
		jobEvent().Info("task completed", "task", task.ID, "kind", task.Name,
			"duration", time.Since(start).Round(time.Millisecond))
	}
	completed = true
}

// markFailed records a failed terminal status on a background context (the
// worker ctx may already be cancelled during shutdown/panic).
func (d *Dispatcher) markFailed(id, reason string) {
	result, _ := json.Marshal(map[string]string{"error": reason})
	if err := d.st.UpdateTaskStatus(context.Background(), id, store.TaskFailed, result); err != nil {
		jobEvent().Warn("failed to mark task failed", "task", id, "error", err.Error())
	}
}
