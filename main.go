package main

import (
	"context"
	"cs-agent/backup"
	"cs-agent/config"
	"cs-agent/firewall"
	"cs-agent/httpapi"
	"cs-agent/job"
	"cs-agent/log"
	"cs-agent/maintenance"
	"cs-agent/s3upload"
	"cs-agent/store"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/spf13/viper"
)

// Build-info, injected at release time via GoReleaser ldflags
// (-X main.version / -X main.commit / -X main.date). Defaults apply to
// `go build` / `go run` so behavior is unchanged when unset.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	// `cs-agent maintenance ...` is the local operator CLI, not the daemon. It is
	// dispatched before flag parsing so its own flags are not seen here.
	if len(os.Args) > 1 && os.Args[1] == "maintenance" {
		os.Exit(runMaintenanceCLI(os.Args[2:]))
	}

	showVersion := flag.Bool("version", false, "print version information and exit")
	flag.Parse()
	if *showVersion {
		fmt.Printf("cs-agent %s (commit %s, built %s)\n", version, commit, date)
		return
	}

	config.ConfigureApp()
	configureSentry(version)
	log.New().Info("Starting CS-Agent", "version", version, "commit", commit, "date", date)
	validateExportConfig()

	// Open the embedded data plane. control.db is the sole source of truth for
	// coordination state now (no Consul); opening it also runs migrations.
	st, err := store.Open(viper.GetString("store.data_dir"), store.Options{})
	if err != nil {
		log.New().Error("Failed to open control store", "error", err.Error())
		sentry.CaptureException(err)
		panic(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	// In-process coordination components (all read/write control.db).
	dispatcher := job.NewDispatcher(st)
	fwReconciler := firewall.NewReconciler(st)
	// control.db retention runs unconditionally (independent of backups.enabled).
	housekeeper := backup.NewHousekeeper(st)
	var scheduler *backup.Scheduler
	if viper.GetBool("backups.enabled") {
		scheduler = backup.NewScheduler(st, dispatcher.Signal)
	}

	// Customer-metadata + admin HTTP front door. Reconcile hooks wake the
	// in-process consumers after a controller DOWN write (all non-blocking).
	srv := httpapi.New(httpapi.Config{
		ListenAddr:        viper.GetString("metadata.listen_addr"),
		AdminTokenHash:    viper.GetString("metadata.admin_token_hash"),
		MaxBodyBytes:      viper.GetInt64("metadata.max_body_bytes"),
		OnTaskCreated:     dispatcher.Signal,
		OnFirewallChanged: fwReconciler.Signal,
		OnVolumesChanged: func() {
			if scheduler != nil {
				scheduler.ReconcileSignal()
			}
		},
	}, st, log.New())

	// No maintenance job (prune, compact) survives a restart; a marker left by
	// the previous process would otherwise block that job from ever starting.
	if err := st.ClearMaintJobMarkers(ctx); err != nil {
		log.New().Error("Failed to clear maintenance job markers", "error", err.Error())
		sentry.CaptureException(err)
	}

	// Start order: components (dispatcher runs its boot crash-reconcile before
	// accepting work) → then the HTTP front door LAST, so the DOWN surface only
	// opens once the consumers that react to it are running.
	dispatcher.Start(ctx, &wg) // registers its own worker/loop wg counts
	wg.Add(1)
	go func() { defer wg.Done(); fwReconciler.Run(ctx) }()
	wg.Add(1)
	go func() { defer wg.Done(); housekeeper.Run(ctx) }()
	if scheduler != nil {
		wg.Add(1)
		go func() { defer wg.Done(); scheduler.Run(ctx) }()
	}
	staleHold := time.Duration(viper.GetInt("maintenance.stale_hold_hours")) * time.Hour
	wg.Add(1)
	go func() { defer wg.Done(); maintenance.Watch(ctx, st, staleHold) }()

	go func() {
		if err := srv.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.New().Error("metadata HTTP server stopped", "error", err.Error())
			sentry.CaptureException(err)
		}
	}()

	log.New().Info("Agent Configuration", "environment", config.ReleaseEnvironment())
	log.New().Info("Agent Configuration", "backupWorkers", viper.GetInt("queue.numworkers")+1)
	log.New().Info("Agent Configuration", "backingFS", currentBackupMethod())

	// Ordered shutdown. On SIGTERM/SIGINT:
	//  1. Shut the HTTP DOWN surface first (drain in-flight customer requests;
	//     they use the store) so a late DOWN write can't signal a reconciler we're
	//     about to stop. In-process signals are non-blocking, so this order is safe.
	//  2. Cancel ctx → dispatcher/workers/scheduler/firewall observe it and drain.
	//  3. Bounded wait for the workers/loops to finish an in-flight job.
	//  4. Close the store (nothing uses it after the workers stop).
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
	log.New().Info("Shutdown signal received, draining")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.New().Warn("metadata server shutdown", "error", err)
	}

	cancel()
	waitWorkers(&wg, 25*time.Second)

	if err := st.Close(); err != nil {
		log.New().Warn("control store close", "error", err)
	}
	log.New().Info("Shutdown complete")
}

// waitWorkers blocks until wg drains or timeout elapses, whichever is first. The
// dispatcher/scheduler/firewall loops all observe ctx cancel, so wg normally
// drains promptly; the bound is a safety net so a worker stuck in a long-running
// borg job can't hold shutdown open forever.
func waitWorkers(wg *sync.WaitGroup, timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		log.New().Info("Workers stopped")
	case <-time.After(timeout):
		log.New().Warn("Workers did not stop within timeout; proceeding with shutdown", "timeout", timeout.String())
	}
}

func currentBackupMethod() string {
	if viper.GetBool("backups.borg.ssh.enabled") {
		return "ssh"
	} else if viper.GetBool("backups.borg.nfs") {
		return "nfs"
	} else {
		return "local"
	}
}

// validateExportConfig loudly surfaces export misconfiguration at startup. It
// does not abort the agent (export jobs fail individually if misconfigured), but
// the no-compactor combination is dangerous enough to flag prominently: with
// export enabled and the host compact cron retired, an empty compact_freq means
// nothing ever compacts repos and they grow unbounded.
func validateExportConfig() {
	if viper.GetString("backups.export.s3.bucket") == "" {
		return // export disabled (no bucket)
	}
	if viper.GetString("backups.compact_freq") == "" {
		log.New().Error("backups.export is enabled but backups.compact_freq is empty: with the host compact cron retired, nothing compacts repositories and they will grow unbounded. Set backups.compact_freq.")
		sentry.CaptureMessage("backup export enabled with compaction disabled (no compactor)")
	}
	if err := s3upload.ConfigFromViper().Validate(0); err != nil {
		log.New().Error("Invalid backups.export.s3 configuration", "error", err.Error())
		sentry.CaptureException(err)
	}
}

/*
*
Configure Sentry

Resources:
  - https://github.com/getsentry/sentry-go
  - https://github.com/getsentry/sentry-go/blob/master/MIGRATION.md
  - https://docs.sentry.io/clients/go/
*/
func configureSentry(v string) {
	env := config.ReleaseEnvironment()
	hostname, _ := os.Hostname()
	err := sentry.Init(sentry.ClientOptions{
		Dsn:              viper.GetString("sentry.dsn"),
		Environment:      env,
		Debug:            env != "production",
		ServerName:       hostname,
		AttachStacktrace: true,
		Release:          v,
	})
	if err != nil {
		panic(err)
	}
}
