package ui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/admin"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/configassist"
	"vornik.io/vornik/internal/registry"
)

// Regression: audit 2026-09-15 CA-13 — "The Console Misstates Side Effects and
// Provides No Retry Identity".
//
// The page's introduction promised flatly that "Nothing is written to the live
// configuration here". For a project that has opted in to auto-apply, a judged
// class A or B proposal is written immediately — and the only sign was an
// "auto-applied" badge rendered AFTER the operator had already submitted on
// the strength of the promise.
//
// The template is checked directly: the claim it makes to the operator BEFORE
// they act is the thing that was wrong.
func TestOperatorAssistTemplate_DoesNotPromiseNoLiveWrites(t *testing.T) {
	body := templateSource(t, "operator_assist.html")
	if strings.Contains(body, "Nothing is written to the live configuration here.") {
		t.Fatal("the page still makes an unconditional no-live-writes promise it cannot keep when a project opts in to auto-apply")
	}
	if !strings.Contains(body, ".AutoApplyClasses") {
		t.Fatal("the page must state the project's actual auto-apply opt-in before the operator submits")
	}
}

// The form must carry a stable idempotency token and disable itself on
// submit: without them a refresh after an uncertain response paid for another
// model loop and filed a second proposal.
func TestOperatorAssistTemplate_SubmissionIsRetrySafe(t *testing.T) {
	body := templateSource(t, "operator_assist.html")
	if !strings.Contains(body, `name="requestToken"`) {
		t.Error("the form carries no idempotency token, so a resubmit files a second proposal")
	}
	if !strings.Contains(body, "data-assist-submit") || !strings.Contains(body, "btn.disabled = true") {
		t.Error("the submit button is not disabled on submit, so a double click starts a second expensive run")
	}
	if !strings.Contains(body, "data-assist-pending") {
		t.Error("a request that can take minutes needs a visible pending state")
	}
}

// The result's proposal link must open THAT proposal, not the whole inbox —
// the recovery path is harder to follow when the operator has to find their
// own proposal in a list.
func TestOperatorAssistTemplate_LinksTheSpecificProposal(t *testing.T) {
	body := templateSource(t, "operator_assist.html")
	if !strings.Contains(body, "section=proposals&amp;proposal={{.Proposal.ID}}") {
		t.Error("the proposal link does not address the specific proposal")
	}
}

// templateSource reads a template's raw source from the SAME embed.FS the
// production handler renders from, so a test cannot pass against a copy that
// does not ship.
func templateSource(t *testing.T, name string) string {
	t.Helper()
	data, err := getEmbedFS(t).ReadFile("templates/" + name)
	if err != nil {
		t.Fatalf("read template %s: %v", name, err)
	}
	return string(data)
}

// Regression: audit 2026-09-15 CA-12 — "Community Self-Service Has No
// Reachable Personal Login".
//
// The self-service account pages need a browser session, and production
// session-login wiring comes only from the enterprise identity provider.
// A Community deployment builds none, so these pages answered "sign in to
// see your account" — pointing the person at a sign-in that does not exist
// on their edition, which is the remedy that stops them looking further.
//
// This does not give CE a login (an open prerequisite, amendment R3). It
// makes the deployment say what is actually true.
func TestMyAccount_SaysSoWhenThereIsNoPersonalLogin(t *testing.T) {
	s := NewServer() // no session-login backend: the CE shape
	for _, tc := range []struct {
		name    string
		handler func(http.ResponseWriter, *http.Request)
		method  string
	}{
		{"page", s.MyAccount, http.MethodGet},
		{"action", s.MyAccountAction, http.MethodPost},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.handler(rec, httptest.NewRequest(tc.method, "/ui/account", nil))
			if rec.Code != http.StatusNotImplemented {
				t.Fatalf("status = %d, want 501: an unavailable capability is not an unauthenticated caller", rec.Code)
			}
			body := rec.Body.String()
			if strings.Contains(body, "sign in to") {
				t.Fatalf("the response still tells the user to sign in somewhere that does not exist: %q", body)
			}
			if !strings.Contains(body, "not available on this deployment") {
				t.Fatalf("the response does not say the capability is absent: %q", body)
			}
		})
	}
}

