package ui

// /ui/operator/assist — the configuration assistant's CONSOLE door
// (2026-09-13 design §6.3 door 2; plan §7f). Community: it lives in the CE
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
}

// operatorAssistRouter serves GET (form) and POST (run) on /ui/operator/assist.
func (s *Server) operatorAssistRouter(w http.ResponseWriter, r *http.Request) {
	if !s.operatorCapable(r) {
		http.Error(w, "operator capability required", http.StatusForbidden)
		return
	}
	data := OperatorAssistData{
		adminCommonData: adminCommonData{Title: "Configuration assistant", CurrentPage: "operator", IsAdmin: true},
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
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
		defer cancel()
		res, err := s.configAssist.Propose(ctx, configassist.Request{
			ProjectID: data.ProjectID, Intent: data.Intent, Door: configassist.DoorConsole,
			RequestID: persistence.GenerateID("careq"), Actor: uiAssistActor(r),
		})
		if err != nil {
			s.logger.Error().Err(err).Str("project_id", data.ProjectID).Msg("operator assist: request failed")
			data.Error = "the assistant failed: " + err.Error()
			s.render(w, "operator_assist.html", data)
			return
		}
		data.Result = res
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
		return configassist.Actor{Kind: "human", Principal: p, CredentialID: p, SourceID: p}
	}
	return configassist.Actor{Kind: "credential", Principal: p, CredentialID: p, SourceID: p}
}
