package firewall

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/getsentry/sentry-go"
)

// Cross-project network isolation, for both address families.
//
// Every ComputeStacks project network is created (on Docker >= 28) with
// gateway_mode_ipv4=nat-unprotected (controller: CreateBridgeNetworkService).
// That mode makes Docker emit, for each project bridge, a blanket
//
//	-A DOCKER ! -i br-X -o br-X -j ACCEPT
//
// which accepts *all* forwarded ingress to that bridge -- including traffic
// routed from a *different* project's bridge. The result is that any container
// (e.g. an ssh/bastion container) can reach containers in every other project
// on the same node. We keep nat-unprotected (the controller publishes ports
// out-of-band via the expose-ports / container-inbound chains, which plain
// "nat" mode would drop), and instead restore isolation in DOCKER-USER.
//
// DOCKER-USER is evaluated in the FORWARD chain *before* DOCKER-FORWARD, and
// Docker never flushes its contents, so it is the supported place for this.
// Three rules, in required order (1 < 2 < 3):
//
//  1. RETURN packets that are being *bridged* (L2, same bridge) -- this is
//     intra-project container-to-container traffic, which must keep working.
//     Where br_netfilter is loaded (net.bridge.bridge-nf-call-ip[6]tables=1)
//     those frames traverse FORWARD as "-i br-X -o br-X"; --physdev-is-bridged
//     is the canonical way to distinguish bridged (L2) from routed (L3) here.
//     Do NOT assume br_netfilter is loaded everywhere: where it is not, bridged
//     frames never enter FORWARD at all, so neither rule 1 nor rule 3 ever sees
//     intra-bridge traffic and rule 1 has nothing to match. Isolation of
//     *routed* cross-bridge traffic -- what rule 3 exists for -- works either
//     way, because routed packets traverse FORWARD regardless.
//  2. RETURN routed bridge-to-bridge packets that belong to a DNAT'd connection
//     -- i.e. cross-project traffic that arrived via a *published port*. The
//     connection is DNAT-marked in prerouting, so --ctstate DNAT matches here in
//     FORWARD (both directions -- the status bit is connection-scoped, so replies
//     match without a separate ESTABLISHED rule). RETURN falls through to
//     Docker's per-bridge nat-unprotected ACCEPT, so the packet is delivered.
//     This is deliberate policy: a published port is a node-level endpoint,
//     reachable from any project on the node, not an external-only one (reaching
//     it must not depend on the luck of container co-placement).
//  3. DROP everything else that both enters from and leaves to a docker bridge
//     -- i.e. *routed* bridge-to-bridge traffic that is NOT a published-port
//     connection: direct private-bridge-IP access across projects. Still blocked.
//
// External/WAN published-port ingress is unaffected either way: it arrives on a
// non-bridge interface after DNAT, so the "-i br-+" rules never match it.
// Host -> container management traffic is OUTPUT, not FORWARD, so it is
// untouched. The interface-wildcard form is deliberately subnet-agnostic
// (project supernets differ per region: 10.100/16, 10.167/16, ...), so nothing
// here is hardcoded.
//
// SCOPE of rule 2: --ctstate DNAT whitelists *every* DNAT'd bridge-to-bridge
// connection regardless of who installed the DNAT (the cs_agent published-port
// table, Docker's own -p mappings on infra containers, any future NAT rule) --
// i.e. "publish a port on this node" means "cross-project reachable". That is the
// intended policy; do not re-narrow it by accident. Crucially this does NOT open
// direct private-bridge-IP access: the cs_agent DNAT chain is gated with
// `fib daddr type local` (nftable_apply.go dnatRule), so only a packet dialing a
// node-local published endpoint is DNAT'd -- dialing a sibling's private bridge
// IP is never rewritten, so it carries no DNAT ctstate and falls to rule 3's DROP.
//
// IPv6
// ----
// Project bridge networks may now be created dual-stack (a controller-side
// option), which gives tenant containers IPv6 *egress only*: no v6 published
// ports and no v6 inbound. Docker does not take an IPv6 gateway mode from that
// option, so such a bridge keeps Docker's default v6 behaviour, which already
// emits the mirror image of the v4 blanket accept:
//
//	-A DOCKER ! -i br-X -o br-X -j DROP
//
// Cross-project IPv6 is therefore not open today, and these three rules are
// defense-in-depth on v6 -- load-bearing on v4. They exist on v6 because that
// invariant is Docker's default rather than something this repo asserts: a
// future Docker default, or anyone setting
// com.docker.network.bridge.gateway_mode_ipv6=nat-unprotected by symmetry with
// the v4 option, would open cross-project IPv6 immediately with nothing here
// noticing. Stated precisely, these rules make that misconfiguration less bad,
// not safe: nat-unprotected on v6 would *also* admit "-i <any non-bridge
// interface> -o br-X" inbound straight to container addresses, which none of the
// three rules match. On v4 the host firewall plus the expose-ports /
// container-inbound chains cover WAN ingress; there is no v6 counterpart today.
//
// Rule 2 has nothing to match on v6 under an egress-only design (there are no v6
// published ports), but it is NOT side-effect-free: inserting an -m conntrack
// rule makes xt_conntrack's checkentry call nf_ct_netns_get(net, par->family),
// registering IPv6 conntrack hooks in the netns if nothing else has. nf_conntrack
// budgets are per-netns and family-agnostic, so that would start charging host
// IPv6 traffic against the same budget as IPv4. What makes it safe -- on any
// host, independently of what else is installed -- is that the v6 rules are
// applied only where Docker has an IPv6-enabled bridge network, and Docker emits
// its own per-network v6 conntrack rule for such a bridge
// (DOCKER-CT -o <iface> -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT),
// so the registration is already there and the delta is zero. Keep the two paths
// identical rather than dropping rule 2 on v6: one spec list and one ordering
// decision cannot diverge between families.
//
// The v6 rules are applied only on a node that has at least one IPv6-enabled
// docker bridge network (ipv6_detect.go) and REMOVED on a node that has none.
// The removal path is deliberate and load-bearing: nothing else in this codebase
// deletes these rules except as the first half of a re-apply, Docker never
// flushes DOCKER-USER, and downgrading the package cannot remove them either
// (an older binary has no v6 code path at all). Removal is the only rollback
// there is, so it has to keep working. "Docker did not answer" is a third state
// and must never delete anything.
//
// INSERT ORDER, precisely. The three rules are inserted at position 1 in reverse
// order, so a partially failed apply fails towards the DROP: a failed RETURN
// insert cannot open the chain. It is NOT symmetrical -- a failed DROP insert
// leaves the chain with two RETURNs and no DROP, i.e. fail *open* -- and the
// delete-then-reinsert sequence likewise has the live DROP absent for a few
// shell-outs on every non-fast-path apply. That window is bounded by the next
// reconcile and is now detected: the post-condition sees isolationOrdered ==
// false and pages. Cross-tenant reachability is a security incident and an
// outage is not, which is why the order favours the DROP.
//
// The FORWARD jump is verified too, per family. The contents of DOCKER-USER are
// worthless if FORWARD no longer jumps to it, and another daemon may rewrite
// FORWARD on its own schedule. The check is presence-only, never an index check,
// and never repairs the jump -- re-inserting a jump underneath another daemon's
// chain layout is not this agent's business. Detection only, so the gap surfaces
// instead of being enforced by nothing.
//
// INVARIANT: this assumes exactly one project per Linux bridge (enforced by the
// controller: Deployment has_one :private_network, one docker bridge network per
// project). Rule 1 RETURNs *any* L2-bridged packet, so if two projects ever
// shared a bridge, intra-bridge traffic between them would escape isolation.
// Rule 3 (the DROP) treats ALL br-* bridges as mutually isolated for direct
// access -- there is no allow-list, so a future shared/infra bridge that must
// reach project containers directly would need an explicit
// "-i br-INFRA -o br-+ -j RETURN" above the DROP.
const (
	isoChain = "DOCKER-USER"
	// isoForwardChain is read (never written) to confirm it still jumps to
	// DOCKER-USER; isoJumpSpec is the spec searched for in that listing.
	isoForwardChain = "FORWARD"
	isoJumpSpec     = "-j " + isoChain
	// The three DOCKER-USER isolation rules, in required order (1 < 2 < 3). Each
	// string is the EXACT spec used to insert, delete, and find the rule, so the
	// "insert", "delete" and "verify" paths can never diverge.
	isoReturnSpec     = "-m physdev --physdev-is-bridged -j RETURN"
	isoDNATReturnSpec = "-i br-+ -o br-+ -m conntrack --ctstate DNAT -j RETURN"
	isoDropSpec       = "-i br-+ -o br-+ -j DROP"

	// Bound on the per-spec delete loop. Real chains have 0-2 stray copies; the
	// bound only guards against a pathological/looping state.
	maxIsolationCleanupPasses = 8
)

