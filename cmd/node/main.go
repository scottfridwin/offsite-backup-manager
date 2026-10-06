// Command node is the backup-node agent. Not yet implemented (Phase 2); this
// stub exists so the monorepo builds both binaries from the start.
package main

import (
	"fmt"
	"os"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "-version", "--version", "-v":
			fmt.Println(version)
			return
		}
	}
	fmt.Fprintln(os.Stderr, "node agent: not yet implemented (Phase 2)")
	os.Exit(1)
}
