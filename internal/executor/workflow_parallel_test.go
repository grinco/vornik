package executor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/stepoutcome"
)

// --- pure helpers -------------------------------------------------------

func TestBuildParallelBranchSpecs(t *testing.T) {
	step := registry.WorkflowStep{
		Type: "parallel",
		Join: "synth",
		Branches: []registry.WorkflowBranch{
			{ID: "a", Role: "researcher", Prompt: "one"},
			{ID: "b", Role: "analyst", Prompt: "two", Workflow: "research"},
		},
	}
	specs := buildParallelBranchSpecs(step, "fanout", 3)
	if len(specs) != 2 {
		t.Fatalf("specs len = %d, want 2", len(specs))
	}
	if specs[0].Role != "researcher" || specs[0].Prompt != "one" || specs[0].Workflow != "" {
		t.Errorf("spec[0] = %+v", specs[0])
	}
	if specs[1].Workflow != "research" {
		t.Errorf("spec[1] workflow = %q, want research", specs[1].Workflow)
	}
	// Priority left 0 so createDelegatedTasks inherits the parent's priority.
	if specs[0].Priority != 0 {
		t.Errorf("spec priority = %d, want 0 (inherit parent)", specs[0].Priority)
	}
	// Every spec is tagged for attempt-scoped resume detection.
	if specs[0].parallelStepID != "fanout" || specs[0].parentAttempt != 3 {
		t.Errorf("spec[0] tag = {%q,%d}, want {fanout,3}", specs[0].parallelStepID, specs[0].parentAttempt)
	}
}

func TestParallelJoinProceeds(t *testing.T) {
	tests := []struct {
		policy    string
		succeeded int
		total     int
		want      bool
	}{
		{"all", 3, 3, true},
		{"all", 2, 3, false},
		{"", 3, 3, true}, // empty defaults to all
		{"", 2, 3, false},
		{"quorum:2", 2, 3, true},
		{"quorum:2", 3, 3, true},
		{"quorum:2", 1, 3, false},
		{"best_effort", 1, 3, true},
		{"best_effort", 3, 3, true},
		{"best_effort", 0, 3, false},
		{"garbage", 3, 3, true},  // malformed → strict all semantics
		{"garbage", 2, 3, false}, // malformed → strict all semantics
	}
	for _, tt := range tests {
		if got := parallelJoinProceeds(tt.policy, tt.succeeded, tt.total); got != tt.want {
			t.Errorf("parallelJoinProceeds(%q,%d,%d) = %v, want %v", tt.policy, tt.succeeded, tt.total, got, tt.want)
		}
	}
}

func TestJoinPolicyOrDefault(t *testing.T) {
	if got := joinPolicyOrDefault(""); got != "all" {
		t.Errorf("empty → %q, want all", got)
	}
	if got := joinPolicyOrDefault("quorum:2"); got != "quorum:2" {
		t.Errorf("quorum:2 → %q, want quorum:2", got)
	}
}

func TestParallelChildMatches(t *testing.T) {
	tagged, _ := json.Marshal(map[string]any{"parallel_step_id": "fanout", "parent_attempt": 2})
	cases := []struct {
		name  string
		child *persistence.Task
		want  bool
	}{
		{"match", &persistence.Task{Payload: tagged}, true},
		{"nil child", nil, false},
		{"empty payload", &persistence.Task{}, false},
		{"malformed payload", &persistence.Task{Payload: []byte("{not json")}, false},
		{"untagged (LLM delegation)", &persistence.Task{Payload: []byte(`{"context":{"prompt":"x"}}`)}, false},
	}
	for _, c := range cases {
		if got := parallelChildMatches(c.child, "fanout", 2); got != c.want {
			t.Errorf("%s: parallelChildMatches = %v, want %v", c.name, got, c.want)
		}
	}
	// Wrong attempt / wrong step do not match.
	if parallelChildMatches(&persistence.Task{Payload: tagged}, "fanout", 1) {
		t.Error("wrong attempt should not match")
	}
	if parallelChildMatches(&persistence.Task{Payload: tagged}, "other", 2) {
		t.Error("wrong step should not match")
	}
}

func TestCountParallelLegs(t *testing.T) {
	children := []*persistence.Task{
		{Status: persistence.TaskStatusCompleted},
		{Status: persistence.TaskStatusCompleted},
		{Status: persistence.TaskStatusFailed},
		{Status: persistence.TaskStatusCancelled},
		nil,
	}
	succeeded, total := countParallelLegs(children)
	if succeeded != 2 || total != 4 {
		t.Errorf("countParallelLegs = (%d,%d), want (2,4)", succeeded, total)
	}
}

// --- first pass: fan out -----------------------------------------------

