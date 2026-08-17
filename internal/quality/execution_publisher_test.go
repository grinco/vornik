package quality

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"vornik.io/vornik/internal/persistence"
)

type fakeExecutionScoreRepo struct {
	pending []*persistence.Execution
	written []*persistence.ExecutionQualityScore
	failID  string
	stats   persistence.ExecutionQualityPendingStats
}

func (f *fakeExecutionScoreRepo) Upsert(_ context.Context, score *persistence.ExecutionQualityScore) error {
	if score.ExecutionID == f.failID {
		return errors.New("write unavailable")
	}
	written := *score
	f.written = append(f.written, &written)
	return nil
}
func (f *fakeExecutionScoreRepo) GetByExecution(context.Context, string) (*persistence.ExecutionQualityScore, error) {
	return nil, persistence.ErrNotFound
}
func (f *fakeExecutionScoreRepo) List(context.Context, persistence.ExecutionQualityScoreFilter) ([]*persistence.ExecutionQualityScore, error) {
	return nil, nil
}
func (f *fakeExecutionScoreRepo) ListPendingTerminal(context.Context, int) ([]*persistence.Execution, error) {
	return f.pending, nil
}
func (f *fakeExecutionScoreRepo) PendingTerminalStats(context.Context, []string) (persistence.ExecutionQualityPendingStats, error) {
	return f.stats, nil
}

func terminalExecution(id string, policy *ScoringPolicy, state []byte) *persistence.Execution {
	return &persistence.Execution{
		ID: id, TaskID: "task-" + id, ProjectID: "project-" + id,
		WorkflowID: "workflow-" + id, WorkflowRevision: "rev-" + id,
		WorkflowSnapshot: workflowSnapshotNoTest(policy), StateSnapshot: state,
		Status: persistence.ExecutionStatusCompleted,
	}
}

func workflowSnapshotNoTest(policy *ScoringPolicy) []byte {
	b, _ := json.Marshal(struct {
		QualityScoring *ScoringPolicy `json:"qualityScoring,omitempty"`
	}{QualityScoring: policy})
	return b
}

func TestExecutionScorePublisher_PublishesEveryVerdictShape(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 30, 0, 0, time.UTC)
	repo := &fakeExecutionScoreRepo{}
	publisher := NewExecutionScorePublisher(repo, func() time.Time { return now })

	scored := terminalExecution("scored", pinnedPolicy(), scoreSnapshot(t, []string{"a", "b"}, 2,
		[]PinnedCaseEvidence{{ID: "a", Status: "passed"}}))
	if err := publisher.Publish(context.Background(), scored); err != nil {
		t.Fatalf("publish scored: %v", err)
	}
	notApplicable := terminalExecution("na", nil, nil)
	if err := publisher.Publish(context.Background(), notApplicable); err != nil {
		t.Fatalf("publish not-applicable: %v", err)
	}
	corrupt := terminalExecution("corrupt", pinnedPolicy(), []byte(`{"stepResults":`))
	if err := publisher.Publish(context.Background(), corrupt); err != nil {
		t.Fatalf("production corrupt evidence must still publish a row: %v", err)
	}

	if len(repo.written) != 3 {
		t.Fatalf("written rows = %d, want 3", len(repo.written))
	}
	if got := repo.written[0]; got.Status != string(ScoreStatusScored) || got.Score == nil || *got.Score != 0.5 ||
		got.ProjectID != scored.ProjectID || got.TaskID != scored.TaskID || got.WorkflowRevision != scored.WorkflowRevision ||
		got.PassedCaseCount != 1 || got.PinnedCaseCount != 2 || got.ScoringPolicySHA == "" || got.ScorerVersion != ExecutionScorerVersion {
		t.Fatalf("scored row = %+v", got)
	}
	if got := repo.written[1]; got.Status != string(ScoreStatusNotApplicable) || got.Score != nil {
		t.Fatalf("not-applicable row = %+v", got)
	}
	if got := repo.written[2]; got.Status != string(ScoreStatusInvalidEvidence) || got.Score == nil || *got.Score != 0 ||
		got.Diagnostic != DiagnosticCorruptStateSnapshot {
		t.Fatalf("corrupt row = %+v", got)
	}
}

