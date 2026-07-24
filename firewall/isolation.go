package firewall

import (
	"fmt"
	"os/exec"
	"strings"
)

// Cross-project network isolation.
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
//     With net.bridge.bridge-nf-call-iptables=1 (true on all nodes) those
//     frames traverse FORWARD as "-i br-X -o br-X"; --physdev-is-bridged is the
//     canonical way to distinguish bridged (L2) from routed (L3) here.
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
// non-bridge interface (WAN/tailscale) after DNAT, so the "-i br-+" rules never
// match it. Host -> container management traffic is OUTPUT, not FORWARD, so it is
// untouched. The interface-wildcard form is deliberately subnet-agnostic (project
// supernets differ per region: 10.100/16, 10.167/16, ...), so nothing here is
// hardcoded.
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
// INVARIANT: this assumes exactly one project per Linux bridge (enforced by the
// controller: Deployment has_one :private_network, one docker bridge network per
// project). Rule 1 RETURNs *any* L2-bridged packet, so if two projects ever
// shared a bridge, intra-bridge traffic between them would escape isolation.
// Rule 3 (the DROP) treats ALL br-* bridges as mutually isolated for direct
// access -- there is no allow-list, so a future shared/infra bridge that must
// reach project containers directly would need an explicit
// "-i br-INFRA -o br-+ -j RETURN" above the DROP. IPv6 is intentionally not
// handled here: project bridge networks are IPv4-only (controller rejects IPv6
// subnets); revisit with an ip6tables variant if that ever changes.
const (
	isoChain = "DOCKER-USER"
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

// isolationOrdered reports whether all three isolation rules are present in
// DOCKER-USER and in the required order: phys-RETURN < DNAT-RETURN < DROP.
func isolationOrdered(lines []string) bool {
	ret := ruleIndex(lines, isoReturnSpec)
	dnat := ruleIndex(lines, isoDNATReturnSpec)
	drop := ruleIndex(lines, isoDropSpec)
	return ret >= 0 && dnat >= 0 && drop >= 0 && ret < dnat && dnat < drop
}

// ensureProjectIsolation makes DOCKER-USER hold the three isolation rules above,
// in the correct order, idempotently. It is safe to call on every reconcile.
func ensureProjectIsolation() {
	// DOCKER-USER is created by dockerd. If it is not present yet (docker not
	// started), skip; the next reconcile / boot run will install the rules.
	if !chainExists(isoChain) {
		csFirewallLog().Warn("DOCKER-USER not present; skipping cross-project isolation rules")
		return
	}

	// Already correct: all three present and correctly ordered. Do nothing so we
	// never momentarily remove the isolation during steady-state reconciles.
	if isolationOrdered(chainRules(isoChain)) {
		return
	}

	csFirewallLog().Info("Enforcing cross-project network isolation in DOCKER-USER")

	// Remove every stray/misordered copy of each spec, then re-insert all three at
	// the top in the correct order. Deletes are issued unconditionally (loop until
	// -D errors) rather than gated on a chainRules() text search: iptables -D
	// matches rules *semantically* (by compiled spec, not by -S output text), so
	// this removes a rule even if the kernel renders its match tokens differently
	// from our literal spec string. That bounds a rendering mismatch to per-reconcile
	// churn (delete+re-insert) instead of unbounded chain growth.
	for _, spec := range []string{isoReturnSpec, isoDNATReturnSpec, isoDropSpec} {
		deleteAllRules(isoChain, spec)
	}

	// Insert at position 1 in reverse of the desired order, so each "-I ... 1"
	// pushes the prior insert down: DROP -> DNAT-RETURN -> phys-RETURN lands
	// phys-RETURN@1, DNAT-RETURN@2, DROP@3 -- ahead of anything else in the chain.
	if err := runIptables("-I " + isoChain + " 1 " + isoDropSpec); err != nil {
		csFirewallLog().Warn("failed to insert cross-project isolation DROP", "err", err.Error())
	}
	if err := runIptables("-I " + isoChain + " 1 " + isoDNATReturnSpec); err != nil {
		// Most likely cause: xt_conntrack / nf_conntrack not loadable on this
		// kernel (practically impossible on a Docker host -- NAT needs conntrack).
		csFirewallLog().Warn("failed to insert cross-project isolation DNAT RETURN (xt_conntrack missing?)", "err", err.Error())
	}
	if err := runIptables("-I " + isoChain + " 1 " + isoReturnSpec); err != nil {
		// Most likely cause: xt_physdev not loadable on this kernel.
		csFirewallLog().Warn("failed to insert cross-project isolation RETURN (xt_physdev missing?)", "err", err.Error())
	}

	// Post-condition: if the rules did not end up present and correctly ordered
	// (failed insert, or a rule-rendering mismatch that would otherwise make us
	// churn delete+re-insert every reconcile), surface it loudly.
	if !isolationOrdered(chainRules(isoChain)) {
		csFirewallLog().Error("cross-project isolation rules not present/ordered after apply", "docker-user", strings.Join(chainRules(isoChain), " | "))
	}
}

// deleteAllRules removes every copy of spec from chain. iptables -D deletes one
// matching rule per call and errors when none remain, so we loop (bounded) until
// it errors. Because -D matches semantically, it clears strays even when the
// kernel's -S rendering of the rule diverges from our literal spec string.
func deleteAllRules(chain, spec string) {
	for i := 0; i < maxIsolationCleanupPasses; i++ {
		if err := runIptables("-D " + chain + " " + spec); err != nil {
			return
		}
	}
}

func chainExists(chain string) bool {
	return exec.Command("bash", "-c", fmt.Sprintf("%s -S %s", iptablesCmd(), chain)).Run() == nil
}

// chainRules returns the `iptables -S <chain>` lines. An empty slice is returned
// on error as well; callers treat "absent" and "empty" the same (re-apply), and
// ensureProjectIsolation only calls this after chainExists has passed.
func chainRules(chain string) []string {
	out, err := exec.Command("bash", "-c", fmt.Sprintf("%s -S %s", iptablesCmd(), chain)).CombinedOutput()
	if err != nil {
		return []string{}
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n")
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

// runIptables runs `<iptables-cmd> <args>` and returns the error (if any). It
// matches the package convention of shelling out via bash; all args here are
// compile-time constants, so there is no untrusted input to quote.
func runIptables(args string) error {
	execCmd := fmt.Sprintf("%s %s", iptablesCmd(), args)
	if out, err := exec.Command("bash", "-c", execCmd).CombinedOutput(); err != nil {
		csFirewallLog().Debug("isolation iptables cmd failed", "cmd", execCmd, "out", string(out))
		return err
	}
	return nil
}