func TestHandleParallelStep_FirstPassFansOut(t *testing.T) {
	e, _, er, _, tr := setup()
	parent := &persistence.Task{ID: "par", ProjectID: "p1", Priority: 40, Attempt: 1, Status: persistence.TaskStatusRunning}
	tr.AddTask(parent)
	exec := &persistence.Execution{ID: "ex", TaskID: "par", ProjectID: "p1", Status: persistence.ExecutionStatusRunning}
	_ = er.Create(context.Background(), exec)

	step := registry.WorkflowStep{
		Type:       "parallel",
		Join:       "synthesize",
		JoinPolicy: "quorum:2",
		Branches: []registry.WorkflowBranch{
			{ID: "market", Role: "researcher", Prompt: "m"},
			{ID: "tech", Role: "researcher", Prompt: "t"},
			{ID: "legal", Role: "researcher", Prompt: "l"},
		},
	}
	var state executionState
	completed, next, paused, err := e.handleParallelStep(context.Background(), parent, exec, "fanout", step, nil, &state)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !paused {
		t.Fatalf("expected paused=true on first-pass fan-out")
	}
	if next != "" {
		t.Errorf("nextStep = %q, want empty (paused)", next)
	}
	// 3 PARALLEL children created, no dependency edges (concurrent).
	children, _ := tr.GetChildren(context.Background(), "par")
	if len(children) != 3 {
		t.Fatalf("children = %d, want 3", len(children))
	}
	for _, c := range children {
		if c.DelegationMode == nil || *c.DelegationMode != persistence.DelegationModeParallel {
			t.Errorf("child %s mode = %v, want PARALLEL", c.ID, c.DelegationMode)
		}
		if len(c.Dependencies) != 0 {
			t.Errorf("child %s has dependencies %v, want none (PARALLEL)", c.ID, c.Dependencies)
		}
		// Each leg is tagged for THIS step at THIS parent attempt.
		if !parallelChildMatches(c, "fanout", 1) {
			t.Errorf("child %s payload not tagged {parallel_step_id:fanout, parent_attempt:1}: %s", c.ID, string(c.Payload))
		}
	}
	// Parent parked WAITING_FOR_CHILDREN, execution paused.
	if got, _ := tr.Get(context.Background(), "par"); got.Status != persistence.TaskStatusWaitingForChildren {
		t.Errorf("parent status = %s, want WAITING_FOR_CHILDREN", got.Status)
	}
	// Descriptor written: resume step == join, policy carried, N children.
	if state.ParallelJoin == nil {
		t.Fatalf("state.ParallelJoin not populated")
	}
	if state.ParallelJoin.JoinStepID != "synthesize" {
		t.Errorf("JoinStepID = %q, want synthesize", state.ParallelJoin.JoinStepID)
	}
	if state.ParallelJoin.Policy != "quorum:2" {
		t.Errorf("Policy = %q, want quorum:2", state.ParallelJoin.Policy)
	}
	if len(state.ParallelJoin.ChildTaskIDs) != 3 {
		t.Errorf("ChildTaskIDs = %d, want 3", len(state.ParallelJoin.ChildTaskIDs))
	}
	if len(state.ParallelJoin.BranchIDs) != 3 || state.ParallelJoin.BranchIDs[0] != "market" {
		t.Errorf("BranchIDs = %v", state.ParallelJoin.BranchIDs)
	}
	// completedSteps carries the parallel step; persisted resume step = join.
	if len(completed) != 1 || completed[0] != "fanout" {
		t.Errorf("completedSteps = %v, want [fanout]", completed)
	}
	persisted, _ := er.Get(context.Background(), "ex")
	if persisted.CurrentStepID == nil || *persisted.CurrentStepID != "synthesize" {
		t.Errorf("persisted resume step = %v, want synthesize", persisted.CurrentStepID)
	}
}

// TestHandleParallelStep_N4FanOutGuard — an over-limit fan-out is rejected by
// the reused N4 guard with NO partial children created.
func TestHandleParallelStep_N4FanOutGuard(t *testing.T) {
	e, _, er, _, tr := setup()
	parent := &persistence.Task{ID: "par", ProjectID: "p1", Attempt: 1, Status: persistence.TaskStatusRunning}
	tr.AddTask(parent)
	exec := &persistence.Execution{ID: "ex", TaskID: "par", ProjectID: "p1"}
	_ = er.Create(context.Background(), exec)

	limit := e.delegationFanOutLimit()
	branches := make([]registry.WorkflowBranch, limit+1)
	for i := range branches {
		branches[i] = registry.WorkflowBranch{ID: string(rune('a'+i%26)) + string(rune('0'+i/26)), Role: "r", Prompt: "p"}
	}
	step := registry.WorkflowStep{Type: "parallel", Join: "j", Branches: branches}

	var state executionState
	_, _, paused, err := e.handleParallelStep(context.Background(), parent, exec, "fanout", step, nil, &state)
	if err == nil {
		t.Fatalf("expected a fan-out guard error")
	}
	var ge *delegationGuardError
	if !errors.As(err, &ge) || ge.reason != "fanout" {
		t.Fatalf("expected *delegationGuardError{fanout}, got %v", err)
	}
	if paused {
		t.Errorf("must not pause on guard rejection")
	}
	children, _ := tr.GetChildren(context.Background(), "par")
	if len(children) != 0 {
		t.Errorf("children = %d, want 0 (no partial batch on guard rejection)", len(children))
	}
	if state.ParallelJoin != nil {
		t.Errorf("ParallelJoin must not be written on guard rejection")
	}
}

