package hallucination

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/pricing"
)

// fakeVerdictRepo captures Record calls. GetByTask always
// returns ErrNotFound so the runner doesn't short-circuit on
// "verdict already recorded".
type fakeVerdictRepo struct {
	mu       sync.Mutex
	recorded []*persistence.TaskJudgeVerdict
}

func (f *fakeVerdictRepo) Record(_ context.Context, v *persistence.TaskJudgeVerdict) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recorded = append(f.recorded, v)
	return nil
}

func (f *fakeVerdictRepo) GetByTask(_ context.Context, _ string) (*persistence.TaskJudgeVerdict, error) {
	return nil, persistence.ErrNotFound
}

func (f *fakeVerdictRepo) ListRecent(_ context.Context, _ string, _ int) ([]*persistence.TaskJudgeVerdict, error) {
	return nil, nil
}

// fakeUsageRepo captures the task_llm_usage rows the runner
// writes for each judge call. Implements only the narrow
// UsageRecorder interface the runner depends on, so the stub
// stays small.
type fakeUsageRepo struct {
	mu       sync.Mutex
	recorded []*persistence.TaskLLMUsage
}

func (f *fakeUsageRepo) Record(_ context.Context, u *persistence.TaskLLMUsage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recorded = append(f.recorded, u)
	return nil
}

// pricingTableForJudgeTests returns a minimal table with one
// model pinned to a known $/Mtok rate so cost assertions are
// exact rather than approximate. Loaded via pricing.Load from
// a temp YAML file because pricing.Table doesn't export a
// programmatic constructor.
func pricingTableForJudgeTests(t *testing.T) *pricing.Table {
	t.Helper()
	path := t.TempDir() + "/pricing.yaml"
	require.NoError(t, os.WriteFile(path, []byte(
		`models:
  "openai.gpt-oss-120b-1:0": { input: 0.15, output: 0.60 }
`), 0o644))
	tbl, err := pricing.Load(path)
	require.NoError(t, err)
	return tbl
}

// TestJudgeRunner_RecordsCostOnVerdict — the canonical happy
// path: the LLM returned tokens, the runner computed cost via
// the pricing table, and the verdict row carries CostUSD.
func TestJudgeRunner_RecordsCostOnVerdict(t *testing.T) {
	verdicts := &fakeVerdictRepo{}
	usage := &fakeUsageRepo{}
	stub := &StubJudge{
		Out: &Verdict{Decision: persistence.JudgeVerdictPass, Confidence: 0.9},
		Metrics: &JudgeMetrics{
			Model:            "openai.gpt-oss-120b-1:0",
			PromptTokens:     1_000_000,
			CompletionTokens: 500_000,
		},
	}
	r := &JudgeRunner{
		Judge:    stub,
		Verdicts: verdicts,
		LLMUsage: usage,
		Pricing:  pricingTableForJudgeTests(t),
		Logger:   zerolog.Nop(),
	}
	task := &persistence.Task{ID: "task_x", ProjectID: "p1"}
	require.NoError(t, r.Run(context.Background(), task))

	require.Len(t, verdicts.recorded, 1)
	row := verdicts.recorded[0]
	// 1M prompt @ $0.15 + 0.5M completion @ $0.60 = 0.15 + 0.30 = $0.45.
	assert.InDelta(t, 0.45, row.CostUSD, 0.0001,
		"verdict CostUSD must reflect priced tokens (1M*0.15 + 0.5M*0.60)")
	assert.Equal(t, "openai.gpt-oss-120b-1:0", row.Model)
}

