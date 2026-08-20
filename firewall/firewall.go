package firewall

import (
	"context"
	"cs-agent/log"
	"cs-agent/store"

	"github.com/getsentry/sentry-go"
	"github.com/hashicorp/go-hclog"
)

// Reconcile enforces cross-project network isolation and renders this node's
// published-port nftables table from control.db desired-state.
//
// The populated-sentinel gate is placed AFTER isolation on purpose: at the
// big-bang cutover a node boots with a fresh, empty control.db while its
// workloads are live. Until the controller has populated firewall desired-state
// (the firewall-populated sentinel latches), we STILL apply cross-project
// isolation but SKIP the published-port render — leaving the live cs_agent table
// (rendered by the prior binary, persistent in the kernel) untouched so no
// published port is closed in the gap. Once populated, an empty rule set renders
// an empty table (fail-closed) as intended.
func Reconcile(ctx context.Context, st *store.Store) {
	defer sentry.Recover()

	// Cross-project isolation stays iptables-based in DOCKER-USER and is
	// independent of the published-port table; always apply it.
	ensureProjectIsolation(ctx)

	populated, err := st.IsPopulated(ctx, store.MetaFirewallPopulated)
	if err != nil {
		csFirewallLog().Warn("firewall: could not check populated sentinel", "error", err.Error())
		return
	}
	if !populated {
		csFirewallLog().Info("firewall desired-state not yet populated; leaving live published-port table untouched")
		return
	}

	expectedRules, err := loadExpectedRules(ctx, st)
	if err != nil {
		// Load/parse error: leave the kernel state untouched (do not render an
		// empty table). The next reconcile re-renders from desired state.
		return
	}
	// A nil *NatRules here means "populated, but zero published ports" — a
	// legitimate desired state, so render an empty cs_agent table (fail-closed).
	if err := renderTable(expectedRules); err != nil {
		sentry.CaptureException(err)
		csFirewallLog().Error("Failed to render cs_agent nftables table", "error", err.Error())
		return
	}
}

// Logger
func csFirewallLog() hclog.Logger {
	return log.New().Named("cs-firewall")
}