// TestHandleParallelStep_ResumeAdvancesAndEmits — on resume (children exist),
// the parent skips re-fan-out, emits the parallel_join observability outcome,
// clears the descriptor, and advances to the join step.
func TestHandleParallelStep_ResumeAdvancesAndEmits(t *testing.T) {
	e, _, er, _, tr := setup()
	stub := newStubStepOutcomeRepo()
	e.outcomeRepo = stub

	parent := &persistence.Task{ID: "par", ProjectID: "p1", Attempt: 1, Status: persistence.TaskStatusRunning}
	tr.AddTask(parent)
	exec := &persistence.Execution{ID: "ex", TaskID: "par", ProjectID: "p1", Status: persistence.ExecutionStatusRunning}
	_ = er.Create(context.Background(), exec)
	// This attempt's (attempt 1) legs: 2 succeeded, 1 failed → quorum:2 met.
	addParallelChildren(tr, []persistence.TaskStatus{
		persistence.TaskStatusCompleted, persistence.TaskStatusCompleted, persistence.TaskStatusFailed,
	})

	step := registry.WorkflowStep{Type: "parallel", Join: "synthesize", JoinPolicy: "quorum:2",
		Branches: []registry.WorkflowBranch{{ID: "a", Role: "r", Prompt: "p"}}}
	// Fresh-execution resume: state is empty (ParallelJoin nil), resume is
	// derived from the attempt-tagged children, not from state.
	state := executionState{}

	completed, next, paused, err := e.handleParallelStep(context.Background(), parent, exec, "fanout", step, nil, &state)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if paused {
		t.Fatalf("resume must not pause")
	}
	if next != "synthesize" {
		t.Errorf("nextStep = %q, want synthesize", next)
	}
	if state.ParallelJoin != nil {
		t.Errorf("descriptor must be cleared on resume")
	}
	if len(completed) != 1 || completed[0] != "fanout" {
		t.Errorf("completedSteps = %v, want [fanout]", completed)
	}
	// Exactly one parallel_join outcome row on the parallel step.
	rows := parallelJoinRows(stub)
	if len(rows) != 1 {
		t.Fatalf("parallel_join rows = %d, want 1", len(rows))
	}
	if rows[0].StepID != "fanout" {
		t.Errorf("outcome step_id = %q, want fanout (the parallel step)", rows[0].StepID)
	}
	var detail struct {
		Policy    string `json:"policy"`
		Succeeded int    `json:"succeeded"`
		Total     int    `json:"total"`
	}
	_ = json.Unmarshal([]byte(rows[0].ErrorDetail), &detail)
	if detail.Policy != "quorum:2" || detail.Succeeded != 2 || detail.Total != 3 {
		t.Errorf("detail = %+v, want {quorum:2 2 3}", detail)
	}
}

// TestEmitParallelJoinOutcome_IdempotentAcrossExecutions is the real crash /
// reattach scenario (§7 F2 / I-4): the first proceed-true resume emits under
// execution ex1; a crash before advancing re-queues the parent to a FRESH
// execution ex2 (different execution_id, SAME task_id + step_id) which re-emits
// → still exactly ONE parallel_join row, because dedup is keyed on
// (task_id, step_id), not the ever-changing execution_id.
func TestEmitParallelJoinOutcome_IdempotentAcrossExecutions(t *testing.T) {
	e, _, _, _, tr := setup()
	stub := newStubStepOutcomeRepo()
	e.outcomeRepo = stub
	task := &persistence.Task{ID: "par", ProjectID: "p1", Attempt: 1}
	tr.AddTask(task)
	addParallelChildren(tr, []persistence.TaskStatus{
		persistence.TaskStatusCompleted, persistence.TaskStatusCompleted,
	})
	step := registry.WorkflowStep{Type: "parallel", Join: "j", JoinPolicy: "all"}

	exec1 := &persistence.Execution{ID: "ex1", TaskID: "par", ProjectID: "p1"}
	exec2 := &persistence.Execution{ID: "ex2", TaskID: "par", ProjectID: "p1"}
	e.emitParallelJoinOutcome(context.Background(), task, exec1, "fanout", step) // first resume
	e.emitParallelJoinOutcome(context.Background(), task, exec2, "fanout", step) // reattach under a NEW execution id

	rows, _ := stub.List(context.Background(), persistence.ExecutionStepOutcomeFilter{})
	var pjRows []*persistence.ExecutionStepOutcome
	for _, r := range rows {
		if r.TaskID == "par" && r.StepID == "fanout" && r.Outcome == string(stepoutcome.ParallelJoin) {
			pjRows = append(pjRows, r)
		}
	}
	if len(pjRows) != 1 {
		t.Fatalf("parallel_join rows for task+step = %d, want exactly 1 across executions (I-4)", len(pjRows))
	}
}

