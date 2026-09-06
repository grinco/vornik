package imagemanifest

import (
	"errors"
	"testing"
)

// Target resolution (design §S2.2). The load-bearing distinction is between
// "the registry answered and has nothing for us" and "we could not ask" —
// those produce ActionBuild and ActionLeave respectively, and conflating them
// is how a transient outage silently rebuilds over a verifiable image.

func TestResolveFromRecordUsesTheDeclaredDigest(t *testing.T) {
	rec := &ReleaseRecord{Images: []ImageRecord{{
		Tag:     AgentImageTag,
		Digests: map[string]string{"amd64": digestA, "arm64": digestB},
	}}}

	got, err := ResolveTarget(AgentImageTag, rec, "amd64", eeHead, nil)
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	if !got.RegistryReached || got.Digest != digestA {
		t.Errorf("got %+v, want the amd64 digest from the record", got)
	}
	if got.Commit != eeHead {
		t.Errorf("Commit = %q, want HEAD carried through for the local-tag branch", got.Commit)
	}
}

// An architecture the release did not publish must NOT fall back to another
// one's digest — that compares a host against an image it is not running.
func TestResolveFromRecordRefusesAnotherArchsDigest(t *testing.T) {
	rec := &ReleaseRecord{Images: []ImageRecord{{
		Tag:     AgentImageTag,
		Digests: map[string]string{"amd64": digestA},
	}}}

	got, err := ResolveTarget(AgentImageTag, rec, "arm64", eeHead, nil)
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	if !got.RegistryReached {
		t.Error("the record answered; that is reached, not unknowable")
	}
	if got.Digest != "" {
		t.Errorf("Digest = %q, want empty — arm64 was never published, and borrowing amd64's "+
			"digest would compare against an image this host cannot run", got.Digest)
	}
	// Decide turns that into a build with a reason.
	if act, reason := Decide(AgentImageTag, LocalImage{Present: true}, got); act != ActionBuild || reason == "" {
		t.Errorf("got (%v, %q), want ActionBuild with a reason", act, reason)
	}
}

// With no record, the CE path resolves the commit-addressed tag.
func TestResolveWithoutRecordUsesTheCommitTag(t *testing.T) {
	var asked string
	lookup := func(ref string) (string, error) {
		asked = ref
		return digestA, nil
	}

	got, err := ResolveTarget(AgentImageTag, nil, "amd64", ceCommit, lookup)
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	want := "ghcr.io/grinco/vornik-agent:sha-" + ceCommit[:12]
	if asked != want {
		t.Errorf("resolved %q, want %q — the CE path is commit-addressed", asked, want)
	}
	if !got.RegistryReached || got.Digest != digestA {
		t.Errorf("got %+v, want the resolved digest", got)
	}
}

// THE DISTINCTION. A registry that answered "no such tag" is an ANSWER; a
// registry that could not be reached is not.
func TestResolveDistinguishesAbsentFromUnreachable(t *testing.T) {
	t.Run("tag absent is an answer", func(t *testing.T) {
		lookup := func(string) (string, error) { return "", ErrReferenceAbsent }

		got, err := ResolveTarget(AgentImageTag, nil, "amd64", ceCommit, lookup)
		if err != nil {
			t.Fatalf("an absent tag is not an error condition: %v", err)
		}
		if !got.RegistryReached {
			t.Error("RegistryReached = false for an absent tag — the registry DID answer, " +
				"and conflating this with an outage turns a build into a leave")
		}
		if got.Digest != "" {
			t.Errorf("Digest = %q, want empty", got.Digest)
		}
		if act, _ := Decide(AgentImageTag, LocalImage{Present: true}, got); act != ActionBuild {
			t.Errorf("got %v, want ActionBuild — an unpublished commit builds", act)
		}
	})

	t.Run("unreachable is not", func(t *testing.T) {
		lookup := func(string) (string, error) { return "", errors.New("dial tcp: connection refused") }

		got, err := ResolveTarget(AgentImageTag, nil, "amd64", ceCommit, lookup)
		if err != nil {
			t.Fatalf("an unreachable registry is a state, not an error: %v", err)
		}
		if got.RegistryReached {
			t.Error("RegistryReached = true for a connection failure — this is the case that " +
				"must leave a pulled image alone rather than rebuild over it")
		}
		if act, reason := Decide(AgentImageTag, LocalImage{Present: true}, got); act != ActionLeave || reason == "" {
			t.Errorf("got (%v, %q), want ActionLeave with a reason", act, reason)
		}
	})
}

// A host-built image never consults the registry at all — no lookup, no
// network, no dependency on GHCR for the broker/scraper/cluster images (§S2.7).
func TestResolveSkipsTheRegistryForHostBuiltTags(t *testing.T) {
	lookup := func(string) (string, error) {
		t.Fatal("a localhost/ tag must not reach the registry")
		return "", nil
	}

	got, err := ResolveTarget("localhost/vornik-broker:latest", nil, "amd64", eeHead, lookup)
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	if got.Commit != eeHead {
		t.Errorf("Commit = %q, want HEAD", got.Commit)
	}
	if act, _ := Decide("localhost/vornik-broker:latest", LocalImage{Present: true, Revision: eeHead}, got); act != ActionSkip {
		t.Errorf("got %v, want ActionSkip", act)
	}
}

// The record wins over the commit tag when both could answer: it is what the
// release DECLARED, and it needs no network round-trip.
func TestRecordBeatsTheCommitTag(t *testing.T) {
	rec := &ReleaseRecord{Images: []ImageRecord{{
		Tag:     AgentImageTag,
		Digests: map[string]string{"amd64": digestA},
	}}}
	lookup := func(string) (string, error) {
		t.Fatal("the record answered; the registry must not be consulted")
		return "", nil
	}

	got, err := ResolveTarget(AgentImageTag, rec, "amd64", eeHead, lookup)
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	if got.Digest != digestA {
		t.Errorf("Digest = %q, want the record's", got.Digest)
	}
}

// A record that does not mention this image falls through to the registry
// rather than reporting "nothing published" — the record describes the release,
// not every image a host might hold.
func TestRecordWithoutThisImageFallsThrough(t *testing.T) {
	rec := &ReleaseRecord{Images: []ImageRecord{{Tag: "ghcr.io/grinco/something-else:latest"}}}
	called := false
	lookup := func(string) (string, error) { called = true; return digestB, nil }

	got, err := ResolveTarget(AgentImageTag, rec, "amd64", ceCommit, lookup)
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	if !called {
		t.Error("a record that does not name this image must not be read as an answer about it")
	}
	if got.Digest != digestB {
		t.Errorf("Digest = %q, want the resolved one", got.Digest)
	}
}
