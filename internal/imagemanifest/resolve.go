package imagemanifest

import (
	"errors"
	"fmt"
	"strings"
)

// Target resolution for the Stage 2 obtain path (design §S2.2).
//
// EVERY OBTAIN PULLS BY DIGEST. A tag is only ever used to RESOLVE one. A GHCR
// tag is mutable — it can be overwritten by a later run at the same commit, or
// deleted and re-pushed with different content — so a commit-NAMED tag is not
// commit-ADDRESSED. Resolving it here and pulling the result means the fetched
// artifact is exactly named and the host can record what it actually got.
//
// The residual exposure — a tag moved between two hosts' resolutions — is not
// closable by the update path. It belongs to the attestation work
// (editions-phase2-leak-surface-inventory.md F2/F3) and is stated in the design
// rather than papered over here.

// ErrReferenceAbsent reports that the registry answered and has no such
// reference.
//
// DISTINCT FROM AN UNREACHABLE REGISTRY, and the distinction is the whole point.
// "No such tag" is an ANSWER — this commit was never published, so build. "I
// could not ask" is not an answer, and rebuilding on it would churn forever and
// replace a verifiable pulled image with an unverifiable local one (§S2.3).
var ErrReferenceAbsent = errors.New("reference absent from registry")

// DigestLookup resolves a registry reference to its digest for this host's
// platform. It returns ErrReferenceAbsent when the registry answered and has no
// such reference; any other error means the registry could not be consulted.
type DigestLookup func(ref string) (digest string, err error)

// commitTagPrefix is the tag namespace the CE publish workflow writes:
// `sha-<first 12 of the CE commit>` (.github/workflows/publish-agent-image.yml,
// injected into the public repo by scripts/export-public-ce.sh).
const commitTagPrefix = "sha-"

// commitTagLen is how many characters of the commit the publish workflow uses
// (`${GITHUB_SHA::12}`). Named because a mismatch here resolves nothing and the
// symptom — every update builds — looks like a registry outage.
const commitTagLen = 12

// CommitTag returns the commit-addressed reference for a repository.
func CommitTag(tag, commit string) string {
	repo, _, _ := strings.Cut(tag, ":")
	if len(commit) > commitTagLen {
		commit = commit[:commitTagLen]
	}
	return repo + ":" + commitTagPrefix + commit
}

// ResolveTarget works out what this host should be holding for one image.
//
// Order, and why:
//
//  1. A host-built tag never consults the registry at all. The broker, scraper
//     and cluster images are published nowhere, and making their update path
//     depend on GHCR would be a new outage surface for no benefit (§S2.7).
//  2. The release record wins when it names this image: it is what the release
//     DECLARED, it is per-architecture, and it needs no network round-trip.
//  3. Otherwise the commit-addressed tag. This is the CE path, where no record
//     ships and the checkout's HEAD *is* a CE commit.
//
// A record that does not mention the image falls through to (3) rather than
// being read as "nothing published": the record describes one release's images,
// not every image a host might hold.
func ResolveTarget(tag string, rec *ReleaseRecord, arch, headCommit string, lookup DigestLookup) (Target, error) {
	target := Target{Commit: headCommit}

	if !IsRegistryTag(tag) {
		return target, nil
	}

	if rec != nil {
		for _, img := range rec.Images {
			if img.Tag != tag {
				continue
			}
			// The record answered about this image. An architecture it did not
			// publish yields an empty digest — reported honestly rather than
			// borrowing another platform's, which would compare a host against
			// an image it cannot run.
			digest, _ := img.DigestForArch(arch)
			target.RegistryReached = true
			target.Digest = digest
			return target, nil
		}
	}

	if lookup == nil {
		// No way to ask. Not the same as an answer.
		return target, nil
	}

	digest, err := lookup(CommitTag(tag, headCommit))
	switch {
	case err == nil:
		target.RegistryReached = true
		target.Digest = digest
	case errors.Is(err, ErrReferenceAbsent):
		// The registry answered: this commit was never published.
		target.RegistryReached = true
	default:
		// Could not consult it. RegistryReached stays false, which is what
		// routes an existing image to ActionLeave rather than a rebuild.
	}
	return target, nil
}

// ClassifyLookupError turns a registry client's failure into either
// ErrReferenceAbsent or a transport error, so ResolveTarget's caller does not
// have to.
//
// Matched on the message because skopeo reports both through the same non-zero
// exit. The mapping is deliberately CONSERVATIVE: anything not recognisably a
// "no such thing" answer is treated as unreachable, because that direction is
// safe (leave the image alone) while the other silently rebuilds over a
// verifiable artifact.
func ClassifyLookupError(err error, stderr string) error {
	if err == nil {
		return nil
	}
	s := strings.ToLower(stderr)
	for _, absent := range []string{
		"manifest unknown",
		"name unknown",
		"not found",
		"no such",
		"repository name not known",
		"unauthorized", // a private/absent repo answers this way
	} {
		if strings.Contains(s, absent) {
			return fmt.Errorf("%w: %s", ErrReferenceAbsent, strings.TrimSpace(stderr))
		}
	}
	return err
}