// TestHandleParallelStep_ProceedFalseRetryReFansOut is the C1 regression: a
// proceed-false attempt bumps parent.Attempt; on the fresh-execution re-entry
// the parallel step must NOT mistake the PRIOR attempt's terminal (sub-quorum)
// legs for a resume — it must genuinely re-fan-out a fresh cohort tagged with
// the new attempt, advance to NO join, and emit NO parallel_join for the failed
// attempt.
func TestHandleParallelStep_ProceedFalseRetryReFansOut(t *testing.T) {
	e, _, er, _, tr := setup()
	stub := newStubStepOutcomeRepo()
	e.outcomeRepo = stub
	// The wake path already bubbled up proceed-false and re-queued Attempt 2.
	parent := &persistence.Task{ID: "par", ProjectID: "p1", Attempt: 2, Status: persistence.TaskStatusRunning}
	tr.AddTask(parent)
	exec := &persistence.Execution{ID: "ex2", TaskID: "par", ProjectID: "p1", Status: persistence.ExecutionStatusRunning}
	_ = er.Create(context.Background(), exec)
	// Attempt 1's terminal legs: 1/3 succeeded → below quorum:2 (why we retried).
	addParallelChildren(tr, []persistence.TaskStatus{
		persistence.TaskStatusCompleted, persistence.TaskStatusFailed, persistence.TaskStatusFailed,
	})

	step := registry.WorkflowStep{Type: "parallel", Join: "synthesize", JoinPolicy: "quorum:2",
		Branches: []registry.WorkflowBranch{
			{ID: "a", Role: "r", Prompt: "p"},
			{ID: "b", Role: "r", Prompt: "p"},
			{ID: "c", Role: "r", Prompt: "p"},
		}}
	var state executionState
	_, next, paused, err := e.handleParallelStep(context.Background(), parent, exec, "fanout", step, nil, &state)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !paused {
		t.Fatalf("expected re-fan-out (paused=true), not a resume — the retry must NOT advance on stale legs")
	}
	if next != "" {
		t.Errorf("nextStep = %q, want empty (paused, re-fanned-out), not the join", next)
	}
	// A fresh cohort tagged attempt 2 was created; the attempt-1 legs remain.
	all, _ := tr.GetChildren(context.Background(), "par")
	att2 := filterParallelChildren(all, "fanout", 2)
	if len(att2) != 3 {
		t.Errorf("attempt-2 legs = %d, want 3 (genuine re-fan-out)", len(att2))
	}
	// Descriptor names only the new attempt's legs.
	if state.ParallelJoin == nil || len(state.ParallelJoin.ChildTaskIDs) != 3 {
		t.Errorf("descriptor ChildTaskIDs = %v, want 3 attempt-2 legs", state.ParallelJoin)
	}
	// No parallel_join outcome emitted for the failed attempt.
	if rows := parallelJoinRows(stub); len(rows) != 0 {
		t.Errorf("proceed-false retry must not emit parallel_join, got %d rows", len(rows))
	}
}

// TestEmitParallelJoinOutcome_GetChildrenError — a GetChildren error at resume
// must NOT record a fabricated outcome and must NOT panic (the caller still
// advances to join; §6 F4).
func TestEmitParallelJoinOutcome_GetChildrenError(t *testing.T) {
	stub := newStubStepOutcomeRepo()
	tr := &mocks.MockTaskRepository{
		GetChildrenFunc: func(context.Context, string) ([]*persistence.Task, error) {
			return nil, errors.New("db down")
		},
	}
	e := &Executor{taskRepo: tr, outcomeRepo: stub, logger: zerolog.Nop()}
	task := &persistence.Task{ID: "par", ProjectID: "p1"}
	exec := &persistence.Execution{ID: "ex", TaskID: "par", ProjectID: "p1"}
	step := registry.WorkflowStep{Type: "parallel", Join: "j"}

	e.emitParallelJoinOutcome(context.Background(), task, exec, "fanout", step)
	if rows := parallelJoinRows(stub); len(rows) != 0 {
		t.Fatalf("parallel_join rows = %d, want 0 (skip on GetChildren error)", len(rows))
	}
}

// TestEmitParallelJoinOutcome_ListErrorSkips — when the dedup List errors we
// cannot prove a prior row is absent, and a reattach (new execution_id) would
// not be caught by execution-scoped idempotency, so we SKIP the emit rather
// than risk a duplicate (NEW-3).
func TestEmitParallelJoinOutcome_ListErrorSkips(t *testing.T) {
	e, _, _, _, tr := setup()
	stub := newStubStepOutcomeRepo()
	stub.listErr = errors.New("db down")
	e.outcomeRepo = stub
	task := &persistence.Task{ID: "par", ProjectID: "p1", Attempt: 1}
	tr.AddTask(task)
	addParallelChildren(tr, []persistence.TaskStatus{persistence.TaskStatusCompleted})
	exec := &persistence.Execution{ID: "ex", TaskID: "par", ProjectID: "p1"}
	e.emitParallelJoinOutcome(context.Background(), task, exec, "fanout", registry.WorkflowStep{Type: "parallel", Join: "j"})

	stub.mu.Lock()
	n := len(stub.rows)
	stub.mu.Unlock()
	if n != 0 {
		t.Fatalf("recorded %d rows, want 0 (skip on dedup List error)", n)
	}
}

// TestFilterChildrenByIDs covers the cohort-scoping filter incl. nil entries
// and non-matching ids.
func TestFilterChildrenByIDs(t *testing.T) {
	children := []*persistence.Task{
		{ID: "a"}, nil, {ID: "b"}, {ID: "c"},
	}
	got := filterChildrenByIDs(children, []string{"a", "c", "missing"})
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "c" {
		t.Fatalf("filterChildrenByIDs = %+v, want [a c]", got)
	}
	if len(filterChildrenByIDs(children, nil)) != 0 {
		t.Errorf("empty ids should yield empty cohort")
	}
}

// TestEmitParallelJoinOutcome_NoRepoIsNoOp guards the nil outcome repo path.
func TestEmitParallelJoinOutcome_NoRepoIsNoOp(_ *testing.T) {
	e := &Executor{logger: zerolog.Nop()}
	// Must not panic with a nil outcomeRepo / nil taskRepo.
	e.emitParallelJoinOutcome(context.Background(), &persistence.Task{ID: "x"},
		&persistence.Execution{ID: "ex"}, "fanout", registry.WorkflowStep{})
}

