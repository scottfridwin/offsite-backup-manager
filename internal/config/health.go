package config

import (
	"flag"
	"fmt"
	"time"
)

// HealthConfig holds settings for the Primary's health evaluation (§9.2),
// surfaced via `primary healthcheck` for a Docker HEALTHCHECK.
type HealthConfig struct {
	// OutputDir is where packaging runs are published (PACKAGE_OUTPUT_DIR).
	OutputDir string
	// NodeRosterFile is the flat file tracking enrolled nodes (NODE_ROSTER_FILE).
	NodeRosterFile string
	// RunInterval is the max age of the last successful run before unhealthy
	// (HEALTH_RUN_INTERVAL).
	RunInterval time.Duration
	// SyncGrace is the grace window for required nodes to confirm the latest
	// run before unhealthy (HEALTH_SYNC_GRACE).
	SyncGrace time.Duration
	// SpaceCriticalPct is the node free-space level that drives the Primary
	// unhealthy, via heartbeat (NODE_SPACE_CRITICAL_PCT).
	SpaceCriticalPct float64
	// Schedule is the cron expression the primary self-schedules on (SCHEDULE);
	// when set, run freshness is judged against expected scheduled occurrences.
	Schedule string
}

// HealthConfigFromEnv builds a HealthConfig from environment variables.
func HealthConfigFromEnv() HealthConfig {
	return HealthConfig{
		OutputDir:        envOr("PACKAGE_OUTPUT_DIR", ""),
		NodeRosterFile:   envOr("NODE_ROSTER_FILE", "roster.json"),
		RunInterval:      envDurationOr("HEALTH_RUN_INTERVAL", 48*time.Hour),
		SyncGrace:        envDurationOr("HEALTH_SYNC_GRACE", 6*time.Hour),
		SpaceCriticalPct: envFloatOr("NODE_SPACE_CRITICAL_PCT", 90),
		Schedule:         envOr("SCHEDULE", ""),
	}
}

// BindFlags registers flags that override the current (env-derived) values.
func (c *HealthConfig) BindFlags(fs *flag.FlagSet) {
	fs.StringVar(&c.OutputDir, "output", c.OutputDir, "directory serving published runs (PACKAGE_OUTPUT_DIR)")
	fs.StringVar(&c.NodeRosterFile, "roster", c.NodeRosterFile, "node roster file (NODE_ROSTER_FILE)")
	fs.DurationVar(&c.RunInterval, "health-run-interval", c.RunInterval, "max age of the last successful run before unhealthy (HEALTH_RUN_INTERVAL)")
	fs.DurationVar(&c.SyncGrace, "health-sync-grace", c.SyncGrace, "grace window for nodes to confirm the latest run (HEALTH_SYNC_GRACE)")
	fs.Float64Var(&c.SpaceCriticalPct, "node-space-critical-pct", c.SpaceCriticalPct, "node free-space critical threshold (NODE_SPACE_CRITICAL_PCT)")
	fs.StringVar(&c.Schedule, "schedule", c.Schedule, "cron expression the primary self-schedules on (SCHEDULE)")
}

// Validate checks that the required fields are present.
func (c HealthConfig) Validate() error {
	if c.OutputDir == "" {
		return fmt.Errorf("output directory is required (set PACKAGE_OUTPUT_DIR or -output)")
	}
	if c.NodeRosterFile == "" {
		return fmt.Errorf("a node roster file is required (set NODE_ROSTER_FILE or -roster)")
	}
	return nil
}
