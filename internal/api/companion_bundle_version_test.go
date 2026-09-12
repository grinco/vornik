package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Both companion bundles carry the SAME guidance change, so both bump.
// Clients auto-update off the manifest version: a bundle edited without a
// bump reaches nobody, and for a plugin whose whole payload is guidance,
// "it's only documentation" is not an exemption (unreachable-work guard
// design §5; and the standing project rule that any contrib edit bumps).
//
// bundleBaseline pins, per bundle, the state this change started from and
// the delegate skill digest the CURRENT version is published against.
type bundleBaseline struct {
	manifest string // path to the plugin manifest
	// baselineVersion is the version in the tree BEFORE the
	// unreachable-work guard change. Shipping versions must exceed it.
	baselineVersion string
	// skill is the guidance file the version is published against, and
	// skillSHA256 its digest at the last bump. Editing the skill without
	// bumping the manifest fails TestCompanionBundles_SkillEditRequiresBump.
	skill       string
	skillSHA256 string
}

var companionBundles = map[string]bundleBaseline{
	"claude-code": {
		manifest:        "../../contrib/claude-code-companion/.claude-plugin/plugin.json",
		baselineVersion: "0.21.0",
		skill:           "../../contrib/claude-code-companion/skills/delegate/SKILL.md",
		skillSHA256:     "a5c6039a576bb8e0ad21953b3194547b00c29f60878a2aae78e477a33fe4ba12",
	},
	"codex": {
		manifest:        "../../contrib/codex-companion/.codex-plugin/plugin.json",
		baselineVersion: "0.18.0+codex.20260904",
		skill:           "../../contrib/codex-companion/skills/delegate/SKILL.md",
		skillSHA256:     "14c7d25fb55eaffda97b00e9993d4f1a18ebd77597862f0b25ce25aec8e2e0c6",
	},
}

func readManifestVersion(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoErrorf(t, err, "read %s", path)
	var m struct {
		Version string `json:"version"`
	}
	require.NoError(t, json.Unmarshal(raw, &m))
	require.NotEmpty(t, m.Version)
	return m.Version
}

// semverBase strips any build metadata ("0.18.0+codex.20260904" ->
// "0.18.0") and returns the numeric components.
func semverBase(t *testing.T, v string) []int {
	t.Helper()
	base, _, _ := strings.Cut(v, "+")
	base, _, _ = strings.Cut(base, "-")
	parts := strings.Split(base, ".")
	require.Lenf(t, parts, 3, "version %q is not major.minor.patch", v)
	out := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		require.NoErrorf(t, err, "version %q component %q is not numeric", v, p)
		out[i] = n
	}
	return out
}

func versionGreater(t *testing.T, got, floor string) bool {
	t.Helper()
	g, f := semverBase(t, got), semverBase(t, floor)
	for i := range g {
		if g[i] != f[i] {
			return g[i] > f[i]
		}
	}
	return false
}

// TestCompanionBundles_VersionsBumpedPastBaseline — §5. Both bundles are
// edited by the unreachable-work guard change, so both versions must now
// exceed the versions that change started from.
func TestCompanionBundles_VersionsBumpedPastBaseline(t *testing.T) {
	for name, b := range companionBundles {
		t.Run(name, func(t *testing.T) {
			got := readManifestVersion(t, b.manifest)
			assert.Truef(t, versionGreater(t, got, b.baselineVersion),
				"%s version is %q, which is not strictly greater than the pre-change baseline %q — "+
					"clients auto-update off this field, so an unbumped bundle ships its new guidance "+
					"to zero installs", name, got, b.baselineVersion)
		})
	}
}

// TestCompanionBundles_SkillEditRequiresBump is what stops the version
// assertion above from passing trivially on every future tree. The
// delegate skill's digest is pinned against the version it is published
// under: edit the guidance and the digest moves, and the only way to
// make this pass again is to bump the manifest and re-pin here — which
// is precisely the discipline §5 is asking for.
//
// This is a deliberate, decidable check on a NAMED file, not a fuzzy
// scan of the bundle: the design rejected natural-language gates because
// a check that fires on legitimate prose teaches people to ignore it.
func TestCompanionBundles_SkillEditRequiresBump(t *testing.T) {
	for name, b := range companionBundles {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(b.skill)
			require.NoErrorf(t, err, "read %s", b.skill)
			sum := sha256.Sum256(raw)
			got := hex.EncodeToString(sum[:])
			assert.Equalf(t, b.skillSHA256, got,
				"%s has changed since the last manifest bump.\n"+
					"  file:    %s\n"+
					"  version: %s\n"+
					"Bump the bundle version (clients auto-update off it — an unbumped edit "+
					"reaches nobody), add the matching changelog entry to the manifest "+
					"description, then re-pin this digest to %s.",
				filepath.Base(b.skill), b.skill, readManifestVersion(t, b.manifest), got)
		})
	}
}

// TestCompanionBundles_DelegateSkillCarriesNetworkRule — §4.2. The
// guidance must actually be in BOTH bundles: the daemon's refusal names
// a path, and the skill is where the host learns it before it ever hits
// the refusal. Asserted positively on the substance, not on a heading.
func TestCompanionBundles_DelegateSkillCarriesNetworkRule(t *testing.T) {
	for name, b := range companionBundles {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(b.skill)
			require.NoError(t, err)
			body := string(raw)
			for _, want := range []string{
				"network_access",                    // the catalogue field to read
				"companion-rag-ingest",              // the blessed path's terminal workflow
				"acknowledge_workflow_cannot_fetch", // the escape, named
			} {
				assert.Containsf(t, body, want,
					"%s delegate skill must mention %q — the host learns the constraint here "+
						"or not at all", name, want)
			}
			assert.Contains(t, strings.ToLower(body), "cannot reach the internet",
				"the skill must state the constraint plainly, not imply it")
		})
	}
}