// TestHandleParallelStep_GetChildrenErrorOnEntry — a GetChildren failure on
// entry is a genuine DB error and is surfaced (never risk a double fan-out).
func TestHandleParallelStep_GetChildrenErrorOnEntry(t *testing.T) {
	tr := &mocks.MockTaskRepository{
		GetChildrenFunc: func(context.Context, string) ([]*persistence.Task, error) {
			return nil, errors.New("db down")
		},
	}
	e := &Executor{taskRepo: tr, logger: zerolog.Nop()}
	step := registry.WorkflowStep{Type: "parallel", Join: "j",
		Branches: []registry.WorkflowBranch{{ID: "a", Role: "r", Prompt: "p"}}}
	var state executionState
	_, _, paused, err := e.handleParallelStep(context.Background(),
		&persistence.Task{ID: "par"}, &persistence.Execution{ID: "ex"}, "fanout", step, nil, &state)
	if err == nil || !strings.Contains(err.Error(), "failed to list children") {
		t.Fatalf("expected list-children error, got %v", err)
	}
	if paused {
		t.Errorf("must not pause on entry error")
	}
}

// --- wake-path join policy matrix (unblockParentIfChildrenDone) ---------

// parallelWakeExecutor wires an Executor + a parent task + its paused
// execution carrying a ParallelJoin descriptor, plus N children of the given
// statuses. Returns the executor, the task repo (for status assertions), the
// transition-call recorder, and the outcome stub.
func parallelWakeExecutor(t *testing.T, policy string, parentAttempt int, childStatuses []persistence.TaskStatus) (*Executor, *MockTaskRepo, *stubStepOutcomeRepo) {
	t.Helper()
	tr := NewMockTaskRepo()
	er := NewMockExecRepo()
	stub := newStubStepOutcomeRepo()
	parent := &persistence.Task{ID: "par", ProjectID: "p1", Status: persistence.TaskStatusWaitingForChildren, Attempt: parentAttempt, MaxAttempts: 3}
	tr.AddTask(parent)
	cohortIDs := make([]string, 0, len(childStatuses))
	for i, st := range childStatuses {
		pid := parent.ID
		mode := persistence.DelegationModeParallel
		id := "child-" + string(rune('0'+i))
		tr.AddTask(&persistence.Task{ID: id, ParentTaskID: &pid, DelegationMode: &mode, Status: st})
		cohortIDs = append(cohortIDs, id)
	}
	// The descriptor names the current cohort so the wake-path count is
	// cohort-scoped (NEW-1).
	snapshot, _ := json.Marshal(executionState{ParallelJoin: &ParallelJoinState{
		JoinStepID: "synthesize", Policy: policy, ChildTaskIDs: cohortIDs,
	}})
	_ = er.Create(context.Background(), &persistence.Execution{ID: "ex", TaskID: "par", ProjectID: "p1", Status: persistence.ExecutionStatusPaused, StateSnapshot: snapshot})
	e := &Executor{taskRepo: tr, execRepo: er, outcomeRepo: stub, logger: zerolog.Nop()}
	return e, tr, stub
}

func TestUnblockParent_ParallelQuorumProceed(t *testing.T) {
	e, tr, stub := parallelWakeExecutor(t, "quorum:2", 1, []persistence.TaskStatus{
		persistence.TaskStatusCompleted, persistence.TaskStatusCompleted, persistence.TaskStatusFailed,
	})
	e.unblockParentIfChildrenDone(context.Background(), "par")

	got, _ := tr.Get(context.Background(), "par")
	if got.Status != persistence.TaskStatusQueued {
		t.Errorf("parent status = %s, want QUEUED (quorum satisfied)", got.Status)
	}
	if got.Attempt != 1 {
		t.Errorf("Attempt = %d, want 1 (no bump on proceed)", got.Attempt)
	}
	if got.LastError != nil {
		t.Errorf("LastError = %v, want nil on proceed", got.LastError)
	}
	// Non-emission: the wake path never records a parallel_join outcome
	// (that happens only at resume).
	if rows := parallelJoinRows(stub); len(rows) != 0 {
		t.Errorf("wake path must not emit parallel_join rows, got %d", len(rows))
	}
}

func TestUnblockParent_ParallelQuorumBubbleUp_RetryBudget(t *testing.T) {
	e, tr, stub := parallelWakeExecutor(t, "quorum:2", 1, []persistence.TaskStatus{
		persistence.TaskStatusCompleted, persistence.TaskStatusFailed, persistence.TaskStatusFailed,
	})
	e.unblockParentIfChildrenDone(context.Background(), "par")

	got, _ := tr.Get(context.Background(), "par")
	if got.Status != persistence.TaskStatusQueued || got.Attempt != 2 {
		t.Errorf("parent = %s attempt %d, want QUEUED attempt 2 (retry budget)", got.Status, got.Attempt)
	}
	if got.LastError == nil || !strings.Contains(*got.LastError, "quorum:2 not satisfied: 1/3") {
		t.Errorf("LastError = %v, want policy+ratio message", got.LastError)
	}
	if got.LastErrorClass == nil || *got.LastErrorClass != persistence.TaskFailureClassChildFailed {
		t.Errorf("LastErrorClass = %v, want CHILD_FAILED", got.LastErrorClass)
	}
	// proceed-false: no parallel_join outcome (observed via last_error).
	if rows := parallelJoinRows(stub); len(rows) != 0 {
		t.Errorf("proceed-false must not emit parallel_join, got %d rows", len(rows))
	}
}

