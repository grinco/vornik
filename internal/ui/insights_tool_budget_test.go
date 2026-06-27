package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
)

// seedBudgetInstincts returns fixture budget-domain instincts with over and
// under-provisioning signals for the "coder" role, used in advisory tests.
func seedBudgetInstincts() []*persistence.Instinct {
	now := time.Now().Add(-10 * time.Minute)
	return []*persistence.Instinct{
		{
			ID: "bud_1", Scope: "project", ProjectID: "proj_a",
			Domain: persistence.InstinctDomainBudget, TriggerKey: "tk_bud_over",
			Trigger:    []byte(`{"role":"coder","signal":"over_provisioned"}`),
			Action:     "role coder is over-provisioned (finishes well under its tool budget)",
			Confidence: 0.74, SupportCount: 12, ContradictCount: 2,
			Source: "observer", Status: persistence.InstinctStatusActive,
			LastSeenAt: now,
		},
		{
			ID: "bud_2", Scope: "project", ProjectID: "proj_a",
			Domain: persistence.InstinctDomainBudget, TriggerKey: "tk_bud_under",
			Trigger:    []byte(`{"role":"analyst","signal":"under_provisioned"}`),
			Action:     "role analyst is under-provisioned (hits the tool-iteration ceiling)",
			Confidence: 0.81, SupportCount: 7, ContradictCount: 1,
			Source: "observer", Status: persistence.InstinctStatusPromoted,
			LastSeenAt: now,
		},
	}
}

// mkAuditEntries builds tool-audit entries: count rows per execution id.
func mkAuditEntries(perExec map[string]int) []*persistence.ToolAuditEntry {
	var out []*persistence.ToolAuditEntry
	for execID, n := range perExec {
		for i := 0; i < n; i++ {
			out = append(out, &persistence.ToolAuditEntry{ExecutionID: execID, ToolName: "t"})
		}
	}
	return out
}

func TestSummarizeToolCalls_Distribution(t *testing.T) {
	// counts per execution: 1, 3, 3, 10 → sorted [1,3,3,10]
	s := summarizeToolCalls(mkAuditEntries(map[string]int{"e1": 1, "e2": 3, "e3": 3, "e4": 10}))

	assert.Equal(t, 4, s.Executions)
	assert.Equal(t, 17, s.TotalCalls)
	assert.Equal(t, 10, s.Max)
	assert.Equal(t, 3, s.P50, "nearest-rank p50 of [1,3,3,10]")
	assert.Equal(t, 10, s.P95, "nearest-rank p95 of [1,3,3,10]")

	// Buckets cover every execution exactly once.
	total := 0
	for _, b := range s.Buckets {
		total += b.Count
	}
	assert.Equal(t, 4, total, "every execution lands in exactly one bucket")
}

func TestSummarizeToolCalls_Empty(t *testing.T) {
	s := summarizeToolCalls(nil)
	assert.Equal(t, 0, s.Executions)
	assert.Equal(t, 0, s.TotalCalls)
	assert.Equal(t, 0, s.Max)
	for _, b := range s.Buckets {
		assert.Equal(t, 0, b.Count)
	}
}

