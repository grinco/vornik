package replay

import (
	"context"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ---- in-memory fakes satisfying the narrow interfaces -------------

type fakeExecGet struct {
	exec *persistence.Execution
	err  error
}

func (f *fakeExecGet) Get(_ context.Context, id string) (*persistence.Execution, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.exec == nil || f.exec.ID != id {
		return nil, persistence.ErrNotFound
	}
	return f.exec, nil
}

type fakeTaskGet struct {
	task *persistence.Task
}

func (f *fakeTaskGet) Get(_ context.Context, id string) (*persistence.Task, error) {
	if f.task == nil || f.task.ID != id {
		return nil, persistence.ErrNotFound
	}
	return f.task, nil
}

type fakeOutcomeList struct {
	rows []*persistence.ExecutionStepOutcome
}

func (f *fakeOutcomeList) List(context.Context, persistence.ExecutionStepOutcomeFilter) ([]*persistence.ExecutionStepOutcome, error) {
	return f.rows, nil
}

type fakeLLMUsageList struct {
	rows []*persistence.TaskLLMUsage
}

func (f *fakeLLMUsageList) List(context.Context, persistence.TaskLLMUsageFilter) ([]*persistence.TaskLLMUsage, error) {
	return f.rows, nil
}

type fakeToolAuditList struct {
	rows []*persistence.ToolAuditEntry
}

func (f *fakeToolAuditList) List(context.Context, persistence.ToolAuditFilter) ([]*persistence.ToolAuditEntry, error) {
	return f.rows, nil
}

type fakeArtifactList struct {
	rows []*persistence.Artifact
}

func (f *fakeArtifactList) List(context.Context, persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
	return f.rows, nil
}

type fakeMessageList struct {
	rows []*persistence.TaskMessage
}

func (f *fakeMessageList) List(context.Context, persistence.TaskMessageFilter) ([]*persistence.TaskMessage, error) {
	return f.rows, nil
}

type fakePostMortemGet struct {
	pm *persistence.TaskPostMortem
}

func (f *fakePostMortemGet) Get(_ context.Context, taskID string) (*persistence.TaskPostMortem, error) {
	if f.pm == nil || f.pm.TaskID != taskID {
		return nil, persistence.ErrNotFound
	}
	return f.pm, nil
}

// ---- tests --------------------------------------------------------

func newBuilderWithFixture(t *testing.T) *Builder {
	t.Helper()
	execID := "exec_1"
	taskID := "task_1"
	exec := &persistence.Execution{
		ID:        execID,
		TaskID:    taskID,
		ProjectID: "proj_1",
		Status:    persistence.ExecutionStatusFailed,
	}
	task := &persistence.Task{
		ID:        taskID,
		ProjectID: "proj_1",
	}
	t0 := time.Date(2026, 5, 22, 10, 0, 0, 0, time.UTC)
	step1Dur := int64(1200)
	step2Dur := int64(8400)
	outcomes := []*persistence.ExecutionStepOutcome{
		{
			ID: "out2", ExecutionID: execID, TaskID: taskID,
			StepID: "research_1", Role: "scout", Model: "m1",
			Outcome: "ok", DurationMS: &step1Dur,
			RecordedAt: t0.Add(2 * time.Second),
		},
		{
			ID: "out1", ExecutionID: execID, TaskID: taskID,
			StepID: "summarise", Role: "writer", Model: "m1",
			Outcome: "hallucination", ErrorClass: "fabricated_file",
			ErrorDetail: "claimed file 'news.md' which doesn't exist",
			DurationMS:  &step2Dur,
			RecordedAt:  t0.Add(10 * time.Second),
		},
	}
	llmRows := []*persistence.TaskLLMUsage{
		{StepID: "research_1", Model: "m1", Role: "scout", PromptTokens: 100, CompletionTokens: 50, CostUSD: 0.10, Iterations: 1, Source: "workflow_step"},
		{StepID: "summarise", Model: "m1", Role: "writer", PromptTokens: 800, CompletionTokens: 200, CostUSD: 0.50, Iterations: 2, Source: "workflow_step"},
	}
	toolRows := []*persistence.ToolAuditEntry{
		{ExecutionID: execID, StepID: "research_1", ToolName: "web_fetch", ToolInput: `{"url":"https://x"}`, ToolOutput: `{"status":200}`, CreatedAt: t0.Add(1 * time.Second)},
	}
	tID := taskID
	size := int64(1024)
	hash := "sha256:abc"
	artifacts := []*persistence.Artifact{
		{ID: "art_1", TaskID: &tID, ExecutionID: &execID, Name: "research.md", SizeBytes: &size, ContentHashSHA256: &hash, CreatedAt: t0.Add(3 * time.Second)},
	}
	pm := &persistence.TaskPostMortem{
		TaskID:  taskID,
		Summary: "writer fabricated news.md",
	}
	return &Builder{
		Executions:  &fakeExecGet{exec: exec},
		Tasks:       &fakeTaskGet{task: task},
		Outcomes:    &fakeOutcomeList{rows: outcomes},
		LLMUsage:    &fakeLLMUsageList{rows: llmRows},
		ToolAudit:   &fakeToolAuditList{rows: toolRows},
		Artifacts:   &fakeArtifactList{rows: artifacts},
		Messages:    &fakeMessageList{},
		PostMortems: &fakePostMortemGet{pm: pm},
	}
}

func TestBuilder_Build_HappyPath(t *testing.T) {
	b := newBuilderWithFixture(t)
	tl, err := b.Build(context.Background(), "exec_1")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if tl == nil {
		t.Fatal("expected timeline, got nil")
	}
	if tl.Execution == nil || tl.Execution.ID != "exec_1" {
		t.Errorf("execution not populated: %+v", tl.Execution)
	}
	if tl.Task == nil || tl.Task.ID != "task_1" {
		t.Errorf("task not populated: %+v", tl.Task)
	}
	if tl.PostMortem == nil {
		t.Error("post-mortem should be populated")
	}
	if len(tl.Steps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(tl.Steps))
	}
	// Steps ordered by recorded_at ASC — research_1 (t+2s) before
	// summarise (t+10s).
	if tl.Steps[0].StepID != "research_1" || tl.Steps[1].StepID != "summarise" {
		t.Errorf("steps wrong order: %v", []string{tl.Steps[0].StepID, tl.Steps[1].StepID})
	}
	// Research step picked up its single tool call.
	if len(tl.Steps[0].ToolCalls) != 1 || tl.Steps[0].ToolCalls[0].ToolName != "web_fetch" {
		t.Errorf("research step missing tool call: %+v", tl.Steps[0].ToolCalls)
	}
	// Summarise step picked up its hallucination outcome.
	if tl.Steps[1].Outcome != "hallucination" {
		t.Errorf("summarise outcome wrong: %q", tl.Steps[1].Outcome)
	}
	if tl.Steps[1].CostUSD != 0.50 {
		t.Errorf("summarise cost wrong: %f", tl.Steps[1].CostUSD)
	}
	if tl.Steps[1].Iterations != 2 {
		t.Errorf("summarise iterations wrong: %d", tl.Steps[1].Iterations)
	}
	// Totals roll up.
	if tl.Totals.StepCount != 2 || tl.Totals.OkSteps != 1 || tl.Totals.FailSteps != 1 {
		t.Errorf("totals wrong: %+v", tl.Totals)
	}
	if tl.Totals.Artifacts != 1 {
		t.Errorf("artifact total wrong: %d", tl.Totals.Artifacts)
	}
	if tl.Totals.ToolCalls != 1 {
		t.Errorf("tool call total wrong: %d", tl.Totals.ToolCalls)
	}
	if tl.Totals.CostUSD != 0.60 {
		t.Errorf("cost total wrong: %f", tl.Totals.CostUSD)
	}
	// Artifacts at the timeline level (v1 doesn't attribute per-step).
	if len(tl.Artifacts) != 1 || tl.Artifacts[0].Filename != "research.md" {
		t.Errorf("artifacts not rendered: %+v", tl.Artifacts)
	}
	if tl.Artifacts[0].SizeBytes != 1024 || tl.Artifacts[0].Hash != "sha256:abc" {
		t.Errorf("artifact fields wrong: %+v", tl.Artifacts[0])
	}
}

