package imagemanifest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// The obtain-provenance record: what THIS HOST actually got, and how.
//
// Design: 2026-08-28-packaged-image-provenance-design.md §S2.4, contract C5.
//
// WHY A RECORD AND NOT AN INSPECTION. §5.2 established, by running it, that
// podman writes a RepoDigests entry even for a locally built, never-pushed
// image — so RepoDigests cannot distinguish pulled from built. Neither can the
// revision label once a pull has happened: a pulled image carries the CE commit
// it was built from, which is indistinguishable from a host that happened to
// build at that commit. Provenance that cannot be recovered by inspection has
// to be written down at the moment it is known.
//
// DELIBERATELY NOT MERGED INTO images.json. That file is the release's
// DECLARATION ("what this release published"); this is the host's OBSERVATION
// ("what this machine obtained, and how"). The entire value of the check is
// comparing the two, which a single file makes impossible.

// ObtainedPathEnv overrides where a host records what it obtained.
//
// Overridable because the CE quickstart and the EE package do not share a data
// directory, and a record written where nothing reads it is worse than none.
const ObtainedPathEnv = "VORNIK_OBTAINED_RECORD"

// DefaultObtainedPath returns the record path for this host.
func DefaultObtainedPath() string {
	if p := os.Getenv(ObtainedPathEnv); p != "" {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".local", "share", "vornik", "obtained-images.json")
	}
	return "/var/lib/vornik/obtained-images.json"
}

// ObtainMethod is how a host came to hold an image.
type ObtainMethod string

const (
	// MethodPulled — fetched from a registry by digest.
	MethodPulled ObtainMethod = "pulled"
	// MethodBuilt — built locally from a checkout.
	MethodBuilt ObtainMethod = "built"
)

// ObtainedImage is one host-side observation.
type ObtainedImage struct {
	Tag    string       `json:"tag"`
	Method ObtainMethod `json:"method"`
	// Reference is what was actually fetched or built: the digest reference
	// for a pull, the source commit for a build.
	Reference string `json:"reference"`
	// ResolvedFrom is the tag the digest was resolved THROUGH, for a pull.
	// Recorded because a GHCR tag is mutable: keeping what we resolved next to
	// what we got is what makes a moved tag detectable afterwards rather than
	// silently absorbed (§S2.2).
	ResolvedFrom string `json:"resolved_from,omitempty"`
	At           string `json:"at"`
}

// ObtainedRecord is the host's whole observation set.
type ObtainedRecord struct {
	Version int             `json:"version"`
	Images  []ObtainedImage `json:"images"`
}

// ObtainedVersion is the schema version for this file.
const ObtainedVersion = 1

// LoadObtained reads the host record. A missing file is an empty record, not an
// error: a host that has never obtained anything through this path is a normal
// state (a source install, a dev box, any host predating Stage 2).
func LoadObtained(path string) (*ObtainedRecord, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &ObtainedRecord{Version: ObtainedVersion}, nil
	}
	if err != nil {
		return nil, err
	}
	var rec ObtainedRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		// Corrupt is NOT absent. Reporting it as absent would leave a host
		// believing provenance is recorded when it is not — the same conflation
		// ErrRecordAbsent exists to prevent for the release record.
		return nil, corrupt("%s: %v", path, err)
	}
	if rec.Version != ObtainedVersion {
		return nil, corrupt("%s: unknown version %d (this build understands %d)",
			path, rec.Version, ObtainedVersion)
	}
	return &rec, nil
}

// Note records one obtain, replacing any previous entry for the same tag. The
// latest observation is the true one; a history would grow without bound and
// answer a question nobody asks.
func (r *ObtainedRecord) Note(img ObtainedImage) {
	if img.At == "" {
		img.At = time.Now().UTC().Format(time.RFC3339)
	}
	for i := range r.Images {
		if r.Images[i].Tag == img.Tag {
			r.Images[i] = img
			return
		}
	}
	r.Images = append(r.Images, img)
}

// MethodFor reports how this host obtained a tag, and whether it knows.
func (r *ObtainedRecord) MethodFor(tag string) (ObtainedImage, bool) {
	for _, img := range r.Images {
		if img.Tag == tag {
			return img, true
		}
	}
	return ObtainedImage{}, false
}

// Save writes the record, creating its directory.
func (r *ObtainedRecord) Save(path string) error {
	r.Version = ObtainedVersion
	// Stable order so the file does not churn between runs that changed
	// nothing — a diff that is always non-empty is a diff nobody reads.
	sort.Slice(r.Images, func(i, j int) bool { return r.Images[i].Tag < r.Images[j].Tag })
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("cannot create %s: %w", dir, err)
		}
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
