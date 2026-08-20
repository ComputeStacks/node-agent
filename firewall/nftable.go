package firewall

import (
	"net"
	"sort"
)

// Native nftables renderer for the published-port path.
//
// This replaces the old iptables string-diff reconcile of the `expose-ports`
// DNAT chain. The agent now owns a single `cs_agent` ip table, rendered
// atomically from the same desired state (Consul `nodes/<host>/ingress_rules`
// -> NatRules) on every reconcile.
//
// cs_agent renders DNAT only -- no forward/filter chain. The old
// `container-inbound` forward-accept has always been redundant: project bridges
// run gateway_mode_ipv4=nat-unprotected, so Docker emits a per-bridge blanket
// accept that fires (terminating) ahead of where container-inbound sat.
// Cross-project isolation stays in DOCKER-USER (isolation.go, unchanged).
//
// Table shape (DNAT-only):
//
//	table ip cs_agent {
//	  set published_tcp { type inet_service }            # live nat ports (tcp)
//	  set published_udp { type inet_service }            # live nat ports (udp)
//	  map dnat_tcp { type inet_service : ipv4_addr . inet_service }   # nat_port -> dest . port
//	  map dnat_udp { type inet_service : ipv4_addr . inet_service }
//	  chain prerouting { type nat hook prerouting priority dstnat;
//	        tcp dport @published_tcp dnat to tcp dport map @dnat_tcp; udp ... }
//	  chain output     { type nat hook output priority -100;  ... }   # host-origin mirror
//	}
//
// The build (desired-state -> renderPlan) is a pure function so it can be unit
// tested without the kernel; the netlink apply (nftable_apply.go) is a thin
// separate layer.

// Table / object names. Centralised so the build, apply, and any future
// read-back/verify path can never diverge on a name.
const (
	csTableName = "cs_agent"

	setPublishedTCP = "published_tcp"
	setPublishedUDP = "published_udp"
	mapDNATTCP      = "dnat_tcp"
	mapDNATUDP      = "dnat_udp"

	chainPrerouting = "prerouting"
	chainOutput     = "output"

	protoTCP = "tcp"
	protoUDP = "udp"
)

// dnatEntry is one published-port DNAT target: nat_port -> dest . container_port.
type dnatEntry struct {
	NatPort  uint16
	DestIP   net.IP // always a 4-byte IPv4 (project bridge networks are IPv4-only)
	DestPort uint16
}

// protoPlan holds the rendered objects for a single L4 protocol (tcp or udp).
type protoPlan struct {
	Proto string // "tcp" / "udp"

	// Published host ports (the set elements). Sorted, de-duplicated.
	Published []uint16

	// DNAT map entries (nat_port -> dest . port). Sorted by NatPort, de-duplicated.
	DNAT []dnatEntry
}

// renderPlan is the serializable, kernel-free description of the cs_agent table
// produced from desired state. The netlink apply layer consumes it directly.
type renderPlan struct {
	TCP protoPlan
	UDP protoPlan
}

// buildPlan is the pure desired-state -> nft-objects function. It is total: a
// nil or empty ruleset yields an empty-but-well-formed plan (the table will be
// rendered with empty sets/maps -> all published ports closed, which is the
// intended fail-closed default).
//
// cs_agent renders DNAT only (no forward/filter chain), so the Driver field is a
// no-op here: the DNAT/published entry is built for every rule regardless of
// Driver, matching the old nat_rule.go (which had no Driver guard). The old
// forward-accept's calico-node skip no longer applies because there is no
// forward-accept.
//
// Rules whose Dest does not parse as an IPv4 address are skipped entirely (the
// old shell path would have emitted a malformed iptables rule; here we cannot
// represent a non-IPv4 dest in the ipv4_addr map, and project bridges are
// IPv4-only by construction). Unknown protocols (neither tcp nor udp) are
// likewise skipped.
func buildPlan(rules *NatRules) *renderPlan {
	p := &renderPlan{
		TCP: protoPlan{Proto: protoTCP},
		UDP: protoPlan{Proto: protoUDP},
	}
	if rules == nil {
		return p
	}

	// Accumulate into de-dup maps keyed by the natural identity of each object,
	// so duplicate desired-state rules collapse instead of producing duplicate
	// set/map elements (which the kernel would reject).
	type accum struct {
		published map[uint16]struct{}
		dnat      map[uint16]dnatEntry // keyed by nat port (the map key must be unique)
	}
	tcp := accum{published: map[uint16]struct{}{}, dnat: map[uint16]dnatEntry{}}
	udp := accum{published: map[uint16]struct{}{}, dnat: map[uint16]dnatEntry{}}

	for i := range rules.Rules {
		r := rules.Rules[i]

		var a *accum
		switch r.Proto {
		case protoTCP:
			a = &tcp
		case protoUDP:
			a = &udp
		default:
			// Unknown protocol: cannot place it in a tcp/udp set. Skip.
			continue
		}

		dest := parseIPv4(r.Dest)
		if dest == nil {
			// Non-IPv4 / unparseable dest cannot be represented in the
			// ipv4_addr map. Skip (project bridges are IPv4-only).
			continue
		}

		nat := uint16(r.Nat)
		port := uint16(r.Port)

		// Published port + DNAT map entry: applied for every rule regardless of
		// Driver (matches the old nat_rule.go, which had no Driver guard).
		a.published[nat] = struct{}{}
		a.dnat[nat] = dnatEntry{NatPort: nat, DestIP: dest, DestPort: port}
	}

	p.TCP.Published = sortedPorts(tcp.published)
	p.UDP.Published = sortedPorts(udp.published)
	p.TCP.DNAT = sortedDNAT(tcp.dnat)
	p.UDP.DNAT = sortedDNAT(udp.dnat)
	return p
}

// parseIPv4 returns the 4-byte representation of s, or nil if s is not a valid
// IPv4 address. (net.ParseIP returns a 16-byte form for IPv4 literals; To4
// normalises and also rejects IPv6.)
//
// Rejecting IPv6 here is deliberate and stays correct even where a project
// bridge network is dual-stack: this is the published-port NAT layer, and
// published ports are IPv4-only (the cs_agent nftables table is
// TableFamilyIPv4). A container's IPv6 connectivity is egress-only, so there is
// no v6 destination to DNAT to. Not an oversight -- do not "fix" it by
// accepting v6 here without a v6 table to render into.
func parseIPv4(s string) net.IP {
	ip := net.ParseIP(s)
	if ip == nil {
		return nil
	}
	return ip.To4()
}

func sortedPorts(m map[uint16]struct{}) []uint16 {
	if len(m) == 0 {
		return nil
	}
	out := make([]uint16, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func sortedDNAT(m map[uint16]dnatEntry) []dnatEntry {
	if len(m) == 0 {
		return nil
	}
	out := make([]dnatEntry, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NatPort < out[j].NatPort })
	return out
}
