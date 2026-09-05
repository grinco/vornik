package imagemanifest

import (
	"errors"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The registry record mode. It exists because cmd/vornik-images -record refuses
// to write a record when the images do not exist — correctly — and a CI runner
// has none. Reading the published manifests instead is how a release describes
// what it PUBLISHED rather than what happened to be on the build machine.

type fakeIndex struct {
	byTag map[string]map[string]string
	err   error
}

func (f fakeIndex) PlatformDigests(tag string) (map[string]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	d, ok := f.byTag[tag]
	if !ok {
		return nil, errors.New("no such tag: " + tag)
	}
	return d, nil
}

func TestBuildRegistryRecord_SplitsByImageKind(t *testing.T) {
	idx := fakeIndex{byTag: map[string]map[string]string{
		"ghcr.io/grinco/vornik-agent:latest": {"amd64": "sha256:" + hex64, "arm64": "sha256:" + hex64b},
	}}
	rec, err := BuildRegistryRecord(idx, []Image{
		{Tag: "ghcr.io/grinco/vornik-agent:latest"},
		{Tag: "localhost/vornik-broker:latest"},
	}, goodCommit)
	if err != nil {
		t.Fatalf("BuildRegistryRecord: %v", err)
	}
	if err := rec.Validate(); err != nil {
		t.Fatalf("the record it produced does not validate: %v", err)
	}
	if rec.RecordSource != RecordSourceRegistry {
		t.Errorf("RecordSource = %q, want %q", rec.RecordSource, RecordSourceRegistry)
	}
	if len(rec.Images) != 2 {
		t.Fatalf("images = %d, want 2", len(rec.Images))
	}
	// Registry image: both architectures, so ONE record describes both packages.
	if d, ok := rec.Images[0].DigestForArch("arm64"); !ok || d != "sha256:"+hex64b {
		t.Errorf("arm64 digest = %q,%v", d, ok)
	}
	// Host-built: no digest at all.
	if len(rec.Images[1].Digests) != 0 {
		t.Errorf("host-built image carried digests: %v", rec.Images[1].Digests)
	}
}

// A registry image the release did not publish must FAIL the record, not be
// silently omitted: a record that quietly drops an image claims a release
// shipped less than it did, and the host would then report "not declared".
func TestBuildRegistryRecord_UnpublishedRegistryImageIsFatal(t *testing.T) {
	idx := fakeIndex{byTag: map[string]map[string]string{}}
	_, err := BuildRegistryRecord(idx, []Image{{Tag: "ghcr.io/grinco/vornik-agent:latest"}}, goodCommit)
	if err == nil {
		t.Fatal("a registry image missing from the registry produced a record anyway")
	}
}

// Host-built images never touch the registry, so a total registry outage still
// yields a usable record for them — but must not silently drop a registry image.
func TestBuildRegistryRecord_HostBuiltOnlyNeedsNoRegistry(t *testing.T) {
	rec, err := BuildRegistryRecord(fakeIndex{err: errors.New("network down")},
		[]Image{{Tag: "localhost/vornik-broker:latest"}}, goodCommit)
	if err != nil {
		t.Fatalf("host-built-only record needed the registry: %v", err)
	}
	if err := rec.Validate(); err != nil {
		t.Fatalf("record does not validate: %v", err)
	}
}

// A dirty tree is a release error, and the record must refuse it here exactly as
// the local mode does.
func TestBuildRegistryRecord_RejectsADirtyCommit(t *testing.T) {
	idx := fakeIndex{byTag: map[string]map[string]string{
		"ghcr.io/grinco/vornik-agent:latest": {"amd64": "sha256:" + hex64},
	}}
	if _, err := BuildRegistryRecord(idx, []Image{{Tag: "ghcr.io/grinco/vornik-agent:latest"}},
		goodCommit+"-dirty"); err == nil {
		t.Fatal("a -dirty commit was accepted; it names a tree that exists on one machine")
	}
}

// The attestation manifest buildx adds must not become a platform entry.
func TestBuildRegistryRecord_DropsTheAttestationManifest(t *testing.T) {
	// Both release architectures PLUS the attestation entry — the shape a
	// healthy buildx multi-arch publish actually produces. The fixture used to
	// be amd64+attestation only, which is the BROKEN shape the arch-coverage
	// guard now rejects; this test's subject is the attestation, not coverage,
	// so it gets a realistic index rather than an exemption.
	idx := fakeIndex{byTag: map[string]map[string]string{
		"ghcr.io/grinco/vornik-agent:latest": {
			"amd64": "sha256:" + hex64, "arm64": "sha256:" + hex64b,
			"unknown": "sha256:" + hex64b,
		},
	}}
	rec, err := BuildRegistryRecord(idx, []Image{{Tag: "ghcr.io/grinco/vornik-agent:latest"}}, goodCommit)
	if err != nil {
		t.Fatalf("BuildRegistryRecord: %v", err)
	}
	if _, ok := rec.Images[0].Digests["unknown"]; ok {
		t.Error(`the "unknown" attestation manifest was recorded as a platform`)
	}
	if err := rec.Validate(); err != nil {
		t.Fatalf("record does not validate: %v", err)
	}
}

// A test-only image is not part of a release and must not appear in its record.
// Found by running the recorder for real: test-image:latest (ConditionTest) was
// included, and the record then claimed a release ships an image it does not.
func TestBuildRegistryRecord_ExcludesTestOnlyImages(t *testing.T) {
	rec, err := BuildRegistryRecord(fakeIndex{}, []Image{
		{Tag: "localhost/vornik-broker:latest"},
		{Tag: "test-image:latest", Condition: ConditionTest},
	}, goodCommit)
	if err != nil {
		t.Fatalf("BuildRegistryRecord: %v", err)
	}
	for _, img := range rec.Images {
		if img.Tag == "test-image:latest" {
			t.Fatal("a test-only image was recorded as part of the release")
		}
	}
	if rec.Count != len(rec.Images) {
		t.Errorf("count %d does not match %d images after filtering", rec.Count, len(rec.Images))
	}
}

// A record that covers only SOME of the architectures the release packages is
// as broken as no record, and quieter about it.
//
// Live case, 2026-09-02: the published index for the agent image listed amd64
// plus an attestation entry and nothing else, because the CE workflow had no
// `platforms:` key. len(idx.Manifests) == 2, so the single-platform guard
// passed, the attestation was correctly skipped, and the recorder emitted a
// one-architecture record with no warning of any kind. Every arm64 packaged
// host then falls through to the commit comparison, which the freshness check's
// own comment says can never match for a registry image (CE revision label vs
// EE record) — a guaranteed false warning, forever, indistinguishable from a
// real provenance failure.
func TestBuildRegistryRecord_RejectsAnIndexMissingAReleaseArch(t *testing.T) {
	idx := fakeIndex{byTag: map[string]map[string]string{
		// amd64 only — exactly the shape the live registry had.
		"ghcr.io/grinco/vornik-agent:latest": {"amd64": "sha256:" + hex64},
	}}
	_, err := BuildRegistryRecord(idx, []Image{
		{Tag: "ghcr.io/grinco/vornik-agent:latest"},
	}, goodCommit)
	if err == nil {
		t.Fatal("a record covering only amd64 was accepted; the release packages arm64 too, " +
			"so this record describes half the packages while claiming to describe the release")
	}
	for _, want := range []string{"arm64", "ghcr.io/grinco/vornik-agent:latest"} {
		if !contains(err.Error(), want) {
			t.Errorf("error %q does not name %q — an operator cannot act on it", err, want)
		}
	}
}

// The complement, so the guard cannot be satisfied by rejecting everything.
func TestBuildRegistryRecord_AcceptsFullArchCoverage(t *testing.T) {
	idx := fakeIndex{byTag: map[string]map[string]string{
		"ghcr.io/grinco/vornik-agent:latest": {"amd64": "sha256:" + hex64, "arm64": "sha256:" + hex64b},
	}}
	if _, err := BuildRegistryRecord(idx, []Image{
		{Tag: "ghcr.io/grinco/vornik-agent:latest"},
	}, goodCommit); err != nil {
		t.Fatalf("full coverage was rejected: %v", err)
	}
}

// An arch beyond the first-class set is RECORDED, never REQUIRED. FULL_ARCH=1
// (scripts/package-enterprise.sh) appends a long tail to local builds that the
// published agent image does not carry, so requiring it would fail every
// ordinary release.
func TestBuildRegistryRecord_ExtraArchesAreRecordedNotRequired(t *testing.T) {
	idx := fakeIndex{byTag: map[string]map[string]string{
		"ghcr.io/grinco/vornik-agent:latest": {
			"amd64": "sha256:" + hex64, "arm64": "sha256:" + hex64b, "s390x": "sha256:" + hex64,
		},
	}}
	rec, err := BuildRegistryRecord(idx, []Image{
		{Tag: "ghcr.io/grinco/vornik-agent:latest"},
	}, goodCommit)
	if err != nil {
		t.Fatalf("an index with an EXTRA arch was rejected: %v", err)
	}
	if _, ok := rec.Images[0].DigestForArch("s390x"); !ok {
		t.Error("the extra architecture was dropped; a host on it can then verify nothing")
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && stringsIndex(s, sub) >= 0)
}

func stringsIndex(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// RATCHET. ReleaseArchitectures is a second statement of a fact
// .goreleaser.enterprise.yaml already owns, and a second statement of a fact
// goes stale by construction: adding an architecture to the build matrix
// without touching this constant would leave the coverage guard passing on a
// record that omits the new arch — the exact silence the guard exists to break,
// reintroduced one level up.
//
// Parsed rather than hardcoded, so the test fails when the matrix moves instead
// of when someone remembers to update it.
func TestReleaseArchitecturesMatchGoreleaser(t *testing.T) {
	raw, err := os.ReadFile("../../.goreleaser.enterprise.yaml")
	if os.IsNotExist(err) {
		// The Community export prunes the enterprise goreleaser config; the
		// ratchet is Enterprise-only and is proven there. Found 2026-09-05
		// when this test broke the export's "manifest paths exist" gate.
		t.Skip("enterprise goreleaser config not in this tree (Community export)")
	}
	if err != nil {
		t.Fatalf("read goreleaser config: %v", err)
	}
	re := regexp.MustCompile(`(?m)^\s*goarch:\s*\[([^\]]*)\]`)
	matches := re.FindAllStringSubmatch(string(raw), -1)
	if len(matches) == 0 {
		t.Fatal("no `goarch: [...]` line found in .goreleaser.enterprise.yaml — " +
			"the build matrix moved and this ratchet can no longer read it")
	}

	want := map[string]bool{}
	for _, m := range matches {
		for _, a := range strings.Split(m[1], ",") {
			if a = strings.TrimSpace(a); a != "" {
				want[a] = true
			}
		}
	}
	got := map[string]bool{}
	for _, a := range ReleaseArchitectures {
		got[a] = true
	}
	if !reflect.DeepEqual(want, got) {
		t.Errorf("ReleaseArchitectures = %v but .goreleaser.enterprise.yaml builds %v.\n"+
			"They must agree: this constant is what the image-record coverage guard "+
			"requires, so an architecture the release packages but this list omits ships "+
			"with a record that cannot verify it.",
			sortedSet(got), sortedSet(want))
	}
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