// isoExec is the single shell-out boundary for the isolation path. Tests replace
// it to capture an ordered transcript of the literal commands issued; nothing
// else in this file calls exec directly. The seam is here, at the exec boundary,
// rather than around chainExists/chainRules/runIptables, because runIptables
// builds the final command string itself -- a seam above it would observe
// neither the binary nor the spacing -- and because deleteAllRules calls
// runIptables directly, so a seam that missed it would delete live firewall
// rules from a test run.
var isoExec = func(cmd string) ([]byte, error) {
	return exec.Command("bash", "-c", cmd).CombinedOutput()
}

// ipv6PresenceFn is the detection seam (ipv6_detect.go). Tests replace it.
var ipv6PresenceFn = ipv6BridgePresence

// isoCapture is the Sentry boundary. It is seamed for one reason: without it,
// deleting either capture call site leaves the whole test suite green, so
// "log it AND page" can quietly decay into "log it" with nothing noticing.
// The page is the requirement here, not a nicety -- an unisolated tenant that
// only produces a log line nobody reads is the failure this whole check exists
// to prevent.
var isoCapture = captureIsoFailure

// isoFamily is the address family being acted on: a name for logs and the
// binary to shell out to. Both families run the same code path, so the specs
// and their ordering cannot diverge between them.
type isoFamily struct {
	name string
	bin  string
}

