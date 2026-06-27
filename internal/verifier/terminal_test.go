package verifier

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
)

// TestNoStatus429_DefaultsToTerminal — the canonical use case: a
// scraper hits a 429, the verifier flags it, and the violation
// carries Terminal=true so the executor's adaptive routing loop
// won't burn two more iterations bumping the rate limit further.
func TestNoStatus429_DefaultsToTerminal(t *testing.T) {
	cfg := Config{Type: "no_status_429_in_audit"}
	in := Input{
		AuditEntries: []*persistence.ToolAuditEntry{
			{
				ToolName:   "mcp__scraper__web_fetch",
				ToolOutput: `{"status_code":429,"body":"Too Many Requests"}`,
			},
		},
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	require.NotNil(t, v)
	assert.True(t, v.Terminal, "429 violation must default to Terminal=true so adaptive routing doesn't retry")
}

// TestConfigTerminal_OverrideViaYAML — operators can promote a
// non-terminal verifier (e.g. artifact_min_entries) to terminal via
// `terminal: true` in YAML. Used when a contract is hard-fail and
// looping would be pointless (e.g. operator opting an autonomy
// invariant out of the retry loop).
func TestConfigTerminal_OverrideViaYAML(t *testing.T) {
	cfg, ok := ConfigFromMap(map[string]any{
		"type":     "artifact_min_entries",
		"terminal": true,
		"params": map[string]any{
			"artifact_pattern": "scan-*.md",
			"min":              5,
		},
	})
	require.True(t, ok)
	assert.True(t, cfg.Terminal)

	// Verifier impl normally produces Terminal=false; the override
	// must propagate to the Violation regardless.
	in := Input{} // empty artifacts → "no artifact matched" violation
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	require.NotNil(t, v)
	assert.True(t, v.Terminal, "Config.Terminal=true must lift through to Violation.Terminal")
}

// TestConfigTerminal_DefaultFalseForArtifactMinEntries — without
// the operator override, artifact_min_entries failures stay
// non-terminal so the executor retries normally.
func TestConfigTerminal_DefaultFalseForArtifactMinEntries(t *testing.T) {
	cfg := Config{
		Type: "artifact_min_entries",
		Params: map[string]any{
			"artifact_pattern": "scan-*.md",
			"min":              1,
		},
	}
	in := Input{}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	require.NotNil(t, v)
	assert.False(t, v.Terminal, "artifact_min_entries default must NOT be terminal")
}

// TestConfigFromMap_TerminalFalseIgnored — operators can promote
// non-terminal → terminal, but not the other way around. The verifier
// impl knows when a failure is genuinely non-retryable (rate limit)
// and overruling that to "do retry" is almost always a misconfig.
func TestConfigFromMap_TerminalFalseIgnored(t *testing.T) {
	cfg, ok := ConfigFromMap(map[string]any{
		"type":     "no_status_429_in_audit",
		"terminal": false,
	})
	require.True(t, ok)
	// Config.Terminal stays false (operator didn't promote it), but
	// the verifier impl still emits Terminal=true on the Violation.
	assert.False(t, cfg.Terminal, "Config.Terminal honours operator's literal value")

	in := Input{AuditEntries: []*persistence.ToolAuditEntry{
		{ToolName: "mcp__scraper__web_fetch", ToolOutput: `{"status_code":429}`},
	}}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	require.NotNil(t, v)
	assert.True(t, v.Terminal, "verifier impl's Terminal=true is not undone by operator's terminal:false")
}

// TestConfigFromMap_TerminalMissing — the common case: no
// `terminal` field in YAML → Config.Terminal=false, impl default
// applies.
func TestConfigFromMap_TerminalMissing(t *testing.T) {
	cfg, ok := ConfigFromMap(map[string]any{
		"type": "artifact_min_entries",
	})
	require.True(t, ok)
	assert.False(t, cfg.Terminal)
}
