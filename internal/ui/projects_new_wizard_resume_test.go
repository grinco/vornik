package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// resumeReq builds an auth-disabled GET for the wizard page with the
// given ?session and operator.
func resumeReq(session, operator string) *http.Request {
	url := "/ui/projects/new/wizard"
	if session != "" {
		url += "?session=" + session
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	if operator != "" {
		req.Header.Set("X-Operator-Id", operator)
	}
	return authDisabledUIRequest(req)
}

func TestProjectsNewWizard_ResumeSeedsSessionAndTranscript(t *testing.T) {
	transcript := []byte(`[{"role":"user","content":"track AI policy"},{"role":"assistant","content":"Sure — which sites?"}]`)
	lister := &stubWizardLister{rows: []*persistence.ProjectWizardSession{
		{ID: "pw_resume", OperatorID: "op_1", UpdatedAt: time.Now(), Transcript: transcript},
	}}
	srv := NewServer(WithWizardSessionLister(lister))

	rec := httptest.NewRecorder()
	srv.ProjectsNewWizard(rec, resumeReq("pw_resume", "op_1"))
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	// sessionID seeded so the page continues (not starts) the draft.
	if !strings.Contains(body, `let sessionID = "pw_resume";`) {
		t.Error("resumed page must seed sessionID with the draft id")
	}
	// Prior turns are available for replay.
	if !strings.Contains(body, "track AI policy") || !strings.Contains(body, "which sites") {
		t.Errorf("resumed page must carry the prior transcript for replay; body:\n%s", body)
	}
}

func TestProjectsNewWizard_FreshWhenNoSessionParam(t *testing.T) {
	srv := NewServer(WithWizardSessionLister(&stubWizardLister{}))
	rec := httptest.NewRecorder()
	srv.ProjectsNewWizard(rec, resumeReq("", "op_1"))
	body := rec.Body.String()
	if !strings.Contains(body, `let sessionID = "";`) {
		t.Error("a fresh wizard must start with an empty sessionID")
	}
}

// A crafted ?session= must not resume another operator's draft, nor a
// committed/cancelled one — the page falls back to fresh.
func TestProjectsNewWizard_ResumeRejectsForeignAndClosed(t *testing.T) {
	committed := "shipped"
	now := time.Now()
	lister := &stubWizardLister{rows: []*persistence.ProjectWizardSession{
		{ID: "pw_foreign", OperatorID: "victim", UpdatedAt: now},
		{ID: "pw_committed", OperatorID: "op_1", UpdatedAt: now, CommittedProjectID: &committed},
		{ID: "pw_cancelled", OperatorID: "op_1", UpdatedAt: now, CancelledAt: &now},
	}}
	srv := NewServer(WithWizardSessionLister(lister))

	for _, sid := range []string{"pw_foreign", "pw_committed", "pw_cancelled", "pw_does_not_exist"} {
		rec := httptest.NewRecorder()
		srv.ProjectsNewWizard(rec, resumeReq(sid, "op_1"))
		body := rec.Body.String()
		if !strings.Contains(body, `let sessionID = "";`) {
			t.Errorf("session %q must NOT be resumable (foreign/closed/unknown) — page should start fresh", sid)
		}
	}
}
