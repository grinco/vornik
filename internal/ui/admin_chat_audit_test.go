package ui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
)

// fakeChatAudit is an in-memory ChatAuditRepository that the
// AdminChatAudit handler tests script directly. List honours
// project + chat filters so the handler's filter-render branch
// is exercised.
type fakeChatAudit struct {
	rows    []*persistence.ChatAuditEntry
	prompts map[string]string
	listErr error
	getErr  error
}

func (f *fakeChatAudit) Insert(_ context.Context, _ *persistence.ChatAuditEntry) error { return nil }

func (f *fakeChatAudit) List(_ context.Context, filter persistence.ChatAuditFilter) ([]*persistence.ChatAuditEntry, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := []*persistence.ChatAuditEntry{}
	for _, e := range f.rows {
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
		if filter.PageSize > 0 && len(out) >= filter.PageSize {
			break
		}
	}
	return out, nil
}

func (f *fakeChatAudit) SavePrompt(_ context.Context, hash, body string) error {
	if f.prompts == nil {
		f.prompts = map[string]string{}
	}
	f.prompts[hash] = body
	return nil
}

func (f *fakeChatAudit) GetPrompt(_ context.Context, hash string) (string, error) {
	if f.getErr != nil {
		return "", f.getErr
	}
	return f.prompts[hash], nil
}

// TestAdminChatAudit_NoRepoRendersEmptyState — without the repo
// wired the page still loads with a "not wired" banner.
func TestAdminChatAudit_NoRepoRendersEmptyState(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/admin/chat-audit", nil)
	rec := httptest.NewRecorder()
	srv.AdminChatAudit(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "Chat Audit")
}

// TestAdminChatAudit_RendersRows — happy path with a couple of
// entries. The rendered body shows the chat_id and the project_id.
func TestAdminChatAudit_RendersRows(t *testing.T) {
	repo := &fakeChatAudit{
		rows: []*persistence.ChatAuditEntry{
			{ID: "chat_1", ChatID: "555", ProjectID: "p1", Model: "test-model", Timestamp: time.Now()},
			{ID: "chat_2", ChatID: "777", ProjectID: "p2", Model: "test-model", Timestamp: time.Now()},
		},
	}
	srv := NewServer(WithAdminChatAuditRepository(repo))
	req := httptest.NewRequest(http.MethodGet, "/admin/chat-audit", nil)
	rec := httptest.NewRecorder()
	srv.AdminChatAudit(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "555")
	assert.Contains(t, body, "p1")
}

// TestAdminChatAudit_PageSizeSelectorPreservesFilters — the page
// migrated from inline Show to the shared pageSizeSelector
// partial; the selector now lives in its own form below the
// filter inputs and must thread chat/project/since through as
// hidden inputs so a Show-change doesn't reset the operator's
// filter view.
func TestAdminChatAudit_PageSizeSelectorPreservesFilters(t *testing.T) {
	srv := NewServer(WithAdminChatAuditRepository(&fakeChatAudit{}))
	req := httptest.NewRequest(http.MethodGet,
		"/admin/chat-audit?chat=559741208&project=janka&since=2026-05-01&limit=50",
		nil)
	rec := httptest.NewRecorder()
	srv.AdminChatAudit(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	for _, want := range []string{
		`action="/ui/admin/chat-audit"`,
		`name="limit"`,
		`name="chat" value="559741208"`,
		`name="project" value="janka"`,
		`name="since" value="2026-05-01"`,
	} {
		assert.Contains(t, body, want, "selector wiring missing %q", want)
	}
}

// TestAdminChatAudit_ProjectFilter — supplying ?project=p1 must
// filter the list to that project only.
func TestAdminChatAudit_ProjectFilter(t *testing.T) {
	repo := &fakeChatAudit{
		rows: []*persistence.ChatAuditEntry{
			{ID: "chat_a", ChatID: "1", ProjectID: "p1", Model: "x", Timestamp: time.Now()},
			{ID: "chat_b", ChatID: "2", ProjectID: "p2", Model: "x", Timestamp: time.Now()},
		},
	}
	srv := NewServer(WithAdminChatAuditRepository(repo))
	req := httptest.NewRequest(http.MethodGet, "/admin/chat-audit?project=p1", nil)
	rec := httptest.NewRecorder()
	srv.AdminChatAudit(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	// p1 row id renders in table; p2 row id should be absent.
	assert.Contains(t, body, "chat_a")
	assert.NotContains(t, body, "chat_b")
}

// TestAdminChatAudit_SinceFilterAcceptsRFC3339 — both RFC3339 and
// YYYY-MM-DD parse paths must work without 500-ing.
func TestAdminChatAudit_SinceFilterAcceptsRFC3339(t *testing.T) {
	repo := &fakeChatAudit{}
	srv := NewServer(WithAdminChatAuditRepository(repo))

	cases := []string{
		"2026-05-01T00:00:00Z",
		"2026-05-01",
	}
	for _, since := range cases {
		req := httptest.NewRequest(http.MethodGet, "/admin/chat-audit?since="+since, nil)
		rec := httptest.NewRecorder()
		srv.AdminChatAudit(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code, "since=%s should render 200", since)
	}
}

// TestAdminChatAudit_DrillDownLoadsPrompt — passing ?id=<row>
// selects that row and renders the system prompt body inline.
func TestAdminChatAudit_DrillDownLoadsPrompt(t *testing.T) {
	repo := &fakeChatAudit{
		rows: []*persistence.ChatAuditEntry{
			{ID: "row-x", ChatID: "1", ProjectID: "p1", SystemPromptHash: "abc123",
				UserMessage: "test prompt", Response: "ack", Timestamp: time.Now()},
		},
		prompts: map[string]string{"abc123": "you are a helpful assistant"},
	}
	srv := NewServer(WithAdminChatAuditRepository(repo))
	req := httptest.NewRequest(http.MethodGet, "/admin/chat-audit?id=row-x", nil)
	rec := httptest.NewRecorder()
	srv.AdminChatAudit(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	// The prompt body should appear when drill-down fired.
	assert.Contains(t, body, "you are a helpful assistant",
		"prompt body should render in drill-down panel")
}

// TestAdminChatAudit_ListErrorStillRenders — repo List error
// gets logged and the page still 200s with the empty list.
func TestAdminChatAudit_ListErrorStillRenders(t *testing.T) {
	repo := &fakeChatAudit{listErr: errors.New("db down")}
	srv := NewServer(WithAdminChatAuditRepository(repo))
	req := httptest.NewRequest(http.MethodGet, "/admin/chat-audit", nil)
	rec := httptest.NewRecorder()
	srv.AdminChatAudit(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	// Page renders without leaking the error.
	assert.NotContains(t, strings.ToLower(rec.Body.String()), "db down")
}

func (f *fakeChatAudit) GetByID(_ context.Context, _ string) (*persistence.ChatAuditEntry, error) {
	return nil, persistence.ErrNotFound
}