func TestBuilder_Build_ExecutionNotFound(t *testing.T) {
	b := newBuilderWithFixture(t)
	_, err := b.Build(context.Background(), "missing")
	if !errors.Is(err, ErrExecutionNotFound) {
		t.Fatalf("expected ErrExecutionNotFound, got %v", err)
	}
}

func TestBuilder_Build_PostMortemOptional(t *testing.T) {
	b := newBuilderWithFixture(t)
	b.PostMortems = nil
	tl, err := b.Build(context.Background(), "exec_1")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if tl.PostMortem != nil {
		t.Errorf("expected nil post-mortem when repo unwired, got %+v", tl.PostMortem)
	}
}

func TestBuilder_Build_PostMortemMissingNotFatal(t *testing.T) {
	b := newBuilderWithFixture(t)
	b.PostMortems = &fakePostMortemGet{} // no row for the task
	tl, err := b.Build(context.Background(), "exec_1")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if tl.PostMortem != nil {
		t.Errorf("expected nil post-mortem when not yet generated, got %+v", tl.PostMortem)
	}
}

func TestBuilder_Build_NilBuilderErrors(t *testing.T) {
	var b *Builder
	_, err := b.Build(context.Background(), "exec_1")
	if err == nil {
		t.Fatal("expected error on nil builder")
	}
}