func TestUnblockParent_ParallelQuorumBubbleUp_Terminal(t *testing.T) {
	e, tr, _ := parallelWakeExecutor(t, "quorum:2", 3, []persistence.TaskStatus{
		persistence.TaskStatusCompleted, persistence.TaskStatusFailed, persistence.TaskStatusFailed,
	})
	e.unblockParentIfChildrenDone(context.Background(), "par")

	got, _ := tr.Get(context.Background(), "par")
	if got.Status != persistence.TaskStatusFailed {
		t.Errorf("parent status = %s, want FAILED (budget exhausted)", got.Status)
	}
	if got.LastErrorClass == nil || *got.LastErrorClass != persistence.TaskFailureClassChildFailed {
		t.Errorf("LastErrorClass = %v, want CHILD_FAILED", got.LastErrorClass)
	}
}

func TestUnblockParent_ParallelBestEffort(t *testing.T) {
	t.Run("zero succeeded → bubble-up", func(t *testing.T) {
		e, tr, _ := parallelWakeExecutor(t, "best_effort", 1, []persistence.TaskStatus{
			persistence.TaskStatusFailed, persistence.TaskStatusFailed,
		})
		e.unblockParentIfChildrenDone(context.Background(), "par")
		got, _ := tr.Get(context.Background(), "par")
		if got.Status != persistence.TaskStatusQueued || got.Attempt != 2 {
			t.Errorf("parent = %s attempt %d, want QUEUED attempt 2 (retry)", got.Status, got.Attempt)
		}
		if got.LastError == nil || !strings.Contains(*got.LastError, "best_effort not satisfied: 0/2") {
			t.Errorf("LastError = %v", got.LastError)
		}
	})
	t.Run("one succeeded → proceed", func(t *testing.T) {
		e, tr, _ := parallelWakeExecutor(t, "best_effort", 1, []persistence.TaskStatus{
			persistence.TaskStatusFailed, persistence.TaskStatusCompleted,
		})
		e.unblockParentIfChildrenDone(context.Background(), "par")
		got, _ := tr.Get(context.Background(), "par")
		if got.Status != persistence.TaskStatusQueued || got.Attempt != 1 {
			t.Errorf("parent = %s attempt %d, want QUEUED attempt 1 (proceed, no bump)", got.Status, got.Attempt)
		}
		if got.LastError != nil {
			t.Errorf("LastError = %v, want nil on proceed", got.LastError)
		}
	})
}

// TestUnblockParent_ParallelAll_OneFail_IdenticalToLegacy — the `all` policy
// bubbles up on any failure, behaviourally identical to a legacy delegation
// join (both wait-for-all-then-fail-if-any).
func TestUnblockParent_ParallelAll_OneFail_IdenticalToLegacy(t *testing.T) {
	e, tr, _ := parallelWakeExecutor(t, "all", 1, []persistence.TaskStatus{
		persistence.TaskStatusCompleted, persistence.TaskStatusCompleted, persistence.TaskStatusFailed,
	})
	e.unblockParentIfChildrenDone(context.Background(), "par")
	got, _ := tr.Get(context.Background(), "par")
	if got.Status != persistence.TaskStatusQueued || got.Attempt != 2 {
		t.Errorf("parent = %s attempt %d, want QUEUED attempt 2 (retry, like legacy)", got.Status, got.Attempt)
	}
	if got.LastErrorClass == nil || *got.LastErrorClass != persistence.TaskFailureClassChildFailed {
		t.Errorf("LastErrorClass = %v, want CHILD_FAILED", got.LastErrorClass)
	}
}

// TestUnblockParent_ParallelStillRunningBlocksQuorum — a still-running 3rd leg
// blocks a quorum:2 resume even though 2/3 already succeeded (no early
// short-circuit; evaluate only after ALL terminal).
func TestUnblockParent_ParallelStillRunningBlocksQuorum(t *testing.T) {
	e, tr, _ := parallelWakeExecutor(t, "quorum:2", 1, []persistence.TaskStatus{
		persistence.TaskStatusCompleted, persistence.TaskStatusCompleted, persistence.TaskStatusRunning,
	})
	e.unblockParentIfChildrenDone(context.Background(), "par")
	got, _ := tr.Get(context.Background(), "par")
	if got.Status != persistence.TaskStatusWaitingForChildren {
		t.Errorf("parent status = %s, want WAITING_FOR_CHILDREN (3rd leg still running)", got.Status)
	}
}

