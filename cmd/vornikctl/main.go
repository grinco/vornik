// Package main provides the CLI entrypoint for vornik CLI commands.
package main

import (
	"fmt"
	"os"

	"vornik.io/vornik/internal/cli"
	"vornik.io/vornik/internal/version"
)

// Build information (injected at build time via ldflags).
//
// These use the shared version constants rather than local "dev"/"unknown"
// literals: version.Resolve decides a build is unstamped by comparing against
// version.Default, so a divergent sentinel here would read as a real version
// and suppress the VCS fallback.
var (
	Version   = version.Default
	BuildDate = version.UnknownBuildDate
	Edition   = "" // empty → cli normalizes to community
)

func main() {
	// Recover the commit from embedded VCS metadata when the build did not
	// stamp a version. See cmd/vornik-enterprise/main.go.
	Version, BuildDate = version.Resolve(Version, BuildDate)

	// Set version for CLI
	cli.SetVersion(Version)
	cli.SetBuildDate(BuildDate)
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
