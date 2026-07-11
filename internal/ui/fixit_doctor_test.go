package ui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/persistence"
)

// scopedIdentityReq stamps both a project-scope allowlist AND a
// verified identity, mirroring what AuthMiddleware does for a
// session-authenticated, project-scoped caller. Needed because
// FixItDoctorPanel/Message resolve an operator id via
// RequestOperatorIDOrSingleTenant, which (unlike the plain project-
// scope helpers used by most /ui scope-bypass regression tests)
// requires a non-nil Identity once auth is enabled. Every test using
// this scopes the caller to "proj-mine", varying only the requested
// project/session, so the scope itself is a fixed fixture rather than
// a parameter.
func scopedIdentityReq(r *http.Request) *http.Request {
	ctx := api.ContextWithProjectScope(r.Context(), "proj-mine")
	ctx = api.ContextWithSessionID(ctx, "test-session")
	return r.WithContext(ctx)
}

type fakeFixItSessionReader struct {
	rows map[string]*persistence.FixItSession
}

func (f *fakeFixItSessionReader) Get(_ context.Context, id string) (*persistence.FixItSession, error) {
	r, ok := f.rows[id]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	return r, nil
}

type stubUIFixItDoctor struct {
	converseResult *api.FixItResult
	converseErr    error
	scopeProject   string
	scopeOK        bool
	scopeErr       error
	lastArgs       []string

	applyResult    *api.FixItApplyResult
	applyErr       error
	applyArgs      []string
	applyIndex     int
	rollbackResult *api.FixItApplyResult
	rollbackErr    error
	rollbackArgs   []string
}

func (s *stubUIFixItDoctor) Converse(_ context.Context, sessionID, operatorID, failureKind, failureRefID, projectID, msg string) (*api.FixItResult, error) {
	s.lastArgs = []string{sessionID, operatorID, failureKind, failureRefID, projectID, msg}
	if s.converseErr != nil {
		return nil, s.converseErr
	}
	if s.converseResult != nil {
		return s.converseResult, nil
	}
	return &api.FixItResult{SessionID: "fix-1"}, nil
}

func (s *stubUIFixItDoctor) SessionScope(_ context.Context, _, _ string) (string, bool, error) {
	if s.scopeErr != nil {
		return "", false, s.scopeErr
	}
	return s.scopeProject, s.scopeOK, nil
}

func (s *stubUIFixItDoctor) Apply(_ context.Context, sessionID, operatorID string, actionIndex int, secretValue string) (*api.FixItApplyResult, error) {
	s.applyArgs = []string{sessionID, operatorID, secretValue}
	s.applyIndex = actionIndex
	if s.applyErr != nil {
		return nil, s.applyErr
	}
	if s.applyResult != nil {
		return s.applyResult, nil
	}
	return &api.FixItApplyResult{Kind: "retry_task", Result: "applied", Detail: "ok"}, nil
}

func (s *stubUIFixItDoctor) Rollback(_ context.Context, sessionID, operatorID, proposalID string) (*api.FixItApplyResult, error) {
	s.rollbackArgs = []string{sessionID, operatorID, proposalID}
	if s.rollbackErr != nil {
		return nil, s.rollbackErr
	}
	if s.rollbackResult != nil {
		return s.rollbackResult, nil
	}
	return &api.FixItApplyResult{Kind: "config_apply", Result: "applied", Detail: "rolled back"}, nil
}

func TestFixItDoctorPanel_Unconfigured(t *testing.T) {
	srv := NewServer()
	req := authDisabledUIRequest(httptest.NewRequest(http.MethodGet, "/ui/fixit/failed_task/t1", nil))
	rec := httptest.NewRecorder()
	srv.FixItDoctorPanel(rec, req, "failed_task", "t1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not configured") {
		t.Fatalf("expected an unconfigured notice, got:\n%s", rec.Body.String())
	}
}

func TestFixItDoctorPanel_FreshPage_InScope(t *testing.T) {
	srv := NewServer(WithFixItDoctor(&stubUIFixItDoctor{}))
	req := httptest.NewRequest(http.MethodGet, "/ui/fixit/failed_task/t1?project=proj-mine", nil)
	req = scopedIdentityReq(req)
	rec := httptest.NewRecorder()
	srv.FixItDoctorPanel(rec, req, "failed_task", "t1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "failed_task") {
		t.Fatalf("expected the panel to render the failure kind, got:\n%s", rec.Body.String())
	}
}

