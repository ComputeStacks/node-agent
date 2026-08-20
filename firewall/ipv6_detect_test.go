package firewall

import (
	"testing"

	"github.com/docker/docker/api/types/network"
)

// bridgeNet builds a docker bridge-driver network summary. opts is passed
// through verbatim so a nil Options map (the common real-world case: no
// per-network options set) is exercised rather than papered over.
func bridgeNet(name string, ipv6 bool, opts map[string]string) network.Summary {
	return network.Summary{Name: name, Driver: "bridge", EnableIPv6: ipv6, Options: opts}
}

// bridgeNameOpts is the Options map docker writes when a network overrides its
// generated br-<netid> interface name.
func bridgeNameOpts(iface string) map[string]string {
	return map[string]string{bridgeNameOption: iface}
}

// classifyIPv6Bridges gates the ip6tables half of the cross-project isolation
// rules. A wrong answer is asymmetric: a false "present" installs rules that
// have nothing to act on, while a false "none" DELETES a live tenant-isolation
// control. Both directions are pinned here, so the "none" cases are as
// load-bearing as the "present" ones.
func TestClassifyIPv6Bridges(t *testing.T) {
	cases := []struct {
		name        string
		nets        []network.Summary
		expect      ipv6BridgeState
		unreachable []string
	}{
		// A successful query returning nothing is a definite "none", never
		// unknown -- unknown is reserved for a failed query.
		{"nil list", nil, ipv6BridgeNone, nil},
		{"empty list", []network.Summary{}, ipv6BridgeNone, nil},

		// docker's predefined "bridge" network lives on docker0, which the
		// br-+ wildcard never matches, so it must not flip the gate even
		// with IPv6 enabled.
		{
			"predefined bridge network with IPv6 does not count",
			[]network.Summary{bridgeNet(defaultBridgeNetwork, true, nil)},
			ipv6BridgeNone, nil,
		},
		{
			"predefined bridge network with a custom iface is skipped before the iface check",
			[]network.Summary{bridgeNet(defaultBridgeNetwork, true, bridgeNameOpts("docker0"))},
			ipv6BridgeNone, nil,
		},

		// IPv4-only bridges are the majority of real networks; counting one
		// would install IPv6 rules on a node with no IPv6 bridges at all.
		{
			"bridge network with IPv6 disabled",
			[]network.Summary{bridgeNet("proj_a", false, nil)},
			ipv6BridgeNone, nil,
		},
		{
			"several IPv4-only bridges",
			[]network.Summary{bridgeNet("proj_a", false, nil), bridgeNet("proj_b", false, nil)},
			ipv6BridgeNone, nil,
		},

		// Non-bridge drivers get no br- interface. These also catch an
		// implementation that filters on the network name alone and forgets
		// the driver check: none of these are named "bridge".
		{
			"host driver with IPv6",
			[]network.Summary{{Name: "host", Driver: "host", EnableIPv6: true}},
			ipv6BridgeNone, nil,
		},
		{
			"null driver with IPv6",
			[]network.Summary{{Name: "none", Driver: "null", EnableIPv6: true}},
			ipv6BridgeNone, nil,
		},
		{
			"overlay driver with IPv6 and a non-default name",
			[]network.Summary{{Name: "swarm_overlay", Driver: "overlay", EnableIPv6: true}},
			ipv6BridgeNone, nil,
		},
		{
			"macvlan driver with IPv6",
			[]network.Summary{{Name: "lan6", Driver: "macvlan", EnableIPv6: true}},
			ipv6BridgeNone, nil,
		},

		// The affirmative case. A nil Options map must not panic.
		{
			"one qualifying network, nil Options",
			[]network.Summary{bridgeNet("proj_a", true, nil)},
			ipv6BridgePresent, nil,
		},
		{
			"one qualifying network, empty Options",
			[]network.Summary{bridgeNet("proj_a", true, map[string]string{})},
			ipv6BridgePresent, nil,
		},
		{
			"one qualifying network, unrelated Options only",
			[]network.Summary{bridgeNet("proj_a", true, map[string]string{"com.docker.network.bridge.enable_icc": "true"})},
			ipv6BridgePresent, nil,
		},

		// A single qualifying network buried among non-qualifying ones. This
		// is the false-"none" direction: missing it would remove live rules.
		{
			"only one of many qualifies",
			[]network.Summary{
				bridgeNet(defaultBridgeNetwork, true, nil),
				{Name: "host", Driver: "host", EnableIPv6: true},
				bridgeNet("proj_a", false, nil),
				bridgeNet("proj_b", true, nil),
				{Name: "swarm_overlay", Driver: "overlay", EnableIPv6: true},
			},
			ipv6BridgePresent, nil,
		},
		{
			"qualifying network last in the list",
			[]network.Summary{bridgeNet("proj_a", false, nil), bridgeNet("proj_b", true, nil)},
			ipv6BridgePresent, nil,
		},
		{
			"qualifying network first in the list",
			[]network.Summary{bridgeNet("proj_a", true, nil), bridgeNet("proj_b", false, nil)},
			ipv6BridgePresent, nil,
		},
		{
			"several qualifying networks",
			[]network.Summary{bridgeNet("proj_a", true, nil), bridgeNet("proj_b", true, nil)},
			ipv6BridgePresent, nil,
		},

		// A custom interface name that br-+ cannot match is a real
		// disagreement between detection and reach: still "present", but
		// named so the caller can warn.
		{
			"custom iface not matching br-+",
			[]network.Summary{bridgeNet("proj_a", true, bridgeNameOpts("cs0"))},
			ipv6BridgePresent, []string{"proj_a (cs0)"},
		},
		{
			"custom iface merely containing br-",
			[]network.Summary{bridgeNet("proj_a", true, bridgeNameOpts("cs-br-0"))},
			ipv6BridgePresent, []string{"proj_a (cs-br-0)"},
		},

		// A custom name that does start with br- IS reachable and must not
		// be reported -- a false warning here would send an operator hunting
		// a non-problem.
		{
			"custom iface with the br- prefix is reachable",
			[]network.Summary{bridgeNet("proj_a", true, bridgeNameOpts("br-custom"))},
			ipv6BridgePresent, nil,
		},
		{
			"custom iface equal to the bare br- prefix is reachable",
			[]network.Summary{bridgeNet("proj_a", true, bridgeNameOpts("br-"))},
			ipv6BridgePresent, nil,
		},

		// Non-qualifying networks must never contribute a warning, however
		// unmatchable their interface name.
		{
			"IPv6-disabled bridge with a bad custom iface is not reported",
			[]network.Summary{bridgeNet("proj_a", false, bridgeNameOpts("cs0"))},
			ipv6BridgeNone, nil,
		},
		{
			"non-bridge driver with a bad custom iface is not reported",
			[]network.Summary{{Name: "lan6", Driver: "macvlan", EnableIPv6: true, Options: bridgeNameOpts("cs0")}},
			ipv6BridgeNone, nil,
		},

		// Every unreachable network must be collected, in list order. This
		// catches an implementation that returns as soon as it finds the
		// first qualifying network and so never sees the later warning.
		{
			"reachable network first, unreachable second",
			[]network.Summary{bridgeNet("proj_a", true, nil), bridgeNet("proj_b", true, bridgeNameOpts("cs1"))},
			ipv6BridgePresent, []string{"proj_b (cs1)"},
		},
		{
			"two unreachable networks are both collected in order",
			[]network.Summary{
				bridgeNet("proj_a", true, bridgeNameOpts("cs0")),
				bridgeNet("proj_b", true, nil),
				bridgeNet("proj_c", true, bridgeNameOpts("cs2")),
			},
			ipv6BridgePresent, []string{"proj_a (cs0)", "proj_c (cs2)"},
		},

		// A realistic full node listing: predefined networks, IPv4-only
		// project networks, and two IPv6 project networks, one of which
		// pins an unmatchable interface name.
		{
			"realistic node listing",
			[]network.Summary{
				bridgeNet(defaultBridgeNetwork, false, nil),
				{Name: "host", Driver: "host"},
				{Name: "none", Driver: "null"},
				bridgeNet("proj_1_net", false, nil),
				bridgeNet("proj_2_net", true, bridgeNameOpts("br-proj2")),
				bridgeNet("proj_3_net", true, bridgeNameOpts("legacy2")),
			},
			ipv6BridgePresent, []string{"proj_3_net (legacy2)"},
		},
	}

	for _, c := range cases {
		state, unreachable := classifyIPv6Bridges(c.nets)
		if state != c.expect {
			t.Errorf("%s: classifyIPv6Bridges state = %v, want %v", c.name, state, c.expect)
		}
		// classifyIPv6Bridges documents itself as never returning unknown:
		// only a failed docker query may produce that, and this function
		// never queries.
		if state == ipv6BridgeUnknown {
			t.Errorf("%s: classifyIPv6Bridges returned unknown from a pure list", c.name)
		}
		if len(unreachable) != len(c.unreachable) {
			t.Errorf("%s: unreachable = %v, want %v", c.name, unreachable, c.unreachable)
			continue
		}
		for i := range unreachable {
			if unreachable[i] != c.unreachable[i] {
				t.Errorf("%s: unreachable[%d] = %q, want %q", c.name, i, unreachable[i], c.unreachable[i])
			}
		}
	}
}