func TestBuilder_Build_PartiallyWiredErrors(t *testing.T) {
	b := &Builder{Executions: &fakeExecGet{}}
	_, err := b.Build(context.Background(), "exec_1")
	if err == nil {
		t.Fatal("expected error when repos missing")
	}
}

// fakeExecGetMulti supports lineage tests where the Builder walks
// multiple ancestor executions back from the current one.
type fakeExecGetMulti struct {
	execs map[string]*persistence.Execution
}

func (f *fakeExecGetMulti) Get(_ context.Context, id string) (*persistence.Execution, error) {
	e, ok := f.execs[id]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	return e, nil
}

func TestBuilder_Lineage_WalksChain(t *testing.T) {
	// Three-deep chain: original → fork_1 → fork_2 (the "you").
	mkExec := func(id, parent, step string) *persistence.Execution {
		e := &persistence.Execution{ID: id, TaskID: "task_" + id, ProjectID: "p"}
		if parent != "" {
			pid := parent
			e.ParentExecutionID = &pid
		}
		if step != "" {
			s := step
			e.ForkedFromStepID = &s
		}
		return e
	}
	multi := &fakeExecGetMulti{execs: map[string]*persistence.Execution{
		"original": mkExec("original", "", ""),
		"fork_1":   mkExec("fork_1", "original", "research_1"),
		"fork_2":   mkExec("fork_2", "fork_1", "summarise"),
	}}
	b := &Builder{
		Executions:  multi,
		Tasks:       &fakeTaskGet{task: &persistence.Task{ID: "task_fork_2", ProjectID: "p"}},
		Outcomes:    &fakeOutcomeList{},
		LLMUsage:    &fakeLLMUsageList{},
		ToolAudit:   &fakeToolAuditList{},
		Artifacts:   &fakeArtifactList{},
		Messages:    &fakeMessageList{},
		PostMortems: nil,
	}
	// Re-point Tasks.Get to fork_2's task_id so the build path
	// succeeds. fakeTaskGet only returns the one task.
	tl, err := b.Build(context.Background(), "fork_2")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(tl.Lineage) != 2 {
		t.Fatalf("expected 2 ancestors, got %d: %+v", len(tl.Lineage), tl.Lineage)
	}
	// Oldest first.
	if tl.Lineage[0].ExecutionID != "original" || tl.Lineage[1].ExecutionID != "fork_1" {
		t.Errorf("lineage order wrong: %+v", tl.Lineage)
	}
	// fork_1's hop should carry its ForkedFromStep.
	if tl.Lineage[1].ForkedFromStep != "research_1" {
		t.Errorf("fork_1 step missing: %+v", tl.Lineage[1])
	}
}

func TestBuilder_Lineage_NonForkReturnsEmpty(t *testing.T) {
	b := newBuilderWithFixture(t)
	tl, err := b.Build(context.Background(), "exec_1")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(tl.Lineage) != 0 {
		t.Errorf("non-fork should have empty lineage, got %+v", tl.Lineage)
	}
}

func TestBuilder_Lineage_StopsAtMissingAncestor(t *testing.T) {
	mkExec := func(id, parent string) *persistence.Execution {
		e := &persistence.Execution{ID: id, TaskID: "task_" + id, ProjectID: "p"}
		if parent != "" {
			pid := parent
			e.ParentExecutionID = &pid
		}
		return e
	}
	// Chain: fork → (missing) → original. Walker stops at the gap.
	multi := &fakeExecGetMulti{execs: map[string]*persistence.Execution{
		"fork": mkExec("fork", "deleted"),
	}}
	b := &Builder{
		Executions:  multi,
		Tasks:       &fakeTaskGet{task: &persistence.Task{ID: "task_fork", ProjectID: "p"}},
		Outcomes:    &fakeOutcomeList{},
		LLMUsage:    &fakeLLMUsageList{},
		ToolAudit:   &fakeToolAuditList{},
		Artifacts:   &fakeArtifactList{},
		Messages:    &fakeMessageList{},
		PostMortems: nil,
	}
	tl, err := b.Build(context.Background(), "fork")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(tl.Lineage) != 0 {
		t.Errorf("expected empty lineage when ancestor missing, got %+v", tl.Lineage)
	}
}

