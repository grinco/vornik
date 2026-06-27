// Package ui: tests for the pure helpers that don't touch I/O —
// parseCheckpointView / parseScratchpadQuestions / buildPhaseTracker
// (task-conversation view layer) + truncateError (post-mortem URL
// short-circuit) + severityClass (trading panel CSS map).
package ui

import (
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// --- parseCheckpointView ---------------------------------------------

func TestParseCheckpointView_Empty(t *testing.T) {
	if got := parseCheckpointView(nil); got != nil {
		t.Errorf("expected nil for empty bytes; got %+v", got)
	}
}

func TestParseCheckpointView_InvalidJSON(t *testing.T) {
	if got := parseCheckpointView([]byte("not-json")); got != nil {
		t.Errorf("expected nil for invalid JSON; got %+v", got)
	}
}

func TestParseCheckpointView_HappyPath_FreeText(t *testing.T) {
	raw := []byte(`{
		"kind": "free_text",
		"question": "What email?",
		"task_for_human": "compose the launch email",
		"draft": "Subject: ...",
		"expected_by": "2026-05-21T12:00:00Z",
		"default_if_no_response": "send default"
	}`)
	got := parseCheckpointView(raw)
	if got == nil {
		t.Fatal("expected non-nil view")
	}
	if got.Kind != "free_text" || got.Question != "What email?" {
		t.Errorf("payload: %+v", got)
	}
	if got.DefaultIfNoResponse != "send default" {
		t.Errorf("default: %q", got.DefaultIfNoResponse)
	}
	if len(got.Options) != 0 {
		t.Errorf("expected no options; got %v", got.Options)
	}
}

func TestParseCheckpointView_HappyPath_Decision(t *testing.T) {
	raw := []byte(`{
		"kind": "decision",
		"question": "Approve?",
		"options": [
			{"id":"yes","label":"Yes, ship it"},
			{"id":"no","label":"Block"}
		]
	}`)
	got := parseCheckpointView(raw)
	if got == nil {
		t.Fatal("expected non-nil view")
	}
	if len(got.Options) != 2 {
		t.Fatalf("options: got %d, want 2", len(got.Options))
	}
	if got.Options[0].ID != "yes" || got.Options[0].Label != "Yes, ship it" {
		t.Errorf("first option: %+v", got.Options[0])
	}
}

// --- parseScratchpadQuestions ----------------------------------------

func TestParseScratchpadQuestions(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want []string
	}{
		{"empty", nil, nil},
		{"invalid-json", []byte(`{`), nil},
		{"empty-array", []byte(`[]`), []string{}},
		{"happy", []byte(`["q1","q2","q3"]`), []string{"q1", "q2", "q3"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseScratchpadQuestions(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("len: got %d, want %d", len(got), len(tc.want))
			}
			for i, v := range got {
				if v != tc.want[i] {
					t.Errorf("[%d]: got %q, want %q", i, v, tc.want[i])
				}
			}
		})
	}
}

// --- buildPhaseTracker -----------------------------------------------

func TestBuildPhaseTracker_NilOrEmpty(t *testing.T) {
	if got := buildPhaseTracker(nil); got != nil {
		t.Errorf("nil input: got %v", got)
	}
	if got := buildPhaseTracker(&persistence.TaskScratchpad{}); got != nil {
		t.Errorf("empty PhaseHistory: got %v", got)
	}
}

func TestBuildPhaseTracker_InvalidJSON(t *testing.T) {
	sp := &persistence.TaskScratchpad{
		PhaseHistory: []byte("{not-json"),
	}
	if got := buildPhaseTracker(sp); got != nil {
		t.Errorf("invalid PhaseHistory should yield nil; got %v", got)
	}
}

func TestBuildPhaseTracker_HappyPath_WithCurrent(t *testing.T) {
	current := "phase-2"
	sp := &persistence.TaskScratchpad{
		CurrentPhase: &current,
		PhaseHistory: []byte(`[
			{"name":"phase-1","status":"completed"},
			{"name":"phase-2","status":"in-progress"},
			{"name":"phase-3","status":"pending"}
		]`),
	}
	got := buildPhaseTracker(sp)
	if len(got) != 3 {
		t.Fatalf("count: got %d, want 3", len(got))
	}
	if got[1].Name != "phase-2" || !got[1].IsCurrent {
		t.Errorf("phase-2 should be current: %+v", got[1])
	}
	if got[0].IsCurrent || got[2].IsCurrent {
		t.Errorf("only phase-2 should be current; got %+v", got)
	}
}

// --- truncateError ---------------------------------------------------

func TestTruncateError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"short", errors.New("oops"), "oops"},
		{"exactly-200", errors.New(strings.Repeat("x", 200)), strings.Repeat("x", 200)},
		{"long-gets-suffix", errors.New(strings.Repeat("x", 250)), strings.Repeat("x", 200) + "…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateError(tc.err)
			if got != tc.want {
				t.Errorf("len(got)=%d, len(want)=%d", len(got), len(tc.want))
			}
		})
	}
}

// --- severityClass ---------------------------------------------------

func TestSeverityClass(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"critical", "outcome-bad"},
		{"warn", "outcome-warn"},
		{"info", "outcome-neutral"},
		{"", "outcome-neutral"},
		{"unknown", "outcome-neutral"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := severityClass(tc.in); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
