package executor

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// mockCPCRepo is a thread-safe in-memory CrossProjectCallRepository
// for the call_project handler tests. Captures every Create +
// state transition so the assertions can verify both the row
// shape and the call sequence.
type mockCPCRepo struct {
	mu          sync.Mutex
	rows        map[string]*persistence.CrossProjectCall
	createErr   error
	rejectedIDs []string
	completed   []string
	failed      []string
}

func newMockCPCRepo() *mockCPCRepo {
	return &mockCPCRepo{rows: make(map[string]*persistence.CrossProjectCall)}
}

func (m *mockCPCRepo) Create(_ context.Context, c *persistence.CrossProjectCall) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.createErr != nil {
		return m.createErr
	}
	if c.ID == "" {
		c.ID = persistence.GenerateID("ccp")
	}
	if c.Status == "" {
		c.Status = persistence.CPCStatusPending
	}
	cp := *c
	m.rows[c.ID] = &cp
	return nil
}

func (m *mockCPCRepo) Get(_ context.Context, id string) (*persistence.CrossProjectCall, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.rows[id]; ok {
		cp := *r
		return &cp, nil
	}
	return nil, persistence.ErrNotFound
}

func (m *mockCPCRepo) GetByCalleeTaskID(_ context.Context, calleeTaskID string) (*persistence.CrossProjectCall, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.CalleeTaskID != nil && *r.CalleeTaskID == calleeTaskID {
			cp := *r
			return &cp, nil
		}
	}
	return nil, persistence.ErrNotFound
}

func (m *mockCPCRepo) SetCalleeTaskID(_ context.Context, id, calleeTaskID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id]
	if !ok {
		return persistence.ErrNotFound
	}
	if r.CalleeTaskID != nil {
		return persistence.ErrNotFound
	}
	r.CalleeTaskID = &calleeTaskID
	return nil
}

func (m *mockCPCRepo) MarkRunning(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.rows[id]; ok && r.Status == persistence.CPCStatusPending {
		r.Status = persistence.CPCStatusRunning
	}
	return nil
}

func (m *mockCPCRepo) MarkCompleted(_ context.Context, id string, envelope []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.rows[id]; ok {
		r.Status = persistence.CPCStatusCompleted
		r.ResultEnvelope = envelope
		m.completed = append(m.completed, id)
	}
	return nil
}

func (m *mockCPCRepo) MarkFailed(_ context.Context, id, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.rows[id]; ok {
		r.Status = persistence.CPCStatusFailed
		r.ErrorMessage = &reason
		m.failed = append(m.failed, id)
	}
	return nil
}

func (m *mockCPCRepo) MarkRejected(_ context.Context, id, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.rows[id]; ok {
		r.Status = persistence.CPCStatusRejected
		r.ErrorMessage = &reason
		m.rejectedIDs = append(m.rejectedIDs, id)
	}
	return nil
}

func (m *mockCPCRepo) List(_ context.Context, filter persistence.CPCListFilter) ([]*persistence.CrossProjectCall, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []*persistence.CrossProjectCall{}
	for _, r := range m.rows {
		if filter.Status != "" && r.Status != filter.Status {
			continue
		}
		if filter.CallerProject != "" && r.CallerProject != filter.CallerProject {
			continue
		}
		if filter.CalleeProject != "" && r.CalleeProject != filter.CalleeProject {
			continue
		}
		if !filter.CreatedSince.IsZero() && r.CreatedAt.Before(filter.CreatedSince) {
			continue
		}
		cp := *r
		out = append(out, &cp)
	}
	return out, nil
}

func (m *mockCPCRepo) AdminCancel(_ context.Context, id, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id]
	if !ok {
		return persistence.ErrNotFound
	}
	switch r.Status {
	case persistence.CPCStatusCompleted, persistence.CPCStatusFailed,
		persistence.CPCStatusTimedOut, persistence.CPCStatusRejected:
		return nil
	}
	r.Status = persistence.CPCStatusRejected
	r.ErrorMessage = &reason
	return nil
}

