package executor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// stubHintRepo records ConsumePending calls and returns whichever
// hints the test pre-loaded. Insert / ListByExecution are unused
// surface for these tests and return zero values.
type stubHintRepo struct {
	mu       sync.Mutex
	calls    []hintConsumeCall
	pending  []*persistence.ExecutionHint
	consumed bool
	err      error
}

type hintConsumeCall struct {
	taskID      string
	executionID string
	stepID      string
}

func (s *stubHintRepo) Insert(_ context.Context, _ *persistence.ExecutionHint) error {
	return nil
}

func (s *stubHintRepo) ConsumePending(_ context.Context, taskID, executionID, stepID string) ([]*persistence.ExecutionHint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, hintConsumeCall{taskID: taskID, executionID: executionID, stepID: stepID})
	if s.err != nil {
		return nil, s.err
	}
	if s.consumed {
		return nil, nil
	}
	s.consumed = true
	return s.pending, nil
}

func (s *stubHintRepo) ListByExecution(_ context.Context, _ string) ([]*persistence.ExecutionHint, error) {
	return nil, nil
}

func (s *stubHintRepo) ListForExecution(_ context.Context, _, _ string) ([]*persistence.ExecutionHint, error) {
	return nil, nil
}

func (s *stubHintRepo) ListPendingForTask(_ context.Context, _ string) ([]*persistence.ExecutionHint, error) {
	return nil, nil
}

func (s *stubHintRepo) ListByTask(_ context.Context, _ string) ([]*persistence.ExecutionHint, error) {
	return nil, nil
}

// TestConsumeHintsForStep_NilExecutorReturnsEmpty — defensive: a nil
// Executor receiver must not panic when callers wire the executor
// optionally (test setup paths).
func TestConsumeHintsForStep_NilExecutorReturnsEmpty(t *testing.T) {
	var e *Executor
	got := e.consumeHintsForStep(context.Background(), "task-1", "exec-1", "step-1")
	assert.Empty(t, got)
}

// TestConsumeHintsForStep_NilRepoReturnsEmpty — when no repo wired,
// the function is a clean no-op returning "". Covers the "deployment
// without hint persistence" path.
func TestConsumeHintsForStep_NilRepoReturnsEmpty(t *testing.T) {
	e := &Executor{logger: zerolog.Nop()}
	got := e.consumeHintsForStep(context.Background(), "task-1", "exec-1", "step-1")
	assert.Empty(t, got)
}

// TestConsumeHintsForStep_EmptyExecutionReturnsEmpty — guard against
// calls with no execution context (would consume the wrong rows).
func TestConsumeHintsForStep_EmptyExecutionReturnsEmpty(t *testing.T) {
	repo := &stubHintRepo{}
	e := &Executor{logger: zerolog.Nop(), hintRepo: repo}
	got := e.consumeHintsForStep(context.Background(), "task-1", "", "step-1")
	assert.Empty(t, got)
	assert.Empty(t, repo.calls, "must not query the repo when executionID is blank")
}

// TestConsumeHintsForStep_RepoErrorReturnsEmpty — a transient DB
// error must not propagate; the step-start path can't usefully act
// on it and a failed hint fetch should not block step execution.
func TestConsumeHintsForStep_RepoErrorReturnsEmpty(t *testing.T) {
	repo := &stubHintRepo{err: errors.New("db down")}
	e := &Executor{logger: zerolog.Nop(), hintRepo: repo}
	got := e.consumeHintsForStep(context.Background(), "task-1", "exec-1", "step-1")
	assert.Empty(t, got)
}

