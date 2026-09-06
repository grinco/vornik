package imagemanifest

import "testing"

// The obtain selector (2026-08-28-packaged-image-provenance-design.md §S2.3).
//
// THE DEFECT THIS EXISTS FOR. Before Stage 2 the update path skipped an image
// whose org.opencontainers.image.revision label equalled `git rev-parse HEAD`.
// A PULLED image carries the CE commit it was built from; an EE host's HEAD is
// an EE commit. They never match — so the moment an EE host pulled, every later
// update rebuilt unconditionally, and the pull had made things strictly worse
// than the local build it replaced.
//
// These tests assert WHICH RULE WAS SELECTED, not merely the verdict. A test
// that only checked "old rule fails, new rule passes" would go green against an
// implementation where both rules happen to agree, which is the failure mode
// this project has shipped before (the 2026-08-14 gold manifest).

const (
	registryTag = "ghcr.io/grinco/vornik-agent:latest"
	localTag    = "localhost/vornik-broker:latest"

	eeHead   = "1111111111111111111111111111111111111111"
	ceCommit = "2222222222222222222222222222222222222222"

	digestA = "sha256:aaaa000000000000000000000000000000000000000000000000000000000000"
	digestB = "sha256:bbbb000000000000000000000000000000000000000000000000000000000000"
)

// TestSelectorIsTagDriven proves the choice of rule follows the TAG, not the
// image's provenance or whichever comparison happens to succeed.
func TestSelectorIsTagDriven(t *testing.T) {
	t.Run("registry tag ignores a matching revision label", func(t *testing.T) {
		// Revision matches HEAD exactly — the local rule would say "skip".
		// The digest does not match, so the digest rule says "pull". A
		// tag-driven selector pulls.
		local := LocalImage{Present: true, Digests: []string{digestA}, Revision: eeHead}
		target := Target{RegistryReached: true, Digest: digestB, Commit: eeHead}

		got, _ := Decide(registryTag, local, target)
		if got != ActionPull {
			t.Errorf("registry-pinned tag with matching revision but differing digest: got %v, want ActionPull.\n"+
				"The selector consulted the revision label for a registry tag — that is the "+
				"CE-commit/EE-commit inversion (design §S2.3)", got)
		}
	})

	t.Run("local tag ignores a matching digest", func(t *testing.T) {
		// Digest matches the target exactly — the digest rule would say
		// "skip". The revision does not match HEAD, so the local rule says
		// "build".
		local := LocalImage{Present: true, Digests: []string{digestA}, Revision: ceCommit}
		target := Target{RegistryReached: true, Digest: digestA, Commit: eeHead}

		got, _ := Decide(localTag, local, target)
		if got != ActionBuild {
			t.Errorf("localhost tag with matching digest but differing revision: got %v, want ActionBuild.\n"+
				"The selector consulted a digest for a host-built image, whose digest depends "+
				"on build-time incidentals and is not a freshness signal", got)
		}
	})

	t.Run("a locally built image under a registry tag still takes the digest branch", func(t *testing.T) {
		// The air-gapped host builds and tags with the SAME registry name
		// (design §3.1). The branch must follow the tag, not how the image got
		// there — otherwise the rule depends on unrecoverable provenance.
		local := LocalImage{Present: true, Digests: []string{digestA}, Revision: eeHead}
		target := Target{RegistryReached: true, Digest: digestA, Commit: eeHead}

		if got, _ := Decide(registryTag, local, target); got != ActionSkip {
			t.Errorf("got %v, want ActionSkip on a digest match", got)
		}
	})
}

// TestPulledImageOnEnterpriseHostDoesNotRebuildForever is the named regression:
// the exact shape that made Stage 2 dangerous.
func TestPulledImageOnEnterpriseHostDoesNotRebuildForever(t *testing.T) {
	// A pulled image: its label is the CE commit, and the host is EE.
	local := LocalImage{Present: true, Digests: []string{digestA}, Revision: ceCommit}
	target := Target{RegistryReached: true, Digest: digestA, Commit: eeHead}

	got, _ := Decide(registryTag, local, target)
	if got != ActionSkip {
		t.Fatalf("a pulled, current image on an EE host: got %v, want ActionSkip.\n"+
			"Comparing the image's CE revision label against the EE HEAD can never match, "+
			"so this rebuilds on every update forever — the defect §S2.3 exists to prevent", got)
	}

	// And the old rule really would have failed here, or this proves nothing.
	if local.Revision == target.Commit {
		t.Fatal("fixture is wrong: the CE label must differ from the EE HEAD for this test to mean anything")
	}
}

// TestUnreachableRegistryLeavesTheImageAlone is round-1 finding 2. Rebuilding
// here would churn forever AND silently replace a verifiable pulled artifact
// with an unverifiable local one.
func TestUnreachableRegistryLeavesTheImageAlone(t *testing.T) {
	local := LocalImage{Present: true, Digests: []string{digestA}, Revision: ceCommit}
	target := Target{RegistryReached: false, Commit: eeHead}

	got, reason := Decide(registryTag, local, target)
	if got != ActionLeave {
		t.Errorf("registry-pinned tag with an unreachable registry: got %v, want ActionLeave", got)
	}
	if reason == "" {
		t.Error("ActionLeave must carry a reason — a host that changes nothing and says nothing " +
			"is indistinguishable from one that checked and was happy")
	}
}

