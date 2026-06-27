package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// stubCPCRepo backs the admin-cpc page tests with an in-memory
// ledger. Mirrors the shape of persistence.CrossProjectCallRepository
// but only implements the methods the UI handler touches (List, Get,
// AdminCancel) — the rest panic so future surface drift surfaces in
// tests rather than silently returning zero values.
type stubCPCRepo struct {
	mu        sync.Mutex
	rows      map[string]*persistence.CrossProjectCall
	cancelled map[string]string
	listErr   error
}

func newStubCPCRepo() *stubCPCRepo {
	return &stubCPCRepo{
		rows:      map[string]*persistence.CrossProjectCall{},
		cancelled: map[string]string{},
	}
}

func (s *stubCPCRepo) seed(c *persistence.CrossProjectCall) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[c.ID] = c
}

func (s *stubCPCRepo) Create(_ context.Context, _ *persistence.CrossProjectCall) error {
	panic("not used by ui tests")
}

func (s *stubCPCRepo) Get(_ context.Context, id string) (*persistence.CrossProjectCall, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[id]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	cp := *row
	return &cp, nil
}

func (s *stubCPCRepo) GetByCalleeTaskID(_ context.Context, _ string) (*persistence.CrossProjectCall, error) {
	panic("not used by ui tests")
}

func (s *stubCPCRepo) SetCalleeTaskID(_ context.Context, _, _ string) error {
	panic("not used by ui tests")
}

func (s *stubCPCRepo) MarkRunning(_ context.Context, _ string) error {
	panic("not used by ui tests")
}

func (s *stubCPCRepo) MarkCompleted(_ context.Context, _ string, _ []byte) error {
	panic("not used by ui tests")
}

func (s *stubCPCRepo) MarkFailed(_ context.Context, _, _ string) error {
	panic("not used by ui tests")
}

func (s *stubCPCRepo) MarkRejected(_ context.Context, _, _ string) error {
	panic("not used by ui tests")
}

func (s *stubCPCRepo) ClaimTimedOut(_ context.Context, _ time.Time, _ int) ([]*persistence.CrossProjectCall, error) {
	panic("not used by ui tests")
}

func (s *stubCPCRepo) List(_ context.Context, filter persistence.CPCListFilter) ([]*persistence.CrossProjectCall, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*persistence.CrossProjectCall, 0, len(s.rows))
	for _, r := range s.rows {
		if filter.Status != "" && r.Status != filter.Status {
			continue
		}
		if filter.CallerProject != "" && r.CallerProject != filter.CallerProject {
			continue
		}
		if filter.CalleeProject != "" && r.CalleeProject != filter.CalleeProject {
			continue
		}
		if !filter.CreatedSince.IsZero() && r.CreatedAt.Before(filter.CreatedSince) {
			continue
		}
		cp := *r
		out = append(out, &cp)
	}
	return out, nil
}

func (s *stubCPCRepo) AdminCancel(_ context.Context, id string, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[id]
	if !ok {
		return persistence.ErrNotFound
	}
	row.Status = persistence.CPCStatusRejected
	now := time.Now().UTC()
	row.ResolvedAt = &now
	em := reason
	row.ErrorMessage = &em
	s.cancelled[id] = reason
	return nil
}

// TestAdminCPC_RendersFilteredList covers the happy path —
// repo wired, two rows seeded, filter narrowing to one.
func TestAdminCPC_RendersFilteredList(t *testing.T) {
	repo := newStubCPCRepo()
	tid := "task_caller_1"
	repo.seed(&persistence.CrossProjectCall{
		ID:             "cpc_001",
		CallerProject:  "assistant",
		CallerTaskID:   tid,
		CallerStepID:   "step_summon",
		CalleeProject:  "news-feed",
		CalleeWorkflow: "summary",
		Status:         persistence.CPCStatusRunning,
		CreatedAt:      time.Now().Add(-2 * time.Minute),
	})
	repo.seed(&persistence.CrossProjectCall{
		ID:             "cpc_002",
		CallerProject:  "trader",
		CalleeProject:  "news-feed",
		CalleeWorkflow: "summary",
		Status:         persistence.CPCStatusCompleted,
		CreatedAt:      time.Now().Add(-10 * time.Minute),
	})

	s := NewServer(WithCrossProjectCallRepository(repo))

	req := httptest.NewRequest(http.MethodGet, "/admin/cpc?caller=assistant", nil)
	rec := httptest.NewRecorder()
	s.adminRouter(rec, withAdminUI(req))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "cpc_001") {
		t.Errorf("body missing cpc_001 row")
	}
	if strings.Contains(body, "cpc_002") {
		t.Errorf("body should have filtered out cpc_002 (caller=trader)")
	}
	if !strings.Contains(body, "Cross-project calls") {
		t.Errorf("body missing page header")
	}
	// Cancel button is rendered only for non-terminal rows.
	if !strings.Contains(body, "/ui/admin/cpc/cpc_001/cancel") {
		t.Errorf("body missing cancel form for running row")
	}
}

