package cli

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSwarmPresetsFS_OnlyMD — every embedded preset file must
// be a `.md` (the YAML format was removed in the 2026.6.0
// rollout). A stray `.yaml` here means the embed directive
// regressed.
func TestSwarmPresetsFS_OnlyMD(t *testing.T) {
	entries, err := swarmPresetsFS.ReadDir("presets")
	require.NoError(t, err)
	require.NotEmpty(t, entries, "presets directory must not be empty")
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		assert.True(t, strings.HasSuffix(name, ".md"),
			"preset %q must be .md (YAML presets removed)", name)
		assert.False(t, strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml"),
			"preset %q must not be YAML", name)
	}
}

// TestReadPreset_ReturnsMarkdown — the preset content surfaces
// in WORKFLOW.md / SWARM.md shape: starts with `---` frontmatter.
// Guards against the embed directive accidentally picking up a
// YAML file.
func TestReadPreset_ReturnsMarkdown(t *testing.T) {
	for _, name := range []string{"basic", "dev", "research"} {
		body, err := readPreset(name)
		require.NoError(t, err, "readPreset(%q)", name)
		trimmed := strings.TrimSpace(body)
		assert.True(t, strings.HasPrefix(trimmed, "---"),
			"preset %q body must start with `---` frontmatter; got prefix %q",
			name, trimmed[:minInt(40, len(trimmed))])
	}
}

// TestReadPreset_UnknownTemplateError — operator-facing error
// when the preset name isn't recognised. Wording check survives
// the YAML → MD switch.
func TestReadPreset_UnknownTemplateError(t *testing.T) {
	_, err := readPreset("nonexistent-preset")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown preset")
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