// Regression: re-audit 2026-09-15 CA-13 (REOPENED) — "console warning and
// proposal links do not follow the actual selection".
//
// The first CA-13 fix added a per-project auto-apply warning and computed it
// from data.ProjectID. The ordinary navigation to /ui/operator/assist carries
// NO project query, so that ID was empty while the browser's <select> showed
// — and would submit — the FIRST project. A deployment where every project
// auto-applies class A therefore rendered "This project has no auto-apply
// opt-in" on the page an operator actually lands on.
//
// THE SEAM: the warning is computed from a field the FORM does not use. A
// template test proves the branch exists; only a rendered request proves the
// branch that fires is the one describing what the operator is about to do.
func TestOperatorAssist_DefaultPageWarnsAboutTheProjectItWillSubmit(t *testing.T) {
	s := assistServerWithAutoApply(t, "digest", "A")
	rec := httptest.NewRecorder()
	s.operatorAssistRouter(rec, asOperator(httptest.NewRequest(http.MethodGet, "/ui/operator/assist", nil)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "no auto-apply opt-in") {
		t.Fatal("the landing page claims no auto-apply while the project it would submit auto-applies class A")
	}
	if !strings.Contains(body, "auto-applies class") {
		t.Fatalf("the landing page must state the selected project's real opt-in:\n%s", body)
	}
}

// The selection the page shows must be the one it describes, so the <option>
// marked selected and the warning have to agree.
func TestOperatorAssist_ExplicitProjectSelectionIsDescribed(t *testing.T) {
	s := assistServerWithAutoApply(t, "digest", "A")
	rec := httptest.NewRecorder()
	s.operatorAssistRouter(rec, asOperator(httptest.NewRequest(http.MethodGet, "/ui/operator/assist?project=quiet", nil)))

	body := rec.Body.String()
	if !strings.Contains(body, "no auto-apply opt-in") {
		t.Fatalf("a project WITHOUT an opt-in must say so:\n%s", body)
	}
}

// assistServerWithAutoApply builds a console server whose registry holds two
// projects — autoProject (opted in to the given classes) and "quiet" (not) —
// so a rendered page can be checked against the selection it shows.
func assistServerWithAutoApply(t *testing.T, autoProject, class string) *Server {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"projects", "swarms", "workflows"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	swarm := "---\nswarmId: s1\ndisplayName: S1\nroles:\n  - name: lead\n    model: m\n    runtime:\n      image: \"img\"\n    permissions:\n      allowedTools:\n        - file_read\n---\n"
	if err := os.WriteFile(filepath.Join(root, "swarms", "s1.md"), []byte(swarm), 0o600); err != nil {
		t.Fatal(err)
	}
	wf := "---\nworkflowId: \"w1\"\ndisplayName: \"W1\"\ndescription: \"t\"\nversion: \"1.0.0\"\nentrypoint: \"plan\"\nsteps:\n  plan:\n    type: \"agent\"\n    role: \"lead\"\n    on_success: \"complete\"\n    on_fail: \"failed\"\n    timeout: \"10m\"\n    prompt: \"plan it\"\nterminals:\n  complete:\n    status: \"COMPLETED\"\n  failed:\n    status: \"FAILED\"\n---\n"
	if err := os.WriteFile(filepath.Join(root, "workflows", "w1.md"), []byte(wf), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{autoProject, "quiet"} {
		body := "projectId: " + id + "\ndisplayName: " + id + "\nswarmId: s1\ndefaultWorkflowId: w1\n"
		if err := os.WriteFile(filepath.Join(root, "projects", id+".yaml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reg := registry.New()
	if err := reg.Load(root); err != nil {
		t.Fatalf("registry load: %v", err)
	}
	cfg := config.AssistantConfig{
		Enabled:   true,
		AutoApply: map[string]config.AssistantAutoApply{autoProject: {Classes: []string{class}}},
	}
	eng := &configassist.Engine{Config: func() config.AssistantConfig { return cfg }}
	return NewServer(WithProjectRegistry(reg), WithConfigAssistant(eng))
}

// asOperator stamps the admin principal the console's operator gate accepts.
func asOperator(r *http.Request) *http.Request {
	return r.WithContext(admin.ContextWithAdmin(r.Context(), "test-operator"))
}