func (m *mockCPCRepo) ClaimTimedOut(_ context.Context, now time.Time, limit int) ([]*persistence.CrossProjectCall, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []*persistence.CrossProjectCall{}
	for _, r := range m.rows {
		if r.Status != persistence.CPCStatusPending && r.Status != persistence.CPCStatusRunning {
			continue
		}
		if r.TimeoutAt == nil || !r.TimeoutAt.Before(now) {
			continue
		}
		r.Status = persistence.CPCStatusTimedOut
		reason := "cross_project_call timeout elapsed before resolve"
		r.ErrorMessage = &reason
		cp := *r
		out = append(out, &cp)
		if len(out) >= limit && limit > 0 {
			break
		}
	}
	return out, nil
}

// withInterProjectEnabled flips the feature flag for the
// duration of the test. Tests that don't use this still get
// the secure default (false), so the handler returns
// errCrossProjectDisabled and no CPC/task rows leak.
func withInterProjectEnabled(t *testing.T) {
	t.Helper()
	t.Setenv(interProjectEnabledEnv, "true")
}

func newCallProjectExecutor(workflows WorkflowResolver, cpc persistence.CrossProjectCallRepository) (*Executor, *MockTaskRepo) {
	e, _, _, _, tr := setup()
	e.workflows = workflows
	e.cpcRepo = cpc
	return e, tr
}

func makeCallStep() *registry.WorkflowStep {
	return &registry.WorkflowStep{
		Type:           "call_project",
		TargetProject:  "architect",
		TargetWorkflow: "produce-spec",
		Payload:        map[string]any{"brief": "launch Q3 campaign"},
		Expect:         registry.WorkflowCallExpect{Schema: "spec_envelope.v1"},
	}
}

// TestCallProject_FeatureFlagOff_ReturnsDisabled covers the
// secure default. With VORNIK_INTER_PROJECT_ENABLED unset, the
// handler refuses regardless of how the rest is wired. Lets
// operators land the migration + code without exposing the
// surface until they're ready (LLD §11).
func TestCallProject_FeatureFlagOff_ReturnsDisabled(t *testing.T) {
	// No t.Setenv — feature flag stays off.
	cpc := newMockCPCRepo()
	resolver := &MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"architect": {ID: "architect", AcceptCallsFrom: []string{"marketing"}},
		},
	}
	e, _ := newCallProjectExecutor(resolver, cpc)
	task := &persistence.Task{ID: "task-caller", ProjectID: "marketing"}
	exec := &persistence.Execution{ID: "exec-1"}

	_, err := e.handleCallProjectStep(context.Background(), task, exec, "call-step", makeCallStep(), nil)
	if !errors.Is(err, errCrossProjectDisabled) {
		t.Fatalf("expected errCrossProjectDisabled, got %v", err)
	}
	if len(cpc.rows) != 0 {
		t.Error("no CPC row should have been created with the flag off")
	}
}

// TestCallProject_CPCRepoUnwired_ReturnsDisabled covers the
// "feature flag on but repo nil" case — same surface, same
// error. The handler must NOT call into a nil repo even when
// the flag is true.
func TestCallProject_CPCRepoUnwired_ReturnsDisabled(t *testing.T) {
	withInterProjectEnabled(t)
	resolver := &MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"architect": {ID: "architect"},
		},
	}
	e, _ := newCallProjectExecutor(resolver, nil)
	task := &persistence.Task{ID: "t1", ProjectID: "marketing"}
	exec := &persistence.Execution{ID: "exec-1"}

	_, err := e.handleCallProjectStep(context.Background(), task, exec, "step-1", makeCallStep(), nil)
	if !errors.Is(err, errCrossProjectDisabled) {
		t.Fatalf("expected errCrossProjectDisabled, got %v", err)
	}
}