func TestFixItDoctorPanel_OutOfScope_404(t *testing.T) {
	srv := NewServer(WithFixItDoctor(&stubUIFixItDoctor{}))
	req := httptest.NewRequest(http.MethodGet, "/ui/fixit/failed_task/t1?project=other-project", nil)
	req = scopedIdentityReq(req)
	rec := httptest.NewRecorder()
	srv.FixItDoctorPanel(rec, req, "failed_task", "t1")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (not 403), got %d", rec.Code)
	}
}

func TestFixItDoctorPanel_ResumeSeedsTranscriptAndRendersActionCards(t *testing.T) {
	transcript := []byte(`[
		{"role":"user","content":"my task keeps failing"},
		{"role":"assistant","content":"Let's retry it.","envelope":{"message":"Let's retry it.","resolved":false,"actions":[
			{"kind":"retry_task","label":"Retry the task","params":{"task_id":"t1"}},
			{"kind":"link_out","label":"Open task detail","params":{"url":"/ui/tasks/t1"}}
		]}}
	]`)
	// "local:dev" is the documented single-tenant operator fallback
	// (api.defaultSingleTenantOperatorID) an auth-enabled request with
	// an Identity but no api-key principal resolves to.
	reader := &fakeFixItSessionReader{rows: map[string]*persistence.FixItSession{
		"fix-1": {ID: "fix-1", OperatorID: "local:dev", FailureKind: "failed_task", FailureRefID: "t1", ProjectID: "proj-mine", Transcript: transcript},
	}}
	srv := NewServer(WithFixItDoctor(&stubUIFixItDoctor{}), WithFixItSessionReader(reader))

	req := httptest.NewRequest(http.MethodGet, "/ui/fixit/failed_task/t1?session=fix-1", nil)
	req = scopedIdentityReq(req)
	rec := httptest.NewRecorder()
	srv.FixItDoctorPanel(rec, req, "failed_task", "t1")

	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, body)
	}
	if !strings.Contains(body, "my task keeps failing") {
		t.Fatalf("expected the resumed transcript to render, got:\n%s", body)
	}
	if !strings.Contains(body, "/ui/tasks/t1") {
		t.Fatalf("expected the link_out action's URL to render, got:\n%s", body)
	}
}

func TestPopulateFixItResume_MatchesOwnerAndRef(t *testing.T) {
	transcript := []byte(`[
		{"role":"user","content":"help"},
		{"role":"assistant","content":"working on it","envelope":{"message":"working on it","resolved":true,"actions":[
			{"kind":"link_out","label":"Open it","params":{"url":"/ui/tasks/t1"}},
			{"kind":"retry_task","label":"Retry","params":{"task_id":"t1"}}
		]}}
	]`)
	reader := &fakeFixItSessionReader{rows: map[string]*persistence.FixItSession{
		"fix-1": {ID: "fix-1", OperatorID: "op1", FailureKind: "failed_task", FailureRefID: "t1", ProjectID: "proj-mine", Transcript: transcript},
	}}
	srv := &Server{fixItSessions: reader}
	req := httptest.NewRequest(http.MethodGet, "/ui/fixit/failed_task/t1?session=fix-1", nil)
	req.Header.Set("X-Operator-Id", "op1")
	req = authDisabledUIRequest(req)

	data := &FixItPanelData{Kind: "failed_task", RefID: "t1"}
	srv.populateFixItResume(req, data, "fix-1")

	if data.SessionID != "fix-1" || data.ProjectID != "proj-mine" {
		t.Fatalf("expected resume to seed session/project, got %+v", data)
	}
	if len(data.Turns) != 2 {
		t.Fatalf("expected 2 turns, got %d: %+v", len(data.Turns), data.Turns)
	}
	last := data.Turns[1]
	if !last.Resolved {
		t.Fatalf("expected the assistant turn's Resolved flag to carry through")
	}
	if len(last.Actions) != 2 {
		t.Fatalf("expected 2 action cards, got %+v", last.Actions)
	}
	linkAction, retryAction := last.Actions[0], last.Actions[1]
	if !linkAction.Executable || linkAction.LinkURL != "/ui/tasks/t1" {
		t.Fatalf("expected link_out to be executable with its URL, got %+v", linkAction)
	}
	if retryAction.Executable {
		t.Fatalf("expected retry_task to render as a non-executable preview card, got %+v", retryAction)
	}
}

