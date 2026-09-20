package api

import (
	"context"
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/imagemanifest"
)

// The tag rule on the record-ABSENT path (amendment 2026-09-18, E1/E2).
//
// The defect: legacyFreshness compared every image's revision label against the
// daemon's, including images PUBLISHED from the CE repo, whose label is a CE
// commit while the daemon carries an EE one. Measured from the registry on
// 2026-09-18: revision a24d62e5583c, version main-a24d62e5583c. Unrelated to an
// EE sha by construction, so the check could never pass on a source install and
// its remedy rebuilt seven images to fix nothing.

const (
	eeDaemonRev = "bda9fc53feb0aaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	ceImageRev  = "a24d62e5583c00103eb2f2a5cd8fb8d76690469b"
	hostBuiltCE = "localhost/vornik:thin"
)

// clusterHostProbe makes the localhost/vornik:{thin,full} rows deployable, so
// the host-built branch has something to walk.
type clusterHostProbe struct{}

func (clusterHostProbe) UnitEnabled(string) bool              { return false }
func (clusterHostProbe) StackHasContainers(stack string) bool { return stack == "cluster" }

// freshnessTagRuleHandlers extends freshnessHandlers with the two digest seams
// the registry branch needs.
func freshnessTagRuleHandlers(
	revs map[string]string,
	localDigests map[string]string,
	published map[string]map[string]string,
	publishErr error,
) *DoctorHandlers {
	h := freshnessHandlers(eeDaemonRev, revs, nil, nil)
	h.imageDigestFunc = func(_ context.Context, image string) (string, error) {
		d, ok := localDigests[image]
		if !ok {
			return "", errors.New("no local digest")
		}
		return d, nil
	}
	h.publishedDigestsFunc = func(_ context.Context, tag string) (map[string]string, error) {
		if publishErr != nil {
			return nil, publishErr
		}
		return published[tag], nil
	}
	return h
}

// Test 1 — the defect itself.
func TestRegistryTagIsNotComparedByRevisionLabel(t *testing.T) {
	h := freshnessTagRuleHandlers(
		map[string]string{imagemanifest.AgentImageTag: ceImageRev},
		map[string]string{imagemanifest.AgentImageTag: "sha256:aaaa"},
		map[string]map[string]string{imagemanifest.AgentImageTag: {"amd64": "sha256:aaaa"}},
		nil)

	got := h.checkImageFreshness(context.Background())
	if strings.Contains(got.Message, "built from a different commit") {
		t.Fatalf("a published image was compared by revision label; its label is a CE commit and the "+
			"daemon's is an EE commit, unrelated by construction: %s", got.Message)
	}
	if got.Status != "OK" {
		t.Fatalf("a published image running the digest the registry serves must be OK, got %q: %s",
			got.Status, got.Message)
	}
}

// Test 10 — the label as a POSITIVE-only signal, and no network for it.
func TestRegistryTagWithMatchingLabelIsOKWithoutNetwork(t *testing.T) {
	networked := false
	h := freshnessTagRuleHandlers(
		map[string]string{imagemanifest.AgentImageTag: eeDaemonRev}, nil, nil, nil)
	h.publishedDigestsFunc = func(_ context.Context, _ string) (map[string]string, error) {
		networked = true
		return nil, errors.New("registry must not be consulted")
	}

	got := h.checkImageFreshness(context.Background())
	if got.Status != "OK" {
		t.Fatalf("an image built from the daemon's own source must be OK, got %q: %s", got.Status, got.Message)
	}
	if networked {
		t.Fatal("the label match short-circuits; no network I/O may be attempted. This is what makes " +
			"the air-gapped local build correct offline instead of by coincidence")
	}
}

