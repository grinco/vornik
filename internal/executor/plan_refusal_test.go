package executor

import (
	"errors"
	"strings"
	"testing"
)

// TestIsPlanRefusal_OnlyMatchesRefusal — the retry only fires for
// the explicit refusal case. Invalid-JSON and no-steps failures stay
// on the existing fail-fast path because the shape-retry layer
// already gave them one re-run.
func TestIsPlanRefusal_OnlyMatchesRefusal(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"refusal", errors.New("lead agent refused to plan: out of scope"), true},
		{"refusal wrapped", errors.New("could not parse plan from lead output: lead agent refused to plan"), true},
		{"invalid json", errors.New("invalid JSON from lead agent: unexpected token"), false},
		{"no steps", errors.New("plan contains no steps"), false},
		{"empty result", errors.New("empty result from lead agent"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isPlanRefusal(tc.err)
			if got != tc.want {
				t.Fatalf("got %v, want %v for %q", got, tc.want, tc.err)
			}
		})
	}
}

// TestPlanRefusalCorrectiveHint_HasResearchFallback — the corrective
// hint must offer a concrete escape valve: a research-only plan that
// the swarm CAN execute. Without that, the lead is likely to repeat
// the refusal because it has no alternative shape to emit.
func TestPlanRefusalCorrectiveHint_HasResearchFallback(t *testing.T) {
	prev := errors.New("lead agent refused to plan: too risky")
	hint := planRefusalCorrectiveHint(prev)
	for _, want := range []string{
		"CORRECTION",
		"too risky", // previous error inlined
		`"role": "researcher"`,
		"research-only plan",
		"Respond ONLY",
	} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint missing %q:\n%s", want, hint)
		}
	}
}

// TestPlanRefusalCorrectiveHint_TruncatesLongError — agent error
// strings can be very long when the model emitted a wall of refusal
// prose. The hint must clip them so the next request body stays
// within reasonable bounds.
func TestPlanRefusalCorrectiveHint_TruncatesLongError(t *testing.T) {
	long := errors.New(strings.Repeat("x", 2000))
	hint := planRefusalCorrectiveHint(long)
	if len(hint) > 1500 {
		t.Fatalf("hint too long: %d chars", len(hint))
	}
	if !strings.Contains(hint, "(truncated)") {
		t.Error("missing truncation marker in long-error hint")
	}
}
