package workflowhealing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/contracts"
	"vornik.io/vornik/internal/persistence"
)

// ---- adapter fakes -----------------------------------------------------

// fakeHealingApplier satisfies contracts.HealingApplier. It records
// the plans it receives (for assertion) and serves canned traces.
type fakeHealingApplier struct {
	// newID maps an OriginalTaskID to the spawned replay task id.
	newID    map[string]string
	err      error
	stampErr error // returned ALONGSIDE the created trace (Apply's stamp-failure contract)
	gotPlans []contracts.CounterfactualPlan
	// traces maps task id to the ExecutionTrace to return from BaselineTrace.
	traces map[string]contracts.ExecutionTrace
	// baselineErr maps task id to an error to return from BaselineTrace.
	baselineErr error
}

func (f *fakeHealingApplier) ApplyPlan(_ context.Context, plan contracts.CounterfactualPlan) (contracts.ExecutionTrace, error) {
	f.gotPlans = append(f.gotPlans, plan)
	if f.err != nil {
		return contracts.ExecutionTrace{}, f.err
	}
	id := f.newID[plan.OriginalTaskID]
	if id == "" {
		id = "replay-" + plan.OriginalTaskID
	}
	// Return a trace whose TaskID is the new replay task id.
	// Alongside stampErr if set (mirrors the production stamp-failure path).
	return contracts.ExecutionTrace{TaskID: id}, f.stampErr
}

func (f *fakeHealingApplier) BaselineTrace(_ context.Context, taskID string) (contracts.ExecutionTrace, error) {
	if f.baselineErr != nil {
		return contracts.ExecutionTrace{}, f.baselineErr
	}
	tr, ok := f.traces[taskID]
	if !ok {
		return contracts.ExecutionTrace{}, errors.New("trace not found: " + taskID)
	}
	return tr, nil
}

type fakeTaskReader struct {
	// status per task id; absent → treated as terminal COMPLETED so
	// the poll loop doesn't hang in the common happy path.
	status map[string]persistence.TaskStatus
	err    error
}

func (f *fakeTaskReader) Get(_ context.Context, id string) (*persistence.Task, error) {
	if f.err != nil {
		return nil, f.err
	}
	st, ok := f.status[id]
	if !ok {
		st = persistence.TaskStatusCompleted
	}
	return &persistence.Task{ID: id, Status: st}, nil
}

func fastOpts() ReplayAdapterOptions {
	return ReplayAdapterOptions{PollInterval: time.Millisecond, PollTimeout: time.Second}
}

// fakeExecReader serves canned executions for the exec→task resolution
// tests.
type fakeExecReader struct {
	rows map[string]*persistence.Execution
	err  error
}

func (f *fakeExecReader) Get(_ context.Context, id string) (*persistence.Execution, error) {
	if f.err != nil {
		return nil, f.err
	}
	ex, ok := f.rows[id]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	return ex, nil
}

// ---- adapter tests -----------------------------------------------------

func TestReplayAdapter_BaselineTrace(t *testing.T) {
	applier := &fakeHealingApplier{traces: map[string]contracts.ExecutionTrace{
		"ev1": traceWith("ev1", "COMPLETED", 0.1, 10),
	}}
	a := NewReplayEngineAdapter(applier, &fakeTaskReader{}, nil, fastOpts(), zerolog.Nop())
	tr, err := a.BaselineTrace(context.Background(), "ev1")
	if err != nil {
		t.Fatalf("BaselineTrace: %v", err)
	}
	if tr == nil || tr.TaskID != "ev1" {
		t.Fatalf("baseline trace = %+v, want ev1", tr)
	}
}

