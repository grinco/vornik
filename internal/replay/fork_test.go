package replay

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"vornik.io/vornik/internal/persistence"
)

type capturingAuditInserter struct {
	rows []*persistence.AdminAuditEntry
	err  error
}

func (c *capturingAuditInserter) Insert(_ context.Context, e *persistence.AdminAuditEntry) error {
	if c.err != nil {
		return c.err
	}
	c.rows = append(c.rows, e)
	return nil
}

// metricValue reads one outcome label's counter value via the
// Prometheus testing API. Returns 0 when the label hasn't been
// touched yet.
func metricValue(t *testing.T, m *Metrics, outcome string) float64 {
	t.Helper()
	if m == nil || m.ForksTotal == nil {
		return 0
	}
	c, err := m.ForksTotal.GetMetricWithLabelValues(outcome)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues: %v", err)
	}
	var dst dto.Metric
	if err := c.Write(&dst); err != nil {
		t.Fatalf("metric write: %v", err)
	}
	if dst.Counter == nil {
		return 0
	}
	return dst.Counter.GetValue()
}

type capturingTaskCreator struct {
	created *persistence.Task
	err     error
}

func (c *capturingTaskCreator) Create(_ context.Context, t *persistence.Task) error {
	if c.err != nil {
		return c.err
	}
	c.created = t
	return nil
}

func newForkerWithFixture(t *testing.T) (*Forker, *capturingTaskCreator) {
	t.Helper()
	source := &persistence.Execution{
		ID:         "exec_src",
		TaskID:     "task_src",
		ProjectID:  "proj_1",
		WorkflowID: "wf_1",
		Status:     persistence.ExecutionStatusFailed,
	}
	outcomes := []*persistence.ExecutionStepOutcome{
		{ExecutionID: "exec_src", StepID: "research_1", RecordedAt: time.Now()},
		{ExecutionID: "exec_src", StepID: "summarise", RecordedAt: time.Now()},
	}
	creator := &capturingTaskCreator{}
	f := &Forker{
		Executions: &fakeExecGet{exec: source},
		Outcomes:   &fakeOutcomeList{rows: outcomes},
		Tasks:      creator,
		IDGenerator: func() string {
			return "task_fork_1"
		},
		Now: func() time.Time {
			return time.Date(2026, 5, 22, 10, 0, 0, 0, time.UTC)
		},
	}
	return f, creator
}

func TestFork_HappyPath_CreatesTaskWithForkPayload(t *testing.T) {
	f, creator := newForkerWithFixture(t)
	result, err := f.Fork(context.Background(), "exec_src", ForkRequest{
		StepID:         "summarise",
		PromptOverride: "this time, only summarise items from after 2026-05-01",
	})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if result == nil || result.TaskID != "task_fork_1" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.URL != "/ui/tasks/task_fork_1" {
		t.Errorf("url wrong: %q", result.URL)
	}

	if creator.created == nil {
		t.Fatal("expected task to be created")
	}
	task := creator.created
	if task.ProjectID != "proj_1" {
		t.Errorf("project id not inherited: %q", task.ProjectID)
	}
	if task.WorkflowID == nil || *task.WorkflowID != "wf_1" {
		t.Errorf("workflow id not inherited: %v", task.WorkflowID)
	}
	if task.CreationSource != persistence.TaskCreationSourceFork {
		t.Errorf("creation source wrong: %q", task.CreationSource)
	}
	if task.ParentTaskID == nil || *task.ParentTaskID != "task_src" {
		t.Errorf("parent task id wrong: %v", task.ParentTaskID)
	}
	if task.Status != persistence.TaskStatusQueued {
		t.Errorf("status wrong: %q", task.Status)
	}

	// Validate the payload envelope round-trips.
	target, err := ExtractForkTarget(task.Payload)
	if err != nil {
		t.Fatalf("ExtractForkTarget: %v", err)
	}
	if target == nil {
		t.Fatal("expected fork target in payload")
	}
	if target.SourceExecutionID != "exec_src" || target.StepID != "summarise" {
		t.Errorf("target wrong: %+v", target)
	}
	if target.PromptOverride != "this time, only summarise items from after 2026-05-01" {
		t.Errorf("override wrong: %q", target.PromptOverride)
	}
}

func TestFork_EmptyStepIDRejected(t *testing.T) {
	f, _ := newForkerWithFixture(t)
	_, err := f.Fork(context.Background(), "exec_src", ForkRequest{StepID: "  "})
	if !errors.Is(err, ErrForkValidation) {
		t.Fatalf("expected validation error, got %v", err)
	}
}

func TestFork_SourceNotFound(t *testing.T) {
	f, _ := newForkerWithFixture(t)
	_, err := f.Fork(context.Background(), "missing", ForkRequest{StepID: "summarise"})
	if !errors.Is(err, ErrForkSourceNotFound) {
		t.Fatalf("expected source-not-found, got %v", err)
	}
}

func TestFork_StepDidNotRunRejected(t *testing.T) {
	f, _ := newForkerWithFixture(t)
	_, err := f.Fork(context.Background(), "exec_src", ForkRequest{StepID: "never_ran"})
	if !errors.Is(err, ErrForkStepMissing) {
		t.Fatalf("expected step-missing, got %v", err)
	}
}

func TestFork_NilForkerErrors(t *testing.T) {
	var f *Forker
	_, err := f.Fork(context.Background(), "exec_src", ForkRequest{StepID: "summarise"})
	if err == nil {
		t.Fatal("expected nil-forker error")
	}
}

