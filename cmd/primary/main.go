// Command primary is the Primary-side backup orchestrator.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/scottfridwin/offsite-backup-manager/internal/config"
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
  primary version               Print the version
  primary help                  Show this help

package flags:
  -source        backup source directory (env BACKUP_SOURCE_DIR)
  -output        output directory (env PACKAGE_OUTPUT_DIR)
  -work          staging directory (env PACKAGE_WORK_DIR)
  -recipient     age recipient, repeatable (env AGE_RECIPIENT)
  -minisign-key  minisign secret key file (env MINISIGN_SECKEY)

serve flags:
  -listen           listen address (env BACKUP_LISTEN_ADDR, default :8080)
  -output           directory serving published runs (env PACKAGE_OUTPUT_DIR)
  -roster           node roster file (env NODE_ROSTER_FILE)
  -enroll-token-ttl enrollment token lifetime (env ENROLL_TOKEN_TTL)

enroll-token flags:
  -roster           node roster file (env NODE_ROSTER_FILE)
  -enroll-token-ttl enrollment token lifetime (env ENROLL_TOKEN_TTL)
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