// isolationOrdered reports whether all three isolation rules are present in
// DOCKER-USER and in the required order: phys-RETURN < DNAT-RETURN < DROP.
func isolationOrdered(lines []string) bool {
	ret := ruleIndex(lines, isoReturnSpec)
	dnat := ruleIndex(lines, isoDNATReturnSpec)
	drop := ruleIndex(lines, isoDropSpec)
	return ret >= 0 && dnat >= 0 && drop >= 0 && ret < dnat && dnat < drop
}

// ensureProjectIsolation reconciles the isolation rules for both address
// families. It is safe to call on every reconcile.
//
// IPv4 runs first and unconditionally: it is the control that protects tenants
// today, so nothing in the IPv6 path -- detection included -- may precede it,
// short-circuit it, or share an error path with it.
func ensureProjectIsolation(ctx context.Context) {
	ensureProjectIsolationFamily(isoFamily{name: "IPv4", bin: iptablesCmd()})

	v6 := isoFamily{name: "IPv6", bin: ip6tablesCmd()}
	switch ipv6PresenceFn(ctx) {
	case ipv6BridgePresent:
		shouldCaptureIsoFailure(isoCondition{family: v6.name, condition: isoCondDetection}, false)
		ensureProjectIsolationFamily(v6)
	case ipv6BridgeNone:
		shouldCaptureIsoFailure(isoCondition{family: v6.name, condition: isoCondDetection}, false)
		removeProjectIsolationFamily(v6)
	default: // ipv6BridgeUnknown
		// Docker did not answer. Leave the kernel exactly as it is: treating
		// this as "no IPv6 bridges" would delete a live isolation control
		// because dockerd was briefly unreachable.
		noteIsolationDetectionUnknown(v6)
	}
}