func TestFork_PartiallyWiredErrors(t *testing.T) {
	f := &Forker{Executions: &fakeExecGet{}}
	_, err := f.Fork(context.Background(), "exec_src", ForkRequest{StepID: "summarise"})
	if err == nil {
		t.Fatal("expected not-fully-wired error")
	}
}

func TestFork_OmittedPromptOverrideIsValid(t *testing.T) {
	f, creator := newForkerWithFixture(t)
	_, err := f.Fork(context.Background(), "exec_src", ForkRequest{StepID: "summarise"})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	target, _ := ExtractForkTarget(creator.created.Payload)
	if target == nil || target.PromptOverride != "" {
		t.Errorf("expected empty override, got %+v", target)
	}
}

func TestExtractForkTarget_NonForkPayloadIsNil(t *testing.T) {
	// A normal task payload without fork_target should produce nil.
	payload, _ := json.Marshal(map[string]any{
		"taskType": "research",
		"context":  map[string]any{"prompt": "hello"},
	})
	target, err := ExtractForkTarget(payload)
	if err != nil {
		t.Errorf("expected nil error for non-fork payload, got %v", err)
	}
	if target != nil {
		t.Errorf("expected nil target, got %+v", target)
	}
}

func TestExtractForkTarget_EmptyPayloadIsNil(t *testing.T) {
	target, err := ExtractForkTarget(nil)
	if err != nil || target != nil {
		t.Errorf("expected nil/nil for empty payload, got %+v / %v", target, err)
	}
}

func TestExtractForkTarget_MalformedReturnsNil(t *testing.T) {
	target, err := ExtractForkTarget([]byte("{not json"))
	if err != nil || target != nil {
		t.Errorf("malformed payload should be treated as 'not a fork', got %+v / %v", target, err)
	}
}

func TestExtractForkTarget_MissingFieldsErrors(t *testing.T) {
	// Envelope present but missing required fields.
	payload, _ := json.Marshal(map[string]any{
		ForkTargetPayloadKey: map[string]any{},
	})
	_, err := ExtractForkTarget(payload)
	if err == nil {
		t.Error("expected error for empty fork target")
	}
}

func TestFork_HappyPath_RecordsAuditAndMetrics(t *testing.T) {
	f, _ := newForkerWithFixture(t)
	audit := &capturingAuditInserter{}
	metrics := NewMetrics(prometheus.NewRegistry())
	f.AuditAdmin = audit
	f.Metrics = metrics

	_, err := f.Fork(context.Background(), "exec_src", ForkRequest{
		StepID:         "summarise",
		PromptOverride: "tighten the section on Q3",
	})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}

	if metricValue(t, metrics, forkOutcomeCreated) != 1 {
		t.Errorf("expected 1 created counter increment, got %.2f", metricValue(t, metrics, forkOutcomeCreated))
	}
	if len(audit.rows) != 1 {
		t.Fatalf("expected 1 audit row, got %d", len(audit.rows))
	}
	row := audit.rows[0]
	if row.Action != "execution.fork" {
		t.Errorf("action wrong: %q", row.Action)
	}
	if row.Target != "task_fork_1" {
		t.Errorf("target wrong: %q", row.Target)
	}
	if row.Source != "api" {
		t.Errorf("source wrong: %q", row.Source)
	}
	// Before should contain the source execution ID + step.
	var before map[string]string
	_ = json.Unmarshal([]byte(row.Before), &before)
	if before["source_execution_id"] != "exec_src" || before["step_id"] != "summarise" {
		t.Errorf("before payload wrong: %+v", before)
	}
	var after map[string]string
	_ = json.Unmarshal([]byte(row.After), &after)
	if after["new_task_id"] != "task_fork_1" || after["prompt_override"] != "tighten the section on Q3" {
		t.Errorf("after payload wrong: %+v", after)
	}
}

func TestFork_AuditFailureDoesNotAbortFork(t *testing.T) {
	f, _ := newForkerWithFixture(t)
	f.AuditAdmin = &capturingAuditInserter{err: errors.New("audit table down")}
	result, err := f.Fork(context.Background(), "exec_src", ForkRequest{StepID: "summarise"})
	if err != nil {
		t.Fatalf("audit failure should not bubble up to fork: %v", err)
	}
	if result == nil || result.TaskID == "" {
		t.Error("fork should succeed despite audit failure")
	}
}

func TestFork_MetricsOnFailures(t *testing.T) {
	cases := []struct {
		name        string
		req         ForkRequest
		execID      string
		wantOutcome string
	}{
		{"validation", ForkRequest{StepID: "  "}, "exec_src", forkOutcomeValidationFail},
		{"source not found", ForkRequest{StepID: "summarise"}, "missing", forkOutcomeSourceNotFound},
		{"step missing", ForkRequest{StepID: "never_ran"}, "exec_src", forkOutcomeStepMissing},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, _ := newForkerWithFixture(t)
			metrics := NewMetrics(prometheus.NewRegistry())
			f.Metrics = metrics
			_, _ = f.Fork(context.Background(), c.execID, c.req)
			if metricValue(t, metrics, c.wantOutcome) != 1 {
				t.Errorf("expected 1 %s increment, got %.2f", c.wantOutcome, metricValue(t, metrics, c.wantOutcome))
			}
			if metricValue(t, metrics, forkOutcomeCreated) != 0 {
				t.Errorf("created counter should be 0 on refusal, got %.2f", metricValue(t, metrics, forkOutcomeCreated))
			}
		})
	}
}

func TestExtractForkTarget_ParseErrorPropagates(t *testing.T) {
	// fork_target is present but unparseable into ForkTarget.
	payload := []byte(`{"fork_target": "not an object"}`)
	_, err := ExtractForkTarget(payload)
	if err == nil {
		t.Error("expected parse error")
	}
}
