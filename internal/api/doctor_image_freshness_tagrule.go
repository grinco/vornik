package api

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"time"

	"vornik.io/vornik/internal/imagemanifest"
)

// The tag rule on the record-absent path. See
// https://docs.vornik.io,
// amendment 2026-09-18 (E1/E2).
//
// WHY THIS EXISTS. legacyFreshness compared every deployable image's revision
// label against the daemon's. That is right for a host-built image and
// structurally impossible for a published one: the agent image is built in the
// CE repo, so its label carries a CE commit while the daemon carries an EE
// commit, and the two are unrelated as strings by construction. Measured from
// the registry 2026-09-18: revision a24d62e5583c, version main-a24d62e5583c.
// The check therefore reported a half-applied deploy for a correct one, and
// printed a remedy that rebuilt seven images to fix nothing.
//
// imagemanifest.Decide already holds the rule this catches up with: the branch
// follows the TAG, not the image's provenance, because "how did this image get
// here" is not recoverable by inspection.

// freshnessBudget is ONE deadline covering every image, not per image. A host
// with DNS but no route to the registry is the case that makes a per-image
// timeout feel like a hang, and doctor's value is being runnable mid-incident.
const freshnessBudget = 5 * time.Second

// notVerifiedReason distinguishes the three ways a registry lookup fails to
// answer. They are one bucket in the aggregate and three different operator
// actions, so the per-image line carries which one happened.
type notVerifiedReason string

const (
	reasonUnreachable notVerifiedReason = "registry unreachable"
	reasonNoSkopeo    notVerifiedReason = "skopeo absent"
	reasonBudget      notVerifiedReason = "budget exhausted"
)

// registryFreshness decides one registry-tagged image on the record-absent
// path. It returns the bucket the caller files it under.
type registryVerdict struct {
	// OK is true when the image is confirmed current.
	OK bool
	// NotVerified is true when the check could not establish freshness.
	// NEVER conflated with OK: reporting "clean" because the lookup failed is
	// the control CLAUDE.md §4 forbids.
	NotVerified bool
	// Detail is the per-image line.
	Detail string
}

// decideRegistryImage implements E2a/E2b/E2c for one image.
//
// E2a — the revision label is a POSITIVE-only signal. A label EQUAL to the
// daemon revision proves the image was built from the daemon's own source, so
// it is OK immediately with no network call: correct offline, and correct for
// the air-gapped host that builds the agent image under the ghcr tag (§3.1).
// A label that DIFFERS proves nothing — it may be a CE sha for this very
// release — so it falls through rather than condemning the image.
//
// KNOWN BOUND, inherited not introduced: both sides append "-dirty" (Makefile
// VORNIK_REVISION and resolveDaemonRevision), which makes the asymmetric cases
// safe but cannot separate two DIFFERENT dirty trees at the same commit. The
// record layer closes that by refusing to record from a dirty tree; this path
// has no record by definition.
func (h *DoctorHandlers) decideRegistryImage(ctx context.Context, tag, daemonRev string, imageRev string, labelled bool) registryVerdict {
	// revisionsMatch, NOT ==. The daemon's revision and an image label come
	// from different sources with different lengths, and a prior bug rendered
	// "image e2c94d1a47bf, daemon e2c94d1a47bf" — two identical strings
	// reported as different — because an exact comparison missed the
	// prefix case. Pinned by TestImageFreshnessShortDaemonRevisionMatchesFullImageLabel.
	if labelled && revisionsMatch(imageRev, daemonRev) {
		return registryVerdict{OK: true}
	}

	readPublished := h.publishedDigestsFunc
	if readPublished == nil {
		readPublished = realPublishedDigests
	}
	local, err := h.localDigest(ctx, tag)
	if err != nil {
		return registryVerdict{
			NotVerified: true,
			Detail:      fmt.Sprintf("%s: NOT VERIFIED (%s)", tag, reasonUnreachable),
		}
	}

	published, err := readPublished(ctx, tag)
	if err != nil {
		reason := reasonUnreachable
		switch {
		case ctx.Err() != nil:
			reason = reasonBudget
		case errorMentionsMissingSkopeo(err):
			reason = reasonNoSkopeo
		}
		return registryVerdict{
			NotVerified: true,
			Detail:      fmt.Sprintf("%s: NOT VERIFIED (%s)", tag, reason),
		}
	}

	// E2b: compare against the digest for THIS host's architecture.
	// platformDigestsFromIndex already drops unknown-arch attestation
	// manifests and non-linux platforms; the check inherits that filtering and
	// must not restate it. Recording the manifest-LIST digest instead of the
	// platform one is a bug this project shipped once, and it would have
	// failed on every host, forever.
	for _, digest := range published {
		if digest == local {
			return registryVerdict{OK: true}
		}
	}
	if want, ok := published[runtime.GOARCH]; ok && want == local {
		return registryVerdict{OK: true}
	}

	// E2c: the local digest is not what the registry serves, and the label did
	// not identify the image either. Two explanations, indistinguishable by
	// inspection: a stale pull, or a deliberate local build under the same tag.
	// The check reports both and prints NO bare pull — pulling is correct for
	// the first and destroys the second.
	return registryVerdict{
		Detail: fmt.Sprintf(
			"%s: INDETERMINATE — local %s is not the digest the registry serves. "+
				"Either this image is a stale pull (then: podman pull %s), "+
				"or it was built locally under this tag (then: rebuild it, and the local build is current). "+
				"Which one is not recoverable by inspection, so this check will not guess.",
			tag, shortDigest(local), tag),
	}
}

// localDigest reads the image's own manifest digest through the injected seam.
func (h *DoctorHandlers) localDigest(ctx context.Context, tag string) (string, error) {
	read := h.imageDigestFunc
	if read == nil {
		read = realImageDigest
	}
	return read(ctx, tag)
}

func shortDigest(d string) string {
	if len(d) > 19 {
		return d[:19]
	}
	return d
}

// errSkopeoAbsent marks the "the tool is not installed" case, which is a
// different operator action from "the registry refused us".
var errSkopeoAbsent = errors.New("skopeo not found on PATH")

func errorMentionsMissingSkopeo(err error) bool { return errors.Is(err, errSkopeoAbsent) }

// realPublishedDigests reads what the registry currently serves, reusing the
// recorder's reader rather than growing a second one (tenet §5).
func realPublishedDigests(ctx context.Context, tag string) (map[string]string, error) {
	skopeoPath, err := exec.LookPath("skopeo")
	if err != nil {
		return nil, errSkopeoAbsent
	}
	reader := imagemanifest.SkopeoIndexReader{
		Run: func(args ...string) ([]byte, error) {
			// The context carries the SHARED budget, so an exhausted deadline
			// surfaces here as ctx.Err() and is reported as `budget exhausted`
			// rather than as an unreachable registry.
			return exec.CommandContext(ctx, skopeoPath, args...).Output()
		},
	}
	return reader.PlatformDigests(tag)
}

// registryTagged splits the deployable set by the manifest's own predicate.
// Never a local copy of the rule: a safety check with two implementations has
// one that is wrong, and the wrong one is usually the newer.
func registryTagged(tag string) bool { return imagemanifest.IsRegistryTag(tag) }
