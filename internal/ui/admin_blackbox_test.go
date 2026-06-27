package ui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/contracts"
)

// stubUIBlackBoxTraceService is a test double for UIBlackBoxTraceService.
// It controls what AssembleCached returns so tests stay fully within the
// ui package without importing internal/blackbox.
type stubUIBlackBoxTraceService struct {
	// trace is what AssembleCached returns (nil → ErrBlackBoxTaskNotFound).
	trace  any
	err    error
	cached bool
}

func (s *stubUIBlackBoxTraceService) AssembleCached(_ context.Context, _ string) (any, bool, error) {
	if s.err != nil {
		return nil, false, s.err
	}
	if s.trace == nil {
		return nil, false, contracts.ErrBlackBoxTaskNotFound
	}
	return s.trace, s.cached, nil
}

func (s *stubUIBlackBoxTraceService) Compare(_, _ any) (any, error) {
	return nil, nil
}

// fakeTrace is an opaque struct whose exported fields the template can
// render through reflect. We keep it minimal — only fields the templates
// actually reference need to be populated.
type fakeTrace struct {
	Header fakeTraceHeader
	Events []fakeEvent
}

type fakeTraceHeader struct {
	TaskID  string
	Status  string
	CostUSD float64
}

type fakeEvent struct {
	Kind      string
	Title     string
	CostUSD   float64
	Timestamp time.Time
}

// TestAdminBlackBox_IndexRendersWithServiceWired confirms the
// landing page renders the search box when the service is
// configured. The "not configured" notice should be absent.
func TestAdminBlackBox_IndexRendersWithServiceWired(t *testing.T) {
	svc := &stubUIBlackBoxTraceService{trace: &fakeTrace{}}
	srv := NewServer(WithBlackBoxService(svc))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ui/admin/blackbox", nil)
	srv.AdminBlackBox(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status=%d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "not configured on this deployment") {
		t.Errorf("page rendered the 'not configured' notice despite the service being wired")
	}
	if !strings.Contains(body, "Task ID") {
		t.Errorf("page missing the search-box label")
	}
}

// TestAdminBlackBox_IndexWithoutServiceShowsNotice: when the
// service is unwired (SQLite deployment), the page must show
// the operator-friendly "trace service not configured" notice
// instead of rendering an empty search form.
func TestAdminBlackBox_IndexWithoutServiceShowsNotice(t *testing.T) {
	srv := NewServer() // no WithBlackBoxService
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ui/admin/blackbox", nil)
	srv.AdminBlackBox(rec, req)

	if !strings.Contains(rec.Body.String(), "not configured on this deployment") {
		t.Errorf("page should show 'not configured' notice when service is nil")
	}
}

// TestAdminBlackBoxTrace_HappyPath renders the trace view for a
// task with a populated trace. Asserts the task_id is present
// (operators cite it) in the response.
func TestAdminBlackBoxTrace_HappyPath(t *testing.T) {
	tr := &fakeTrace{
		Header: fakeTraceHeader{TaskID: "task_x", Status: "completed", CostUSD: 0.0123},
		Events: []fakeEvent{
			{Kind: "llm_call", Title: "llm call", CostUSD: 0.0123,
				Timestamp: time.Date(2026, 5, 26, 9, 0, 0, 0, time.UTC)},
		},
	}
	svc := &stubUIBlackBoxTraceService{trace: tr}
	srv := NewServer(WithBlackBoxService(svc))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ui/admin/blackbox/task_x", nil)
	srv.AdminBlackBoxTrace(rec, req, "task_x")

	if rec.Code != http.StatusOK {
		t.Errorf("status=%d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "task_x") {
		t.Errorf("body missing task_id; got %q", body[:min(200, len(body))])
	}
}

// TestAdminBlackBoxTrace_TaskNotFound shows the "no audit data"
// message when AssembleCached returns ErrBlackBoxTaskNotFound.
func TestAdminBlackBoxTrace_TaskNotFound(t *testing.T) {
	svc := &stubUIBlackBoxTraceService{} // nil trace → ErrBlackBoxTaskNotFound
	srv := NewServer(WithBlackBoxService(svc))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ui/admin/blackbox/missing", nil)
	srv.AdminBlackBoxTrace(rec, req, "missing")

	if !strings.Contains(rec.Body.String(), "No audit data") {
		t.Errorf("body should show 'No audit data' for missing task; got %q", rec.Body.String())
	}
}

// TestAdminBlackBoxTrace_PropagatesError exercises the catch-
// all error branch. A non-ErrBlackBoxTaskNotFound source error
// should render the operator-friendly error banner, not 500.
func TestAdminBlackBoxTrace_PropagatesError(t *testing.T) {
	svc := &stubUIBlackBoxTraceService{err: errors.New("postgres down")}
	srv := NewServer(WithBlackBoxService(svc))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ui/admin/blackbox/task_x", nil)
	srv.AdminBlackBoxTrace(rec, req, "task_x")

	if !strings.Contains(rec.Body.String(), "Trace assembly failed") {
		t.Errorf("body should show assembly-failed banner; got %q", rec.Body.String())
	}
}
