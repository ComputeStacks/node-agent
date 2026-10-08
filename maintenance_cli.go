package main

import (
	"bytes"
	"context"
	"cs-agent/maintenance"
	"cs-agent/store"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/viper"
)

// Exit codes of `cs-agent maintenance`.
const (
	exitOK             = 0 // done
	exitUsage          = 1 // usage error, or a state error (e.g. the local hold was cleared while waiting)
	exitQuiesceTimeout = 2 // --wait timed out with work still in flight
	exitControlDB      = 3 // control.db (or the config locating it) could not be opened
	exitNoController   = 4 // the controller has not confirmed the local hold
)

const (
	// defaultAgentConfig is the only config the CLI reads; CS_AGENT_CONFIG
	// overrides it (tests).
	defaultAgentConfig = "/etc/computestacks/agent.yml"
	defaultDataDir     = "/var/lib/cs-agent"
	defaultDockerAPI   = "1.44" // same default the daemon sets in config.ConfigureApp

	// advertisedFreshness is how recently the controller must have read the
	// changelog with node_maintenance support for --wait to rely on it.
	advertisedFreshness = 120 * time.Second
	// maxReasonBytes matches the admin API's limit on a controller hold reason.
	maxReasonBytes = 512
)

// Injection points for tests.
var (
	maintLister       = func() maintenance.ContainerLister { return maintenance.DockerLister() }
	maintAgentActive  = systemdAgentActive
	maintNow          = time.Now
	maintSleep        = sleepCtx
	maintPollInterval = 2 * time.Second
)

const maintenanceUsage = `Usage: cs-agent maintenance <command> [flags]

Commands:
  on --reason TEXT [--wait --timeout DUR [--settle DUR] [--no-controller]] [--json]
        Place the local maintenance hold. With --wait, block until nothing is
        running and (unless --no-controller) the controller has acknowledged
        the hold.
  off [--json]
        Clear the local maintenance hold. A controller hold is left in place.
  status [--json]
        Show the maintenance state and the work still in flight.
  help
        Show this help.

Flags:
  --reason TEXT      why the node is going into maintenance (on; required)
  --wait             block until the node is quiesced (on)
  --timeout DUR      how long --wait may block, e.g. 30m or 2h (on; required with --wait)
  --settle DUR       once quiesced, wait this long and re-check (on; default 30s)
  --no-controller    with --wait, do not wait for the controller to acknowledge (on)
  --json             print exactly one JSON object on stdout (all commands)

Reads store.data_dir from /etc/computestacks/agent.yml (default /var/lib/cs-agent).

Exit codes: 0 ok, 1 usage or state error, 2 still busy at --timeout,
3 control.db could not be opened, 4 controller has not confirmed the hold.
`

// cliOutput is the --json document: the maintenance status plus whether the
// controller has acknowledged the latest hold change and whether the agent
// service is running.
type cliOutput struct {
	maintenance.Status
	ControllerAcked bool `json:"controller_acked"`
	AgentActive     bool `json:"agent_active"`
}

// runMaintenanceCLI implements `cs-agent maintenance ...` and returns the
// process exit code.
func runMaintenanceCLI(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return maintenanceCLI(ctx, args, os.Stdout, os.Stderr)
}

type onOptions struct {
	reason       string
	wait         bool
	timeout      time.Duration
	settle       time.Duration
	noController bool
}

