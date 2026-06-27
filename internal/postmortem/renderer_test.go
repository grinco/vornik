package postmortem

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/playbook"
)

// ---------- stubs ----------

// rendererTaskRepo embeds persistence.TaskRepository so the
// renderer's narrow Get-only usage works without forcing the
// stub to implement every TaskRepository method. Same trick
// as stubTaskRepo for Explainer tests, kept distinct so the
// rendering tests are self-contained.
type rendererTaskRepo struct {
	persistence.TaskRepository
	task    *persistence.Task
	err     error
	wantNil bool
}

func (s *rendererTaskRepo) Get(_ context.Context, _ string) (*persistence.Task, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.wantNil {
		return nil, nil
	}
	return s.task, nil
}

type rendererExecRepo struct {
	persistence.ExecutionRepository
	exec *persistence.Execution
	err  error
}

func (s *rendererExecRepo) GetByTaskID(_ context.Context, _ string) (*persistence.Execution, error) {
	return s.exec, s.err
}

type rendererOutcomeRepo struct {
	persistence.ExecutionStepOutcomeRepository
	rows []*persistence.ExecutionStepOutcome
	err  error
}

func (s *rendererOutcomeRepo) List(_ context.Context, _ persistence.ExecutionStepOutcomeFilter) ([]*persistence.ExecutionStepOutcome, error) {
	return s.rows, s.err
}

type rendererAuditRepo struct {
	persistence.ToolAuditRepository
	rows []*persistence.ToolAuditEntry
	err  error
}

func (s *rendererAuditRepo) List(_ context.Context, _ persistence.ToolAuditFilter) ([]*persistence.ToolAuditEntry, error) {
	return s.rows, s.err
}

type rendererLogs struct {
	out string
	err error
}

func (l *rendererLogs) TaskLogs(_ context.Context, _ string, _ int) (string, error) {
	return l.out, l.err
}

// ---------- input validation ----------

func TestRender_NilRenderer(t *testing.T) {
	var r *Renderer
	if _, err := r.Render(context.Background(), "task-1"); err == nil {
		t.Fatal("expected error for nil renderer")
	}
}

func TestRender_RequiresTaskRepo(t *testing.T) {
	r := &Renderer{}
	if _, err := r.Render(context.Background(), "task-1"); err == nil ||
		!strings.Contains(err.Error(), "task repo is required") {
		t.Fatalf("expected task-repo-required error, got %v", err)
	}
}

func TestRender_RequiresTaskID(t *testing.T) {
	r := &Renderer{Tasks: &rendererTaskRepo{}}
	if _, err := r.Render(context.Background(), ""); err == nil ||
		!strings.Contains(err.Error(), "task ID is required") {
		t.Fatalf("expected task-ID-required error, got %v", err)
	}
}

func TestRender_TaskLookupError(t *testing.T) {
	r := &Renderer{Tasks: &rendererTaskRepo{err: errors.New("conn closed")}}
	if _, err := r.Render(context.Background(), "task-1"); err == nil {
		t.Fatal("expected task lookup error")
	}
}