// TestCallProject_CalleeProjectMissing_ReturnsNotFound asserts
// the handler refuses when the workflows resolver doesn't know
// the callee project. The error message is a clean
// PROJECT_NOT_FOUND so the on_fail branch can match on it.
func TestCallProject_CalleeProjectMissing_ReturnsNotFound(t *testing.T) {
	withInterProjectEnabled(t)
	cpc := newMockCPCRepo()
	resolver := &MockWorkflowResolver{projects: map[string]*registry.Project{}}
	e, _ := newCallProjectExecutor(resolver, cpc)
	task := &persistence.Task{ID: "t1", ProjectID: "marketing"}
	exec := &persistence.Execution{ID: "exec-1"}

	_, err := e.handleCallProjectStep(context.Background(), task, exec, "s1", makeCallStep(), nil)
	if err == nil || !contains(err.Error(), "PROJECT_NOT_FOUND") {
		t.Fatalf("expected PROJECT_NOT_FOUND error, got %v", err)
	}
	if len(cpc.rows) != 0 {
		t.Error("no CPC row should have been created on PROJECT_NOT_FOUND")
	}
}

// TestCallProject_AcceptCallsFromDenies_ReturnsRejected asserts
// the security gate fires. Callee project exists but has
// closed acceptCallsFrom — the handler refuses with
// CROSS_PROJECT_REJECTED and no CPC row is created (the gate
// runs before any DB write).
func TestCallProject_AcceptCallsFromDenies_ReturnsRejected(t *testing.T) {
	withInterProjectEnabled(t)
	cpc := newMockCPCRepo()
	resolver := &MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"architect": {ID: "architect" /* no AcceptCallsFrom — closed */},
		},
	}
	e, _ := newCallProjectExecutor(resolver, cpc)
	task := &persistence.Task{ID: "t1", ProjectID: "marketing"}
	exec := &persistence.Execution{ID: "exec-1"}

	_, err := e.handleCallProjectStep(context.Background(), task, exec, "s1", makeCallStep(), nil)
	if err == nil || !contains(err.Error(), "CROSS_PROJECT_REJECTED") {
		t.Fatalf("expected CROSS_PROJECT_REJECTED, got %v", err)
	}
	if len(cpc.rows) != 0 {
		t.Error("no CPC row should have been created when acceptCallsFrom denies")
	}
}

// TestCallProject_CanCallDenies_ReturnsRejected asserts the CALLER-side
// outbound allowlist fires before the callee gate: even though the callee
// accepts marketing, the caller's canCallProjects doesn't permit calling
// architect, so the handler refuses with CROSS_PROJECT_REJECTED and writes
// no CPC row.
func TestCallProject_CanCallDenies_ReturnsRejected(t *testing.T) {
	withInterProjectEnabled(t)
	cpc := newMockCPCRepo()
	resolver := &MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"architect": {ID: "architect", AcceptCallsFrom: []string{"marketing"}},
			"marketing": {ID: "marketing", CanCallProjects: []string{"data-*"}}, // not architect
		},
	}
	e, _ := newCallProjectExecutor(resolver, cpc)
	task := &persistence.Task{ID: "t1", ProjectID: "marketing"}
	exec := &persistence.Execution{ID: "exec-1"}

	_, err := e.handleCallProjectStep(context.Background(), task, exec, "s1", makeCallStep(), nil)
	if err == nil || !contains(err.Error(), "not allowed to call") {
		t.Fatalf("expected caller-side CROSS_PROJECT_REJECTED (not allowed to call), got %v", err)
	}
	if len(cpc.rows) != 0 {
		t.Error("no CPC row should be created when canCallProjects denies")
	}
}