func maintenanceCLI(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, maintenanceUsage)
		return exitUsage
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, maintenanceUsage)
		return exitOK
	case "on", "off", "status":
	default:
		fmt.Fprintf(stderr, "unknown maintenance command %q\n\n%s", cmd, maintenanceUsage)
		return exitUsage
	}

	fs := flag.NewFlagSet("cs-agent maintenance "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, maintenanceUsage) }
	jsonOut := fs.Bool("json", false, "print one JSON object on stdout")
	var on onOptions
	if cmd == "on" {
		fs.StringVar(&on.reason, "reason", "", "why the node is going into maintenance (required)")
		fs.BoolVar(&on.wait, "wait", false, "wait until the node is quiesced")
		fs.DurationVar(&on.timeout, "timeout", 0, "how long to wait (required with --wait)")
		fs.DurationVar(&on.settle, "settle", 30*time.Second, "after quiescing, wait this long and re-check")
		fs.BoolVar(&on.noController, "no-controller", false, "do not wait for the controller to acknowledge the hold")
	}
	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		if *jsonOut {
			writeJSONError(stdout, err.Error(), exitUsage)
		}
		return exitUsage
	}
	usageErr := func(msg string) int {
		fmt.Fprintf(stderr, "%s\n\n%s", msg, maintenanceUsage)
		if *jsonOut {
			writeJSONError(stdout, msg, exitUsage)
		}
		return exitUsage
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Sprintf("unexpected argument %q", fs.Arg(0)))
	}
	if cmd == "on" {
		if msg := on.validate(fs); msg != "" {
			return usageErr(msg)
		}
	}

	openErr := func(err error) int {
		fmt.Fprintf(stderr, "error: %v\n", err)
		if *jsonOut {
			writeJSONError(stdout, err.Error(), exitControlDB)
		}
		return exitControlDB
	}
	dataDir, err := resolveDataDir()
	if err != nil {
		return openErr(err)
	}
	fmt.Fprintf(stderr, "control.db: %s\n", filepath.Join(dataDir, "control.db"))
	st, err := store.OpenExistingControl(dataDir)
	if err != nil {
		return openErr(err)
	}
	defer st.Close()

	c := &maintCLI{st: st, lister: maintLister(), stdout: stdout, stderr: stderr, json: *jsonOut}
	switch cmd {
	case "on":
		return c.on(ctx, on)
	case "off":
		return c.off(ctx)
	default:
		return c.status(ctx)
	}
}

func (o *onOptions) validate(fs *flag.FlagSet) string {
	o.reason = strings.TrimSpace(o.reason)
	switch {
	case o.reason == "":
		return "--reason is required"
	case len(o.reason) > maxReasonBytes:
		return fmt.Sprintf("--reason is longer than %d bytes", maxReasonBytes)
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if !o.wait {
		for _, name := range []string{"timeout", "settle", "no-controller"} {
			if set[name] {
				return fmt.Sprintf("--%s requires --wait", name)
			}
		}
		return ""
	}
	if o.timeout <= 0 {
		return "--timeout is required with --wait"
	}
	if o.settle < 0 {
		return "--settle must not be negative"
	}
	return ""
}

// resolveDataDir reads store.data_dir from the agent config only (never the
// working directory) and sets docker.version for the docker lister the same
// way the daemon does. A missing config file means the defaults.
func resolveDataDir() (string, error) {
	path := defaultAgentConfig
	if p := os.Getenv("CS_AGENT_CONFIG"); p != "" {
		path = p
	}
	cfg := viper.New()
	cfg.SetConfigType("yaml")
	cfg.SetDefault("store.data_dir", defaultDataDir)
	cfg.SetDefault("docker.version", defaultDockerAPI)
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return "", fmt.Errorf("read config: %w", err)
	default:
		if err := cfg.ReadConfig(bytes.NewReader(b)); err != nil {
			return "", fmt.Errorf("parse config %s: %w", path, err)
		}
	}
	viper.Set("docker.version", cfg.GetString("docker.version"))
	return cfg.GetString("store.data_dir"), nil
}

type maintCLI struct {
	st     *store.Store
	lister maintenance.ContainerLister
	stdout io.Writer
	stderr io.Writer
	json   bool
}

func (c *maintCLI) status(ctx context.Context) int {
	s, err := maintenance.BuildStatus(ctx, c.st, c.lister)
	if err != nil {
		return c.storeErr(err)
	}
	acked, err := c.controllerAcked(ctx, s.Seq)
	if err != nil {
		return c.storeErr(err)
	}
	c.emit(ctx, s, acked)
	return exitOK
}

