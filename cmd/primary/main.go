// Command primary is the Primary-side backup orchestrator.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/scottfridwin/offsite-backup-manager/internal/capacity"
	"github.com/scottfridwin/offsite-backup-manager/internal/config"
	"github.com/scottfridwin/offsite-backup-manager/internal/cryptoutil"
	"github.com/scottfridwin/offsite-backup-manager/internal/health"
	"github.com/scottfridwin/offsite-backup-manager/internal/pipeline"
	"github.com/scottfridwin/offsite-backup-manager/internal/provision"
	"github.com/scottfridwin/offsite-backup-manager/internal/roster"
	"github.com/scottfridwin/offsite-backup-manager/internal/server"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	// Default to "serve" (the image's primary function) so container usage
	// needs no explicit command/CLI needs no subcommand for the common case.
	cmd, args := "serve", os.Args[1:]
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}

	switch cmd {
	case "package":
		runPackage(args)
	case "serve":
		runServe(args)
	case "enroll-token":
		runEnrollToken(args)
	case "provision-node":
		runProvisionNode(args)
	case "healthcheck":
		runHealthcheck(args)
	case "nodes":
		runNodes(args)
	case "retire-node":
		runRetireNode(args)
	case "capacity":
		runCapacity(args)
	case "keygen":
		runKeygen(args)
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

func runPackage(args []string) {
	cfg, err := config.FromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fs := flag.NewFlagSet("package", flag.ExitOnError)
	cfg.BindFlags(fs)
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	res, err := pipeline.Run(cfg, version)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	fmt.Printf("run %s\n", res.RunID)
	fmt.Printf("  package   %s (%d bytes, sha256 %s)\n", res.PackagePath, res.Manifest.Package.Bytes, res.Manifest.Package.SHA256)
	fmt.Printf("  manifest  %s\n", res.ManifestPath)
	fmt.Printf("  signature %s\n", res.SignaturePath)
}

func usage() {
	fmt.Fprintf(os.Stderr, `offsite-backup-manager (primary) %s

Usage:
  primary [serve flags]         Serve the node-facing enroll/pull/heartbeat API (default)
  primary package [flags]       Build one encrypted, signed backup package
  primary enroll-token [flags]  Issue a one-time node enrollment token
  primary provision-node [flags] Render a zero-touch Raspberry Pi provisioning bundle
  primary healthcheck [flags]   Exit 0 if healthy, 1 if not (for Docker HEALTHCHECK)
  primary nodes [flags]         List enrolled nodes and their status
  primary retire-node <id>      Decommission a node (§5.4)
  primary capacity [flags]      Project storage growth and node time-to-full (§7.3)
  primary keygen [flags]        Generate an age identity + a minisign keypair
  primary version               Print the version
  primary help                  Show this help

package flags:
  -source              backup source directory (env BACKUP_SOURCE_DIR)
  -output              output directory (env PACKAGE_OUTPUT_DIR)
  -work                staging directory (env PACKAGE_WORK_DIR)
  -recipient            age recipient, repeatable (env AGE_RECIPIENT)
  -minisign-key         minisign secret key file (env MINISIGN_SECKEY)
  -distribution-scheme  replicate-all or round-robin (env DISTRIBUTION_SCHEME)
  -roster               node roster file; required for round-robin (env NODE_ROSTER_FILE)

serve flags:
  -listen           listen address (env BACKUP_LISTEN_ADDR, default :8080)
  -output           directory serving published runs (env PACKAGE_OUTPUT_DIR)
  -roster           node roster file (env NODE_ROSTER_FILE)
  -enroll-token-ttl enrollment token lifetime (env ENROLL_TOKEN_TTL)
  -schedule         cron expression for self-scheduled packaging runs; empty
                    disables (env SCHEDULE). When set, serve also needs every
                    'package' env var (BACKUP_SOURCE_DIR, AGE_RECIPIENT, etc.)

enroll-token flags:
  -roster           node roster file (env NODE_ROSTER_FILE)
  -enroll-token-ttl enrollment token lifetime (env ENROLL_TOKEN_TTL)

provision-node flags:
  -label            node label to register (required)
  -out              directory to write the provisioning bundle into (required)
  -roster           node roster file (env NODE_ROSTER_FILE)
  -backup-host      Primary endpoint the node pulls from (env BACKUP_HOST)
  -minisign-key     minisign secret key file; its public key is embedded (env MINISIGN_SECKEY)
  -node-image       node container image reference (default matches this version)
  -node-user        unprivileged login user the node runs as (default backupnode)
  -store            node append-only store path (default /srv/backup-node/store)
  -pull-interval    how often the node polls (default 1h)
  -capacity-warn-pct node free-space warning threshold (default 90)
  -enroll-token-ttl enrollment token lifetime (default 4h; baked onto the card)

healthcheck flags:
  -output                   directory serving published runs (env PACKAGE_OUTPUT_DIR)
  -roster                   node roster file (env NODE_ROSTER_FILE)
  -schedule                 cron the primary self-schedules on; judges run freshness (env SCHEDULE)
  -health-run-interval      max age of the last run before unhealthy when no schedule is set (env HEALTH_RUN_INTERVAL)
  -health-sync-grace        grace window for nodes to confirm the latest run (env HEALTH_SYNC_GRACE)
  -node-space-critical-pct  node free-space critical threshold (env NODE_SPACE_CRITICAL_PCT)

nodes flags:
  -roster  node roster file (env NODE_ROSTER_FILE)

capacity flags:
  -output  directory serving published runs (env PACKAGE_OUTPUT_DIR)
  -roster  node roster file; omit to skip per-node projections (env NODE_ROSTER_FILE)

keygen flags:
  -out                directory to write the generated keys into (default .)
  -minisign-password  passphrase for the minisign secret key; empty generates
                      an unprotected key (env MINISIGN_PASSWORD or _FILE)
`, version)
}

func runServe(args []string) {
	cfg := config.ServerConfigFromEnv()
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfg.BindFlags(fs)
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	// Create the output directory ourselves so a freshly deployed primary is
	// self-sufficient (no external orchestration script needed) and its
	// healthcheck can read it before the first run.
	if err := os.MkdirAll(cfg.OutputDir, 0o750); err != nil {
		fmt.Fprintln(os.Stderr, "error: cannot create output directory:", err)
		os.Exit(1)
	}

	handler := server.NewHandler(server.Config{
		OutputDir: cfg.OutputDir,
		Roster:    roster.Open(cfg.NodeRosterFile),
	})
	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// Fallback write cap for the small control endpoints; the package
		// download handler overrides this with its own idle write deadline so
		// multi-GB transfers are bounded by lack of progress, not total time.
		WriteTimeout: 5 * time.Minute,
	}

	scheduler := startScheduler(cfg)
	if scheduler != nil {
		defer scheduler.Stop()
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		fmt.Printf("listening on %s (output=%s roster=%s)\n", cfg.ListenAddr, cfg.OutputDir, cfg.NodeRosterFile)
		errCh <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintln(os.Stderr, "error during shutdown:", err)
		}
	}
}

