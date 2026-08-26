// Command vornik-images prints the container-image manifest for shell
// consumers (vornik-update.sh, the Makefile image targets,
// install-enterprise).
//
// It exists so those paths and the daemon's image_freshness doctor check read
// the SAME rows. Before 2026-08-25 each consumer carried its own idea of which
// images exist and when they are needed, which is how the cluster tags ended
// up with no builder at all and how a CE customer went six weeks without an
// agent image.
//
// Usage:
//
//	vornik-images            # rows whose condition holds on THIS host
//	vornik-images -all       # every row, conditions ignored
package main

import (
	"flag"
	"fmt"
	"os"

	"vornik.io/vornik/internal/imagemanifest"
)

func main() {
	all := flag.Bool("all", false, "print every manifest row, ignoring host conditions")
	flag.Parse()

	images := imagemanifest.All()
	if !*all {
		images = imagemanifest.Deployable(imagemanifest.HostProber{})
	}
	if _, err := fmt.Fprint(os.Stdout, imagemanifest.EmitRows(images)); err != nil {
		fmt.Fprintf(os.Stderr, "vornik-images: %v\n", err)
		os.Exit(1)
	}
}
