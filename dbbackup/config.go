package dbbackup

import (
	"fmt"
	"os"
	"time"

	"filippo.io/age"
	"github.com/spf13/viper"
)

// Config is the runner configuration.
type Config struct {
	Enabled    bool
	Freq       string
	Recipients []age.Recipient
	NodeName   string
	KeepAll    time.Duration
	// RetentionDays bounds the per-day tier.
	RetentionDays int
	// IsProjectGone reports whether a SnapshotProject error means the project
	// vanished (skipped, not a failure). nil = never.
	IsProjectGone func(error) bool
}

// ConfigFromViper reads store.backup.*. Disabled returns a Config with
// Enabled=false and no error.
func ConfigFromViper() (Config, error) {
	if !viper.GetBool("store.backup.enabled") {
		return Config{Freq: viper.GetString("store.backup.freq")}, nil
	}
	cfg := Config{Enabled: true, Freq: viper.GetString("store.backup.freq"), NodeName: viper.GetString("store.backup.node_name")}
	raws := viper.GetStringSlice("store.backup.recipients")
	if len(raws) == 0 {
		return Config{}, fmt.Errorf("store.backup.recipients: at least one age recipient is required when store.backup.enabled is true")
	}
	for _, s := range raws {
		rcp, err := age.ParseX25519Recipient(s)
		if err != nil {
			return Config{}, fmt.Errorf("store.backup.recipients: invalid age recipient %q: %w", s, err)
		}
		cfg.Recipients = append(cfg.Recipients, rcp)
	}
	hours := 48
	if viper.IsSet("store.backup.keep_all_hours") {
		hours = viper.GetInt("store.backup.keep_all_hours")
	}
	if hours < 0 {
		return Config{}, fmt.Errorf("store.backup.keep_all_hours: must not be negative")
	}
	cfg.KeepAll = time.Duration(hours) * time.Hour
	cfg.RetentionDays = 30
	if viper.IsSet("store.backup.retention_days") {
		cfg.RetentionDays = viper.GetInt("store.backup.retention_days")
	}
	if cfg.RetentionDays < 1 {
		return Config{}, fmt.Errorf("store.backup.retention_days: must be at least 1")
	}
	if cfg.NodeName == "" {
		h, err := os.Hostname()
		if err != nil || h == "" {
			return Config{}, fmt.Errorf("store.backup.node_name: unset and hostname unavailable: %v", err)
		}
		cfg.NodeName = h
	}
	return cfg, nil
}