// ensureProjectIsolationFamily makes DOCKER-USER hold the three isolation rules
// above, in the correct order, idempotently, for one address family.
//
// Step order matters. The jump check is a PEER of the contents check and runs
// BEFORE the "already correct" fast-path return, not after the apply: a node
// whose FORWARD jump was dropped while DOCKER-USER still holds the three correct
// rules takes that fast path on every reconcile forever, so a jump check placed
// below it would only ever fire when the contents were also wrong -- i.e. only
// when it was redundant, and never in the one state it exists to detect.
//
// Steady state is therefore reads only and zero mutating commands.
func ensureProjectIsolationFamily(f isoFamily) {
	contents := isoCondition{family: f.name, condition: isoCondContents}
	jump := isoCondition{family: f.name, condition: isoCondForwardJump}

	// DOCKER-USER is created by dockerd. If it is not present yet (docker not
	// started, or no ip6tables/IPv6 on this host), skip; the next reconcile /
	// boot run will install the rules. Never fatal, and never a page: this is a
	// legitimate state, not evidence that isolation is broken. The warning is
	// rate-limited to the transition -- Reconcile runs every 60s, and an
	// unconditional warning would emit ~1440 lines a day and train operators to
	// filter a message that also means "IPv4 tenant isolation is not installed".
	if !chainExists(f.bin, isoChain) {
		if shouldCaptureIsoFailure(isoCondition{family: f.name, condition: isoCondChainAbsent}, true) {
			csFirewallLog().Warn("DOCKER-USER not present; skipping cross-project isolation rules", "family", f.name)
		}
		shouldCaptureIsoFailure(contents, false)
		shouldCaptureIsoFailure(jump, false)
		return
	}
	shouldCaptureIsoFailure(isoCondition{family: f.name, condition: isoCondChainAbsent}, false)

	// Jump check. Presence only -- another daemon may rewrite FORWARD on its own
	// schedule, so the index is not ours to assert -- and detection only: we
	// never insert or move the jump.
	if forward, err := chainRules(f.bin, isoForwardChain); err != nil {
		// A read failure is not a verdict: no capture, no state change. The
		// shell-outs here pass no -w and share /run/xtables.lock with dockerd,
		// so a container-start burst can lose the lock.
		csFirewallLog().Warn("could not read FORWARD to verify the DOCKER-USER jump", "family", f.name, "err", err.Error())
	} else {
		missing := !forwardJumpPresent(forward)
		if missing {
			csFirewallLog().Error("FORWARD does not jump to DOCKER-USER; cross-project isolation rules are not being evaluated", "family", f.name, "forward", strings.Join(forward, " | "))
		}
		if shouldCaptureIsoFailure(jump, missing) {
			isoCapture(jump, strings.Join(forward, " | "))
		}
	}

	current, err := chainRules(f.bin, isoChain)
	if err != nil {
		// Read failure again: leave the chain and the failure state alone. Do
		// NOT treat it as "rules missing" and re-apply, and do not page.
		csFirewallLog().Warn("could not read DOCKER-USER; leaving cross-project isolation rules as-is", "family", f.name, "err", err.Error())
		return
	}

	// Already correct: all three present and correctly ordered. Do nothing so we
	// never momentarily remove the isolation during steady-state reconciles.
	if isolationOrdered(current) {
		shouldCaptureIsoFailure(contents, false)
		return
	}

	csFirewallLog().Info("Enforcing cross-project network isolation in DOCKER-USER", "family", f.name)

	// Remove every stray/misordered copy of each spec, then re-insert all three at
	// the top in the correct order. Deletes are issued unconditionally (loop until
	// -D errors) rather than gated on a chainRules() text search: iptables -D
	// matches rules *semantically* (by compiled spec, not by -S output text), so
	// this removes a rule even if the kernel renders its match tokens differently
	// from our literal spec string. That bounds a rendering mismatch to per-reconcile
	// churn (delete+re-insert) instead of unbounded chain growth.
	for _, spec := range []string{isoReturnSpec, isoDNATReturnSpec, isoDropSpec} {
		deleteAllRules(f.bin, isoChain, spec)
	}

	// Insert at position 1 in reverse of the desired order, so each "-I ... 1"
	// pushes the prior insert down: DROP -> DNAT-RETURN -> phys-RETURN lands
	// phys-RETURN@1, DNAT-RETURN@2, DROP@3 -- ahead of anything else in the chain.
	if err := runIptables(f.bin, "-I "+isoChain+" 1 "+isoDropSpec); err != nil {
		csFirewallLog().Warn("failed to insert cross-project isolation DROP", "family", f.name, "err", err.Error())
	}
	if err := runIptables(f.bin, "-I "+isoChain+" 1 "+isoDNATReturnSpec); err != nil {
		// Most likely cause: xt_conntrack / nf_conntrack not loadable on this
		// kernel (practically impossible on a Docker host -- NAT needs conntrack).
		csFirewallLog().Warn("failed to insert cross-project isolation DNAT RETURN (xt_conntrack missing?)", "family", f.name, "err", err.Error())
	}
	if err := runIptables(f.bin, "-I "+isoChain+" 1 "+isoReturnSpec); err != nil {
		// Most likely cause: xt_physdev not loadable on this kernel.
		csFirewallLog().Warn("failed to insert cross-project isolation RETURN (xt_physdev missing?)", "family", f.name, "err", err.Error())
	}

	// Post-condition: if the rules did not end up present and correctly ordered
	// (failed insert, or a rule-rendering mismatch that would otherwise make us
	// churn delete+re-insert every reconcile), surface it loudly.
	final, err := chainRules(f.bin, isoChain)
	if err != nil {
		csFirewallLog().Warn("could not re-read DOCKER-USER to verify the cross-project isolation rules", "family", f.name, "err", err.Error())
		return
	}
	failed := !isolationOrdered(final)
	if failed {
		csFirewallLog().Error("cross-project isolation rules not present/ordered after apply", "family", f.name, "docker-user", strings.Join(final, " | "))
	}
	if shouldCaptureIsoFailure(contents, failed) {
		isoCapture(contents, strings.Join(final, " | "))
	}
}

