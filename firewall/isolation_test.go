package firewall

import "testing"

// ruleIndex must match a rule only by its exact spec, so a decoy rule that
// merely shares tokens (e.g. another "br-+ ... DROP", or a different conntrack
// ctstate) is not mistaken for ours.
func TestRuleIndex(t *testing.T) {
	ret := "-A DOCKER-USER " + isoReturnSpec
	dnat := "-A DOCKER-USER " + isoDNATReturnSpec
	drop := "-A DOCKER-USER " + isoDropSpec

	cases := []struct {
		name   string
		lines  []string
		spec   string
		expect int
	}{
		{"empty chain (only -N)", []string{"-N DOCKER-USER"}, isoReturnSpec, -1},
		{"return present", []string{"-N DOCKER-USER", ret, dnat, drop}, isoReturnSpec, 1},
		{"dnat-return present", []string{"-N DOCKER-USER", ret, dnat, drop}, isoDNATReturnSpec, 2},
		{"drop present", []string{"-N DOCKER-USER", ret, dnat, drop}, isoDropSpec, 3},
		{"decoy drop with shared tokens does not match", []string{"-N DOCKER-USER", "-A DOCKER-USER -i br-+ -p tcp --dport 22 -j DROP"}, isoDropSpec, -1},
		{"decoy conntrack with different ctstate does not match", []string{"-N DOCKER-USER", "-A DOCKER-USER -i br-+ -o br-+ -m conntrack --ctstate ESTABLISHED -j RETURN"}, isoDNATReturnSpec, -1},
		{"bare docker RETURN is not the physdev RETURN", []string{"-N DOCKER-USER", "-A DOCKER-USER -j RETURN"}, isoReturnSpec, -1},
		{"dnat line does not match the drop spec", []string{"-N DOCKER-USER", dnat}, isoDropSpec, -1},
		{"drop line does not match the dnat spec", []string{"-N DOCKER-USER", drop}, isoDNATReturnSpec, -1},
		{"not present", []string{"-N DOCKER-USER", ret}, isoDropSpec, -1},
	}
	for _, c := range cases {
		if got := ruleIndex(c.lines, c.spec); got != c.expect {
			t.Errorf("%s: ruleIndex(_, %q) = %d, want %d", c.name, c.spec, got, c.expect)
		}
	}
}

// isolationOrdered (the fast-path "already correct" decision) must be true only
// when all three rules are present and ordered phys-RETURN < DNAT-RETURN < DROP,
// across every chain state the reconcile can encounter.
func TestIsolationOrderingDecision(t *testing.T) {
	ret := "-A DOCKER-USER " + isoReturnSpec
	dnat := "-A DOCKER-USER " + isoDNATReturnSpec
	drop := "-A DOCKER-USER " + isoDropSpec

	cases := []struct {
		name   string
		lines  []string
		expect bool
	}{
		{"ordered (three)", []string{"-N DOCKER-USER", ret, dnat, drop}, true},
		{"ordered with trailing docker RETURN", []string{"-N DOCKER-USER", ret, dnat, drop, "-A DOCKER-USER -j RETURN"}, true},
		{"fully reversed", []string{"-N DOCKER-USER", drop, dnat, ret}, false},
		{"dnat before phys-return", []string{"-N DOCKER-USER", dnat, ret, drop}, false},
		{"dnat after drop", []string{"-N DOCKER-USER", ret, drop, dnat}, false},
		{"dnat missing (two-rule, pre-upgrade)", []string{"-N DOCKER-USER", ret, drop}, false},
		{"phys-return missing", []string{"-N DOCKER-USER", dnat, drop}, false},
		{"drop missing", []string{"-N DOCKER-USER", ret, dnat}, false},
		{"only return", []string{"-N DOCKER-USER", ret}, false},
		{"only drop", []string{"-N DOCKER-USER", drop}, false},
		{"empty", []string{"-N DOCKER-USER"}, false},
	}
	for _, c := range cases {
		if got := isolationOrdered(c.lines); got != c.expect {
			t.Errorf("%s: isolationOrdered=%v, want %v", c.name, got, c.expect)
		}
	}
}
