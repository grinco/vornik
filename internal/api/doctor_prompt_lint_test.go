package api

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"vornik.io/vornik/internal/registry"
)

// TestLintRole_UntrustedAwarenessFromBuiltinPrelude is the regression
// guard for the false positive that fired for every memory-enabled
// role: the linter used to check only role.SystemPrompt, but P5
// injects BuiltinRolePrelude into the effective prompt. A role with
// memory_search and no untrusted_content clause in its own text is
// fine as long as the effective prompt says so.
func TestLintRole_UntrustedAwarenessFromBuiltinPrelude(t *testing.T) {
	sw := &registry.Swarm{
		ID:       "test-swarm",
		LeadRole: "researcher",
		Roles: []registry.SwarmRole{
			{
				Name: "researcher",
				// No mention of untrusted_content, no mention of memory_search.
				// The test-only allowlist below includes both memory_search
				// (to trip the "retrieves external text" branch) and an
				// explicit hint to "use memory_search" (to satisfy the
				// worth-mentioning check).
				SystemPrompt: "You are a researcher. Use memory_search before hitting the web. Output only: {\"research\":{\"written\":true}}",
				Permissions: registry.SwarmRolePermissions{
					AllowedTools: []string{"memory_search", "file_read"},
				},
			},
		},
	}

	// The builtin prelude mentions untrusted_content — via the effective
	// composer the researcher's effective prompt contains it even though
	// the role body doesn't.
	assert.Contains(t, strings.ToLower(registry.BuildEffectiveRolePrompt(sw, sw.Roles[0])), "untrusted_content")

	findings := lintRole(sw, sw.Roles[0], false)
	for _, f := range findings {
		assert.NotContains(t, f, "untrusted_content",
			"lintRole should not flag untrusted_content when the builtin prelude already covers it: %s", f)
	}
}

// TestLintRole_UntrustedAwarenessStillFiresWhenTrulyAbsent covers the
// opposite direction: if a swarm explicitly overrides the effective
// prompt in a way that strips the builtin clause, the warning must
// still fire. We can't easily strip BuiltinRolePrelude directly
// (it's always prepended), so the test exercises a role with no
// memory_search / mcp__* tools — the retrievesExternal branch
// shouldn't fire at all and therefore no untrusted_content finding
// should appear.
func TestLintRole_UntrustedAwarenessSilentOnNonRetrievalRoles(t *testing.T) {
	sw := &registry.Swarm{
		ID: "test-swarm",
		Roles: []registry.SwarmRole{
			{
				Name:         "coder",
				SystemPrompt: "Coder. Output only: {\"implementation\":{\"committed\":true}}",
				Permissions: registry.SwarmRolePermissions{
					AllowedTools: []string{"file_read", "file_write", "run_shell"},
				},
			},
		},
	}
	for _, f := range lintRole(sw, sw.Roles[0], false) {
		assert.NotContains(t, f, "untrusted_content",
			"non-retrieval role should never be flagged for untrusted_content awareness: %s", f)
	}
}

// TestLintRole_UtilityToolsNotRequiredInPrompt guards the scope fix
// that removed file_edit / git_log / git_status from the
// worthMentioning allowlist. Operators were getting a warning per
// utility tool on every coder / analyst / architect role, training
// them to ignore the check entirely. Only memory_search — a
// retrieval tool the model won't discover on its own — earns the
// "prompt never mentions it" finding now.
func TestLintRole_UtilityToolsNotRequiredInPrompt(t *testing.T) {
	sw := &registry.Swarm{ID: "s"}
	role := registry.SwarmRole{
		Name:         "coder",
		SystemPrompt: "Coder. Implement the change and commit.",
		Permissions: registry.SwarmRolePermissions{
			AllowedTools: []string{"file_read", "file_write", "file_edit", "git_status", "git_log"},
		},
	}
	for _, f := range lintRole(sw, role, false) {
		for _, tool := range []string{"file_edit", "git_status", "git_log"} {
			assert.NotContains(t, f, tool,
				"utility tool %q should no longer warrant a prompt-mention finding: %s", tool, f)
		}
	}
}

// TestLintRole_MemorySearchStillRequiredInPrompt is the positive
// pairing for the above: memory_search SHOULD still produce the
// finding when the prompt never mentions it, because without the
// explicit instruction the model won't retrieve project context.
func TestLintRole_MemorySearchStillRequiredInPrompt(t *testing.T) {
	sw := &registry.Swarm{ID: "s"}
	role := registry.SwarmRole{
		Name:         "lead",
		SystemPrompt: "Lead. Coordinate work.",
		Permissions: registry.SwarmRolePermissions{
			AllowedTools: []string{"memory_search", "file_read"},
		},
	}
	var found bool
	for _, f := range lintRole(sw, role, false) {
		if strings.Contains(f, "memory_search") && strings.Contains(f, "never mentions") {
			found = true
			break
		}
	}
	assert.True(t, found, "memory_search should still be flagged when the prompt ignores it")
}

// TestLintRole_EmptyPromptStillFlags is a sanity check that the
// narrower re-scoping of the untrusted check didn't disable the
// other checks.
func TestLintRole_EmptyPromptStillFlags(t *testing.T) {
	sw := &registry.Swarm{ID: "sw"}
	role := registry.SwarmRole{
		Name:         "empty",
		SystemPrompt: "",
		Permissions: registry.SwarmRolePermissions{
			AllowedTools: []string{"file_read"},
		},
	}
	findings := lintRole(sw, role, false)
	require := false
	for _, f := range findings {
		if strings.Contains(f, "systemPrompt is empty") {
			require = true
			break
		}
	}
	assert.True(t, require, "expected empty-prompt finding, got: %v", findings)
}

// TestMentionedOnlyUnderProhibition pins the negation-awareness fix. Rule 3
// flagged assistant-swarm/writer for "referencing" memory_search when its
// prompt actually FORBIDS it ("Do NOT use memory_search results ... those are
// derived/lossy") and correctly withholds the tool — the config was right and
// the doctor inverted it (audit 2026-07-25 follow-up).
func TestMentionedOnlyUnderProhibition(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prompt string
		want   bool
	}{
		{
			// The real prompt: the cue is at the end of the PRECEDING line.
			name: "prohibition split across a line break",
			prompt: "facts come ONLY from that file. Do NOT\nuse memory_search results, scan artifacts,\n" +
				"or prior knowledge as a source of facts.",
			want: true,
		},
		{"never", "You must never call memory_search here.", true},
		{"avoid", "Avoid memory_search — the researcher already aggregated it.", true},
		{"instead of", "Read RESUME.md instead of memory_search.", true},
		{"plain instruction is not a prohibition", "Use memory_search first to avoid rereading material.", false},
		{
			// One negated + one genuine instruction ⇒ the finding must stand.
			name:   "mixed mentions keep the finding",
			prompt: "Use memory_search first. Do not use memory_search for career facts.",
			want:   false,
		},
		{"absent tool is not a prohibition", "No tools mentioned at all.", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mentionedOnlyUnderProhibition(tc.prompt, "memory_search"); got != tc.want {
				t.Errorf("mentionedOnlyUnderProhibition(%q) = %v, want %v", tc.prompt, got, tc.want)
			}
		})
	}
}