// TestCallProject_HappyPath_CreatesCPCAndCalleeTask asserts
// the full positive flow: CPC row + callee task created atomically,
// callee_task_id stamped on the CPC, callee task carries the
// CPC id back-reference, parent linkage set so the existing
// checkParentUnblock wakes the caller on terminal.
func TestCallProject_HappyPath_CreatesCPCAndCalleeTask(t *testing.T) {
	withInterProjectEnabled(t)
	cpc := newMockCPCRepo()
	resolver := &MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"architect": {ID: "architect", AcceptCallsFrom: []string{"marketing"}},
		},
	}
	e, tr := newCallProjectExecutor(resolver, cpc)
	task := &persistence.Task{ID: "task-caller", ProjectID: "marketing"}
	exec := &persistence.Execution{ID: "exec-1"}

	result, err := e.handleCallProjectStep(context.Background(), task, exec, "step-1", makeCallStep(), nil)
	if err != nil {
		t.Fatalf("handleCallProjectStep: %v", err)
	}
	if result.CPCId == "" {
		t.Error("CPCId should be populated on success")
	}
	if result.CalleeTaskID == "" {
		t.Error("CalleeTaskID should be populated on success")
	}
	if result.PauseReason != PauseReasonAwaitingChildren {
		t.Errorf("PauseReason = %q, want %q", result.PauseReason, PauseReasonAwaitingChildren)
	}

	// Inspect CPC row shape.
	stored, _ := cpc.Get(context.Background(), result.CPCId)
	if stored == nil {
		t.Fatal("CPC row not stored")
	}
	if stored.CallerProject != "marketing" || stored.CalleeProject != "architect" {
		t.Errorf("project routing wrong: caller=%q callee=%q", stored.CallerProject, stored.CalleeProject)
	}
	if stored.CallerTaskID != "task-caller" || stored.CallerStepID != "step-1" {
		t.Errorf("caller linkage wrong: task=%q step=%q", stored.CallerTaskID, stored.CallerStepID)
	}
	if stored.CalleeTaskID == nil || *stored.CalleeTaskID != result.CalleeTaskID {
		t.Errorf("callee_task_id not stamped: %v", stored.CalleeTaskID)
	}
	if stored.ExpectedSchema != "spec_envelope.v1" {
		t.Errorf("expected_schema = %q", stored.ExpectedSchema)
	}

	// Inspect the callee task row.
	calleeTask, err := tr.Get(context.Background(), result.CalleeTaskID)
	if err != nil {
		t.Fatalf("callee task not stored: %v", err)
	}
	if calleeTask.ProjectID != "architect" {
		t.Errorf("callee task lands in %q, want architect", calleeTask.ProjectID)
	}
	if calleeTask.Status != persistence.TaskStatusQueued {
		t.Errorf("callee task status = %q, want QUEUED", calleeTask.Status)
	}
	if calleeTask.ParentTaskID == nil || *calleeTask.ParentTaskID != "task-caller" {
		t.Errorf("callee.ParentTaskID = %v, want task-caller", calleeTask.ParentTaskID)
	}
	if calleeTask.CrossProjectCallID == nil || *calleeTask.CrossProjectCallID != result.CPCId {
		t.Errorf("callee.CrossProjectCallID = %v, want %s", calleeTask.CrossProjectCallID, result.CPCId)
	}
}

// TestResolveCPC_NoOpForNonCallee asserts the resolve hook is
// safe to call on every terminal task — most tasks aren't CPC
// callees, so the hook short-circuits cleanly. Without this
// guard, every task termination would hit the repo
// unnecessarily.
func TestResolveCPC_NoOpForNonCallee(t *testing.T) {
	withInterProjectEnabled(t)
	cpc := newMockCPCRepo()
	e, _ := newCallProjectExecutor(&MockWorkflowResolver{}, cpc)

	// Task has NO CrossProjectCallID — hook should no-op.
	task := &persistence.Task{ID: "regular-task"}
	e.resolveCrossProjectCallForTask(context.Background(), task, true)
	if len(cpc.completed) != 0 || len(cpc.failed) != 0 || len(cpc.rejectedIDs) != 0 {
		t.Error("resolve hook touched CPC repo for non-callee task")
	}
}

