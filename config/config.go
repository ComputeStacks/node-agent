package config

import (
	"cs-agent/log"

	"github.com/spf13/viper"
)

// ConfigureApp initializes the application configuration using Viper
func ConfigureApp() {
	viper.SetConfigType("yaml")

	// Load File
	viper.SetConfigName("agent")              // name of config file (without extension)
	viper.AddConfigPath("/etc/computestacks") // path to look for the config file in
	viper.AddConfigPath(".")                  // optionally look for config in the working directory
	err := viper.ReadInConfig()               // Find and read the config file
	if err != nil {                           // Handle errors reading the config file
		log.New().Warn("Error loading configuration file", "error", err)
	}

	////
	// Defaults
	viper.SetDefault("log.level", "INFO")
	viper.SetDefault("sentry.dsn", "https://caf0e228c0dc4c36a4b4972cc2c0eba2@sentry.cmptstks.com/3")

	////
	// Specify which iptables command to use
	// For docker environments using the older legacy iptables, switch to: iptables-legacy
	viper.SetDefault("host.iptables-cmd", "iptables")

	////
	// Specify which ip6tables command to use for the cross-project isolation
	// rules. Independent of host.iptables-cmd; setting one does not imply the
	// other. For docker environments using the older legacy iptables, switch
	// to: ip6tables-legacy
	viper.SetDefault("host.ip6tables-cmd", "ip6tables")

	// For testing purposes only, dont set `true` in production environments.
	viper.SetDefault("docker.privileged", false)

	viper.SetDefault("docker.version", "1.44")

	viper.SetDefault("computestacks.host", "localhost:3000")

	viper.SetDefault("queue.numworkers", 3)

	// Changelog retention (prune janitor): delete rows at/below the controller's
	// acked watermark once older than prune_min_age_sec, plus any row older than
	// prune_max_age_sec regardless of ack (bounds growth before the controller
	// integrates and starts acking).
	viper.SetDefault("changelog.prune_freq", "*/15 * * * *")
	viper.SetDefault("changelog.prune_min_age_sec", 604800)  // 7d
	viper.SetDefault("changelog.prune_max_age_sec", 2592000) // 30d
	// Terminal task-row retention (all kinds). Must exceed the longest export
	// presigned-URL TTL so a completed export whose link is still live isn't reaped.
	viper.SetDefault("tasks.retention_sec", 604800) // 7d

	// Embedded SQLite data plane (store/): control.db + per-project metadata DBs
	// live under this directory.
	viper.SetDefault("store.data_dir", "/var/lib/cs-agent")

	// Customer-metadata HTTP front door (httpapi/). Binds the node's
	// own :8500 (baked into customer containers via metadata.internal:8500, so
	// the port must stay 8500); the provisioner sets listen_addr to
	// primary_ip:8500. admin_token_hash is the hex sha256 of the per-node admin
	// Bearer the controller authenticates with (empty disables the admin scope).
	// max_body_bytes caps a single request body (413 on exceed) — the STORED
	// value is uncapped; this is only a transport limit.
	viper.SetDefault("metadata.listen_addr", ":8500")
	viper.SetDefault("metadata.admin_token_hash", "")
	viper.SetDefault("metadata.max_body_bytes", 10485760) // 10 MiB

	// Node maintenance mode: warn (log + Sentry, hourly) about a maintenance
	// hold older than this many hours. 0 disables the warning.
	viper.SetDefault("maintenance.stale_hold_hours", 12)

	viper.SetDefault("backups.enabled", true)
	viper.SetDefault("backups.prune_freq", "15 1 * * *")
	// Compaction now runs in-agent (was a host cron on the backup server). Set
	// to "" to disable scheduling it. Offset from prune so prune (mark) runs
	// before compact (reclaim).
	viper.SetDefault("backups.compact_freq", "45 2 * * *")
	// Per-node random delay (seconds) before a compact sweep, so many nodes
	// sharing one backup server don't all compact at the same minute.
	viper.SetDefault("backups.compact_jitter_sec", 1800)
	viper.SetDefault("backups.key", "changeme!")

	viper.SetDefault("backups.borg.image", "ghcr.io/computestacks/cs-docker-borg:latest")
	viper.SetDefault("backups.borg.compression", "zstd,3")
	viper.SetDefault("backups.borg.lock_wait", "1")
	// Longer lock-wait for `borg create`: a scheduled backup should wait out an
	// in-agent compact/prune (both hold borg's exclusive lock) rather than fail
	// after 1s and miss the backup. Ops without an override fall back to lock_wait.
	viper.SetDefault("backups.borg.lock_wait_create", "600")
	// Longer lock-wait for `borg extract` too, but deliberately far shorter than
	// lock_wait_create: a create waits with the volume in place and the service up,
	// whereas a restore's extract waits AFTER the pre-restore hook has stopped the
	// service and moved /mnt/data into the backup container. Every second of that wait
	// is a second with the volume empty and its only copy inside an AutoRemove
	// container. 120s rides out an in-agent prune/compact or a lock handover instead of
	// rolling back a restore over a transient lock, while keeping the empty-volume
	// window to the same order as the extract itself. When it does expire the failure is
	// the safe one: LockTimeout with nothing extracted, and the snapshot goes back.
	viper.SetDefault("backups.borg.lock_wait_restore", "120")

	viper.SetDefault("backups.borg.ssh.enabled", false)
	viper.SetDefault("backups.borg.ssh.user", "")
	viper.SetDefault("backups.borg.ssh.host", "")
	viper.SetDefault("backups.borg.ssh.port", "22")
	viper.SetDefault("backups.borg.ssh.host_path", "/tmp")
	viper.SetDefault("backups.borg.ssh.keyfile", "/etc/computestacks/backup/.ssh/id_ed25519")
	viper.SetDefault("backups.borg.ssh_borg_remote_path", "/usr/bin/borg")

	viper.SetDefault("backups.borg.nfs", false)
	viper.SetDefault("backups.borg.nfs_host", "127.0.0.1")
	viper.SetDefault("backups.borg.nfs_opts", ",async,noatime,rsize=32768,wsize=32768")

	viper.SetDefault("backups.borg.nfs_host_path", "/var/nfsshare/volume_backups")
	// Path to the borg binary ON the NFS/backup server. The agent runs
	// `borg compact` locally there over SSH (heavy segment rewriting stays off
	// the network). A non-interactive SSH session may lack /usr/local/bin on
	// PATH, so this is configurable; bare "borg" relies on the remote PATH.
	viper.SetDefault("backups.borg.nfs_borg_path", "borg")
	viper.SetDefault("backups.borg.nfs_ssh.user", "root")
	viper.SetDefault("backups.borg.nfs_ssh.port", "22")
	viper.SetDefault("backups.borg.nfs_ssh.keyfile", "/root/.ssh/id_ed25519")

	// Whether we ssh into the backup server and create the path.
	viper.SetDefault("backups.borg.nfs_create_path", true)

	// When using NFS, and nfs_creat_path is enabled, we will attempt to change the ownership
	// on the host to this value. The SSH user MUST have permissions to do so.
	viper.SetDefault("backups.borg.nfs_ssh.fs_user", "nobody")
	viper.SetDefault("backups.borg.nfs_ssh.fs_group", "nogroup")

	// Backup export ("download backup"): stream a chosen archive to S3 and hand
	// back a presigned URL. Inert until backups.export.s3.bucket is set.
	viper.SetDefault("backups.export.workers", 1)         // dedicated export worker count
	viper.SetDefault("backups.export.tar_filter", "gzip") // borg --tar-filter for the exported tar; "" disables. export-tar emits the ORIGINAL (decompressed) files, so the repo's own compression does NOT carry over — without a filter the upload is full plaintext size. Suffix tracks this (gzip -> .tar.gz). "pigz"/"gzip -1" trade ratio for speed if the borg image has them.
	viper.SetDefault("backups.export.timeout_sec", 14400) // hard cap on a single export so a hung borg/S3 can't hold the repo lock (4h)
	viper.SetDefault("backups.export.s3.endpoint", "")    // empty = real AWS; set for S3-compatible (MinIO/Ceph)
	viper.SetDefault("backups.export.s3.region", "us-east-1")
	viper.SetDefault("backups.export.s3.bucket", "")
	viper.SetDefault("backups.export.s3.prefix", "exports/")
	viper.SetDefault("backups.export.s3.access_key", "")
	viper.SetDefault("backups.export.s3.secret_key", "")
	viper.SetDefault("backups.export.s3.force_path_style", false)   // true for MinIO/path-style
	viper.SetDefault("backups.export.s3.part_size_mb", 64)          // 5MB*10000=50GB ceiling is too tight
	viper.SetDefault("backups.export.s3.concurrency", 4)            // parts in flight; mem ≈ part_size*concurrency
	viper.SetDefault("backups.export.s3.sse", "AES256")             // server-side encryption (exported tar is plaintext)
	viper.SetDefault("backups.export.s3.default_ttl_sec", 43200)    // presigned URL TTL when unspecified (12h)
	viper.SetDefault("backups.export.s3.max_ttl_sec", 86400)        // hard cap on a requested TTL (24h)

	// MariaDB Backup Configuration
	viper.SetDefault("mariadb.lock_wait.query_type", "ALL")
	viper.SetDefault("mariadb.lock_wait.timeout", "60")
	viper.SetDefault("mariadb.long_queries.timeout", "20")
	viper.SetDefault("mariadb.long_queries.query_type", "SELECT")

}

// ReleaseEnvironment is a helper used to determine current release
func ReleaseEnvironment() string {
	if viper.GetString("backups.key") == "changeme!" {
		return "development"
	} else if viper.GetString("backups.key") == "tester!" {
		return "testing"
	} else {
		return "production"
	}
}
