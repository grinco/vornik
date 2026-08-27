package api

import (
	"context"
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/imagemanifest"
)

// bareHostProbe answers "no optional stacks here", so only the `always` rows
// of the manifest are walked. Keeps these tests independent of whether the
// machine running them happens to have a trading stack.
type bareHostProbe struct{}

func (bareHostProbe) UnitEnabled(string) bool        { return false }
func (bareHostProbe) StackHasContainers(string) bool { return false }

// freshnessHandlers builds a DoctorHandlers with every host-touching seam
// injected, so the check never shells out to podman or reads real host state.
func freshnessHandlers(daemonRev string, revs map[string]string, absent map[string]bool, inspectErr error) *DoctorHandlers {
	return &DoctorHandlers{
		imageProber: bareHostProbe{},
		daemonRevisionFunc: func() (string, bool) {
			return daemonRev, daemonRev != ""
		},
		imageRevisionFunc: func(_ context.Context, image string) (string, bool, error) {
			if inspectErr != nil {
				return "", false, inspectErr
			}
			if absent[image] {
				return "", false, errImageAbsent
			}
			rev, labelled := revs[image]
			return rev, labelled, nil
		},
	}
}

func TestImageFreshnessMatchingRevisionIsOK(t *testing.T) {
	rev := "abc123def456"
	h := freshnessHandlers(rev, map[string]string{imagemanifest.AgentImageTag: rev}, nil, nil)

	got := h.checkImageFreshness(context.Background())
	if got.Status != "OK" {
		t.Fatalf("image built from the daemon's own revision must be OK, got %q: %s",
			got.Status, got.Message)
	}
}

// TestImageFreshnessDivergentRevisionWarns is the regression test for the
// reported bug: a CE customer ran the documented update path for six weeks
// while localhost/vornik-agent:latest stayed frozen at install date, and
// nothing reported it. checkAgentImages returned OK the whole time because it
// only asks whether the image EXISTS.
func TestImageFreshnessDivergentRevisionWarns(t *testing.T) {
	daemon := "1111111111111111111111111111111111111111"
	stale := "9999999999999999999999999999999999999999"
	h := freshnessHandlers(daemon, map[string]string{imagemanifest.AgentImageTag: stale}, nil, nil)

	got := h.checkImageFreshness(context.Background())
	if got.Status != "WARNING" {
		t.Fatalf("a stale image must WARN, got %q: %s", got.Status, got.Message)
	}
	// The operator has to be able to act on it, which means seeing both
	// sides of the comparison and the command that fixes it.
	for _, want := range []string{stale[:12], daemon[:12], imagemanifest.AgentImageTag} {
		if !strings.Contains(got.Message, want) && !containsItem(got.Items, want) {
			t.Errorf("message must name %q so the drift is actionable; got: %s", want, got.Message)
		}
	}
}

// TestImageFreshnessUnlabelledImageWarns covers every image built before
// provenance labelling shipped. It is not an error — the image may be
// perfectly current — but it is unverifiable, and the design's position is
// that unverifiable must not read as fine.
func TestImageFreshnessUnlabelledImageWarns(t *testing.T) {
	h := freshnessHandlers("abc123", map[string]string{}, nil, nil)

	got := h.checkImageFreshness(context.Background())
	if got.Status != "WARNING" {
		t.Fatalf("an unlabelled image must WARN, got %q: %s", got.Status, got.Message)
	}
	if !strings.Contains(strings.ToLower(got.Message), "rebuild") {
		t.Errorf("message should tell the operator the fix is a rebuild; got: %s", got.Message)
	}
}

// TestImageFreshnessAbsentImageIsSkippedNotSwallowed pins review finding 6
// (companion review-20260825-868b.md): a missing image is checkAgentImages'
// failure to report. This check must skip it BEFORE inspecting, so the
// operator sees one clear message about the missing image rather than two
// checks complaining about the same thing in different words.
func TestImageFreshnessAbsentImageIsSkippedNotSwallowed(t *testing.T) {
	h := freshnessHandlers("abc123", nil, map[string]bool{imagemanifest.AgentImageTag: true}, nil)

	got := h.checkImageFreshness(context.Background())
	if got.Status != "OK" {
		t.Fatalf("an absent image is checkAgentImages' to report, so freshness must "+
			"stay OK; got %q: %s", got.Status, got.Message)
	}
}

func TestImageFreshnessPodmanUnavailableWarns(t *testing.T) {
	h := freshnessHandlers("abc123", nil, nil, errors.New("exec: \"podman\": executable file not found in $PATH"))

	got := h.checkImageFreshness(context.Background())
	if got.Status != "WARNING" {
		t.Fatalf("cannot verify -> WARNING (consistent with checkAgentImages), got %q", got.Status)
	}
}

// TestImageFreshnessUnstampedDaemonIsOK: a daemon built without VCS stamping
// has no revision to compare against. That is the build's shortcoming, not
// the image's, and warning about it would train operators to ignore the check.
func TestImageFreshnessUnstampedDaemonIsOK(t *testing.T) {
	h := freshnessHandlers("", map[string]string{imagemanifest.AgentImageTag: "abc123"}, nil, nil)

	got := h.checkImageFreshness(context.Background())
	if got.Status != "OK" {
		t.Fatalf("an unstamped daemon cannot be compared; got %q: %s", got.Status, got.Message)
	}
}

// TestImageFreshnessWalksOnlyManifestImages is the §6.1 guarantee that a
// deliberate retag pin is invisible rather than merely tolerated. The
// benchmark host runs localhost/vornik-agent:bench precisely so a bench
// rebuild cannot reach production's agents; that tag is not in the manifest,
// so the check must never look at it and must never warn about it.
const manifestWalkSHA = "e2c94d1a47bf24e85300db77f2a6a834d3354a60"

