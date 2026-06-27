package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
)

// stubCPCAdminRepo is a focused mock for the admin-handler
// tests. Implements only the methods the handlers actually
// call (Get + List + AdminCancel); the executor-side methods
// can stay no-ops because the admin surface never invokes
// them.
type stubCPCAdminRepo struct {
	mu          sync.Mutex
	rows        map[string]*persistence.CrossProjectCall
	cancelCalls int
}

func newStubCPCAdminRepo() *stubCPCAdminRepo {
	return &stubCPCAdminRepo{rows: map[string]*persistence.CrossProjectCall{}}
}
func (s *stubCPCAdminRepo) Create(context.Context, *persistence.CrossProjectCall) error {
	return nil
}
func (s *stubCPCAdminRepo) Get(_ context.Context, id string) (*persistence.CrossProjectCall, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.rows[id]; ok {
		cp := *r
		return &cp, nil
	}
	return nil, persistence.ErrNotFound
}
func (s *stubCPCAdminRepo) GetByCalleeTaskID(context.Context, string) (*persistence.CrossProjectCall, error) {
	return nil, persistence.ErrNotFound
}
func (s *stubCPCAdminRepo) SetCalleeTaskID(context.Context, string, string) error { return nil }
func (s *stubCPCAdminRepo) MarkRunning(context.Context, string) error             { return nil }
func (s *stubCPCAdminRepo) MarkCompleted(context.Context, string, []byte) error   { return nil }
func (s *stubCPCAdminRepo) MarkFailed(context.Context, string, string) error      { return nil }
func (s *stubCPCAdminRepo) MarkRejected(context.Context, string, string) error    { return nil }
func (s *stubCPCAdminRepo) ClaimTimedOut(context.Context, time.Time, int) ([]*persistence.CrossProjectCall, error) {
	return nil, nil
}
func (s *stubCPCAdminRepo) List(_ context.Context, filter persistence.CPCListFilter) ([]*persistence.CrossProjectCall, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []*persistence.CrossProjectCall{}
	for _, r := range s.rows {
		if filter.Status != "" && r.Status != filter.Status {
			continue
		}
		if filter.CallerProject != "" && r.CallerProject != filter.CallerProject {
			continue
		}
		cp := *r
		out = append(out, &cp)
	}
	return out, nil
}
func (s *stubCPCAdminRepo) AdminCancel(_ context.Context, id, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancelCalls++
	r, ok := s.rows[id]
	if !ok {
		return persistence.ErrNotFound
	}
	r.Status = persistence.CPCStatusRejected
	r.ErrorMessage = &reason
	return nil
}

// adminCPCServer constructs a Server with the admin gate
// enabled + the supplied admin key allowlisted. Mirrors the
// admin_handlers_test newServer helper.
func adminCPCServer(t *testing.T, repo *stubCPCAdminRepo, audit *stubAdminAuditRepo) *Server {
	t.Helper()
	opts := []ServerOption{
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin-x"}}),
	}
	if repo != nil {
		opts = append(opts, WithCrossProjectCallRepository(repo))
	}
	if audit != nil {
		opts = append(opts, WithAdminAuditRepository(audit))
	}
	return NewServer(opts...)
}

