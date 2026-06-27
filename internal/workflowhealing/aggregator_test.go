package workflowhealing

import (
	"context"
	"errors"
	"math"
	"testing"

	"vornik.io/vornik/internal/contracts"
	"vornik.io/vornik/internal/persistence"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// TestAggregator_AllSignalKinds exercises every per-event rate the
// aggregator computes: hallucination (judge), verifier failure (step),
// and operator intervention (operator op), plus failure-status counting.
func TestAggregator_AllSignalKinds(t *testing.T) {
	agg := newAggregator()

	// Run 1: completed, one verifier step that FAILED, one failing
	// judge verdict, one intervention. EE adapter sets flat fields on
	// ExecutionEvent (Role, Outcome, Verdict, Hallucination, Detail).
	{
		v := traceWith("t1", "COMPLETED", 0.10, 10,
			contracts.ExecutionEvent{Kind: eventKindStep, Role: "verifier", Outcome: "schema_violation"},
			judgeEvent("fail"),
			contracts.ExecutionEvent{Kind: eventKindOperatorOp, Detail: "retry"},
		)
		agg.add(&v)
	}
	// Run 2: FAILED status, one verifier step that PASSED (outcome ok),
	// one passing judge verdict, no intervention.
	{
		v := traceWith("t2", "FAILED", 0.30, 20,
			contracts.ExecutionEvent{Kind: eventKindStep, Role: "verifier", Outcome: "ok"},
			judgeEvent("pass"),
		)
		agg.add(&v)
	}
	// nil trace is a no-op.
	agg.add(nil)

	s := agg.summary()
	if s.Runs != 2 {
		t.Fatalf("runs = %d, want 2", s.Runs)
	}
	if s.Successes != 1 || s.Failures != 1 {
		t.Errorf("successes/failures = %d/%d, want 1/1", s.Successes, s.Failures)
	}
	if !near(s.AvgCostUSD, 0.20) {
		t.Errorf("avg cost = %f, want 0.20", s.AvgCostUSD)
	}
	if !near(s.AvgDurationSeconds, 15) {
		t.Errorf("avg duration = %f, want 15", s.AvgDurationSeconds)
	}
	// 1 hallucination of 2 judge verdicts.
	if !near(s.HallucinationRate, 0.5) {
		t.Errorf("hallucination rate = %f, want 0.5", s.HallucinationRate)
	}
	// 1 failed verifier step of 2 verifier steps.
	if !near(s.VerifierFailureRate, 0.5) {
		t.Errorf("verifier failure rate = %f, want 0.5", s.VerifierFailureRate)
	}
	// 1 intervention over 2 runs.
	if !near(s.OperatorInterventionRate, 0.5) {
		t.Errorf("intervention rate = %f, want 0.5", s.OperatorInterventionRate)
	}
}

func TestAggregator_EmptySummaryHasZeroRates(t *testing.T) {
	s := newAggregator().summary()
	if s.Runs != 0 || s.HallucinationRate != 0 || s.VerifierFailureRate != 0 || s.OperatorInterventionRate != 0 || s.AvgCostUSD != 0 || s.AvgDurationSeconds != 0 {
		t.Errorf("empty aggregate should be all-zero, got %+v", s)
	}
}

func TestSignalHelpers(t *testing.T) {
	// hallucination via boolean flag (EE adapter pre-computes this).
	if !isHallucinationVerdict(contracts.ExecutionEvent{Hallucination: true}) {
		t.Error("boolean hallucination flag should count")
	}
	// Real judge vocabulary is "pass"/"fail"/"abstain": "fail" counts,
	// "pass" does not.
	if !isHallucinationVerdict(contracts.ExecutionEvent{Verdict: "fail"}) {
		t.Error(`judge verdict "fail" should count as a failing signal`)
	}
	if isHallucinationVerdict(contracts.ExecutionEvent{Verdict: "pass"}) {
		t.Error(`judge verdict "pass" must not count`)
	}
	// EE adapter sets Role on step events.
	if !isVerifierStep(contracts.ExecutionEvent{Role: "verifier"}) {
		t.Error("verifier role should be detected")
	}
	if isVerifierStep(contracts.ExecutionEvent{Role: "writer"}) {
		t.Error("writer is not a verifier")
	}
	// failed step — keyed on Outcome (stepoutcome vocabulary):
	// any non-"ok" terminal outcome is a failure.
	if !isFailedStep(contracts.ExecutionEvent{Outcome: "parse_error"}) {
		t.Error("parse_error outcome should be a failed step")
	}
	if isFailedStep(contracts.ExecutionEvent{Outcome: "ok"}) {
		t.Error("ok outcome is not a failed step")
	}
	if isFailedStep(contracts.ExecutionEvent{}) {
		t.Error("empty outcome is not a failed step")
	}
	// intervention — EE adapter sets Detail to the op value for operator_op events.
	if !isIntervention(contracts.ExecutionEvent{Detail: "fork"}) {
		t.Error("fork action should be an intervention")
	}
	if isIntervention(contracts.ExecutionEvent{Detail: "noop"}) {
		t.Error("noop op is not an intervention")
	}
}

func TestMustJSON_FallsBackOnUnmarshalable(t *testing.T) {
	if got := mustJSON(make(chan int)); got != "{}" {
		t.Errorf("mustJSON(chan) = %q, want {}", got)
	}
	if got := mustJSON(map[string]int{"a": 1}); got != `{"a":1}` {
		t.Errorf("mustJSON map = %q", got)
	}
}

// TestRunTrial_StaticAdvanceWarnDoesNotBlock: when SetStatus errors
// after a passed verdict, the trial still completes (the verdict is on
// the trial row; the status mirror is best-effort).
func TestRunTrial_StaticAdvanceWarnDoesNotBlock(t *testing.T) {
	cands := newFakeCandidateRepo()
	trials := newFakeTrialRepo()
	h, _ := GenomeHashFromMarkdown([]byte(validCandidateMD), "dev-pipeline.md")
	c := seedCandidate(t, cands, persistence.HealingCandidateDraft, validCandidateMD, h)
	cands.setErr = errors.New("status write failed")

	r := newTestRunner(cands, trials, nil, GateThresholds{}, 0)
	res, err := r.RunTrial(context.Background(), c.ID, persistence.HealingTrialModeStatic, nil)
	if err != nil {
		t.Fatalf("RunTrial should not fail on a status-mirror error: %v", err)
	}
	if res.Verdict != persistence.HealingTrialPassed {
		t.Fatalf("verdict = %q, want passed", res.Verdict)
	}
	if trials.finishSeen == nil || trials.finishSeen.Verdict != persistence.HealingTrialPassed {
		t.Error("trial row must carry the passed verdict even when the status mirror failed")
	}
}

// TestRunTrial_FinishErrorSurfaces: a Finish failure is returned (with
// the result) so the caller knows the verdict wasn't durably recorded.
func TestRunTrial_FinishErrorSurfaces(t *testing.T) {
	cands := newFakeCandidateRepo()
	trials := newFakeTrialRepo()
	h, _ := GenomeHashFromMarkdown([]byte(validCandidateMD), "dev-pipeline.md")
	c := seedCandidate(t, cands, persistence.HealingCandidateDraft, validCandidateMD, h)
	trials.finishErr = errors.New("finish failed")

	r := newTestRunner(cands, trials, nil, GateThresholds{}, 0)
	res, err := r.RunTrial(context.Background(), c.ID, persistence.HealingTrialModeStatic, nil)
	if err == nil {
		t.Fatal("expected an error when Finish fails")
	}
	if res == nil || res.Verdict != persistence.HealingTrialPassed {
		t.Error("result (with verdict) should still be returned alongside the finish error")
	}
}

// TestRunReplay_SkipsEvidenceWithMissingBaseline: an evidence id whose
// baseline trace can't be assembled is skipped; with too few remaining
// the trial is inconclusive.
func TestRunReplay_SkipsEvidenceWithMissingBaseline(t *testing.T) {
	cands := newFakeCandidateRepo()
	trials := newFakeTrialRepo()
	c := seedCandidate(t, cands, persistence.HealingCandidateDraft, validCandidateMD, "")

	eng := newFakeReplayEngine()
	// ev1 baseline errors; ev2 is fine. Only 1 comparable run < min 2.
	eng.baselineErr["ev1"] = errors.New("no recorded trace")
	{
		v := traceWith("ev2", "completed", 0.10, 10)
		eng.baseline["ev2"] = &v
	}
	{
		v := traceWith("ev2", "completed", 0.05, 5)
		eng.replay["ev2"] = &v
	}

	r := newTestRunner(cands, trials, eng, DefaultGateThresholds(), 2)
	res, err := r.RunTrial(context.Background(), c.ID, persistence.HealingTrialModeReplay, []string{"ev1", "ev2"})
	if err != nil {
		t.Fatalf("RunTrial: %v", err)
	}
	if res.Verdict != persistence.HealingTrialInconclusive {
		t.Fatalf("verdict = %q, want inconclusive (only 1 comparable run)", res.Verdict)
	}
}

// TestRunReplay_SkipsEvidenceWithReplayError: a replay-engine error on
// one evidence id is skipped rather than failing the whole trial.
func TestRunReplay_SkipsEvidenceWithReplayError(t *testing.T) {
	cands := newFakeCandidateRepo()
	trials := newFakeTrialRepo()
	c := seedCandidate(t, cands, persistence.HealingCandidateDraft, validCandidateMD, "")

	eng := newFakeReplayEngine()
	{
		v := traceWith("ev1", "completed", 0.10, 10)
		eng.baseline["ev1"] = &v
	}
	{
		v := traceWith("ev2", "completed", 0.10, 10)
		eng.baseline["ev2"] = &v
	}
	eng.replayErr["ev1"] = errors.New("replay boom")
	{
		v := traceWith("ev2", "completed", 0.05, 5)
		eng.replay["ev2"] = &v
	}

	r := newTestRunner(cands, trials, eng, DefaultGateThresholds(), 2)
	res, err := r.RunTrial(context.Background(), c.ID, persistence.HealingTrialModeReplay, []string{"ev1", "ev2"})
	if err != nil {
		t.Fatalf("RunTrial: %v", err)
	}
	// Only ev2 was comparable → below min 2 → inconclusive.
	if res.Verdict != persistence.HealingTrialInconclusive {
		t.Fatalf("verdict = %q, want inconclusive", res.Verdict)
	}
}