// removeProjectIsolationFamily deletes the three isolation rules for one family.
// It runs when detection reports, affirmatively, that this node has no
// IPv6-enabled docker bridge network -- and it is the only rollback path there
// is, so it must keep working.
//
// The no-op guard is chainExists plus an empty chain, NOT a per-spec text match:
// deletes are semantic (see deleteAllRules), and gating them on our literal spec
// text would reintroduce exactly the rendering mismatch that rationale exists to
// avoid -- here, by silently disabling the rollback. When the chain is non-empty
// but ours are absent we simply accept three failing -D calls.
//
// No post-condition and no page: an absent rule set is the intended state here.
func removeProjectIsolationFamily(f isoFamily) {
	if !chainExists(f.bin, isoChain) {
		// Nothing to remove, and nothing to warn about: this family has no
		// chain and no rules, which is the state we are trying to reach.
		resetIsoFailures(f)
		return
	}

	current, err := chainRules(f.bin, isoChain)
	if err != nil {
		csFirewallLog().Warn("could not read DOCKER-USER; leaving cross-project isolation rules as-is", "family", f.name, "err", err.Error())
		return
	}
	// An empty chain (just its -N line) is the steady state on every node with
	// no IPv6-enabled bridge, hit every 60s forever: it must issue nothing.
	if !chainHasRules(current) {
		resetIsoFailures(f)
		return
	}

	removed := 0
	for _, spec := range []string{isoReturnSpec, isoDNATReturnSpec, isoDropSpec} {
		removed += deleteAllRules(f.bin, isoChain, spec)
	}
	if removed > 0 {
		csFirewallLog().Info("Removed cross-project network isolation rules from DOCKER-USER", "family", f.name, "rules", removed)
	}
	resetIsoFailures(f)
}

// noteIsolationDetectionUnknown reports that docker could not be asked whether
// this node has an IPv6-enabled bridge network, so the IPv6 rules were left
// exactly as they are. A permanent failure (socket permissions, a non-default
// socket path) would otherwise log once and go quiet forever, with the IPv6
// control absent and no way to tell that apart from "intentionally off", so the
// reminder repeats -- hourly, not per reconcile.
func noteIsolationDetectionUnknown(f isoFamily) {
	c := isoCondition{family: f.name, condition: isoCondDetection}
	due := shouldRemindIsoFailure(c, isoDetectionReminderInterval)
	if shouldCaptureIsoFailure(c, true) || due {
		csFirewallLog().Warn("cannot determine whether this node has an IPv6-enabled docker bridge network; leaving the isolation rules for this family untouched", "family", f.name)
	}
}

// Failure state, keyed by (family, condition).
//
// One bool per family would both flood and mask: a family that latched true and
// then took the removal path would never page again, and folding the jump verdict
// into the same bool would let a node with a missing FORWARD jump swallow the page
// for a genuinely unordered chain. Each condition therefore latches on its own,
// and every early-return path above sets or resets it explicitly rather than
// leaving it stale -- except a failed *read*, which is not a verdict and leaves
// the state alone.
type isoCondition struct {
	family    string
	condition string
}

const (
	isoCondContents    = "contents"
	isoCondForwardJump = "forward-jump"
	isoCondChainAbsent = "chain-absent"
	isoCondDetection   = "detection"

	// How often an unresolved detection failure is repeated in the log.
	isoDetectionReminderInterval = time.Hour
)

var (
	isoFailedMu    sync.Mutex
	isoFailedState = map[isoCondition]bool{} // {family, condition} -> last verdict
	isoRemindedAt  = map[isoCondition]time.Time{}
)

// shouldCaptureIsoFailure records the current verdict for a condition and reports
// whether this is a false -> true transition, i.e. worth capturing to Sentry.
// Reconcile runs every 60s, so capturing on every failing pass would flood.
// Logging is deliberately not rate-limited this way: a persistent failure should
// stay visible in the log.
func shouldCaptureIsoFailure(c isoCondition, failed bool) bool {
	isoFailedMu.Lock()
	defer isoFailedMu.Unlock()
	if !failed {
		delete(isoFailedState, c)
		delete(isoRemindedAt, c)
		return false
	}
	was := isoFailedState[c]
	isoFailedState[c] = true
	return !was
}

