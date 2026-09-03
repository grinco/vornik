package imagemanifest

// The release image record: what a release DECLARES its images to be.
//
// Design: https://docs.vornik.io
//
// The manifest (manifest.go) says which images exist and how to build them.
// This says which BUILD of them belongs to a given release. The package ships
// one of these, so a host can answer "is the image I am running the one this
// release declares" — the question the packaged path could not previously ask
// at all, because the package builds and names no image.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// RecordPath is where the package installs the release record.
const RecordPath = "/usr/share/vornik-enterprise/images.json"

// RecordVersion is the only schema version this build understands. An unknown
// version is CORRUPT rather than ignored: a record written by a newer release
// may mean something different by the same field names, and guessing is how a
// check silently starts comparing the wrong things.
// Version 2 (2026-09-02) replaced the single `digest` field with a
// per-architecture `digests` map and added `record_source`. Version 1 described
// one digest per image, which could not describe a multi-arch release and was
// never carried by any shipped package — so nothing in the wild needs reading,
// and a v1 record is rejected with a clear version error rather than mis-read.
const RecordVersion = 2

// ErrRecordAbsent reports that no record exists at the given path.
//
// ABSENT AND CORRUPT ARE DIFFERENT STATES and must never be conflated. Absent
// is legitimate — a source install, a dev box — and degrades silently to the
// label comparison. Corrupt means the provenance check is NOT RUNNING on a host
// that believes it is. Reading one as the other produces a guard that looks
// protective and is not.
var ErrRecordAbsent = errors.New("image record absent")

// CorruptRecordError is returned for a record that exists but cannot be
// trusted. It is deliberately a distinct type so callers cannot handle it by
// accident through the absent path.
type CorruptRecordError struct{ Reason string }

func (e *CorruptRecordError) Error() string {
	return fmt.Sprintf("image record is corrupt: %s", e.Reason)
}

// IsCorrupt reports whether err is a corrupt-record error.
func IsCorrupt(err error) bool {
	var c *CorruptRecordError
	return errors.As(err, &c)
}

func corrupt(format string, args ...any) error {
	return &CorruptRecordError{Reason: fmt.Sprintf(format, args...)}
}

// digestPattern matches "<algorithm>:<hex>".
//
// The algorithm is required, never a naked hex string: a record that drops it
// cannot be compared unambiguously once another algorithm exists. LOWERCASE is
// assumed, which is what podman emits today; an uppercase "SHA256:" fails
// validation and is treated as corrupt — the safe direction, and recorded here
// so a maintainer meeting that error knows it is the assumption firing.
var digestPattern = regexp.MustCompile(`^[a-z0-9]+:[a-f0-9]{32,}$`)

// commitPattern matches a full 40-character lowercase git sha.
//
// A "-dirty" suffix therefore fails, deliberately: it names a tree that exists
// on exactly one machine and can never be reproduced or verified by anyone, so
// it is a release error rather than a value worth recording.
var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ImageRecord is one image as a release declares it.
type ImageRecord struct {
	Tag string `json:"tag"`

	// Digests maps ARCHITECTURE to that platform's manifest digest, for
	// registry-pinned images only. Empty for host-built ones.
	//
	// A MAP, not one value, and per-PLATFORM rather than the manifest-list
	// digest. Both choices are load-bearing and were corrected on 2026-09-02
	// after checking against a live registry:
	//
	//   - One value cannot describe amd64 and arm64, and the EE package matrix
	//     builds both from a single run. That is what kept this record out of
	//     the package at all.
	//   - The manifest-LIST digest is arch-independent but is NOT what a host
	//     observes: a host holding the image reports the PLATFORM digest, so
	//     comparing against the index digest fails on every host, forever.
	//     Recording per-platform digests makes the comparison exact AND
	//     offline — no registry round-trip at check time.
	Digests map[string]string `json:"digests,omitempty"`

	SourceCommit string `json:"source_commit"`
}

// IsRegistryPinned reports whether this image is pulled from a registry, and so
// has a digest a host can meaningfully compare.
//
// A host-built image (localhost/...) is built on each machine from the same
// source, and its digest depends on build-time incidentals. Any digest recorded
// for one is guaranteed not to match — a check that always fails, which is as
// useless as one that always passes and considerably noisier.
func (r ImageRecord) IsRegistryPinned() bool {
	// The OCI rule: a tag is registry-qualified only when its FIRST path
	// segment is a host — it contains a dot or a colon. A bare, host-less name
	// like "something:latest" is only ever built locally; treating it as remote
	// sent the recorder to docker.io looking for an image that has never
	// existed there (found by running the recorder for real, on a test-fixture
	// tag that has since been deleted).
	head, rest, ok := strings.Cut(r.Tag, "/")
	if !ok || rest == "" {
		return false
	}
	if !strings.ContainsAny(head, ".:") {
		return false
	}
	// localhost is a host, but it means "this machine" — including with a port,
	// as a local registry would be written.
	host, _, _ := strings.Cut(head, ":")
	return host != "localhost"
}

// DigestForArch returns the digest this release published for one architecture.
//
// Absent is reported honestly rather than falling back to another
// architecture's digest, which would compare a host against an image it is not
// running.
func (r ImageRecord) DigestForArch(arch string) (string, bool) {
	d, ok := r.Digests[arch]
	return d, ok && d != ""
}