func TestInsightsToolBudget_RendersStats(t *testing.T) {
	repo := &fakeToolAuditRepo{rows: mkAuditEntries(map[string]int{"e1": 2, "e2": 5})}
	srv := NewServer(WithToolAuditRepository(repo))

	req := httptest.NewRequest(http.MethodGet, "/ui/insights/tool-budget", nil)
	rec := httptest.NewRecorder()
	srv.InsightsToolBudget(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	body := rec.Body.String()
	assert.Contains(t, body, "<svg", "renders the histogram")
	assert.Contains(t, body, "standard", "tier reference present")
	assert.Contains(t, body, "open_ended")
}

func TestInsightsToolBudget_NilRepoShowsNotice(t *testing.T) {
	srv := NewServer() // no auditRepo
	req := httptest.NewRequest(http.MethodGet, "/ui/insights/tool-budget", nil)
	rec := httptest.NewRecorder()
	srv.InsightsToolBudget(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "Internal server error")
}

// TestInsightsToolBudget_AdvisoryRendered asserts that when the instinct repo
// returns active/promoted budget instincts the advisory block is rendered with
// the role, direction label, confidence, and evidence count.
func TestInsightsToolBudget_AdvisoryRendered(t *testing.T) {
	auditRepo := &fakeToolAuditRepo{rows: mkAuditEntries(map[string]int{"e1": 3})}
	instinctRepo := &stubInstinctRepo{rows: seedBudgetInstincts()}
	srv := NewServer(
		WithToolAuditRepository(auditRepo),
		WithInstinctPlaybooks(instinctRepo, false),
	)

	req := httptest.NewRequest(http.MethodGet, "/ui/insights/tool-budget", nil)
	rec := httptest.NewRecorder()
	srv.InsightsToolBudget(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	body := rec.Body.String()
	assert.Contains(t, body, "Learned provisioning flags", "advisory section header missing")
	assert.Contains(t, body, "coder", "over-provisioned role missing")
	assert.Contains(t, body, "over-provisioned", "direction label missing")
	assert.Contains(t, body, "0.74", "confidence missing")
	assert.Contains(t, body, "12 tasks", "support count missing")
	assert.Contains(t, body, "analyst", "under-provisioned role missing")
	assert.Contains(t, body, "under-provisioned", "direction label missing")
	assert.Contains(t, body, "0.81", "confidence missing")
}

// TestInsightsToolBudget_AdvisoryAbsentWhenNone asserts that the advisory block
// is absent when the instinct repo returns no budget-domain rows.
func TestInsightsToolBudget_AdvisoryAbsentWhenNone(t *testing.T) {
	auditRepo := &fakeToolAuditRepo{rows: mkAuditEntries(map[string]int{"e1": 3})}
	// Repo with only non-budget instincts — advisory should not render.
	instinctRepo := &stubInstinctRepo{rows: []*persistence.Instinct{
		{ID: "r1", Domain: "recovery", Status: persistence.InstinctStatusActive,
			Trigger: []byte(`{"role":"coder"}`), Action: "retry",
			Confidence: 0.9, SupportCount: 5, LastSeenAt: time.Now()},
	}}
	srv := NewServer(
		WithToolAuditRepository(auditRepo),
		WithInstinctPlaybooks(instinctRepo, false),
	)

	req := httptest.NewRequest(http.MethodGet, "/ui/insights/tool-budget", nil)
	rec := httptest.NewRecorder()
	srv.InsightsToolBudget(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "Learned provisioning flags",
		"advisory block should be absent when no budget instincts")
}

// TestInsightsToolBudget_AdvisoryAbsentWhenInstinctRepoNil asserts that the
// page renders cleanly with no advisory block when the instinct repo is nil.
func TestInsightsToolBudget_AdvisoryAbsentWhenInstinctRepoNil(t *testing.T) {
	auditRepo := &fakeToolAuditRepo{rows: mkAuditEntries(map[string]int{"e1": 3})}
	srv := NewServer(WithToolAuditRepository(auditRepo)) // no instinct repo

	req := httptest.NewRequest(http.MethodGet, "/ui/insights/tool-budget", nil)
	rec := httptest.NewRecorder()
	srv.InsightsToolBudget(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.NotContains(t, body, "Learned provisioning flags",
		"advisory block should be absent without instinct repo")
	assert.NotContains(t, body, "Internal server error")
}

// TestLoadBudgetAdvisory_NilRepo asserts the helper returns nil on nil repo.
func TestLoadBudgetAdvisory_NilRepo(t *testing.T) {
	result := loadBudgetAdvisory(context.Background(), nil, "")
	assert.Nil(t, result, "nil repo should return nil advisory")
}

// TestLoadBudgetAdvisory_RowGrouping asserts the helper groups rows by role
// and sorts them deterministically by role then direction.
func TestLoadBudgetAdvisory_RowGrouping(t *testing.T) {
	repo := &stubInstinctRepo{rows: seedBudgetInstincts()}
	result := loadBudgetAdvisory(context.Background(), repo, "")
	require.NotNil(t, result)
	require.Len(t, result.Rows, 2, "expect one row per instinct")
	// Sorted by role: analyst < coder.
	assert.Equal(t, "analyst", result.Rows[0].Role)
	assert.Equal(t, "under-provisioned", result.Rows[0].Label)
	assert.Equal(t, "coder", result.Rows[1].Role)
	assert.Equal(t, "over-provisioned", result.Rows[1].Label)
}

// TestLoadBudgetAdvisory_SkipsMissingTriggerFields asserts instincts whose
// trigger JSON lacks role or signal are skipped silently.
func TestLoadBudgetAdvisory_SkipsMissingTriggerFields(t *testing.T) {
	now := time.Now()
	repo := &stubInstinctRepo{rows: []*persistence.Instinct{
		// No role field — should be skipped.
		{ID: "bad_1", Domain: persistence.InstinctDomainBudget,
			Trigger: []byte(`{"signal":"over_provisioned"}`),
			Status:  persistence.InstinctStatusActive, Confidence: 0.9,
			SupportCount: 3, LastSeenAt: now},
		// No signal field — should be skipped.
		{ID: "bad_2", Domain: persistence.InstinctDomainBudget,
			Trigger: []byte(`{"role":"coder"}`),
			Status:  persistence.InstinctStatusActive, Confidence: 0.9,
			SupportCount: 3, LastSeenAt: now},
		// Valid row.
		{ID: "ok_1", Domain: persistence.InstinctDomainBudget,
			Trigger: []byte(`{"role":"planner","signal":"under_provisioned"}`),
			Status:  persistence.InstinctStatusActive, Confidence: 0.7,
			SupportCount: 5, LastSeenAt: now},
	}}
	result := loadBudgetAdvisory(context.Background(), repo, "")
	require.NotNil(t, result)
	require.Len(t, result.Rows, 1, "only the valid row should survive")
	assert.Equal(t, "planner", result.Rows[0].Role)
}

// TestAdminInstincts_DomainDropdownIncludesBudget asserts that the budget
// domain appears in the DomainOptions for the admin instincts page, verifying
// the dropdown change (Slice 3 requirement 1).
func TestAdminInstincts_DomainDropdownIncludesBudget(t *testing.T) {
	repo := &stubInstinctRepo{rows: []*persistence.Instinct{}}
	srv := NewServer(WithInstinctPlaybooks(repo, false))

	req := httptest.NewRequest(http.MethodGet, "/admin/instincts", nil)
	rec := httptest.NewRecorder()
	srv.adminRouter(rec, withAdminUI(req))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), persistence.InstinctDomainBudget,
		"budget domain option must appear in the admin instincts dropdown")
}
