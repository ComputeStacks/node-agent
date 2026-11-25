package main

import (
	"bufio"
	"context"
	"cs-agent/config"
	"cs-agent/dbbackup"
	"cs-agent/s3upload"
	"cs-agent/store"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"filippo.io/age"
	"github.com/spf13/viper"
)

const storeRestoreUsage = `Usage: cs-agent store-restore <command> [flags]

Restores the agent's SQLite databases from the encrypted off-node backups.
The agent must be stopped first (systemctl stop cs-agent); the restore takes
the data dir lock and refuses to run while the agent holds it. Existing
databases are never deleted: they are moved into
<data_dir>/pre-restore-<timestamp>/. Every selected snapshot is downloaded and
validated before anything on disk is changed.

Commands:
  list [--node N] [--project ID | --control]
        Show the node's snapshots grouped by lineage (control.db instance_id).
  control --identity FILE [--node N] [--at TIME] [--lineage ID] [--yes]
        Restore control.db only. Mints a new instance_id and clears the
        backup state so the next run uploads everything.
  project --identity FILE --project ID [--node N] [--at TIME] [--lineage ID] [--yes]
        Restore one project's database. control.db is not touched.
  node --identity FILE [--node N] [--at TIME] [--lineage ID] [--allow-missing] [--yes]
        Restore control.db and the database of every project in its tenants
        table. Project databases on disk that are not tenants are moved aside.
  help
        Show this help.

Flags:
  --identity FILE    age identity (private key) file that decrypts the backups
  --node N           node name the backups were taken under (default
                     store.backup.node_name, then this host's name)
  --project ID       project to restore (project) or to list (list)
  --control          list only control.db snapshots (list)
  --at TIME          newest snapshot at or before this RFC 3339 time (default now)
  --lineage ID       lineage to restore from; required when more than one exists
  --allow-missing    restore even if some tenants have no project snapshot (node)
  --yes              do not ask for confirmation

Reads store.data_dir and store.backup.* (falling back to backups.export.s3.*)
from /etc/computestacks/agent.yml, or the file named by CS_AGENT_CONFIG.

Exit codes: 0 ok, 1 error or declined, 2 usage error.
`

const (
	restoreExitOK    = 0
	restoreExitErr   = 1
	restoreExitUsage = 2
)

var storeRestoreCommands = []string{"list", "control", "project", "node"}

// restoreEnv is what the CLI needs from the agent config; replaced in tests.
type restoreEnv struct {
	dataDir  string
	nodeName string // store.backup.node_name, may be empty
	obj      dbbackup.ObjectStore
}

// Injection points for tests.
var (
	restoreLoadEnv  = loadRestoreEnv
	restoreHostname = os.Hostname
	restoreNow      = time.Now
)

func loadRestoreEnv() (restoreEnv, error) {
	path := defaultAgentConfig
	if p := os.Getenv("CS_AGENT_CONFIG"); p != "" {
		path = p
	}
	// Same loader and defaults as the daemon, but only from this file.
	viper.SetConfigFile(path)
	config.ConfigureApp()
	obj, err := s3upload.New(s3upload.ConfigFromViperKey("store.backup.s3", "backups.export.s3"))
	if err != nil {
		return restoreEnv{}, fmt.Errorf("object storage (store.backup.s3): %w", err)
	}
	return restoreEnv{
		dataDir:  viper.GetString("store.data_dir"),
		nodeName: viper.GetString("store.backup.node_name"),
		obj:      obj,
	}, nil
}

type restoreFlags struct {
	identity     string
	node         string
	project      string
	control      bool
	at           string
	lineage      string
	allowMissing bool
	yes          bool
}

func newStoreRestoreFlagSet(cmd string, stderr io.Writer) (*flag.FlagSet, *restoreFlags) {
	fs := flag.NewFlagSet("cs-agent store-restore "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, storeRestoreUsage) }
	f := &restoreFlags{}
	fs.StringVar(&f.node, "node", "", "node name the backups were taken under")
	switch cmd {
	case "list":
		fs.StringVar(&f.project, "project", "", "list only this project's snapshots")
		fs.BoolVar(&f.control, "control", false, "list only control.db snapshots")
	default:
		fs.StringVar(&f.identity, "identity", "", "age identity file")
		fs.StringVar(&f.at, "at", "", "newest snapshot at or before this RFC 3339 time")
		fs.StringVar(&f.lineage, "lineage", "", "lineage to restore from")
		fs.BoolVar(&f.yes, "yes", false, "do not ask for confirmation")
		if cmd == "project" {
			fs.StringVar(&f.project, "project", "", "project to restore")
		}
		if cmd == "node" {
			fs.BoolVar(&f.allowMissing, "allow-missing", false, "tolerate tenants with no project snapshot")
		}
	}
	return fs, f
}

