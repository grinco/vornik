package ui

// /ui/operator/assist — the configuration assistant's CONSOLE entrypoint
// (2026-09-13 design §6.3 entrypoint 2; plan §7f). Community: it lives in the CE
// operator shell (operatorCapable — operator scope + explicit capability),
// never in the EE admin router. One form post = one intent = one proposal
// or one named refusal, rendered inline with the diff, the class, the
// judge's verdict and — for class E — the effective permission diff.

import (
	"context"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/configassist"
	"vornik.io/vornik/internal/persistence"
)

// operatorAssistNavKey is the nav destination this page lights up in the rail.
// The assistant has no rail entry of its own — it is reached as a tab on the
// control-plane hub (cpSectionAssist), because every run files a proposal into
// that ledger — so it highlights Control plane. A CurrentPage naming no nav key
// renders the page with nothing active, which is what it did before.
const operatorAssistNavKey = "admin-control-plane"

// WithConfigAssistant wires the assistant engine behind /ui/operator/assist.
func WithConfigAssistant(e *configassist.Engine) ServerOption {
	return func(s *Server) { s.configAssist = e }
}

// OperatorAssistData backs operator_assist.html.
type OperatorAssistData struct {
	adminCommonData
	Available bool
	Projects  []string
	ProjectID string
	Intent    string
	Result    *configassist.Result
	Error     string
	// RequestToken is a per-form idempotency key. The form carries it
	// hidden, so a resubmit — a refresh after an uncertain response, a
	// double click, a browser retry — REPLAYS the proposal the first
	// submission filed instead of paying for a second assistant loop and
	// filing a second proposal (audit 2026-09-15 CA-13). A fresh token is
	// minted for each newly rendered form, so a deliberate second request
	// is still a second request.
	RequestToken string
	// AutoApplyClasses lists the classes THIS project has opted in to
	// auto-apply, so the page can say what may happen before the operator
	// submits rather than contradicting itself afterwards.
	AutoApplyClasses []string
}

// operatorAssistRouter serves GET (form) and POST (run) on /ui/operator/assist.
func (s *Server) operatorAssistRouter(w http.ResponseWriter, r *http.Request) {
	if !s.operatorCapable(r) {
		http.Error(w, "operator capability required", http.StatusForbidden)
		return
	}
	data := OperatorAssistData{
		adminCommonData: adminCommonData{Title: "Configuration assistant", CurrentPage: operatorAssistNavKey, IsAdmin: true},
		Available:       s.configAssist != nil,
	}
	if s.projectReg != nil {
		for _, p := range s.projectReg.ListProjects() {
			data.Projects = append(data.Projects, p.ID)
		}
	}
	switch r.Method {
	case http.MethodGet:
		data.ProjectID = r.URL.Query().Get("project")
		// Default to the project the BROWSER will show selected — the first
		// option — so the auto-apply warning describes what this page would
		// actually submit. Ordinary navigation carries no ?project, and the
		// warning was computed for an empty id: a deployment where every
		// project auto-applies class A rendered "no auto-apply opt-in" on the
		// landing page (re-audit 2026-09-15, CA-13). A warning about a
		// selection nobody made is worse than none.
		if data.ProjectID == "" && len(data.Projects) > 0 {
			data.ProjectID = data.Projects[0]
		}
		data.RequestToken = persistence.GenerateID("cauix")
		data.AutoApplyClasses = s.assistAutoApplyClasses(data.ProjectID)
		s.render(w, "operator_assist.html", data)
	case http.MethodPost:
		if !data.Available {
			http.Error(w, "configuration assistant not wired", http.StatusServiceUnavailable)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form: "+err.Error(), http.StatusBadRequest)
			return
		}
		data.ProjectID = strings.TrimSpace(r.FormValue("project"))
		data.Intent = strings.TrimSpace(r.FormValue("intent"))
		if data.ProjectID == "" || data.Intent == "" {
			data.Error = "pick a project and describe the change"
			s.render(w, "operator_assist.html", data)
			return
		}
		data.RequestToken = strings.TrimSpace(r.FormValue("requestToken"))
		if data.RequestToken == "" {
			data.RequestToken = persistence.GenerateID("cauix")
		}
		data.AutoApplyClasses = s.assistAutoApplyClasses(data.ProjectID)
		// The request keeps the caller's context: a browser that gave up is
		// a request nobody is waiting for, and continuing to spend on it
		// helps no one. The timeout bounds the rest.
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
		defer cancel()
		res, err := s.configAssist.Propose(ctx, configassist.Request{
			ProjectID: data.ProjectID, Intent: data.Intent, Entrypoint: configassist.EntrypointConsole,
			RequestID: persistence.GenerateID("careq"), Actor: uiAssistActor(r),
			// Bound to this form submission, so a resubmit replays rather
			// than re-running (audit 2026-09-15 CA-13).
			IdempotencyKey: data.RequestToken,
		})
		if err != nil {
			s.logger.Error().Err(err).Str("project_id", data.ProjectID).Msg("operator assist: request failed")
			data.Error = "the assistant failed: " + err.Error()
			s.render(w, "operator_assist.html", data)
			return
		}
		data.Result = res
		// A NEW token for the next submission: the operator has their answer,
		// and their next question is a different request.
		data.RequestToken = persistence.GenerateID("cauix")
		s.render(w, "operator_assist.html", data)
	default:
		http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
	}
}

// uiAssistActor derives the ledger actor from the request: a stable id,
// never a bearer (review R2/R5).
func uiAssistActor(r *http.Request) configassist.Actor {
	p := adminPrincipal(r)
	if strings.HasPrefix(p, "session:") {
		// The console marked the actor human and left AccountID empty, so
		// the ledger recorded that a person acted without recording WHICH
		// person (audit 2026-09-15 CA-11). The session principal already
		// carries the id; it just has to reach the structured field.
		return configassist.Actor{
			Kind:         "human",
			AccountID:    strings.TrimPrefix(p, "session:"),
			Principal:    p,
			CredentialID: p,
			SourceID:     p,
		}
	}
	return configassist.Actor{Kind: "credential", Principal: p, CredentialID: p, SourceID: p}
}

// assistAutoApplyClasses reports which classes this project has opted in to
// auto-apply.
//
// The page's introduction stated flatly that "nothing is written to the live
// configuration here". That is true only for a project with no opt-in: a
// judged class A or B proposal on an opted-in project applies immediately,
// and the badge saying so appeared AFTER the operator had already submitted
// on the strength of the promise (audit 2026-09-15 CA-13). The page now says
// what is true for the project in front of it.
func (s *Server) assistAutoApplyClasses(projectID string) []string {
	if s.configAssist == nil || projectID == "" {
		return nil
	}
	cfg := s.configAssist.Config()
	var out []string
	for _, class := range []string{configassist.ClassA, configassist.ClassB2, configassist.ClassB1} {
		if cfg.AutoApplyAllows(projectID, class) {
			out = append(out, class)
		}
	}
	return out
}
