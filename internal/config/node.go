package config

import (
	"flag"
	"fmt"
	"time"
)

// NodeConfig holds the backup-node agent's settings.
type NodeConfig struct {
	// BackupHost is the Primary endpoint to pull from, e.g.
	// "backup.example.com" (BACKUP_HOST). Always addressed over HTTPS.
	BackupHost string
	// EnrollmentToken is the one-time bootstrap secret (ENROLLMENT_TOKEN).
	EnrollmentToken string
	// NodeLabel is this node's human-friendly name (NODE_LABEL).
	NodeLabel string
	// PullInterval is how often the node polls/syncs (PULL_INTERVAL).
	PullInterval time.Duration
	// StoreDir is the append-only package store (STORE_DIR).
	StoreDir string
	// MinisignPubKeyFile verifies manifests published by the Primary (MINISIGN_PUBKEY).
	MinisignPubKeyFile string
	// CapacityWarnPct is the local free-space warning threshold, 0-100 (CAPACITY_WARN_PCT).
	CapacityWarnPct float64
	// DownloadIdleTimeout aborts a package download only after this long with no
	// progress (DOWNLOAD_IDLE_TIMEOUT); it never caps total transfer time.
	DownloadIdleTimeout time.Duration
}

// NodeConfigFromEnv builds a NodeConfig from environment variables.
func NodeConfigFromEnv() (NodeConfig, error) {
	enrollmentToken, err := SecretFromEnv("ENROLLMENT_TOKEN")
	if err != nil {
		return NodeConfig{}, err
	}
	return NodeConfig{
		BackupHost:          envOr("BACKUP_HOST", ""),
		EnrollmentToken:     enrollmentToken,
		NodeLabel:           envOr("NODE_LABEL", ""),
		PullInterval:        envDurationOr("PULL_INTERVAL", time.Hour),
		StoreDir:            envOr("STORE_DIR", ""),
		MinisignPubKeyFile:  envOr("MINISIGN_PUBKEY", ""),
		CapacityWarnPct:     envFloatOr("CAPACITY_WARN_PCT", 90),
		DownloadIdleTimeout: envDurationOr("DOWNLOAD_IDLE_TIMEOUT", 2*time.Minute),
	}, nil
}

// BindFlags registers flags that override the current (env-derived) values.
func (c *NodeConfig) BindFlags(fs *flag.FlagSet) {
	fs.StringVar(&c.BackupHost, "backup-host", c.BackupHost, "Primary endpoint to pull from (BACKUP_HOST)")
	fs.StringVar(&c.EnrollmentToken, "enrollment-token", c.EnrollmentToken, "one-time bootstrap token (ENROLLMENT_TOKEN)")
	fs.StringVar(&c.NodeLabel, "label", c.NodeLabel, "human-friendly node name (NODE_LABEL)")
	fs.DurationVar(&c.PullInterval, "pull-interval", c.PullInterval, "how often to poll for new runs (PULL_INTERVAL)")
	fs.StringVar(&c.StoreDir, "store", c.StoreDir, "append-only package store directory (STORE_DIR)")
	fs.StringVar(&c.MinisignPubKeyFile, "minisign-pubkey", c.MinisignPubKeyFile, "minisign public key file (MINISIGN_PUBKEY)")
	fs.Float64Var(&c.CapacityWarnPct, "capacity-warn-pct", c.CapacityWarnPct, "local free-space warning threshold (CAPACITY_WARN_PCT)")
	fs.DurationVar(&c.DownloadIdleTimeout, "download-idle-timeout", c.DownloadIdleTimeout, "abort a package download after this long with no progress (DOWNLOAD_IDLE_TIMEOUT)")
}

// Validate checks that the required fields are present.
func (c NodeConfig) Validate() error {
	if c.BackupHost == "" {
		return fmt.Errorf("backup host is required (set BACKUP_HOST or -backup-host)")
	}
	if c.StoreDir == "" {
		return fmt.Errorf("store directory is required (set STORE_DIR or -store)")
	}
	if c.MinisignPubKeyFile == "" {
		return fmt.Errorf("a minisign public key is required (set MINISIGN_PUBKEY or -minisign-pubkey)")
	}
	return nil
}

func envFloatOr(key string, def float64) float64 {
	v := envOr(key, "")
	if v == "" {
		return def
	}
	var f float64
	if _, err := fmt.Sscanf(v, "%g", &f); err != nil {
		return def
	}
	return f
}