// TestJudgeRunner_RecordsLLMUsageRow — the rollup-feeding side
// of the same call: a task_llm_usage row lands with
// source="judge", role="judge", taskID set, and matching
// tokens + cost. Without this, the spend dashboard would miss
// judge cost entirely.
func TestJudgeRunner_RecordsLLMUsageRow(t *testing.T) {
	verdicts := &fakeVerdictRepo{}
	usage := &fakeUsageRepo{}
	stub := &StubJudge{
		Out: &Verdict{Decision: persistence.JudgeVerdictFail, Confidence: 0.7},
		Metrics: &JudgeMetrics{
			Model:            "openai.gpt-oss-120b-1:0",
			PromptTokens:     2000,
			CompletionTokens: 800,
		},
	}
	r := &JudgeRunner{
		Judge:    stub,
		Verdicts: verdicts,
		LLMUsage: usage,
		Pricing:  pricingTableForJudgeTests(t),
		Logger:   zerolog.Nop(),
	}
	task := &persistence.Task{ID: "task_y", ProjectID: "p1"}
	require.NoError(t, r.Run(context.Background(), task))

	require.Len(t, usage.recorded, 1, "exactly one task_llm_usage row per judge call")
	u := usage.recorded[0]
	assert.Equal(t, persistence.TaskLLMUsageSourceJudge, u.Source,
		"source must be 'judge' so the spend dashboard can split judge cost from worker/dispatcher")
	assert.Equal(t, "judge", u.Role)
	require.NotNil(t, u.TaskID)
	assert.Equal(t, "task_y", *u.TaskID)
	assert.Equal(t, "p1", u.ProjectID)
	assert.Equal(t, "openai.gpt-oss-120b-1:0", u.Model)
	assert.Equal(t, int64(2000), u.PromptTokens)
	assert.Equal(t, int64(800), u.CompletionTokens)
	assert.Greater(t, u.CostUSD, 0.0, "row's cost_usd must be priced, not zero")
	assert.WithinDuration(t, time.Now(), u.RecordedAt, 5*time.Second)
}

// TestJudgeRunner_SkipsUsageOnZeroTokens — pre-LLM abstain
// paths (no model configured, nil client) return zero metrics.
// Recording a zero-token usage row would clutter the spend
// dashboard with phantom "successful" rows that didn't cost
// anything; skip them.
func TestJudgeRunner_SkipsUsageOnZeroTokens(t *testing.T) {
	verdicts := &fakeVerdictRepo{}
	usage := &fakeUsageRepo{}
	stub := &StubJudge{
		Out:     abstainVerdict("judge not configured"),
		Metrics: &JudgeMetrics{}, // zero tokens
	}
	r := &JudgeRunner{
		Judge:    stub,
		Verdicts: verdicts,
		LLMUsage: usage,
		Pricing:  pricingTableForJudgeTests(t),
		Logger:   zerolog.Nop(),
	}
	task := &persistence.Task{ID: "task_z", ProjectID: "p1"}
	require.NoError(t, r.Run(context.Background(), task))

	require.Len(t, verdicts.recorded, 1, "verdict row STILL lands so the abstention is visible")
	assert.Empty(t, usage.recorded, "no usage row when zero tokens were spent")
	assert.Equal(t, 0.0, verdicts.recorded[0].CostUSD)
}

// TestJudgeRunner_NoPricingMeansZeroCostNotZeroUsage — when
// the pricing table isn't wired, the runner still records the
// usage row (we have tokens) but cost_usd stays 0. Distinct
// from the SkipsUsageOnZeroTokens case: tokens were spent, we
// just don't know the dollar value.
func TestJudgeRunner_NoPricingMeansZeroCostNotZeroUsage(t *testing.T) {
	verdicts := &fakeVerdictRepo{}
	usage := &fakeUsageRepo{}
	stub := &StubJudge{
		Out: &Verdict{Decision: persistence.JudgeVerdictPass},
		Metrics: &JudgeMetrics{
			Model:            "openai.gpt-oss-120b-1:0",
			PromptTokens:     100,
			CompletionTokens: 50,
		},
	}
	r := &JudgeRunner{
		Judge:    stub,
		Verdicts: verdicts,
		LLMUsage: usage,
		// Pricing intentionally nil
		Logger: zerolog.Nop(),
	}
	task := &persistence.Task{ID: "task_q", ProjectID: "p1"}
	require.NoError(t, r.Run(context.Background(), task))

	require.Len(t, usage.recorded, 1, "usage still recorded — tokens were spent")
	assert.Equal(t, 0.0, usage.recorded[0].CostUSD, "no pricing → cost = 0; tokens still tracked")
	assert.Equal(t, int64(100), usage.recorded[0].PromptTokens)
}
