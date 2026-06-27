package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// callProjectCov_lineageFailRepo is a taskLineageGetter that
// returns a non-NotFound error so handleCallProjectStep takes the
// LINEAGE_WALK_FAILED defensive branch. It embeds MockTaskRepo so
// the executor can still use it as the task repo for the walk.
type callProjectCov_lineageFailRepo struct {
	*MockTaskRepo
	err error
}

func (r *callProjectCov_lineageFailRepo) Get(_ context.Context, _ string) (*persistence.Task, error) {
	return nil, r.err
}

// TestCallProjectCov_LineageWalkFailed drives the defensive
// refusal path: the lineage walk errors with a non-NotFound error
// (a real DB fault on a parent lookup), so the handler refuses the
// call rather than risk an unbounded chain.
func TestCallProjectCov_LineageWalkFailed(t *testing.T) {
	withInterProjectEnabled(t)
	cpc := newMockCPCRepo()
	resolver := &MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"architect": {ID: "architect", AcceptCallsFrom: []string{"marketing"}},
		},
	}
	e, _ := newCallProjectExecutor(resolver, cpc)
	// Replace the task repo with one whose Get always errors on the
	// parent lookup. The caller carries a ParentTaskID so the walk
	// actually performs a Get.
	failRepo := &callProjectCov_lineageFailRepo{MockTaskRepo: NewMockTaskRepo(), err: errors.New("db unreachable")}
	e.taskRepo = failRepo

	parent := "parent-task"
	task := &persistence.Task{ID: "t1", ProjectID: "marketing", ParentTaskID: &parent}
	_, err := e.handleCallProjectStep(context.Background(), task, &persistence.Execution{ID: "e1"}, "s1", makeCallStep(), nil)
	if err == nil || !contains(err.Error(), "LINEAGE_WALK_FAILED") {
		t.Fatalf("expected LINEAGE_WALK_FAILED, got %v", err)
	}
	if len(cpc.rows) != 0 {
		t.Error("no CPC row should be created when the lineage walk fails")
	}
}

// TestCallProjectCov_TimeoutParsed asserts the step.Timeout parse
// branch stamps TimeoutAt on the CPC row, and the metrics +
// SetCalleeTaskID success path runs (metrics non-nil).
func TestCallProjectCov_TimeoutParsedAndMetrics(t *testing.T) {
	withInterProjectEnabled(t)
	cpc := newMockCPCRepo()
	resolver := &MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"architect": {ID: "architect", AcceptCallsFrom: []string{"marketing"}},
		},
	}
	e, _ := newCallProjectExecutor(resolver, cpc)
	e.metrics = NewMetrics(prometheus.NewRegistry())

	step := makeCallStep()
	step.Timeout = "30s"
	step.CancelOnTimeout = true

	task := &persistence.Task{ID: "task-caller", ProjectID: "marketing"}
	result, err := e.handleCallProjectStep(context.Background(), task, &persistence.Execution{ID: "e1"}, "s1", step, nil)
	if err != nil {
		t.Fatalf("handleCallProjectStep: %v", err)
	}
	stored, _ := cpc.Get(context.Background(), result.CPCId)
	if stored.TimeoutAt == nil {
		t.Error("TimeoutAt should be stamped when step.Timeout parses to a positive duration")
	}
	if !stored.CancelOnTimeout {
		t.Error("CancelOnTimeout should propagate to the CPC row")
	}
}

// TestCallProjectCov_CreateCPCError covers the early-return when
// the CPC repo's Create fails — no callee task should be created.
func TestCallProjectCov_CreateCPCError(t *testing.T) {
	withInterProjectEnabled(t)
	cpc := newMockCPCRepo()
	cpc.createErr = errors.New("insert failed")
	resolver := &MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"architect": {ID: "architect", AcceptCallsFrom: []string{"marketing"}},
		},
	}
	e, tr := newCallProjectExecutor(resolver, cpc)
	task := &persistence.Task{ID: "task-caller", ProjectID: "marketing"}
	_, err := e.handleCallProjectStep(context.Background(), task, &persistence.Execution{ID: "e1"}, "s1", makeCallStep(), nil)
	if err == nil || !contains(err.Error(), "create CPC row") {
		t.Fatalf("expected create-CPC error, got %v", err)
	}
	// No callee task should have been created.
	if tasks := len(tr.tasks); tasks != 0 {
		t.Errorf("expected no callee task on CPC-create failure, got %d", tasks)
	}
}