func TestReplayAdapter_ReplayEvidence_RoutesViaVariableWorkflow(t *testing.T) {
	applier := &fakeHealingApplier{
		newID: map[string]string{"ev1": "replay-ev1"},
		traces: map[string]contracts.ExecutionTrace{
			"replay-ev1": traceWith("replay-ev1", "COMPLETED", 0.05, 8),
		},
	}
	a := NewReplayEngineAdapter(applier, &fakeTaskReader{}, nil, fastOpts(), zerolog.Nop())

	id, tr, err := a.ReplayEvidence(context.Background(), "ev1", "dev-pipeline-candidate-abc")
	if err != nil {
		t.Fatalf("ReplayEvidence: %v", err)
	}
	if id != "replay-ev1" {
		t.Errorf("replayTaskID = %q, want replay-ev1", id)
	}
	if id == "ev1" {
		t.Error("replay task id must differ from the original evidence id")
	}
	if tr == nil || tr.TaskID != "replay-ev1" {
		t.Errorf("candidate trace = %+v, want replay-ev1", tr)
	}
	// The plan must route via CounterfactualVariableWorkflow at the candidate id.
	if len(applier.gotPlans) != 1 {
		t.Fatalf("applied %d plans, want 1", len(applier.gotPlans))
	}
	p := applier.gotPlans[0]
	if p.Variable != CounterfactualVariableWorkflow || p.Value != "dev-pipeline-candidate-abc" || p.OriginalTaskID != "ev1" {
		t.Errorf("plan = %+v; want CounterfactualVariableWorkflow at the candidate id for ev1", p)
	}
}

func TestReplayAdapter_ReplayEvidence_RefusesOriginalTaskID(t *testing.T) {
	// Applier returns the ORIGINAL id — the non-production invariant
	// must fail closed.
	applier := &fakeHealingApplier{newID: map[string]string{"ev1": "ev1"}}
	a := NewReplayEngineAdapter(applier, &fakeTaskReader{}, nil, fastOpts(), zerolog.Nop())
	_, _, err := a.ReplayEvidence(context.Background(), "ev1", "wf-candidate-x")
	if err == nil {
		t.Fatal("expected error when replay returns the original task id")
	}
}

func TestReplayAdapter_ReplayEvidence_TimeoutNeverSettles(t *testing.T) {
	applier := &fakeHealingApplier{newID: map[string]string{"ev1": "replay-ev1"}}
	reader := &fakeTaskReader{status: map[string]persistence.TaskStatus{"replay-ev1": persistence.TaskStatusRunning}}
	a := NewReplayEngineAdapter(applier, reader, nil,
		ReplayAdapterOptions{PollInterval: time.Millisecond, PollTimeout: 20 * time.Millisecond}, zerolog.Nop())
	_, _, err := a.ReplayEvidence(context.Background(), "ev1", "wf-candidate-x")
	if err == nil {
		t.Fatal("expected timeout error when task never reaches terminal")
	}
}

func TestReplayAdapter_ApplyError(t *testing.T) {
	applier := &fakeHealingApplier{err: errors.New("boom")}
	a := NewReplayEngineAdapter(applier, &fakeTaskReader{}, nil, fastOpts(), zerolog.Nop())
	_, _, err := a.ReplayEvidence(context.Background(), "ev1", "wf-candidate-x")
	if err == nil {
		t.Fatal("expected apply error to propagate")
	}
}

func TestNewReplayEngineAdapter_NilDepsReturnNil(t *testing.T) {
	if NewReplayEngineAdapter(nil, &fakeTaskReader{}, nil, fastOpts(), zerolog.Nop()) != nil {
		t.Error("nil applier should yield nil engine")
	}
	if NewReplayEngineAdapter(&fakeHealingApplier{}, nil, nil, fastOpts(), zerolog.Nop()) != nil {
		t.Error("nil task reader should yield nil engine")
	}
}

