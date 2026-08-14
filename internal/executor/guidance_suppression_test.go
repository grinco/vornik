package executor

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/promptblock"
)

// The operator knob LLD 09 §13.3.1 promises. Compiling guidance into the binary
// is what makes an upgrade reach every existing deployment (§13.1), but it also
// removes an operator's ability to switch off a block that is redundant for
// their swarm. These tests pin the two halves of the resolution: advisory blocks
// go away when suppressed, and the invariant block does not — whatever the
// config says.

// systemPromptFor composes the agent system prompt the way buildAgentContextMap
// does, so these tests exercise the real seam rather than the block constants.
func systemPromptFor(t *testing.T, opts *agentInputOpts) string {
	t.Helper()
	ctx := buildAgentContextMap("dev", "do the thing", currentDateTimeContext{}, opts)
	sp, ok := ctx["systemPrompt"].(string)
	if !ok {
		t.Fatalf("no systemPrompt composed; context keys: %v", keysOf(ctx))
	}
	return sp
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func baseOpts() *agentInputOpts {
	return &agentInputOpts{
		SystemPrompt:       "You are the worker role.",
		CanonicalContext:   CanonicalContext{ProjectContext: "mission: ship it"},
		ToolGrantAvailable: true,
	}
}

func TestGuidanceSuppression_NoListEmitsEveryBlock(t *testing.T) {
	sp := systemPromptFor(t, baseOpts())
	for name, text := range map[string]string{
		promptblock.CanonicalContext:   canonicalContextSystemPromptBlock,
		promptblock.ToolBudget:         toolGrantSystemPromptBlock,
		promptblock.ReportingIntegrity: claimVerificationSystemPromptBlock,
	} {
		if !strings.Contains(sp, strings.TrimSpace(text)) {
			t.Errorf("block %q missing from the default prompt", name)
		}
	}
}

func TestGuidanceSuppression_AdvisoryBlockIsRemoved(t *testing.T) {
	cases := []struct {
		name      string
		suppress  string
		gone      string
		stillHere []string
	}{
		{
			name:     "tool budget",
			suppress: promptblock.ToolBudget,
			gone:     toolGrantSystemPromptBlock,
			stillHere: []string{
				canonicalContextSystemPromptBlock,
				claimVerificationSystemPromptBlock,
			},
		},
		{
			name:     "canonical context",
			suppress: promptblock.CanonicalContext,
			gone:     canonicalContextSystemPromptBlock,
			stillHere: []string{
				toolGrantSystemPromptBlock,
				claimVerificationSystemPromptBlock,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := baseOpts()
			opts.SuppressedGuidanceBlocks = []string{tc.suppress}
			sp := systemPromptFor(t, opts)
			if strings.Contains(sp, strings.TrimSpace(tc.gone)) {
				t.Errorf("suppressed block %q is still in the prompt", tc.suppress)
			}
			for _, keep := range tc.stillHere {
				if !strings.Contains(sp, strings.TrimSpace(keep)) {
					t.Error("suppressing one block removed another")
				}
			}
			// The role's own identity is never a guidance block.
			if !strings.Contains(sp, "You are the worker role.") {
				t.Error("role prompt lost")
			}
		})
	}
}

// The load-bearing test of the whole knob: reporting integrity is INVARIANT.
// verifyRoleClaims runs after every agent step on every deployment whatever the
// prompt says, so a config that suppressed the block would remove the warning
// and not the rule — leaving agents held to a standard nothing told them about,
// which is exactly the gap §13.5a closed.
//
// Config validation already refuses this (registry.validateSuppressedGuidance),
// so this is the second lock: the composition seam itself will not drop it.
func TestGuidanceSuppression_InvariantBlockCannotBeSuppressed(t *testing.T) {
	opts := baseOpts()
	opts.SuppressedGuidanceBlocks = []string{
		promptblock.ReportingIntegrity,
		promptblock.ToolBudget,
	}
	sp := systemPromptFor(t, opts)
	if !strings.Contains(sp, strings.TrimSpace(claimVerificationSystemPromptBlock)) {
		t.Error("reporting integrity was suppressed; it is an invariant block and the rule " +
			"it describes runs whatever the prompt says")
	}
	// The advisory name alongside it still takes effect — an unhonourable entry
	// does not void the rest of the list.
	if strings.Contains(sp, strings.TrimSpace(toolGrantSystemPromptBlock)) {
		t.Error("the advisory entry beside an invariant one was ignored")
	}
}

func TestGuidanceSuppression_UnknownNameSuppressesNothing(t *testing.T) {
	opts := baseOpts()
	opts.SuppressedGuidanceBlocks = []string{"tool-budgett", "  "}
	sp := systemPromptFor(t, opts)
	for _, text := range []string{
		canonicalContextSystemPromptBlock,
		toolGrantSystemPromptBlock,
		claimVerificationSystemPromptBlock,
	} {
		if !strings.Contains(sp, strings.TrimSpace(text)) {
			t.Error("an unknown suppression name removed a real block")
		}
	}
}

// Suppressing every advisory block must not produce an EMPTY system prompt key:
// the entrypoint applies its own default when the key is absent, and a prompt
// that composes down to nothing but whitespace is worse than the default it
// would have replaced (§13.3(1)).
func TestGuidanceSuppression_SuppressingEverythingKeepsAUsablePrompt(t *testing.T) {
	opts := &agentInputOpts{
		CanonicalContext:         CanonicalContext{ProjectContext: "mission: ship it"},
		ToolGrantAvailable:       true,
		SuppressedGuidanceBlocks: promptblock.SuppressibleNames(),
	}
	sp := systemPromptFor(t, opts)
	if strings.TrimSpace(sp) == "" {
		t.Fatal("composed an empty systemPrompt; the entrypoint's own default would have been better")
	}
	if !strings.Contains(sp, strings.TrimSpace(claimVerificationSystemPromptBlock)) {
		t.Error("the invariant block must survive suppressing every advisory one")
	}
}