func (c *maintCLI) off(ctx context.Context) int {
	m, err := c.st.ClearLocalHold(ctx)
	if err != nil {
		return c.storeErr(err)
	}
	s, err := maintenance.StatusFromState(ctx, c.st, c.lister, m)
	if err != nil {
		return c.storeErr(err)
	}
	acked, err := c.controllerAcked(ctx, s.Seq)
	if err != nil {
		return c.storeErr(err)
	}
	if s.Controller != nil {
		fmt.Fprintln(c.stderr, "local hold cleared; the node stays paused by the controller hold")
	}
	c.emit(ctx, s, acked)
	return exitOK
}

func (c *maintCLI) on(ctx context.Context, o onOptions) int {
	// The controller-support check runs once, before the hold is placed: it
	// exists to fail fast on a controller that cannot confirm the hold, without
	// leaving a hold behind. Once waiting, a controller poll that lags is covered
	// by the timeout rather than aborting a long wait.
	if o.wait && !o.noController {
		_, at, err := c.st.GetAdvertised(ctx)
		if err != nil {
			return c.storeErr(err)
		}
		if age := maintNow().Unix() - at; at == 0 || age > int64(advertisedFreshness/time.Second) {
			last := "never"
			if at != 0 {
				last = fmt.Sprintf("%ds ago", age)
			}
			return c.fail(ctx, exitNoController, -1, fmt.Sprintf(
				"controller has not polled with maintenance support recently (last: %s); no local hold was placed (use --no-controller to wait without it)", last))
		}
	}
	m, mySeq, err := c.st.PutLocalHold(ctx, o.reason, invokingUser())
	if err != nil {
		return c.storeErr(err)
	}
	if !o.wait {
		s, err := maintenance.StatusFromState(ctx, c.st, c.lister, m)
		if err != nil {
			return c.storeErr(err)
		}
		acked, err := c.controllerAcked(ctx, mySeq)
		if err != nil {
			return c.storeErr(err)
		}
		c.emit(ctx, s, acked)
		return exitOK
	}
	return c.wait(ctx, o, mySeq)
}

// wait polls until the node is quiesced and, unless noController, the
// controller has acknowledged the changelog entry written by our PutLocalHold
// (seq mySeq). A store error while polling is reported and retried on the next
// poll; if the last poll before the deadline still fails, wait exits
// exitQuiesceTimeout with that error. See the exit codes for the ways it ends.
func (c *maintCLI) wait(ctx context.Context, o onOptions, mySeq int64) int {
	deadline := maintNow().Add(o.timeout)
	for {
		s, acked, err := c.waitSample(ctx, mySeq)
		if err == nil && s.Local == nil {
			return c.failWith(ctx, exitUsage, s, acked, "the local hold was cleared while waiting")
		}
		if err == nil && done(s, acked, o.noController) && o.settle > 0 {
			fmt.Fprintf(c.stderr, "quiesced; settling for %s\n", o.settle)
			if err := maintSleep(ctx, o.settle); err != nil {
				return c.interrupted(ctx, mySeq)
			}
			s, acked, err = c.waitSample(ctx, mySeq)
			if err == nil && s.Local == nil {
				return c.failWith(ctx, exitUsage, s, acked, "the local hold was cleared while waiting")
			}
			if err == nil && !done(s, acked, o.noController) {
				fmt.Fprintln(c.stderr, "work appeared during the settle period; waiting again")
			}
		}
		if err != nil {
			fmt.Fprintf(c.stderr, "warning: read maintenance state: %v; retrying\n", err)
		} else if done(s, acked, o.noController) {
			c.emit(ctx, s, acked)
			return exitOK
		} else {
			fmt.Fprintln(c.stderr, progressLine(s, acked, o.noController))
		}

		remaining := deadline.Sub(maintNow())
		if remaining <= 0 {
			switch {
			case err != nil:
				return c.fail(ctx, exitQuiesceTimeout, mySeq, fmt.Sprintf("timed out after %s: cannot read maintenance state: %v", o.timeout, err))
			case s.Quiesce != maintenance.QuiesceQuiesced:
				return c.failWith(ctx, exitQuiesceTimeout, s, acked, fmt.Sprintf("timed out after %s: node is %s", o.timeout, s.Quiesce))
			}
			return c.failWith(ctx, exitNoController, s, acked, fmt.Sprintf("timed out after %s: quiesced, but the controller has not confirmed the hold", o.timeout))
		}
		if err := maintSleep(ctx, min(maintPollInterval, remaining)); err != nil {
			return c.interrupted(ctx, mySeq)
		}
	}
}