// classifyIPv6Bridges must not mutate the caller's slice or the Options maps
// inside it -- the list comes straight from the docker client and is logged by
// the caller afterwards.
func TestClassifyIPv6BridgesDoesNotMutateInput(t *testing.T) {
	nets := []network.Summary{
		bridgeNet("proj_a", true, bridgeNameOpts("cs0")),
		bridgeNet(defaultBridgeNetwork, true, nil),
	}
	classifyIPv6Bridges(nets)

	if len(nets) != 2 {
		t.Fatalf("input slice length changed: %d", len(nets))
	}
	if nets[0].Name != "proj_a" || nets[0].Driver != "bridge" || !nets[0].EnableIPv6 {
		t.Errorf("input network 0 mutated: %+v", nets[0])
	}
	if got := nets[0].Options[bridgeNameOption]; got != "cs0" {
		t.Errorf("input Options mutated: %s = %q, want %q", bridgeNameOption, got, "cs0")
	}
	if len(nets[0].Options) != 1 {
		t.Errorf("input Options gained or lost keys: %v", nets[0].Options)
	}
	if nets[1].Options != nil {
		t.Errorf("nil Options map was replaced: %v", nets[1].Options)
	}
}

// The state strings end up in operator-facing logs, and an unrecognised value
// must degrade to "unknown" (the safe, leave-the-kernel-alone reading) rather
// than to a state that would authorise adding or removing rules.
func TestIPv6BridgeStateString(t *testing.T) {
	cases := []struct {
		name   string
		state  ipv6BridgeState
		expect string
	}{
		{"unknown", ipv6BridgeUnknown, "unknown"},
		{"none", ipv6BridgeNone, "none"},
		{"present", ipv6BridgePresent, "present"},
		{"zero value is unknown", ipv6BridgeState(0), "unknown"},
		{"above range", ipv6BridgeState(3), "unknown"},
		{"far above range", ipv6BridgeState(99), "unknown"},
		{"negative", ipv6BridgeState(-1), "unknown"},
	}
	for _, c := range cases {
		if got := c.state.String(); got != c.expect {
			t.Errorf("%s: ipv6BridgeState(%d).String() = %q, want %q", c.name, int(c.state), got, c.expect)
		}
	}
}