func TestRender_TaskNotFound(t *testing.T) {
	r := &Renderer{Tasks: &rendererTaskRepo{wantNil: true}}
	_, err := r.Render(context.Background(), "task-1")
	if !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// ---------- happy path: full evidence ----------

// TestRender_FullEvidence covers every populated branch in the
// renderer: workflow id from task, last error + class, step
// outcomes (with a failure-coloured row), recent tools, log tail,
// playbook hydration.
func TestRender_FullEvidence(t *testing.T) {
	wfID := "wf-1"
	lastErr := "tool call timeout after 30s"
	lastClass := persistence.TaskFailureClassToolError

	task := &persistence.Task{
		ID:             "task-1",
		ProjectID:      "p-1",
		Status:         persistence.TaskStatusFailed,
		WorkflowID:     &wfID,
		LastError:      &lastErr,
		LastErrorClass: &lastClass,
	}
	exec := &persistence.Execution{ID: "exec-1", WorkflowID: "wf-1"}
	outcomes := []*persistence.ExecutionStepOutcome{
		{StepID: "s1", Role: "planner", Model: "claude-sonnet", Outcome: "used"},
		{
			StepID:      "s2",
			Role:        "coder",
			Model:       "claude-opus",
			Outcome:     "parse_error",
			ErrorClass:  "schema",
			ErrorDetail: "expected object, got array",
		},
		nil, // tolerated
	}
	audits := []*persistence.ToolAuditEntry{
		{StepID: "s1", ToolName: "memory_search", ToolInput: "q", ToolOutput: "hit", DurationMs: 42},
		{StepID: "s2", ToolName: "run_shell", ToolInput: "ls", ToolOutput: "stderr: command failed", DurationMs: 1500},
		nil,
	}
	logs := &rendererLogs{out: "panic: nil pointer\nstack trace here\n"}

	r := NewRenderer(
		&rendererTaskRepo{task: task},
		&rendererExecRepo{exec: exec},
		&rendererOutcomeRepo{rows: outcomes},
		&rendererAuditRepo{rows: audits},
		logs,
	)

	res, err := r.Render(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	in := res.Inputs

	if in.TaskID != "task-1" || in.ProjectID != "p-1" || in.WorkflowID != "wf-1" {
		t.Errorf("input identity wrong: %+v", in)
	}
	if in.LastError != lastErr {
		t.Errorf("last_error roundtrip: %q", in.LastError)
	}
	if in.LastErrorClass != lastClass {
		t.Errorf("last_error_class roundtrip: %q", in.LastErrorClass)
	}
	if in.Playbook == nil || in.Playbook.Class != lastClass {
		t.Errorf("playbook should be hydrated for class %q, got %+v", lastClass, in.Playbook)
	}
	if len(in.StepOutcomes) != 2 {
		t.Fatalf("expected 2 step outcomes (nil row dropped), got %d", len(in.StepOutcomes))
	}
	if in.StepOutcomes[1].ErrorDetail != "expected object, got array" {
		t.Errorf("step outcome detail not preserved: %+v", in.StepOutcomes[1])
	}
	if len(in.RecentTools) != 2 {
		t.Fatalf("expected 2 tool entries (nil row dropped), got %d", len(in.RecentTools))
	}
	if in.RecentTools[1].DurationMS != 1500 {
		t.Errorf("tool DurationMS not preserved: %+v", in.RecentTools[1])
	}
	if !strings.Contains(in.LogTail, "panic: nil pointer") {
		t.Errorf("log tail not captured: %q", in.LogTail)
	}

	// Summary: must reference role + class headline, the proximate
	// signal from the failing step, AND a playbook suggestion.
	if !strings.Contains(res.Summary, "task-1") {
		t.Errorf("summary missing task id: %q", res.Summary)
	}
	if !strings.Contains(res.Summary, "coder") {
		t.Errorf("summary should mention failing step role: %q", res.Summary)
	}
	if !strings.Contains(strings.ToLower(res.Summary), "tool error") {
		t.Errorf("summary should mention humanized class: %q", res.Summary)
	}
	if !strings.Contains(res.Summary, "Proximate signal") && !strings.Contains(res.Summary, "Last tool") {
		t.Errorf("summary should carry proximate cause: %q", res.Summary)
	}
	if !strings.Contains(res.Summary, "Try first:") {
		t.Errorf("summary should carry first playbook suggestion: %q", res.Summary)
	}
}

// ---------- summary structure: per branch ----------

// TestRender_HeadlineNoClass covers the headline branch where
// LastErrorClass is empty — the task ended in status but the
// classifier didn't tag it.
func TestRender_HeadlineNoClass(t *testing.T) {
	task := &persistence.Task{ID: "task-1", ProjectID: "p", Status: persistence.TaskStatusCancelled}
	r := &Renderer{Tasks: &rendererTaskRepo{task: task}}
	res, err := r.Render(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(res.Summary, "no classified failure") {
		t.Errorf("expected no-class headline, got %q", res.Summary)
	}
	if !strings.Contains(res.Summary, "inspect the task's executions") {
		t.Errorf("expected generic next-action, got %q", res.Summary)
	}
}

// TestRender_UnknownClass exercises the humanizeClass default
// branch: a class string not in the switch returns the lowercased
// version. Belt-and-braces for forward compatibility.
func TestRender_UnknownClass(t *testing.T) {
	unknown := "UNRECOGNISED_FAILURE"
	task := &persistence.Task{
		ID: "task-1", ProjectID: "p", Status: persistence.TaskStatusFailed,
		LastErrorClass: &unknown,
	}
	r := &Renderer{Tasks: &rendererTaskRepo{task: task}}
	res, err := r.Render(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// humanizeClass lowercases + replaces _ with space; the playbook
	// returns an "unknown class" entry whose Suggestions are populated,
	// so the Try-first branch should fire.
	if !strings.Contains(res.Summary, "unrecognised failure") {
		t.Errorf("expected humanised default-branch class, got %q", res.Summary)
	}
}

// TestRender_HeadlineUsesModel covers the model-aware branch of
// the headline (failing step has a non-empty model field).
func TestRender_HeadlineUsesModel(t *testing.T) {
	class := persistence.TaskFailureClassTimeout
	task := &persistence.Task{
		ID: "task-1", ProjectID: "p", Status: persistence.TaskStatusFailed,
		LastErrorClass: &class,
	}
	r := &Renderer{
		Tasks: &rendererTaskRepo{task: task},
		Outcomes: &rendererOutcomeRepo{rows: []*persistence.ExecutionStepOutcome{
			{StepID: "s1", Role: "reviewer", Model: "claude-haiku", Outcome: "refused"},
		}},
		Executions: &rendererExecRepo{exec: &persistence.Execution{ID: "e-1"}},
	}
	res, err := r.Render(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(res.Summary, "reviewer step on claude-haiku") {
		t.Errorf("expected model-aware headline, got %q", res.Summary)
	}
}

// TestRender_HeadlineUnnamedStep covers the empty-role branch
// inside headline().
func TestRender_HeadlineUnnamedStep(t *testing.T) {
	class := persistence.TaskFailureClassLLMError
	task := &persistence.Task{
		ID: "task-1", ProjectID: "p", Status: persistence.TaskStatusFailed,
		LastErrorClass: &class,
	}
	r := &Renderer{
		Tasks:      &rendererTaskRepo{task: task},
		Executions: &rendererExecRepo{exec: &persistence.Execution{ID: "e-1"}},
		Outcomes: &rendererOutcomeRepo{rows: []*persistence.ExecutionStepOutcome{
			{StepID: "s1", Role: "", Outcome: "refused"}, // no role, no model
		}},
	}
	res, err := r.Render(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(res.Summary, "unnamed step") {
		t.Errorf("expected unnamed-step headline, got %q", res.Summary)
	}
}

// TestRender_HeadlineEmptyStatus covers the empty-status branch
// (task created with an empty status — defensive).
func TestRender_HeadlineEmptyStatus(t *testing.T) {
	class := persistence.TaskFailureClassUnknown
	task := &persistence.Task{
		ID: "task-1", ProjectID: "p",
		LastErrorClass: &class,
	}
	r := &Renderer{Tasks: &rendererTaskRepo{task: task}}
	res, err := r.Render(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(res.Summary, "terminal") {
		t.Errorf("expected empty-status default 'terminal', got %q", res.Summary)
	}
}

// TestRender_ProximateCauseFromToolOutput verifies the second
// branch of proximateCause: no step outcome with detail, but the
// last tool audit row carries an output.
func TestRender_ProximateCauseFromToolOutput(t *testing.T) {
	class := persistence.TaskFailureClassToolError
	task := &persistence.Task{
		ID: "task-1", ProjectID: "p", Status: persistence.TaskStatusFailed,
		LastErrorClass: &class,
	}
	r := &Renderer{
		Tasks: &rendererTaskRepo{task: task},
		Audits: &rendererAuditRepo{rows: []*persistence.ToolAuditEntry{
			{ToolName: "run_shell", ToolOutput: "exit 1: connection refused"},
		}},
	}
	res, err := r.Render(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(res.Summary, "Last tool run_shell output") {
		t.Errorf("expected tool-output proximate cause, got %q", res.Summary)
	}
}

// TestRender_ProximateCauseFromLastError verifies the third
// branch: no outcome detail, no tool output, but task.LastError
// is populated.
func TestRender_ProximateCauseFromLastError(t *testing.T) {
	class := persistence.TaskFailureClassRuntimeError
	errMsg := "panic in container entrypoint"
	task := &persistence.Task{
		ID: "task-1", ProjectID: "p", Status: persistence.TaskStatusFailed,
		LastErrorClass: &class,
		LastError:      &errMsg,
	}
	r := &Renderer{Tasks: &rendererTaskRepo{task: task}}
	res, err := r.Render(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(res.Summary, "Error text: panic in container") {
		t.Errorf("expected last-error proximate cause, got %q", res.Summary)
	}
}

// TestRender_ProximateCauseAbsent covers the no-evidence branch:
// nothing to say beyond the headline + next-action.
func TestRender_ProximateCauseAbsent(t *testing.T) {
	class := persistence.TaskFailureClassTimeout
	task := &persistence.Task{
		ID: "task-1", ProjectID: "p", Status: persistence.TaskStatusFailed,
		LastErrorClass: &class,
	}
	r := &Renderer{Tasks: &rendererTaskRepo{task: task}}
	res, err := r.Render(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// No proximate-cause sentence — but headline and next-action still present.
	if strings.Contains(res.Summary, "Proximate signal") || strings.Contains(res.Summary, "Last tool") {
		t.Errorf("unexpected proximate-cause sentence in empty-evidence path: %q", res.Summary)
	}
	if !strings.Contains(res.Summary, "Try first:") {
		t.Errorf("expected playbook next-action, got %q", res.Summary)
	}
}

// TestRender_PlaybookEntryWithoutSuggestions exercises the
// nextAction fallback when the playbook returns an entry but the
// Suggestions slice is empty.
func TestRender_PlaybookEntryWithoutSuggestions(t *testing.T) {
	// The playbook generic-fallback entry's Suggestions slice IS
	// populated, so to hit the empty-suggestions branch we have to
	// invent an entry inline. Use the renderer's lower-level
	// renderSummary directly with crafted Inputs.
	in := RenderedInputs{
		TaskID:         "task-1",
		TaskStatus:     "failed",
		LastErrorClass: persistence.TaskFailureClassLLMError,
		Playbook:       &playbook.Entry{Class: "LLM_ERROR" /* no Suggestions */},
	}
	got := renderSummary(&in)
	if !strings.Contains(got, "No playbook entry for class LLM_ERROR") {
		t.Errorf("expected empty-suggestions fallback, got %q", got)
	}
}

// ---------- nil/empty inputs and helpers ----------

// TestRenderSummary_NilInputsReturnsEmpty covers the nil-guard at
// the top of renderSummary.
func TestRenderSummary_NilInputsReturnsEmpty(t *testing.T) {
	if got := renderSummary(nil); got != "" {
		t.Errorf("nil inputs should yield empty string, got %q", got)
	}
}

// TestIsFailureOutcome locks in the failure-class classifier
// since the lastFailingStep walk depends on it.
func TestIsFailureOutcome(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"pending_validation", false},
		{"PENDING_VALIDATION", false}, // case-insensitive
		{"used", false},
		{"parse_error", true},
		{"schema_violation", true},
		{"refused", true},
		{"  hallucination  ", true},
	}
	for _, tc := range cases {
		if got := isFailureOutcome(tc.in); got != tc.want {
			t.Errorf("isFailureOutcome(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestTruncateRunes locks in the rune-aware truncation, which
// matters because user prompts and tool outputs carry UTF-8.
func TestTruncateRunes(t *testing.T) {
	if truncateRunes("  hi  ", 10) != "hi" {
		t.Error("should trim whitespace before length check")
	}
	if truncateRunes("hello", 3) != "hel…" {
		t.Errorf("ASCII truncate failed: %q", truncateRunes("hello", 3))
	}
	// 5 multi-byte runes; max=3 should yield "abc…" not bytes-split.
	got := truncateRunes("αβγδε", 3)
	if got != "αβγ…" {
		t.Errorf("rune truncate failed: %q", got)
	}
	// At-limit: no ellipsis.
	if got := truncateRunes("abcde", 5); got != "abcde" {
		t.Errorf("at-limit truncate failed: %q", got)
	}
}

// TestHumanizeClass covers every TaskFailureClass case so a new
// class added to persistence/models.go shows up here when
// extending the playbook + renderer.
func TestHumanizeClass(t *testing.T) {
	cases := map[string]string{
		persistence.TaskFailureClassLLMError:           "an LLM/gateway error",
		persistence.TaskFailureClassTimeout:            "a timeout",
		persistence.TaskFailureClassToolError:          "a tool error",
		persistence.TaskFailureClassToolIterationLimit: "the tool-iteration cap",
		persistence.TaskFailureClassInvalidOutput:      "an invalid-output gate",
		persistence.TaskFailureClassMergeFailed:        "a merge failure",
		persistence.TaskFailureClassGateFailed:         "a gate refusal",
		persistence.TaskFailureClassBudgetBlocked:      "the project budget cap",
		persistence.TaskFailureClassRateLimited:        "a rate limit",
		persistence.TaskFailureClassWorkflowRole:       "a missing workflow role",
		persistence.TaskFailureClassWorkflowCfg:        "a workflow config error",
		persistence.TaskFailureClassWorkflowDrift:      "workflow drift",
		persistence.TaskFailureClassStuckExecution:     "the stuck-execution watchdog",
		persistence.TaskFailureClassLeaseExpired:       "an expired lease",
		persistence.TaskFailureClassRuntimeError:       "a runtime error",
		persistence.TaskFailureClassCancelled:          "operator cancellation",
		persistence.TaskFailureClassOrphaned:           "an orphaned-task scan",
		persistence.TaskFailureClassSecretLeak:         "the secret-leak guard",
		persistence.TaskFailureClassUnknown:            "an unclassified error",
	}
	for class, want := range cases {
		if got := humanizeClass(class); got != want {
			t.Errorf("humanizeClass(%q) = %q, want %q", class, got, want)
		}
	}
}

// ---------- repo / source degradation ----------

// TestRender_TolerantOfRepoErrors verifies that errors from the
// non-required repos (execRepo, outcomes, audits, logs) degrade
// to empty sections rather than failing the whole render.
func TestRender_TolerantOfRepoErrors(t *testing.T) {
	class := persistence.TaskFailureClassTimeout
	task := &persistence.Task{
		ID: "task-1", ProjectID: "p", Status: persistence.TaskStatusFailed,
		LastErrorClass: &class,
	}
	r := NewRenderer(
		&rendererTaskRepo{task: task},
		&rendererExecRepo{err: errors.New("exec broken")},
		&rendererOutcomeRepo{err: errors.New("outcomes broken")},
		&rendererAuditRepo{err: errors.New("audits broken")},
		&rendererLogs{err: errors.New("logs broken")},
	)
	res, err := r.Render(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(res.Inputs.StepOutcomes) != 0 || len(res.Inputs.RecentTools) != 0 || res.Inputs.LogTail != "" {
		t.Errorf("expected empty optional sections on repo errors, got %+v", res.Inputs)
	}
}

// TestRender_WorkflowIDFromExecution covers the branch where the
// task has no WorkflowID but the execution row carries one.
func TestRender_WorkflowIDFromExecution(t *testing.T) {
	task := &persistence.Task{
		ID: "task-1", ProjectID: "p", Status: persistence.TaskStatusFailed,
	}
	exec := &persistence.Execution{ID: "e-1", WorkflowID: "wf-from-exec"}
	r := &Renderer{
		Tasks:      &rendererTaskRepo{task: task},
		Executions: &rendererExecRepo{exec: exec},
	}
	res, err := r.Render(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if res.Inputs.WorkflowID != "wf-from-exec" {
		t.Errorf("expected WorkflowID from exec, got %q", res.Inputs.WorkflowID)
	}
}

// TestRender_NilExecutionStillReturns covers GetByTaskID
// returning (nil, nil) — no execution row exists yet but the
// render should still succeed with a partial view.
func TestRender_NilExecutionStillReturns(t *testing.T) {
	task := &persistence.Task{ID: "t", ProjectID: "p", Status: persistence.TaskStatusFailed}
	r := &Renderer{
		Tasks:      &rendererTaskRepo{task: task},
		Executions: &rendererExecRepo{exec: nil},
	}
	if _, err := r.Render(context.Background(), "t"); err != nil {
		t.Fatalf("Render: %v", err)
	}
}

// ---------- determinism ----------

// TestRender_Deterministic asserts the same inputs yield the
// same summary across multiple invocations. This is the load-
// bearing property that lets us route the API endpoint through
// here instead of an LLM.
func TestRender_Deterministic(t *testing.T) {
	class := persistence.TaskFailureClassToolError
	task := &persistence.Task{
		ID: "task-1", ProjectID: "p", Status: persistence.TaskStatusFailed,
		LastErrorClass: &class,
	}
	mk := func() *Renderer {
		return &Renderer{
			Tasks:      &rendererTaskRepo{task: task},
			Executions: &rendererExecRepo{exec: &persistence.Execution{ID: "e"}},
			Outcomes: &rendererOutcomeRepo{rows: []*persistence.ExecutionStepOutcome{
				{StepID: "s1", Role: "coder", Model: "claude", Outcome: "refused", ErrorDetail: "policy"},
			}},
		}
	}
	r1, err1 := mk().Render(context.Background(), "task-1")
	r2, err2 := mk().Render(context.Background(), "task-1")
	if err1 != nil || err2 != nil {
		t.Fatalf("Render: %v %v", err1, err2)
	}
	if r1.Summary != r2.Summary {
		t.Errorf("renderer not deterministic:\n  r1=%q\n  r2=%q", r1.Summary, r2.Summary)
	}
}

// TestRender_LongLastErrorTruncated guards the rune-aware
// LastError truncation at the 1500-rune cap.
func TestRender_LongLastErrorTruncated(t *testing.T) {
	huge := strings.Repeat("x", 2000)
	task := &persistence.Task{
		ID: "t", ProjectID: "p", Status: persistence.TaskStatusFailed,
		LastError: &huge,
	}
	r := &Renderer{Tasks: &rendererTaskRepo{task: task}}
	res, err := r.Render(context.Background(), "t")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len([]rune(res.Inputs.LastError)) > 1502 { // 1500 + "…" = at most 1501
		t.Errorf("LastError not truncated: %d runes", len([]rune(res.Inputs.LastError)))
	}
}

// _ = time is to keep the import in case future tests need it
// (the rendererLogs stub doesn't take a duration arg today).
var _ = time.Second
