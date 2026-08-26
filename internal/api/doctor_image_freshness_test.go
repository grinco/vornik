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
func TestImageFreshnessWalksOnlyManifestImages(t *testing.T) {
	var inspected []string
	h := &DoctorHandlers{
		imageProber:        bareHostProbe{},
		daemonRevisionFunc: func() (string, bool) { return "abc123", true },
		imageRevisionFunc: func(_ context.Context, image string) (string, bool, error) {
			inspected = append(inspected, image)
			return "abc123", true, nil
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