// TestReplayAdapter_ResolvesExecutionIDs: regression for the
// 2026-06-06 "baseline trace unavailable: blackbox: task not found"
// failure. The healing trigger stores evidence as EXECUTION ids
// (evidence_execution_ids, exec_…), but the adapter handed them to the
// trace assembler + counterfactual engine as TASK ids — every
// replay was skipped and the trial came back inconclusive. With an
// execution reader wired, evidence ids resolve to their owning task
// first.
func TestReplayAdapter_ResolvesExecutionIDs(t *testing.T) {
	execs := &fakeExecReader{rows: map[string]*persistence.Execution{
		"exec_1": {ID: "exec_1", TaskID: "task_1"},
	}}
	applier := &fakeHealingApplier{
		newID: map[string]string{"task_1": "replay-task_1"},
		traces: map[string]contracts.ExecutionTrace{
			"task_1":        traceWith("task_1", "COMPLETED", 0.1, 10),
			"replay-task_1": traceWith("replay-task_1", "COMPLETED", 0.05, 8),
		},
	}
	a := NewReplayEngineAdapter(applier, &fakeTaskReader{}, execs, fastOpts(), zerolog.Nop())

	tr, err := a.BaselineTrace(context.Background(), "exec_1")
	if err != nil {
		t.Fatalf("BaselineTrace: %v", err)
	}
	if tr == nil || tr.TaskID != "task_1" {
		t.Fatalf("baseline trace = %+v, want the owning task task_1", tr)
	}

	id, ctr, err := a.ReplayEvidence(context.Background(), "exec_1", "wf-candidate-abc")
	if err != nil {
		t.Fatalf("ReplayEvidence: %v", err)
	}
	if id != "replay-task_1" || ctr == nil {
		t.Errorf("replay id = %q trace = %v, want replay-task_1 with a trace", id, ctr)
	}
	if len(applier.gotPlans) != 1 || applier.gotPlans[0].OriginalTaskID != "task_1" {
		t.Errorf("plan OriginalTaskID = %+v, want the owning task task_1", applier.gotPlans)
	}
}

// TestReplayAdapter_PassesThroughTaskIDs: an evidence id with no
// execution row (already a task id, or a pre-resolution trigger) passes
// through unchanged; resolution is best-effort, not a new failure mode.
func TestReplayAdapter_PassesThroughTaskIDs(t *testing.T) {
	execs := &fakeExecReader{} // no rows → ErrNotFound for everything
	applier := &fakeHealingApplier{traces: map[string]contracts.ExecutionTrace{
		"ev1": traceWith("ev1", "COMPLETED", 0.1, 10),
	}}
	a := NewReplayEngineAdapter(applier, &fakeTaskReader{}, execs, fastOpts(), zerolog.Nop())
	tr, err := a.BaselineTrace(context.Background(), "ev1")
	if err != nil {
		t.Fatalf("BaselineTrace: %v", err)
	}
	if tr == nil || tr.TaskID != "ev1" {
		t.Fatalf("baseline trace = %+v, want pass-through ev1", tr)
	}
}

// TestReplayAdapter_StampWarningIsNotFatal: regression for the
// 2026-06-06 cascade — Engine.Apply returns (task, err) when the task
// was created but the counterfactual stamp raced the scheduler (the
// NORMAL case for healing replays: the task is QUEUED, so no execution
// row exists at stamp time). The adapter treated that as fatal,
// skipped the evidence, and the trial's deferred transient-genome
// deregistration then orphaned the already-queued replay task
// ("workflow …-candidate-… not found"). A stamp warning alongside a
// valid task must NOT abort the replay — safety doesn't depend on the
// stamp (the payload marker engages the MCP side-effect gate).
func TestReplayAdapter_StampWarningIsNotFatal(t *testing.T) {
	applier := &fakeHealingApplier{
		newID:    map[string]string{"ev1": "replay-ev1"},
		stampErr: errors.New("counterfactual: task created but stamp failed: no execution row yet"),
		traces: map[string]contracts.ExecutionTrace{
			"replay-ev1": traceWith("replay-ev1", "COMPLETED", 0.05, 8),
		},
	}
	a := NewReplayEngineAdapter(applier, &fakeTaskReader{}, nil, fastOpts(), zerolog.Nop())

	id, tr, err := a.ReplayEvidence(context.Background(), "ev1", "wf-candidate-x")
	if err != nil {
		t.Fatalf("ReplayEvidence: stamp warning must not be fatal, got %v", err)
	}
	if id != "replay-ev1" || tr == nil || tr.TaskID != "replay-ev1" {
		t.Errorf("id=%q tr=%+v, want the settled replay trace despite the stamp warning", id, tr)
	}
}

func TestIsTerminalStatus(t *testing.T) {
	terminal := []persistence.TaskStatus{
		persistence.TaskStatusCompleted, persistence.TaskStatusFailed,
		persistence.TaskStatusCancelled, persistence.TaskStatusClosed,
	}
	for _, s := range terminal {
		if !isTerminalStatus(s) {
			t.Errorf("isTerminalStatus(%q) = false, want true", s)
		}
	}
	for _, s := range []persistence.TaskStatus{persistence.TaskStatusRunning, persistence.TaskStatusQueued, persistence.TaskStatusPending} {
		if isTerminalStatus(s) {
			t.Errorf("isTerminalStatus(%q) = true, want false", s)
		}
	}
}