// waitSample builds one status sample for wait.
func (c *maintCLI) waitSample(ctx context.Context, mySeq int64) (maintenance.Status, bool, error) {
	s, err := maintenance.BuildStatus(ctx, c.st, c.lister)
	if err != nil {
		return s, false, err
	}
	acked, err := c.controllerAcked(ctx, mySeq)
	if err != nil {
		return s, false, err
	}
	return s, acked, nil
}

// done is the --wait completion condition (the local hold is checked by
// waitSample).
func done(s maintenance.Status, acked, noController bool) bool {
	return s.Quiesce == maintenance.QuiesceQuiesced && (acked || noController)
}

// controllerAcked reports whether the controller has both been served (with
// node_maintenance support) and acknowledged the changelog up to seq.
func (c *maintCLI) controllerAcked(ctx context.Context, seq int64) (bool, error) {
	ack, err := c.st.GetChangelogAcked(ctx)
	if err != nil {
		return false, err
	}
	hw, _, err := c.st.GetAdvertised(ctx)
	if err != nil {
		return false, err
	}
	return ack >= seq && hw >= seq, nil
}

func (c *maintCLI) interrupted(ctx context.Context, mySeq int64) int {
	// ctx is done; use a fresh one for the final read.
	return c.fail(context.WithoutCancel(ctx), exitUsage, mySeq, "interrupted; the local hold stays in place")
}

// fail builds a final status (for --json) and exits with code and msg. A
// non-positive mySeq means no hold was written: acknowledgement is then judged
// against the latest entry. If the status cannot be read, the --json output is
// the error object instead.
func (c *maintCLI) fail(ctx context.Context, code int, mySeq int64, msg string) int {
	s, err := maintenance.BuildStatus(ctx, c.st, c.lister)
	if err != nil {
		return c.failNoStatus(code, fmt.Sprintf("%s (cannot read maintenance state: %v)", msg, err))
	}
	// No entry of ours (the hold was never placed): nothing for the controller to
	// have acknowledged.
	acked := false
	if mySeq > 0 {
		if acked, err = c.controllerAcked(ctx, mySeq); err != nil {
			return c.failNoStatus(code, fmt.Sprintf("%s (cannot read maintenance state: %v)", msg, err))
		}
	}
	return c.failWith(ctx, code, s, acked, msg)
}

func (c *maintCLI) failWith(ctx context.Context, code int, s maintenance.Status, acked bool, msg string) int {
	fmt.Fprintf(c.stderr, "error: %s\n", msg)
	c.emit(ctx, s, acked)
	return code
}

// failNoStatus exits with code and msg when no status is available; with --json
// stdout gets the error object.
func (c *maintCLI) failNoStatus(code int, msg string) int {
	fmt.Fprintf(c.stderr, "error: %s\n", msg)
	if c.json {
		writeJSONError(c.stdout, msg, code)
	}
	return code
}

func (c *maintCLI) storeErr(err error) int {
	return c.failNoStatus(exitControlDB, err.Error())
}

// writeJSONError writes the --json document of an exit that has no status.
func writeJSONError(w io.Writer, msg string, code int) {
	_ = json.NewEncoder(w).Encode(struct {
		Error    string `json:"error"`
		ExitCode int    `json:"exit_code"`
	}{msg, code})
}

