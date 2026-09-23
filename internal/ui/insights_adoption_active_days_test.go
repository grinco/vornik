package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// Issue grinco/vornik#14, part 2, reported 2026-09-18 and still live after the
// row-set half was fixed by b6222b7c0.
//
// THE DEFECT. `ActiveDays` was written in exactly one place — a deferred
// closure inside addRAGActivity — from a day set built exclusively from
// memory_retrieval_audit. Every other contributor to a row (LLM spend, memory
// writes) fed the counts beside it and fed nothing into the day set, so the
// board rendered rows reading "1,284 LLM calls · 0/30 active days". A
// self-contradicting row is worse than a missing one, because it looks
// measured: a reader takes "0 active days" as a finding about the credential
// rather than about the query behind the column.
//
// THE FIX these tests pin is the UNION, not a sum. A credential that queried
// RAG and spent on an LLM the same day has ONE active day. That is also why the
// spend side needed a new repository method: AggregateByAPIKey is
// pre-aggregated with no date column and TimeSeriesByDay aggregates across all
// credentials, so neither could answer "which days did THIS key spend on", and
// a count could not be unioned with anything even if it could.

// dayStubRetrieval serves retrieval audit rows for the union tests.
type dayStubRetrieval struct {
	persistence.MemoryRetrievalAuditRepository
	rows []*persistence.MemoryRetrievalAudit
}

func (d *dayStubRetrieval) List(context.Context, persistence.MemoryRetrievalAuditFilter) ([]*persistence.MemoryRetrievalAudit, error) {
	return d.rows, nil
}

// dayStubIngest serves memory-write audit rows.
type dayStubIngest struct {
	persistence.MemoryIngestAuditRepository
	rows []*persistence.MemoryIngestAudit
}

func (d *dayStubIngest) List(context.Context, persistence.MemoryIngestAuditFilter) ([]*persistence.MemoryIngestAudit, error) {
	return d.rows, nil
}

// dayStubSpend answers both spend questions: the pre-aggregated totals the
// board already showed, and the per-credential day sets it could not ask for.
type dayStubSpend struct {
	persistence.TaskLLMUsageRepository
	spends []persistence.APIKeySpend
	days   map[string][]string
	called bool
}

func (d *dayStubSpend) AggregateByAPIKey(context.Context, time.Time, time.Time, int, string) ([]persistence.APIKeySpend, error) {
	return d.spends, nil
}

func (d *dayStubSpend) ActiveDaysByAPIKey(context.Context, time.Time, time.Time, string) (map[string][]string, error) {
	d.called = true
	return d.days, nil
}

func strp(s string) *string { return &s }

func collectForDays(t *testing.T, s *Server) *adoptionStats {
	t.Helper()
	st := &adoptionStats{}
	s.collectKeyActivity(context.Background(), st, []string{"p1"}, []string{"p1"}, time.Now().AddDate(0, 0, -adoptionDays))
	return st
}

// The reported symptom: a credential whose only activity is LLM spend shows a
// non-zero call count beside zero active days.
func TestAdoptionActiveDays_CreditsSpendOnlyCredential(t *testing.T) {
	spend := &dayStubSpend{
		spends: []persistence.APIKeySpend{{APIKeyID: "akey_llm", KeyName: "ci/runner", CallCount: 1284}},
		days:   map[string][]string{"akey_llm": {"2026-09-16", "2026-09-17", "2026-09-18"}},
	}
	s := &Server{llmUsageRepo: spend}

	row := findKeyRow(collectForDays(t, s).Keys, "akey_llm")
	if row == nil {
		t.Fatal("a credential with 1,284 LLM calls is absent from the board")
	}
	if !spend.called {
		t.Error("the day set was never asked for; ActiveDays cannot have been derived from spend")
	}
	if row.ActiveDays != 3 {
		t.Errorf("ActiveDays = %d, want 3 — the row reads %d LLM calls beside it", row.ActiveDays, row.LLMCalls)
	}
}

