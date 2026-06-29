// Project template gallery — 2026.6.0 SaaS-readiness feature 2
// slice 2. Server-rendered HTML at /ui/projects/new that lets a
// new user pick a template, fill the parameter form, and submit
// to materialise a new project. POST path reuses the same
// validation + render + write logic the API endpoint uses so
// drift between the two surfaces is impossible.

package ui

import (
	"errors"
	"net/http"
	"strings"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/templates"
)

// ProjectsNewData backs the gallery + form template. When
// SelectedSlug is empty the page renders the gallery grid; when
// set it renders the parameter form for that template only.
//
// Domains powers the tab strip filter — pre-sorted distinct
// values from the catalog (2026-05-15 external-research-inspired
// taxonomy addition). Empty when only one domain exists.
type ProjectsNewData struct {
	Title       string
	CurrentPage string

	// CatalogAvailable=false when no templates are wired. The
	// template renders an explanatory empty state instead of an
	// empty grid so operators know whether to install templates.
	CatalogAvailable bool
	// MaterialisationDisabled=true when the daemon has a catalog
	// but no configsDir to write into. Renders the gallery
	// read-only with a banner.
	MaterialisationDisabled bool

	Templates []templates.Manifest
	Domains   []string

	// SelectedSlug is "" for the gallery view, non-empty for the
	// parameter-form view.
	SelectedSlug     string
	SelectedManifest templates.Manifest

	// ActiveDomain filters the gallery grid; empty shows all.
	ActiveDomain string

	// Error carries the rendered ValidationError text after a
	// failed POST round-trip; empty on the initial GET. Named
	// `Error` (not `FormError`) so the template uses the same
	// `{{if .Error}}` shape every other form in the codebase uses —
	// see https://docs.vornik.io §form-pattern.
	Error string
	// FormValues mirrors the last submitted parameter values so
	// the form repopulates instead of clearing on validation
	// failure. Stays a map (not typed fields) because the
	// parameter set is dynamic — defined by the picked template's
	// manifest at runtime.
	FormValues map[string]string

	// CreatedFiles is the list of rendered targets after a
	// successful POST; non-empty triggers the success view.
	CreatedFiles []string
	// CreatedSlug surfaces which template was applied (for the
	// success-view headline).
	CreatedSlug string
	// CreatedProjectID is the ID the operator chose for the new
	// project (the `projectId` template parameter). Lets the
	// success page link directly to /ui/projects/<id> so the
	// operator can review the new project without hunting through
	// the project list.
	CreatedProjectID string
}

// ProjectsNew renders the gallery at GET /ui/projects/new. When
// `?slug=<slug>` is present, renders the parameter form for that
// template instead of the grid; `?domain=<d>` filters the grid.
func (s *Server) ProjectsNew(w http.ResponseWriter, r *http.Request) {
	if api.SessionRoleFromContext(r.Context()) == auth.RoleUser {
		http.Error(w, "admin scope required", http.StatusForbidden)
		return
	}
	data := s.buildProjectsNewData(r)
	if data.SelectedSlug != "" {
		s.render(w, "projects_new_form.html", data)
		return
	}
	s.render(w, "projects_new.html", data)
}

// buildProjectsNewData hydrates the shared ProjectsNewData
// struct used by both the GET (gallery + form) and POST (after
// validation failure) code paths. Centralised so the two
// surfaces can't drift.
func (s *Server) buildProjectsNewData(r *http.Request) ProjectsNewData {
	data := ProjectsNewData{
		Title:        "New project",
		CurrentPage:  "projects",
		ActiveDomain: strings.TrimSpace(r.URL.Query().Get("domain")),
		SelectedSlug: strings.TrimSpace(r.URL.Query().Get("slug")),
		FormValues:   map[string]string{},
	}
	if s.projectTemplates == nil {
		return data
	}
	data.CatalogAvailable = true
	if strings.TrimSpace(s.configsDir) == "" {
		data.MaterialisationDisabled = true
	}
	data.Templates = s.projectTemplates.List()
	data.Domains = s.projectTemplates.Domains()
	if data.SelectedSlug != "" {
		if m, ok := s.projectTemplates.Get(data.SelectedSlug); ok {
			data.SelectedManifest = m
		} else {
			data.SelectedSlug = ""
			data.Error = "Unknown template — pick one from the gallery."
		}
	}
	if data.ActiveDomain != "" {
		filtered := make([]templates.Manifest, 0, len(data.Templates))
		for _, m := range data.Templates {
			if m.Domain == data.ActiveDomain {
				filtered = append(filtered, m)
			}
		}
		data.Templates = filtered
	}
	return data
}