// Test 11 — the case that would have destroyed this host's own uid fix.
func TestLocallyBuiltRegistryTagOnNetworkedHostIsIndeterminate(t *testing.T) {
	// The reference host on 2026-09-18: a locally built agent image under the
	// ghcr tag, RepoDigests sha256:e935af36…, registry serving sha256:5ca6bfab…
	h := freshnessTagRuleHandlers(
		map[string]string{imagemanifest.AgentImageTag: "665b9b21793a06af34cc4c81430398c5b8778473-dirty"},
		map[string]string{imagemanifest.AgentImageTag: "sha256:e935af36"},
		map[string]map[string]string{imagemanifest.AgentImageTag: {"amd64": "sha256:5ca6bfab"}},
		nil)

	got := h.checkImageFreshness(context.Background())
	if got.Status != "WARNING" {
		t.Fatalf("an unresolvable case must still be reported, got %q", got.Status)
	}
	body := got.Message + " " + strings.Join(got.Items, " ")

	// Both readings named — the check must not pick one, because obtain.go
	// records that they are indistinguishable by inspection.
	for _, want := range []string{"stale", "built"} {
		if !strings.Contains(strings.ToLower(body), want) {
			t.Errorf("both readings must be named; %q is missing from: %s", want, body)
		}
	}
	// And the command fitting EACH, which is what separates this from plain
	// remedy-suppression: the operator chooses with the facts in front of them.
	if !strings.Contains(body, "podman pull") {
		t.Error("the pull command must be offered for the stale-pull reading")
	}
	if !strings.Contains(body, "build") {
		t.Error("the rebuild command must be offered for the local-build reading")
	}
	// What must NOT happen: a bare instruction to pull, which on this host
	// would replace a deliberately locally-built image.
	if strings.Contains(body, "Fix: `podman pull") || strings.Contains(body, "Fix: podman pull") {
		t.Errorf("a bare pull remedy would destroy a deliberate local build: %s", body)
	}
}

// Test 4/5/6 — the registry could not be consulted.
func TestRegistryUnreachableIsNotVerifiedNeverOK(t *testing.T) {
	h := freshnessTagRuleHandlers(
		map[string]string{imagemanifest.AgentImageTag: ceImageRev},
		map[string]string{imagemanifest.AgentImageTag: "sha256:aaaa"},
		nil,
		errors.New("dial tcp: no route to host"))

	got := h.checkImageFreshness(context.Background())
	if got.Status == "OK" {
		t.Fatal("a lookup that failed must never report OK — that is the §4 failure this amendment removes")
	}
	body := got.Message + " " + strings.Join(got.Items, " ")
	if !strings.Contains(strings.ToUpper(body), "NOT VERIFIED") {
		t.Errorf("the couldn't-check outcome must say so: %s", body)
	}
	// Test 12 — the soul of the amendment. The §4 harm is not the warning
	// line, it is the destructive no-op the line tells the operator to run.
	for _, forbidden := range []string{"vornik-update.sh", "--force", "podman pull"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("NOT VERIFIED must carry no remedy; found %q in: %s", forbidden, body)
		}
	}
}

// Test 2 — host-built images keep today's behaviour exactly.
func TestHostBuiltTagStillComparesByRevisionLabel(t *testing.T) {
	h := freshnessTagRuleHandlers(
		map[string]string{hostBuiltCE: "9999999999999999999999999999999999999999"},
		nil, nil, nil)
	h.imageProber = clusterHostProbe{}

	got := h.checkImageFreshness(context.Background())
	if got.Status != "WARNING" {
		t.Fatalf("a host-built image whose label differs from the daemon is still stale, got %q: %s",
			got.Status, got.Message)
	}
	if !strings.Contains(got.Message, "built from a different commit") {
		t.Errorf("the host-built branch keeps its wording: %s", got.Message)
	}
}

// Test 7 — the tag rule comes from imagemanifest, not a second predicate here.
func TestTagRuleUsesTheManifestPredicate(t *testing.T) {
	if !imagemanifest.IsRegistryTag(imagemanifest.AgentImageTag) {
		t.Fatal("precondition: the agent tag is a registry tag")
	}
	if imagemanifest.IsRegistryTag(hostBuiltCE) {
		t.Fatal("precondition: a localhost/ tag is host-built")
	}
	// A safety check with two implementations has one that is wrong, and the
	// wrong one is usually the newer (tenet §5). This pins the BEHAVIOUR that
	// delegation produces: the two tags take different branches, and they do so
	// because of the manifest predicate rather than a local copy of it. A Go
	// test cannot assert the absence of a second predicate directly; the
	// branch-divergence assertions in the tests above are what would fail if
	// doctor grew one that disagreed.
}
