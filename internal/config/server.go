package config

import (
	"flag"
	"fmt"
	"time"
)

// ServerConfig holds the Primary's node-facing HTTP server settings.
type ServerConfig struct {
	// ListenAddr is the address the node-facing HTTP server binds
	// (BACKUP_LISTEN_ADDR). The reverse proxy terminates public TLS and
	// forwards to this address.
	ListenAddr string
	// OutputDir is where packaging runs are published; served read-only
	// (PACKAGE_OUTPUT_DIR, shared with the package command).
	OutputDir string
	// NodeRosterFile is the flat file tracking enrolled nodes and pending
	// enrollment tokens (NODE_ROSTER_FILE).
	NodeRosterFile string
	// EnrollTokenTTL is how long a freshly issued enrollment token remains
	// redeemable (ENROLL_TOKEN_TTL).
	EnrollTokenTTL time.Duration
	// Schedule is a cron expression (standard 5-field crontab syntax) for
	// self-scheduled packaging runs (SCHEDULE, §9.1). Empty disables self-
	// scheduling -- trigger `primary package` externally instead (host cron,
	// Komodo scheduled task, Kubernetes CronJob, etc.).
	Schedule string
}

// ServerConfigFromEnv builds a ServerConfig from environment variables.
func ServerConfigFromEnv() ServerConfig {
	return ServerConfig{
		ListenAddr:     envOr("BACKUP_LISTEN_ADDR", ":8080"),
		OutputDir:      envOr("PACKAGE_OUTPUT_DIR", ""),
		NodeRosterFile: envOr("NODE_ROSTER_FILE", "roster.json"),
		EnrollTokenTTL: envDurationOr("ENROLL_TOKEN_TTL", 24*time.Hour),
		Schedule:       envOr("SCHEDULE", ""),
	}
}

// BindFlags registers flags that override the current (env-derived) values.
func (c *ServerConfig) BindFlags(fs *flag.FlagSet) {
	fs.StringVar(&c.ListenAddr, "listen", c.ListenAddr, "address to listen on (BACKUP_LISTEN_ADDR)")
	fs.StringVar(&c.OutputDir, "output", c.OutputDir, "directory serving published runs (PACKAGE_OUTPUT_DIR)")
	fs.StringVar(&c.NodeRosterFile, "roster", c.NodeRosterFile, "node roster file (NODE_ROSTER_FILE)")
	fs.DurationVar(&c.EnrollTokenTTL, "enroll-token-ttl", c.EnrollTokenTTL, "enrollment token lifetime (ENROLL_TOKEN_TTL)")
	fs.StringVar(&c.Schedule, "schedule", c.Schedule, "cron expression for self-scheduled packaging runs; empty disables (SCHEDULE)")
}

// Validate checks that the required fields are present.
func (c ServerConfig) Validate() error {
	if c.OutputDir == "" {
		return fmt.Errorf("output directory is required (set PACKAGE_OUTPUT_DIR or -output)")
	}
	if c.NodeRosterFile == "" {
		return fmt.Errorf("a node roster file is required (set NODE_ROSTER_FILE or -roster)")
	}
	return nil
}

func envDurationOr(key string, def time.Duration) time.Duration {
	v := envOr(key, "")
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}