// runStoreRestoreCLI implements `cs-agent store-restore ...`.
func runStoreRestoreCLI(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return storeRestoreCLI(ctx, args, os.Stdin, os.Stdout, os.Stderr)
}

func storeRestoreCLI(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, storeRestoreUsage)
		return restoreExitUsage
	}
	cmd, rest := args[0], args[1:]
	switch {
	case cmd == "help" || cmd == "-h" || cmd == "-help" || cmd == "--help":
		fmt.Fprint(stdout, storeRestoreUsage)
		return restoreExitOK
	case !slices.Contains(storeRestoreCommands, cmd):
		fmt.Fprintf(stderr, "unknown store-restore command %q\n\n%s", cmd, storeRestoreUsage)
		return restoreExitUsage
	}
	fs, f := newStoreRestoreFlagSet(cmd, stderr)
	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return restoreExitOK
		}
		return restoreExitUsage
	}
	usageErr := func(msg string) int {
		fmt.Fprintf(stderr, "%s\n\n%s", msg, storeRestoreUsage)
		return restoreExitUsage
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Sprintf("unexpected argument %q", fs.Arg(0)))
	}

	var at time.Time
	var ids []age.Identity
	if cmd == "list" {
		if f.control && f.project != "" {
			return usageErr("--control and --project are mutually exclusive")
		}
	} else {
		if f.identity == "" {
			return usageErr("--identity is required")
		}
		if cmd == "project" && f.project == "" {
			return usageErr("--project is required")
		}
		if f.at != "" {
			t, err := time.Parse(time.RFC3339, f.at)
			if err != nil {
				return usageErr(fmt.Sprintf("--at: %v", err))
			}
			at = t.UTC()
		}
		var err error
		if ids, err = readIdentities(f.identity); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return restoreExitErr
		}
	}

	env, err := restoreLoadEnv()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return restoreExitErr
	}
	node := f.node
	if node == "" {
		node = env.nodeName
	}
	if node == "" {
		if node, err = restoreHostname(); err != nil || node == "" {
			fmt.Fprintf(stderr, "error: cannot determine the node name (%v); pass --node\n", err)
			return restoreExitErr
		}
	}

	if cmd == "list" {
		return restoreList(ctx, env, node, f, stdout, stderr)
	}

	opts := dbbackup.RestoreOptions{
		Mode:         cmd,
		DataDir:      env.dataDir,
		Node:         node,
		ProjectID:    f.project,
		At:           at,
		Lineage:      f.lineage,
		AllowMissing: f.allowMissing,
		Identities:   ids,
		HoldBy:       invokingUser(),
		Now:          restoreNow,
	}
	in := bufio.NewReader(stdin)
	opts.Confirm = func(p *dbbackup.RestorePlan) bool {
		printRestorePlan(stdout, p)
		if f.yes {
			return true
		}
		fmt.Fprint(stdout, "Proceed? [y/N]: ")
		line, _ := in.ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			return true
		}
		return false
	}
	fmt.Fprintf(stdout, "Downloading and validating snapshots for node %s into %s ...\n", node, env.dataDir)
	res, err := dbbackup.Restore(ctx, env.obj, opts)
	if err != nil {
		var se *dbbackup.SwapError
		if errors.As(err, &se) {
			fmt.Fprintf(stderr, "error: %v\nThe agent must stay stopped until this is resolved by hand.\n", err)
		} else if errors.Is(err, dbbackup.ErrAborted) {
			fmt.Fprintln(stderr, err)
		} else {
			fmt.Fprintf(stderr, "error: %v\nNothing was changed.\n", err)
		}
		return restoreExitErr
	}
	fmt.Fprintln(stdout, "Restore complete.")
	for _, p := range res.Installed {
		fmt.Fprintf(stdout, "  installed %s\n", p)
	}
	for _, m := range res.Moved {
		fmt.Fprintf(stdout, "  moved aside %s\n", m)
	}
	if res.PreRestoreDir != "" {
		fmt.Fprintf(stdout, "Originals are in %s (not removed automatically).\n", res.PreRestoreDir)
	}
	if res.Plan.NewInstanceID != "" {
		fmt.Fprintf(stdout, "New instance_id: %s\n", res.Plan.NewInstanceID)
	}
	fmt.Fprintln(stdout, "Start the agent with: systemctl start cs-agent")
	if res.Plan.MaintenanceHold {
		fmt.Fprint(stdout, maintenanceHoldNotice)
	}
	return restoreExitOK
}

