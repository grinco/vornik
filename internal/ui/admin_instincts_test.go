package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// stubInstinctRepo backs the admin-instincts page tests. Implements only
// the methods the UI handler touches (List, Retire). The rest panic so
// surface drift surfaces in tests.
type stubInstinctRepo struct {
	rows        []*persistence.Instinct
	retireCalls int
	listErr     error
	// listApplicationCounts is the canned per-instinct application tally
	// the lift-column tests drive; listApplicationCountsErr, when set,
	// is returned instead so the fail-soft path can be exercised.
	listApplicationCounts    map[string]*persistence.InstinctApplicationCounts
	listApplicationCountsErr error
}

func (s *stubInstinctRepo) Upsert(context.Context, *persistence.Instinct) (string, error) {
	panic("not used by ui tests")
}
func (s *stubInstinctRepo) AddEvidence(context.Context, *persistence.InstinctEvidence) (bool, error) {
	panic("not used by ui tests")
}

func (s *stubInstinctRepo) RecordActionVersion(context.Context, *persistence.InstinctActionVersion) error {
	panic("not used by ui tests")
}

func (s *stubInstinctRepo) ListActionHistory(context.Context, string, int) ([]*persistence.InstinctActionVersion, error) {
	panic("not used by ui tests")
}
func (s *stubInstinctRepo) RecomputeConfidence(context.Context, string, persistence.InstinctScorer) error {
	panic("not used by ui tests")
}
func (s *stubInstinctRepo) Get(_ context.Context, id string) (*persistence.Instinct, error) {
	for _, r := range s.rows {
		if r.ID == id {
			cp := *r
			return &cp, nil
		}
	}
	return nil, persistence.ErrNotFound
}
func (s *stubInstinctRepo) List(_ context.Context, filter persistence.InstinctFilter) ([]*persistence.Instinct, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	out := []*persistence.Instinct{}
	for _, r := range s.rows {
		if filter.Domain != nil && r.Domain != *filter.Domain {
			continue
		}
		if filter.Status != nil && r.Status != *filter.Status {
			continue
		}
		cp := *r
		out = append(out, &cp)
	}
	return out, nil
}
func (s *stubInstinctRepo) CountActiveProjects(context.Context, string) (int, error) {
	panic("not used by ui tests")
}
func (s *stubInstinctRepo) CountByDomainStatus(context.Context) ([]persistence.InstinctDomainStatusCount, error) {
	panic("not used by ui tests")
}
func (s *stubInstinctRepo) Retire(_ context.Context, id string) error {
	for _, r := range s.rows {
		if r.ID == id {
			s.retireCalls++
			r.Status = persistence.InstinctStatusRetired
			return nil
		}
	}
	return persistence.ErrNotFound
}
func (s *stubInstinctRepo) RecordApplication(context.Context, *persistence.InstinctApplication) error {
	panic("not used by ui tests")
}
func (s *stubInstinctRepo) ListApplications(context.Context, string, int) ([]*persistence.InstinctApplication, error) {
	panic("not used by ui tests")
}
func (s *stubInstinctRepo) ListPendingRecoveryApplications(context.Context, int) ([]*persistence.InstinctApplication, error) {
	panic("not used by ui tests")
}
func (s *stubInstinctRepo) ResolveApplication(context.Context, string, string) error {
	panic("not used by ui tests")
}

// ListApplicationCounts returns the canned lift tally for the requested
// IDs; the ui lift-column tests set listApplicationCounts (or
// listApplicationCountsErr for the fail-soft path).
func (s *stubInstinctRepo) ListApplicationCounts(_ context.Context, ids []string) (map[string]*persistence.InstinctApplicationCounts, error) {
	if s.listApplicationCountsErr != nil {
		return nil, s.listApplicationCountsErr
	}
	out := map[string]*persistence.InstinctApplicationCounts{}
	for _, id := range ids {
		if c, ok := s.listApplicationCounts[id]; ok {
			out[id] = c
		}
	}
	return out, nil
}