// callProjectCov_taskCreateErrRepo fails Create so the handler
// takes the "mark CPC rejected + return error" branch. Embeds
// MockTaskRepo so Get (lineage) still works.
type callProjectCov_taskCreateErrRepo struct {
	*MockTaskRepo
}

func (r *callProjectCov_taskCreateErrRepo) Create(_ context.Context, _ *persistence.Task) error {
	return errors.New("callee task insert failed")
}

// TestCallProjectCov_CalleeTaskCreateError covers the branch where
// the callee task fails to insert: the CPC must be marked rejected
// and the error surfaced.
func TestCallProjectCov_CalleeTaskCreateError(t *testing.T) {
	withInterProjectEnabled(t)
	cpc := newMockCPCRepo()
	resolver := &MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"architect": {ID: "architect", AcceptCallsFrom: []string{"marketing"}},
		},
	}
	e, _ := newCallProjectExecutor(resolver, cpc)
	e.taskRepo = &callProjectCov_taskCreateErrRepo{MockTaskRepo: NewMockTaskRepo()}

	task := &persistence.Task{ID: "task-caller", ProjectID: "marketing"}
	_, err := e.handleCallProjectStep(context.Background(), task, &persistence.Execution{ID: "e1"}, "s1", makeCallStep(), nil)
	if err == nil || !contains(err.Error(), "create callee task") {
		t.Fatalf("expected callee-task-create error, got %v", err)
	}
	// The CPC row should have been marked rejected.
	if len(cpc.rejectedIDs) != 1 {
		t.Errorf("expected CPC marked rejected after callee task create failure, got %v", cpc.rejectedIDs)
	}
}

// callProjectCov_setCalleeErrRepo fails SetCalleeTaskID so the
// handler takes the "log + continue" best-effort branch (the step
// still succeeds).
type callProjectCov_setCalleeErrRepo struct {
	*mockCPCRepo
}

func (r *callProjectCov_setCalleeErrRepo) SetCalleeTaskID(_ context.Context, _, _ string) error {
	return errors.New("stamp failed")
}

// TestCallProjectCov_SetCalleeTaskIDErrorContinues asserts a failed
// callee_task_id stamp doesn't fail the step — the row + task exist
// so the worst case is an extra resolve-hook lookup.
func TestCallProjectCov_SetCalleeTaskIDErrorContinues(t *testing.T) {
	withInterProjectEnabled(t)
	base := newMockCPCRepo()
	cpc := &callProjectCov_setCalleeErrRepo{mockCPCRepo: base}
	resolver := &MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"architect": {ID: "architect", AcceptCallsFrom: []string{"marketing"}},
		},
	}
	e, _ := newCallProjectExecutor(resolver, cpc)

	task := &persistence.Task{ID: "task-caller", ProjectID: "marketing"}
	result, err := e.handleCallProjectStep(context.Background(), task, &persistence.Execution{ID: "e1"}, "s1", makeCallStep(), nil)
	if err != nil {
		t.Fatalf("step should succeed despite SetCalleeTaskID error: %v", err)
	}
	if result.CalleeTaskID == "" {
		t.Error("callee task should still be created")
	}
}

// TestCallProjectCov_ResolveMarkFailedError covers the failed-side
// branch where MarkFailed itself errors — the hook logs and returns
// without further work (no finishCPC).
type callProjectCov_markFailedErrRepo struct {
	*mockCPCRepo
}

func (r *callProjectCov_markFailedErrRepo) MarkFailed(_ context.Context, _, _ string) error {
	return errors.New("mark failed boom")
}