// Validate checks one image row.
func (r ImageRecord) Validate() error {
	if r.Tag == "" {
		return errors.New("image has no tag")
	}
	if !commitPattern.MatchString(r.SourceCommit) {
		return fmt.Errorf("%s: malformed source_commit %q — expected 40 lowercase hex "+
			"characters (a -dirty suffix is a release error, not a recordable value)",
			r.Tag, r.SourceCommit)
	}
	if !r.IsRegistryPinned() {
		if len(r.Digests) > 0 {
			return fmt.Errorf("%s: host-built images carry no digest, but %d were recorded — "+
				"a host builds its own, so none of these could ever match",
				r.Tag, len(r.Digests))
		}
		return nil
	}
	if len(r.Digests) == 0 {
		return fmt.Errorf("%s: a registry-pinned image with no digests verifies nothing", r.Tag)
	}
	for arch, d := range r.Digests {
		// buildx `provenance: true` adds an attestation manifest to the index
		// under architecture "unknown". It is not a platform, and recording it
		// puts a digest in the record that no host can ever match.
		if arch == "" || arch == "unknown" {
			return fmt.Errorf("%s: digest recorded for architecture %q — that is an "+
				"attestation manifest, not a platform", r.Tag, arch)
		}
		if !digestPattern.MatchString(d) {
			return fmt.Errorf("%s: malformed digest %q for %s — expected "+
				"<algorithm>:<lowercase-hex>", r.Tag, d, arch)
		}
	}
	return nil
}

// ReleaseRecord is the shipped declaration for one release.
type ReleaseRecord struct {
	Version int `json:"version"`
	// Count is redundant with len(Images) ON PURPOSE. It is the only thing
	// that catches a truncation which happens to leave valid JSON while
	// dropping entries — the case a bare json.Unmarshal reads as success.
	Count int `json:"count"`

	// RecordSource names the mode that produced this record: the registry
	// (authoritative for packaged releases) or local images (a dev-box
	// convenience). Without it the two modes could silently produce
	// conflicting records — a stale local image set and a correct registry —
	// and nothing would show which a package carried.
	RecordSource string `json:"record_source"`

	Images []ImageRecord `json:"images"`
}

// The two record-producing modes.
const (
	// RecordSourceRegistry reads published manifests. The only mode used for
	// packaged releases, because a release describes what it PUBLISHED.
	RecordSourceRegistry = "registry"
	// RecordSourceLocal reads images present on the machine. Dev boxes only.
	RecordSourceLocal = "local"
)

// Validate checks the whole record.
func (rec ReleaseRecord) Validate() error {
	if rec.Version != RecordVersion {
		return fmt.Errorf("unknown schema version %d (this build understands %d)",
			rec.Version, RecordVersion)
	}
	switch rec.RecordSource {
	case RecordSourceRegistry, RecordSourceLocal:
	default:
		return fmt.Errorf("unknown record_source %q — expected %q or %q; the producing "+
			"mode must be unambiguous or two modes can disagree unnoticed",
			rec.RecordSource, RecordSourceRegistry, RecordSourceLocal)
	}
	for i, img := range rec.Images {
		if err := img.Validate(); err != nil {
			return fmt.Errorf("image[%d]: %w", i, err)
		}
	}
	for i, img := range rec.Images {
		if img.SourceCommit != rec.Images[0].SourceCommit {
			return fmt.Errorf("image[%d] (%s) was built from %s but image[0] (%s) from %s — "+
				"one record must describe one build",
				i, img.Tag, img.SourceCommit, rec.Images[0].Tag, rec.Images[0].SourceCommit)
		}
	}
	if rec.Count != len(rec.Images) {
		return fmt.Errorf("count says %d but %d image(s) are present — the record was truncated",
			rec.Count, len(rec.Images))
	}
	return nil
}

// Lookup returns the record for a tag.
func (r *ReleaseRecord) Lookup(tag string) (ImageRecord, bool) {
	if r == nil {
		return ImageRecord{}, false
	}
	for _, img := range r.Images {
		if img.Tag == tag {
			return img, true
		}
	}
	return ImageRecord{}, false
}

// SourceCommit returns the commit every image in the record was built from.
//
// One record is produced by ONE `make build-images` invocation over ONE tree,
// so every image in it shares a commit. That is what makes a record↔daemon
// comparison meaningful: the record speaks for a release, not for a pile of
// images assembled from different ones.
//
// Reports false for an empty record, which declares no images and therefore
// names no commit.
func (r *ReleaseRecord) SourceCommit() (string, bool) {
	if r == nil || len(r.Images) == 0 {
		return "", false
	}
	return r.Images[0].SourceCommit, true
}

// LoadReleaseRecord reads and validates the record at path.
//
// The ordering is the contract (design §5.2) and is not an implementation
// detail — each step distinguishes a state the caller reports differently:
//
//  1. file exists?            no -> ErrRecordAbsent (silent, correct)
//  2. parses as JSON?         no -> corrupt
//  3. every field valid?      no -> corrupt
//  4. count == len(images)?   no -> corrupt
//  5. otherwise               -> verify
//
// Step 3 is the one most easily skipped and the reason this is not three lines:
// a truncation can leave syntactically valid JSON holding a half-written digest
// ("sha256:ab12"), which parses cleanly and then compares false against
// everything, silently.
func LoadReleaseRecord(path string) (*ReleaseRecord, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrRecordAbsent
		}
		// Present but unreadable (permissions, IO error) is NOT absent:
		// something is there and we cannot vouch for it.
		return nil, corrupt("cannot read %s: %v", path, err)
	}

	var rec ReleaseRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, corrupt("not valid JSON (%v)", err)
	}

	// One place decides what a valid record is. The loader used to inline these
	// rules, which is how the digest check and the schema drifted apart when
	// the schema gained per-architecture digests.
	if err := rec.Validate(); err != nil {
		return nil, corrupt("%v", err)
	}

	return &rec, nil
}