// emit writes the final output: one JSON object, or a short human summary.
func (c *maintCLI) emit(ctx context.Context, s maintenance.Status, acked bool) {
	out := cliOutput{Status: s, ControllerAcked: acked, AgentActive: maintAgentActive(ctx)}
	if c.json {
		enc := json.NewEncoder(c.stdout)
		_ = enc.Encode(out)
		return
	}
	fmt.Fprint(c.stdout, humanSummary(out))
}

func humanSummary(o cliOutput) string {
	var b strings.Builder
	if o.Paused {
		fmt.Fprintf(&b, "paused:           yes, since %s\n", fmtUnix(o.PausedSince))
	} else {
		b.WriteString("paused:           no\n")
	}
	b.WriteString("controller hold:  ")
	if h := o.Controller; h != nil {
		fmt.Fprintf(&b, "%q (gen %d, since %s)\n", h.Reason, h.Gen, fmtUnix(h.Since))
	} else {
		b.WriteString("none\n")
	}
	b.WriteString("local hold:       ")
	if h := o.Local; h != nil {
		fmt.Fprintf(&b, "%q (by %s, since %s)\n", h.Reason, h.By, fmtUnix(h.Since))
	} else {
		b.WriteString("none\n")
	}
	fmt.Fprintf(&b, "quiesce:          %s (%d running, %d pending)\n", o.Quiesce, len(o.Running), o.Pending)
	for _, r := range o.Running {
		fmt.Fprintf(&b, "  %s\n", describeInFlight(r))
	}
	fmt.Fprintf(&b, "skipped backups:  %d\n", o.SkippedBackups)
	fmt.Fprintf(&b, "controller acked: %s\n", yesNo(o.ControllerAcked))
	fmt.Fprintf(&b, "agent active:     %s\n", yesNo(o.AgentActive))
	return b.String()
}

// progressLine is the one-line stderr report for each --wait poll.
func progressLine(s maintenance.Status, acked, noController bool) string {
	var parts []string
	switch s.Quiesce {
	case maintenance.QuiesceQuiesced:
		parts = append(parts, "quiesced")
	case maintenance.QuiesceUnknown:
		parts = append(parts, "quiesce unknown (cannot list backup containers)")
	default:
		const maxShown = 3
		var names []string
		for i, r := range s.Running {
			if i == maxShown {
				names = append(names, fmt.Sprintf("+%d more", len(s.Running)-maxShown))
				break
			}
			names = append(names, describeInFlight(r))
		}
		parts = append(parts, fmt.Sprintf("%d running: %s", len(s.Running), strings.Join(names, ", ")))
	}
	if s.Pending > 0 {
		parts = append(parts, fmt.Sprintf("%d pending (held)", s.Pending))
	}
	if !noController && !acked {
		parts = append(parts, "waiting on controller acknowledgement")
	}
	return "waiting: " + strings.Join(parts, "; ")
}

func describeInFlight(r maintenance.InFlight) string {
	d := r.Kind
	if r.Volume != "" {
		d += " " + r.Volume
	}
	if r.ID != "" && r.ID != r.Kind {
		id := r.ID
		if len(id) > 12 {
			id = id[:12]
		}
		d += " [" + id + "]"
	}
	return d
}

func fmtUnix(ts int64) string {
	if ts == 0 {
		return "unknown"
	}
	return time.Unix(ts, 0).UTC().Format(time.RFC3339)
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// invokingUser is the operator behind the command: $SUDO_USER when run via
// sudo, else the current user.
func invokingUser() string {
	if u := os.Getenv("SUDO_USER"); u != "" {
		return u
	}
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "unknown"
}

// systemdAgentActive reports whether the cs-agent service is running; false on
// any error (no systemd, not installed, timeout).
func systemdAgentActive(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", "cs-agent").Run() == nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
