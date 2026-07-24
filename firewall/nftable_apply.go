package firewall

import (
	"fmt"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

// Netlink apply layer for the cs_agent table.
//
// This is the thin, kernel-touching counterpart to the pure buildPlan in
// nftable.go. It translates a renderPlan into nftables objects and applies them
// as a single atomic netlink batch (flush + replace the whole table in one
// transaction), so a reconcile is idempotent and never leaves a half-applied
// ruleset. No string diffing, no shelling out.
//
// dnatValueType is the map value type "ipv4_addr . inet_service": a 4-byte IPv4
// address concatenated with a 2-byte port. Concatenated types are padded to the
// 4-byte register width, so the value occupies 8 bytes across two adjacent 4-byte
// register slots: the addr in the first (reg 1 == NFT_REG32_00) and the port in the
// next (reg 9 == NFT_REG32_01) -- the exact registers the NAT expression references.
var dnatValueType = nftables.MustConcatSetType(nftables.TypeIPAddr, nftables.TypeInetService)

// nftConn is the subset of *nftables.Conn the apply path uses. Defined as an
// interface so the apply assembly could be exercised against a fake in a
// kernel-less test if desired; production uses a real *nftables.Conn.
type nftConn interface {
	AddTable(*nftables.Table) *nftables.Table
	DelTable(*nftables.Table)
	AddChain(*nftables.Chain) *nftables.Chain
	AddSet(*nftables.Set, []nftables.SetElement) error
	AddRule(*nftables.Rule) *nftables.Rule
	Flush() error
}

// applyPlan renders plan into the kernel atomically. It builds the entire
// cs_agent table from scratch each call: add-then-delete the table (so the
// delete always has a target and clears any prior state), then re-create the
// table, chains, sets, maps, elements and rules, and Flush the whole batch as
// one transaction.
func applyPlan(c nftConn, plan *renderPlan) error {
	table := &nftables.Table{Family: nftables.TableFamilyIPv4, Name: csTableName}

	// Idempotent atomic replace: ensure the table exists, delete it (with all
	// its contents), then re-add it fresh -- all in a single Flush batch so the
	// kernel applies it transactionally.
	c.AddTable(table)
	c.DelTable(table)
	c.AddTable(table)

	// nat hook chains. prerouting (dstnat) catches ingress; output (-100,
	// NATDest) mirrors it for host-originated traffic -- the old code jumped the
	// expose-ports chain from both PREROUTING and OUTPUT, so we install the DNAT
	// rules in both.
	prerouting := c.AddChain(&nftables.Chain{
		Name:     chainPrerouting,
		Table:    table,
		Type:     nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookPrerouting,
		Priority: nftables.ChainPriorityNATDest, // dstnat
	})
	output := c.AddChain(&nftables.Chain{
		Name:     chainOutput,
		Table:    table,
		Type:     nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookOutput,
		Priority: nftables.ChainPriorityNATDest, // -100
	})

	// Build + add the published sets and dnat maps FIRST, so they exist before
	// the rules that reference them in the same batch. (Render the published set
	// early -- fail-closed default.)
	pubTCP := publishedSet(table, setPublishedTCP)
	pubUDP := publishedSet(table, setPublishedUDP)
	dnatTCP := dnatMap(table, mapDNATTCP)
	dnatUDP := dnatMap(table, mapDNATUDP)

	if err := c.AddSet(pubTCP, portElements(plan.TCP.Published)); err != nil {
		return fmt.Errorf("add set %s: %w", setPublishedTCP, err)
	}
	if err := c.AddSet(pubUDP, portElements(plan.UDP.Published)); err != nil {
		return fmt.Errorf("add set %s: %w", setPublishedUDP, err)
	}
	dnatTCPElems, err := dnatElements(plan.TCP.DNAT)
	if err != nil {
		return fmt.Errorf("build %s elements: %w", mapDNATTCP, err)
	}
	if err := c.AddSet(dnatTCP, dnatTCPElems); err != nil {
		return fmt.Errorf("add map %s: %w", mapDNATTCP, err)
	}
	dnatUDPElems, err := dnatElements(plan.UDP.DNAT)
	if err != nil {
		return fmt.Errorf("build %s elements: %w", mapDNATUDP, err)
	}
	if err := c.AddSet(dnatUDP, dnatUDPElems); err != nil {
		return fmt.Errorf("add map %s: %w", mapDNATUDP, err)
	}

	// DNAT rules: in both prerouting and output, for tcp and udp.
	//   <proto> dport @published_<proto> dnat to <proto> dport map @dnat_<proto>
	//
	// cs_agent renders DNAT only -- no forward/filter chain. Project bridges run
	// gateway_mode_ipv4=nat-unprotected, so Docker's own per-bridge blanket
	// accept already permits the forwarded ingress (terminating, ahead of where
	// the old container-inbound sat); a base-chain accept here would be
	// redundant and wouldn't terminate FORWARD anyway.
	for _, ch := range []*nftables.Chain{prerouting, output} {
		c.AddRule(dnatRule(table, ch, unix.IPPROTO_TCP, pubTCP, dnatTCP))
		c.AddRule(dnatRule(table, ch, unix.IPPROTO_UDP, pubUDP, dnatUDP))
	}

	return c.Flush()
}

// publishedSet describes a `set <name> { type inet_service }` of live nat ports.
func publishedSet(table *nftables.Table, name string) *nftables.Set {
	return &nftables.Set{
		Table:        table,
		Name:         name,
		KeyType:      nftables.TypeInetService,
		KeyByteOrder: binaryutil.BigEndian,
	}
}

// dnatMap describes a `map <name> { type inet_service : ipv4_addr . inet_service }`.
func dnatMap(table *nftables.Table, name string) *nftables.Set {
	return &nftables.Set{
		Table:        table,
		Name:         name,
		IsMap:        true,
		KeyType:      nftables.TypeInetService,
		DataType:     dnatValueType,
		KeyByteOrder: binaryutil.BigEndian,
	}
}

// portElements turns a list of ports into inet_service set keys (big-endian u16).
func portElements(ports []uint16) []nftables.SetElement {
	if len(ports) == 0 {
		return nil
	}
	out := make([]nftables.SetElement, 0, len(ports))
	for _, p := range ports {
		out = append(out, nftables.SetElement{Key: binaryutil.BigEndian.PutUint16(p)})
	}
	return out
}

// dnatElements turns DNAT entries into map elements: key = nat_port (u16),
// value = ipv4_addr(4) . inet_service(2, padded to 4) = 8 bytes.
func dnatElements(entries []dnatEntry) ([]nftables.SetElement, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	out := make([]nftables.SetElement, 0, len(entries))
	for _, e := range entries {
		ip := e.DestIP.To4()
		if ip == nil {
			// buildPlan already guarantees IPv4, but guard the apply layer too.
			return nil, fmt.Errorf("dnat dest %q is not IPv4", e.DestIP.String())
		}
		val := make([]byte, 0, 8)
		val = append(val, ip...)                                         // 4 bytes ipv4_addr
		val = append(val, binaryutil.BigEndian.PutUint16(e.DestPort)...) // 2 bytes inet_service
		val = append(val, 0, 0)                                          // pad inet_service to the 4-byte register width
		out = append(out, nftables.SetElement{
			Key: binaryutil.BigEndian.PutUint16(e.NatPort),
			Val: val,
		})
	}
	return out, nil
}

// dnatRule builds: fib daddr type local <proto> dport @published_<proto> dnat to <proto> dport map @dnat_<proto>
//
// Expression sequence:
//
//	fib daddr type -> reg1 ; cmp == local     (dest is a node-local address)
//	meta l4proto == <proto>                       (only match this transport proto)
//	payload TH+2 len 2 -> reg1   (the dport)
//	lookup reg1 in @published_<proto>             (is it a live published port?)
//	payload TH+2 len 2 -> reg1   (re-load dport as the map key)
//	lookup reg1 in @dnat_<proto> -> reg1          (map writes ipv4 -> reg1, port -> reg9)
//	nat dnat addr=reg1 proto=reg9
//
// The leading `fib daddr type local` gate mirrors Docker's own
// `-m addrtype --dst-type LOCAL -j DOCKER`: it restricts the DNAT to packets
// addressed to one of THIS node's local addresses (the published endpoint reached
// via the node's WAN IP or a node-owned gateway IP). Without it, the rule DNATs
// ANY forwarded/host-origin packet whose dport merely collides with a published
// port, regardless of the address dialed -- so a container dialing a sibling
// project's *private* bridge IP on a colliding port would be silently redirected
// to the publisher. Gating on daddr-type-local makes "was DNAT'd" mean "dialed a
// node-local published endpoint" by construction, independent of how nat ports
// are allocated. This is load-bearing for the cross-project isolation RETURN in
// firewall/isolation.go (rule 2 RETURNs DNAT'd bridge-to-bridge connections): the
// gate is what keeps that from exposing direct private-bridge-IP access. External
// ingress still DNATs (the node's own WAN IP is local); the host-origin OUTPUT
// mirror still DNATs host->node_ip:port traffic (also local).
func dnatRule(table *nftables.Table, chain *nftables.Chain, proto byte, published, dnat *nftables.Set) *nftables.Rule {
	return &nftables.Rule{
		Table: table,
		Chain: chain,
		Exprs: []expr.Any{
			// fib daddr type -> reg1, gate on == RTN_LOCAL (dest is node-local).
			// fib result is a host-order u32, so compare with native endianness.
			&expr.Fib{Register: 1, FlagDADDR: true, ResultADDRTYPE: true},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.NativeEndian.PutUint32(unix.RTN_LOCAL)},
			&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{proto}},
			// dport -> reg1, gate on membership in the published set
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
			&expr.Lookup{SourceRegister: 1, SetName: published.Name, SetID: published.ID},
			// dport -> reg1 again as the map key; map writes ipv4(reg1) . port(reg9)
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
			&expr.Lookup{SourceRegister: 1, DestRegister: 1, IsDestRegSet: true, SetName: dnat.Name, SetID: dnat.ID},
			// proto_min is reg 9, NOT 2: the map lookup wrote its 8-byte concat value
			// into the 16-byte register reg 1, which the kernel reads as two 4-byte
			// slots -- the ipv4 addr in reg 1 (NFT_REG32_00) and the port in reg 9
			// (NFT_REG32_01, the next slot). Legacy "reg 2" is a different register
			// (byte offset 16) that nothing wrote, so the verifier rejects the rule
			// and Flush returns ENODATA. Matches nft: `nat dnat ip addr_min reg 1 proto_min reg 9`.
			&expr.NAT{Type: expr.NATTypeDestNAT, Family: unix.NFPROTO_IPV4, RegAddrMin: 1, RegProtoMin: 9},
		},
	}
}

// renderTable performs a full reconcile of the cs_agent table from desired
// state against the kernel. It opens a transient netlink connection, builds the
// plan, and applies it atomically. This is the only function in the package
// that touches the kernel for the published-port path.
func renderTable(rules *NatRules) error {
	c, err := nftables.New()
	if err != nil {
		return fmt.Errorf("open nftables netlink conn: %w", err)
	}
	return applyPlan(c, buildPlan(rules))
}
