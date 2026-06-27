package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
)

// stubAdminAuditRepo backs the api handler test. Insert / List only.
type stubAdminAuditRepo struct {
	rows []*persistence.AdminAuditEntry
}

func (s *stubAdminAuditRepo) Insert(_ context.Context, e *persistence.AdminAuditEntry) error {
	cp := *e
	if cp.ID == "" {
		cp.ID = "admaud-" + e.Action
	}
	s.rows = append(s.rows, &cp)
	return nil
}

func (s *stubAdminAuditRepo) List(_ context.Context, filter persistence.AdminAuditFilter) ([]*persistence.AdminAuditEntry, error) {
	if filter.PageSize <= 0 {
		filter.PageSize = 50
	}
	out := make([]*persistence.AdminAuditEntry, 0, len(s.rows))
	for i := len(s.rows) - 1; i >= 0; i-- {
		e := s.rows[i]
		if filter.Action != "" && e.Action != filter.Action {
			continue
		}
		out = append(out, e)
		if len(out) == filter.PageSize {
			break
		}
	}
	return out, nil
}

// withAdminKeyContext stamps the supplied API key onto the request
// context so AdminAuditList can read it. Matches what AuthMiddleware
// does in production.
func withAdminKeyContext(r *http.Request, key string) *http.Request {
	if key == "" {
		return r
	}
	ctx := context.WithValue(r.Context(), apiKeyKey, key)
	return r.WithContext(ctx)
}

// TestAdminAuditList_Disabled returns 404 — the surface is hidden.
func TestAdminAuditList_Disabled(t *testing.T) {
	s := NewServer(WithAdminConfig(config.AdminConfig{Enabled: false}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/audit", nil)
	rec := httptest.NewRecorder()
	s.AdminAuditList(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("disabled: want 404, got %d", rec.Code)
	}
}

// TestAdminAuditList_EnabledNoRepo — 503.
func TestAdminAuditList_EnabledNoRepo(t *testing.T) {
	s := NewServer(WithAdminConfig(config.AdminConfig{
		Enabled: true, AllowedKeys: []string{"sk-admin"},
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/audit", nil)
	rec := httptest.NewRecorder()
	s.AdminAuditList(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no repo: want 503, got %d", rec.Code)
	}
}

// TestAdminAuditList_NoKey — 401.
func TestAdminAuditList_NoKey(t *testing.T) {
	repo := &stubAdminAuditRepo{}
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithAdminAuditRepository(repo),
	)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/audit", nil)
	rec := httptest.NewRecorder()
	s.AdminAuditList(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key: want 401, got %d", rec.Code)
	}
}

// TestAdminAuditList_NonAdmin — 403.
func TestAdminAuditList_NonAdmin(t *testing.T) {
	repo := &stubAdminAuditRepo{}
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithAdminAuditRepository(repo),
	)
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodGet, "/api/v1/admin/audit", nil),
		"sk-user")
	rec := httptest.NewRecorder()
	s.AdminAuditList(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin: want 403, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "admin scope required") {
		t.Errorf("body: missing scope error; got %q", rec.Body.String())
	}
}

// TestAdminAuditList_Admin — happy path returns the JSON shape.
func TestAdminAuditList_Admin(t *testing.T) {
	repo := &stubAdminAuditRepo{}
	_ = repo.Insert(context.Background(), &persistence.AdminAuditEntry{
		Principal: "sk-admin", Source: "ui", Action: "mcp.refresh", Target: "p-1",
	})
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithAdminAuditRepository(repo),
	)
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodGet, "/api/v1/admin/audit", nil),
		"sk-admin")
	rec := httptest.NewRecorder()
	s.AdminAuditList(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin: want 200, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	var resp AdminAuditListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (body=%q)", err, rec.Body.String())
	}
	if len(resp.Entries) != 1 {
		t.Fatalf("entries: want 1, got %d", len(resp.Entries))
	}
	if resp.Entries[0].Action != "mcp.refresh" {
		t.Errorf("Action: got %q", resp.Entries[0].Action)
	}
}

// TestAdminAuditList_LimitClamp — the handler clamps to [1,500].
func TestAdminAuditList_LimitClamp(t *testing.T) {
	repo := &stubAdminAuditRepo{}
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithAdminAuditRepository(repo),
	)
	for _, raw := range []string{"abc", "10", "9999", "0"} {
		req := withAdminKeyContext(
			httptest.NewRequest(http.MethodGet, "/api/v1/admin/audit?limit="+raw, nil),
			"sk-admin")
		rec := httptest.NewRecorder()
		s.AdminAuditList(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("limit=%q: status %d", raw, rec.Code)
		}
	}
}

// TestAdminAuditList_SinceFilter — both date forms accepted.
func TestAdminAuditList_SinceFilter(t *testing.T) {
	repo := &stubAdminAuditRepo{}
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithAdminAuditRepository(repo),
	)
	for _, since := range []string{"2026-05-01", "2026-05-01T00:00:00Z", "garbage"} {
		req := withAdminKeyContext(
			httptest.NewRequest(http.MethodGet, "/api/v1/admin/audit?since="+since, nil),
			"sk-admin")
		rec := httptest.NewRecorder()
		s.AdminAuditList(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("since=%q: status %d", since, rec.Code)
		}
	}
}

// failingRepo returns an error from List.
type failingAdminRepo struct{}

func (failingAdminRepo) Insert(context.Context, *persistence.AdminAuditEntry) error { return nil }
func (failingAdminRepo) List(context.Context, persistence.AdminAuditFilter) ([]*persistence.AdminAuditEntry, error) {
	return nil, errStub("db down")
}

type errStub string

func (e errStub) Error() string { return string(e) }

// TestAdminAuditList_RepoError — 500 with INTERNAL code.
func TestAdminAuditList_RepoError(t *testing.T) {
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithAdminAuditRepository(failingAdminRepo{}),
	)
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodGet, "/api/v1/admin/audit", nil),
		"sk-admin")
	rec := httptest.NewRecorder()
	s.AdminAuditList(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: want 500, got %d", rec.Code)
	}
}

// TestAPIKeyFromContext returns the stamped key.
func TestAPIKeyFromContext(t *testing.T) {
	req := withAdminKeyContext(httptest.NewRequest(http.MethodGet, "/", nil), "sk-abc")
	if got := APIKeyFromContext(req.Context()); got != "sk-abc" {
		t.Errorf("got %q", got)
	}
	if got := APIKeyFromContext(context.Background()); got != "" {
		t.Errorf("background ctx: want \"\", got %q", got)
	}
	// Defensive: nil context.
	//nolint:staticcheck // intentional nil-context check
	if got := APIKeyFromContext(nil); got != "" {
		t.Errorf("nil ctx: want \"\", got %q", got)
	}
}
