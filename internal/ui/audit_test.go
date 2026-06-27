package ui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
)

// fakeToolAuditRepo stubs ToolAuditRepository. Audit only reads via
// List + the optional CountByTool; Log writes are observed via the
// inserts slice for follow-on tests.
type fakeToolAuditRepo struct {
	rows    []*persistence.ToolAuditEntry
	listErr error
}

func (f *fakeToolAuditRepo) Log(context.Context, *persistence.ToolAuditEntry) error { return nil }
func (f *fakeToolAuditRepo) List(_ context.Context, filter persistence.ToolAuditFilter) ([]*persistence.ToolAuditEntry, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	if filter.PageSize > 0 && len(f.rows) > filter.PageSize {
		return f.rows[:filter.PageSize], nil
	}
	return f.rows, nil
}
func (f *fakeToolAuditRepo) CountByTool(context.Context, string) (map[string]int64, error) {
	return nil, nil
}

// fakeWebhookEventRepo stubs WebhookEventRepository for the Audit
// page's lower section.
type fakeWebhookEventRepo struct {
	rows    []*persistence.WebhookEvent
	listErr error
}

func (f *fakeWebhookEventRepo) Record(context.Context, *persistence.WebhookEvent) error { return nil }
func (f *fakeWebhookEventRepo) List(_ context.Context, filter persistence.WebhookEventFilter) ([]*persistence.WebhookEvent, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	if filter.PageSize > 0 && len(f.rows) > filter.PageSize {
		return f.rows[:filter.PageSize], nil
	}
	return f.rows, nil
}

func TestAudit_NoReposRendersEmpty(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/audit", nil)
	rec := httptest.NewRecorder()
	srv.Audit(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "Audit")
}

func TestAudit_WithToolEntries(t *testing.T) {
	repo := &fakeToolAuditRepo{
		rows: []*persistence.ToolAuditEntry{
			{ID: "tool-1", ToolName: "file_read", ExecutionID: "exec-a", CreatedAt: time.Now()},
			{ID: "tool-2", ToolName: "grep", ExecutionID: "exec-b", CreatedAt: time.Now()},
		},
	}
	srv := NewServer(WithToolAuditRepository(repo))
	req := httptest.NewRequest(http.MethodGet, "/audit", nil)
	rec := httptest.NewRecorder()
	srv.Audit(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "file_read")
	assert.Contains(t, body, "grep")
}

func TestAudit_WithWebhookEvents(t *testing.T) {
	repo := &fakeWebhookEventRepo{
		rows: []*persistence.WebhookEvent{
			{ID: "wh-1", Source: "github", Status: "accepted", CreatedAt: time.Now()},
		},
	}
	srv := NewServer(WithWebhookEventRepository(repo))
	req := httptest.NewRequest(http.MethodGet, "/audit", nil)
	rec := httptest.NewRecorder()
	srv.Audit(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "github")
}

func TestAudit_RespectsLimitParam(t *testing.T) {
	rows := make([]*persistence.ToolAuditEntry, 50)
	for i := range rows {
		rows[i] = &persistence.ToolAuditEntry{ID: "tool-x", ToolName: "file_read", ExecutionID: "exec-1", CreatedAt: time.Now()}
	}
	repo := &fakeToolAuditRepo{rows: rows}
	srv := NewServer(WithToolAuditRepository(repo))
	req := httptest.NewRequest(http.MethodGet, "/audit?limit=10", nil)
	rec := httptest.NewRecorder()
	srv.Audit(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	// limit=10 → selector shows 10 selected.
	assert.Contains(t, rec.Body.String(), `value="10" selected`)
}

func TestAudit_ListErrorsStillRender(t *testing.T) {
	repo := &fakeToolAuditRepo{listErr: errors.New("db down")}
	whrepo := &fakeWebhookEventRepo{listErr: errors.New("db down too")}
	srv := NewServer(WithToolAuditRepository(repo), WithWebhookEventRepository(whrepo))
	req := httptest.NewRequest(http.MethodGet, "/audit", nil)
	rec := httptest.NewRecorder()
	srv.Audit(rec, req)
	// Errors don't leak into the body, page still renders.
	require.Equal(t, http.StatusOK, rec.Code)
}
