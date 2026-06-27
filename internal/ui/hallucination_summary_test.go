package ui

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// fakeOutcomeRepo stubs ExecutionStepOutcomeRepository for the
// hallucination-summary signals branch.
type fakeOutcomeRepo struct {
	rows    []*persistence.ExecutionStepOutcome
	listErr error
}

func (f *fakeOutcomeRepo) Record(context.Context, *persistence.ExecutionStepOutcome) error {
	return nil
}
func (f *fakeOutcomeRepo) Finalize(context.Context, string, string, string, string, *string) error {
	return nil
}
func (f *fakeOutcomeRepo) FinalizePending(context.Context, string, string, string, string, string, *string) (string, string, error) {
	return "", "", nil
}
func (f *fakeOutcomeRepo) SweepPending(context.Context, string, string) ([]persistence.SweepResult, error) {
	return nil, nil
}
func (f *fakeOutcomeRepo) List(context.Context, persistence.ExecutionStepOutcomeFilter) ([]*persistence.ExecutionStepOutcome, error) {
	return f.rows, f.listErr
}
func (f *fakeOutcomeRepo) SupersedeAfter(context.Context, string, time.Time) (int64, error) {
	return 0, nil
}
func (f *fakeOutcomeRepo) CountByRoleModelOutcome(context.Context, string, time.Time, time.Time, string) ([]persistence.RoleModelOutcomeCount, error) {
	return nil, nil
}

// extendedJudgeVerdictRepo extends the basic fake with ListRecent
// returning seeded verdicts.
type extendedJudgeVerdictRepo struct {
	fakeJudgeVerdictRepo
	listRows []*persistence.TaskJudgeVerdict
	listErr  error
}

func (e *extendedJudgeVerdictRepo) ListRecent(context.Context, string, int) ([]*persistence.TaskJudgeVerdict, error) {
	return e.listRows, e.listErr
}

func TestComputeHallucinationSummary_NoReposReturnsEmpty(t *testing.T) {
	srv := NewServer()
	project := &registry.Project{
		HallucinationJudge: registry.ProjectHallucinationJudge{Enabled: true, Model: "judge-haiku"},
		Verifiers: []map[string]any{
			{"name": "fact_check"},
		},
	}
	got := srv.computeHallucinationSummary(context.Background(), "p1", project)
	assert.True(t, got.JudgeEnabled, "config fields should still populate")
	assert.Equal(t, "judge-haiku", got.JudgeModel)
	assert.Equal(t, 1, got.VerifierCount)
	assert.Equal(t, 0, got.VerdictsPass)
}

func TestComputeHallucinationSummary_VerdictsTallied(t *testing.T) {
	now := time.Now().UTC()
	srv := NewServer(WithJudgeVerdictRepository(&extendedJudgeVerdictRepo{
		listRows: []*persistence.TaskJudgeVerdict{
			{TaskID: "task-1", Verdict: persistence.JudgeVerdictPass, RecordedAt: now, CostUSD: 0.01},
			{TaskID: "task-2", Verdict: persistence.JudgeVerdictFail, RecordedAt: now, CostUSD: 0.02},
			{TaskID: "task-3", Verdict: persistence.JudgeVerdictAbstain, RecordedAt: now, CostUSD: 0.03},
			{TaskID: "task-4", Verdict: persistence.JudgeVerdictPass, RecordedAt: now, CostUSD: 0.04},
		},
	}))
	got := srv.computeHallucinationSummary(context.Background(), "p1", nil)
	assert.Equal(t, 2, got.VerdictsPass)
	assert.Equal(t, 1, got.VerdictsFail)
	assert.Equal(t, 1, got.VerdictsAbstain)
	assert.InDelta(t, 0.10, got.VerdictTotalCostUSD, 0.001)
	assert.Len(t, got.LatestVerdicts, 4)
}

func TestComputeHallucinationSummary_VerdictsBeforeWindowDropped(t *testing.T) {
	old := time.Now().UTC().Add(-30 * 24 * time.Hour)
	srv := NewServer(WithJudgeVerdictRepository(&extendedJudgeVerdictRepo{
		listRows: []*persistence.TaskJudgeVerdict{
			{TaskID: "task-old", Verdict: persistence.JudgeVerdictPass, RecordedAt: old, CostUSD: 0.50},
		},
	}))
	got := srv.computeHallucinationSummary(context.Background(), "p1", nil)
	assert.Equal(t, 0, got.VerdictsPass, "older-than-window verdicts must be dropped")
	assert.Equal(t, 0.0, got.VerdictTotalCostUSD)
}

func TestComputeHallucinationSummary_LatestVerdictsCappedAt5(t *testing.T) {
	now := time.Now().UTC()
	rows := make([]*persistence.TaskJudgeVerdict, 20)
	for i := range rows {
		rows[i] = &persistence.TaskJudgeVerdict{
			TaskID: "task-x", Verdict: persistence.JudgeVerdictPass, RecordedAt: now,
		}
	}
	srv := NewServer(WithJudgeVerdictRepository(&extendedJudgeVerdictRepo{listRows: rows}))
	got := srv.computeHallucinationSummary(context.Background(), "p1", nil)
	assert.LessOrEqual(t, len(got.LatestVerdicts), 5, "LatestVerdicts panel must cap at 5")
}

func TestComputeHallucinationSummary_VerdictsListErrorSilentlyEmpty(t *testing.T) {
	srv := NewServer(WithJudgeVerdictRepository(&extendedJudgeVerdictRepo{
		listErr: errors.New("db down"),
	}))
	got := srv.computeHallucinationSummary(context.Background(), "p1", nil)
	assert.Equal(t, 0, got.VerdictsPass)
}

func TestComputeHallucinationSummary_OutcomeRepoFlagsHasRecentOutcomes(t *testing.T) {
	srv := NewServer(WithStepOutcomeRepository(&fakeOutcomeRepo{
		rows: []*persistence.ExecutionStepOutcome{
			{ID: "outcome-1", ExecutionID: "exec-1", StepID: "step-a", Outcome: "ok"},
		},
	}))
	got := srv.computeHallucinationSummary(context.Background(), "p1", nil)
	assert.True(t, got.HasRecentOutcomes, "non-empty outcome list should flip HasRecentOutcomes true")
}

func TestComputeHallucinationSummary_OutcomeListErrorSilentlyEmpty(t *testing.T) {
	srv := NewServer(WithStepOutcomeRepository(&fakeOutcomeRepo{
		listErr: errors.New("db down"),
	}))
	got := srv.computeHallucinationSummary(context.Background(), "p1", nil)
	assert.False(t, got.HasRecentOutcomes)
}

func TestTruncateSummary_ShortPassthrough(t *testing.T) {
	assert.Equal(t, "hello", truncateSummary("hello", 200))
}

func TestTruncateSummary_LongTruncated(t *testing.T) {
	long := "abcdefghijklmnopqrst"
	got := truncateSummary(long, 5)
	assert.Equal(t, "abcde…", got)
}
