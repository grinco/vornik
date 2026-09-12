package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/trading"
)

type evidenceAuditRepo struct {
	stubAuditRepo
	entries []*persistence.ToolAuditEntry
	filter  persistence.ToolAuditFilter
	listErr error
}

func (r *evidenceAuditRepo) List(_ context.Context, f persistence.ToolAuditFilter) ([]*persistence.ToolAuditEntry, error) {
	r.filter = f
	return r.entries, r.listErr
}

func evidenceProject() *registry.Project {
	p := tradingFloorProject("trade")
	p.Trading.Scorecard.Enabled = false
	p.Trading.EntryPolicy = trading.EntryPolicy{Enabled: true, AllowedSymbols: []string{"MSFT"}, MaxRiskUSD: 50, MaxEntriesPerTick: 1}
	p.Trading.AnalysisEvidence = trading.AnalysisEvidence{Enabled: true, MinDailyBars: 205, BenchmarkSymbols: []string{"SPY"}}
	return p
}

func evidenceOutcome() *StepOutcome {
	return &StepOutcome{
		Task:        &persistence.Task{ID: "t", ProjectID: "trade", Payload: []byte(`{"taskType":"trading"}`)},
		Execution:   &persistence.Execution{ID: "exec-1"},
		StepID:      "strategize",
		ResultBytes: []byte(`{"proposals":[],"has_proposals":false,"holdings_review":[]}`),
	}
}

func pinSession(t *testing.T, open bool) {
	t.Helper()
	prev := evidenceNow
	tz, _ := time.LoadLocation("America/New_York")
	at := time.Date(2026, 9, 10, 10, 12, 0, 0, tz)
	if !open {
		at = time.Date(2026, 9, 10, 18, 0, 0, 0, tz)
	}
	evidenceNow = func() time.Time { return at }
	t.Cleanup(func() { evidenceNow = prev })
}

// Regression for the 2026-09-10 finding: a strategist step that fetched only
// account, orders and memory and emitted nothing must fail the step with the
// shape-retry prefix, scoped to this execution and step.
func TestTradingEvidenceGate_RefusesTickWithNoAnalysis(t *testing.T) {
	pinSession(t, true)
	repo := &evidenceAuditRepo{entries: []*persistence.ToolAuditEntry{
		{ToolName: "mcp__broker__get_account_summary", ToolInput: "{}", ToolOutput: `{"positions":[{"symbol":"SHEL","qty":26}]}`, Outcome: "ok"},
		{ToolName: "mcp__broker__get_orders", ToolInput: "{}", ToolOutput: `{"open":[]}`, Outcome: "ok"},
		{ToolName: "memory_search", ToolInput: `{"query":"x"}`, ToolOutput: `{}`, Outcome: "ok"},
	}}
	e := &Executor{logger: zerolog.Nop(), workflows: &MockWorkflowResolver{projects: map[string]*registry.Project{"trade": evidenceProject()}}, auditRepo: repo}
	in := evidenceOutcome()
	v := e.tradingFloorParticipant(context.Background(), in)
	require.True(t, v.Refused)
	require.Error(t, in.Err)
	assert.Contains(t, in.Err.Error(), "schema violation: analysis evidence [no_analysis_calls]")
	require.NotNil(t, repo.filter.ExecutionID)
	assert.Equal(t, "exec-1", *repo.filter.ExecutionID)
	require.NotNil(t, repo.filter.StepID)
	assert.Equal(t, "strategize", *repo.filter.StepID)
}

func TestTradingEvidenceGate_PassesExaminedTick(t *testing.T) {
	pinSession(t, true)
	repo := &evidenceAuditRepo{entries: []*persistence.ToolAuditEntry{
		{ToolName: "mcp__broker__get_account_summary", ToolOutput: `{"positions":[{"symbol":"SHEL","qty":26}]}`, Outcome: "ok"},
		{ToolName: "mcp__ta__scorecard", ToolInput: `{"symbol":"SPY","region":"us"}`, ToolOutput: `{"total":1,"trend":1,"momentum":0,"macro":0,"last_close":500,"sma50":490}`, Outcome: "ok"},
		{ToolName: "mcp__ta__scorecard", ToolInput: `{"symbol":"MSFT","region":"us"}`, ToolOutput: `{"total":2,"trend":1,"momentum":1,"macro":0,"last_close":400,"sma50":390}`, Outcome: "ok"},
		{ToolName: "mcp__ta__scorecard", ToolInput: `{"symbol":"SHEL","region":"eu"}`, ToolOutput: `{"total":1,"trend":1,"momentum":0,"macro":0,"last_close":95.5,"sma50":93.1}`, Outcome: "ok"},
	}}
	e := &Executor{logger: zerolog.Nop(), workflows: &MockWorkflowResolver{projects: map[string]*registry.Project{"trade": evidenceProject()}}, auditRepo: repo}
	in := evidenceOutcome()
	in.ResultBytes = []byte(`{"proposals":[],"has_proposals":false,"holdings_review":[{"symbol":"SHEL","last_close":95.5,"sma50":93.1,"verdict":"hold"}]}`)
	v := e.tradingFloorParticipant(context.Background(), in)
	assert.False(t, v.Refused, v.Reason)
	assert.NoError(t, in.Err)
}

func TestTradingEvidenceGate_SelfGates(t *testing.T) {
	base := func() (*evidenceAuditRepo, *Executor) {
		repo := &evidenceAuditRepo{}
		return repo, &Executor{logger: zerolog.Nop(), workflows: &MockWorkflowResolver{projects: map[string]*registry.Project{"trade": evidenceProject()}}, auditRepo: repo}
	}
	t.Run("outside the session", func(t *testing.T) {
		pinSession(t, false)
		_, e := base()
		assert.NoError(t, e.checkTradingEvidence(context.Background(), evidenceOutcome()))
	})
	t.Run("gate disabled", func(t *testing.T) {
		pinSession(t, true)
		repo, e := base()
		p := evidenceProject()
		p.Trading.AnalysisEvidence.Enabled = false
		e.workflows = &MockWorkflowResolver{projects: map[string]*registry.Project{"trade": p}}
		assert.NoError(t, e.checkTradingEvidence(context.Background(), evidenceOutcome()))
		assert.Nil(t, repo.filter.ExecutionID, "audit not read when disabled")
	})
	t.Run("research task", func(t *testing.T) {
		pinSession(t, true)
		_, e := base()
		in := evidenceOutcome()
		in.Task.Payload = []byte(`{"taskType":"research"}`)
		assert.NoError(t, e.checkTradingEvidence(context.Background(), in))
	})
	t.Run("risk-officer envelope has no proposals key", func(t *testing.T) {
		pinSession(t, true)
		_, e := base()
		in := evidenceOutcome()
		in.ResultBytes = []byte(`{"approved":[],"rejected":[]}`)
		assert.NoError(t, e.checkTradingEvidence(context.Background(), in))
	})
	t.Run("audit unavailable fails closed", func(t *testing.T) {
		pinSession(t, true)
		repo, e := base()
		repo.listErr = errors.New("db down")
		err := e.checkTradingEvidence(context.Background(), evidenceOutcome())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "schema violation")
	})
}