// wakeExecutorWithCohort seeds a parent with BOTH a stale prior-attempt cohort
// (not named in the descriptor) and a current cohort (named in the descriptor's
// ChildTaskIDs), so a test can prove the wake-path count is cohort-scoped
// (NEW-1). Returns the executor and task repo.
func wakeExecutorWithCohort(t *testing.T, policy string, parentAttempt int, stale, current []persistence.TaskStatus) (*Executor, *MockTaskRepo) {
	t.Helper()
	tr := NewMockTaskRepo()
	er := NewMockExecRepo()
	parent := &persistence.Task{ID: "par", ProjectID: "p1", Status: persistence.TaskStatusWaitingForChildren, Attempt: parentAttempt, MaxAttempts: 3}
	tr.AddTask(parent)
	pid := parent.ID
	mode := persistence.DelegationModeParallel
	for i, st := range stale {
		tr.AddTask(&persistence.Task{ID: "stale-" + string(rune('0'+i)), ParentTaskID: &pid, DelegationMode: &mode, Status: st})
	}
	cohortIDs := make([]string, 0, len(current))
	for i, st := range current {
		id := "cur-" + string(rune('0'+i))
		tr.AddTask(&persistence.Task{ID: id, ParentTaskID: &pid, DelegationMode: &mode, Status: st})
		cohortIDs = append(cohortIDs, id)
	}
	snapshot, _ := json.Marshal(executionState{ParallelJoin: &ParallelJoinState{
		JoinStepID: "synthesize", Policy: policy, ChildTaskIDs: cohortIDs,
	}})
	_ = er.Create(context.Background(), &persistence.Execution{ID: "ex", TaskID: "par", ProjectID: "p1", Status: persistence.ExecutionStatusPaused, StateSnapshot: snapshot})
	e := &Executor{taskRepo: tr, execRepo: er, outcomeRepo: newStubStepOutcomeRepo(), logger: zerolog.Nop()}
	return e, tr
}

// TestUnblockParent_ParallelCohortScoped_AllRetrySucceeds is the NEW-1
// regression: a proceed-false attempt-1 left 3 FAILED legs in the DB; attempt-2
// re-fans-out 3 FRESH legs that ALL succeed under policy `all`. The wake count
// must see 3/3 (current cohort ONLY) and PROCEED — NOT count the stale FAILED
// legs (which would make 3/6 and doom every retry to terminal FAILED).
func TestUnblockParent_ParallelCohortScoped_AllRetrySucceeds(t *testing.T) {
	e, tr := wakeExecutorWithCohort(t, "all", 2,
		[]persistence.TaskStatus{persistence.TaskStatusFailed, persistence.TaskStatusFailed, persistence.TaskStatusFailed},
		[]persistence.TaskStatus{persistence.TaskStatusCompleted, persistence.TaskStatusCompleted, persistence.TaskStatusCompleted},
	)
	e.unblockParentIfChildrenDone(context.Background(), "par")
	got, _ := tr.Get(context.Background(), "par")
	if got.Status != persistence.TaskStatusQueued {
		t.Fatalf("parent status = %s, want QUEUED (current cohort 3/3 under `all` → proceed)", got.Status)
	}
	if got.Attempt != 2 {
		t.Errorf("Attempt = %d, want 2 (proceed does not bump); stale legs must not doom the retry", got.Attempt)
	}
	if got.LastError != nil {
		t.Errorf("LastError = %v, want nil on proceed (not a bubble-up)", got.LastError)
	}
}

// TestUnblockParent_ParallelCohortScoped_BestEffortStaleSuccessIgnored is the
// other half of NEW-1: attempt-1 left 1 stale SUCCESS; attempt-2's fresh cohort
// has 0 successes. best_effort must NOT proceed on the stale success — it counts
// only the current cohort (0/3) → bubble-up.
func TestUnblockParent_ParallelCohortScoped_BestEffortStaleSuccessIgnored(t *testing.T) {
	e, tr := wakeExecutorWithCohort(t, "best_effort", 2,
		[]persistence.TaskStatus{persistence.TaskStatusCompleted},
		[]persistence.TaskStatus{persistence.TaskStatusFailed, persistence.TaskStatusFailed, persistence.TaskStatusFailed},
	)
	e.unblockParentIfChildrenDone(context.Background(), "par")
	got, _ := tr.Get(context.Background(), "par")
	// Attempt 2 == MaxAttempts 3? No — MaxAttempts is 3, Attempt 2 < 3, so retry
	// budget remains → re-queued Attempt 3 with the policy last_error. The key
	// assertion is that it did NOT proceed cleanly (LastError is set).
	if got.LastError == nil || !strings.Contains(*got.LastError, "best_effort not satisfied: 0/3") {
		t.Fatalf("LastError = %v, want best_effort 0/3 bubble-up (stale success must be ignored)", got.LastError)
	}
	if got.Attempt != 3 {
		t.Errorf("Attempt = %d, want 3 (bubble-up retry), i.e. did NOT proceed on the stale success", got.Attempt)
	}
}