// TestAdminCPCList_Disabled — surface returns 404 when admin
// not enabled. Same hidden-by-default rule the audit endpoint
// uses.
func TestAdminCPCList_Disabled(t *testing.T) {
	s := NewServer(WithAdminConfig(config.AdminConfig{Enabled: false}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/cpc", nil)
	rec := httptest.NewRecorder()
	s.AdminCPCList(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("disabled = %d, want 404", rec.Code)
	}
}

// TestAdminCPCList_NoRepo — 503 when admin is enabled but the
// CPC repo isn't wired. Deployments without the feature flag
// see a clear message instead of a NULL panic.
func TestAdminCPCList_NoRepo(t *testing.T) {
	s := adminCPCServer(t, nil, nil)
	req := withAdminKeyContext(httptest.NewRequest(http.MethodGet, "/api/v1/admin/cpc", nil), "sk-admin-x")
	rec := httptest.NewRecorder()
	s.AdminCPCList(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no repo = %d, want 503", rec.Code)
	}
}

// TestAdminCPCList_NonAdminKey — 403 when the caller's key
// isn't in the admin allowlist.
func TestAdminCPCList_NonAdminKey(t *testing.T) {
	s := adminCPCServer(t, newStubCPCAdminRepo(), nil)
	req := withAdminKeyContext(httptest.NewRequest(http.MethodGet, "/api/v1/admin/cpc", nil), "sk-user-x")
	rec := httptest.NewRecorder()
	s.AdminCPCList(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin = %d, want 403", rec.Code)
	}
}

// TestAdminCPCList_FilterByStatus covers the status query
// param — the CLI's "show me stuck rows" use case.
func TestAdminCPCList_FilterByStatus(t *testing.T) {
	repo := newStubCPCAdminRepo()
	repo.rows["a"] = &persistence.CrossProjectCall{ID: "a", Status: persistence.CPCStatusRunning}
	repo.rows["b"] = &persistence.CrossProjectCall{ID: "b", Status: persistence.CPCStatusCompleted}
	repo.rows["c"] = &persistence.CrossProjectCall{ID: "c", Status: persistence.CPCStatusRunning}
	s := adminCPCServer(t, repo, nil)

	req := withAdminKeyContext(httptest.NewRequest(http.MethodGet, "/api/v1/admin/cpc?status=running", nil), "sk-admin-x")
	rec := httptest.NewRecorder()
	s.AdminCPCList(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status=running = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp CPCListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Entries) != 2 {
		t.Errorf("filter returned %d rows, want 2 (a + c)", len(resp.Entries))
	}
}

// TestAdminCPCShow_NotFound — clean 404 for unknown id.
func TestAdminCPCShow_NotFound(t *testing.T) {
	s := adminCPCServer(t, newStubCPCAdminRepo(), nil)
	req := withAdminKeyContext(httptest.NewRequest(http.MethodGet, "/api/v1/admin/cpc/missing", nil), "sk-admin-x")
	rec := httptest.NewRecorder()
	s.AdminCPCShow(rec, req, "missing")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("show missing = %d, want 404", rec.Code)
	}
}

// TestAdminCPCCancel_HappyPath asserts the cancel action flips
// the status, writes an audit row with the operator's principal,
// and returns the updated row.
func TestAdminCPCCancel_HappyPath(t *testing.T) {
	repo := newStubCPCAdminRepo()
	audit := &stubAdminAuditRepo{}
	repo.rows["ccp_stuck"] = &persistence.CrossProjectCall{
		ID: "ccp_stuck", CallerProject: "marketing", CalleeProject: "architect",
		Status: persistence.CPCStatusRunning,
	}
	s := adminCPCServer(t, repo, audit)

	body := strings.NewReader(`{"reason":"the architect went on vacation"}`)
	req := withAdminKeyContext(httptest.NewRequest(http.MethodPost, "/api/v1/admin/cpc/ccp_stuck/cancel", body), "sk-admin-x")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.AdminCPCCancel(rec, req, "ccp_stuck")

	if rec.Code != http.StatusOK {
		t.Fatalf("cancel = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if repo.cancelCalls != 1 {
		t.Errorf("AdminCancel called %d times, want 1", repo.cancelCalls)
	}
	if repo.rows["ccp_stuck"].Status != persistence.CPCStatusRejected {
		t.Errorf("status not flipped: %q", repo.rows["ccp_stuck"].Status)
	}
	if len(audit.rows) != 1 {
		t.Fatalf("expected 1 audit row, got %d", len(audit.rows))
	}
	entry := audit.rows[0]
	if entry.Action != "interproject.cpc.admincancel" {
		t.Errorf("audit action = %q, want interproject.cpc.admincancel", entry.Action)
	}
	wantPrincipal := "api_key_sha256:" + apikey.Hash("sk-admin-x")[:16]
	if entry.Principal != wantPrincipal {
		t.Errorf("audit principal = %q, want %q", entry.Principal, wantPrincipal)
	}
	if !strings.Contains(entry.After, "the architect went on vacation") {
		t.Errorf("audit After should include reason: %q", entry.After)
	}
}

// TestAdminCPCCancel_DefaultReason — empty body still works,
// gets the default operator-cancel message.
func TestAdminCPCCancel_DefaultReason(t *testing.T) {
	repo := newStubCPCAdminRepo()
	audit := &stubAdminAuditRepo{}
	repo.rows["ccp_x"] = &persistence.CrossProjectCall{
		ID: "ccp_x", Status: persistence.CPCStatusPending,
	}
	s := adminCPCServer(t, repo, audit)
	req := withAdminKeyContext(httptest.NewRequest(http.MethodPost, "/api/v1/admin/cpc/ccp_x/cancel", strings.NewReader("")), "sk-admin-x")
	rec := httptest.NewRecorder()
	s.AdminCPCCancel(rec, req, "ccp_x")
	if rec.Code != http.StatusOK {
		t.Fatalf("default-reason cancel = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if msg := repo.rows["ccp_x"].ErrorMessage; msg == nil || !strings.Contains(*msg, "operator-cancelled") {
		t.Errorf("expected default operator-cancel message, got %v", msg)
	}
}

// TestAdminCPCRouter_DispatchesPaths covers the path-peeling
// router that backs /admin/cpc/{id} and /admin/cpc/{id}/cancel.
// Pins the dispatch so future operators adding a new sub-
// resource don't accidentally break the existing routes.
func TestAdminCPCRouter_DispatchesPaths(t *testing.T) {
	repo := newStubCPCAdminRepo()
	repo.rows["ccp_present"] = &persistence.CrossProjectCall{ID: "ccp_present", Status: persistence.CPCStatusRunning}
	s := adminCPCServer(t, repo, &stubAdminAuditRepo{})

	cases := []struct {
		name   string
		method string
		path   string
		want   int
	}{
		{"show_ok", http.MethodGet, "/api/v1/admin/cpc/ccp_present", http.StatusOK},
		{"show_not_found", http.MethodGet, "/api/v1/admin/cpc/ccp_missing", http.StatusNotFound},
		{"cancel_post", http.MethodPost, "/api/v1/admin/cpc/ccp_present/cancel", http.StatusOK},
		{"cancel_wrong_method", http.MethodGet, "/api/v1/admin/cpc/ccp_present/cancel", http.StatusMethodNotAllowed},
		{"unknown_subresource", http.MethodGet, "/api/v1/admin/cpc/ccp_present/unknown", http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Re-seed the repo between subtests because the
			// cancel one mutates the row.
			repo.rows["ccp_present"] = &persistence.CrossProjectCall{ID: "ccp_present", Status: persistence.CPCStatusRunning}
			req := withAdminKeyContext(httptest.NewRequest(c.method, c.path, nil), "sk-admin-x")
			rec := httptest.NewRecorder()
			s.adminCPCRouter(rec, req)
			if rec.Code != c.want {
				t.Errorf("%s %s = %d, want %d; body=%s", c.method, c.path, rec.Code, c.want, rec.Body.String())
			}
		})
	}
}
