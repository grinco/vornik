package quality

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

type capturingScoreRepo struct {
	rows []*persistence.ExecutionQualityScore
}

func (c *capturingScoreRepo) Upsert(_ context.Context, row *persistence.ExecutionQualityScore) error {
	c.rows = append(c.rows, row)
	return nil
}

func (c *capturingScoreRepo) GetByExecution(context.Context, string) (*persistence.ExecutionQualityScore, error) {
	return nil, persistence.ErrNotFound
}

func (c *capturingScoreRepo) List(context.Context, persistence.ExecutionQualityScoreFilter) ([]*persistence.ExecutionQualityScore, error) {
	return nil, nil
}

func (c *capturingScoreRepo) ListPendingTerminal(context.Context, int) ([]*persistence.Execution, error) {
	return nil, nil
}

func (c *capturingScoreRepo) PendingTerminalStats(context.Context, []string) (persistence.ExecutionQualityPendingStats, error) {
	return persistence.ExecutionQualityPendingStats{}, nil
}

// stubOutcomes serves the step outcomes contract_satisfaction reads. A
// snapshot cannot stand in for these: it records only steps that SUCCEEDED, so
// it cannot tell a step that ran and failed from one never reached, and those
// are opposite facts for this metric.
type stubOutcomes struct{ byExec map[string]map[string]string }

func (s stubOutcomes) OutcomesByExecution(_ context.Context, executionID string) (map[string]string, error) {
	return s.byExec[executionID], nil
}

// contract_satisfaction exists so a dashboard can show numbers for workflows a
// small model can actually satisfy: "write artifacts/out/findings.md" is a
// promise it can keep, while pinned_case_validation needs testing.cases[] that
// the local 27B emitted 15% of the time. Wiring is what makes it real — the
// scorer shipped 2026-08-18 with tests and nothing calling it.
func TestPublish_ContractSatisfactionIsScoredAndRecorded(t *testing.T) {
	repo := &capturingScoreRepo{}
	pub := NewExecutionScorePublisher(repo, func() time.Time { return time.Unix(0, 0).UTC() })
	pub.WithStepOutcomes(stubOutcomes{byExec: map[string]map[string]string{
		"exec-1": {"gather": "ok", "verify": "failed"},
	}})

	err := pub.Publish(context.Background(), &persistence.Execution{
		ID: "exec-1", ProjectID: "p", TaskID: "t", WorkflowID: "wf",
		Status: persistence.ExecutionStatusCompleted,
		WorkflowSnapshot: []byte(`{"ID":"wf","QualityScoring":{"kind":"contract_satisfaction"},` +
			`"Steps":{"gather":{"RequireOutputGlob":"out/a.md"},"verify":{"RequireOutputGlob":"out/b.md"}}}`),
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if len(repo.rows) != 1 {
		t.Fatalf("expected one row, got %d", len(repo.rows))
	}
	row := repo.rows[0]
	if row.Kind != string(ScoreKindContractSatisfaction) {
		t.Errorf("kind = %q, want contract_satisfaction", row.Kind)
	}
	if row.Status != string(ScoreStatusScored) {
		t.Errorf("status = %q, want scored", row.Status)
	}
	if row.Score == nil || *row.Score != 0.5 {
		t.Errorf("score = %v, want 0.5 — one of two declared obligations was met", row.Score)
	}
	if row.PassedCaseCount != 1 || row.PinnedCaseCount != 2 {
		t.Errorf("counts = %d/%d, want 1/2", row.PassedCaseCount, row.PinnedCaseCount)
	}
}

// A workflow that declares no scoring policy is untouched by the new kind — the
// overwhelming majority of production executions, which must stay
// not_applicable rather than acquiring a manufactured score.
func TestPublish_NoPolicyStaysNotApplicable(t *testing.T) {
	repo := &capturingScoreRepo{}
	pub := NewExecutionScorePublisher(repo, func() time.Time { return time.Unix(0, 0).UTC() })
	if err := pub.Publish(context.Background(), &persistence.Execution{
		ID: "exec-2", ProjectID: "p", TaskID: "t", WorkflowID: "wf",
		Status:           persistence.ExecutionStatusCompleted,
		WorkflowSnapshot: []byte(`{"ID":"wf","Steps":{"gather":{}}}`),
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got := repo.rows[0].Status; got != string(ScoreStatusNotApplicable) {
		t.Errorf("status = %q, want not_applicable", got)
	}
}