func readIdentities(path string) ([]age.Identity, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("--identity: %w", err)
	}
	defer fh.Close()
	ids, err := age.ParseIdentities(fh)
	if err != nil {
		return nil, fmt.Errorf("--identity %s: %w", path, err)
	}
	return ids, nil
}

func restoreList(ctx context.Context, env restoreEnv, node string, f *restoreFlags, stdout, stderr io.Writer) int {
	db := ""
	switch {
	case f.control:
		db = "control"
	case f.project != "":
		db = "projects/" + f.project
	}
	ls, err := dbbackup.ListLineages(ctx, env.obj, node, db)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return restoreExitErr
	}
	if len(ls) == 0 {
		fmt.Fprintf(stdout, "No snapshots for node %s.\n", node)
		return restoreExitOK
	}
	fmt.Fprintf(stdout, "Snapshots for node %s by lineage (oldest first):\n", node)
	for _, l := range ls {
		fmt.Fprintf(stdout, "  lineage %s\n    first %s  last %s  objects %d  control.db %d  projects %d\n",
			l.ID, l.First.Format(time.RFC3339), l.Last.Format(time.RFC3339), l.Objects, l.Control, len(l.Projects))
		if db != "" && len(l.Projects) > 0 {
			fmt.Fprintf(stdout, "    project %s\n", strings.Join(l.Projects, ", "))
		}
	}
	return restoreExitOK
}

func printRestorePlan(w io.Writer, p *dbbackup.RestorePlan) {
	fmt.Fprintf(w, "\nRestore plan: %s restore of node %s from lineage %s (snapshots at or before %s)\n",
		p.Mode, p.Node, p.Lineage, p.At.Format(time.RFC3339))
	fmt.Fprintf(w, "Data dir: %s\n", p.DataDir)
	for _, d := range p.DBs {
		fmt.Fprintf(w, "  %s\n    object %s (%d bytes, taken %s)\n    restored size %d bytes -> %s\n",
			d.DB, d.Key, d.ObjectSize, d.Time.Format(time.RFC3339), d.Size, d.Target)
		for _, m := range d.MoveAside {
			fmt.Fprintf(w, "    move aside %s\n", m)
		}
	}
	if len(p.Orphans) > 0 {
		fmt.Fprintln(w, "  project files not in the restored tenants (moved aside):")
		for _, o := range p.Orphans {
			fmt.Fprintf(w, "    %s\n", o)
		}
	}
	if len(p.MissingTenants) > 0 {
		fmt.Fprintf(w, "  tenants with no snapshot, left as they are on disk: %s\n", strings.Join(p.MissingTenants, ", "))
	}
	fmt.Fprintf(w, "Originals go to %s\n", p.PreRestoreDir)
	if p.NewInstanceID != "" {
		fmt.Fprintf(w, "control.db gets a new instance_id %s; the backup state file is cleared.\n", p.NewInstanceID)
	}
	if p.Mode == dbbackup.ModeControl {
		fmt.Fprintln(w, "Project databases are left as they are.")
	}
	if p.MaintenanceHold {
		fmt.Fprintln(w, "The restored control.db is left in local maintenance mode: no task runs until you run `cs-agent maintenance off`.")
		if len(p.CanceledTasks) > 0 {
			fmt.Fprintln(w, "  pending volume.restore tasks canceled by entering maintenance:")
			printTasks(w, p.CanceledTasks)
		}
		if len(p.OpenTasks) == 0 {
			fmt.Fprintln(w, "  the restored control.db has no pending or running tasks.")
		} else {
			fmt.Fprintln(w, "  pending or running tasks in the restored control.db (some may already have run after the snapshot was taken):")
			printTasks(w, p.OpenTasks)
			fmt.Fprintln(w, "  running tasks are marked failed when the agent starts; pending tasks run once maintenance is turned off.")
		}
	}
	for _, warn := range p.Warnings {
		fmt.Fprintf(w, "WARNING: %s\n", warn)
	}
}

// maintenanceHoldNotice is printed after a control or node restore.
const maintenanceHoldNotice = `The node is in local maintenance mode: the agent serves its API but runs no
tasks or scheduled work. Review the pending tasks listed above (they may
already have run after the snapshot was taken), then resume work with:
  sudo cs-agent maintenance off
The hold does not expire on its own; after maintenance.stale_hold_hours the
agent only logs and reports that it is still in place.
`

func printTasks(w io.Writer, ts []store.Task) {
	for _, t := range ts {
		target := t.Volume
		if target == "" {
			target = t.ProjectID
		}
		fmt.Fprintf(w, "    %s %s %s %s (created %s)\n", t.ID, t.Status, t.Name, target,
			time.Unix(t.CreatedAt, 0).UTC().Format(time.RFC3339))
	}
}