// TestResolveCPC_Completed_WritesEnvelope covers the happy path:
// a callee task terminates COMPLETED with a non-empty JSON
// envelope; the resolve hook validates the shape and persists
// the envelope to the CPC row.
func TestResolveCPC_Completed_WritesEnvelope(t *testing.T) {
	withInterProjectEnabled(t)
	cpc := newMockCPCRepo()
	e, _ := newCallProjectExecutor(&MockWorkflowResolver{}, cpc)

	// Seed a pending CPC row.
	envelopeBody := []byte(`{"schema":"spec_envelope.v1","status":"ok","summary":"spec done","data":{"kpis":2}}`)
	cpcID := "ccp_test"
	cpc.rows[cpcID] = &persistence.CrossProjectCall{
		ID: cpcID, Status: persistence.CPCStatusRunning, ExpectedSchema: "spec_envelope.v1",
	}
	calleeTask := &persistence.Task{
		ID:                 "callee-task",
		CrossProjectCallID: &cpcID,
		ResultEnvelope:     envelopeBody,
	}

	e.resolveCrossProjectCallForTask(context.Background(), calleeTask, true)
	if len(cpc.completed) != 1 || cpc.completed[0] != cpcID {
		t.Errorf("expected exactly one MarkCompleted for %s, got %v", cpcID, cpc.completed)
	}
	stored := cpc.rows[cpcID]
	if stored.Status != persistence.CPCStatusCompleted {
		t.Errorf("status = %q, want completed", stored.Status)
	}
	var got map[string]any
	if err := json.Unmarshal(stored.ResultEnvelope, &got); err != nil {
		t.Errorf("envelope not parseable JSON: %v", err)
	}
	if got["summary"] != "spec done" {
		t.Errorf("envelope content lost: %v", got)
	}
}

// TestResolveCPC_EmptyEnvelope_Rejects asserts the rejected
// path: a callee task terminated COMPLETED but produced no
// envelope. The caller should NOT block waiting forever —
// resolve as rejected so on_fail can fire.
func TestResolveCPC_EmptyEnvelope_Rejects(t *testing.T) {
	withInterProjectEnabled(t)
	cpc := newMockCPCRepo()
	e, _ := newCallProjectExecutor(&MockWorkflowResolver{}, cpc)
	cpcID := "ccp_test"
	cpc.rows[cpcID] = &persistence.CrossProjectCall{ID: cpcID, Status: persistence.CPCStatusRunning}
	calleeTask := &persistence.Task{ID: "callee-task", CrossProjectCallID: &cpcID}

	e.resolveCrossProjectCallForTask(context.Background(), calleeTask, true)
	if len(cpc.rejectedIDs) != 1 {
		t.Errorf("expected MarkRejected for empty envelope, got rejected=%v", cpc.rejectedIDs)
	}
}

// TestResolveCPC_FailedSide_MarksFailed asserts the failed
// path: callee task ended FAILED or CANCELLED → CPC=failed
// with the last_error propagated as the reason.
func TestResolveCPC_FailedSide_MarksFailed(t *testing.T) {
	withInterProjectEnabled(t)
	cpc := newMockCPCRepo()
	e, _ := newCallProjectExecutor(&MockWorkflowResolver{}, cpc)
	cpcID := "ccp_test"
	cpc.rows[cpcID] = &persistence.CrossProjectCall{ID: cpcID, Status: persistence.CPCStatusRunning}
	errMsg := "tool budget exceeded"
	calleeTask := &persistence.Task{
		ID:                 "callee-task",
		CrossProjectCallID: &cpcID,
		LastError:          &errMsg,
	}

	e.resolveCrossProjectCallForTask(context.Background(), calleeTask, false)
	if len(cpc.failed) != 1 {
		t.Errorf("expected MarkFailed, got failed=%v", cpc.failed)
	}
	stored := cpc.rows[cpcID]
	if stored.ErrorMessage == nil || !contains(*stored.ErrorMessage, "tool budget exceeded") {
		t.Errorf("error_message should include callee's last_error, got %v", stored.ErrorMessage)
	}
}

// MockWorkflowResolver as referenced in executor_test.go has
// projects/swarms/workflows maps. We re-use that here via
// the inlined declaration above. No additional fields needed.

func contains(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