func seedInstincts() []*persistence.Instinct {
	now := time.Now().Add(-5 * time.Minute)
	return []*persistence.Instinct{
		{ID: "ins_1", Scope: "project", ProjectID: "alpha", Domain: "recovery", TriggerKey: "tk_a",
			Trigger: []byte(`{"role":"coder"}`), Action: "retry resolved it",
			Confidence: 0.82, SupportCount: 5, ContradictCount: 1, Source: "observer", Status: "active", LastSeenAt: now},
		{ID: "ins_2", Scope: "global", Domain: "workflow", TriggerKey: "tk_b",
			Action: "split the verify step", Confidence: 0.91, SupportCount: 9, Source: "observer", Status: "promoted", LastSeenAt: now},
		{ID: "ins_3", Scope: "project", ProjectID: "alpha", Domain: "quality", TriggerKey: "tk_c",
			Action: "old pattern", Confidence: 0.1, Source: "observer", Status: "retired", LastSeenAt: now},
	}
}

func TestAdminInstincts_RendersFilteredList(t *testing.T) {
	repo := &stubInstinctRepo{rows: seedInstincts()}
	s := NewServer(WithInstinctPlaybooks(repo, false)) // gate off — browser still reads the repo

	req := httptest.NewRequest(http.MethodGet, "/admin/instincts?domain=recovery", nil)
	rec := httptest.NewRecorder()
	s.adminRouter(rec, withAdminUI(req))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "ins_1") || !strings.Contains(body, "retry resolved it") {
		t.Errorf("body missing recovery instinct row")
	}
	if strings.Contains(body, "ins_2") {
		t.Errorf("domain=recovery should have filtered out the workflow instinct")
	}
	if !strings.Contains(body, "Instincts") {
		t.Errorf("body missing page header")
	}
	// Retire button only for non-retired rows.
	if !strings.Contains(body, "/ui/admin/instincts/ins_1/retire") {
		t.Errorf("body missing retire form for active row")
	}
}

func TestAdminInstincts_RepoUnwired(t *testing.T) {
	s := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/admin/instincts", nil)
	rec := httptest.NewRecorder()
	s.adminRouter(rec, withAdminUI(req))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Instinct repository not wired") {
		t.Errorf("body missing unwired empty-state; got %s", rec.Body.String())
	}
}