func TestPopulateFixItResume_ForeignOperatorIgnored(t *testing.T) {
	reader := &fakeFixItSessionReader{rows: map[string]*persistence.FixItSession{
		"fix-1": {ID: "fix-1", OperatorID: "someone-else", FailureKind: "failed_task", FailureRefID: "t1", ProjectID: "proj-mine"},
	}}
	srv := &Server{fixItSessions: reader}
	req := httptest.NewRequest(http.MethodGet, "/ui/fixit/failed_task/t1?session=fix-1", nil)
	req.Header.Set("X-Operator-Id", "op1")
	req = authDisabledUIRequest(req)

	data := &FixItPanelData{Kind: "failed_task", RefID: "t1"}
	srv.populateFixItResume(req, data, "fix-1")
	if data.SessionID != "" {
		t.Fatalf("expected a foreign-operator session to be ignored, got %+v", data)
	}
}

func TestPopulateFixItResume_MismatchedRefIgnored(t *testing.T) {
	reader := &fakeFixItSessionReader{rows: map[string]*persistence.FixItSession{
		"fix-1": {ID: "fix-1", OperatorID: "op1", FailureKind: "degraded_feature", FailureRefID: "other", ProjectID: "proj-mine"},
	}}
	srv := &Server{fixItSessions: reader}
	req := httptest.NewRequest(http.MethodGet, "/ui/fixit/failed_task/t1?session=fix-1", nil)
	req.Header.Set("X-Operator-Id", "op1")
	req = authDisabledUIRequest(req)

	data := &FixItPanelData{Kind: "failed_task", RefID: "t1"}
	srv.populateFixItResume(req, data, "fix-1")
	if data.SessionID != "" {
		t.Fatalf("expected a session bound to a different kind/ref to be ignored, got %+v", data)
	}
}

func TestFixItDoctorMessage_HappyPath_RendersUpdatedTranscript(t *testing.T) {
	stub := &stubUIFixItDoctor{converseResult: &api.FixItResult{SessionID: "fix-1"}}
	reader := &fakeFixItSessionReader{rows: map[string]*persistence.FixItSession{
		"fix-1": {ID: "fix-1", Transcript: []byte(`[{"role":"user","content":"help me"},{"role":"assistant","content":"looking into it"}]`)},
	}}
	srv := NewServer(WithFixItDoctor(stub), WithFixItSessionReader(reader))

	form := url.Values{"session_id": {""}, "project": {"proj-mine"}, "message": {"help me"}}
	req := httptest.NewRequest(http.MethodPost, "/ui/fixit/failed_task/t1/message", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = scopedIdentityReq(req)
	rec := httptest.NewRecorder()
	srv.FixItDoctorMessage(rec, req, "failed_task", "t1")

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "looking into it") {
		t.Fatalf("expected the re-fetched transcript in the response, got:\n%s", rec.Body.String())
	}
	if stub.lastArgs[3] != "t1" || stub.lastArgs[4] != "proj-mine" {
		t.Fatalf("unexpected converse args: %+v", stub.lastArgs)
	}
}

