package imagemanifest

import (
	"fmt"
	"strings"
)

// The obtain selector: what an update path should do about one image.
//
// Design: 2026-08-28-packaged-image-provenance-design.md, "Amendment
// 2026-09-06 — Stage 2: the obtain half", §S2.3.
//
// This is Go-owned for the same reason the manifest is (§4.2): the daemon's
// doctor check and the shell update path must reach the SAME verdict, and a
// rule duplicated in bash and Go is a rule with two answers.

// ObtainAction is the decision for one image.
type ObtainAction int

const (
	// ActionSkip — the host already has what it should have.
	ActionSkip ObtainAction = iota
	// ActionPull — fetch the target digest from the registry.
	ActionPull
	// ActionBuild — build locally from the checkout.
	ActionBuild
	// ActionLeave — change nothing, and say why.
	//
	// Reachable only for a registry-pinned tag whose registry cannot be
	// reached while an image is already present. The target is unknowable, so
	// neither skipping silently nor rebuilding is honest: rebuilding would
	// churn on every update forever (a pulled image's revision label is the
	// CE commit, which an EE HEAD never equals) and would replace a verifiable
	// pulled artifact with an unverifiable local one.
	ActionLeave
)

func (a ObtainAction) String() string {
	switch a {
	case ActionSkip:
		return "skip"
	case ActionPull:
		return "pull"
	case ActionBuild:
		return "build"
	case ActionLeave:
		return "leave"
	}
	return fmt.Sprintf("ObtainAction(%d)", int(a))
}

// LocalImage is what the host currently holds for a tag.
type LocalImage struct {
	// Present is false when the host has no image under this tag.
	Present bool
	// Digests are podman's RepoDigests for it. A list, because a tag
	// re-pointed at new content can leave more than one entry.
	Digests []string
	// Revision is org.opencontainers.image.revision. Empty means the image
	// predates provenance labelling (2026-08-25) and is always rebuilt.
	Revision string
}

// hasDigest reports an EXACT match against any recorded digest. Exact because a
// prefix comparison would let a truncated digest pass, and a digest is the only
// thing standing between a host and an image it did not ask for.
func (l LocalImage) hasDigest(want string) bool {
	if want == "" {
		return false
	}
	for _, d := range l.Digests {
		if d == want {
			return true
		}
	}
	return false
}

// Target is what this update is trying to reach.
type Target struct {
	// Digest is the resolved registry digest for this host's architecture.
	// Empty with RegistryReached true means the release published nothing this
	// host can use — an architecture it did not build, or a commit never
	// published. That is an answer, and the answer is "build".
	Digest string
	// RegistryReached records whether resolution was attempted AND succeeded.
	// False is "unknowable", not "absent" — the distinction ActionLeave exists
	// for.
	RegistryReached bool
	// Commit is the checkout's HEAD, used for host-built images.
	Commit string
}

// IsRegistryTag reports whether a tag names an image obtained from a registry,
// and so one with a digest a host can meaningfully compare.
//
// THE OCI RULE: a tag is registry-qualified only when its first path segment is
// a host — it contains a dot or a colon. A bare, host-less name like
// "something:latest" is only ever built locally; treating it as remote sent the
// recorder to docker.io looking for an image that has never existed there
// (found by running the recorder for real, on a test-fixture tag since deleted).
//
// ImageRecord.IsRegistryPinned delegates here rather than restating it: the
// recorder and the selector must not carry two ideas of "is this from a
// registry", because a safety check with two implementations has one that is
// wrong and the wrong one is usually the newer (tenet §5).
func IsRegistryTag(tag string) bool {
	head, rest, ok := strings.Cut(tag, "/")
	if !ok || rest == "" {
		return false
	}
	if !strings.ContainsAny(head, ".:") {
		return false
	}
	// localhost is a host, but it means "this machine" — including with a
	// port, as a local registry would be written.
	host, _, _ := strings.Cut(head, ":")
	return host != "localhost"
}

// Decide returns the action for one image, and a reason fit to show an
// operator. The reason is never empty for an action that does something
// surprising (ActionLeave, or a fallback build) — silence there reads as a
// Stage 2 bug rather than the deliberate choice it is.
//
// THE BRANCH FOLLOWS THE TAG, NOT THE IMAGE'S PROVENANCE. An air-gapped host
// builds the agent image and tags it with the same ghcr.io/… name (§3.1), so
// "how did this image get here" is not recoverable by inspection — podman
// writes a RepoDigests entry even for a never-pushed local build (§5.2). A rule
// keyed on provenance would therefore be keyed on a guess.
func Decide(tag string, local LocalImage, target Target) (ObtainAction, string) {
	if !IsRegistryTag(tag) {
		// Host-built: compare the revision label to HEAD, exactly as before
		// Stage 2. A host-built image's digest depends on build-time
		// incidentals, so it is not a freshness signal.
		if local.Present && local.Revision != "" && local.Revision == target.Commit {
			return ActionSkip, ""
		}
		return ActionBuild, ""
	}

	if !target.RegistryReached {
		if local.Present {
			return ActionLeave, "the registry could not be reached, so the target digest is " +
				"unknown; leaving the existing image in place. `vornikctl doctor` reports its " +
				"real state — rebuilding here would replace a verifiable pulled image with an " +
				"unverifiable local one, on every update"
		}
		// Nothing to leave alone. This is the air-gapped first install, and
		// building is the whole point of keeping that path (contract C7).
		return ActionBuild, "the registry could not be reached and this host has no image yet; building locally"
	}

	if target.Digest == "" {
		return ActionBuild, "the release published no image this host can use " +
			"(architecture not built, or this commit was never published); building locally"
	}

	if local.Present && local.hasDigest(target.Digest) {
		return ActionSkip, ""
	}
	return ActionPull, ""
}