// startScheduler starts the opt-in self-scheduler (§9.1) when cfg.Schedule is
// set, triggering `pipeline.Run` on its own cron schedule from the same
// env-derived packaging config `primary package` would use. Returns nil (and
// does nothing) when scheduling is disabled.
func startScheduler(cfg config.ServerConfig) *cron.Cron {
	if cfg.Schedule == "" {
		return nil
	}

	pkgCfg, err := config.FromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: SCHEDULE is set but packaging config is invalid:", err)
		os.Exit(1)
	}
	if err := pkgCfg.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, "error: SCHEDULE is set but packaging config is invalid:", err)
		os.Exit(1)
	}

	c := cron.New()
	_, err = c.AddFunc(cfg.Schedule, func() {
		res, err := pipeline.Run(pkgCfg, version)
		if err != nil {
			fmt.Fprintln(os.Stderr, "scheduled run failed:", err)
			return
		}
		fmt.Printf("scheduled run %s published\n", res.RunID)
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: invalid SCHEDULE cron expression:", err)
		os.Exit(1)
	}
	c.Start()
	fmt.Printf("self-scheduling packaging runs: %s\n", cfg.Schedule)
	return c
}

func runEnrollToken(args []string) {
	cfg := config.ServerConfigFromEnv()
	fs := flag.NewFlagSet("enroll-token", flag.ExitOnError)
	fs.StringVar(&cfg.NodeRosterFile, "roster", cfg.NodeRosterFile, "node roster file (NODE_ROSTER_FILE)")
	fs.DurationVar(&cfg.EnrollTokenTTL, "enroll-token-ttl", cfg.EnrollTokenTTL, "enrollment token lifetime (ENROLL_TOKEN_TTL)")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if cfg.NodeRosterFile == "" {
		fmt.Fprintln(os.Stderr, "error: a node roster file is required (set NODE_ROSTER_FILE or -roster)")
		os.Exit(1)
	}

	token, err := roster.Open(cfg.NodeRosterFile).IssueEnrollmentToken(cfg.EnrollTokenTTL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	fmt.Println(token)
	fmt.Fprintf(os.Stderr, "expires in %s; set as ENROLLMENT_TOKEN on the new node before first boot\n", cfg.EnrollTokenTTL)
}

// runProvisionNode issues a short-lived enrollment token and renders a
// zero-touch Raspberry Pi provisioning bundle (dropped on the SD card's boot
// partition) so a new node self-provisions on first boot with no SSH.
func runProvisionNode(args []string) {
	srvCfg := config.ServerConfigFromEnv()
	pkgCfg, err := config.FromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	var (
		label      string
		backupHost = os.Getenv("BACKUP_HOST")
		nodeImage  = "ghcr.io/scottfridwin/offsite-backup-manager-node:" + version
		outDir     string
		nodeUser   = "backupnode"
		storeDir   = "/srv/backup-node/store"
		pullEvery  = "1h"
		warnPct    = 90
		ttl        = 4 * time.Hour
	)
	fs := flag.NewFlagSet("provision-node", flag.ExitOnError)
	fs.StringVar(&srvCfg.NodeRosterFile, "roster", srvCfg.NodeRosterFile, "node roster file (NODE_ROSTER_FILE)")
	fs.StringVar(&pkgCfg.MinisignKeyFile, "minisign-key", pkgCfg.MinisignKeyFile, "minisign secret key file; its public key is embedded (MINISIGN_SECKEY)")
	fs.StringVar(&label, "label", "", "node label to register (required)")
	fs.StringVar(&backupHost, "backup-host", backupHost, "Primary endpoint the node pulls from (BACKUP_HOST)")
	fs.StringVar(&nodeImage, "node-image", nodeImage, "node container image reference")
	fs.StringVar(&outDir, "out", "", "directory to write the provisioning bundle into (required)")
	fs.StringVar(&nodeUser, "node-user", nodeUser, "unprivileged login user the node runs as")
	fs.StringVar(&storeDir, "store", storeDir, "node append-only store path")
	fs.StringVar(&pullEvery, "pull-interval", pullEvery, "how often the node polls")
	fs.IntVar(&warnPct, "capacity-warn-pct", warnPct, "node free-space warning threshold")
	fs.DurationVar(&ttl, "enroll-token-ttl", ttl, "enrollment token lifetime (kept short; the token is baked onto the card)")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	switch {
	case srvCfg.NodeRosterFile == "":
		fmt.Fprintln(os.Stderr, "error: a node roster file is required (set NODE_ROSTER_FILE or -roster)")
		os.Exit(1)
	case label == "":
		fmt.Fprintln(os.Stderr, "error: -label is required")
		os.Exit(1)
	case backupHost == "":
		fmt.Fprintln(os.Stderr, "error: a backup host is required (set BACKUP_HOST or -backup-host)")
		os.Exit(1)
	case outDir == "":
		fmt.Fprintln(os.Stderr, "error: -out is required")
		os.Exit(1)
	case pkgCfg.MinisignKeyFile == "":
		fmt.Fprintln(os.Stderr, "error: a minisign secret key is required (set MINISIGN_SECKEY or -minisign-key)")
		os.Exit(1)
	}

	signer, err := cryptoutil.LoadSigner(pkgCfg.MinisignKeyFile, pkgCfg.MinisignPassword)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	pubKey, err := signer.PublicKeyText()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	token, err := roster.Open(srvCfg.NodeRosterFile).IssueEnrollmentToken(ttl)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	files, err := provision.Render(provision.Bundle{
		BackupHost:      backupHost,
		NodeLabel:       label,
		NodeImage:       nodeImage,
		EnrollmentToken: token,
		MinisignPubKey:  pubKey,
		NodeUser:        nodeUser,
		StoreDir:        storeDir,
		PullInterval:    pullEvery,
		CapacityWarnPct: warnPct,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	if err := os.MkdirAll(outDir, 0o750); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	for name, content := range files {
		mode := os.FileMode(0o644)
		if name == "user-data" {
			mode = 0o600 // contains the one-time enrollment token
		}
		if err := os.WriteFile(filepath.Join(outDir, name), []byte(content), mode); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	}

	fmt.Printf("provisioning bundle for node %q written to %s\n", label, outDir)
	fmt.Printf("  enrollment token expires in %s\n", ttl)
	fmt.Println("next: flash Raspberry Pi OS Lite (64-bit), then copy user-data + meta-data")
	fmt.Printf("  from %s onto the card's 'bootfs' partition and boot the Pi.\n", outDir)
}

func runHealthcheck(args []string) {
	cfg := config.HealthConfigFromEnv()
	fs := flag.NewFlagSet("healthcheck", flag.ExitOnError)
	cfg.BindFlags(fs)
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	res, err := health.Evaluate(health.Config{
		OutputDir:        cfg.OutputDir,
		Roster:           roster.Open(cfg.NodeRosterFile),
		Schedule:         cfg.Schedule,
		RunInterval:      cfg.RunInterval,
		SyncGrace:        cfg.SyncGrace,
		SpaceCriticalPct: cfg.SpaceCriticalPct,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if res.Healthy {
		fmt.Println("healthy")
		return
	}
	fmt.Println("unhealthy:")
	for _, reason := range res.Reasons {
		fmt.Println("  -", reason)
	}
	os.Exit(1)
}

func runNodes(args []string) {
	cfg := config.ServerConfigFromEnv()
	fs := flag.NewFlagSet("nodes", flag.ExitOnError)
	fs.StringVar(&cfg.NodeRosterFile, "roster", cfg.NodeRosterFile, "node roster file (NODE_ROSTER_FILE)")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if cfg.NodeRosterFile == "" {
		fmt.Fprintln(os.Stderr, "error: a node roster file is required (set NODE_ROSTER_FILE or -roster)")
		os.Exit(1)
	}

	nodes, err := roster.Open(cfg.NodeRosterFile).AllNodes()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if len(nodes) == 0 {
		fmt.Println("no enrolled nodes")
		return
	}
	for _, n := range nodes {
		lastSeen := "never"
		if !n.LastSeen.IsZero() {
			lastSeen = n.LastSeen.Format(time.RFC3339)
		}
		fmt.Printf("%s  %-10s  %-8s  last_seen=%-20s  last_run=%s\n", n.ID, n.Label, n.Status, lastSeen, n.LastRunID)
	}
}

func runRetireNode(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: primary retire-node [-roster FILE] <node-id>")
		os.Exit(2)
	}
	cfg := config.ServerConfigFromEnv()
	fs := flag.NewFlagSet("retire-node", flag.ExitOnError)
	fs.StringVar(&cfg.NodeRosterFile, "roster", cfg.NodeRosterFile, "node roster file (NODE_ROSTER_FILE)")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if cfg.NodeRosterFile == "" {
		fmt.Fprintln(os.Stderr, "error: a node roster file is required (set NODE_ROSTER_FILE or -roster)")
		os.Exit(1)
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: primary retire-node [-roster FILE] <node-id>")
		os.Exit(2)
	}

	if err := roster.Open(cfg.NodeRosterFile).Retire(fs.Arg(0)); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Printf("retired node %s\n", fs.Arg(0))
}

func runCapacity(args []string) {
	cfg := config.ServerConfigFromEnv()
	fs := flag.NewFlagSet("capacity", flag.ExitOnError)
	fs.StringVar(&cfg.OutputDir, "output", cfg.OutputDir, "directory serving published runs (PACKAGE_OUTPUT_DIR)")
	fs.StringVar(&cfg.NodeRosterFile, "roster", cfg.NodeRosterFile, "node roster file (NODE_ROSTER_FILE)")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if cfg.OutputDir == "" {
		fmt.Fprintln(os.Stderr, "error: output directory is required (set PACKAGE_OUTPUT_DIR or -output)")
		os.Exit(1)
	}

	proj, err := capacity.Compute(cfg.OutputDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if proj.RunCount == 0 {
		fmt.Println("no published runs yet; nothing to project")
		return
	}

	fmt.Printf("runs published:     %d\n", proj.RunCount)
	fmt.Printf("avg package size:   %d bytes\n", proj.AvgPackageBytes)
	if !proj.HasCadence {
		fmt.Println("avg cadence:        insufficient history (need at least 2 runs)")
	} else {
		fmt.Printf("avg cadence:        %s\n", proj.AvgCadence.Round(time.Minute))
		fmt.Printf("projected growth/yr: %d bytes\n", proj.GrowthPerYr)
	}

	if cfg.NodeRosterFile == "" {
		return
	}
	nodes, err := roster.Open(cfg.NodeRosterFile).ActiveNodes()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if len(nodes) == 0 {
		return
	}
	fmt.Println()
	fmt.Println("node time-to-full (from last reported free space):")
	for _, np := range capacity.NodeProjections(proj, nodes) {
		if !np.HasEstimate {
			fmt.Printf("  %s (%s): free=%d bytes, insufficient data to project\n", np.Label, np.NodeID, np.FreeBytes)
			continue
		}
		fmt.Printf("  %s (%s): free=%d bytes, ~%s until full\n", np.Label, np.NodeID, np.FreeBytes, np.TimeToFull.Round(time.Hour))
	}
}

func runKeygen(args []string) {
	var outDir, minisignPassword string
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	fs.StringVar(&outDir, "out", ".", "directory to write the generated keys into")
	fs.StringVar(&minisignPassword, "minisign-password", "", "passphrase for the minisign secret key; empty generates an unprotected key (env MINISIGN_PASSWORD or MINISIGN_PASSWORD_FILE)")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if minisignPassword == "" {
		pw, err := config.SecretFromEnv("MINISIGN_PASSWORD")
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		minisignPassword = pw
	}

	if err := os.MkdirAll(outDir, 0o750); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	identity, recipient, err := cryptoutil.GenerateAgeIdentity()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	identityPath := filepath.Join(outDir, "age-identity.txt")
	identityFile := fmt.Sprintf("# created by: primary keygen\n# public key: %s\n%s\n", recipient, identity)
	if err := os.WriteFile(identityPath, []byte(identityFile), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	pubText, secText, err := cryptoutil.GenerateMinisignKeyPair(minisignPassword)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	pubPath := filepath.Join(outDir, "minisign.pub")
	secPath := filepath.Join(outDir, "minisign.key")
	if err := os.WriteFile(pubPath, pubText, 0o644); err != nil { //nolint:gosec // public key, world-readable is fine
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(secPath, secText, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	fmt.Printf("wrote %s (age identity -- move this to your password manager, keep it off the Primary)\n", identityPath)
	fmt.Printf("wrote %s (minisign secret key -- stays on the Primary, MINISIGN_SECKEY)\n", secPath)
	fmt.Printf("wrote %s (minisign public key -- copy to every node, MINISIGN_PUBKEY)\n", pubPath)
	if minisignPassword == "" {
		fmt.Fprintln(os.Stderr, "warning: minisign secret key is unprotected; set -minisign-password (or MINISIGN_PASSWORD/_FILE) to encrypt it")
	}
	fmt.Println()
	fmt.Printf("AGE_RECIPIENT=%s\n", recipient)
}
