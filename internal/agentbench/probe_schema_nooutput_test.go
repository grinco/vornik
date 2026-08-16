package agentbench

import "testing"

// Regression, 2026-08-16. The long-horizon arm reported 32 of 57 terminal steps
// (56.1%) producing no output — the largest single reliability fact in the run —
// and neither the verdict nor the rollup could say what caused it. Container
// exits, timeouts and iteration exhaustion need three different fixes, and a
// bare count picks none of them.
func TestScoreSchema_NoOutputCarriesItsCause(t *testing.T) {
	trace := Trace{
		ExecutionID: "exec1",
		Outcomes: []StepOutcome{
			{StepID: "s1", Role: "coder", Outcome: OutcomeOK, Attempt: 1},
			{StepID: "s2", Role: "coder", Outcome: OutcomeTimeout, Attempt: 1},
			{StepID: "s3", Role: "tester", Outcome: OutcomeTimeout, Attempt: 1},
			{StepID: "s4", Role: "tester", Outcome: OutcomeIterationExhausted, Attempt: 1},
		},
	}

	v := SchemaProbe{}.ScoreSchema(trace, TaskRef{ID: "t1"})

	if v.NoOutput != 3 {
		t.Fatalf("NoOutput = %d, want 3", v.NoOutput)
	}
	if got := v.NoOutputByOutcome[OutcomeTimeout]; got != 2 {
		t.Errorf("NoOutputByOutcome[%s] = %d, want 2", OutcomeTimeout, got)
	}
	if got := v.NoOutputByOutcome[OutcomeIterationExhausted]; got != 1 {
		t.Errorf("NoOutputByOutcome[%s] = %d, want 1", OutcomeIterationExhausted, got)
	}
	// The breakdown must ACCOUNT for the count, or it explains a different
	// number than the one published beside it.
	total := 0
	for _, n := range v.NoOutputByOutcome {
		total += n
	}
	if total != v.NoOutput {
		t.Errorf("breakdown sums to %d but NoOutput is %d", total, v.NoOutput)
	}
}

// A run where everything produced output must not carry an empty map into the
// journal — omitempty keeps it out, and an absent key reads as "nothing to
// explain" rather than "explanation missing".
func TestScoreSchema_NoOutputBreakdownAbsentWhenNothingFailed(t *testing.T) {
	trace := Trace{
		ExecutionID: "exec1",
		Outcomes: []StepOutcome{
			{StepID: "s1", Role: "coder", Outcome: OutcomeOK, Attempt: 1},
		},
	}

	v := SchemaProbe{}.ScoreSchema(trace, TaskRef{ID: "t1"})

	if v.NoOutput != 0 {
		t.Fatalf("NoOutput = %d, want 0", v.NoOutput)
	}
	if len(v.NoOutputByOutcome) != 0 {
		t.Errorf("NoOutputByOutcome = %v, want empty", v.NoOutputByOutcome)
	}
}

// The rollup is what gets published, so the breakdown has to survive the fold
// across executions — otherwise the cause is visible per-execution and gone in
// the number anyone actually reads.
func TestBuildRollup_AggregatesNoOutputCauses(t *testing.T) {
	mk := func(execID, outcome string) ExecutionRecord {
		trace := Trace{
			ExecutionID: execID,
			Outcomes:    []StepOutcome{{StepID: "s1", Role: "coder", Outcome: outcome, Attempt: 1}},
		}
		return ExecutionRecord{
			TaskID:    "t",
			Succeeded: false,
			Verdicts: []Verdict{{
				Probe:  "schema-following",
				Schema: func() *SchemaVerdict { v := SchemaProbe{}.ScoreSchema(trace, TaskRef{ID: "t"}); return &v }(),
			}},
		}
	}

	r := BuildRollup("arm", []ExecutionRecord{
		mk("e1", OutcomeTimeout),
		mk("e2", OutcomeTimeout),
		mk("e3", OutcomeIterationExhausted),
	})

	if r.Accuracy.SchemaNoOutput != 3 {
		t.Fatalf("SchemaNoOutput = %d, want 3", r.Accuracy.SchemaNoOutput)
	}
	if got := r.Accuracy.SchemaNoOutputByOutcome[OutcomeTimeout]; got != 2 {
		t.Errorf("rolled-up %s = %d, want 2", OutcomeTimeout, got)
	}
	if got := r.Accuracy.SchemaNoOutputByOutcome[OutcomeIterationExhausted]; got != 1 {
		t.Errorf("rolled-up %s = %d, want 1", OutcomeIterationExhausted, got)
	}
}