// TestConsumeHintsForStep_WrapsHintsInTags — the consumed hints are
// rendered with <operator-hint>...</operator-hint> wrappers so the
// agent treats them as instruction-shaped context. Multiple hints
// concatenate in insertion order. This is the contract the plan
// step's hint plumbing (2026-05-26 fix) and the workflow agent
// branch both depend on.
func TestConsumeHintsForStep_WrapsHintsInTags(t *testing.T) {
	repo := &stubHintRepo{
		pending: []*persistence.ExecutionHint{
			{ID: "h1", Content: "use alternative source", CreatedBy: "alice"},
			{ID: "h2", Content: "skip paywalled URLs", CreatedBy: "bob"},
		},
	}
	e := &Executor{logger: zerolog.Nop(), hintRepo: repo}
	got := e.consumeHintsForStep(context.Background(), "task-1", "exec-1", "step-1")
	require.NotEmpty(t, got)
	assert.Contains(t, got, "<operator-hint")
	assert.Contains(t, got, `from="alice"`)
	assert.Contains(t, got, "use alternative source")
	assert.Contains(t, got, `from="bob"`)
	assert.Contains(t, got, "skip paywalled URLs")
	assert.Equal(t, 2, strings.Count(got, "</operator-hint>"))
	require.Len(t, repo.calls, 1)
	assert.Equal(t, "exec-1", repo.calls[0].executionID)
	assert.Equal(t, "step-1", repo.calls[0].stepID)
}

// TestExecutePlanStep_ConsumesHints_AtBoundary — guards Bug C from
// T-87bf: the case "plan": branch in workflow.go must consume any
// pending operator hints at the parent plan-step boundary so
// steering messages reach the lead's prompt. Pre-fix, only
// case "agent": did this, so hints posted while the recover step
// was running were silently dropped.
//
// We drive executePlanStep down its resume-past-end branch
// (PlanIndex >= len(PlanSteps)) to keep the test focused on the
// hint-consume side effect without needing a full agent runtime.
func TestExecutePlanStep_ConsumesHints_AtBoundary(t *testing.T) {
	rt := NewMockRuntime()
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, ar, tr, nil)
	e.config.RetryDelay = 0
	hr := &stubHintRepo{
		pending: []*persistence.ExecutionHint{
			{ID: "h1", Content: "use cached snapshot", CreatedBy: "alice"},
		},
	}
	e.hintRepo = hr

	task := &persistence.Task{ID: "t-hint", ProjectID: "p"}
	tr.AddTask(task)
	exec := &persistence.Execution{ID: "x-hint", TaskID: task.ID, ProjectID: "p"}
	require.NoError(t, er.Create(context.Background(), exec))

	plan := &executionPlan{
		swarm:    &registry.Swarm{ID: "s", Roles: []registry.SwarmRole{{Name: "lead"}}},
		workflow: &registry.Workflow{ID: "wf"},
	}
	step := registry.WorkflowStep{Type: "plan", Role: "lead", OnSuccess: "final"}
	state := &executionState{
		PlanSteps:       []string{"researcher"},
		PlanIndex:       1, // past end → skip loop
		PlanLeadStepID:  "plan_lead_lead",
		PlanLeadMessage: "noop",
	}

	_, _, _, _, err := e.executePlanStep(
		context.Background(), task, exec, plan,
		"recover", step, time.Minute, state,
		nil, nil,
	)
	require.NoError(t, err)
	require.Len(t, hr.calls, 1, "executePlanStep must consume hints exactly once at the plan-step boundary")
	assert.Equal(t, "x-hint", hr.calls[0].executionID)
	assert.Equal(t, "recover", hr.calls[0].stepID,
		"hint consume must use the parent plan stepID, not a synthetic sub-step")
}

// TestConsumeHintsForStep_SkipsBlankContent — pending hints whose
// content is whitespace-only are dropped silently. Defends against
// a UI bug that posts an empty string.
func TestConsumeHintsForStep_SkipsBlankContent(t *testing.T) {
	repo := &stubHintRepo{
		pending: []*persistence.ExecutionHint{
			{ID: "h1", Content: "   "},
			{ID: "h2", Content: "real content"},
		},
	}
	e := &Executor{logger: zerolog.Nop(), hintRepo: repo}
	got := e.consumeHintsForStep(context.Background(), "task-1", "exec-1", "step-1")
	assert.Equal(t, 1, strings.Count(got, "</operator-hint>"))
	assert.Contains(t, got, "real content")
}
