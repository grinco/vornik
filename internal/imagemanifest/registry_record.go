package imagemanifest

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// IndexReader resolves a tag to the manifest digest of each platform in its
// index.
//
// An interface so the record can be built without a registry client in tests,
// and so a future provider (a mirror, a different registry) is a new
// implementation rather than a change here.
type IndexReader interface {
	// PlatformDigests returns architecture → manifest digest for one tag.
	PlatformDigests(tag string) (map[string]string, error)
}

// BuildRegistryRecord builds a release record from PUBLISHED manifests rather
// than from images on the build machine.
//
// This mode exists because the local one cannot serve a release. `-record`
// refuses to write anything when the images are absent — correctly, since a
// record must describe images that exist — and a CI runner building packages has
// no images at all. That is why no package has ever carried a record.
//
// It also describes the right thing: a release should declare what it PUBLISHED,
// not what happened to be lying on the machine that built it.
//
// The two image kinds are treated differently on purpose (design §Stage 1b):
//
//   - REGISTRY-PINNED images carry a digest per architecture, read from the
//     index. A host compares its own architecture's entry, exactly and offline.
//   - HOST-BUILT images carry source_commit only. Each machine builds its own,
//     so any digest recorded here is guaranteed not to match, and a check that
//     always fails is as useless as one that always passes.
func BuildRegistryRecord(idx IndexReader, images []Image, sourceCommit string) (*ReleaseRecord, error) {
	if !commitPattern.MatchString(sourceCommit) {
		return nil, fmt.Errorf("source commit %q is not 40 lowercase hex characters — "+
			"a -dirty suffix names a tree that exists on exactly one machine and can never "+
			"be verified by anyone", sourceCommit)
	}

	rec := &ReleaseRecord{
		Version:      RecordVersion,
		RecordSource: RecordSourceRegistry,
		Images:       make([]ImageRecord, 0, len(images)),
	}

	for _, img := range images {
		// A test-only image is not part of the release, and recording it makes
		// the record claim the release ships something it does not. Found by
		// running the recorder for real, where a test fixture turned up in the
		// output. That fixture has since been deleted; this guard is not about
		// that one image but about the CLASS, so it stays — it is what stops
		// the next ConditionTest row reaching a release record.
		if img.Condition == ConditionTest {
			continue
		}
		entry := ImageRecord{Tag: img.Tag, SourceCommit: sourceCommit}
		if !entry.IsRegistryPinned() {
			// Host-built: no registry call, no digest. A registry outage
			// therefore cannot stop these being recorded.
			rec.Images = append(rec.Images, entry)
			continue
		}

		digests, err := idx.PlatformDigests(img.Tag)
		if err != nil {
			// FATAL, not skipped. Omitting an image the release actually ships
			// would make the record claim a smaller release than happened, and
			// the host would then report that image as "not declared" — an
			// error message describing the record's gap rather than the host's
			// real state.
			return nil, fmt.Errorf("%s: cannot read published manifests: %w", img.Tag, err)
		}

		platforms := make(map[string]string, len(digests))
		for arch, d := range digests {
			// buildx `provenance: true` adds an attestation manifest to the
			// index under architecture "unknown". It is not a platform, and
			// recording it would put a digest in the record that no host can
			// ever match. Dropped here rather than rejected: its presence is
			// normal and expected, it simply is not a platform.
			if arch == "" || arch == "unknown" {
				continue
			}
			platforms[arch] = d
		}
		if len(platforms) == 0 {
			return nil, fmt.Errorf("%s: the published index lists no platforms "+
				"(only attestation manifests?) — nothing here can be verified", img.Tag)
		}
		if missing := missingReleaseArches(platforms); len(missing) > 0 {
			return nil, fmt.Errorf("%s: the published index covers %v but the release "+
				"packages %v — missing %v. A record covering half the packages is worse "+
				"than none: on a missing architecture the freshness check falls through to "+
				"the commit comparison, which for a registry image can never match (the "+
				"image carries a CE revision label, the record an EE commit), so every host "+
				"on that architecture warns forever and cannot tell it from a real "+
				"provenance failure. Republish the image for all release architectures",
				img.Tag, sortedKeys(platforms), ReleaseArchitectures, missing)
		}
		entry.Digests = platforms
		rec.Images = append(rec.Images, entry)
	}

	rec.Count = len(rec.Images)
	if err := rec.Validate(); err != nil {
		// Validate here so a malformed record is caught where it is built,
		// rather than at load time on a customer's host.
		return nil, fmt.Errorf("built an invalid record: %w", err)
	}
	return rec, nil
}

