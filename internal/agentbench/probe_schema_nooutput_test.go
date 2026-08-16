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

// NoOutputByOutcome alone cannot name a cause: every container failure arrives
// as outcome "failed". The 2026-08-16 long-horizon arm's 73 failures were one
// indistinguishable bucket, and the real split had to be recovered by hand
// from error_detail prose. Error class is the axis that separates them.
func TestScoreSchema_NoOutputByErrorClass(t *testing.T) {
	trace := Trace{
		ExecutionID: "exec-nobec-1",
		Outcomes: []StepOutcome{
			{StepID: "s1", Role: "tester", Outcome: OutcomeSchemaViolation, Attempt: 1, ErrorClass: "plausibility_violation"},
			{StepID: "s2", Role: "coder", Outcome: OutcomeDegenerateLoop, Attempt: 1, ErrorClass: "degenerate_loop"},
			{StepID: "s3", Role: "analyst", Outcome: OutcomeFailed, Attempt: 1, ErrorClass: "context_overflow"},
			{StepID: "s4", Role: "analyst", Outcome: OutcomeDegenerateLoop, Attempt: 1, ErrorClass: "degenerate_loop"},
			{StepID: "s5", Role: "analyst", Outcome: OutcomeIterationExhausted, Attempt: 1, ErrorClass: "iteration_cap"},
			{StepID: "s6", Role: "coder", Outcome: OutcomeOK, Attempt: 1},
		},
	}

	v := SchemaProbe{}.ScoreSchema(trace, TaskRef{ID: "lh-01"})

	// s1 is a schema_violation, which IS judgeable — it produced output. The
	// other four produced nothing.
	if v.NoOutput != 4 {
		t.Fatalf("NoOutput: got %d want 4 (byClass=%v)", v.NoOutput, v.NoOutputByErrorClass)
	}
	want := map[string]int{
		"degenerate_loop":  2,
		"context_overflow": 1,
		"iteration_cap":    1,
	}
	for class, n := range want {
		if v.NoOutputByErrorClass[class] != n {
			t.Errorf("class %q: got %d want %d", class, v.NoOutputByErrorClass[class], n)
		}
	}
	if len(v.NoOutputByErrorClass) != len(want) {
		t.Errorf("unexpected classes present: %v", v.NoOutputByErrorClass)
	}

	// The breakdown must account for every no-output step, or it misleads
	// exactly where it is meant to explain.
	sum := 0
	for _, n := range v.NoOutputByErrorClass {
		sum += n
	}
	if sum != v.NoOutput {
		t.Errorf("class shares sum to %d but NoOutput is %d", sum, v.NoOutput)
	}
}

// A no-output step whose class nothing recorded must still be counted, under a
// name that says so. Dropping it would break the sum-to-NoOutput property and
// hide the very steps most in need of attention.
func TestScoreSchema_NoOutputByErrorClass_EmptyClassBucketsAsUnclassified(t *testing.T) {
	trace := Trace{
		ExecutionID: "exec-nobec-2",
		Outcomes:    []StepOutcome{{StepID: "s1", Role: "coder", Outcome: OutcomeFailed, Attempt: 1}},
	}
	v := SchemaProbe{}.ScoreSchema(trace, TaskRef{ID: "lh-02"})
	if v.NoOutputByErrorClass["unclassified"] != 1 {
		t.Errorf(`empty error class must bucket as "unclassified", got %v`, v.NoOutputByErrorClass)
	}
}
