package firewall

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"
)

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

// ---------------------------------------------------------------------------
// Fixtures for the transcript / behavioural tests below.
//
// isoExec is the SINGLE shell-out boundary of the isolation path, so replacing
// it is what keeps this suite from touching a real firewall. That matters
// concretely: this package's tests may be run as root on a host with live
// DOCKER-USER rules, and deleteAllRules issues unconditional `-D` commands, so
// any path that escaped the fake would delete production isolation rules.
// Nothing below is allowed to reach a real iptables/ip6tables binary.
// ---------------------------------------------------------------------------

// errIsoExec stands in for a failed shell-out (lost xtables lock, missing
// chain, no ip6tables binary -- the isolation path only ever inspects err != nil).
var errIsoExec = errors.New("exit status 1")

type isoReply struct {
	out string
	err error
}

// isoFake records, in order, every literal command string handed to isoExec and
// answers from a script. Replies are keyed by the exact command; the last reply
// queued for a command repeats, which is what lets one entry serve both the
// chainExists probe and the contents read (they issue the identical command).
type isoFake struct {
	calls    []string
	replies  map[string][]isoReply
	failPref []string
}

func newIsoFake(replies map[string][]isoReply, failPref ...string) *isoFake {
	if replies == nil {
		replies = map[string][]isoReply{}
	}
	return &isoFake{replies: replies, failPref: failPref}
}

func (f *isoFake) exec(cmd string) ([]byte, error) {
	f.calls = append(f.calls, cmd)
	if q, ok := f.replies[cmd]; ok && len(q) > 0 {
		r := q[0]
		if len(q) > 1 {
			f.replies[cmd] = q[1:]
		}
		return []byte(r.out), r.err
	}
	for _, p := range f.failPref {
		if strings.HasPrefix(cmd, p) {
			return nil, errIsoExec
		}
	}
	// Unscripted commands succeed silently rather than erroring: every
	// transcript assertion below compares the FULL ordered call list, so a
	// command nobody expected still fails its test instead of hiding.
	return nil, nil
}

// forBin returns only the commands issued to one family's binary, so an IPv4
// transcript can be asserted independently of anything IPv6 did.
func (f *isoFake) forBin(bin string) []string {
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, bin+" ") {
			out = append(out, c)
		}
	}
	return out
}

// mutations returns the commands that change kernel state. Any command that is
// not an `-S` listing mutates something, so this is deliberately a denylist of
// one: a new read-only probe would show up here and have to be acknowledged.
func (f *isoFake) mutations() []string {
	var out []string
	for _, c := range f.calls {
		if !strings.Contains(c, " -S ") {
			out = append(out, c)
		}
	}
	return out
}

// resetIsoTestState restores both package-level seams (isoExec, ipv6PresenceFn)
// and clears the failure/reminder maps behind shouldCaptureIsoFailure and
// shouldRemindIsoFailure. It is the one reset helper, deferred by every test
// that touches package state:
//
//	defer resetIsoTestState()()
//
// Without it these cases would be order-dependent inside the package binary:
// isoFailedState latches, so a case that leaves a condition set would suppress
// the transition another case asserts.
func resetIsoTestState() func() {
	origExec, origPresence, origCapture := isoExec, ipv6PresenceFn, isoCapture
	clearIsoFailureState()
	return func() {
		isoExec, ipv6PresenceFn, isoCapture = origExec, origPresence, origCapture
		clearIsoFailureState()
	}
}

func clearIsoFailureState() {
	isoFailedMu.Lock()
	defer isoFailedMu.Unlock()
	isoFailedState = map[isoCondition]bool{}
	isoRemindedAt = map[isoCondition]time.Time{}
}

