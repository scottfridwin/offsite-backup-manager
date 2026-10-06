// Command primary is the Primary-side backup orchestrator.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/scottfridwin/offsite-backup-manager/internal/capacity"
	"github.com/scottfridwin/offsite-backup-manager/internal/config"
	"github.com/scottfridwin/offsite-backup-manager/internal/health"
	"github.com/scottfridwin/offsite-backup-manager/internal/pipeline"
	"github.com/scottfridwin/offsite-backup-manager/internal/roster"
	"github.com/scottfridwin/offsite-backup-manager/internal/server"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "package":
		runPackage(os.Args[2:])
	case "serve":
		runServe(os.Args[2:])
	case "enroll-token":
		runEnrollToken(os.Args[2:])
	case "healthcheck":
		runHealthcheck(os.Args[2:])
	case "nodes":
		runNodes(os.Args[2:])
	case "retire-node":
		runRetireNode(os.Args[2:])
	case "capacity":
		runCapacity(os.Args[2:])
	case "version", "-version", "--version", "-v":
		fmt.Println(version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func runPackage(args []string) {
	cfg := config.FromEnv()
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
  primary package [flags]       Build one encrypted, signed backup package
  primary serve [flags]         Serve the node-facing enroll/pull/heartbeat API
  primary enroll-token [flags]  Issue a one-time node enrollment token
  primary healthcheck [flags]   Exit 0 if healthy, 1 if not (for Docker HEALTHCHECK)
  primary nodes [flags]         List enrolled nodes and their status
  primary retire-node <id>      Decommission a node (§5.4)
  primary capacity [flags]      Project storage growth and node time-to-full (§7.3)
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

enroll-token flags:
  -roster           node roster file (env NODE_ROSTER_FILE)
  -enroll-token-ttl enrollment token lifetime (env ENROLL_TOKEN_TTL)

healthcheck flags:
  -output                   directory serving published runs (env PACKAGE_OUTPUT_DIR)
  -roster                   node roster file (env NODE_ROSTER_FILE)
  -health-run-interval      max age of the last successful run before unhealthy (env HEALTH_RUN_INTERVAL)
  -health-sync-grace        grace window for nodes to confirm the latest run (env HEALTH_SYNC_GRACE)
  -node-space-critical-pct  node free-space critical threshold (env NODE_SPACE_CRITICAL_PCT)

nodes flags:
  -roster  node roster file (env NODE_ROSTER_FILE)

capacity flags:
  -output  directory serving published runs (env PACKAGE_OUTPUT_DIR)
  -roster  node roster file; omit to skip per-node projections (env NODE_ROSTER_FILE)
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

	handler := server.NewHandler(server.Config{
		OutputDir: cfg.OutputDir,
		Roster:    roster.Open(cfg.NodeRosterFile),
	})
	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      5 * time.Minute, // package downloads can be large
	}

	fmt.Printf("listening on %s (output=%s roster=%s)\n", cfg.ListenAddr, cfg.OutputDir, cfg.NodeRosterFile)
	if err := httpServer.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
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