func TestBuilder_Build_MessageStepFilter(t *testing.T) {
	// Messages with execution_id matching the requested execution
	// are kept; messages from another execution on the same task
	// are dropped. The step-id-from-metadata grouping then attaches
	// remaining messages to the right step.
	b := newBuilderWithFixture(t)
	execID := "exec_1"
	otherExec := "exec_2"
	b.Messages = &fakeMessageList{rows: []*persistence.TaskMessage{
		{ID: "m1", TaskID: "task_1", ExecutionID: &execID, AuthorKind: "lead", MessageKind: "note", Content: "in-execution note", Metadata: []byte(`{"step_id":"research_1"}`)},
		{ID: "m2", TaskID: "task_1", ExecutionID: &otherExec, AuthorKind: "lead", MessageKind: "note", Content: "wrong-execution note", Metadata: []byte(`{"step_id":"research_1"}`)},
		{ID: "m3", TaskID: "task_1", ExecutionID: &execID, AuthorKind: "operator", MessageKind: "message", Content: "no step pin"},
	}}
	tl, err := b.Build(context.Background(), execID)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// Only m1 should attach to research_1; m2 is wrong execution,
	// m3 has no step_id in metadata.
	var found *Message
	for i := range tl.Steps[0].Messages {
		if tl.Steps[0].Messages[i].ID == "m1" {
			found = &tl.Steps[0].Messages[i]
		}
	}
	if found == nil {
		t.Errorf("research_1 didn't pick up m1: %+v", tl.Steps[0].Messages)
	}
	for _, s := range tl.Steps {
		for _, m := range s.Messages {
			if m.ID == "m2" {
				t.Errorf("wrong-execution message leaked into step %s", s.StepID)
			}
		}
	}
}

func TestTruncateForRender(t *testing.T) {
	short := "small"
	got, trunc := truncateForRender(short)
	if got != short || trunc {
		t.Errorf("short string mutated: got=%q trunc=%v", got, trunc)
	}
	long := make([]byte, RenderLimit+100)
	for i := range long {
		long[i] = 'x'
	}
	got, trunc = truncateForRender(string(long))
	if len(got) != RenderLimit || !trunc {
		t.Errorf("long string not truncated correctly: len=%d trunc=%v", len(got), trunc)
	}
}

func TestExtractStepID(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty bytes", "", ""},
		{"missing field", `{"other":"x"}`, ""},
		{"present", `{"step_id":"research_2","other":"x"}`, "research_2"},
		{"malformed", `{not json`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractStepID([]byte(c.in))
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestAggregateLLMCalls_CollapsesByModelRole(t *testing.T) {
	rows := []*persistence.TaskLLMUsage{
		{Model: "m1", Role: "scout", PromptTokens: 100, CompletionTokens: 50, CostUSD: 0.10, Iterations: 1},
		{Model: "m1", Role: "scout", PromptTokens: 50, CompletionTokens: 25, CostUSD: 0.05, Iterations: 1},
		{Model: "m2", Role: "scout", PromptTokens: 200, CompletionTokens: 0, CostUSD: 0.20, Iterations: 1},
	}
	got := aggregateLLMCalls(rows)
	if len(got) != 2 {
		t.Fatalf("expected 2 collapsed rows, got %d: %+v", len(got), got)
	}
	// m1 collapsed: 150p + 75c + $0.15 + 2 iterations
	var m1 *LLMCall
	for i := range got {
		if got[i].Model == "m1" {
			m1 = &got[i]
		}
	}
	if m1 == nil || m1.PromptTokens != 150 || m1.CompletionTokens != 75 || m1.Iterations != 2 || !almostEqual(m1.CostUSD, 0.15) {
		t.Errorf("m1 aggregation wrong: %+v", m1)
	}
}

func almostEqual(a, b float64) bool {
	const epsilon = 1e-9
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < epsilon
}
