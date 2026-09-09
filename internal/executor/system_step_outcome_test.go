package executor

import (
	"encoding/json"
	"testing"
)

// `on_outcome` routing for system steps (design
// 2026-09-01-forge-rereview-triggers-design.md §17.2).
//
// Filed by §16.7: the §16 guard stops a review of nothing from being POSTED,
// after the model has already been paid for writing it. This is how a workflow
// declines to pay.

func TestNextAfterSystemStep_RoutesOnTheReportedOutcome(t *testing.T) {
	res := json.RawMessage(`{"message":"nothing new","scope":"no-change","outcome":"no-change"}`)
	got := nextAfterSystemStep("review", map[string]string{"no-change": "nothing_to_review"}, res)
	if got != "nothing_to_review" {
		t.Errorf("next = %q, want nothing_to_review — the reviewer must not run", got)
	}
}

// An outcome the map does not name routes normally. This is what makes the
// feature additive: a handler that grows a new outcome value cannot break a
// deployed workflow that never heard of it.
func TestNextAfterSystemStep_UnnamedOutcomeFallsThroughToOnSuccess(t *testing.T) {
	res := json.RawMessage(`{"outcome":"incremental"}`)
	got := nextAfterSystemStep("review", map[string]string{"no-change": "nothing_to_review"}, res)
	if got != "review" {
		t.Errorf("next = %q, want review", got)
	}
}

// Today's behaviour, pinned. Every handler but forge.fetch_diff reports no
// outcome, and every deployed workflow declares no map.
func TestNextAfterSystemStep_NoOutcomeAndNoMapAreTodaysBehaviour(t *testing.T) {
	for _, tc := range []struct {
		name string
		m    map[string]string
		res  json.RawMessage
	}{
		{"no map at all", nil, json.RawMessage(`{"outcome":"no-change"}`)},
		{"map, but the handler reports nothing", map[string]string{"no-change": "t"}, json.RawMessage(`{"message":"x"}`)},
		{"empty result", map[string]string{"no-change": "t"}, nil},
		{"result is not an object", map[string]string{"no-change": "t"}, json.RawMessage(`"a bare string"`)},
		{"outcome is not a string", map[string]string{"no-change": "t"}, json.RawMessage(`{"outcome":42}`)},
		{"target is empty", map[string]string{"no-change": ""}, json.RawMessage(`{"outcome":"no-change"}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextAfterSystemStep("review", tc.m, tc.res); got != "review" {
				t.Errorf("next = %q, want review — this must route exactly as it did before on_outcome existed", got)
			}
		})
	}
}

// A malformed envelope must not panic or fail; it reads as "no outcome".
func TestSystemStepOutcome_UnreadableIsEmptyNotAnError(t *testing.T) {
	for _, in := range []string{``, `{`, `null`, `[]`, `{"outcome":null}`} {
		if got := systemStepOutcome(json.RawMessage(in)); got != "" {
			t.Errorf("systemStepOutcome(%q) = %q, want empty", in, got)
		}
	}
}