// SkopeoIndexReader reads published manifests with `skopeo inspect --raw`.
//
// skopeo rather than podman: reading an index must NOT pull it. `podman manifest
// inspect` populates local storage, which on a release runner means downloading
// every platform of every image to learn digests we already have names for.
type SkopeoIndexReader struct {
	// Run executes skopeo and returns stdout. Injectable for tests.
	Run func(args ...string) ([]byte, error)
}

// PlatformDigests implements IndexReader.
func (s SkopeoIndexReader) PlatformDigests(tag string) (map[string]string, error) {
	if s.Run == nil {
		return nil, fmt.Errorf("skopeo runner not configured")
	}
	ref := "docker://" + strings.TrimPrefix(tag, "docker://")
	out, err := s.Run("inspect", "--raw", ref)
	if err != nil {
		return nil, err
	}
	return platformDigestsFromIndex(out)
}

// ReleaseArchitectures are the architectures this release packages as
// first-class, and therefore the ones a registry-pinned image's index MUST
// cover. It mirrors the `goarch:` matrix in .goreleaser.enterprise.yaml;
// TestReleaseArchitecturesMatchGoreleaser fails if the two drift.
//
// The FULL_ARCH long tail (386, arm, ppc64le, s390x, riscv64, appended by
// scripts/package-enterprise.sh) is deliberately NOT here. It is an opt-in
// local build, not what the release publishes, and requiring it would fail
// every ordinary release. An index that happens to carry an extra architecture
// still has it RECORDED — recorded, never required.
var ReleaseArchitectures = []string{"amd64", "arm64"}

// missingReleaseArches reports which release architectures the index omits.
func missingReleaseArches(platforms map[string]string) []string {
	var missing []string
	for _, arch := range ReleaseArchitectures {
		if _, ok := platforms[arch]; !ok {
			missing = append(missing, arch)
		}
	}
	return missing
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// platformDigestsFromIndex parses an OCI image index (or a Docker manifest
// list) into architecture → manifest digest.
//
// A SINGLE-PLATFORM manifest is rejected rather than guessed at. If a tag
// resolves to one image instead of an index, the release published one
// architecture — which was true of the agent image until 2026-09-02 and left
// arm64 hosts unable to pull the image their package named. Recording it as
// though it covered every platform would hide exactly that.
func platformDigestsFromIndex(raw []byte) (map[string]string, error) {
	var idx struct {
		MediaType string `json:"mediaType"`
		Manifests []struct {
			Digest   string `json:"digest"`
			Platform struct {
				Architecture string `json:"architecture"`
				OS           string `json:"os"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(raw, &idx); err != nil {
		return nil, fmt.Errorf("manifest is not valid JSON: %w", err)
	}
	if len(idx.Manifests) == 0 {
		return nil, fmt.Errorf("tag resolves to a single-platform image, not a multi-arch index — " +
			"a release that publishes one architecture leaves the others unable to pull it")
	}
	out := make(map[string]string, len(idx.Manifests))
	for _, m := range idx.Manifests {
		arch := m.Platform.Architecture
		if arch == "" || arch == "unknown" {
			continue // attestation manifest, not a platform
		}
		// linux only: these images run in containers on this daemon.
		if m.Platform.OS != "" && m.Platform.OS != "linux" {
			continue
		}
		out[arch] = m.Digest
	}
	return out, nil
}