// The union, which is the part a sum would get wrong: one shared day across two
// ledgers is one active day, and the days that differ still add.
func TestAdoptionActiveDays_UnionsLedgersRatherThanSumming(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 9, d, 11, 0, 0, 0, time.UTC) }
	s := &Server{
		memoryRetrievalAudit: &dayStubRetrieval{rows: []*persistence.MemoryRetrievalAudit{
			{ActorID: strp("akey_both"), ActorKind: strp("companion:claude-code"), ProjectID: "p1", RetrievedAt: day(16)},
			{ActorID: strp("akey_both"), ActorKind: strp("companion:claude-code"), ProjectID: "p1", RetrievedAt: day(17)},
		}},
		memoryIngestAudit: &dayStubIngest{rows: []*persistence.MemoryIngestAudit{
			{ActorID: strp("akey_both"), ActorKind: strp("companion:claude-code"), ProjectID: "p1", IngestedAt: day(17)},
			{ActorID: strp("akey_both"), ActorKind: strp("companion:claude-code"), ProjectID: "p1", IngestedAt: day(19)},
		}},
		llmUsageRepo: &dayStubSpend{
			spends: []persistence.APIKeySpend{{APIKeyID: "akey_both", CallCount: 7}},
			// 09-16 overlaps retrieval, 09-18 is new.
			days: map[string][]string{"akey_both": {"2026-09-16", "2026-09-18"}},
		},
	}

	row := findKeyRow(collectForDays(t, s).Keys, "akey_both")
	if row == nil {
		t.Fatal("the credential is absent from the board")
	}
	// 16, 17 (retrieval) ∪ 17, 19 (ingest) ∪ 16, 18 (spend) = 16,17,18,19.
	if row.ActiveDays != 4 {
		t.Errorf("ActiveDays = %d, want 4 distinct days; 6 would mean the sets were summed", row.ActiveDays)
	}
}

// Memory writes alone are activity. Before the fix the ingest ledger fed
// MemoryWrites and nothing else, so a companion that only deposited looked
// dormant.
func TestAdoptionActiveDays_CreditsMemoryWriteOnlyCredential(t *testing.T) {
	s := &Server{memoryIngestAudit: &dayStubIngest{rows: []*persistence.MemoryIngestAudit{
		{ActorID: strp("akey_ingest"), ActorKind: strp("companion:claude-code"), ProjectID: "p1",
			IngestedAt: time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)},
	}}}

	row := findKeyRow(collectForDays(t, s).Keys, "akey_ingest")
	if row == nil {
		t.Fatal("a credential that deposited into memory is absent from the board")
	}
	if row.ActiveDays != 1 {
		t.Errorf("ActiveDays = %d, want 1 beside %d memory writes", row.ActiveDays, row.MemoryWrites)
	}
}

// A scope whose day query fails must not discard days already collected — from
// the other ledgers, or from a scope that answered.
//
// THE FIRST VERSION OF THIS TEST COULD NOT FAIL, and the review of this change
// said so. It gave the erroring repository no days to lose: it asserted that a
// retrieval day survived, which the ORIGINAL buggy code also did, because that
// code only ever set ActiveDays from retrievals. A test that passes identically
// against the broken and the fixed implementation has demonstrated nothing. It
// now spans two scopes, one answering and one failing, so a regression that
// drops the answered scope's days when a later one errors turns it red.
func TestAdoptionActiveDays_AFailedScopeDoesNotDiscardTheAnsweredOne(t *testing.T) {
	spend := &partiallyFailingDaysRepo{
		days: map[string]map[string][]string{
			"p1": {"akey_x": {"2026-09-16", "2026-09-17"}},
			// "p2" is absent: that scope errors.
		},
	}
	s := &Server{
		memoryRetrievalAudit: &dayStubRetrieval{rows: []*persistence.MemoryRetrievalAudit{
			{ActorID: strp("akey_x"), ActorKind: strp("companion:claude-code"), ProjectID: "p1",
				RetrievedAt: time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)},
		}},
		llmUsageRepo: spend,
	}

	st := &adoptionStats{}
	s.collectKeyActivity(context.Background(), st, []string{"p1", "p2"}, []string{"p1", "p2"},
		time.Now().AddDate(0, 0, -adoptionDays))

	if !spend.failed {
		t.Fatal("the failing scope was never reached (p2 must error after p1 answers); check scope iteration order")
	}
	row := findKeyRow(st.Keys, "akey_x")
	if row == nil {
		t.Fatal("the credential is absent from the board")
	}
	// 09-16 and 09-17 from the scope that answered, 09-18 from retrieval. The
	// failing scope contributes nothing and takes nothing away.
	if row.ActiveDays != 3 {
		t.Errorf("ActiveDays = %d, want 3: a failing scope must neither add nor subtract", row.ActiveDays)
	}
}

