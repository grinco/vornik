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
//	vornik-images -record F  # write the release image record to F
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"vornik.io/vornik/internal/imagemanifest"
)

func main() {
	all := flag.Bool("all", false, "print every manifest row, ignoring host conditions")
	record := flag.String("record", "", "write the release image record (digest + source commit per image) to this path")
	flag.Parse()

	if *record != "" {
		if err := writeRecord(*record); err != nil {
			fmt.Fprintf(os.Stderr, "vornik-images: %v\n", err)
			os.Exit(1)
		}
		return
	}

	images := imagemanifest.All()
	if !*all {
		images = imagemanifest.Deployable(imagemanifest.HostProber{})
	}
	if _, err := fmt.Fprint(os.Stdout, imagemanifest.EmitRows(images)); err != nil {
		fmt.Fprintf(os.Stderr, "vornik-images: %v\n", err)
		os.Exit(1)
	}
}

// cleanCommit matches a 40-char lowercase sha with NO -dirty suffix.
var cleanCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)

// writeRecord emits what this build DECLARES its images to be, for the package
// to ship (design §5.1).
//
// It REFUSES a dirty worktree. VORNIK_REVISION appends "-dirty" when the tree
// is modified, and a record naming such a commit describes a tree that exists
// on exactly one machine and can never be reproduced or verified by anyone —
// a release error, not a value worth recording. A dev box simply gets no
// record, which lands it on the record-absent path: the correct description of
// a dev box.
func writeRecord(path string) error {
	// THE IMAGES ARE THE SOURCE OF TRUTH, NOT THE WORKING TREE.
	//
	// This read HEAD until 2026-08-29, and the first real release run proved
	// why that is wrong: images were built at f67607ef, one commit landed
	// (touching no image), and the record then claimed source_commit 50dfce25
	// for artifacts built from something else. A provenance record that states
	// what the TREE says rather than what the ARTIFACT says is not provenance —
	// it is a plausible-looking assertion nobody can check, and the gap widens
	// every time a release does the ordinary thing of committing between
	// building and packaging.
	//
	// Each image is labelled org.opencontainers.image.revision at build time.
	// Reading that per image also makes a MIXED set self-detecting: the record's
	// own "one record describes one build" validation refuses images whose
	// commits disagree, so a stale image cannot slip into a release record.
	commit, err := imageRevision(imagemanifest.AgentImageTag)
	if err != nil {
		return fmt.Errorf("cannot read the agent image's build revision: %w\n"+
			"  Run `make build-images` first — the record describes images that exist", err)
	}
	if !cleanCommit.MatchString(commit) {
		// Guidance goes to stderr, not into the error value: an error string
		// is a single clause by convention (ST1005), and the operator needs
		// three lines of it.
		fmt.Fprint(os.Stderr,
			"\nA record names the build a release DECLARES, so it must name a commit anyone\n"+
				"can check out. A <sha>-dirty image was built from a tree that exists on\n"+
				"exactly one machine and can never be reproduced or verified.\n\n"+
				"Commit your changes, re-run `make build-images`, then record. Dev boxes\n"+
				"need no record.\n\n")
		return fmt.Errorf("refusing to write an image record: the agent image was built from "+
			"a dirty worktree (revision %q)", commit)
	}

	images := imagemanifest.All()
	rec := imagemanifest.ReleaseRecord{
		Version: imagemanifest.RecordVersion,
		Images:  make([]imagemanifest.ImageRecord, 0, len(images)),
	}
	for _, img := range images {
		digest, err := imageDigest(img.Tag)
		if err != nil {
			return fmt.Errorf("%s: %w\n"+
				"  Every manifest image must be built before the record is written — "+
				"run `make build-images` first", img.Tag, err)
		}
		rev, err := imageRevision(img.Tag)
		if err != nil {
			return fmt.Errorf("%s: cannot read its build revision: %w", img.Tag, err)
		}
		// Recorded per image on purpose. If one image is stale the commits
		// disagree, and LoadReleaseRecord refuses the whole record rather than
		// letting a release ship a mixed set described as one build.
		rec.Images = append(rec.Images, imagemanifest.ImageRecord{
			Tag: img.Tag, Digest: digest, SourceCommit: rev,
		})
	}
	rec.Count = len(rec.Images)

	body, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	// The record's usual home is dist/, which is build output and therefore
	// absent on a clean checkout — `make images-record` failed outright the
	// first time it ran for exactly this. Create the parent rather than
	// requiring the caller to, so any path works.
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("cannot create %s: %w", dir, err)
		}
	}
	return os.WriteFile(path, append(body, '\n'), 0o644)
}

func imageDigest(tag string) (string, error) {
	out, err := exec.Command("podman", "image", "inspect", tag,
		"--format", "{{.Digest}}").Output()
	if err != nil {
		return "", fmt.Errorf("cannot read digest (image not built?): %w", err)
	}
	digest := strings.TrimSpace(string(out))
	if digest == "" {
		return "", fmt.Errorf("podman reported an empty digest")
	}
	return digest, nil
}

// imageRevision reads the commit an image was BUILT from, out of the OCI
// revision label the Containerfile stamps. Authoritative in a way the working
// tree is not: it describes the artifact rather than the checkout.
func imageRevision(tag string) (string, error) {
	out, err := exec.Command("podman", "image", "inspect", tag,
		"--format", "{{index .Labels \"org.opencontainers.image.revision\"}}").Output()
	if err != nil {
		return "", fmt.Errorf("podman inspect failed (image not built?): %w", err)
	}
	rev := strings.TrimSpace(string(out))
	if rev == "" || rev == "<no value>" {
		return "", fmt.Errorf("image carries no org.opencontainers.image.revision label; " +
			"rebuild it so its provenance is recordable")
	}
	return rev, nil
}
