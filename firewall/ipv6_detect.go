package firewall

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/spf13/viper"
)

// Detection of IPv6-enabled docker bridge networks, which gates the ip6tables
// half of the cross-project isolation rules (see isolation.go).
//
// The isolation rules are applied to a family only where they can do something:
// on a node with no IPv6-enabled bridge network there is nothing for them to
// isolate, and installing them anyway would put an -m conntrack rule on a host
// that may have no IPv6 conntrack hooks registered at all -- which would start
// charging host IPv6 traffic against the same (family-agnostic, per-netns)
// nf_conntrack budget as IPv4. Where an IPv6-enabled bridge DOES exist, docker
// has already emitted its own per-network
//
//	DOCKER-CT -o <iface> -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
//
// so our rule adds no new registration and the delta is zero. That is what
// makes gating on docker state the right gate rather than a config toggle.

// ipv6BridgeState is the tri-state answer to "does this node have an
// IPv6-enabled docker bridge network?".
//
// The unknown case is load-bearing, not a nuisance value: treating "docker did
// not answer" as "no IPv6 bridges" would delete a live tenant-isolation control
// because dockerd was briefly unreachable. Only an affirmative, successful
// answer of zero may drive removal.
type ipv6BridgeState int

const (
	ipv6BridgeUnknown ipv6BridgeState = iota // docker unreachable / query failed
	ipv6BridgeNone                           // answered: zero IPv6-enabled bridges
	ipv6BridgePresent                        // answered: at least one
)

func (s ipv6BridgeState) String() string {
	switch s {
	case ipv6BridgeNone:
		return "none"
	case ipv6BridgePresent:
		return "present"
	default:
		return "unknown"
	}
}

// ipv6DetectTimeout bounds the docker query. The reconcile loop is serialized
// (firewall/reconciler.go), so an unbounded call against a wedged dockerd would
// reduce how often the IPv4 rules are reconciled. IPv4 is applied before this
// runs, so a hang cannot block the current apply -- only the next one.
const ipv6DetectTimeout = 5 * time.Second

// bridgeNameOption is docker's per-network override for the generated
// br-<netid> interface name.
const bridgeNameOption = "com.docker.network.bridge.name"

// defaultBridgeNetwork is docker's predefined bridge network. Its interface is
// docker0, which the br-+ wildcard in the isolation specs never matches, so
// counting it would install rules that cannot act on it.
const defaultBridgeNetwork = "bridge"

// classifyIPv6Bridges decides presence from an already-fetched network list. It
// is pure and never returns ipv6BridgeUnknown -- an empty list is a definite
// "none". The second return value names any counted network whose bridge
// interface will NOT match the br-+ wildcard the isolation rules use, so the
// caller can surface the disagreement between what we detect and what the rules
// can reach.
func classifyIPv6Bridges(nets []network.Summary) (ipv6BridgeState, []string) {
	state := ipv6BridgeNone
	var unreachable []string
	for _, n := range nets {
		if n.Driver != "bridge" || !n.EnableIPv6 || n.Name == defaultBridgeNetwork {
			continue
		}
		// A ConfigOnly network is a placeholder for configuration other networks
		// inherit; docker cannot run containers on it, so it has no bridge
		// interface on this host and no DOCKER-CT conntrack rule of its own.
		// Counting it would install our -m conntrack rule as the FIRST IPv6
		// conntrack registration on the host -- exactly the side effect gating on
		// docker state exists to avoid.
		if n.ConfigOnly {
			continue
		}
		state = ipv6BridgePresent
		// An empty value is treated as no override: docker should not emit one,
		// and reporting it would render as a bare "name ()" with nothing useful
		// in it.
		if name := n.Options[bridgeNameOption]; name != "" && !strings.HasPrefix(name, "br-") {
			unreachable = append(unreachable, n.Name+" ("+name+")")
		}
	}
	return state, unreachable
}

// ipv6BridgePresence fetches the docker network list and classifies it. Every
// error path -- no socket, permission denied, API mismatch, timeout -- returns
// ipv6BridgeUnknown, which leaves the kernel untouched.
func ipv6BridgePresence(ctx context.Context) ipv6BridgeState {
	cli, err := client.NewClientWithOpts(client.WithVersion(viper.GetString("docker.version")))
	if err != nil {
		// Debug, not Warn: this runs on every reconcile (60s), so a host where
		// docker is permanently unreachable would emit ~1440 identical lines a
		// day. The operator-visible signal is owned by the caller in
		// isolation.go, which reports the unknown state on transition and then
		// hourly; this line only carries the underlying cause for debugging.
		csFirewallLog().Debug("cannot reach docker to detect IPv6 bridge networks", "error", err.Error())
		return ipv6BridgeUnknown
	}
	defer cli.Close()

	qctx, cancel := context.WithTimeout(ctx, ipv6DetectTimeout)
	defer cancel()

	nets, err := cli.NetworkList(qctx, network.ListOptions{})
	if err != nil {
		// Debug for the same reason as above -- see the comment on the client
		// construction failure.
		csFirewallLog().Debug("docker network list failed", "error", err.Error())
		return ipv6BridgeUnknown
	}

	state, unreachable := classifyIPv6Bridges(nets)
	for _, n := range unreachable {
		warnUnreachableBridgeOnce(n)
	}
	return state
}

// warnUnreachableBridgeOnce logs at most one warning per network name per
// process. Reconcile runs every 60s, so an unconditional warning here would
// emit ~1440 identical lines a day.
var (
	warnedBridgesMu sync.Mutex
	warnedBridges   = map[string]bool{}
)

func warnUnreachableBridgeOnce(name string) {
	warnedBridgesMu.Lock()
	seen := warnedBridges[name]
	warnedBridges[name] = true
	warnedBridgesMu.Unlock()
	if seen {
		return
	}
	// Error, not Warn: this is not a degraded condition, it is an absent control.
	// The isolation specs match interfaces via the br-+ wildcard, so a bridge
	// named outside that prefix is not isolated from other projects at all, even
	// though the rules install cleanly and the post-condition reports healthy.
	// The same limitation applies to the IPv4 rules, which do not detect it.
	csFirewallLog().Error(
		"IPv6-enabled bridge network has a custom interface name the isolation rules cannot match; this network is NOT isolated",
		"network", name, "required-prefix", "br-",
	)
}