func TestImageFreshnessWalksOnlyManifestImages(t *testing.T) {
	var inspected []string
	h := &DoctorHandlers{
		imageProber: bareHostProbe{},
		// Realistic shapes: the daemon reports git's 12-char short form,
		// the image label carries the full SHA. A placeholder shorter than a
		// short-sha is not evidence of a commit and revisionsMatch refuses it.
		daemonRevisionFunc: func() (string, bool) { return manifestWalkSHA[:12], true },
		imageRevisionFunc: func(_ context.Context, image string) (string, bool, error) {
			inspected = append(inspected, image)
			return manifestWalkSHA, true, nil
		},
	}

	if got := h.checkImageFreshness(context.Background()); got.Status != "OK" {
		t.Fatalf("expected OK, got %q", got.Status)
	}
	for _, image := range inspected {
		if strings.HasSuffix(image, ":bench") {
			t.Errorf("checked %q — a retag pin outside the manifest must never be walked", image)
		}
	}
	if len(inspected) == 0 {
		t.Fatal("nothing was inspected; the check is not walking the manifest at all")
	}
}

func containsItem(items []string, want string) bool {
	for _, it := range items {
		if strings.Contains(it, want) {
			return true
		}
	}
	return false
}

// TestResolveDaemonRevisionUsesRealBuildInfo exercises the un-injected path —
// what runs in production. The test binary is itself a Go build, so
// version.BuildRevision answers from real VCS build info here.
func TestResolveDaemonRevisionUsesRealBuildInfo(t *testing.T) {
	h := &DoctorHandlers{} // no daemonRevisionFunc: take the real path

	rev, ok := h.resolveDaemonRevision()
	if ok && rev == "" {
		t.Error("reported a revision but returned an empty string")
	}
	if !ok && rev != "" {
		t.Errorf("reported no revision but returned %q", rev)
	}
	// A dirty tree must be distinguishable from a clean one at the same
	// commit, or an image built from uncommitted changes compares equal to
	// one built from HEAD.
	if ok && strings.HasSuffix(rev, "-dirty") && len(rev) <= len("-dirty") {
		t.Errorf("dirty suffix with no revision: %q", rev)
	}
}

// TestShortRevKeepsShortRevisionsIntact: shortRev must not pad or panic on a
// revision shorter than its cut length.
func TestShortRevKeepsShortRevisionsIntact(t *testing.T) {
	if got := shortRev("abc"); got != "abc" {
		t.Errorf("shortRev(abc) = %q, want abc", got)
	}
	if got := shortRev("0123456789abcdef"); got != "0123456789ab" {
		t.Errorf("shortRev truncated to %q, want 0123456789ab", got)
	}
}

// REGRESSION, found on the 2026-08-27 deploy: this check could never pass.
//
// version.BuildRevision truncates to 12 characters (git's short-sha length),
// while the image label carries the full 40-character SHA, so `rev != daemonRev`
// was true for every image on every run. The check fired on a host where the
// daemon and every image had just been built from the same commit.
//
// The existing tests missed it because they pass the SAME 12-char string as
// both the daemon and the image revision — a shape that never occurs in
// production, where the two sides come from different sources with different
// lengths. A fixture that cannot reproduce the real inputs cannot catch a real
// bug.
//
// The operator-visible symptom was the tell: the finding rendered as
// "image e2c94d1a47bf, daemon e2c94d1a47bf" — two identical strings reported
// as different, because the message shortens both for display.
func TestImageFreshnessShortDaemonRevisionMatchesFullImageLabel(t *testing.T) {
	const fullSHA = "e2c94d1a47bf24e85300db77f2a6a834d3354a60"
	shortRevision := fullSHA[:12]

	h := freshnessHandlers(shortRevision, map[string]string{imagemanifest.AgentImageTag: fullSHA}, nil, nil)

	got := h.checkImageFreshness(context.Background())
	if got.Status != "OK" {
		t.Fatalf("a 12-char daemon revision must match the full-SHA image label it is a prefix of; "+
			"got %q: %s items=%v", got.Status, got.Message, got.Items)
	}
}

// The check must still catch what it exists to catch: a genuinely different
// commit, where the daemon's short revision is NOT a prefix of the label.
func TestImageFreshnessShortRevisionStillCatchesRealDrift(t *testing.T) {
	h := freshnessHandlers("e2c94d1a47bf",
		map[string]string{imagemanifest.AgentImageTag: "0000000000000000000000000000000000000000"}, nil, nil)

	got := h.checkImageFreshness(context.Background())
	if got.Status == "OK" {
		t.Fatal("an image from a different commit must still be reported")
	}
}

// A dirty daemon build is not the same as a stale image, and must not be
// reported as one — the operator's fix differs (commit vs rebuild).
func TestImageFreshnessDirtyDaemonIsNotReportedAsDrift(t *testing.T) {
	const fullSHA = "e2c94d1a47bf24e85300db77f2a6a834d3354a60"
	h := freshnessHandlers(fullSHA[:12]+"-dirty",
		map[string]string{imagemanifest.AgentImageTag: fullSHA}, nil, nil)

	got := h.checkImageFreshness(context.Background())
	if got.Status == "OK" {
		t.Skip("a dirty daemon may legitimately be flagged; this test pins only that " +
			"it is not mislabelled as a commit mismatch")
	}
	for _, item := range got.Items {
		if strings.Contains(item, "different commit") {
			t.Errorf("a dirty build is not a different commit: %q", item)
		}
	}
}
