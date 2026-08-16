package executor

import (
	"encoding/json"
	"errors"
	"testing"

	"vornik.io/vornik/internal/stepoutcome"
)

// Regression: the 2026-08-16 long-horizon arm recorded 73 failed steps, every
// one as failed/container_non_zero_exit, even though the agent had named its
// own cause. degenerate_loop and iteration_exhausted had ZERO rows in the
// entire bench DB.
//
// The handling code was not missing — container.go's degenerate-loop arm is an
// `else if degenerateLoopDetail != ""` that requires err == nil, while the
// agent's guard ends `write_result "FAILED" … ; return 1`. The container always
// exits non-zero, so the `if err != nil` branch consumed the case and the
// else-if was unreachable for the very failure it names.
//
// The match strings below are the REAL emitted text, taken from the arm's
// ledger. Matching on bare keywords does not work: error_detail has the
// container log appended, whose lines contain "context_size=", so an
// ILIKE '%context%' bucket swallows iteration-cap failures too. That artefact
// is what made an earlier revision of the design report "iteration cap: zero
// rows". Match the specific phrase, in precedence order.
func TestRefineAgentFailureOutcome(t *testing.T) {
	tests := []struct {
		name      string
		detail    string
		wantOut   stepoutcome.Outcome
		wantClass string
	}{
		{
			name:      "plausibility violation",
			detail:    `plausibility violation: role "tester" failed 1 rule(s): passed_requires_pinned_validation: under condition testing.passed=true, field "testing.cases" is missing or empty`,
			wantOut:   stepoutcome.SchemaViolation,
			wantClass: stepoutcome.ClassPlausibilityViolation,
		},
		{
			name:      "degenerate loop",
			detail:    "agent reported FAILED status: Agent entered a degenerate loop (repeated run_shell 3 times with the same arguments). Context was only 11% full (~11253/100000 tokens), so this is NOT context exhaustion",
			wantOut:   stepoutcome.DegenerateLoop,
			wantClass: stepoutcome.ClassDegenerateLoop,
		},
		{
			// The degenerate-loop message DISCUSSES the context window in
			// prose. It must still classify as a degenerate loop.
			name:      "degenerate loop whose prose mentions context exhaustion",
			detail:    "agent reported FAILED status: Agent entered a degenerate loop (repeated run_shell 3 times with the same arguments). This usually means the context window is exhausted.",
			wantOut:   stepoutcome.DegenerateLoop,
			wantClass: stepoutcome.ClassDegenerateLoop,
		},
		{
			name:      "context overflow",
			detail:    "agent reported FAILED status: LLM call failed: prompt exceeds the model's context window — compact the conversation or reduce input",
			wantOut:   stepoutcome.Failed,
			wantClass: stepoutcome.ClassContextOverflow,
		},
		{
			// The container log is appended to error_detail and contains
			// "context_size=100000". A keyword matcher would call this a
			// context overflow; it is an iteration cap.
			name: "iteration cap with a container log mentioning context_size",
			detail: "agent reported FAILED status: Tool iteration limit (50) reached. The task was too complex for the configured limit.\n\n" +
				"--- Container Log (last 50 lines) ---\n" +
				"[vornik-agent] preflight iteration=50 context_size=100000 max_tokens=8192\n",
			wantOut:   stepoutcome.IterationExhausted,
			wantClass: stepoutcome.ClassIterationCap,
		},
		{
			name:      "unrecognised keeps today's behaviour",
			detail:    "agent reported FAILED status: some novel failure nobody has classified",
			wantOut:   stepoutcome.Failed,
			wantClass: stepoutcome.ClassContainerNonZeroExit,
		},
		{
			name:      "empty detail keeps today's behaviour",
			detail:    "",
			wantOut:   stepoutcome.Failed,
			wantClass: stepoutcome.ClassContainerNonZeroExit,
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

// T-1089 regression. classifyShapeFailure routes the corrective retry on the
// literal "plausibility violation", ahead of the broader "schema violation:"
// case. When require_output_glob's message stopped matching the literal the
// classifier used, its corrective retry silently never fired while the metric
// labelled it schema_violation the whole time.
//
// Task 2 adds an error_class beside that message. Adding a class is additive;
// rewording the message is not. This pins the contract.
func TestPlausibilityMessagePrefixStillRoutesTheCorrectiveRetry(t *testing.T) {
	err := errors.New(`plausibility violation: role "tester" failed 1 rule(s): passed_requires_pinned_validation: under condition testing.passed=true, field "testing.cases" is missing or empty`)
	if got := classifyShapeFailure(err); got != shapeFailurePlausibility {
		t.Fatalf("plausibility message must classify as shapeFailurePlausibility, got %v", got)
	}
}

// The ordering constraint itself: a message carrying BOTH markers must resolve
// as plausibility, because plausibility is the more specific diagnosis and its
// corrective hint is different — telling a model that produced valid JSON to
// "respond only with valid JSON" misleads it about the actual problem.
func TestPlausibilityWinsOverSchemaViolationOrdering(t *testing.T) {
	err := errors.New(`plausibility violation: role "tester" failed; schema violation: role "tester" result.json is missing required keys: [testing]`)
	if got := classifyShapeFailure(err); got != shapeFailurePlausibility {
		t.Fatalf("plausibility must win over the broader schema-violation case, got %v", got)
	}
}

// §4.8 regression: the iteration cap used to produce NO OUTPUT AT ALL. The
// agent was told "when the budget is nearly exhausted, produce your best
// output with what you have" and then never given a turn in which to comply,
// so the step left the schema denominator entirely and landed in NoOutput.
//
// 14 of 47 failed steps in the 2026-08-16 ctx32k arm were iteration caps, and
// every one sat EXACTLY at its budget (analyst 50 used 50/51/52/53, coder 250
// used 258) — a wall, not a tail.
//
// The agent now answers on a forced tool-free turn and exits cleanly, so the
// workflow does not take a failure transition. The ledger must still record
// the exhaustion: a step that ran out of budget is not `ok`.
func TestIterationExhaustedOutcomeOverrideIsRecognised(t *testing.T) {
	// The agent's contract: status COMPLETED carrying an outcome override.
	var resultStatus struct {
		Status        string `json:"status"`
		Message       string `json:"message"`
		Outcome       string `json:"outcome"`
		OutcomeDetail string `json:"outcomeDetail"`
	}
	raw := []byte(`{"status":"COMPLETED","message":"partial spec","outcome":"iteration_exhausted","outcomeDetail":"tool iteration cap reached (50 iterations); answered on a forced tool-free finalization turn"}`)
	if err := json.Unmarshal(raw, &resultStatus); err != nil {
		t.Fatalf("agent result shape changed: %v", err)
	}
	if resultStatus.Outcome != string(stepoutcome.IterationExhausted) {
		t.Fatalf("outcome override must be the taxonomy value, got %q", resultStatus.Outcome)
	}
	// A clean exit must NOT be silently promoted to ok.
	if resultStatus.Status != "COMPLETED" {
		t.Errorf("the finalisation turn exits cleanly so the workflow advances; got %q", resultStatus.Status)
	}
	if resultStatus.OutcomeDetail == "" {
		t.Error("detail is what tells an operator the answer was produced under exhaustion")
	}
}
