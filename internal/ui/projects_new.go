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

	// FormValuesMulti carries multi-value fields (list textarea
	// content re-joined with \n, multiselect checked values) for
	// re-render after validation failure. FormValues stays for
	// scalar fields (existing template references).
	FormValuesMulti map[string][]string
	// ResolvedOptions holds submit-and-render-time dynamic options
	// per optionsFrom parameter name; OptionErrors the per-field
	// resolution failure (field rendered disabled with the error).
	ResolvedOptions map[string][]string
	OptionErrors    map[string]string
	// SuggestedSlug is the free projectId offered after an ID
	// conflict (banner + prefilled field).
	SuggestedSlug string
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
		Title:           "New project",
		CurrentPage:     "projects",
		ActiveDomain:    strings.TrimSpace(r.URL.Query().Get("domain")),
		SelectedSlug:    strings.TrimSpace(r.URL.Query().Get("slug")),
		FormValues:      map[string]string{},
		FormValuesMulti: map[string][]string{},
	}
	if s.projectTemplates == nil {
		s.resolveDynamicOptions(&data)
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
	s.resolveDynamicOptions(&data)
	return data
}

// resolveDynamicOptions resolves every optionsFrom parameter on
// data.SelectedManifest into data.ResolvedOptions, recording a
// per-field message in data.OptionErrors on failure instead of
// letting the form render a silently empty select (spec
// error-handling contract). Called both from buildProjectsNewData
// (GET path) and again by ProjectsCreateFromTemplate after it
// overrides SelectedSlug/SelectedManifest post-validation-failure —
// buildProjectsNewData alone can't see the POST body's slug, only
// the query string, so a bare call there would leave dynamic options
// unresolved on a failed-then-re-rendered form.
func (s *Server) resolveDynamicOptions(data *ProjectsNewData) {
	data.ResolvedOptions = map[string][]string{}
	data.OptionErrors = map[string]string{}
	if data.SelectedSlug == "" {
		return
	}
	for _, p := range data.SelectedManifest.Parameters {
		if p.OptionsFrom == "" {
			continue
		}
		if s.templateOptions == nil {
			data.OptionErrors[p.Name] = "dynamic options unavailable (resolver not wired)"
			continue
		}
		opts, err := s.templateOptions.ResolveOptions(p.OptionsFrom)
		if err != nil {
			data.OptionErrors[p.Name] = "could not load options from " + p.OptionsFrom + ": " + err.Error()
			continue
		}
		data.ResolvedOptions[p.Name] = opts
	}
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
	// with user parameters. List params post as one textarea (one
	// value per line, split here); multiselect params post as
	// repeated checkbox values under the same name; everything else
	// stays a single scalar value.
	multi := make(map[string][]string, len(manifest.Parameters))
	for _, p := range manifest.Parameters {
		switch strings.ToLower(p.Type) {
		case "list":
			raw := r.FormValue("p_" + p.Name)
			multi[p.Name] = normalizeTemplateListValues(raw)
		case "multiselect":
			multi[p.Name] = r.Form["p_"+p.Name]
		default:
			multi[p.Name] = []string{strings.TrimSpace(r.FormValue("p_" + p.Name))}
		}
	}
	// flat collapses multi to the legacy map[string]string shape
	// ("last value wins" for anything scalar) so unchanged template
	// refs (data.FormValues, MaterialiseFiles) keep working
	// regardless of which materialisation path runs below.
	flat := make(map[string]string, len(multi))
	for k, v := range multi {
		if len(v) > 0 {
			flat[k] = v[len(v)-1]
		}
	}

	var rendered map[string]string
	var err error
	if manifest.NeedsMultiValue() {
		rendered, err = s.projectTemplates.MaterialiseFilesMulti(manifest, multi, s.templateOptions)
	} else {
		rendered, err = s.projectTemplates.MaterialiseFiles(manifest, flat)
	}
	if err != nil {
		// ValidationError → re-render with operator's values so
		// they don't lose their work. Other errors → 500.
		var ve *templates.ValidationError
		if errors.As(err, &ve) {
			data := s.buildProjectsNewData(r)
			data.SelectedSlug = slug
			data.SelectedManifest = manifest
			s.resolveDynamicOptions(&data)
			data.Error = err.Error()
			data.FormValues = flat
			data.FormValuesMulti = multi
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
			s.resolveDynamicOptions(&data)

			pid := ""
			if v := multi["projectId"]; len(v) > 0 {
				pid = v[len(v)-1]
			}
			if sug := templates.SuggestFreeProjectID(s.configsDir, pid); sug != "" && sug != pid {
				data.SuggestedSlug = sug
				data.Error = "A project at " + exists.Target + " already exists. Suggested free ID: " + sug + "."
			} else {
				data.Error = "A project at " + exists.Target + " already exists. Pick a different ID or delete the existing one first."
			}
			data.FormValues = flat
			data.FormValuesMulti = multi
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
			s.resolveDynamicOptions(&data)
			data.FormValues = flat
			data.FormValuesMulti = multi
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
	data.CreatedProjectID = flat["projectId"]
	s.render(w, "projects_new_success.html", data)
}

func normalizeTemplateListValues(raw string) []string {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")
	lines := strings.Split(raw, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if v := strings.TrimSpace(line); v != "" {
			out = append(out, v)
		}
	}
	return out
}