// TestUnreachableRegistryWithNoImageMustBuild — leaving nothing alone is not an
// option when there is nothing there. This is the air-gapped FIRST install.
func TestUnreachableRegistryWithNoImageMustBuild(t *testing.T) {
	local := LocalImage{Present: false}
	target := Target{RegistryReached: false, Commit: eeHead}

	if got, _ := Decide(registryTag, local, target); got != ActionBuild {
		t.Errorf("air-gapped first install: got %v, want ActionBuild — there is no image to leave alone", got)
	}
}

// TestPublishedNothingForThisHostBuilds covers both seams where the registry
// answered but has nothing for us: an architecture the release never published,
// and a commit that was never published at all.
func TestPublishedNothingForThisHostBuilds(t *testing.T) {
	local := LocalImage{Present: true, Digests: []string{digestA}, Revision: ceCommit}
	target := Target{RegistryReached: true, Digest: "", Commit: eeHead}

	got, reason := Decide(registryTag, local, target)
	if got != ActionBuild {
		t.Errorf("registry reachable but nothing published for this host: got %v, want ActionBuild", got)
	}
	if reason == "" {
		t.Error("a fallback build must say why, or it reads as a Stage 2 bug (round-1 finding 3)")
	}
}

// TestLocalTagRules keeps the pre-Stage-2 behaviour intact for host-built
// images — contract C7, and the broker/scraper/cluster images (§S2.7).
func TestLocalTagRules(t *testing.T) {
	t.Run("current", func(t *testing.T) {
		local := LocalImage{Present: true, Revision: eeHead}
		if got, _ := Decide(localTag, local, Target{Commit: eeHead}); got != ActionSkip {
			t.Errorf("got %v, want ActionSkip", got)
		}
	})
	t.Run("drifted", func(t *testing.T) {
		local := LocalImage{Present: true, Revision: ceCommit}
		if got, _ := Decide(localTag, local, Target{Commit: eeHead}); got != ActionBuild {
			t.Errorf("got %v, want ActionBuild", got)
		}
	})
	t.Run("absent", func(t *testing.T) {
		if got, _ := Decide(localTag, LocalImage{}, Target{Commit: eeHead}); got != ActionBuild {
			t.Errorf("got %v, want ActionBuild", got)
		}
	})
	t.Run("unlabelled predates provenance and is always rebuilt", func(t *testing.T) {
		local := LocalImage{Present: true, Revision: ""}
		if got, _ := Decide(localTag, local, Target{Commit: eeHead}); got != ActionBuild {
			t.Errorf("got %v, want ActionBuild", got)
		}
	})
}

// TestOneRegistryPredicate — the selector and the recorder must not carry two
// ideas of "is this from a registry". Two implementations of one safety check
// means one of them is wrong (tenet §5).
func TestOneRegistryPredicate(t *testing.T) {
	cases := map[string]bool{
		"ghcr.io/grinco/vornik-agent:latest": true,
		"docker.io/library/golang:1.25":      true,
		"localhost/vornik-broker:latest":     false,
		"localhost:5000/vornik-agent:latest": false,
		"vornik-agent:latest":                false,
		"vornik-agent":                       false,
	}
	for tag, want := range cases {
		if got := IsRegistryTag(tag); got != want {
			t.Errorf("IsRegistryTag(%q) = %v, want %v", tag, got, want)
		}
		// The record method must agree, because it must BE this function.
		if got := (ImageRecord{Tag: tag}).IsRegistryPinned(); got != want {
			t.Errorf("ImageRecord{%q}.IsRegistryPinned() = %v, want %v — the recorder and the "+
				"selector have diverged", tag, got, want)
		}
	}
}

// TestDigestMatchIsExact — a substring or prefix comparison would let a
// truncated digest pass.
func TestDigestMatchIsExact(t *testing.T) {
	local := LocalImage{Present: true, Digests: []string{digestA[:20]}, Revision: ceCommit}
	target := Target{RegistryReached: true, Digest: digestA, Commit: eeHead}

	if got, _ := Decide(registryTag, local, target); got != ActionPull {
		t.Errorf("truncated local digest: got %v, want ActionPull — the comparison must be exact", got)
	}
}

// TestAnyRepoDigestMatches — podman reports RepoDigests as a list, and a tag
// re-pointed at a new digest can leave more than one entry.
func TestAnyRepoDigestMatches(t *testing.T) {
	local := LocalImage{Present: true, Digests: []string{digestB, digestA}, Revision: ceCommit}
	target := Target{RegistryReached: true, Digest: digestA, Commit: eeHead}

	if got, _ := Decide(registryTag, local, target); got != ActionSkip {
		t.Errorf("target digest present among several RepoDigests: got %v, want ActionSkip", got)
	}
}