// partiallyFailingDaysRepo answers the day query for the scopes it knows and
// errors for the rest — the shape of a real partial outage, which a repository
// that always errors cannot reproduce.
type partiallyFailingDaysRepo struct {
	persistence.TaskLLMUsageRepository
	days   map[string]map[string][]string
	failed bool
}

func (e *partiallyFailingDaysRepo) AggregateByAPIKey(context.Context, time.Time, time.Time, int, string) ([]persistence.APIKeySpend, error) {
	return nil, nil
}

func (e *partiallyFailingDaysRepo) ActiveDaysByAPIKey(_ context.Context, _, _ time.Time, projectID string) (map[string][]string, error) {
	if d, ok := e.days[projectID]; ok {
		return d, nil
	}
	e.failed = true
	return nil, context.DeadlineExceeded
}

// TestInsightsAdoptionPage_DoesNotRenderCallsBesideZeroDays — the defect at the
// surface it was reported from.
//
// Every other test in this file stops at collectKeyActivity. That is one layer
// BELOW where the customer saw the problem: they read a rendered table cell,
// "1,284" in the LLM calls column and "0/30" in the active days column, on one
// row. The template was never wrong — it faithfully rendered the 0 the
// collector handed it — which is exactly why a template-only test could not
// have caught this, and why the page test that shipped with part 1 of
// grinco/vornik#14 did not: it builds AdoptionData by hand and never runs a
// collector.
//
// So this one drives the real HTTP handler with real ledgers behind it and
// asserts on the rendered HTML. It is the only test here that would have
// reproduced the report as written.
func TestInsightsAdoptionPage_DoesNotRenderCallsBesideZeroDays(t *testing.T) {
	spend := &dayStubSpend{
		spends: []persistence.APIKeySpend{{
			APIKeyID: "akey_llm", KeyName: "ci/runner", CallCount: 1284, CostUSD: 12.5,
		}},
		days: map[string][]string{"akey_llm": {"2026-09-16", "2026-09-17", "2026-09-18"}},
	}
	s := NewServer(
		WithTaskRepository(&mocks.MockTaskRepository{}),
		WithLLMUsageRepository(spend),
	)

	rec := httptest.NewRecorder()
	s.InsightsAdoption(rec, httptest.NewRequest(http.MethodGet, "/ui/insights/adoption", nil))

	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %q", rec.Code, body[:min(400, len(body))])
	}
	if strings.Contains(body, "Internal server error") || !strings.Contains(body, "</html>") {
		t.Fatalf("page did not render to completion: %q", body[max(0, len(body)-300):])
	}
	if !strings.Contains(body, "ci/runner") {
		t.Fatal("the credential is absent from the rendered board")
	}
	if !strings.Contains(body, "1284") {
		t.Error("the LLM call count did not render; this test is not looking at the reported row")
	}

	// The reported cell, verbatim from the template:
	// {{.ActiveDays}}<span class="text-gray-600">/{{$.Stats.Days}}</span>
	zeroCell := `>0<span class="text-gray-600">/30</span>`
	if strings.Contains(body, zeroCell) {
		t.Error(`the board still renders "0/30" active days on a row with 1,284 LLM calls`)
	}
	if !strings.Contains(body, `>3<span class="text-gray-600">/30</span>`) {
		t.Error("the row does not show the 3 days the spend ledger recorded")
	}
}
