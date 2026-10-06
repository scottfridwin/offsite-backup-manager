// Command primary is the Primary-side backup orchestrator.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/scottfridwin/offsite-backup-manager/internal/config"
	"github.com/scottfridwin/offsite-backup-manager/internal/pipeline"
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
  primary package [flags]   Build one encrypted, signed backup package
  primary version           Print the version
  primary help              Show this help

package flags:
  -source        backup source directory (env BACKUP_SOURCE_DIR)
  -output        output directory (env PACKAGE_OUTPUT_DIR)
  -work          staging directory (env PACKAGE_WORK_DIR)
  -recipient     age recipient, repeatable (env AGE_RECIPIENT)
  -minisign-key  minisign secret key file (env MINISIGN_SECKEY)
`, version)
}
