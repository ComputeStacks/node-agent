package firewall

import (
	"context"
	"cs-agent/store"
	"cs-agent/watchdog"
	"time"
)

// firewallBackstop re-runs the reconcile even without a signal, so a missed poke
// (or drift) is corrected within a bounded window.
const firewallBackstop = 60 * time.Second

// Reconciler runs firewall.Reconcile on boot, on a signal (a controller
// firewall_rules PUT/DELETE), and on a periodic backstop — off the request
// goroutine so the DOWN handler never blocks on an nftables render.
type Reconciler struct {
	st     *store.Store
	signal chan struct{}
	// Heartbeat, when set, is beaten after every reconcile so a wedged
	// reconcile withholds the systemd watchdog keepalive.
	Heartbeat *watchdog.Heartbeat
}

func NewReconciler(st *store.Store) *Reconciler {
	return &Reconciler{st: st, signal: make(chan struct{}, 1)}
}

// Signal requests a reconcile on the next loop iteration. Non-blocking +
// coalescing.
func (r *Reconciler) Signal() {
	select {
	case r.signal <- struct{}{}:
	default:
	}
}

// Run reconciles until ctx is cancelled. Call in its own goroutine.
func (r *Reconciler) Run(ctx context.Context) {
	Reconcile(ctx, r.st) // boot
	r.Heartbeat.Beat()
	t := time.NewTicker(firewallBackstop)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.signal:
			Reconcile(ctx, r.st)
		case <-t.C:
			Reconcile(ctx, r.st)
		}
		r.Heartbeat.Beat()
	}
}
