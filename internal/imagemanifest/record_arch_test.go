package imagemanifest

import (
	"runtime"
	"testing"
)

// Stage 1b (design amendment 2026-09-02). The record splits images into
// REGISTRY-PINNED and HOST-BUILT, because they support different claims:
//
//   - a registry image is pulled, so its per-architecture manifest digest is
//     exactly what the host observes locally — an offline, exact comparison
//   - a host-built image is built on each machine, so ANY recorded digest is
//     guaranteed not to match, and recording one produces a check that always
//     fails, which is as useless as one that always passes and noisier

func TestImageRecord_HostBuiltHasNoDigest(t *testing.T) {
	r := ImageRecord{Tag: "localhost/vornik-broker:latest", SourceCommit: goodCommit}
	if r.IsRegistryPinned() {
		t.Fatal("a localhost image classified as registry-pinned")
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("a host-built record with no digest must be valid: %v", err)
	}
}

// The failure this prevents: a digest on a host-built image is a promise nothing
// can keep.
func TestImageRecord_HostBuiltWithADigestIsRejected(t *testing.T) {
	r := ImageRecord{
		Tag: "localhost/vornik-broker:latest", SourceCommit: goodCommit,
		Digests: map[string]string{"amd64": "sha256:" + hex64},
	}
	if err := r.Validate(); err == nil {
		t.Fatal("a host-built image carrying a digest was accepted; no host could ever match it")
	}
}

func TestImageRecord_RegistryPinnedNeedsAtLeastOneDigest(t *testing.T) {
	r := ImageRecord{Tag: "ghcr.io/grinco/vornik-agent:latest", SourceCommit: goodCommit}
	if !r.IsRegistryPinned() {
		t.Fatal("a ghcr.io image was not classified as registry-pinned")
	}
	if err := r.Validate(); err == nil {
		t.Fatal("a registry image with no digests was accepted; there is nothing to verify")
	}
}

// The digest a host compares is its OWN architecture's. Recording a map rather
// than one value is what lets a single record describe amd64 and arm64
// truthfully — the blocker that kept this record out of the package.
func TestImageRecord_DigestForArch(t *testing.T) {
	r := ImageRecord{
		Tag: "ghcr.io/grinco/vornik-agent:latest", SourceCommit: goodCommit,
		Digests: map[string]string{"amd64": "sha256:" + hex64, "arm64": "sha256:" + hex64b},
	}
	if got, ok := r.DigestForArch("arm64"); !ok || got != "sha256:"+hex64b {
		t.Errorf("DigestForArch(arm64) = %q,%v", got, ok)
	}
	// An architecture the release did not publish must report ABSENT rather
	// than falling back to another arch's digest, which would compare a host
	// against an image it is not running.
	if got, ok := r.DigestForArch("riscv64"); ok {
		t.Errorf("DigestForArch(riscv64) = %q, want absent", got)
	}
}

// The attestation manifest buildx adds to an index has architecture "unknown".
// Recording it would put a digest in the record that no host can ever match.
func TestImageRecord_UnknownArchIsRejected(t *testing.T) {
	r := ImageRecord{
		Tag: "ghcr.io/grinco/vornik-agent:latest", SourceCommit: goodCommit,
		Digests: map[string]string{"unknown": "sha256:" + hex64},
	}
	if err := r.Validate(); err == nil {
		t.Fatal(`a digest for architecture "unknown" was accepted — that is buildx's attestation manifest, not a platform`)
	}
}

func TestImageRecord_DigestShapeIsValidated(t *testing.T) {
	for _, bad := range []string{hex64, "sha256:nothex", "sha256:abc", "SHA256:" + hex64} {
		r := ImageRecord{
			Tag: "ghcr.io/grinco/vornik-agent:latest", SourceCommit: goodCommit,
			Digests: map[string]string{"amd64": bad},
		}
		if err := r.Validate(); err == nil {
			t.Errorf("malformed digest %q was accepted", bad)
		}
	}
}

// RecordSource keeps the two producing modes from silently disagreeing.
func TestReleaseRecord_SourceIsValidated(t *testing.T) {
	base := func(src string) ReleaseRecord {
		return ReleaseRecord{
			Version: RecordVersion, Count: 1, RecordSource: src,
			Images: []ImageRecord{{
				Tag: "ghcr.io/grinco/vornik-agent:latest", SourceCommit: goodCommit,
				Digests: map[string]string{"amd64": "sha256:" + hex64},
			}},
		}
	}
	for _, ok := range []string{RecordSourceRegistry, RecordSourceLocal} {
		if err := base(ok).Validate(); err != nil {
			t.Errorf("record_source %q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "registry-ish", "whatever"} {
		if err := base(bad).Validate(); err == nil {
			t.Errorf("record_source %q accepted; the mode must be unambiguous", bad)
		}
	}
}

// The host asks about the architecture it is actually running.
func TestRuntimeArchIsAKnownKey(t *testing.T) {
	if runtime.GOARCH == "" {
		t.Skip("no GOARCH")
	}
}

const (
	goodCommit = "0123456789abcdef0123456789abcdef01234567"
	hex64      = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	hex64b     = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
)

// Registry-pinned means the tag names a REMOTE registry host. Found by running
// the record for real: "test-image:latest" has no host component, so the
// !localhost heuristic classified it as registry-pinned and the recorder tried
// to resolve it against docker.io — where it has never existed.
func TestImageRecord_RegistryPinnedRequiresAHost(t *testing.T) {
	for _, tc := range []struct {
		tag  string
		want bool
	}{
		{"ghcr.io/grinco/vornik-agent:latest", true},
		{"registry.example.com:5000/team/img:1", true},
		{"localhost/vornik-broker:latest", false},
		{"localhost:5000/vornik-broker:latest", false},
		// No host component at all: a bare name that is only ever built and
		// used locally, never pushed.
		{"test-image:latest", false},
		{"vornik-agent", false},
	} {
		t.Run(tc.tag, func(t *testing.T) {
			if got := (ImageRecord{Tag: tc.tag}).IsRegistryPinned(); got != tc.want {
				t.Errorf("IsRegistryPinned(%q) = %v, want %v", tc.tag, got, tc.want)
			}
		})
	}
}
