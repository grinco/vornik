package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

type stubFixItDoctor struct {
	converseCalled func(sessionID, operatorID, failureKind, failureRefID, projectID, msg string)
	converseErr    error
	converseResult *FixItResult
	scopeProject   string
	scopeOK        bool
	scopeErr       error
}

func (s *stubFixItDoctor) Converse(_ context.Context, sessionID, operatorID, failureKind, failureRefID, projectID, msg string) (*FixItResult, error) {
	if s.converseCalled != nil {
		s.converseCalled(sessionID, operatorID, failureKind, failureRefID, projectID, msg)
	}
	if s.converseErr != nil {
		return nil, s.converseErr
	}
	if s.converseResult != nil {
		return s.converseResult, nil
	}
	return &FixItResult{SessionID: "fix-1", Envelope: &FixItEnvelope{Message: "ok"}}, nil
}

func (s *stubFixItDoctor) SessionScope(_ context.Context, _, _ string) (string, bool, error) {
	if s.scopeErr != nil {
		return "", false, s.scopeErr
	}
	return s.scopeProject, s.scopeOK, nil
}

func (s *stubFixItDoctor) Apply(_ context.Context, _, _ string, _ int, _ string) (*FixItApplyResult, error) {
	return &FixItApplyResult{Kind: "retry_task", Result: "applied", Detail: "ok"}, nil
}

func (s *stubFixItDoctor) Rollback(_ context.Context, _, _, _ string) (*FixItApplyResult, error) {
	return &FixItApplyResult{Kind: "config_apply", Result: "applied", Detail: "rolled back"}, nil
}

func newFixItTestServer(stub FixItDoctor) *Server {
	return NewServer(
		WithLogger(zerolog.Nop()),
		WithFixItDoctor(stub),
	)
}

func doFixItConverse(srv *Server, body string, ctxMutate func(context.Context) context.Context) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/fixit/converse", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx := context.WithValue(req.Context(), authEnabledKey, false)
	if ctxMutate != nil {
		ctx = ctxMutate(ctx)
	}
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	srv.FixItConverse(rec, req)
	return rec
}

