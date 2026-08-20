package firewall

import (
	"github.com/spf13/viper"
)

// iptablesCmd selects the IPv4 iptables binary used by the cross-project
// isolation path in isolation.go (which still shells iptables in DOCKER-USER).
// The published-port path no longer shells iptables -- it renders the native
// cs_agent nftables table directly via netlink (nftable.go / nftable_apply.go)
// -- so the only remaining shell-out is isolation, now for both address
// families. The host.iptables-cmd toggle stays for that path.
//
// Both this and ip6tablesCmd deliberately return one of two compile-time
// constants rather than whatever the config file holds. The result is
// interpolated into a shell command line (see isoExec in isolation.go), so
// returning the configured string directly would make these keys a
// shell-injection vector from agent.yml. Keep the allow-list.
func iptablesCmd() string {
	if viper.GetString("host.iptables-cmd") == "iptables-legacy" {
		return "iptables-legacy"
	}
	return "iptables"
}

// ip6tablesCmd selects the IPv6 counterpart, used by the same isolation path
// when this node has an IPv6-enabled docker bridge network. The key is
// independent of host.iptables-cmd -- setting one does not imply the other.
func ip6tablesCmd() string {
	if viper.GetString("host.ip6tables-cmd") == "ip6tables-legacy" {
		return "ip6tables-legacy"
	}
	return "ip6tables"
}
