package executor

import (
	"testing"

	"vornik.io/vornik/internal/stepoutcome"
)

// Wave 2 of the unclassified-step-outcome design. The residual bucket was
// 3,027 of 5,791 classified step failures (52.3%) and 87.6% of it was
// NAMEABLE — the refiner simply had four match arms and the fleet's failures
// needed more. Every count below was measured on the production database 2026-08-26 over
// the whole bucket.
//
// Design: https://docs.vornik.io (D1)
func TestRefineAgentFailureOutcome_NewClasses(t *testing.T) {
	tests := []struct {
		name      string
		detail    string
		wantOut   stepoutcome.Outcome
		wantClass string
	}{
		{
			// 1,374 rows — 45.4% of the bucket, and 175 of the trailing 30
			// days. The fleet's single most common failure was an upstream
			// provider error wearing a container-shaped name.
			name:      "llm provider failure (1374 rows, the largest slice)",
			detail:    "agent reported FAILED status: LLM call failed: upstream provider returned an error",
			wantOut:   stepoutcome.Failed,
			wantClass: stepoutcome.ClassLLMCallFailed,
		},
		{
			name:      "llm failure via curl transport",
			detail:    "agent reported FAILED status: LLM call failed: curl failed (exit 7): curl: (7) Failed to connect",
			wantOut:   stepoutcome.Failed,
			wantClass: stepoutcome.ClassLLMCallFailed,
		},
		{
			// 183 rows.
			name:      "missing prerequisite",
			detail:    `agent reported FAILED status: Missing prerequisite: file_read of "/app/workspace/project/analysis.md"`,
			wantOut:   stepoutcome.Failed,
			wantClass: stepoutcome.ClassMissingPrerequisite,
		},
		{
			// 61 rows. Both literals select the same rows; either must match.
			name:      "container refused to start",
			detail:    "failed to start container: failed to start container: podman run failed: exit status 125",
			wantOut:   stepoutcome.Failed,
			wantClass: stepoutcome.ClassContainerStartFailed,
		},
		{
			// 154 rows total for `podman wait failed`, of which 47 are the
			// `signal: killed` subset claimed by the higher-precedence arm.
			name:      "container wait failed (107 rows after the killed subset)",
			detail:    "failed waiting for container: podman wait failed: exit status 1",
			wantOut:   stepoutcome.Failed,
			wantClass: stepoutcome.ClassContainerWaitFailed,
		},
		{
			// THE PRECEDENCE CASE. `signal: killed` rows are a strict SUBSET
			// of `podman wait failed` rows — the full literal contains both.
			// If container_wait_failed is matched first, all 47 kills vanish
			// into the generic bucket and an OOM becomes indistinguishable
			// from a wait error.
			name:      "killed container must not be read as a generic wait failure",
			detail:    "failed waiting for container: podman wait failed: signal: killed",
			wantOut:   stepoutcome.Failed,
			wantClass: stepoutcome.ClassContainerKilled,
		},
		{
			// The residual, correctly named now.
			name:      "genuinely unrecognised text lands in the residual",
			detail:    "agent reported FAILED status: some novel failure nobody has classified",
			wantOut:   stepoutcome.Failed,
			wantClass: stepoutcome.ClassUnclassified,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotOut, gotClass := refineAgentFailureOutcome(tc.detail)
			if gotOut != tc.wantOut {
				t.Errorf("outcome: got %q want %q", gotOut, tc.wantOut)
			}
			if gotClass != tc.wantClass {
				t.Errorf("class: got %q want %q", gotClass, tc.wantClass)
			}
		})
	}
}

// The four pre-existing arms are ordered against two documented misreadings
// (the degenerate-loop message DISCUSSES the context window; error_detail
// carries `context_size=` in the log tail). The new arms append AFTER them, so
// a message matching both an old and a new arm must still take the old one.
func TestRefineAgentFailureOutcome_ExistingArmsKeepPrecedence(t *testing.T) {
	// A provider error that also mentions the context window is a context
	// overflow, not a generic LLM call failure — the more specific arm wins.
	_, class := refineAgentFailureOutcome(
		"agent reported FAILED status: LLM call failed: request exceeds the model's context window")
	if class != stepoutcome.ClassContextOverflow {
		t.Errorf("context window must outrank the new llm arm: got %q", class)
	}

	// An iteration cap whose log tail happens to mention an LLM call is still
	// an iteration cap.
	_, class = refineAgentFailureOutcome(
		"agent reported FAILED status: Tool iteration limit (20) reached.\n\n" +
			"--- Container Log (last 400 lines) ---\nLLM call failed: retrying\n")
	if class != stepoutcome.ClassIterationCap {
		t.Errorf("iteration cap must outrank the new llm arm: got %q", class)
	}
}

// timeoutOutcomeAndClass prefers a NAMED cause over the wall clock, and uses
// the residual class as its "nothing recognised" sentinel. Renaming the
// sentinel must not break that, and the new classes must now count as named —
// a step that ran out of clock while the provider was failing is a provider
// failure, and more wall clock would not have helped it.
func TestTimeoutOutcomeAndClass_NewClassesCountAsNamed(t *testing.T) {
	out, class := timeoutOutcomeAndClass(
		newContainerExitError(1, "agent reported FAILED status: LLM call failed: gateway error 400"))
	if class != stepoutcome.ClassLLMCallFailed {
		t.Errorf("a named provider failure must outrank the wall clock: got %q", class)
	}
	if out != string(stepoutcome.Failed) {
		t.Errorf("outcome: got %q want %q", out, stepoutcome.Failed)
	}

	// Nothing named: the timeout pair stands, because that is the one case a
	// timeout raise legitimately addresses.
	out, class = timeoutOutcomeAndClass(newContainerExitError(1, "some novel failure"))
	if class != stepoutcome.ClassContextTimeout || out != string(stepoutcome.Timeout) {
		t.Errorf("unnamed timeout must keep the timeout pair: got %q/%q", out, class)
	}
}