func TestCallProjectCov_ResolveMarkFailedError(t *testing.T) {
	withInterProjectEnabled(t)
	base := newMockCPCRepo()
	cpc := &callProjectCov_markFailedErrRepo{mockCPCRepo: base}
	e, _ := newCallProjectExecutor(&MockWorkflowResolver{}, cpc)

	cpcID := "ccp_mf"
	base.rows[cpcID] = &persistence.CrossProjectCall{ID: cpcID, Status: persistence.CPCStatusRunning}
	calleeTask := &persistence.Task{ID: "callee-task", CrossProjectCallID: &cpcID}
	// Should not panic; just logs and returns.
	e.resolveCrossProjectCallForTask(context.Background(), calleeTask, false)
}

// TestCallProjectCov_ResolveEnvelopeFromExecRepo covers the branch
// where task.ResultEnvelope is empty but the execution repo has a
// non-empty Result — the hook falls back to the execution result.
func TestCallProjectCov_ResolveEnvelopeFromExecRepo(t *testing.T) {
	withInterProjectEnabled(t)
	cpc := newMockCPCRepo()
	e, _ := newCallProjectExecutor(&MockWorkflowResolver{}, cpc)

	cpcID := "ccp_exec"
	cpc.rows[cpcID] = &persistence.CrossProjectCall{ID: cpcID, Status: persistence.CPCStatusRunning, ExpectedSchema: "spec_envelope.v1"}
	// Seed an execution whose Result carries the envelope.
	envelope := []byte(`{"schema":"spec_envelope.v1","status":"ok"}`)
	if er, ok := e.execRepo.(*MockExecRepo); ok {
		_ = er.Create(context.Background(), &persistence.Execution{ID: "exec-callee", TaskID: "callee-task", Result: envelope})
	}
	calleeTask := &persistence.Task{ID: "callee-task", CrossProjectCallID: &cpcID} // no ResultEnvelope
	e.resolveCrossProjectCallForTask(context.Background(), calleeTask, true)
	if len(cpc.completed) != 1 {
		t.Errorf("expected MarkCompleted via exec-repo fallback envelope, got completed=%v", cpc.completed)
	}
}

// TestCallProjectCov_ResolveBadJSONEnvelope covers the parse-error
// branch: a non-JSON-object envelope resolves the CPC as rejected.
func TestCallProjectCov_ResolveBadJSONEnvelope(t *testing.T) {
	withInterProjectEnabled(t)
	cpc := newMockCPCRepo()
	e, _ := newCallProjectExecutor(&MockWorkflowResolver{}, cpc)
	cpcID := "ccp_badjson"
	cpc.rows[cpcID] = &persistence.CrossProjectCall{ID: cpcID, Status: persistence.CPCStatusRunning}
	calleeTask := &persistence.Task{ID: "callee-task", CrossProjectCallID: &cpcID, ResultEnvelope: []byte(`not json`)}
	e.resolveCrossProjectCallForTask(context.Background(), calleeTask, true)
	if len(cpc.rejectedIDs) != 1 {
		t.Errorf("expected MarkRejected for unparseable envelope, got %v", cpc.rejectedIDs)
	}
}

// TestCallProjectCov_ResolveEnvelopeShapeRejected covers the
// validateEnvelopeShape rejection inside the resolve hook: the
// envelope parses but its schema id doesn't match the expected one.
func TestCallProjectCov_ResolveEnvelopeShapeRejected(t *testing.T) {
	withInterProjectEnabled(t)
	cpc := newMockCPCRepo()
	e, _ := newCallProjectExecutor(&MockWorkflowResolver{}, cpc)
	cpcID := "ccp_shape"
	cpc.rows[cpcID] = &persistence.CrossProjectCall{ID: cpcID, Status: persistence.CPCStatusRunning, ExpectedSchema: "spec_envelope.v1"}
	// Wrong schema id in the envelope → shape rejection.
	calleeTask := &persistence.Task{ID: "callee-task", CrossProjectCallID: &cpcID, ResultEnvelope: []byte(`{"schema":"other.v9","status":"ok"}`)}
	e.resolveCrossProjectCallForTask(context.Background(), calleeTask, true)
	if len(cpc.rejectedIDs) != 1 {
		t.Errorf("expected MarkRejected for envelope-shape mismatch, got %v", cpc.rejectedIDs)
	}
}