// latchedIsoConditions renders the currently latched failure conditions as a
// sorted "family/condition,..." string. A latched condition is exactly what
// suppresses the next Sentry capture, so asserting the whole set (rather than
// one key) also asserts that no OTHER condition was silently latched -- the
// masking failure mode 6f exists to prevent.
func latchedIsoConditions() string {
	isoFailedMu.Lock()
	defer isoFailedMu.Unlock()
	var out []string
	for c, failed := range isoFailedState {
		if failed {
			out = append(out, c.family+"/"+c.condition)
		}
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// seedIsoFailures pre-latches conditions given as "family/condition", so a test
// can prove a path leaves state untouched (a failed read) or clears it (chain
// absent, removal) rather than merely not setting it.
func seedIsoFailures(keys ...string) {
	isoFailedMu.Lock()
	defer isoFailedMu.Unlock()
	for _, k := range keys {
		family, condition, _ := strings.Cut(k, "/")
		isoFailedState[isoCondition{family: family, condition: condition}] = true
	}
}

// isoListing renders what `<bin> -S DOCKER-USER` prints for a chain holding
// specs in the given order.
func isoListing(specs ...string) string {
	lines := []string{"-N " + isoChain}
	for _, s := range specs {
		lines = append(lines, "-A "+isoChain+" "+s)
	}
	return strings.Join(lines, "\n")
}

// forwardListing renders `<bin> -S FORWARD`. withJump controls only whether the
// DOCKER-USER jump is among the jumps; the other entries stand in for the other
// daemons that write this chain.
func forwardListing(withJump bool) string {
	lines := []string{"-P FORWARD DROP"}
	if withJump {
		lines = append(lines, "-A FORWARD -j "+isoChain)
	}
	lines = append(lines, "-A FORWARD -j DOCKER-FORWARD", "-A FORWARD -j DOCKER-ISOLATION-STAGE-1")
	return strings.Join(lines, "\n")
}

func cmdRead(bin, chain string) string  { return bin + " -S " + chain }
func cmdDel(bin, spec string) string    { return bin + " -D " + isoChain + " " + spec }
func cmdInsert(bin, spec string) string { return bin + " -I " + isoChain + " 1 " + spec }

// steadyReads is the whole transcript of a healthy family: the chainExists
// probe, the FORWARD jump read, and the contents read. chainExists and the
// contents check issue the SAME command; that duplicate call is deliberate (a
// lost xtables lock must not read as "the chain is gone"), so it is pinned.
func steadyReads(bin string) []string {
	return []string{cmdRead(bin, isoChain), cmdRead(bin, isoForwardChain), cmdRead(bin, isoChain)}
}

func sameCalls(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func fmtCalls(c []string) string {
	if len(c) == 0 {
		return "\n\t(none)"
	}
	return "\n\t" + strings.Join(c, "\n\t")
}

// ---------------------------------------------------------------------------
// 7a. The golden command transcript.
// ---------------------------------------------------------------------------

// The IPv4 apply must be provably unchanged, so this case asserts the literal
// shell strings -- binary, flag spacing, spec text, `-I <chain> 1`, and the
// order of all ten commands -- against hardcoded text rather than against the
// isoReturnSpec/isoDropSpec constants the implementation itself uses. Deriving
// the expectation from those constants would let a change to a spec string move
// both sides together and still pass; this list cannot.
func TestIPv4ApplyGoldenTranscript(t *testing.T) {
	defer resetIsoTestState()()

	fake := newIsoFake(map[string][]isoReply{
		// Misordered contents (DROP first) on the probe, the contents read and
		// the post-condition read.
		"iptables -S DOCKER-USER": {{out: isoListing(isoDropSpec, isoDNATReturnSpec, isoReturnSpec)}},
		"iptables -S FORWARD":     {{out: forwardListing(true)}},
	}, "iptables -D ") // every delete errors on its first pass: one -D per spec
	isoExec = fake.exec

	want := []string{
		"iptables -S DOCKER-USER",
		"iptables -S FORWARD",
		"iptables -S DOCKER-USER",
		"iptables -D DOCKER-USER -m physdev --physdev-is-bridged -j RETURN",
		"iptables -D DOCKER-USER -i br-+ -o br-+ -m conntrack --ctstate DNAT -j RETURN",
		"iptables -D DOCKER-USER -i br-+ -o br-+ -j DROP",
		"iptables -I DOCKER-USER 1 -i br-+ -o br-+ -j DROP",
		"iptables -I DOCKER-USER 1 -i br-+ -o br-+ -m conntrack --ctstate DNAT -j RETURN",
		"iptables -I DOCKER-USER 1 -m physdev --physdev-is-bridged -j RETURN",
		"iptables -S DOCKER-USER",
	}

	ensureProjectIsolationFamily(isoFamily{name: "IPv4", bin: "iptables"})

	if !sameCalls(fake.calls, want) {
		t.Errorf("IPv4 golden apply transcript: got:%s\nwant:%s", fmtCalls(fake.calls), fmtCalls(want))
	}
}

// Per-family transcripts for every shape a reconcile can take. The steady-state
// case is the load-bearing one: it pins THREE reads and ZERO mutating commands.
// An implementation that dropped the isolationOrdered fast path would churn the
// live DROP (delete + re-insert) every 60 seconds on every node -- a few
// milliseconds of fail-open per node per minute, announced only by an Info log
// -- and only a zero-mutation assertion catches it.
func TestIsolationFamilyTranscripts(t *testing.T) {
	defer resetIsoTestState()()

	ordered := isoListing(isoReturnSpec, isoDNATReturnSpec, isoDropSpec)
	misordered := isoListing(isoDropSpec, isoDNATReturnSpec, isoReturnSpec)

	// Deletes derived from the script, never hardcoded: deleteAllRules loops to
	// maxIsolationCleanupPasses but returns on the first error, so a fake that
	// errors immediately yields one -D per spec, and a fake that never errors
	// yields exactly maxIsolationCleanupPasses per spec.
	oneDeletePass := func(bin string) []string {
		return []string{cmdDel(bin, isoReturnSpec), cmdDel(bin, isoDNATReturnSpec), cmdDel(bin, isoDropSpec)}
	}
	boundedDeletes := func(bin string) []string {
		var out []string
		for _, spec := range []string{isoReturnSpec, isoDNATReturnSpec, isoDropSpec} {
			for i := 0; i < maxIsolationCleanupPasses; i++ {
				out = append(out, cmdDel(bin, spec))
			}
		}
		return out
	}
	inserts := func(bin string) []string {
		return []string{cmdInsert(bin, isoDropSpec), cmdInsert(bin, isoDNATReturnSpec), cmdInsert(bin, isoReturnSpec)}
	}
	concat := func(parts ...[]string) []string {
		var out []string
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}

	cases := []struct {
		name     string
		fam      isoFamily
		replies  map[string][]isoReply
		failPref []string
		expect   []string
	}{
		// Steady state, both families. Three shell-outs, two distinct commands,
		// zero mutations.
		{
			name: "IPv4 steady state issues three reads and no mutation",
			fam:  isoFamily{name: "IPv4", bin: "iptables"},
			replies: map[string][]isoReply{
				"iptables -S DOCKER-USER": {{out: ordered}},
				"iptables -S FORWARD":     {{out: forwardListing(true)}},
			},
			expect: steadyReads("iptables"),
		},
		// Same code path with the other binary: pins that the family's bin is
		// threaded through chainExists, chainRules AND runIptables rather than
		// any of them falling back to iptablesCmd().
		{
			name: "IPv6 steady state uses ip6tables throughout",
			fam:  isoFamily{name: "IPv6", bin: "ip6tables"},
			replies: map[string][]isoReply{
				"ip6tables -S DOCKER-USER": {{out: ordered}},
				"ip6tables -S FORWARD":     {{out: forwardListing(true)}},
			},
			expect: steadyReads("ip6tables"),
		},
		// The jump is present but last. Presence-only, never an index check --
		// another daemon may rewrite FORWARD -- so this is still steady state.
		{
			name: "jump present but not first is still steady state",
			fam:  isoFamily{name: "IPv4", bin: "iptables"},
			replies: map[string][]isoReply{
				"iptables -S DOCKER-USER": {{out: ordered}},
				"iptables -S FORWARD": {{out: strings.Join([]string{
					"-P FORWARD DROP",
					"-A FORWARD -j DOCKER-FORWARD",
					"-A FORWARD -j SOME-OTHER-DAEMON",
					"-A FORWARD -j " + isoChain,
				}, "\n")}},
			},
			expect: steadyReads("iptables"),
		},
		// Chain absent: exactly the one probe, nothing else. An implementation
		// that read FORWARD or the contents before probing would fail here.
		{
			name: "chain absent issues exactly one probe",
			fam:  isoFamily{name: "IPv4", bin: "iptables"},
			replies: map[string][]isoReply{
				"iptables -S DOCKER-USER": {{err: errIsoExec}},
			},
			expect: []string{cmdRead("iptables", isoChain)},
		},
		// No ip6tables binary at all (or IPv6 disabled at the kernel): the
		// shell-out fails, chainExists is false, one probe and out. R10.
		{
			name:     "no ip6tables binary is one probe and no mutation",
			fam:      isoFamily{name: "IPv6", bin: "ip6tables"},
			failPref: []string{"ip6tables "},
			expect:   []string{cmdRead("ip6tables", isoChain)},
		},
		// The jump is missing while the contents are correct. This must NOT
		// trigger an apply: the jump is detection-only and is never repaired.
		{
			name: "missing jump with correct contents mutates nothing",
			fam:  isoFamily{name: "IPv4", bin: "iptables"},
			replies: map[string][]isoReply{
				"iptables -S DOCKER-USER": {{out: ordered}},
				"iptables -S FORWARD":     {{out: forwardListing(false)}},
			},
			expect: steadyReads("iptables"),
		},
		// A failed FORWARD read is not a verdict and not a reason to apply: the
		// contents were fine, so the transcript is still the steady three.
		{
			name: "failed FORWARD read still evaluates the contents and mutates nothing",
			fam:  isoFamily{name: "IPv4", bin: "iptables"},
			replies: map[string][]isoReply{
				"iptables -S DOCKER-USER": {{out: ordered}},
				"iptables -S FORWARD":     {{err: errIsoExec}},
			},
			expect: steadyReads("iptables"),
		},
		// A failed contents read must never be mistaken for "rules missing":
		// isolationOrdered(nil) is false, so an implementation that swallowed
		// the error would delete and re-insert a correct chain here.
		{
			name: "failed contents read mutates nothing",
			fam:  isoFamily{name: "IPv4", bin: "iptables"},
			replies: map[string][]isoReply{
				"iptables -S DOCKER-USER": {{out: ordered}, {err: errIsoExec}},
				"iptables -S FORWARD":     {{out: forwardListing(true)}},
			},
			expect: steadyReads("iptables"),
		},
		// Unordered contents, deletes error on the first pass: three deletes,
		// then the three inserts in DROP -> DNAT-RETURN -> phys-RETURN order
		// each at position 1, then the post-condition read.
		{
			name: "unordered contents applies deletes then reverse-order inserts",
			fam:  isoFamily{name: "IPv4", bin: "iptables"},
			replies: map[string][]isoReply{
				"iptables -S DOCKER-USER": {{out: misordered}, {out: misordered}, {out: ordered}},
				"iptables -S FORWARD":     {{out: forwardListing(true)}},
			},
			failPref: []string{"iptables -D "},
			expect: concat(steadyReads("iptables"), oneDeletePass("iptables"), inserts("iptables"),
				[]string{cmdRead("iptables", isoChain)}),
		},
		// One stray copy of each spec: the delete loop must keep going until -D
		// errors, so two passes per spec. An implementation that issued a single
		// delete per spec would leave the stray behind and pass the previous
		// case; it fails this one.
		{
			name: "delete loop drains a stray copy of each spec",
			fam:  isoFamily{name: "IPv4", bin: "iptables"},
			replies: map[string][]isoReply{
				"iptables -S DOCKER-USER":             {{out: misordered}, {out: misordered}, {out: ordered}},
				"iptables -S FORWARD":                 {{out: forwardListing(true)}},
				cmdDel("iptables", isoReturnSpec):     {{}, {err: errIsoExec}},
				cmdDel("iptables", isoDNATReturnSpec): {{}, {err: errIsoExec}},
				cmdDel("iptables", isoDropSpec):       {{}, {err: errIsoExec}},
			},
			expect: concat(steadyReads("iptables"),
				[]string{
					cmdDel("iptables", isoReturnSpec), cmdDel("iptables", isoReturnSpec),
					cmdDel("iptables", isoDNATReturnSpec), cmdDel("iptables", isoDNATReturnSpec),
					cmdDel("iptables", isoDropSpec), cmdDel("iptables", isoDropSpec),
				},
				inserts("iptables"), []string{cmdRead("iptables", isoChain)}),
		},
		// Pathological chain where -D never errors: the loop must stay bounded
		// by maxIsolationCleanupPasses per spec, not run forever.
		{
			name: "delete loop is bounded when -D never errors",
			fam:  isoFamily{name: "IPv4", bin: "iptables"},
			replies: map[string][]isoReply{
				"iptables -S DOCKER-USER": {{out: misordered}, {out: misordered}, {out: ordered}},
				"iptables -S FORWARD":     {{out: forwardListing(true)}},
			},
			expect: concat(steadyReads("iptables"), boundedDeletes("iptables"), inserts("iptables"),
				[]string{cmdRead("iptables", isoChain)}),
		},
		// A failed post-condition read ends the apply; nothing is retried.
		{
			name: "failed post-condition read ends the apply",
			fam:  isoFamily{name: "IPv4", bin: "iptables"},
			replies: map[string][]isoReply{
				"iptables -S DOCKER-USER": {{out: misordered}, {out: misordered}, {err: errIsoExec}},
				"iptables -S FORWARD":     {{out: forwardListing(true)}},
			},
			failPref: []string{"iptables -D "},
			expect: concat(steadyReads("iptables"), oneDeletePass("iptables"), inserts("iptables"),
				[]string{cmdRead("iptables", isoChain)}),
		},
		// Every insert fails (missing xt_physdev / xt_conntrack): all three are
		// still attempted -- one failure must not abort the rest, or a failed
		// RETURN would take the DROP down with it -- and the post-condition
		// still runs.
		{
			name: "all three inserts are attempted even when each fails",
			fam:  isoFamily{name: "IPv4", bin: "iptables"},
			replies: map[string][]isoReply{
				"iptables -S DOCKER-USER": {{out: misordered}},
				"iptables -S FORWARD":     {{out: forwardListing(true)}},
			},
			failPref: []string{"iptables -D ", "iptables -I "},
			expect: concat(steadyReads("iptables"), oneDeletePass("iptables"), inserts("iptables"),
				[]string{cmdRead("iptables", isoChain)}),
		},
	}

	for _, c := range cases {
		clearIsoFailureState()
		fake := newIsoFake(c.replies, c.failPref...)
		isoExec = fake.exec
		ensureProjectIsolationFamily(c.fam)
		if !sameCalls(fake.calls, c.expect) {
			t.Errorf("%s: transcript got:%s\nwant:%s", c.name, fmtCalls(fake.calls), fmtCalls(c.expect))
		}
	}
}

// Transcripts for the removal path (the D5/D6 rollback, and the only way a v6
// rule ever comes back off a node). The empty-chain case is the one that runs
// every 60 seconds forever on every node with no IPv6 bridge: it must issue
// nothing.
func TestRemoveIsolationFamilyTranscripts(t *testing.T) {
	defer resetIsoTestState()()

	cases := []struct {
		name     string
		fam      isoFamily
		replies  map[string][]isoReply
		failPref []string
		expect   []string
	}{
		{
			name: "empty chain is a genuine no-op",
			fam:  isoFamily{name: "IPv6", bin: "ip6tables"},
			replies: map[string][]isoReply{
				"ip6tables -S DOCKER-USER": {{out: isoListing()}},
			},
			expect: []string{cmdRead("ip6tables", isoChain), cmdRead("ip6tables", isoChain)},
		},
		{
			name:     "absent chain is a no-op with a single probe",
			fam:      isoFamily{name: "IPv6", bin: "ip6tables"},
			failPref: []string{"ip6tables "},
			expect:   []string{cmdRead("ip6tables", isoChain)},
		},
		{
			name: "failed read leaves the chain alone",
			fam:  isoFamily{name: "IPv6", bin: "ip6tables"},
			replies: map[string][]isoReply{
				"ip6tables -S DOCKER-USER": {{out: isoListing(isoReturnSpec)}, {err: errIsoExec}},
			},
			expect: []string{cmdRead("ip6tables", isoChain), cmdRead("ip6tables", isoChain)},
		},
		// Our three rules present: deleted unconditionally, in spec order, and
		// no post-condition read -- an absent rule set is the intended state.
		{
			name: "populated chain deletes all three specs and never re-reads",
			fam:  isoFamily{name: "IPv6", bin: "ip6tables"},
			replies: map[string][]isoReply{
				"ip6tables -S DOCKER-USER": {{out: isoListing(isoReturnSpec, isoDNATReturnSpec, isoDropSpec)}},
			},
			failPref: []string{"ip6tables -D "},
			expect: []string{
				cmdRead("ip6tables", isoChain), cmdRead("ip6tables", isoChain),
				cmdDel("ip6tables", isoReturnSpec), cmdDel("ip6tables", isoDNATReturnSpec), cmdDel("ip6tables", isoDropSpec),
			},
		},
		// A non-empty chain holding somebody else's rules: deletes are still
		// issued, because -D matches semantically and a text-match guard here
		// would silently disable the only rollback path there is.
		{
			name: "non-empty chain without our rules still issues the deletes",
			fam:  isoFamily{name: "IPv6", bin: "ip6tables"},
			replies: map[string][]isoReply{
				"ip6tables -S DOCKER-USER": {{out: isoListing("-i br-+ -o br-+ -j ACCEPT")}},
			},
			failPref: []string{"ip6tables -D "},
			expect: []string{
				cmdRead("ip6tables", isoChain), cmdRead("ip6tables", isoChain),
				cmdDel("ip6tables", isoReturnSpec), cmdDel("ip6tables", isoDNATReturnSpec), cmdDel("ip6tables", isoDropSpec),
			},
		},
	}

	for _, c := range cases {
		clearIsoFailureState()
		fake := newIsoFake(c.replies, c.failPref...)
		isoExec = fake.exec
		removeProjectIsolationFamily(c.fam)
		if !sameCalls(fake.calls, c.expect) {
			t.Errorf("%s: transcript got:%s\nwant:%s", c.name, fmtCalls(fake.calls), fmtCalls(c.expect))
		}
	}
}

// ---------------------------------------------------------------------------
// 7b. Behavioural cases.
// ---------------------------------------------------------------------------

// The two-family orchestration. wantV4 is asserted against the SAME
// steadyReads() list in every case, whatever detection says and whatever the
// IPv6 path then does -- that literal equality is the "IPv4 is unchanged by the
// presence of IPv6" claim. The `unknown` case is the important one: it must
// issue no ip6tables command whatsoever, because treating a dockerd blip as
// "no IPv6 bridges" would delete a live tenant-isolation control.
func TestEnsureProjectIsolationDetectionBranches(t *testing.T) {
	defer resetIsoTestState()()

	v4, v6 := iptablesCmd(), ip6tablesCmd()
	ordered := isoListing(isoReturnSpec, isoDNATReturnSpec, isoDropSpec)

	cases := []struct {
		name        string
		state       ipv6BridgeState
		v6Chain     string
		seed        []string
		wantV6      []string
		wantLatched string
	}{
		{
			name:    "present applies the IPv6 family",
			state:   ipv6BridgePresent,
			v6Chain: ordered,
			wantV6:  steadyReads(v6),
		},
		{
			name:    "none removes the IPv6 rules and mutates no IPv4",
			state:   ipv6BridgeNone,
			v6Chain: isoListing(),
			wantV6:  []string{cmdRead(v6, isoChain), cmdRead(v6, isoChain)},
		},
		// R8: docker did not answer. Not one ip6tables command, and the IPv6
		// failure state is left exactly as it was.
		{
			name:        "unknown issues no ip6tables command and leaves IPv6 state alone",
			state:       ipv6BridgeUnknown,
			v6Chain:     ordered,
			seed:        []string{"IPv6/contents"},
			wantV6:      nil,
			wantLatched: "IPv6/contents,IPv6/detection",
		},
	}

	for _, c := range cases {
		clearIsoFailureState()
		seedIsoFailures(c.seed...)
		fake := newIsoFake(map[string][]isoReply{
			cmdRead(v4, isoChain):        {{out: ordered}},
			cmdRead(v4, isoForwardChain): {{out: forwardListing(true)}},
			cmdRead(v6, isoChain):        {{out: c.v6Chain}},
			cmdRead(v6, isoForwardChain): {{out: forwardListing(true)}},
		})
		isoExec = fake.exec

		// Detection must run AFTER the whole IPv4 apply: it is the one call in
		// this path that can hang (a wedged dockerd), and IPv4 is the control
		// protecting tenants today.
		detections, callsBeforeDetect := 0, -1
		ipv6PresenceFn = func(context.Context) ipv6BridgeState {
			detections++
			callsBeforeDetect = len(fake.calls)
			return c.state
		}

		ensureProjectIsolation(context.Background())

		if !sameCalls(fake.forBin(v4), steadyReads(v4)) {
			t.Errorf("%s: IPv4 transcript got:%s\nwant:%s", c.name, fmtCalls(fake.forBin(v4)), fmtCalls(steadyReads(v4)))
		}
		if !sameCalls(fake.forBin(v6), c.wantV6) {
			t.Errorf("%s: IPv6 transcript got:%s\nwant:%s", c.name, fmtCalls(fake.forBin(v6)), fmtCalls(c.wantV6))
		}
		if len(fake.mutations()) != 0 {
			t.Errorf("%s: steady state issued mutating commands:%s", c.name, fmtCalls(fake.mutations()))
		}
		if detections != 1 {
			t.Errorf("%s: detection called %d times, want 1", c.name, detections)
		}
		if callsBeforeDetect != len(steadyReads(v4)) {
			t.Errorf("%s: %d commands issued before detection, want the %d IPv4 reads (IPv4 must not wait on docker)",
				c.name, callsBeforeDetect, len(steadyReads(v4)))
		}
		if got := latchedIsoConditions(); got != c.wantLatched {
			t.Errorf("%s: latched conditions = %q, want %q", c.name, got, c.wantLatched)
		}
	}
}

// R3: neither family may be skipped because the other failed. There is no
// shared error path and no early return between them, in either direction.
func TestOneFamilyFailureDoesNotSkipTheOther(t *testing.T) {
	defer resetIsoTestState()()

	v4, v6 := iptablesCmd(), ip6tablesCmd()
	ordered := isoListing(isoReturnSpec, isoDNATReturnSpec, isoDropSpec)
	misordered := isoListing(isoDropSpec, isoDNATReturnSpec, isoReturnSpec)

	cases := []struct {
		name        string
		replies     map[string][]isoReply
		failPref    []string
		wantV4      []string
		wantV6      []string
		wantLatched string
	}{
		// IPv4 has no DOCKER-USER chain at all; IPv6 must still be reconciled.
		{
			name: "IPv4 chain absent does not stop the IPv6 family",
			replies: map[string][]isoReply{
				cmdRead(v4, isoChain):        {{err: errIsoExec}},
				cmdRead(v6, isoChain):        {{out: ordered}},
				cmdRead(v6, isoForwardChain): {{out: forwardListing(true)}},
			},
			wantV4:      []string{cmdRead(v4, isoChain)},
			wantV6:      steadyReads(v6),
			wantLatched: "IPv4/chain-absent",
		},
		// The mirror: a totally broken IPv6 family (no binary) leaves the IPv4
		// transcript untouched. R10.
		{
			name: "missing ip6tables binary leaves IPv4 untouched",
			replies: map[string][]isoReply{
				cmdRead(v4, isoChain):        {{out: ordered}},
				cmdRead(v4, isoForwardChain): {{out: forwardListing(true)}},
			},
			failPref:    []string{v6 + " "},
			wantV4:      steadyReads(v4),
			wantV6:      []string{cmdRead(v6, isoChain)},
			wantLatched: "IPv6/chain-absent",
		},
		// An IPv6 apply that fails outright still leaves IPv4 in steady state
		// and pages only for IPv6.
		{
			name: "failing IPv6 apply leaves IPv4 in steady state",
			replies: map[string][]isoReply{
				cmdRead(v4, isoChain):        {{out: ordered}},
				cmdRead(v4, isoForwardChain): {{out: forwardListing(true)}},
				cmdRead(v6, isoChain):        {{out: misordered}},
				cmdRead(v6, isoForwardChain): {{out: forwardListing(true)}},
			},
			failPref:    []string{v6 + " -D ", v6 + " -I "},
			wantV4:      steadyReads(v4),
			wantLatched: "IPv6/contents",
		},
	}

	for _, c := range cases {
		clearIsoFailureState()
		fake := newIsoFake(c.replies, c.failPref...)
		isoExec = fake.exec
		ipv6PresenceFn = func(context.Context) ipv6BridgeState { return ipv6BridgePresent }

		ensureProjectIsolation(context.Background())

		if !sameCalls(fake.forBin(v4), c.wantV4) {
			t.Errorf("%s: IPv4 transcript got:%s\nwant:%s", c.name, fmtCalls(fake.forBin(v4)), fmtCalls(c.wantV4))
		}
		if c.wantV6 != nil && !sameCalls(fake.forBin(v6), c.wantV6) {
			t.Errorf("%s: IPv6 transcript got:%s\nwant:%s", c.name, fmtCalls(fake.forBin(v6)), fmtCalls(c.wantV6))
		}
		if len(fake.forBin(v6)) == 0 {
			t.Errorf("%s: the IPv6 family was never attempted", c.name)
		}
		if got := latchedIsoConditions(); got != c.wantLatched {
			t.Errorf("%s: latched conditions = %q, want %q", c.name, got, c.wantLatched)
		}
	}
}

// Which conditions each path latches, which it clears, and which it leaves
// alone. The latch is what suppresses the next Sentry capture, so this table is
// the pager's behaviour: asserting the whole set catches both flooding (a
// condition that never latches) and masking (a condition latched by the wrong
// path, which then swallows a real page).
//
// The "jump missing while the contents are correct" case is the one the earlier
// design could not reach at all: with the jump check below the isolationOrdered
// fast-path return, this node takes that return forever and the check fires
// only when the contents are ALSO wrong -- i.e. only when it is redundant.
func TestIsolationFailureStateTransitions(t *testing.T) {
	defer resetIsoTestState()()

	ordered := isoListing(isoReturnSpec, isoDNATReturnSpec, isoDropSpec)
	misordered := isoListing(isoDropSpec, isoDNATReturnSpec, isoReturnSpec)

	cases := []struct {
		name        string
		replies     map[string][]isoReply
		failPref    []string
		seed        []string
		wantLatched string
	}{
		{
			name: "steady state latches nothing and clears a stale latch",
			seed: []string{"IPv4/contents", "IPv4/forward-jump", "IPv4/chain-absent"},
			replies: map[string][]isoReply{
				"iptables -S DOCKER-USER": {{out: ordered}},
				"iptables -S FORWARD":     {{out: forwardListing(true)}},
			},
			wantLatched: "",
		},
		{
			name: "missing jump latches only the jump, with the contents correct",
			replies: map[string][]isoReply{
				"iptables -S DOCKER-USER": {{out: ordered}},
				"iptables -S FORWARD":     {{out: forwardListing(false)}},
			},
			wantLatched: "IPv4/forward-jump",
		},
		// Presence, never an index: another daemon may rewrite FORWARD on its
		// own schedule and put its own jumps above ours. An index assertion
		// would page on every node that runs one, which is a false page for a
		// chain that evaluates DOCKER-USER perfectly well.
		{
			name: "a jump anywhere in FORWARD latches nothing",
			seed: []string{"IPv4/forward-jump"},
			replies: map[string][]isoReply{
				"iptables -S DOCKER-USER": {{out: ordered}},
				"iptables -S FORWARD": {{out: strings.Join([]string{
					"-P FORWARD DROP",
					"-A FORWARD -j DOCKER-FORWARD",
					"-A FORWARD -j SOME-OTHER-DAEMON",
					"-A FORWARD -j " + isoChain,
				}, "\n")}},
			},
			wantLatched: "",
		},
		{
			name: "chain absent latches chain-absent and clears the rule verdicts",
			seed: []string{"IPv4/contents", "IPv4/forward-jump"},
			replies: map[string][]isoReply{
				"iptables -S DOCKER-USER": {{err: errIsoExec}},
			},
			wantLatched: "IPv4/chain-absent",
		},
		// R7, read side one: a lost xtables lock on the FORWARD read is not a
		// verdict. It must neither latch the jump nor clear a latched one.
		{
			name: "failed FORWARD read leaves a latched jump alone",
			seed: []string{"IPv4/forward-jump"},
			replies: map[string][]isoReply{
				"iptables -S DOCKER-USER": {{out: ordered}},
				"iptables -S FORWARD":     {{err: errIsoExec}},
			},
			wantLatched: "IPv4/forward-jump",
		},
		{
			name: "failed FORWARD read does not latch the jump",
			replies: map[string][]isoReply{
				"iptables -S DOCKER-USER": {{out: ordered}},
				"iptables -S FORWARD":     {{err: errIsoExec}},
			},
			wantLatched: "",
		},
		// R7, read side two: the contents read. Without this the flake pages
		// for a chain that is correct.
		{
			name: "failed contents read leaves a latched contents verdict alone",
			seed: []string{"IPv4/contents"},
			replies: map[string][]isoReply{
				"iptables -S DOCKER-USER": {{out: ordered}, {err: errIsoExec}},
				"iptables -S FORWARD":     {{out: forwardListing(true)}},
			},
			wantLatched: "IPv4/contents",
		},
		{
			name: "failed contents read does not latch the contents verdict",
			replies: map[string][]isoReply{
				"iptables -S DOCKER-USER": {{out: ordered}, {err: errIsoExec}},
				"iptables -S FORWARD":     {{out: forwardListing(true)}},
			},
			wantLatched: "",
		},
		{
			name: "failed post-condition read does not latch the contents verdict",
			replies: map[string][]isoReply{
				"iptables -S DOCKER-USER": {{out: misordered}, {out: misordered}, {err: errIsoExec}},
				"iptables -S FORWARD":     {{out: forwardListing(true)}},
			},
			failPref:    []string{"iptables -D "},
			wantLatched: "",
		},
		{
			name: "still unordered after the apply latches the contents verdict",
			replies: map[string][]isoReply{
				"iptables -S DOCKER-USER": {{out: misordered}},
				"iptables -S FORWARD":     {{out: forwardListing(true)}},
			},
			failPref:    []string{"iptables -D ", "iptables -I "},
			wantLatched: "IPv4/contents",
		},
		{
			name: "a successful apply latches nothing",
			replies: map[string][]isoReply{
				"iptables -S DOCKER-USER": {{out: misordered}, {out: misordered}, {out: ordered}},
				"iptables -S FORWARD":     {{out: forwardListing(true)}},
			},
			failPref:    []string{"iptables -D "},
			wantLatched: "",
		},
		// Both conditions can fail at once and must latch independently: one
		// bool per family would let the jump verdict swallow the contents page.
		{
			name: "a missing jump and an unordered chain latch independently",
			replies: map[string][]isoReply{
				"iptables -S DOCKER-USER": {{out: misordered}},
				"iptables -S FORWARD":     {{out: forwardListing(false)}},
			},
			failPref:    []string{"iptables -D ", "iptables -I "},
			wantLatched: "IPv4/contents,IPv4/forward-jump",
		},
	}

	for _, c := range cases {
		clearIsoFailureState()
		seedIsoFailures(c.seed...)
		fake := newIsoFake(c.replies, c.failPref...)
		isoExec = fake.exec
		ensureProjectIsolationFamily(isoFamily{name: "IPv4", bin: "iptables"})
		if got := latchedIsoConditions(); got != c.wantLatched {
			t.Errorf("%s: latched conditions = %q, want %q", c.name, got, c.wantLatched)
		}
	}
}

// A missing chain warns once, not 1440 times a day, and recovers: the latch
// must clear when the chain comes back so a later disappearance warns again.
// A latched condition surviving the second pass is what proves the warning was
// rate-limited to the transition; nothing else in the process observes it.
func TestChainAbsentWarnsOnceAndRecovers(t *testing.T) {
	defer resetIsoTestState()()

	fam := isoFamily{name: "IPv4", bin: "iptables"}
	absent := map[string][]isoReply{"iptables -S DOCKER-USER": {{err: errIsoExec}}}
	present := map[string][]isoReply{
		"iptables -S DOCKER-USER": {{out: isoListing(isoReturnSpec, isoDNATReturnSpec, isoDropSpec)}},
		"iptables -S FORWARD":     {{out: forwardListing(true)}},
	}

	run := func(replies map[string][]isoReply) *isoFake {
		fake := newIsoFake(replies)
		isoExec = fake.exec
		ensureProjectIsolationFamily(fam)
		return fake
	}

	first := run(absent)
	if got := latchedIsoConditions(); got != "IPv4/chain-absent" {
		t.Errorf("first absent pass: latched %q, want %q", got, "IPv4/chain-absent")
	}
	if len(first.mutations()) != 0 {
		t.Errorf("first absent pass: issued mutating commands:%s", fmtCalls(first.mutations()))
	}

	second := run(map[string][]isoReply{"iptables -S DOCKER-USER": {{err: errIsoExec}}})
	if got := latchedIsoConditions(); got != "IPv4/chain-absent" {
		t.Errorf("second absent pass: latched %q, want the condition to stay latched (warning rate-limited to the transition)", got)
	}
	if len(second.calls) != 1 {
		t.Errorf("second absent pass: transcript%s, want a single probe", fmtCalls(second.calls))
	}

	run(present)
	if got := latchedIsoConditions(); got != "" {
		t.Errorf("after the chain returned: latched %q, want nothing", got)
	}

	run(absent)
	if got := latchedIsoConditions(); got != "IPv4/chain-absent" {
		t.Errorf("after the chain went away again: latched %q, want %q (a second disappearance must warn again)", got, "IPv4/chain-absent")
	}
}

// D5/D6: removal resets the family's verdicts, so a later genuine failure is
// still a false -> true transition and still pages. Without the reset, a family
// that latched `contents` and then took the removal path would never page again
// -- and removal is the only rollback path there is, so it is a path real nodes
// take. The latch being empty after removal and true after the failing apply is
// the transition, which is otherwise unobservable in-process.
func TestRemovalResetsStateSoALaterFailureStillPages(t *testing.T) {
	defer resetIsoTestState()()

	v6 := isoFamily{name: "IPv6", bin: "ip6tables"}

	// All three removal exits must clear the verdicts, not just the one a test
	// happens to walk: the early "chain absent" return, the early "empty chain"
	// return (the 60s steady state), and the path that actually deletes rules.
	exits := []struct {
		name     string
		replies  map[string][]isoReply
		failPref []string
	}{
		{
			name:     "chain absent",
			failPref: []string{"ip6tables "},
		},
		{
			name:    "empty chain",
			replies: map[string][]isoReply{"ip6tables -S DOCKER-USER": {{out: isoListing()}}},
		},
		{
			name: "rules deleted",
			replies: map[string][]isoReply{
				"ip6tables -S DOCKER-USER": {{out: isoListing(isoReturnSpec, isoDNATReturnSpec, isoDropSpec)}},
			},
			failPref: []string{"ip6tables -D "},
		},
	}

	for _, e := range exits {
		clearIsoFailureState()
		seedIsoFailures("IPv6/contents", "IPv6/forward-jump")
		fake := newIsoFake(e.replies, e.failPref...)
		isoExec = fake.exec
		removeProjectIsolationFamily(v6)
		if got := latchedIsoConditions(); got != "" {
			t.Errorf("removal via %s: latched %q, want nothing (a later failure must still be a transition)", e.name, got)
		}

		// The transition itself is not observable in-process, so this pair is
		// the proof: empty after removal, latched after the failing apply, so a
		// false -> true edge -- and therefore a capture -- necessarily happened.
		fake = newIsoFake(map[string][]isoReply{
			"ip6tables -S DOCKER-USER": {{out: isoListing(isoDropSpec)}},
			"ip6tables -S FORWARD":     {{out: forwardListing(true)}},
		}, "ip6tables -D ", "ip6tables -I ")
		isoExec = fake.exec
		ensureProjectIsolationFamily(v6)
		if got := latchedIsoConditions(); got != "IPv6/contents" {
			t.Errorf("genuine failure after removal via %s: latched %q, want %q", e.name, got, "IPv6/contents")
		}
	}
}

// shouldCaptureIsoFailure is the pager's rate limiter, tested directly as a
// sequence: Reconcile runs every 60s, so it must capture on the false -> true
// edge only, and it must key on (family, condition) so no condition can swallow
// another's page.
func TestShouldCaptureIsoFailure(t *testing.T) {
	defer resetIsoTestState()()

	v4Contents := isoCondition{family: "IPv4", condition: isoCondContents}
	v4Jump := isoCondition{family: "IPv4", condition: isoCondForwardJump}
	v6Contents := isoCondition{family: "IPv6", condition: isoCondContents}

	steps := []struct {
		name   string
		cond   isoCondition
		failed bool
		expect bool
	}{
		{"first failure captures", v4Contents, true, true},
		{"repeat failure does not capture", v4Contents, true, false},
		{"third failure still does not capture", v4Contents, true, false},
		{"a different condition in the same family captures on its own", v4Jump, true, true},
		{"the same condition in another family captures on its own", v6Contents, true, true},
		{"clearing never captures", v4Contents, false, false},
		{"clearing again never captures", v4Contents, false, false},
		{"a failure after a clear captures again", v4Contents, true, true},
		{"the other condition is unaffected by that", v4Jump, true, false},
		{"clearing one condition does not clear its sibling", v4Jump, false, false},
		{"the sibling is still latched", v4Contents, true, false},
	}
	for _, s := range steps {
		if got := shouldCaptureIsoFailure(s.cond, s.failed); got != s.expect {
			t.Errorf("%s: shouldCaptureIsoFailure(%v, %v) = %v, want %v", s.name, s.cond, s.failed, got, s.expect)
		}
	}
}

// R8's hourly reminder. A node whose docker query fails permanently (socket
// permissions, a non-default socket path) would otherwise log once and go quiet
// forever with the IPv6 control absent and no way to tell that apart from
// "intentionally off" -- but the reminder must not fire per reconcile either.
func TestShouldRemindIsoFailure(t *testing.T) {
	defer resetIsoTestState()()

	c := isoCondition{family: "IPv6", condition: isoCondDetection}

	if !shouldRemindIsoFailure(c, time.Hour) {
		t.Errorf("first reminder: got false, want true (an unresolved condition must be reported at least once)")
	}
	if shouldRemindIsoFailure(c, time.Hour) {
		t.Errorf("immediate second reminder: got true, want false (Reconcile runs every 60s)")
	}

	// Age the stamp past the interval.
	isoFailedMu.Lock()
	isoRemindedAt[c] = time.Now().Add(-2 * time.Hour)
	isoFailedMu.Unlock()
	if !shouldRemindIsoFailure(c, time.Hour) {
		t.Errorf("reminder after the interval: got false, want true")
	}

	// Resolving the condition must drop the stamp, so the next occurrence is
	// reported immediately rather than waiting out the old interval.
	shouldCaptureIsoFailure(c, false)
	if !shouldRemindIsoFailure(c, time.Hour) {
		t.Errorf("reminder after the condition cleared: got false, want true")
	}
}

// Detection `unknown` latches its own condition and clears it again once docker
// answers, so a transient blip does not leave the node reporting an unknown
// detection state forever.
func TestDetectionUnknownLatchesAndClears(t *testing.T) {
	defer resetIsoTestState()()

	v4 := iptablesCmd()
	ordered := isoListing(isoReturnSpec, isoDNATReturnSpec, isoDropSpec)
	state := ipv6BridgeUnknown

	fake := newIsoFake(map[string][]isoReply{
		cmdRead(v4, isoChain):        {{out: ordered}},
		cmdRead(v4, isoForwardChain): {{out: forwardListing(true)}},
	})
	isoExec = func(cmd string) ([]byte, error) { return fake.exec(cmd) }
	ipv6PresenceFn = func(context.Context) ipv6BridgeState { return state }

	ensureProjectIsolation(context.Background())
	if got := latchedIsoConditions(); got != "IPv6/detection" {
		t.Errorf("unknown detection: latched %q, want %q", got, "IPv6/detection")
	}

	state = ipv6BridgeNone
	fake = newIsoFake(map[string][]isoReply{
		cmdRead(v4, isoChain):             {{out: ordered}},
		cmdRead(v4, isoForwardChain):      {{out: forwardListing(true)}},
		cmdRead(ip6tablesCmd(), isoChain): {{out: isoListing()}},
	})
	ensureProjectIsolation(context.Background())
	if got := latchedIsoConditions(); got != "" {
		t.Errorf("after docker answered: latched %q, want nothing", got)
	}
}

// deleteAllRules reports how many rules it removed, and the removal path logs
// only when that is non-zero (so a steady-state node is silent). The count is
// also the loop bound, which must hold even when -D never errors.
func TestDeleteAllRules(t *testing.T) {
	defer resetIsoTestState()()

	cases := []struct {
		name     string
		replies  map[string][]isoReply
		failPref []string
		expect   int
	}{
		{
			name:     "no copies present",
			failPref: []string{"iptables -D "},
			expect:   0,
		},
		{
			name:    "one copy",
			replies: map[string][]isoReply{cmdDel("iptables", isoDropSpec): {{}, {err: errIsoExec}}},
			expect:  1,
		},
		{
			name:    "three copies",
			replies: map[string][]isoReply{cmdDel("iptables", isoDropSpec): {{}, {}, {}, {err: errIsoExec}}},
			expect:  3,
		},
		{
			name:   "bounded when -D never errors",
			expect: maxIsolationCleanupPasses,
		},
	}

	for _, c := range cases {
		fake := newIsoFake(c.replies, c.failPref...)
		isoExec = fake.exec
		got := deleteAllRules("iptables", isoChain, isoDropSpec)
		if got != c.expect {
			t.Errorf("%s: deleteAllRules = %d, want %d", c.name, got, c.expect)
		}
		if len(fake.calls) != c.expect+1 && got != maxIsolationCleanupPasses {
			t.Errorf("%s: issued %d commands, want %d (one per removal plus the failing probe)", c.name, len(fake.calls), c.expect+1)
		}
	}
}

// chainHasRules is the removal path's no-op guard, and R9 turns on it: a node
// with no IPv6 bridge hits that path every 60s forever. It must see only real
// rules -- a chain declaration or a policy line is not a rule.
func TestChainHasRules(t *testing.T) {
	cases := []struct {
		name   string
		lines  []string
		expect bool
	}{
		{"nil", nil, false},
		{"empty single line from a trimmed empty output", []string{""}, false},
		{"only the chain declaration", []string{"-N DOCKER-USER"}, false},
		{"only a policy line", []string{"-P FORWARD DROP"}, false},
		{"declaration plus a rule", []string{"-N DOCKER-USER", "-A DOCKER-USER -j RETURN"}, true},
		{"indented rule", []string{"-N DOCKER-USER", "  -A DOCKER-USER -j RETURN"}, true},
		{"a rule naming another chain is still a rule", []string{"-N DOCKER-USER", "-A FORWARD -j DOCKER-USER"}, true},
		{"a delete-shaped line is not an appended rule", []string{"-N DOCKER-USER", "-D DOCKER-USER -j RETURN"}, false},
	}
	for _, c := range cases {
		if got := chainHasRules(c.lines); got != c.expect {
			t.Errorf("%s: chainHasRules=%v, want %v", c.name, got, c.expect)
		}
	}
}

// chainExists and chainRules both shell `<bin> -S <chain>`, deliberately as two
// separate calls: folding them together would make a lost xtables lock
// indistinguishable from an absent chain, and isolationOrdered(nil) is false so
// that would re-apply a correct chain and page for it.
func TestChainExistsAndChainRules(t *testing.T) {
	defer resetIsoTestState()()

	fake := newIsoFake(map[string][]isoReply{
		"iptables -S DOCKER-USER":  {{out: "-N DOCKER-USER\n-A DOCKER-USER -j RETURN\n"}},
		"ip6tables -S DOCKER-USER": {{err: errIsoExec}},
	})
	isoExec = fake.exec

	if !chainExists("iptables", isoChain) {
		t.Errorf("chainExists(iptables): got false, want true")
	}
	if chainExists("ip6tables", isoChain) {
		t.Errorf("chainExists(ip6tables): got true, want false")
	}

	lines, err := chainRules("iptables", isoChain)
	if err != nil {
		t.Errorf("chainRules(iptables): unexpected error %v", err)
	}
	want := []string{"-N DOCKER-USER", "-A DOCKER-USER -j RETURN"}
	if !sameCalls(lines, want) {
		t.Errorf("chainRules(iptables) = %v, want %v", lines, want)
	}

	if _, err := chainRules("ip6tables", isoChain); err == nil {
		t.Errorf("chainRules(ip6tables): got nil error, want the read failure surfaced rather than swallowed")
	}

	wantCalls := []string{
		"iptables -S DOCKER-USER", "ip6tables -S DOCKER-USER",
		"iptables -S DOCKER-USER", "ip6tables -S DOCKER-USER",
	}
	if !sameCalls(fake.calls, wantCalls) {
		t.Errorf("transcript got:%s\nwant:%s", fmtCalls(fake.calls), fmtCalls(wantCalls))
	}
}

// The Sentry capture is the half of "log it and page" that a log-only
// implementation silently drops, so assert the call itself and not merely the
// latch that gates it. Deleting either isoCapture call site in isolation.go
// must fail this test.
func TestSentryCaptureFiresOnTheFailingEdge(t *testing.T) {
	defer resetIsoTestState()()

	var captured []string
	isoCapture = func(c isoCondition, detail string) {
		captured = append(captured, c.family+"/"+c.condition)
	}

	v4 := isoFamily{name: "IPv4", bin: "iptables"}
	ordered := isoListing(isoReturnSpec, isoDNATReturnSpec, isoDropSpec)

	// Healthy: nothing pages.
	isoExec = newIsoFake(map[string][]isoReply{
		cmdRead("iptables", isoChain):        {{out: ordered}},
		cmdRead("iptables", isoForwardChain): {{out: forwardListing(true)}},
	}).exec
	ensureProjectIsolationFamily(v4)
	if len(captured) != 0 {
		t.Errorf("healthy family paged: %v", captured)
	}

	// Jump gone while the contents are correct -- the case an earlier draft of
	// this check could never reach, because it sat below the fast-path return.
	clearIsoFailureState()
	captured = nil
	jumpGone := func() {
		isoExec = newIsoFake(map[string][]isoReply{
			cmdRead("iptables", isoChain):        {{out: ordered}},
			cmdRead("iptables", isoForwardChain): {{out: forwardListing(false)}},
		}).exec
		ensureProjectIsolationFamily(v4)
	}
	jumpGone()
	if want := []string{"IPv4/forward-jump"}; !sameCalls(captured, want) {
		t.Errorf("missing jump: captured %v, want %v", captured, want)
	}
	// Still broken on the next reconcile: logged again, but not paged again.
	jumpGone()
	if want := []string{"IPv4/forward-jump"}; !sameCalls(captured, want) {
		t.Errorf("missing jump twice: captured %v, want %v (a persistent failure must not re-page every 60s)", captured, want)
	}

	// Contents still unordered after the apply: the post-condition pages.
	clearIsoFailureState()
	captured = nil
	isoExec = newIsoFake(map[string][]isoReply{
		cmdRead("iptables", isoChain):        {{out: isoListing(isoDropSpec)}},
		cmdRead("iptables", isoForwardChain): {{out: forwardListing(true)}},
	}).exec
	ensureProjectIsolationFamily(v4)
	if want := []string{"IPv4/contents"}; !sameCalls(captured, want) {
		t.Errorf("unordered after apply: captured %v, want %v", captured, want)
	}
}