func TestExecutionScorePublisher_ReconcileContinuesAfterPerRowFailure(t *testing.T) {
	repo := &fakeExecutionScoreRepo{failID: "bad"}
	repo.pending = []*persistence.Execution{
		terminalExecution("bad", nil, nil),
		terminalExecution("good", nil, nil),
	}
	publisher := NewExecutionScorePublisher(repo, time.Now)
	result, err := publisher.Reconcile(context.Background(), 100)
	if err == nil || result.Selected != 2 || result.Published != 1 || result.Failed != 1 {
		t.Fatalf("Reconcile = %+v, %v", result, err)
	}
	if len(repo.written) != 1 || repo.written[0].ExecutionID != "good" {
		t.Fatalf("failure starved later execution: %#v", repo.written)
	}
}

func TestExecutionScorePublisher_RefusesNonTerminalWithoutMutatingIt(t *testing.T) {
	repo := &fakeExecutionScoreRepo{}
	exec := terminalExecution("running", nil, nil)
	exec.Status = persistence.ExecutionStatusRunning
	publisher := NewExecutionScorePublisher(repo, time.Now)
	if err := publisher.Publish(context.Background(), exec); err == nil {
		t.Fatal("non-terminal execution must not be scored")
	}
	if exec.Status != persistence.ExecutionStatusRunning || len(repo.written) != 0 {
		t.Fatalf("publisher changed lifecycle state: exec=%+v rows=%d", exec, len(repo.written))
	}
}

func TestExecutionScorePublisher_CorruptWorkflowSnapshotIsVisibleInvalidEvidence(t *testing.T) {
	repo := &fakeExecutionScoreRepo{}
	exec := terminalExecution("wf-corrupt", nil, nil)
	exec.WorkflowSnapshot = []byte(`{"qualityScoring":`)
	publisher := NewExecutionScorePublisher(repo, time.Now)
	if err := publisher.Publish(context.Background(), exec); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if len(repo.written) != 1 || repo.written[0].Diagnostic != DiagnosticCorruptWorkflowSnapshot ||
		repo.written[0].Status != string(ScoreStatusInvalidEvidence) {
		t.Fatalf("corrupt workflow snapshot vanished: %#v", repo.written)
	}
}

func TestExecutionScorePublisher_UnsupportedPinnedPolicyIsVisibleInvalidEvidence(t *testing.T) {
	repo := &fakeExecutionScoreRepo{}
	exec := terminalExecution("wf-unsupported", nil, nil)
	exec.WorkflowSnapshot = []byte(`{"qualityScoring":{"kind":"future_rubric","producerStep":"a","verifierStep":"b"}}`)
	publisher := NewExecutionScorePublisher(repo, time.Now)
	if err := publisher.Publish(context.Background(), exec); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if len(repo.written) != 1 || repo.written[0].Diagnostic != DiagnosticUnsupportedScorePolicy ||
		repo.written[0].Status != string(ScoreStatusInvalidEvidence) {
		t.Fatalf("unsupported policy vanished: %#v", repo.written)
	}
}

func TestExecutionScorePublisher_PublishesAlertableBacklogMetrics(t *testing.T) {
	oldest := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	now := oldest.Add(5 * time.Minute)
	repo := &fakeExecutionScoreRepo{
		pending: []*persistence.Execution{terminalExecution("good", nil, nil)},
		stats:   persistence.ExecutionQualityPendingStats{Count: 3, OldestAt: &oldest},
	}
	reg := prometheus.NewRegistry()
	metrics := NewExecutionScoreMetrics(reg)
	publisher := NewExecutionScorePublisher(repo, func() time.Time { return now }, metrics)
	if _, err := publisher.Reconcile(context.Background(), 100); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := testutil.ToFloat64(metrics.PublicationPending); got != 3 {
		t.Fatalf("pending gauge = %v, want 3", got)
	}
	if got := testutil.ToFloat64(metrics.OldestPendingSeconds); got != 300 {
		t.Fatalf("oldest-pending age = %v, want 300", got)
	}
	if got := testutil.ToFloat64(metrics.WritesTotal.WithLabelValues("", "not_applicable")); got != 1 {
		t.Fatalf("successful writes counter = %v, want 1", got)
	}
}
