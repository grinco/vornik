package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	"vornik.io/vornik/internal/telemetryclient"
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
	// Setup surfaces the manifest's setup: block as short
	// operator-facing "Needs: ..." chips (SetupSpec.Summary(), nil
	// receiver-safe) so the gallery card shows prerequisites before
	// the user commits to a template.
	Setup []string `json:"setup,omitempty"`
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
	// OptionsFrom names the dynamic option source (mcp_registry |
	// models) declared in the manifest, if any — provenance for the
	// UI so it knows Options below came from a live resolve rather
	// than a static manifest list.
	OptionsFrom string `json:"optionsFrom,omitempty"`
	// OptionsError carries the resolver failure message when
	// OptionsFrom is set but resolution failed at listing time.
	// Options stays at its manifest default (usually empty) in that
	// case; the form falls back to free-text entry.
	OptionsError string `json:"optionsError,omitempty"`
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
	if !s.requireAdminGate(w, r) {
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
			d := paramDescriptor{
				Name:        p.Name,
				Type:        p.Type,
				Label:       p.Label,
				Description: p.Description,
				Default:     p.Default,
				Pattern:     p.Pattern,
				Options:     p.Options,
				Required:    p.Required,
				OptionsFrom: p.OptionsFrom,
			}
			// Inline the resolved option set for dynamic sources so
			// the gallery/CLI can render a select without a
			// follow-up round-trip. Resolution failure surfaces as
			// OptionsError rather than failing the whole listing —
			// one broken source shouldn't hide every other template.
			if p.OptionsFrom != "" && s.templateOptions != nil {
				if opts, rerr := s.templateOptions.ResolveOptions(p.OptionsFrom); rerr == nil {
					d.Options = opts
				} else {
					d.OptionsError = rerr.Error()
				}
			}
			params = append(params, d)
		}
		out = append(out, projectTemplateSummary{
			Slug:        m.Slug,
			DisplayName: m.DisplayName,
			Description: m.Description,
			Domain:      m.Domain,
			Parameters:  params,
			Setup:       m.Setup.Summary(),
		})
	}
	respondJSON(w, http.StatusOK, map[string]any{"templates": out, "total": len(out)})
}

// paramValues accepts a JSON string or array-of-strings for one
// parameter — the wire back-compat shim for list parameters
// (spec back-compat contract item 4: existing string-valued
// clients keep working unchanged).
type paramValues []string

// UnmarshalJSON tries a plain string first (today's wire shape),
// then falls back to an array of strings (list/multiselect
// parameters introduced alongside this shim).
func (p *paramValues) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*p = []string{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return fmt.Errorf("parameter values must be a string or an array of strings")
	}
	*p = many
	return nil
}

// createProjectFromTemplateRequest is the wire shape for the
// POST endpoint. `slug` selects the template; `parameters` is the
// map of values the form / CLI collected — each value is either a
// JSON string (scalar parameter) or an array of strings (list /
// multiselect parameter).
type createProjectFromTemplateRequest struct {
	Slug       string                 `json:"slug"`
	Parameters map[string]paramValues `json:"parameters"`
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

	// Flatten the wire shape (map[string]paramValues) to the
	// map[string][]string ValidateParamsMulti/MaterialiseFilesMulti
	// expect. Scalar-only manifests keep the byte-identical legacy
	// path (spec back-compat contract item 1) by collapsing back to
	// map[string]string and calling MaterialiseFiles directly.
	multi := make(map[string][]string, len(req.Parameters))
	for k, v := range req.Parameters {
		multi[k] = v
	}
	var rendered map[string]string
	if manifest.NeedsMultiValue() {
		rendered, err = s.projectTemplates.MaterialiseFilesMulti(manifest, multi, s.templateOptions)
	} else {
		flat := make(map[string]string, len(multi))
		for k, v := range multi {
			if len(v) > 0 {
				flat[k] = v[len(v)-1]
			}
		}
		rendered, err = s.projectTemplates.MaterialiseFiles(manifest, flat)
	}
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
			// Mirrors respondError's exact {"error":{"code",
			// "message"}} shape (checked against
			// internal/api/middleware.go's respondError) so
			// existing consumers parse this 409 unchanged, plus a
			// new top-level "suggestedSlug" (omitted when there's
			// no free ID to suggest).
			message := "file already exists: " + exists.Target + " — pick a different projectId or remove the existing project first"
			payload := map[string]any{
				"error": map[string]any{
					"code":    "FILE_EXISTS",
					"message": message,
				},
			}
			if pid := lastValue(multi["projectId"]); pid != "" {
				if sug := templates.SuggestFreeProjectID(s.configsDir, pid); sug != "" && sug != pid {
					payload["suggestedSlug"] = sug
				}
			}
			respondJSON(w, http.StatusConflict, payload)
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

	// The catalog lookup above proves this is a built-in template. Emission is
	// best-effort and must not alter the already-successful API result.
	//
	// The response is already written, so the client may hang up immediately.
	// WithoutCancel detaches from that: inheriting r.Context() let a prompt
	// disconnect cancel the emit, silently undercounting creations that in fact
	// succeeded. The client's own 2s timeout still bounds how long this holds
	// the handler.
	emitCtx, cancelEmit := context.WithTimeout(context.WithoutCancel(r.Context()), 3*time.Second)
	defer cancelEmit()
	_ = s.lifecycleTelemetry.Emit(emitCtx, telemetryclient.ProjectEvent(
		s.BuildVersion(),
		telemetryclient.SourceAPITemplate,
		req.Slug,
		true,
		renderedProjectAutonomy(rendered),
	))
}

func renderedProjectAutonomy(rendered map[string]string) bool {
	for target, body := range rendered {
		if !strings.HasPrefix(target, "projects/") || !strings.HasSuffix(target, ".yaml") {
			continue
		}
		var project struct {
			Autonomy struct {
				Enabled bool `yaml:"enabled"`
			} `yaml:"autonomy"`
		}
		if yaml.Unmarshal([]byte(body), &project) == nil {
			return project.Autonomy.Enabled
		}
	}
	return false
}

// lastValue returns the last element of v, or "" when v is empty —
// scalar-collapse helper matching ValidateParamsMulti's "last value
// wins" convention for the projectId lookup used to build the
// suggestedSlug hint.
func lastValue(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[len(v)-1]
}
