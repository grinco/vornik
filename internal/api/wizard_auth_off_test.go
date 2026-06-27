package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/projectwizard"
)

// Gap 3 regression — the wizard handlers used to 401 every request
// without X-Operator-Id, which made the CLI unreachable on a fresh
// auth-off install. The fallback resolves to `local:dev` (or a
// configured override) and the handler proceeds.

type stubWizard struct {
	converseCalled func(sessionID, operatorID, msg string)
	commitCalled   func(sessionID, operatorID string)
	cancelCalled   func(sessionID, operatorID string)
	cancelErr      error
}

func (s *stubWizard) Converse(_ context.Context, sessionID, operatorID, msg string) (*ProjectWizardResult, error) {
	if s.converseCalled != nil {
		s.converseCalled(sessionID, operatorID, msg)
	}
	return &ProjectWizardResult{SessionID: "sess-1"}, nil
}

func (s *stubWizard) Commit(_ context.Context, sessionID, operatorID string) (*ProjectWizardCommitResult, error) {
	if s.commitCalled != nil {
		s.commitCalled(sessionID, operatorID)
	}
	return &ProjectWizardCommitResult{SessionID: sessionID, ProjectID: "proj-1", URL: "/ui/projects/proj-1"}, nil
}

func (s *stubWizard) Cancel(_ context.Context, sessionID, operatorID string) error {
	if s.cancelCalled != nil {
		s.cancelCalled(sessionID, operatorID)
	}
	return s.cancelErr
}

func TestWizardConverse_AuthOffStampsSingleTenantFallback(t *testing.T) {
	var seenOp string
	stub := &stubWizard{converseCalled: func(_, op, _ string) { seenOp = op }}

	srv := NewServer(
		WithLogger(zerolog.Nop()),
		WithProjectWizard(stub),
	)

	body := strings.NewReader(`{"message":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/wizard/converse", body)
	req = req.WithContext(context.WithValue(req.Context(), authEnabledKey, false))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.ProjectWizardConverse(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("auth-off converse: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if seenOp != defaultSingleTenantOperatorID {
		t.Fatalf("wizard saw operator %q, want %q", seenOp, defaultSingleTenantOperatorID)
	}
}

func TestWizardConverse_AuthOffUsesConfiguredFallback(t *testing.T) {
	var seenOp string
	stub := &stubWizard{converseCalled: func(_, op, _ string) { seenOp = op }}

	cfg := &config.Config{}
	cfg.API.SingleTenantOperatorID = "tenant-x:root"
	srv := NewServer(
		WithLogger(zerolog.Nop()),
		WithProjectWizard(stub),
		WithConfig(cfg),
	)

	body := strings.NewReader(`{"message":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/wizard/converse", body)
	req = req.WithContext(context.WithValue(req.Context(), authEnabledKey, false))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.ProjectWizardConverse(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("auth-off converse: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if seenOp != "tenant-x:root" {
		t.Fatalf("wizard saw operator %q, want %q", seenOp, "tenant-x:root")
	}
}

func TestWizardConverse_AuthOnAnonymousStill401(t *testing.T) {
	// The fallback must NOT apply under auth-on: a missing
	// principal is a hard denial. Otherwise any unauthenticated
	// caller could open sessions under `local:dev`.
	stub := &stubWizard{converseCalled: func(_, _, _ string) {
		t.Fatalf("wizard reached despite auth-on + no principal")
	}}

	srv := NewServer(
		WithLogger(zerolog.Nop()),
		WithProjectWizard(stub),
	)

	body := strings.NewReader(`{"message":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/wizard/converse", body)
	req = req.WithContext(context.WithValue(req.Context(), authEnabledKey, true))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.ProjectWizardConverse(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("auth-on anonymous: got %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
}

func TestWizardCommit_AuthOffStampsSingleTenantFallback(t *testing.T) {
	var seenOp string
	stub := &stubWizard{commitCalled: func(_, op string) { seenOp = op }}

	srv := NewServer(
		WithLogger(zerolog.Nop()),
		WithProjectWizard(stub),
	)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/wizard/sess-1/commit", nil)
	req = req.WithContext(context.WithValue(req.Context(), authEnabledKey, false))
	rec := httptest.NewRecorder()

	srv.ProjectWizardCommit(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("auth-off commit: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if seenOp != defaultSingleTenantOperatorID {
		t.Fatalf("wizard saw operator %q, want %q", seenOp, defaultSingleTenantOperatorID)
	}
}

func TestWizardCancel_HappyPath(t *testing.T) {
	var seenSession, seenOp string
	stub := &stubWizard{cancelCalled: func(sess, op string) { seenSession, seenOp = sess, op }}

	srv := NewServer(
		WithLogger(zerolog.Nop()),
		WithProjectWizard(stub),
	)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/wizard/sess-1/cancel", nil)
	req = req.WithContext(context.WithValue(req.Context(), authEnabledKey, false))
	rec := httptest.NewRecorder()

	// Route through the dispatcher to also exercise the new case.
	srv.projectWizardRouter(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("cancel: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if seenSession != "sess-1" {
		t.Fatalf("wizard saw session %q, want %q", seenSession, "sess-1")
	}
	if seenOp != defaultSingleTenantOperatorID {
		t.Fatalf("wizard saw operator %q, want %q", seenOp, defaultSingleTenantOperatorID)
	}
	if !strings.Contains(rec.Body.String(), `"status":"cancelled"`) {
		t.Fatalf("response missing cancelled status: %s", rec.Body.String())
	}
}

func TestWizardCancel_CommittedReturns409(t *testing.T) {
	stub := &stubWizard{cancelErr: projectwizard.ErrSessionCommitted}

	srv := NewServer(
		WithLogger(zerolog.Nop()),
		WithProjectWizard(stub),
	)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/wizard/sess-1/cancel", nil)
	req = req.WithContext(context.WithValue(req.Context(), authEnabledKey, false))
	rec := httptest.NewRecorder()

	srv.ProjectWizardCancel(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("cancel committed: got %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

func TestWizardCancel_NotFoundReturns404(t *testing.T) {
	stub := &stubWizard{cancelErr: persistence.ErrNotFound}

	srv := NewServer(
		WithLogger(zerolog.Nop()),
		WithProjectWizard(stub),
	)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/wizard/sess-1/cancel", nil)
	req = req.WithContext(context.WithValue(req.Context(), authEnabledKey, false))
	rec := httptest.NewRecorder()

	srv.ProjectWizardCancel(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("cancel missing: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}