// shouldRemindIsoFailure reports whether a still-unresolved condition is due for
// its periodic reminder, stamping the time when it is.
func shouldRemindIsoFailure(c isoCondition, every time.Duration) bool {
	isoFailedMu.Lock()
	defer isoFailedMu.Unlock()
	now := time.Now()
	if last, ok := isoRemindedAt[c]; ok && now.Sub(last) < every {
		return false
	}
	isoRemindedAt[c] = now
	return true
}

// resetIsoFailures clears both rule-level verdicts for a family. Used by the
// paths where "no rules" is the intended state (chain absent, removal), so a
// later genuine failure is still a transition and still pages.
func resetIsoFailures(f isoFamily) {
	shouldCaptureIsoFailure(isoCondition{family: f.name, condition: isoCondContents}, false)
	shouldCaptureIsoFailure(isoCondition{family: f.name, condition: isoCondForwardJump}, false)
}

// captureIsoFailure pages. Called only on a condition's false -> true
// transition; the Error log beside it fires every reconcile.
func captureIsoFailure(c isoCondition, detail string) {
	sentry.WithScope(func(scope *sentry.Scope) {
		scope.SetTag("family", c.family)
		scope.SetTag("isolation-condition", c.condition)
		scope.SetExtra("detail", detail)
		sentry.CaptureMessage(fmt.Sprintf("cross-project isolation %s check failed (%s)", c.condition, c.family))
	})
}

// deleteAllRules removes every copy of spec from chain and returns how many were
// removed. iptables -D deletes one matching rule per call and errors when none
// remain, so we loop (bounded) until it errors. Because -D matches semantically,
// it clears strays even when the kernel's -S rendering of the rule diverges from
// our literal spec string.
func deleteAllRules(bin, chain, spec string) int {
	for i := 0; i < maxIsolationCleanupPasses; i++ {
		if err := runIptables(bin, "-D "+chain+" "+spec); err != nil {
			return i
		}
	}
	return maxIsolationCleanupPasses
}

func chainExists(bin, chain string) bool {
	_, err := isoExec(fmt.Sprintf("%s -S %s", bin, chain))
	return err == nil
}

// chainRules returns the `<bin> -S <chain>` lines. The error is returned rather
// than swallowed because a failed *read* must not be mistaken for missing rules:
// isolationOrdered(nil) is false, so treating a lost xtables lock as "the rules
// are gone" would re-apply a correct chain and page for it.
func chainRules(bin, chain string) ([]string, error) {
	out, err := isoExec(fmt.Sprintf("%s -S %s", bin, chain))
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n"), nil
}

// chainHasRules reports whether an `-S <chain>` listing holds any rule at all. A
// chain carrying only its own -N declaration is empty.
func chainHasRules(lines []string) bool {
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "-A ") {
			return true
		}
	}
	return false
}

// forwardJumpPresent reports whether any FORWARD rule jumps to isoChain.
//
// The match is an exact terminal one, NOT a substring: "-j DOCKER-USER" is a
// prefix of "-j DOCKER-USER2", so a Contains test would accept a jump to an
// unrelated chain whose name merely starts with ours and report the isolation
// rules as reachable while nothing evaluates them. A jump target is terminal in
// `-S` output, so anchoring at the end of the line is sufficient and still
// accepts a conditional jump ("-A FORWARD -i eth0 -j DOCKER-USER").
func forwardJumpPresent(lines []string) bool {
	for _, l := range lines {
		if strings.HasSuffix(strings.TrimSpace(l), isoJumpSpec) {
			return true
		}
	}
	return false
}

// ruleIndex returns the index of the first line containing spec, or -1. spec is
// the exact rule specification used to insert and delete the rule, so the
// "find", "delete" and "verify" paths can never diverge.
func ruleIndex(lines []string, spec string) int {
	for i, l := range lines {
		if strings.Contains(l, spec) {
			return i
		}
	}
	return -1
}

// runIptables runs `<bin> <args>` and returns the error (if any). It matches the
// package convention of shelling out via bash; bin is one of two compile-time
// constants (iptables.go) and all args here are compile-time constants, so there
// is no untrusted input to quote.
func runIptables(bin, args string) error {
	execCmd := fmt.Sprintf("%s %s", bin, args)
	if out, err := isoExec(execCmd); err != nil {
		csFirewallLog().Debug("isolation iptables cmd failed", "cmd", execCmd, "out", string(out))
		return err
	}
	return nil
}
