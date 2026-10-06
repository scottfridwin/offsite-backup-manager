// Package config loads orchestrator configuration from environment variables,
// with optional command-line flag overrides.
package config

import (
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/scottfridwin/offsite-backup-manager/internal/manifest"
)

// Config holds all Primary-side packaging settings.
type Config struct {
	// SourceDir is the backup source tree to snapshot in full (BACKUP_SOURCE_DIR).
	SourceDir string
	// WorkDir is scratch space for staging (PACKAGE_WORK_DIR).
	WorkDir string
	// OutputDir is where the package, manifest, and signature are written (PACKAGE_OUTPUT_DIR).
	OutputDir string
	// AgeRecipients are the public age recipients to encrypt the package to (AGE_RECIPIENT).
	AgeRecipients []string
	// MinisignKeyFile is the path to the minisign secret key used to sign the manifest (MINISIGN_SECKEY).
	MinisignKeyFile string
	// MinisignPassword decrypts the minisign secret key, if it is password-protected (MINISIGN_PASSWORD).
	MinisignPassword string
	// DistributionScheme is "replicate-all" or "round-robin" (DISTRIBUTION_SCHEME, §8).
	DistributionScheme string
	// NodeRosterFile is required when DistributionScheme is "round-robin", to
	// assign the run to the next node in rotation (NODE_ROSTER_FILE).
	NodeRosterFile string
}

var recipientSep = regexp.MustCompile(`[\s,]+`)

// FromEnv builds a Config from environment variables.
func FromEnv() Config {
	return Config{
		SourceDir:          os.Getenv("BACKUP_SOURCE_DIR"),
		WorkDir:            envOr("PACKAGE_WORK_DIR", os.TempDir()),
		OutputDir:          os.Getenv("PACKAGE_OUTPUT_DIR"),
		AgeRecipients:      splitRecipients(os.Getenv("AGE_RECIPIENT")),
		MinisignKeyFile:    os.Getenv("MINISIGN_SECKEY"),
		MinisignPassword:   os.Getenv("MINISIGN_PASSWORD"),
		DistributionScheme: envOr("DISTRIBUTION_SCHEME", manifest.SchemeReplicateAll),
		NodeRosterFile:     os.Getenv("NODE_ROSTER_FILE"),
	}
}

// BindFlags registers flags that override the current (env-derived) values.
func (c *Config) BindFlags(fs *flag.FlagSet) {
	fs.StringVar(&c.SourceDir, "source", c.SourceDir, "backup source directory to snapshot (BACKUP_SOURCE_DIR)")
	fs.StringVar(&c.WorkDir, "work", c.WorkDir, "staging/scratch directory (PACKAGE_WORK_DIR)")
	fs.StringVar(&c.OutputDir, "output", c.OutputDir, "output directory for package+manifest (PACKAGE_OUTPUT_DIR)")
	fs.StringVar(&c.MinisignKeyFile, "minisign-key", c.MinisignKeyFile, "minisign secret key file (MINISIGN_SECKEY)")
	fs.StringVar(&c.DistributionScheme, "distribution-scheme", c.DistributionScheme, "replicate-all or round-robin (DISTRIBUTION_SCHEME)")
	fs.StringVar(&c.NodeRosterFile, "roster", c.NodeRosterFile, "node roster file; required for round-robin (NODE_ROSTER_FILE)")
	fs.Func("recipient", "age recipient to encrypt to; repeatable (AGE_RECIPIENT)", func(v string) error {
		c.AgeRecipients = append(c.AgeRecipients, splitRecipients(v)...)
		return nil
	})
}

// Validate checks that the required fields are present.
func (c Config) Validate() error {
	if c.SourceDir == "" {
		return fmt.Errorf("source directory is required (set BACKUP_SOURCE_DIR or -source)")
	}
	if c.OutputDir == "" {
		return fmt.Errorf("output directory is required (set PACKAGE_OUTPUT_DIR or -output)")
	}
	if len(c.AgeRecipients) == 0 {
		return fmt.Errorf("at least one age recipient is required (set AGE_RECIPIENT or -recipient)")
	}
	if c.MinisignKeyFile == "" {
		return fmt.Errorf("a minisign signing key is required (set MINISIGN_SECKEY or -minisign-key)")
	}
	switch c.DistributionScheme {
	case "", manifest.SchemeReplicateAll:
	case manifest.SchemeRoundRobin:
		if c.NodeRosterFile == "" {
			return fmt.Errorf("round-robin distribution requires a node roster file (set NODE_ROSTER_FILE or -roster)")
		}
	default:
		return fmt.Errorf("invalid distribution scheme %q (want %q or %q)", c.DistributionScheme, manifest.SchemeReplicateAll, manifest.SchemeRoundRobin)
	}
	return nil
}

func splitRecipients(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := recipientSep.Split(s, -1)
	out := parts[:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
