package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"vornik.io/vornik/internal/templates"
)

// Project template gallery — 2026.6.0 SaaS-readiness feature 2.
// Two endpoints:
//
//   GET  /api/v1/project-templates           — list available templates
//   POST /api/v1/projects/from-template      — materialise a template
//
// Templates live under VORNIK_TEMPLATES_DIR (typically
// configs/project-templates/<slug>/) and are loaded once at daemon
// startup. Materialisation writes the rendered files into the
// daemon's configs/ tree and relies on the existing registry
// watcher to pick the new project up.

// projectTemplateSummary is the per-template payload of
// GET /api/v1/project-templates. Keeps the response small enough
// for the gallery card grid + a CLI listing without per-template
// detail round-trips.
type projectTemplateSummary struct {
	Slug        string            `json:"slug"`
	DisplayName string            `json:"displayName"`
	Description string            `json:"description"`
	Domain      string            `json:"domain"`
	Parameters  []paramDescriptor `json:"parameters"`
}

// paramDescriptor is the shape the form-builder UI consumes.
// Mirrors templates.Parameter but with explicit JSON tags so the
// wire format stays stable independent of internal refactors.
type paramDescriptor struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Label       string   `json:"label"`
	Description string   `json:"description,omitempty"`
	Default     string   `json:"default,omitempty"`
	Pattern     string   `json:"pattern,omitempty"`
	Options     []string `json:"options,omitempty"`
	Required    bool     `json:"required,omitempty"`
}

// ListProjectTemplates handles GET /api/v1/project-templates.
// Returns the loaded catalog as a JSON list. When the catalog
// isn't wired (no VORNIK_TEMPLATES_DIR / no
// configs/project-templates/ on disk), the endpoint returns
// 503 — the operator typically wants to know they're missing
// the gallery rather than seeing an empty 200.
func (s *Server) ListProjectTemplates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use GET")
		return
	}
	if s.projectTemplates == nil {
		respondError(w, http.StatusServiceUnavailable, "TEMPLATES_NOT_CONFIGURED",
			"project-template catalog not wired; check VORNIK_TEMPLATES_DIR and the configs/project-templates/ directory")
		return
	}
	manifests := s.projectTemplates.List()
	out := make([]projectTemplateSummary, 0, len(manifests))
	for _, m := range manifests {
		params := make([]paramDescriptor, 0, len(m.Parameters))
		for _, p := range m.Parameters {
			params = append(params, paramDescriptor{
				Name:        p.Name,
				Type:        p.Type,
				Label:       p.Label,
				Description: p.Description,
				Default:     p.Default,
				Pattern:     p.Pattern,
				Options:     p.Options,
				Required:    p.Required,
			})
		}
		out = append(out, projectTemplateSummary{
			Slug:        m.Slug,
			DisplayName: m.DisplayName,
			Description: m.Description,
			Domain:      m.Domain,
			Parameters:  params,
		})
	}
	respondJSON(w, http.StatusOK, map[string]any{"templates": out, "total": len(out)})
}

// createProjectFromTemplateRequest is the wire shape for the
// POST endpoint. `slug` selects the template; `parameters` is the
// flat map of values the form / CLI collected.
type createProjectFromTemplateRequest struct {
	Slug       string            `json:"slug"`
	Parameters map[string]string `json:"parameters"`
}

// createProjectFromTemplateResponse echoes back which files were
// written so the CLI can show "Created N files" without a follow-up
// directory walk. The shape is also useful in tests for sanity
// checks.
type createProjectFromTemplateResponse struct {
	Slug         string   `json:"slug"`
	FilesWritten []string `json:"filesWritten"`
}

// CreateProjectFromTemplate handles
// POST /api/v1/projects/from-template. Materialises every file in
// the chosen template, writes them to configs/, and triggers a
// registry reload via the existing watcher (no explicit signal —
// fsnotify catches the new files).
//
// Refusals:
//
//   - 503 if the catalog isn't wired
//   - 503 if configsDir isn't set (no target for the writes)
//   - 400 if the slug doesn't exist or parameters fail validation
//   - 409 if the rendered target path already exists (refuses to
//     overwrite — the UI should suggest a different projectId)
//   - 500 on filesystem errors
func (s *Server) CreateProjectFromTemplate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use POST")
		return
	}
	if !s.requireAdminGate(w, r) {
		return
	}
	if s.projectTemplates == nil {
		respondError(w, http.StatusServiceUnavailable, "TEMPLATES_NOT_CONFIGURED",
			"project-template catalog not wired")
		return
	}
	if strings.TrimSpace(s.configsDir) == "" {
		respondError(w, http.StatusServiceUnavailable, "CONFIGS_DIR_NOT_CONFIGURED",
			"daemon doesn't know where to write rendered project files")
		return
	}

	body, err := readLimitedBody(w, r, 64*1024)
	if err != nil {
		respondError(w, http.StatusBadRequest, "READ_FAILED", err.Error())
		return
	}
	defer func() { _ = r.Body.Close() }()

	var req createProjectFromTemplateRequest
	if uerr := json.Unmarshal(body, &req); uerr != nil {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", uerr.Error())
		return
	}
	req.Slug = strings.TrimSpace(req.Slug)
	if req.Slug == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "slug is required")
		return
	}

	manifest, ok := s.projectTemplates.Get(req.Slug)
	if !ok {
		respondError(w, http.StatusBadRequest, "UNKNOWN_TEMPLATE",
			"no template with slug "+req.Slug+" — call GET /api/v1/project-templates to list")
		return
	}

	rendered, err := s.projectTemplates.MaterialiseFiles(manifest, req.Parameters)
	if err != nil {
		// ValidationError surfaces as a 400 with the field name
		// so the UI can highlight the offending input. Other
		// errors (filesystem read of the source template, etc.)
		// are 500.
		var ve *templates.ValidationError
		if errors.As(err, &ve) {
			respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
			return
		}
		respondError(w, http.StatusInternalServerError, "RENDER_FAILED", err.Error())
		return
	}

	written, err := templates.WriteRenderedFilesExclusive(s.configsDir, rendered)
	if err != nil {
		var exists *templates.ExistingTargetError
		if errors.As(err, &exists) {
			respondError(w, http.StatusConflict, "FILE_EXISTS",
				"file already exists: "+exists.Target+" — pick a different projectId or remove the existing project first")
			return
		}
		respondError(w, http.StatusInternalServerError, "WRITE_FAILED", err.Error())
		return
	}

	// Register the new project in-memory now so a follow-on navigation
	// to /ui/projects/{id} resolves immediately rather than racing the
	// async file-watcher (the 2026-05-30 "created project not picked up
	// until restart" bug). Best-effort: files are already written, so a
	// reload failure leaves the watcher fallback intact.
	if s.reloadHook != nil {
		_ = s.reloadHook()
	}

	respondJSON(w, http.StatusCreated, createProjectFromTemplateResponse{
		Slug:         req.Slug,
		FilesWritten: written,
	})
}
