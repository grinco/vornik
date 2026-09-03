package imagemanifest

import (
	"errors"
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
	idx := fakeIndex{byTag: map[string]map[string]string{
		"ghcr.io/grinco/vornik-agent:latest": {
			"amd64": "sha256:" + hex64, "unknown": "sha256:" + hex64b,
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
