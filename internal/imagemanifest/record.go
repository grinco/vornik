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
)

// RecordPath is where the package installs the release record.
const RecordPath = "/usr/share/vornik-enterprise/images.json"

// RecordVersion is the only schema version this build understands. An unknown
// version is CORRUPT rather than ignored: a record written by a newer release
// may mean something different by the same field names, and guessing is how a
// check silently starts comparing the wrong things.
const RecordVersion = 1

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
	Tag          string `json:"tag"`
	Digest       string `json:"digest"`
	SourceCommit string `json:"source_commit"`
}

// ReleaseRecord is the shipped declaration for one release.
type ReleaseRecord struct {
	Version int `json:"version"`
	// Count is redundant with len(Images) ON PURPOSE. It is the only thing
	// that catches a truncation which happens to leave valid JSON while
	// dropping entries — the case a bare json.Unmarshal reads as success.
	Count  int           `json:"count"`
	Images []ImageRecord `json:"images"`
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

	if rec.Version != RecordVersion {
		return nil, corrupt("unknown schema version %d (this build understands %d)",
			rec.Version, RecordVersion)
	}

	for i, img := range rec.Images {
		switch {
		case img.Tag == "":
			return nil, corrupt("image[%d] has no tag", i)
		case !digestPattern.MatchString(img.Digest):
			return nil, corrupt("image[%d] (%s) has a malformed digest %q — "+
				"expected <algorithm>:<lowercase-hex>", i, img.Tag, img.Digest)
		case !commitPattern.MatchString(img.SourceCommit):
			return nil, corrupt("image[%d] (%s) has a malformed source_commit %q — "+
				"expected 40 lowercase hex characters (a -dirty suffix is a release error, "+
				"not a recordable value)", i, img.Tag, img.SourceCommit)
		}
	}

	// Every image in one record comes from one build over one tree, so a
	// record whose images disagree was assembled from several — which makes
	// "the commit this release declares" undefined, and a record↔daemon
	// comparison meaningless. Corrupt, not merely odd.
	for i, img := range rec.Images {
		if img.SourceCommit != rec.Images[0].SourceCommit {
			return nil, corrupt("image[%d] (%s) was built from %s but image[0] (%s) from %s — "+
				"one record must describe one build",
				i, img.Tag, img.SourceCommit, rec.Images[0].Tag, rec.Images[0].SourceCommit)
		}
	}

	if rec.Count != len(rec.Images) {
		return nil, corrupt("count says %d but %d image(s) are present — "+
			"the record was truncated", rec.Count, len(rec.Images))
	}

	return &rec, nil
}
