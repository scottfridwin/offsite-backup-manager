// Command node is the backup-node agent: it enrolls once with a one-time
// bootstrap token, then periodically pulls new runs and reports a heartbeat.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/scottfridwin/offsite-backup-manager/internal/agent"
	"github.com/scottfridwin/offsite-backup-manager/internal/config"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	// Default to "run" (the image's primary function) so container usage
	// needs no explicit command/CLI needs no subcommand for the common case.
	cmd, args := "run", os.Args[1:]
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}

	switch cmd {
	case "run":
		runAgent(args)
	case "healthcheck":
		// Liveness only (the process can exec): the richer run-confirmation
		// health gating in docs/requirements.md §9.2 lives on the Primary.
		fmt.Println("ok")
	case "version", "-version", "--version", "-v":
		fmt.Println(version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func runAgent(args []string) {
	cfg, err := config.NodeConfigFromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfg.BindFlags(fs)
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	a, err := agent.New(agent.Config{
		BackupHost:          cfg.BackupHost,
		EnrollmentToken:     cfg.EnrollmentToken,
		NodeLabel:           cfg.NodeLabel,
		PullInterval:        cfg.PullInterval,
		StoreDir:            cfg.StoreDir,
		MinisignPubKeyFile:  cfg.MinisignPubKeyFile,
		CapacityWarnPct:     cfg.CapacityWarnPct,
		DownloadIdleTimeout: cfg.DownloadIdleTimeout,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fmt.Printf("node agent %s starting: host=%s store=%s interval=%s\n", version, cfg.BackupHost, cfg.StoreDir, cfg.PullInterval)
	if err := a.Loop(ctx); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `offsite-backup-manager (node) %s

Usage:
  node [run flags]    Enroll (if needed) and run the pull + heartbeat loop (default)
  node healthcheck    Exit 0 (liveness only, for Docker HEALTHCHECK)
  node version        Print the version
  node help           Show this help

run flags:
  -backup-host       Primary endpoint to pull from (env BACKUP_HOST)
  -enrollment-token  one-time bootstrap token (env ENROLLMENT_TOKEN)
  -label             human-friendly node name (env NODE_LABEL)
  -pull-interval     how often to poll for new runs (env PULL_INTERVAL)
  -store             append-only package store directory (env STORE_DIR)
  -minisign-pubkey   minisign public key file (env MINISIGN_PUBKEY)
  -capacity-warn-pct local free-space warning threshold (env CAPACITY_WARN_PCT)
`, version)
}
