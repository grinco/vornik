package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
)

// stubChatAuditRepo backs the API handler tests. List + minimal
// Insert / SavePrompt / GetPrompt stubs so the persistence.ChatAuditRepository
// interface is satisfied without dragging the postgres implementation
// into the api unit tests.
type stubChatAuditAdminRepo struct {
	rows    []*persistence.ChatAuditEntry
	listErr error
}

func (s *stubChatAuditAdminRepo) Insert(_ context.Context, e *persistence.ChatAuditEntry) error {
	cp := *e
	if cp.ID == "" {
		cp.ID = "chat-" + e.ChatID
	}
	s.rows = append(s.rows, &cp)
	return nil
}

func (s *stubChatAuditAdminRepo) List(_ context.Context, filter persistence.ChatAuditFilter) ([]*persistence.ChatAuditEntry, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	if filter.PageSize <= 0 {
		filter.PageSize = 50
	}
	out := make([]*persistence.ChatAuditEntry, 0, len(s.rows))
	// Newest first to match the production repo's contract.
	for i := len(s.rows) - 1; i >= 0; i-- {
		e := s.rows[i]
		if filter.ChatID != "" && e.ChatID != filter.ChatID {
			continue
		}
		if filter.ProjectID != "" && e.ProjectID != filter.ProjectID {
			continue
		}
		if !filter.Since.IsZero() && e.Timestamp.Before(filter.Since) {
			continue
		}
		out = append(out, e)
		if len(out) == filter.PageSize {
			break
		}
	}
	return out, nil
}

func (s *stubChatAuditAdminRepo) SavePrompt(_ context.Context, _, _ string) error { return nil }
func (s *stubChatAuditAdminRepo) GetPrompt(_ context.Context, _ string) (string, error) {
	return "", persistence.ErrNotFound
}

func TestAdminChatAuditList_Disabled(t *testing.T) {
	s := NewServer(WithAdminConfig(config.AdminConfig{Enabled: false}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/chat-audit", nil)
	rec := httptest.NewRecorder()
	s.AdminChatAuditList(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("disabled: want 404, got %d", rec.Code)
	}
}

func TestAdminChatAuditList_EnabledNoRepo(t *testing.T) {
	s := NewServer(WithAdminConfig(config.AdminConfig{
		Enabled: true, AllowedKeys: []string{"sk-admin"},
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/chat-audit", nil)
	rec := httptest.NewRecorder()
	s.AdminChatAuditList(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no repo: want 503, got %d", rec.Code)
	}
}

func TestAdminChatAuditList_NoKey(t *testing.T) {
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithChatAuditRepository(&stubChatAuditAdminRepo{}),
	)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/chat-audit", nil)
	rec := httptest.NewRecorder()
	s.AdminChatAuditList(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key: want 401, got %d", rec.Code)
	}
}

func TestAdminChatAuditList_NonAdmin(t *testing.T) {
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithChatAuditRepository(&stubChatAuditAdminRepo{}),
	)
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodGet, "/api/v1/admin/chat-audit", nil),
		"sk-user")
	rec := httptest.NewRecorder()
	s.AdminChatAuditList(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin: want 403, got %d", rec.Code)
	}
}

func TestAdminChatAuditList_Admin(t *testing.T) {
	repo := &stubChatAuditAdminRepo{}
	now := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	_ = repo.Insert(context.Background(), &persistence.ChatAuditEntry{
		Timestamp:  now,
		ChatID:     "telegram:42",
		ProjectID:  "p-1",
		RoleUsed:   "lead",
		Model:      "claude-sonnet-4-6",
		Iterations: 3,
		DurationMs: 1234,
		CostUSD:    0.0021,
	})
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithChatAuditRepository(repo),
	)
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodGet, "/api/v1/admin/chat-audit", nil),
		"sk-admin")
	rec := httptest.NewRecorder()
	s.AdminChatAuditList(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin: want 200, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	var resp AdminChatAuditListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Entries) != 1 {
		t.Fatalf("entries: want 1, got %d", len(resp.Entries))
	}
	got := resp.Entries[0]
	if got.ChatID != "telegram:42" || got.Model != "claude-sonnet-4-6" {
		t.Fatalf("entry: %+v", got)
	}
}

func TestAdminChatAuditList_ChatFilter(t *testing.T) {
	repo := &stubChatAuditAdminRepo{}
	now := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	_ = repo.Insert(context.Background(), &persistence.ChatAuditEntry{Timestamp: now, ChatID: "a", ProjectID: "p"})
	_ = repo.Insert(context.Background(), &persistence.ChatAuditEntry{Timestamp: now, ChatID: "b", ProjectID: "p"})
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithChatAuditRepository(repo),
	)
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodGet, "/api/v1/admin/chat-audit?chat=a", nil),
		"sk-admin")
	rec := httptest.NewRecorder()
	s.AdminChatAuditList(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var resp AdminChatAuditListResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Entries) != 1 || resp.Entries[0].ChatID != "a" {
		t.Fatalf("filter failed: %+v", resp.Entries)
	}
}

func TestAdminChatAuditList_RepoError(t *testing.T) {
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithChatAuditRepository(&stubChatAuditAdminRepo{listErr: errStub("db down")}),
	)
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodGet, "/api/v1/admin/chat-audit", nil),
		"sk-admin")
	rec := httptest.NewRecorder()
	s.AdminChatAuditList(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: want 500, got %d", rec.Code)
	}
}

func (s *stubChatAuditAdminRepo) GetByID(_ context.Context, _ string) (*persistence.ChatAuditEntry, error) {
	return nil, persistence.ErrNotFound
}
