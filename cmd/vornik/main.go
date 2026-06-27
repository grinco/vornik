// Package main provides the Community-edition entrypoint for the Vornik daemon.
//
// This is the main that ships in the public Community-Edition module
// (github.com/grinco/vornik) per editions-phase2-module-split-design.md §2. It
// wires ONLY service.CommunityProviders() and deliberately does NOT import
// vornik.io/vornik/internal/enterprise — the edition is fixed to
// Community by construction (you cannot get enterprise capabilities from this
// binary, because the EE code is not linked in).
//
// Contrast with cmd/vornik-enterprise, the EE-assembly entrypoint, which imports
// internal/enterprise and selects providers by EDITION. At the Phase 2b clean
// export, cmd/vornik-enterprise is dropped from the CE tree and this package
// becomes the sole main of the public CE repo. The import-law test
// (internal/architecture) checks cmd/vornik for EE-freeness; cmd/vornik-enterprise is exempt.
package main

import (
	"fmt"
	"os"

	"vornik.io/vornik/internal/service"
	"vornik.io/vornik/internal/version"
)

// Build information (injected at build time via ldflags: -X main.Version, -X main.BuildDate).
// Edition is NOT an ldflag here — this binary is Community by construction.
var (
	Version   = version.Default
	BuildDate = version.UnknownBuildDate
)

func main() {
	if err := service.Run(Version, BuildDate, version.EditionCommunity, service.CommunityProviders()); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
