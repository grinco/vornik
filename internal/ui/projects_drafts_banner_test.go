package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/persistence"
)

type stubWizardLister struct {
	rows []*persistence.ProjectWizardSession
	err  error
}

func authDisabledUIRequest(req *http.Request) *http.Request {
	var captured *http.Request
	api.AuthMiddleware(api.AuthConfig{Enabled: false})(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = r
	})).ServeHTTP(httptest.NewRecorder(), req)
	if captured == nil {
		return req
	}
	return captured
}

func (s *stubWizardLister) ListByOperator(_ context.Context, operatorID string, _ int) ([]*persistence.ProjectWizardSession, error) {
	if s.err != nil {
		return nil, s.err
	}
	out := make([]*persistence.ProjectWizardSession, 0, len(s.rows))
	for _, row := range s.rows {
		if row != nil && row.OperatorID == operatorID {
			out = append(out, row)
		}
	}
	return out, nil
}

func TestProjects_DraftsBannerHiddenWhenUnwired(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/ui/projects", nil)
	rec := httptest.NewRecorder()
	srv.Projects(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "unfinished wizard") {
		t.Error("banner should not render when wizardSessions unwired")
	}
}

func TestProjects_DraftsBannerHiddenWhenNoOperatorID(t *testing.T) {
	lister := &stubWizardLister{rows: []*persistence.ProjectWizardSession{
		{ID: "pw_1", OperatorID: "op_1", UpdatedAt: time.Now()},
	}}
	srv := NewServer(WithWizardSessionLister(lister))
	req := httptest.NewRequest(http.MethodGet, "/ui/projects", nil)
	rec := httptest.NewRecorder()
	srv.Projects(rec, req)
	// Without X-Operator-Id header the operator is anonymous, so
	// the banner stays hidden.
	if strings.Contains(rec.Body.String(), "unfinished wizard") {
		t.Error("banner should not render without operator id")
	}
}

func TestProjects_DraftsBannerVisibleWithDrafts(t *testing.T) {
	now := time.Now()
	lister := &stubWizardLister{rows: []*persistence.ProjectWizardSession{
		{ID: "pw_1", OperatorID: "op_1", UpdatedAt: now.Add(-30 * time.Minute)},
		{ID: "pw_2", OperatorID: "op_1", UpdatedAt: now.Add(-2 * time.Hour)},
	}}
	srv := NewServer(WithWizardSessionLister(lister))
	req := httptest.NewRequest(http.MethodGet, "/ui/projects", nil)
	req.Header.Set("X-Operator-Id", "op_1")
	req = authDisabledUIRequest(req)
	rec := httptest.NewRecorder()
	srv.Projects(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "2 unfinished wizard drafts") {
		t.Errorf("expected pluralised banner text, body: %s", body)
	}
	if !strings.Contains(body, "Resume wizard") {
		t.Errorf("expected resume link, body: %s", body)
	}
}

func TestProjects_DraftsBannerIgnoresSpoofedHeaderWhenAuthEnabled(t *testing.T) {
	lister := &stubWizardLister{rows: []*persistence.ProjectWizardSession{
		{ID: "pw_1", OperatorID: "victim", UpdatedAt: time.Now()},
	}}
	srv := NewServer(WithWizardSessionLister(lister))
	req := scopedUIRequest(http.MethodGet, "/ui/projects", []string{"project-a"})
	req.Header.Set("X-Operator-Id", "victim")
	rec := httptest.NewRecorder()
	srv.Projects(rec, req)
	if strings.Contains(rec.Body.String(), "unfinished wizard") {
		t.Fatalf("spoofed X-Operator-Id rendered another operator's drafts")
	}
}

func TestProjects_DraftsBannerCountIgnoresCommitted(t *testing.T) {
	now := time.Now()
	committed := "already-shipped"
	lister := &stubWizardLister{rows: []*persistence.ProjectWizardSession{
		{ID: "pw_1", OperatorID: "op_1", UpdatedAt: now, CommittedProjectID: &committed},
		{ID: "pw_2", OperatorID: "op_1", UpdatedAt: now},
	}}
	srv := NewServer(WithWizardSessionLister(lister))
	req := httptest.NewRequest(http.MethodGet, "/ui/projects", nil)
	req.Header.Set("X-Operator-Id", "op_1")
	req = authDisabledUIRequest(req)
	rec := httptest.NewRecorder()
	srv.Projects(rec, req)
	body := rec.Body.String()
	// Only the uncommitted row counts.
	if !strings.Contains(body, "1 unfinished wizard draft") {
		t.Errorf("expected singular banner text, body: %s", body)
	}
}

func TestProjects_DraftsBannerCountIgnoresCancelled(t *testing.T) {
	now := time.Now()
	cancelledAt := now.Add(-10 * time.Minute)
	lister := &stubWizardLister{rows: []*persistence.ProjectWizardSession{
		// A cancelled draft must NOT count — otherwise the banner keeps
		// nagging after the operator hit Cancel in the wizard.
		{ID: "pw_cancelled", OperatorID: "op_1", UpdatedAt: now, CancelledAt: &cancelledAt},
		{ID: "pw_live", OperatorID: "op_1", UpdatedAt: now},
	}}
	srv := NewServer(WithWizardSessionLister(lister))
	req := httptest.NewRequest(http.MethodGet, "/ui/projects", nil)
	req.Header.Set("X-Operator-Id", "op_1")
	req = authDisabledUIRequest(req)
	rec := httptest.NewRecorder()
	srv.Projects(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "1 unfinished wizard draft") {
		t.Errorf("cancelled draft must not count; expected singular banner, body: %s", body)
	}
}

// humanAgo's exact format is defined in documents.go and shared
// across the UI; we just sanity-check that the banner's render
// path can call it without panicking. Format assertions live in
// documents-package tests if any.
func TestHumanAgoSanity(t *testing.T) {
	now := time.Now()
	if humanAgo(now.Add(-5*time.Minute)) == "" {
		t.Error("humanAgo should return non-empty for recent timestamps")
	}
}
