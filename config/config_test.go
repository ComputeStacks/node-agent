package config

import (
	"testing"

	"github.com/spf13/viper"
)

func TestConfigureAppDefaults(t *testing.T) {
	// Reset viper
	viper.Reset()

	ConfigureApp()

	tests := []struct {
		key      string
		expected interface{}
	}{
		{"log.level", "INFO"},
		{"docker.version", "1.44"},
		{"queue.numworkers", 3},
		{"backups.enabled", true},
		{"backups.borg.image", "ghcr.io/computestacks/cs-docker-borg:latest"},
		{"backups.compact_freq", "45 2 * * *"},
		{"backups.compact_jitter_sec", 1800},
		{"backups.borg.lock_wait_create", "600"},
		{"backups.borg.lock_wait_restore", "120"},
		{"backups.borg.nfs_borg_path", "borg"},
		{"backups.export.workers", 1},
		{"backups.export.timeout_sec", 14400},
		{"backups.export.s3.part_size_mb", 64},
		{"backups.export.s3.sse", "AES256"},
		{"backups.export.s3.default_ttl_sec", 43200},
		{"changelog.prune_freq", "*/15 * * * *"},
		{"changelog.prune_min_age_sec", 604800},
		{"tasks.retention_sec", 604800},
	}

	for _, tt := range tests {
		val := viper.Get(tt.key)
		if val != tt.expected {
			t.Errorf("Expected %s to be %v, got %v", tt.key, tt.expected, val)
		}
	}
}

func TestReleaseEnvironment(t *testing.T) {
	tests := []struct {
		backupKey string
		expected  string
	}{
		{"changeme!", "development"},
		{"tester!", "testing"},
		{"somesecurekey", "production"},
	}

	for _, tt := range tests {
		viper.Set("backups.key", tt.backupKey)
		got := ReleaseEnvironment()
		if got != tt.expected {
			t.Errorf("With backups.key=%s, expected %s, got %s", tt.backupKey, tt.expected, got)
		}
	}
}
