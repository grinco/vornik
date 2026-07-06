package ui

import (
	"net/http"

	"vornik.io/vornik/internal/projectdoctor"
)

// ProjectSetupData backs project_setup.html.
type ProjectSetupData struct {
	Title       string
	CurrentPage string
	ProjectID   string
	Report      projectdoctor.Report
}

// projectSetupCheckData backs the per-check fragment (project_setup_check.html).
type projectSetupCheckData struct {
	ProjectID string
	Check     projectdoctor.CheckResult
}

// ProjectSetup renders the readiness checklist page. Project scope is
// already enforced by projectRouter's central uiRequireProjectScope gate
// before this is reached, so a nonexistent project isn't a 404 here —
// projectDoctor.Run degrades gracefully to a single red config_valid
// check, which the page renders like any other report.
func (s *Server) ProjectSetup(w http.ResponseWriter, r *http.Request, projectID string) {
	if s.projectDoctor == nil {
		http.Error(w, "project doctor not wired", http.StatusServiceUnavailable)
		return
	}
	data := ProjectSetupData{
		Title:       "Project setup",
		CurrentPage: "projects",
		ProjectID:   projectID,
		Report:      s.projectDoctor.Run(r.Context(), projectID),
	}
	s.render(w, "project_setup.html", data)
}

// ProjectSetupCheck renders one check fragment. Used both by the page's
// htmx-load lazy fetch (so a check that changed since the page's initial
// Run() shows fresh state) and by the "Re-run" button on every panel.
func (s *Server) ProjectSetupCheck(w http.ResponseWriter, r *http.Request, projectID, key string) {
	if s.projectDoctor == nil {
		http.Error(w, "project doctor not wired", http.StatusServiceUnavailable)
		return
	}
	res, err := s.projectDoctor.RunOne(r.Context(), projectID, key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.render(w, "project_setup_check.html", projectSetupCheckData{ProjectID: projectID, Check: res})
}

// ProjectSetupSecret handles the inline masked-secret fix: set the
// named secret then re-render the secrets check panel so the operator
// sees it flip green (or see what's still missing) without a full
// page reload.
func (s *Server) ProjectSetupSecret(w http.ResponseWriter, r *http.Request, projectID string) {
	if s.projectDoctor == nil {
		http.Error(w, "project doctor not wired", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	name := r.FormValue("name")
	value := r.FormValue("value")
	if name == "" || value == "" {
		http.Error(w, "name and value required", http.StatusBadRequest)
		return
	}
	if err := s.projectDoctor.SetSecret(projectID, name, value); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	res, err := s.projectDoctor.RunOne(r.Context(), projectID, "secrets")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.render(w, "project_setup_check.html", projectSetupCheckData{ProjectID: projectID, Check: res})
}

// ProjectSetupSmoke triggers a smoke run then renders the smoke fragment
// so the panel shows the newly-running state (and the operator's button
// disables itself via .Check.Meta["running"]).
func (s *Server) ProjectSetupSmoke(w http.ResponseWriter, r *http.Request, projectID string) {
	if s.projectDoctor == nil {
		http.Error(w, "project doctor not wired", http.StatusServiceUnavailable)
		return
	}
	if _, err := s.projectDoctor.TriggerSmoke(r.Context(), projectID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	res, err := s.projectDoctor.RunOne(r.Context(), projectID, "smoke")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.render(w, "project_setup_check.html", projectSetupCheckData{ProjectID: projectID, Check: res})
}