// TestCallProjectCov_ResolveCPCRowMissing covers the finishCPC
// nil-guard: when the CPC row can't be loaded, the hook still
// completes the repo write but skips emit/metrics/audit.
func TestCallProjectCov_ResolveCPCRowMissing(t *testing.T) {
	withInterProjectEnabled(t)
	cpc := newMockCPCRepo()
	e, _ := newCallProjectExecutor(&MockWorkflowResolver{}, cpc)
	// CrossProjectCallID points at a row that doesn't exist; Get
	// returns ErrNotFound so cpc==nil inside finishCPC.
	missing := "ccp_missing"
	calleeTask := &persistence.Task{ID: "callee-task", CrossProjectCallID: &missing, LastError: ptrString("boom")}
	// Failed side: MarkFailed runs (no-op on the missing row) then
	// finishCPC short-circuits on cpc==nil.
	e.resolveCrossProjectCallForTask(context.Background(), calleeTask, false)
}

func ptrString(s string) *string { return &s }

// TestCallProjectCov_LookupExecutionIDForTaskGuards covers the nil
// and not-found guards on lookupExecutionIDForTask.
func TestCallProjectCov_LookupExecutionIDForTaskGuards(t *testing.T) {
	e, _ := newCallProjectExecutor(&MockWorkflowResolver{}, newMockCPCRepo())
	if got := e.lookupExecutionIDForTask(context.Background(), ""); got != "" {
		t.Errorf("empty task id should yield empty exec id, got %q", got)
	}
	if got := e.lookupExecutionIDForTask(context.Background(), "no-such-task"); got != "" {
		t.Errorf("unknown task should yield empty exec id, got %q", got)
	}
}

// TestCallProjectCov_BuildCalleePayloadMarshalsDepth asserts the
// callee payload envelope carries context.callDepth so the callee's
// own depth guard reads it without re-walking. Round-trips through
// readCarriedCallDepth (the cross-boundary backstop).
func TestCallProjectCov_BuildCalleePayloadCarriesDepth(t *testing.T) {
	body := buildCalleePayload([]byte(`{"brief":"x"}`), 4)
	got := readCarriedCallDepth(&persistence.Task{Payload: body})
	if got != 4 {
		t.Errorf("carried call depth = %d, want 4", got)
	}
}

// TestCallProjectCov_ResolveEmitsViaCallerExec exercises the
// finishCPC emit branch with a non-zero CreatedAt so the duration
// computation runs, plus the metrics + caller-exec lookup paths.
func TestCallProjectCov_ResolveEmitsWithDuration(t *testing.T) {
	withInterProjectEnabled(t)
	cpc := newMockCPCRepo()
	pub := &stubLivePub{}
	e, _ := newCallProjectExecutor(&MockWorkflowResolver{}, cpc)
	e.livePub = pub
	e.metrics = NewMetrics(prometheus.NewRegistry())

	cpcID := "ccp_dur"
	cpc.rows[cpcID] = &persistence.CrossProjectCall{
		ID:            cpcID,
		CallerTaskID:  "task-caller",
		CallerProject: "marketing",
		CalleeProject: "architect",
		Status:        persistence.CPCStatusRunning,
		CreatedAt:     time.Now().Add(-2 * time.Second),
	}
	if er, ok := e.execRepo.(*MockExecRepo); ok {
		_ = er.Create(context.Background(), &persistence.Execution{ID: "exec-caller", TaskID: "task-caller"})
	}
	calleeTask := &persistence.Task{
		ID:                 "callee-task",
		CrossProjectCallID: &cpcID,
		ResultEnvelope:     []byte(`{"schema":"","status":"ok"}`),
	}
	e.resolveCrossProjectCallForTask(context.Background(), calleeTask, true)
	if len(cpc.completed) != 1 {
		t.Errorf("expected completed, got %v", cpc.completed)
	}
}