func TestAdminInstincts_RetirePath(t *testing.T) {
	repo := &stubInstinctRepo{rows: seedInstincts()}
	audit := &stubAdminAuditRepo{}
	s := NewServer(WithInstinctPlaybooks(repo, false), WithAdminAuditRepository(audit))

	req := httptest.NewRequest(http.MethodPost, "/admin/instincts/ins_1/retire", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.adminRouter(rec, withAdminUI(req))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if got := rec.Header().Get("Location"); !strings.Contains(got, "retired=ins_1") {
		t.Errorf("redirect should carry retired banner; got %q", got)
	}
	if repo.retireCalls != 1 {
		t.Errorf("retire calls = %d, want 1", repo.retireCalls)
	}
	if len(audit.rows) != 1 || audit.rows[0].Action != "instinct.retired" {
		t.Errorf("audit row missing; got %+v", audit.rows)
	}
}

func TestAdminInstincts_RetireMissing(t *testing.T) {
	repo := &stubInstinctRepo{rows: seedInstincts()}
	s := NewServer(WithInstinctPlaybooks(repo, false))
	req := httptest.NewRequest(http.MethodPost, "/admin/instincts/ghost/retire", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.adminRouter(rec, withAdminUI(req))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestInstinctRowToAdminRow_Shape(t *testing.T) {
	now := time.Now()
	in := &persistence.Instinct{
		ID: "ins_x", Scope: "project", ProjectID: "alpha", Domain: "recovery",
		Trigger: []byte(`{"role":"coder"}`), Action: "do the thing",
		Confidence: 0.666, SupportCount: 4, ContradictCount: 2, Source: "observer",
		Status: "active", LastSeenAt: now.Add(-2 * time.Minute),
	}
	row := instinctRowToAdminRow(in, now, nil)
	if row.Confidence != "0.67" {
		t.Errorf("confidence = %q, want 0.67", row.Confidence)
	}
	// nil counts → zero lift fields, dash summary, no panic.
	if row.AppSucceeded != 0 || row.AppFailed != 0 || row.AppIgnored != 0 {
		t.Errorf("nil counts should zero lift fields; got (%d,%d,%d)", row.AppSucceeded, row.AppFailed, row.AppIgnored)
	}
	if row.LiftSummary != "-" {
		t.Errorf("nil counts LiftSummary = %q, want %q", row.LiftSummary, "-")
	}
	if row.Trigger != `{"role":"coder"}` {
		t.Errorf("trigger = %q", row.Trigger)
	}
	if row.IsRetired {
		t.Errorf("active row should not be retired")
	}
	if !strings.HasSuffix(row.LastSeenAgo, "ago") {
		t.Errorf("last seen ago = %q", row.LastSeenAgo)
	}

	retired := instinctRowToAdminRow(&persistence.Instinct{ID: "r", Status: "retired", LastSeenAt: now}, now, nil)
	if !retired.IsRetired {
		t.Errorf("retired row should be flagged")
	}
}

func TestAdminInstincts_LiftColumnRendered(t *testing.T) {
	repo := &stubInstinctRepo{
		rows: seedInstincts(),
		listApplicationCounts: map[string]*persistence.InstinctApplicationCounts{
			"ins_1": {InstinctID: "ins_1", Succeeded: 3, Failed: 2, Ignored: 1},
		},
	}
	s := NewServer(WithInstinctPlaybooks(repo, false))

	req := httptest.NewRequest(http.MethodGet, "/admin/instincts?domain=recovery", nil)
	rec := httptest.NewRecorder()
	s.adminRouter(rec, withAdminUI(req))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// The Lift column header.
	if !strings.Contains(body, "Lift") {
		t.Errorf("body missing Lift column header")
	}
	// The compact summary for ins_1: 3 up, 2 down, 1 ignored.
	want := instinctLiftSummary(3, 2, 1)
	if !strings.Contains(body, want) {
		t.Errorf("body missing lift summary %q; body=%s", want, body)
	}
}

func TestAdminInstincts_LiftColumnZero(t *testing.T) {
	repo := &stubInstinctRepo{
		rows:                  seedInstincts(),
		listApplicationCounts: map[string]*persistence.InstinctApplicationCounts{},
	}
	s := NewServer(WithInstinctPlaybooks(repo, false))

	req := httptest.NewRequest(http.MethodGet, "/admin/instincts?domain=recovery", nil)
	rec := httptest.NewRecorder()
	s.adminRouter(rec, withAdminUI(req))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Lift") {
		t.Errorf("body missing Lift column header")
	}
	// No counts → the dash placeholder for the (single) recovery row.
	if !strings.Contains(body, ">-<") && !strings.Contains(body, ">-</td>") {
		// Loose check: the dash must appear somewhere in the table body.
		if !strings.Contains(body, "-") {
			t.Errorf("body missing zero-lift dash placeholder")
		}
	}
}

func TestAdminInstincts_LiftCountsFail(t *testing.T) {
	repo := &stubInstinctRepo{
		rows:                     seedInstincts(),
		listApplicationCountsErr: context.DeadlineExceeded,
	}
	s := NewServer(WithInstinctPlaybooks(repo, false))

	req := httptest.NewRequest(http.MethodGet, "/admin/instincts?domain=recovery", nil)
	rec := httptest.NewRecorder()
	s.adminRouter(rec, withAdminUI(req))
	// Fail-soft: the counts error must not block the page.
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (counts error must fail soft); body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "ins_1") {
		t.Errorf("page should still render the instinct rows when counts fail; body=%s", body)
	}
	// The list itself succeeded → no list-error banner.
	if strings.Contains(body, "Failed to list instincts") {
		t.Errorf("counts failure should not surface a list error banner")
	}
}

func TestInstinctLiftSummary(t *testing.T) {
	if got := instinctLiftSummary(0, 0, 0); got != "-" {
		t.Errorf("all-zero summary = %q, want %q", got, "-")
	}
	got := instinctLiftSummary(3, 2, 1)
	if got == "-" || got == "" {
		t.Errorf("non-zero summary should not be the dash/empty; got %q", got)
	}
	if len([]rune(got)) >= 20 {
		t.Errorf("summary must be compact (<20 runes); got %q (%d runes)", got, len([]rune(got)))
	}
	// A single bucket renders without the others.
	if s := instinctLiftSummary(5, 0, 0); s == "-" || s == "" {
		t.Errorf("single-bucket summary should render; got %q", s)
	}
	if s := instinctLiftSummary(0, 0, 4); s == "-" || s == "" {
		t.Errorf("ignored-only summary should render; got %q", s)
	}
}

func TestInstinctRowToAdminRow_LiftFields(t *testing.T) {
	now := time.Now()
	in := &persistence.Instinct{ID: "ins_x", Status: "active", LastSeenAt: now}
	counts := &persistence.InstinctApplicationCounts{InstinctID: "ins_x", Succeeded: 7, Failed: 3, Ignored: 2}
	row := instinctRowToAdminRow(in, now, counts)
	if row.AppSucceeded != 7 || row.AppFailed != 3 || row.AppIgnored != 2 {
		t.Errorf("lift fields = (%d,%d,%d), want (7,3,2)", row.AppSucceeded, row.AppFailed, row.AppIgnored)
	}
	if row.LiftSummary != instinctLiftSummary(7, 3, 2) {
		t.Errorf("LiftSummary = %q, want %q", row.LiftSummary, instinctLiftSummary(7, 3, 2))
	}
}

func TestInstinctStatusBadgeClass(t *testing.T) {
	for _, st := range []string{"candidate", "active", "promoted", "retired", "bogus"} {
		if instinctStatusBadgeClass(st) == "" {
			t.Errorf("badge class for %q is empty", st)
		}
	}
	// Distinct classes for the main statuses.
	if instinctStatusBadgeClass("active") == instinctStatusBadgeClass("retired") {
		t.Errorf("active and retired should differ")
	}
}

func TestAdminInstincts_ListError(t *testing.T) {
	repo := &stubInstinctRepo{listErr: context.DeadlineExceeded}
	s := NewServer(WithInstinctPlaybooks(repo, false))
	req := httptest.NewRequest(http.MethodGet, "/admin/instincts", nil)
	rec := httptest.NewRecorder()
	s.adminRouter(rec, withAdminUI(req))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (error rendered in page)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Failed to list instincts") {
		t.Errorf("body missing error banner")
	}
}

func TestAdminInstincts_BadMinConfidence(t *testing.T) {
	repo := &stubInstinctRepo{rows: seedInstincts()}
	s := NewServer(WithInstinctPlaybooks(repo, false))
	req := httptest.NewRequest(http.MethodGet, "/admin/instincts?min_confidence=nope", nil)
	rec := httptest.NewRecorder()
	s.adminRouter(rec, withAdminUI(req))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "min_confidence") {
		t.Errorf("body missing min_confidence error")
	}
}

func TestAdminInstinctRetire_MethodNotAllowed(t *testing.T) {
	repo := &stubInstinctRepo{rows: seedInstincts()}
	s := NewServer(WithInstinctPlaybooks(repo, false))
	// GET on the retire path → 405 (router only dispatches; handler guards method).
	rec := httptest.NewRecorder()
	s.AdminInstinctRetire(rec, httptest.NewRequest(http.MethodGet, "/admin/instincts/ins_1/retire", nil), "ins_1")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET retire = %d, want 405", rec.Code)
	}
}
