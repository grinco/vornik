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
	commit, err := gitHeadCommit()
	if err != nil {
		return err
	}
	if !cleanCommit.MatchString(commit) {
		// Guidance goes to stderr, not into the error value: an error string
		// is a single clause by convention (ST1005), and the operator needs
		// three lines of it.
		fmt.Fprint(os.Stderr,
			"\nA record names the build a release DECLARES, so it must name a commit anyone\n"+
				"can check out. A <sha>-dirty record describes a tree that exists on exactly\n"+
				"one machine and can never be reproduced or verified.\n\n"+
				"Commit or stash your changes, then re-run. Dev boxes need no record.\n\n")
		return fmt.Errorf("refusing to write an image record from a dirty worktree (revision %q)", commit)
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
		rec.Images = append(rec.Images, imagemanifest.ImageRecord{
			Tag: img.Tag, Digest: digest, SourceCommit: commit,
		})
	}
	rec.Count = len(rec.Images)

	body, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(body, '\n'), 0o644)
}

func gitHeadCommit() (string, error) {
	out, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("cannot resolve HEAD: %w", err)
	}
	commit := strings.TrimSpace(string(out))
	// Mirrors Makefile:18 — a modified worktree marks the revision dirty.
	if err := exec.Command("git", "diff", "--quiet").Run(); err != nil {
		commit += "-dirty"
	}
	return commit, nil
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