// TestUnblockParent_LegacyNoDescriptorByteIdentical — with an execution
// present but NO ParallelJoin descriptor, the legacy anyFailed bubble-up runs
// exactly as before (the guard keys on the descriptor).
func TestUnblockParent_LegacyNoDescriptorByteIdentical(t *testing.T) {
	tr := NewMockTaskRepo()
	er := NewMockExecRepo()
	parent := &persistence.Task{ID: "par", ProjectID: "p1", Status: persistence.TaskStatusWaitingForChildren, Attempt: 1, MaxAttempts: 3}
	tr.AddTask(parent)
	pid := parent.ID
	mode := persistence.DelegationModeParallel
	tr.AddTask(&persistence.Task{ID: "c0", ParentTaskID: &pid, DelegationMode: &mode, Status: persistence.TaskStatusFailed})
	tr.AddTask(&persistence.Task{ID: "c1", ParentTaskID: &pid, DelegationMode: &mode, Status: persistence.TaskStatusCompleted})
	// Execution with EMPTY state (no ParallelJoin).
	_ = er.Create(context.Background(), &persistence.Execution{ID: "ex", TaskID: "par", ProjectID: "p1", Status: persistence.ExecutionStatusPaused})
	e := &Executor{taskRepo: tr, execRepo: er, logger: zerolog.Nop()}

	e.unblockParentIfChildrenDone(context.Background(), "par")
	got, _ := tr.Get(context.Background(), "par")
	if got.Status != persistence.TaskStatusQueued || got.Attempt != 2 {
		t.Errorf("parent = %s attempt %d, want QUEUED attempt 2 (legacy anyFailed retry)", got.Status, got.Attempt)
	}
	if got.LastError == nil || !strings.Contains(*got.LastError, "child task(s) failed") {
		t.Errorf("LastError = %v, want the legacy default message", got.LastError)
	}
}

// TestUnblockParent_ParallelProceed_BackCompatSuccess — an all-succeeded
// parallel join proceeds identically to a legacy all-success delegation join.
func TestUnblockParent_ParallelProceed_BackCompatSuccess(t *testing.T) {
	e, tr, _ := parallelWakeExecutor(t, "all", 1, []persistence.TaskStatus{
		persistence.TaskStatusCompleted, persistence.TaskStatusCompleted,
	})
	e.unblockParentIfChildrenDone(context.Background(), "par")
	got, _ := tr.Get(context.Background(), "par")
	if got.Status != persistence.TaskStatusQueued || got.Attempt != 1 {
		t.Errorf("parent = %s attempt %d, want QUEUED attempt 1", got.Status, got.Attempt)
	}
}

// --- fan-in: only COMPLETED legs staged, failed leg in Missing ----------

// TestParallelFanIn_FailedLegInMissing proves the join step's staging (reusing
// gatherChildArtifacts) surfaces a failed leg in Missing rather than silently
// dropping it (§7).
func TestParallelFanIn_FailedLegInMissing(t *testing.T) {
	now := time.Now()
	er := &gatherFakeExecRepo{execs: []*persistence.Execution{
		completedExec("legA-exec", "legA", now),
	}}
	ar := &gatherFakeArtifactRepo{artifacts: []*persistence.Artifact{
		{ID: "art1", Name: "brief.md", ExecutionID: strPtr("legA-exec"), StoragePath: "/store/brief.md"},
	}}
	e := newGatherExecutor(er, ar)

	children := []*persistence.Task{
		{ID: "legA", DelegationMode: delegationMode(persistence.DelegationModeParallel), Status: persistence.TaskStatusCompleted, CreatedAt: now},
		{ID: "legB", DelegationMode: delegationMode(persistence.DelegationModeParallel), Status: persistence.TaskStatusFailed, CreatedAt: now.Add(time.Second)},
	}
	entries, summary := e.gatherChildArtifacts(context.Background(), children, "")
	if summary.Expected != 2 {
		t.Errorf("Expected = %d, want 2", summary.Expected)
	}
	if summary.Staged != 1 || len(entries) != 1 {
		t.Errorf("Staged = %d entries %d, want 1 (only the COMPLETED leg)", summary.Staged, len(entries))
	}
	if len(summary.Missing) != 1 || summary.Missing[0] != "legB" {
		t.Errorf("Missing = %v, want [legB] (the failed leg, not silently dropped)", summary.Missing)
	}
}

// --- shared test helpers ------------------------------------------------

// addParallelChildren seeds N children of parent "par" tagged as legs of the
// "fanout" parallel step at parent attempt 1 (mirroring the payload tag
// createDelegatedTasks writes), so attempt-scoped resume detection sees them.
// Fixed step id / attempt keep the helper simple — every caller uses these; a
// retry test seeds attempt-1 stale legs here and lets handleParallelStep create
// the attempt-2 cohort itself.
func addParallelChildren(tr *MockTaskRepo, statuses []persistence.TaskStatus) {
	const (
		parentID = "par"
		stepID   = "fanout"
		attempt  = 1
	)
	for i, st := range statuses {
		pid := parentID
		mode := persistence.DelegationModeParallel
		payload, _ := json.Marshal(map[string]any{
			"context":          map[string]any{"prompt": "leg"},
			"parallel_step_id": stepID,
			"parent_attempt":   attempt,
		})
		tr.AddTask(&persistence.Task{
			ID:             parentID + "-a" + string(rune('0'+attempt)) + "c" + string(rune('0'+i)),
			ParentTaskID:   &pid,
			DelegationMode: &mode,
			Status:         st,
			Payload:        payload,
		})
	}
}

func parallelJoinRows(s *stubStepOutcomeRepo) []*persistence.ExecutionStepOutcome {
	rows, _ := s.List(context.Background(), persistence.ExecutionStepOutcomeFilter{})
	var out []*persistence.ExecutionStepOutcome
	for _, r := range rows {
		if r.Outcome == string(stepoutcome.ParallelJoin) {
			out = append(out, r)
		}
	}
	return out
}

func strPtr(s string) *string { return &s }