func TestFixItDoctorMessage_EmptyMessage_RendersNotice(t *testing.T) {
	srv := NewServer(WithFixItDoctor(&stubUIFixItDoctor{}))
	form := url.Values{"project": {"proj-mine"}, "message": {"  "}}
	req := httptest.NewRequest(http.MethodPost, "/ui/fixit/failed_task/t1/message", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = scopedIdentityReq(req)
	rec := httptest.NewRecorder()
	srv.FixItDoctorMessage(rec, req, "failed_task", "t1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Message is required") {
		t.Fatalf("expected a validation notice, got:\n%s", rec.Body.String())
	}
}

func TestFixItDoctorMessage_ConverseError_RendersNoticeNot500(t *testing.T) {
	stub := &stubUIFixItDoctor{converseErr: context.DeadlineExceeded}
	srv := NewServer(WithFixItDoctor(stub))
	form := url.Values{"project": {"proj-mine"}, "message": {"help"}}
	req := httptest.NewRequest(http.MethodPost, "/ui/fixit/failed_task/t1/message", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = scopedIdentityReq(req)
	rec := httptest.NewRecorder()
	srv.FixItDoctorMessage(rec, req, "failed_task", "t1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (fragment always renders), got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Turn failed") {
		t.Fatalf("expected a turn-failed notice, got:\n%s", rec.Body.String())
	}
}

func TestFixItDoctorMessage_ResumedOutOfScope_404(t *testing.T) {
	stub := &stubUIFixItDoctor{scopeProject: "other-project", scopeOK: true}
	srv := NewServer(WithFixItDoctor(stub))
	form := url.Values{"session_id": {"fix-1"}, "message": {"turn 2"}}
	req := httptest.NewRequest(http.MethodPost, "/ui/fixit/failed_task/t1/message", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = scopedIdentityReq(req)
	rec := httptest.NewRecorder()
	srv.FixItDoctorMessage(rec, req, "failed_task", "t1")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestFixItDoctorMessage_UnknownSession_404(t *testing.T) {
	stub := &stubUIFixItDoctor{scopeOK: false}
	srv := NewServer(WithFixItDoctor(stub))
	form := url.Values{"session_id": {"ghost"}, "message": {"hi"}}
	req := httptest.NewRequest(http.MethodPost, "/ui/fixit/failed_task/t1/message", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = authDisabledUIRequest(req)
	rec := httptest.NewRecorder()
	srv.FixItDoctorMessage(rec, req, "failed_task", "t1")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestFixItDoctorPanel_MethodNotAllowed(t *testing.T) {
	srv := NewServer()
	req := authDisabledUIRequest(httptest.NewRequest(http.MethodPost, "/ui/fixit/failed_task/t1", nil))
	rec := httptest.NewRecorder()
	srv.FixItDoctorPanel(rec, req, "failed_task", "t1")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

func TestFixItDoctorMessage_MethodNotAllowed(t *testing.T) {
	srv := NewServer()
	req := authDisabledUIRequest(httptest.NewRequest(http.MethodGet, "/ui/fixit/failed_task/t1/message", nil))
	rec := httptest.NewRecorder()
	srv.FixItDoctorMessage(rec, req, "failed_task", "t1")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

func TestFixItDoctorMessage_BadForm(t *testing.T) {
	srv := NewServer(WithFixItDoctor(&stubUIFixItDoctor{}))
	req := httptest.NewRequest(http.MethodPost, "/ui/fixit/failed_task/t1/message", strings.NewReader("%zz"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = authDisabledUIRequest(req)
	rec := httptest.NewRecorder()
	srv.FixItDoctorMessage(rec, req, "failed_task", "t1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestSafeFixItLinkOutURL_WhitespaceAndScheme(t *testing.T) {
	if safeFixItLinkOutURL("/ui/x y") {
		t.Fatalf("expected whitespace-containing path to be unsafe")
	}
	if safeFixItLinkOutURL("/ui/x://y") {
		t.Fatalf("expected a scheme-looking path to be unsafe")
	}
}

func TestFixItDoctorRouter_Dispatch(t *testing.T) {
	srv := NewServer(WithFixItDoctor(&stubUIFixItDoctor{}))

	t.Run("panel", func(t *testing.T) {
		req := authDisabledUIRequest(httptest.NewRequest(http.MethodGet, "/fixit/failed_task/t1", nil))
		rec := httptest.NewRecorder()
		srv.fixItDoctorRouter(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("message", func(t *testing.T) {
		form := url.Values{"message": {"help"}}
		req := authDisabledUIRequest(httptest.NewRequest(http.MethodPost, "/fixit/failed_task/t1/message", strings.NewReader(form.Encode())))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		srv.fixItDoctorRouter(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("bad_path", func(t *testing.T) {
		req := authDisabledUIRequest(httptest.NewRequest(http.MethodGet, "/fixit/", nil))
		rec := httptest.NewRecorder()
		srv.fixItDoctorRouter(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", rec.Code)
		}
	})
}

func TestDecodeFixItTurns(t *testing.T) {
	if got := decodeFixItTurns(nil); got != nil {
		t.Fatalf("expected nil for empty input")
	}
	if got := decodeFixItTurns([]byte("not json")); got != nil {
		t.Fatalf("expected nil for malformed input, got %+v", got)
	}
	turns := decodeFixItTurns([]byte(`[{"role":"user","content":"hi"}]`))
	if len(turns) != 1 || turns[0].Content != "hi" {
		t.Fatalf("unexpected turns: %+v", turns)
	}
}

// --- task 3.3: Apply / Rollback --------------------------------------------

func TestFixItDoctorRouter_Dispatch_ApplyAndRollback(t *testing.T) {
	srv := NewServer(WithFixItDoctor(&stubUIFixItDoctor{scopeProject: "proj-x", scopeOK: true}))

	t.Run("apply", func(t *testing.T) {
		form := url.Values{"session_id": {"fix-1"}}
		req := authDisabledUIRequest(httptest.NewRequest(http.MethodPost, "/fixit/failed_task/t1/actions/0/apply", strings.NewReader(form.Encode())))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		srv.fixItDoctorRouter(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("rollback", func(t *testing.T) {
		form := url.Values{"session_id": {"fix-1"}, "proposal_id": {"cpp_1"}}
		req := authDisabledUIRequest(httptest.NewRequest(http.MethodPost, "/fixit/failed_task/t1/actions/rollback", strings.NewReader(form.Encode())))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		srv.fixItDoctorRouter(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("apply_bad_index", func(t *testing.T) {
		req := authDisabledUIRequest(httptest.NewRequest(http.MethodPost, "/fixit/failed_task/t1/actions/not-a-number/apply", nil))
		rec := httptest.NewRecorder()
		srv.fixItDoctorRouter(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404 for a non-numeric action index, got %d", rec.Code)
		}
	})
}

func TestFixItDoctorApply_UnconfiguredNotice(t *testing.T) {
	srv := NewServer()
	req := authDisabledUIRequest(httptest.NewRequest(http.MethodPost, "/ui/fixit/failed_task/t1/actions/0/apply", nil))
	rec := httptest.NewRecorder()
	srv.FixItDoctorApply(rec, req, "failed_task", "t1", 0)
	if !strings.Contains(rec.Body.String(), "not configured") {
		t.Fatalf("expected an unconfigured notice, got:\n%s", rec.Body.String())
	}
}

func TestFixItDoctorApply_MethodNotAllowed(t *testing.T) {
	srv := NewServer(WithFixItDoctor(&stubUIFixItDoctor{}))
	req := httptest.NewRequest(http.MethodGet, "/ui/fixit/failed_task/t1/actions/0/apply", nil)
	rec := httptest.NewRecorder()
	srv.FixItDoctorApply(rec, req, "failed_task", "t1", 0)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

func TestFixItDoctorApply_NoSession_Notice(t *testing.T) {
	srv := NewServer(WithFixItDoctor(&stubUIFixItDoctor{}))
	req := authDisabledUIRequest(httptest.NewRequest(http.MethodPost, "/ui/fixit/failed_task/t1/actions/0/apply", strings.NewReader("")))
	rec := httptest.NewRecorder()
	srv.FixItDoctorApply(rec, req, "failed_task", "t1", 0)
	if !strings.Contains(rec.Body.String(), "No active session") {
		t.Fatalf("expected a no-session notice, got:\n%s", rec.Body.String())
	}
}

// TestFixItDoctorApply_ScopeRecheck_OutOfScope404 is the load-bearing
// "scope re-check on EVERY apply" test (§5.4/§7): a session scoped to a
// project the caller does NOT have access to must 404 on Apply exactly
// like it 404s on Converse — even though this test never touches
// Converse at all.
func TestFixItDoctorApply_ScopeRecheck_OutOfScope404(t *testing.T) {
	stub := &stubUIFixItDoctor{scopeProject: "other-project", scopeOK: true}
	srv := NewServer(WithFixItDoctor(stub))
	form := url.Values{"session_id": {"fix-1"}}
	req := scopedIdentityReq(httptest.NewRequest(http.MethodPost, "/ui/fixit/failed_task/t1/actions/0/apply", strings.NewReader(form.Encode())))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.FixItDoctorApply(rec, req, "failed_task", "t1", 0)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for an out-of-scope session, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(stub.applyArgs) != 0 {
		t.Fatalf("Apply must not be called when the scope re-check fails, got %v", stub.applyArgs)
	}
}

// TestFixItDoctorApply_DaemonScope_RequiresAdmin covers §7's
// "daemon-scope actions admin-only" rule: a project-scoped (non-admin)
// caller must be refused on a daemon-scope session (empty project),
// even though the session itself resolves fine.
func TestFixItDoctorApply_DaemonScope_RequiresAdmin(t *testing.T) {
	stub := &stubUIFixItDoctor{scopeProject: "", scopeOK: true}
	srv := NewServer(WithFixItDoctor(stub))
	form := url.Values{"session_id": {"fix-1"}}
	req := scopedIdentityReq(httptest.NewRequest(http.MethodPost, "/ui/fixit/failed_reload//actions/0/apply", strings.NewReader(form.Encode())))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.FixItDoctorApply(rec, req, "failed_reload", "", 0)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for a non-admin caller on a daemon-scope session, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(stub.applyArgs) != 0 {
		t.Fatalf("Apply must not be called for a refused daemon-scope caller, got %v", stub.applyArgs)
	}
}

// TestFixItDoctorApply_DaemonScope_AdminAllowed is the positive
// counterpart: an admin-class (unscoped) caller on a daemon-scope
// session IS allowed through to Apply.
func TestFixItDoctorApply_DaemonScope_AdminAllowed(t *testing.T) {
	stub := &stubUIFixItDoctor{scopeProject: "", scopeOK: true}
	srv := NewServer(WithFixItDoctor(stub))
	form := url.Values{"session_id": {"fix-1"}}
	req := authDisabledUIRequest(httptest.NewRequest(http.MethodPost, "/ui/fixit/failed_reload//actions/0/apply", strings.NewReader(form.Encode())))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.FixItDoctorApply(rec, req, "failed_reload", "", 0)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for an admin caller, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(stub.applyArgs) != 3 || stub.applyArgs[0] != "fix-1" {
		t.Fatalf("expected Apply to be called with session fix-1, got %v", stub.applyArgs)
	}
}

func TestFixItDoctorApply_SecretValue_NeverFromParams(t *testing.T) {
	// The template only ever posts secret_value from the operator's own
	// masked input — this test asserts the handler forwards EXACTLY that
	// form field to Apply, not anything else in the request.
	stub := &stubUIFixItDoctor{scopeProject: "proj-mine", scopeOK: true}
	srv := NewServer(WithFixItDoctor(stub))
	form := url.Values{"session_id": {"fix-1"}, "secret_value": {"typed-by-operator"}}
	req := scopedIdentityReq(httptest.NewRequest(http.MethodPost, "/ui/fixit/degraded_feature/instinct/actions/2/apply", strings.NewReader(form.Encode())))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.FixItDoctorApply(rec, req, "degraded_feature", "instinct", 2)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if stub.applyIndex != 2 {
		t.Fatalf("actionIndex = %d, want 2", stub.applyIndex)
	}
	if len(stub.applyArgs) != 3 || stub.applyArgs[2] != "typed-by-operator" {
		t.Fatalf("Apply args = %v, want secretValue=typed-by-operator", stub.applyArgs)
	}
}

func TestFixItDoctorApply_ErrorRendersNoticeNot500(t *testing.T) {
	stub := &stubUIFixItDoctor{scopeProject: "proj-mine", scopeOK: true, applyErr: errFixItApplyBoom}
	srv := NewServer(WithFixItDoctor(stub))
	form := url.Values{"session_id": {"fix-1"}}
	req := scopedIdentityReq(httptest.NewRequest(http.MethodPost, "/ui/fixit/failed_task/t1/actions/0/apply", strings.NewReader(form.Encode())))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.FixItDoctorApply(rec, req, "failed_task", "t1", 0)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (error rendered as a notice, not a 5xx), got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Apply failed") {
		t.Fatalf("expected an Apply-failed notice, got:\n%s", rec.Body.String())
	}
}

func TestFixItDoctorRollback_MethodNotAllowed(t *testing.T) {
	srv := NewServer(WithFixItDoctor(&stubUIFixItDoctor{}))
	req := httptest.NewRequest(http.MethodGet, "/ui/fixit/failed_task/t1/actions/rollback", nil)
	rec := httptest.NewRecorder()
	srv.FixItDoctorRollback(rec, req, "failed_task", "t1")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

func TestFixItDoctorRollback_NoProposalID_Notice(t *testing.T) {
	srv := NewServer(WithFixItDoctor(&stubUIFixItDoctor{}))
	form := url.Values{"session_id": {"fix-1"}}
	req := authDisabledUIRequest(httptest.NewRequest(http.MethodPost, "/ui/fixit/failed_task/t1/actions/rollback", strings.NewReader(form.Encode())))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.FixItDoctorRollback(rec, req, "failed_task", "t1")
	if !strings.Contains(rec.Body.String(), "Nothing to roll back") {
		t.Fatalf("expected a nothing-to-roll-back notice, got:\n%s", rec.Body.String())
	}
}

func TestFixItDoctorApply_BadForm(t *testing.T) {
	srv := NewServer(WithFixItDoctor(&stubUIFixItDoctor{}))
	req := authDisabledUIRequest(httptest.NewRequest(http.MethodPost, "/ui/fixit/failed_task/t1/actions/0/apply", strings.NewReader("%")))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.FixItDoctorApply(rec, req, "failed_task", "t1", 0)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a malformed form body, got %d", rec.Code)
	}
}

func TestFixItDoctorApply_NoOperatorIdentity(t *testing.T) {
	srv := NewServer(WithFixItDoctor(&stubUIFixItDoctor{}))
	form := url.Values{"session_id": {"fix-1"}}
	req := httptest.NewRequest(http.MethodPost, "/ui/fixit/failed_task/t1/actions/0/apply", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.FixItDoctorApply(rec, req, "failed_task", "t1", 0)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no operator identity, got %d", rec.Code)
	}
}

func TestFixItDoctorRollback_BadForm(t *testing.T) {
	srv := NewServer(WithFixItDoctor(&stubUIFixItDoctor{}))
	req := authDisabledUIRequest(httptest.NewRequest(http.MethodPost, "/ui/fixit/failed_task/t1/actions/rollback", strings.NewReader("%")))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.FixItDoctorRollback(rec, req, "failed_task", "t1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a malformed form body, got %d", rec.Code)
	}
}

func TestFixItDoctorRollback_NoOperatorIdentity(t *testing.T) {
	srv := NewServer(WithFixItDoctor(&stubUIFixItDoctor{}))
	form := url.Values{"session_id": {"fix-1"}, "proposal_id": {"cpp_1"}}
	req := httptest.NewRequest(http.MethodPost, "/ui/fixit/failed_task/t1/actions/rollback", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.FixItDoctorRollback(rec, req, "failed_task", "t1")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no operator identity, got %d", rec.Code)
	}
}

func TestFixItDoctorRollback_ScopeRecheck_OutOfScope404(t *testing.T) {
	stub := &stubUIFixItDoctor{scopeProject: "other-project", scopeOK: true}
	srv := NewServer(WithFixItDoctor(stub))
	form := url.Values{"session_id": {"fix-1"}, "proposal_id": {"cpp_1"}}
	req := scopedIdentityReq(httptest.NewRequest(http.MethodPost, "/ui/fixit/degraded_feature/x/actions/rollback", strings.NewReader(form.Encode())))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.FixItDoctorRollback(rec, req, "degraded_feature", "x")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for an out-of-scope session, got %d", rec.Code)
	}
	if len(stub.rollbackArgs) != 0 {
		t.Fatalf("Rollback must not be called when the scope re-check fails, got %v", stub.rollbackArgs)
	}
}

func TestFixItDoctorRollback_ErrorRendersNoticeNot500(t *testing.T) {
	stub := &stubUIFixItDoctor{scopeProject: "proj-mine", scopeOK: true, rollbackErr: errFixItApplyBoom}
	srv := NewServer(WithFixItDoctor(stub))
	form := url.Values{"session_id": {"fix-1"}, "proposal_id": {"cpp_1"}}
	req := scopedIdentityReq(httptest.NewRequest(http.MethodPost, "/ui/fixit/degraded_feature/x/actions/rollback", strings.NewReader(form.Encode())))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.FixItDoctorRollback(rec, req, "degraded_feature", "x")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (error rendered as a notice, not a 5xx), got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Rollback failed") {
		t.Fatalf("expected a Rollback-failed notice, got:\n%s", rec.Body.String())
	}
}

func TestFixItDoctorRollback_HappyPath(t *testing.T) {
	stub := &stubUIFixItDoctor{scopeProject: "proj-mine", scopeOK: true}
	srv := NewServer(WithFixItDoctor(stub))
	form := url.Values{"session_id": {"fix-1"}, "proposal_id": {"cpp_1"}}
	req := scopedIdentityReq(httptest.NewRequest(http.MethodPost, "/ui/fixit/degraded_feature/x/actions/rollback", strings.NewReader(form.Encode())))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.FixItDoctorRollback(rec, req, "degraded_feature", "x")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(stub.rollbackArgs) != 3 || stub.rollbackArgs[2] != "cpp_1" {
		t.Fatalf("Rollback args = %v, want proposalID=cpp_1", stub.rollbackArgs)
	}
}

func TestFixItCallerIsAdmin(t *testing.T) {
	admin := authDisabledUIRequest(httptest.NewRequest(http.MethodGet, "/", nil))
	if !fixItCallerIsAdmin(admin) {
		t.Error("an auth-disabled (unscoped) request should be admin-class")
	}
	scoped := scopedIdentityReq(httptest.NewRequest(http.MethodGet, "/", nil))
	if fixItCallerIsAdmin(scoped) {
		t.Error("a project-scoped request should NOT be admin-class")
	}
}

func TestLatestRollbackID(t *testing.T) {
	if got := latestRollbackID(nil); got != "" {
		t.Errorf("nil -> %q, want empty", got)
	}
	if got := latestRollbackID([]byte("not json")); got != "" {
		t.Errorf("malformed -> %q, want empty", got)
	}
	raw := []byte(`[
		{"kind":"config_apply_gate","result":"applied"},
		{"kind":"config_apply","result":"applied","rollback_id":"cpp_1"},
		{"kind":"retry_task","result":"applied"}
	]`)
	if got := latestRollbackID(raw); got != "cpp_1" {
		t.Errorf("got %q, want cpp_1", got)
	}
	rolledBack := []byte(`[
		{"kind":"config_apply","result":"applied","rollback_id":"cpp_1"},
		{"kind":"config_apply_rollback","result":"applied"}
	]`)
	if got := latestRollbackID(rolledBack); got != "" {
		t.Errorf("got %q after a rollback was applied, want empty", got)
	}
}

func TestDecodeFixItTurns_OnlyLastEnvelopeTurnIsApplyable(t *testing.T) {
	raw := []byte(`[
		{"role":"user","content":"u1"},
		{"role":"assistant","content":"a1","envelope":{"message":"a1","resolved":false,
			"actions":[{"kind":"retry_task","label":"retry","params":{"task_id":"t1"}}]}},
		{"role":"system","content":"applied: task requeued"},
		{"role":"user","content":"u2"},
		{"role":"assistant","content":"a2","envelope":{"message":"a2","resolved":false,
			"actions":[{"kind":"link_out","label":"open","params":{"url":"/ui/x"}},
			           {"kind":"reprobe_integration","label":"recheck","params":{"integration_id":"telegram"}}]}}
	]`)
	turns := decodeFixItTurns(raw)
	if len(turns) != 5 {
		t.Fatalf("expected 5 turns, got %d", len(turns))
	}
	firstEnvelopeActions := turns[1].Actions
	if len(firstEnvelopeActions) != 1 || firstEnvelopeActions[0].Applyable {
		t.Fatalf("the FIRST envelope turn's action must not be applyable (superseded): %+v", firstEnvelopeActions)
	}
	lastActions := turns[4].Actions
	if len(lastActions) != 2 {
		t.Fatalf("expected 2 actions on the last envelope turn, got %d", len(lastActions))
	}
	if lastActions[0].Kind != "link_out" || !lastActions[0].Executable || lastActions[0].Applyable {
		t.Fatalf("link_out must be Executable, never Applyable: %+v", lastActions[0])
	}
	if lastActions[1].Kind != "reprobe_integration" || !lastActions[1].Applyable || lastActions[1].ActionIndex != 1 {
		t.Fatalf("reprobe_integration on the last turn must be Applyable at index 1: %+v", lastActions[1])
	}
}

var errFixItApplyBoom = errors.New("boom")

func TestSafeFixItLinkOutURL(t *testing.T) {
	cases := map[string]bool{
		"/ui/tasks/t1":        true,
		"":                    false,
		"http://evil.com":     false,
		"//evil.com":          false,
		"javascript:alert(1)": false,
		"relative/path":       false,
	}
	for url, want := range cases {
		if got := safeFixItLinkOutURL(url); got != want {
			t.Errorf("safeFixItLinkOutURL(%q) = %v, want %v", url, got, want)
		}
	}
}
