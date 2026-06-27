package dispatcher

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
)

// recordingUsageRepo captures every Record call so the test can
// assert what project_id the dispatcher wrote without a database.
// Implements the narrow surface recordLLMUsage needs; the rest of
// the TaskLLMUsageRepository interface is filled with no-ops.
type recordingUsageRepo struct {
	mu      sync.Mutex
	records []*persistence.TaskLLMUsage
}

func (r *recordingUsageRepo) Record(_ context.Context, u *persistence.TaskLLMUsage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *u
	r.records = append(r.records, &cp)
	return nil
}

func (r *recordingUsageRepo) Upsert(_ context.Context, u *persistence.TaskLLMUsage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *u
	for i, rec := range r.records {
		if rec.ID == u.ID {
			r.records[i] = &cp
			return nil
		}
	}
	r.records = append(r.records, &cp)
	return nil
}

func (r *recordingUsageRepo) snapshot() []*persistence.TaskLLMUsage {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*persistence.TaskLLMUsage, len(r.records))
	copy(out, r.records)
	return out
}

// Stub fills for the rest of the TaskLLMUsageRepository interface.
func (r *recordingUsageRepo) List(context.Context, persistence.TaskLLMUsageFilter) ([]*persistence.TaskLLMUsage, error) {
	return nil, nil
}
func (r *recordingUsageRepo) SumCostByProject(context.Context, string, time.Time, time.Time) (float64, error) {
	return 0, nil
}
func (r *recordingUsageRepo) SumCost(context.Context, time.Time, time.Time) (float64, error) {
	return 0, nil
}
func (r *recordingUsageRepo) AggregateByRoleModel(context.Context, time.Time, time.Time, int, string) ([]persistence.RoleModelSpend, error) {
	return nil, nil
}
func (r *recordingUsageRepo) AggregateByProject(context.Context, time.Time, time.Time, int) ([]persistence.ProjectSpend, error) {
	return nil, nil
}
func (r *recordingUsageRepo) AggregateBySource(context.Context, time.Time, time.Time, string) ([]persistence.SourceSpend, error) {
	return nil, nil
}
func (r *recordingUsageRepo) TimeSeriesByDay(context.Context, time.Time, time.Time, string) ([]persistence.DailySpend, error) {
	return nil, nil
}
func (r *recordingUsageRepo) TopTasks(context.Context, time.Time, time.Time, int, string) ([]persistence.TaskSpend, error) {
	return nil, nil
}
func (r *recordingUsageRepo) TaskCostBreakdown(context.Context, string) ([]persistence.StepSpend, error) {
	return nil, nil
}

// TestRecordLLMUsage_NoBillingProjectKeepsActiveProject — back-
// compat: with WithBillingProjectID unset, every dispatcher row
// keeps the active project's project_id, matching pre-2026.4.x
// behaviour.
func TestRecordLLMUsage_NoBillingProjectKeepsActiveProject(t *testing.T) {
	repo := &recordingUsageRepo{}
	a := &Agent{
		llmUsageRepo: repo,
		logger:       zerolog.Nop(),
	}
	resp := &chat.ChatResponse{Model: "test-model"}
	resp.Usage.PromptTokens = 100
	resp.Usage.CompletionTokens = 20

	a.recordLLMUsage(context.Background(), "headmatch", 1234, resp)

	rows := repo.snapshot()
	require.Len(t, rows, 1)
	assert.Equal(t, "headmatch", rows[0].ProjectID,
		"without WithBillingProjectID, project_id must equal the active project — back-compat for deployments that haven't opted in")
}

// TestRecordLLMUsage_BillingProjectOverridesActive — with the
// billing project set, every row's project_id lands on the
// configured value regardless of which project the chat is
// pinned to. This is the headline fix for the operator's
// "$6.32 on a project with no automation" symptom.
func TestRecordLLMUsage_BillingProjectOverridesActive(t *testing.T) {
	repo := &recordingUsageRepo{}
	a := &Agent{
		llmUsageRepo:     repo,
		billingProjectID: "assistant",
		logger:           zerolog.Nop(),
	}
	resp := &chat.ChatResponse{Model: "test-model"}
	resp.Usage.PromptTokens = 100
	resp.Usage.CompletionTokens = 20

	// Three separate chat turns, each with a different active
	// project — all three must land on "assistant".
	a.recordLLMUsage(context.Background(), "headmatch", 1, resp)
	a.recordLLMUsage(context.Background(), "test-project", 2, resp)
	a.recordLLMUsage(context.Background(), "dev-project", 3, resp)

	rows := repo.snapshot()
	require.Len(t, rows, 3)
	for i, r := range rows {
		assert.Equal(t, "assistant", r.ProjectID,
			"row %d must be billed to the configured assistant project, not the active chat project", i)
		assert.Equal(t, "dispatcher", r.Role)
		assert.Equal(t, persistence.TaskLLMUsageSourceDispatcher, r.Source)
	}
}

// TestRecordLLMUsage_SkipsZeroTokens — defensive guard: chat
// responses with no usage block (some upstreams omit it on
// streaming partial completions) shouldn't write rows. Keeps
// the dashboard from being polluted with zero-cost noise.
func TestRecordLLMUsage_SkipsZeroTokens(t *testing.T) {
	repo := &recordingUsageRepo{}
	a := &Agent{
		llmUsageRepo:     repo,
		billingProjectID: "assistant",
		logger:           zerolog.Nop(),
	}
	a.recordLLMUsage(context.Background(), "headmatch", 1, &chat.ChatResponse{Model: "test"})
	assert.Empty(t, repo.snapshot())
}

// TestWithMaxIterations_RejectsNonPositive — defensive: an
// operator setting MaxToolIterations: 0 in YAML should NOT
// silently disable the iteration loop entirely (every chat would
// produce zero LLM calls). Zero falls through to the constructor
// default; only positive values override.
func TestWithMaxIterations_RejectsNonPositive(t *testing.T) {
	a := &Agent{maxIterations: 10}
	WithMaxIterations(0)(a)
	assert.Equal(t, 10, a.maxIterations, "zero must not override")
	WithMaxIterations(-5)(a)
	assert.Equal(t, 10, a.maxIterations, "negative must not override")
	WithMaxIterations(25)(a)
	assert.Equal(t, 25, a.maxIterations, "positive value applies")
}

func (r *recordingUsageRepo) SumCostByAPIKey(_ context.Context, _ string, _, _ time.Time) (float64, error) {
	return 0, nil
}

func (r *recordingUsageRepo) MeanCostByWorkflow(_ context.Context, _, _ string, _, _ time.Time) (float64, int, error) {
	return 0, 0, nil
}