func TestFixItConverse_NewSession_HappyPath(t *testing.T) {
	var gotKind, gotRef, gotProject string
	stub := &stubFixItDoctor{converseCalled: func(_, _, failureKind, failureRefID, projectID, _ string) {
		gotKind, gotRef, gotProject = failureKind, failureRefID, projectID
	}}
	srv := newFixItTestServer(stub)

	rec := doFixItConverse(srv, `{"failure_kind":"failed_task","failure_ref_id":"t1","project_id":"proj-1","message":"help"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if gotKind != "failed_task" || gotRef != "t1" || gotProject != "proj-1" {
		t.Fatalf("unexpected converse args: kind=%q ref=%q project=%q", gotKind, gotRef, gotProject)
	}
	var result FixItResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result.SessionID != "fix-1" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestFixItConverse_Unwired_503(t *testing.T) {
	srv := NewServer(WithLogger(zerolog.Nop()))
	rec := doFixItConverse(srv, `{"failure_kind":"failed_task","failure_ref_id":"t1","message":"help"}`, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}

func TestFixItConverse_MissingMessage_400(t *testing.T) {
	srv := newFixItTestServer(&stubFixItDoctor{})
	rec := doFixItConverse(srv, `{"failure_kind":"failed_task","failure_ref_id":"t1"}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestFixItConverse_NewSessionMissingRef_400(t *testing.T) {
	srv := newFixItTestServer(&stubFixItDoctor{})
	rec := doFixItConverse(srv, `{"message":"help"}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestFixItConverse_MethodNotAllowed(t *testing.T) {
	srv := newFixItTestServer(&stubFixItDoctor{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/fixit/converse", nil)
	rec := httptest.NewRecorder()
	srv.FixItConverse(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

func TestFixItConverse_NewSessionOutOfScope_404NotForbidden(t *testing.T) {
	stub := &stubFixItDoctor{}
	srv := newFixItTestServer(stub)
	rec := doFixItConverse(srv, `{"failure_kind":"failed_task","failure_ref_id":"t1","project_id":"other-project","message":"help"}`,
		func(ctx context.Context) context.Context {
			return ContextWithSessionID(ContextWithProjectScope(ctx, "proj-mine"), "test-session")
		})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (not 403) for an out-of-scope project, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestFixItConverse_ResumedSession_ScopesOnSessionsOwnProject_TamperedBodyIgnored(t *testing.T) {
	var gotProject string
	stub := &stubFixItDoctor{
		scopeProject: "proj-mine", scopeOK: true,
		converseCalled: func(_, _, _, _, projectID, _ string) { gotProject = projectID },
	}
	srv := newFixItTestServer(stub)
	// Body claims project_id=other-project, but SessionScope says the
	// session actually belongs to proj-mine — the handler must scope-
	// gate (and pass along) proj-mine, never the tampered body value.
	rec := doFixItConverse(srv, `{"session_id":"fix-1","project_id":"other-project","message":"turn 2"}`,
		func(ctx context.Context) context.Context {
			return ContextWithSessionID(ContextWithProjectScope(ctx, "proj-mine"), "test-session")
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for an in-scope session, got %d: %s", rec.Code, rec.Body.String())
	}
	if gotProject != "proj-mine" {
		t.Fatalf("expected the session's own project (proj-mine) to be used, got %q", gotProject)
	}
}

func TestFixItConverse_ResumedSession_OutOfScope_404(t *testing.T) {
	stub := &stubFixItDoctor{scopeProject: "other-project", scopeOK: true}
	srv := newFixItTestServer(stub)
	rec := doFixItConverse(srv, `{"session_id":"fix-1","message":"turn 2"}`,
		func(ctx context.Context) context.Context {
			return ContextWithSessionID(ContextWithProjectScope(ctx, "proj-mine"), "test-session")
		})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for a session scoped to a project the caller can't see, got %d", rec.Code)
	}
}

func TestFixItConverse_ResumedSession_UnknownSession_404(t *testing.T) {
	stub := &stubFixItDoctor{scopeOK: false}
	srv := newFixItTestServer(stub)
	rec := doFixItConverse(srv, `{"session_id":"ghost","message":"hi"}`, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestFixItConverse_ResumedSession_ScopeLookupError_502(t *testing.T) {
	stub := &stubFixItDoctor{scopeErr: errors.New("db down")}
	srv := newFixItTestServer(stub)
	rec := doFixItConverse(srv, `{"session_id":"fix-1","message":"hi"}`, nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", rec.Code)
	}
}

func TestFixItConverse_ErrorMapping(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		wantHTT int
	}{
		{"closed", errors.New("fixitdoctor: session already closed"), http.StatusGone},
		{"too_many", errors.New("fixitdoctor: too many active sessions"), http.StatusTooManyRequests},
		{"turn_limit", errors.New("fixitdoctor: session turn limit reached"), http.StatusTooManyRequests},
		{"budget", errors.New("fixitdoctor: budget exceeded: daily cap"), http.StatusTooManyRequests},
		{"not_found", errors.New("fixitdoctor: not found"), http.StatusNotFound},
		{"generic", errors.New("boom"), http.StatusBadGateway},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stub := &stubFixItDoctor{converseErr: c.err}
			srv := newFixItTestServer(stub)
			rec := doFixItConverse(srv, `{"failure_kind":"failed_task","failure_ref_id":"t1","message":"help"}`, nil)
			if rec.Code != c.wantHTT {
				t.Fatalf("expected %d, got %d: %s", c.wantHTT, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestFixItConverse_BadJSON_400(t *testing.T) {
	srv := newFixItTestServer(&stubFixItDoctor{})
	rec := doFixItConverse(srv, `{not json`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}
