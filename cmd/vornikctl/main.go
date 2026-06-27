// Package main provides the CLI entrypoint for vornik CLI commands.
package main

import (
	"fmt"
	"os"

	"vornik.io/vornik/internal/cli"
)

// Build information (injected at build time via ldflags)
var (
	Version   = "dev"
	BuildDate = "unknown"
	Edition   = "" // empty → cli normalizes to community
)

func main() {
	// Set version for CLI
	cli.SetVersion(Version)
	cli.SetEdition(Edition)

	if err := cli.Execute(); err != nil {
		// Only print the error message when it is non-empty. Structured exit
		// errors (e.g. featureExitError) intentionally return "" from Error()
		// to suppress the "error: " prefix line on expected non-zero exits.
		if msg := err.Error(); msg != "" {
			fmt.Fprintf(os.Stderr, "error: %s\n", msg)
		}
		os.Exit(1)
	}
}
