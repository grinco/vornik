package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPatchSwarmAddDispatcherRole_AppendsRole — happy path: an
// `.md` swarm file with no dispatcher role gets one appended into
// the frontmatter, with the operator's chat model populated and
// the noop runtime stub. The result still parses cleanly as a
// Swarm via ParseSwarmMarkdown, and the original body section
// survives.
func TestPatchSwarmAddDispatcherRole_AppendsRole(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "swarm.md")
	require.NoError(t, os.WriteFile(path, []byte(`---
swarmId: "assistant"
roles:
  - name: "lead"
    runtime:
      image: "agent:latest"
---

# Assistant swarm

## Role prompts

### lead

You are the lead.
`), 0o644))

	require.NoError(t, PatchSwarmAddDispatcherRole(path, "claude-haiku-4-5"))

	out, err := os.ReadFile(path)
	require.NoError(t, err)
	body := string(out)
	assert.Contains(t, body, "name: dispatcher", "dispatcher role must be appended")
	assert.Contains(t, body, "claude-haiku-4-5", "operator's chat model must populate the role's model field")
	assert.Contains(t, body, "noop:dispatcher", "runtime.image must be the noop stub — dispatcher isn't a container")
	assert.Contains(t, body, "name: \"lead\"", "existing roles must survive the patch")
	// Body section must be preserved verbatim.
	assert.Contains(t, body, "## Role prompts", "body section must survive")
	assert.Contains(t, body, "You are the lead.", "role prompt body must survive")
	// Frontmatter markers must remain.
	assert.True(t, strings.HasPrefix(strings.TrimSpace(body), "---"),
		"file must still start with the frontmatter open marker")

	// Round-trip: file still parses as a Swarm with both roles.
	swarm, err := ParseSwarmMarkdown(out, "swarm.md")
	require.NoError(t, err)
	require.Len(t, swarm.Roles, 2)
	roleNames := []string{swarm.Roles[0].Name, swarm.Roles[1].Name}
	assert.Contains(t, roleNames, "dispatcher")
	assert.Contains(t, roleNames, "lead")
}

// TestPatchSwarmAddDispatcherRole_IdempotentWhenPresent — running
// the patch on a swarm that already has the role is a no-op. The
// re-run safety the doctor --fix flow depends on.
func TestPatchSwarmAddDispatcherRole_IdempotentWhenPresent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "swarm.md")
	original := `---
swarmId: "assistant"
roles:
  - name: "dispatcher"
    model: "claude-haiku-4-5"
    runtime:
      image: "noop:dispatcher"
  - name: "lead"
    runtime:
      image: "agent:latest"
---

# Assistant swarm
`
	require.NoError(t, os.WriteFile(path, []byte(original), 0o644))

	require.NoError(t, PatchSwarmAddDispatcherRole(path, "claude-haiku-4-5"))

	out, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, original, string(out), "idempotent run must leave the file byte-for-byte unchanged")
}

// TestPatchSwarmAddDispatcherRole_PreservesComments — the YAML
// inside the frontmatter often carries operator notes via inline
// comments. The patcher walks yaml.Node so comments survive.
func TestPatchSwarmAddDispatcherRole_PreservesComments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "swarm.md")
	src := `---
# Operator note: this swarm is the canonical assistant
swarmId: "assistant"
roles:
  # The lead drives the conversation.
  - name: "lead"
    runtime:
      image: "agent:latest"
---

# Assistant swarm
`
	require.NoError(t, os.WriteFile(path, []byte(src), 0o644))

	require.NoError(t, PatchSwarmAddDispatcherRole(path, "claude-haiku-4-5"))

	out, _ := os.ReadFile(path)
	body := string(out)
	assert.Contains(t, body, "# Operator note", "head comments must survive")
	assert.Contains(t, body, "# The lead drives", "inline role comments must survive")
}

// TestPatchSwarmAddDispatcherRole_EmptyModel — operators without
// a configured chat.model get the role appended without a model
// field (rather than with an empty string), so the role inherits
// the daemon's chat config.
func TestPatchSwarmAddDispatcherRole_EmptyModel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "swarm.md")
	require.NoError(t, os.WriteFile(path, []byte(`---
swarmId: "s"
roles:
  - name: "x"
    runtime:
      image: "img"
---
`), 0o644))
	require.NoError(t, PatchSwarmAddDispatcherRole(path, ""))
	out, _ := os.ReadFile(path)
	body := string(out)
	assert.Contains(t, body, "name: dispatcher")
	assert.NotContains(t, body, "model: \"\"",
		"empty model must not produce an empty-string field — leaves the role to inherit from chat config")
}

// TestPatchSwarmAddDispatcherRole_RejectsNonMD — passing a path
// that doesn't end in .md is rejected loudly. Guards against an
// operator (or a stale code path) calling the patcher with a
// legacy `.yaml` path during the migration window.
func TestPatchSwarmAddDispatcherRole_RejectsNonMD(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "swarm.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`swarmId: "x"
roles:
  - name: "lead"
    runtime:
      image: "img"
`), 0o644))
	err := PatchSwarmAddDispatcherRole(path, "claude-haiku-4-5")
	require.Error(t, err, "patcher must reject non-.md paths in the MD-only world")
	assert.Contains(t, err.Error(), ".md")
}