// ProjectsCreateFromTemplate handles POST /ui/projects/new. The
// body is an HTML form (Content-Type application/x-www-form-
// urlencoded); we collect the parameter values, hand off to the
// shared templates.Catalog.MaterialiseFiles, and write the files
// to s.configsDir. On validation failure we re-render the form
// with the operator's values intact; on success we render a
// confirmation view with links into the new project.
func (s *Server) ProjectsCreateFromTemplate(w http.ResponseWriter, r *http.Request) {
	if api.SessionRoleFromContext(r.Context()) == auth.RoleUser {
		http.Error(w, "admin scope required", http.StatusForbidden)
		return
	}
	if s.projectTemplates == nil {
		http.Error(w, "Project-template catalog not wired", http.StatusServiceUnavailable)
		return
	}
	if strings.TrimSpace(s.configsDir) == "" {
		http.Error(w, "Daemon doesn't know where to write rendered project files", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form payload: "+err.Error(), http.StatusBadRequest)
		return
	}
	slug := strings.TrimSpace(r.FormValue("slug"))
	if slug == "" {
		http.Error(w, "Missing slug — pick a template from the gallery first.", http.StatusBadRequest)
		return
	}
	manifest, ok := s.projectTemplates.Get(slug)
	if !ok {
		http.Error(w, "Unknown template: "+slug, http.StatusBadRequest)
		return
	}

	// Collect declared parameters from the form. Form fields are
	// prefixed `p_<name>` so the slug + meta fields don't collide
	// with user parameters.
	params := make(map[string]string, len(manifest.Parameters))
	for _, p := range manifest.Parameters {
		params[p.Name] = strings.TrimSpace(r.FormValue("p_" + p.Name))
	}

	rendered, err := s.projectTemplates.MaterialiseFiles(manifest, params)
	if err != nil {
		// ValidationError → re-render with operator's values so
		// they don't lose their work. Other errors → 500.
		var ve *templates.ValidationError
		if errors.As(err, &ve) {
			data := s.buildProjectsNewData(r)
			data.SelectedSlug = slug
			data.SelectedManifest = manifest
			data.Error = err.Error()
			data.FormValues = params
			s.render(w, "projects_new_form.html", data)
			return
		}
		http.Error(w, "Render failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	written, err := templates.WriteRenderedFilesExclusive(s.configsDir, rendered)
	if err != nil {
		var exists *templates.ExistingTargetError
		if errors.As(err, &exists) {
			data := s.buildProjectsNewData(r)
			data.SelectedSlug = slug
			data.SelectedManifest = manifest
			data.Error = "A project at " + exists.Target + " already exists. Pick a different ID or delete the existing one first."
			data.FormValues = params
			s.render(w, "projects_new_form.html", data)
			return
		}
		http.Error(w, "Write failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Re-read config so the freshly written project is live in the
	// registry immediately — without this the project stays invisible
	// (GetProject → nil, UI shows "Project Not Found") until the daemon
	// restarts. Mirrors every other config-write handler, which reload
	// after persisting. A reload failure is surfaced but non-fatal: the
	// files are on disk, so a later restart/reload still picks them up.
	data := s.buildProjectsNewData(r)
	if s.configReloader != nil {
		if err := s.configReloader.Reload(); err != nil {
			data.SelectedSlug = slug
			data.SelectedManifest = manifest
			data.FormValues = params
			data.Error = "Created " + strings.Join(written, ", ") +
				" but daemon reload failed: " + err.Error() +
				"\nThe files are on disk; restart the daemon or fix the cause and retry."
			w.WriteHeader(http.StatusConflict)
			s.render(w, "projects_new_form.html", data)
			return
		}
	}

	data.CreatedSlug = slug
	data.CreatedFiles = written
	data.CreatedProjectID = params["projectId"]
	s.render(w, "projects_new_success.html", data)
}