// ConfigOnly networks are placeholders that hold configuration for other
// networks to inherit; docker cannot run containers on one, so it has no bridge
// interface on this host and no DOCKER-CT conntrack rule of its own. Counting
// one would make the agent's -m conntrack insert the FIRST IPv6 conntrack
// registration on the host -- the side effect gating on docker state exists to
// prevent. Deleting the ConfigOnly guard in classifyIPv6Bridges must fail here.
func TestClassifyIPv6BridgesIgnoresConfigOnly(t *testing.T) {
	cases := []struct {
		name   string
		nets   []network.Summary
		expect ipv6BridgeState
	}{
		{
			"config-only IPv6 bridge alone is not presence",
			[]network.Summary{{Name: "cfg", Driver: "bridge", EnableIPv6: true, ConfigOnly: true}},
			ipv6BridgeNone,
		},
		{
			"config-only alongside an IPv4-only real bridge is not presence",
			[]network.Summary{
				{Name: "cfg", Driver: "bridge", EnableIPv6: true, ConfigOnly: true},
				{Name: "proj_1", Driver: "bridge", EnableIPv6: false},
			},
			ipv6BridgeNone,
		},
		{
			"a real IPv6 bridge still counts when a config-only one is present",
			[]network.Summary{
				{Name: "cfg", Driver: "bridge", EnableIPv6: true, ConfigOnly: true},
				{Name: "proj_1", Driver: "bridge", EnableIPv6: true},
			},
			ipv6BridgePresent,
		},
		{
			"config-only listed after a real IPv6 bridge does not undo presence",
			[]network.Summary{
				{Name: "proj_1", Driver: "bridge", EnableIPv6: true},
				{Name: "cfg", Driver: "bridge", EnableIPv6: true, ConfigOnly: true},
			},
			ipv6BridgePresent,
		},
	}
	for _, c := range cases {
		got, unreachable := classifyIPv6Bridges(c.nets)
		if got != c.expect {
			t.Errorf("%s: classifyIPv6Bridges = %v, want %v", c.name, got, c.expect)
		}
		if len(unreachable) != 0 {
			t.Errorf("%s: unexpected unreachable %v", c.name, unreachable)
		}
	}
}

// A config-only network with an unmatchable custom bridge name must not be
// reported as unreachable either: it has no interface at all, so warning about
// its interface name would be noise about a network nothing was going to reach.
func TestClassifyIPv6BridgesConfigOnlyNotReportedUnreachable(t *testing.T) {
	nets := []network.Summary{
		{
			Name: "cfg", Driver: "bridge", EnableIPv6: true, ConfigOnly: true,
			Options: map[string]string{bridgeNameOption: "cs0"},
		},
	}
	got, unreachable := classifyIPv6Bridges(nets)
	if got != ipv6BridgeNone {
		t.Errorf("classifyIPv6Bridges = %v, want %v", got, ipv6BridgeNone)
	}
	if len(unreachable) != 0 {
		t.Errorf("config-only network reported as unreachable: %v", unreachable)
	}
}