// ---- item 4: real-regression integration -------------------------------

// TestRunTrial_Replay_RealAdapter_RegressionFailsScorecard wires the REAL
// runner to the REAL adapter (over fakes for the executor/DB seams) and
// asserts that a candidate that regresses success vs baseline produces a
// non-passing scorecard end-to-end. This exercises the aggregator against
// real trace shapes through the production adapter code path — the LLD's
// "real regression produces a failing scorecard" gate.
func TestRunTrial_Replay_RealAdapter_RegressionFailsScorecard(t *testing.T) {
	cands := newFakeCandidateRepo()
	trials := newFakeTrialRepo()
	h, _ := GenomeHashFromMarkdown([]byte(validCandidateMD), "dev-pipeline.md")
	c := seedCandidate(t, cands, persistence.HealingCandidateDraft, validCandidateMD, h)

	// Baseline: both evidence runs COMPLETED. Candidate replay: both
	// FAILED (a clear regression). UPPERCASE statuses are the real
	// blackbox source shape.
	applier := &fakeHealingApplier{
		newID: map[string]string{"ev1": "replay-ev1", "ev2": "replay-ev2"},
		traces: map[string]contracts.ExecutionTrace{
			"ev1":        traceWith("ev1", "COMPLETED", 0.10, 10),
			"ev2":        traceWith("ev2", "COMPLETED", 0.10, 10),
			"replay-ev1": traceWith("replay-ev1", "FAILED", 0.10, 10),
			"replay-ev2": traceWith("replay-ev2", "FAILED", 0.10, 10),
		},
	}
	adapter := NewReplayEngineAdapter(applier, &fakeTaskReader{}, nil, fastOpts(), zerolog.Nop())

	r := NewTrialRunner(cands, trials, adapter, DefaultGateThresholds(), 2, zerolog.Nop()).
		WithRegistrar(newFakeRegistrar())
	res, err := r.RunTrial(context.Background(), c.ID, persistence.HealingTrialModeReplay, []string{"ev1", "ev2"})
	if err != nil {
		t.Fatalf("RunTrial: %v", err)
	}
	if res.Verdict == persistence.HealingTrialPassed {
		t.Fatalf("a regression must NOT pass the gate; got passed (scorecard %+v)", res.Scorecard)
	}
	// Aggregator read the real shapes: baseline 2 successes, candidate 0.
	if res.BaselineSummary.Successes != 2 {
		t.Errorf("baseline successes = %d, want 2", res.BaselineSummary.Successes)
	}
	if res.CandidateSummary.Successes != 0 {
		t.Errorf("candidate successes = %d, want 0 (regressed)", res.CandidateSummary.Successes)
	}
}

// TestRunTrial_ReplayErrorsWithoutRegistrar: a replay trial with a wired
// engine but NO registrar can't route at the candidate genome, so it
// errors cleanly (fail closed) rather than silently replaying baseline.
func TestRunTrial_ReplayErrorsWithoutRegistrar(t *testing.T) {
	cands := newFakeCandidateRepo()
	trials := newFakeTrialRepo()
	h, _ := GenomeHashFromMarkdown([]byte(validCandidateMD), "dev-pipeline.md")
	c := seedCandidate(t, cands, persistence.HealingCandidateDraft, validCandidateMD, h)

	eng := newFakeReplayEngine()
	// NewTrialRunner WITHOUT WithRegistrar.
	r := NewTrialRunner(cands, trials, eng, DefaultGateThresholds(), 2, zerolog.Nop())
	res, err := r.RunTrial(context.Background(), c.ID, persistence.HealingTrialModeReplay, []string{"ev1", "ev2"})
	if err != nil {
		t.Fatalf("RunTrial: %v", err)
	}
	if res.Verdict != persistence.HealingTrialErrored {
		t.Fatalf("verdict = %q, want errored (no registrar)", res.Verdict)
	}
}