// TestAdminCPC_RepoUnwired renders the page with a friendly empty
// state when the daemon doesn't have the inter-project ledger
// wired (typical legacy deployments).
func TestAdminCPC_RepoUnwired(t *testing.T) {
	s := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/admin/cpc", nil)
	rec := httptest.NewRecorder()
	s.adminRouter(rec, withAdminUI(req))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Cross-project call repository not wired") {
		t.Errorf("body missing unwired empty-state message; got: %s", body)
	}
}

// TestAdminCPC_CancelPath drives the POST surface and checks the
// repo cancel is invoked + an audit row is written.
func TestAdminCPC_CancelPath(t *testing.T) {
	repo := newStubCPCRepo()
	repo.seed(&persistence.CrossProjectCall{
		ID:             "cpc_stuck",
		CallerProject:  "assistant",
		CalleeProject:  "news-feed",
		CalleeWorkflow: "summary",
		Status:         persistence.CPCStatusPending,
		CreatedAt:      time.Now().Add(-1 * time.Hour),
	})
	audit := &stubAdminAuditRepo{}
	s := NewServer(
		WithCrossProjectCallRepository(repo),
		WithAdminAuditRepository(audit),
	)

	form := strings.NewReader("reason=stuck+over+timeout")
	req := httptest.NewRequest(http.MethodPost, "/admin/cpc/cpc_stuck/cancel", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.adminRouter(rec, withAdminUI(req))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: want 303 redirect, got %d", rec.Code)
	}
	if got := rec.Header().Get("Location"); !strings.Contains(got, "cancelled=cpc_stuck") {
		t.Errorf("redirect location should preserve cancelled banner; got %q", got)
	}
	if r, _ := repo.Get(context.Background(), "cpc_stuck"); r == nil || r.Status != persistence.CPCStatusRejected {
		t.Errorf("repo row should be rejected after cancel; got %+v", r)
	}
	if len(audit.rows) != 1 || audit.rows[0].Action != "interproject.cpc.admincancel" {
		t.Errorf("audit row should be written with cancel action; got %+v", audit.rows)
	}
}

// TestAdminCPC_CancelTerminalRow returns 404 when the row doesn't
// exist — the cancel handler shouldn't no-op silently on typos.
func TestAdminCPC_CancelMissingRow(t *testing.T) {
	repo := newStubCPCRepo()
	s := NewServer(WithCrossProjectCallRepository(repo))
	req := httptest.NewRequest(http.MethodPost, "/admin/cpc/no-such/cancel", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.adminRouter(rec, withAdminUI(req))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: want 404, got %d", rec.Code)
	}
}

// TestCPCRowToAdminRow_DurationLabels pins the duration phrasing —
// "resolved in X" for terminal rows, "running for X" for live ones.
func TestCPCRowToAdminRow_DurationLabels(t *testing.T) {
	now := time.Now()
	resolved := now
	live := &persistence.CrossProjectCall{
		ID:        "live",
		Status:    persistence.CPCStatusRunning,
		CreatedAt: now.Add(-90 * time.Second),
	}
	done := &persistence.CrossProjectCall{
		ID:         "done",
		Status:     persistence.CPCStatusCompleted,
		CreatedAt:  now.Add(-3 * time.Minute),
		ResolvedAt: &resolved,
	}
	liveRow := cpcRowToAdminRow(live, now)
	doneRow := cpcRowToAdminRow(done, now)
	if !strings.HasPrefix(liveRow.DurationLabel, "running for") {
		t.Errorf("live row should say 'running for'; got %q", liveRow.DurationLabel)
	}
	if liveRow.IsTerminal {
		t.Errorf("live row should not be terminal")
	}
	if !strings.HasPrefix(doneRow.DurationLabel, "resolved in") {
		t.Errorf("done row should say 'resolved in'; got %q", doneRow.DurationLabel)
	}
	if !doneRow.IsTerminal {
		t.Errorf("completed row should be terminal")
	}
}

// TestHumanDuration_Coarse pins the coarse buckets so future
// edits don't accidentally break the "3m 12s" / "1h 24m" outputs
// the template depends on.
func TestHumanDuration_Coarse(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{500 * time.Millisecond, "0s"},
		{5 * time.Second, "5s"},
		{90 * time.Second, "1m 30s"},
		{3 * time.Minute, "3m"},
		{125 * time.Minute, "2h 5m"},
		{2 * time.Hour, "2h"},
		{50 * time.Hour, "2d 2h"},
	}
	for _, tc := range cases {
		got := humanDuration(tc.d)
		if got != tc.want {
			t.Errorf("humanDuration(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}
