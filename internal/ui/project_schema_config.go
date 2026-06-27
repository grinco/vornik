package ui

// Schema-driven project config editor — UI asset-management feature
// phase 1 (P1c). See https://docs.vornik.io
//
// Unlike the hand-built project_config_form.go (whose Go struct + HTML
// drift behind registry.Project as fields are added), this surface is
// rendered entirely from the curated assetschema.ProjectSchema(): one
// generic template walks the schema's sections + fields, the binder
// reads the POST back against the same schema, and a drift-guard test
// fails CI if a new struct field has no schema entry. One spine, no
// drift.
//
// Save flow reuses the existing primitives verbatim:
//   schema bind → []yamlPatch → field-allowlist guard → applyYAMLPatches
//   → validateProjectConfigEdit (sandbox registry load) →
//   writeProjectConfigAtomic → configReloader → admin-audit row.
// The yaml.Node patcher preserves comments, commented-out scaffolds,
// and key order because it never unmarshal/remarshals the document.

import (
	"fmt"
	"net/http"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
	"vornik.io/vornik/internal/fieldguard"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/ui/assetschema"
)

// ProjectSchemaConfigData backs the generic schema-driven editor
// template. The template renders Schema's sections/fields and looks up
// each field's current value (and any per-field error) by dotted path.
type ProjectSchemaConfigData struct {
	Title       string
	CurrentPage string
	ProjectID   string
	ConfigPath  string
	Error       string
	Success     string
	BackupPath  string

	// Schema is the curated editable surface; the template iterates it.
	Schema assetschema.AssetSchema
	// Values maps each field's dotted path → display string (pre-fill).
	Values map[string]string
	// FieldErrors maps a field path → its validation message, surfaced
	// inline on a rejected save with the operator's input preserved.
	FieldErrors map[string]string
}

// ProjectSchemaConfigEdit renders the schema-driven project editor (GET).
func (s *Server) ProjectSchemaConfigEdit(w http.ResponseWriter, r *http.Request, projectID string) {
	data := s.projectSchemaConfigData(projectID)
	if data.Error != "" {
		w.WriteHeader(http.StatusNotFound)
	}
	s.render(w, "asset_schema_config.html", data)
}

// ProjectSchemaConfigSave binds the POST against the schema, patches the
// YAML surgically, validates, writes atomically, reloads, and audits.
// On any failure it re-renders the form with the operator's input and a
// precise error; nothing is written unless every step succeeds.
func (s *Server) ProjectSchemaConfigSave(w http.ResponseWriter, r *http.Request, projectID string) {
	// Rewriting a project YAML (autonomy gates, tool allowlists, rate
	// limits) is an authoring action — admin scope, matching the
	// raw-YAML and guided-form editors.
	if !s.uiRequireAdminMutation(w, r) {
		return
	}

	data := s.projectSchemaConfigData(projectID)
	if data.Error != "" {
		w.WriteHeader(http.StatusNotFound)
		s.render(w, "asset_schema_config.html", data)
		return
	}

	if err := r.ParseForm(); err != nil {
		data.Error = "Failed to parse form: " + err.Error()
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "asset_schema_config.html", data)
		return
	}

	schema := data.Schema
	values, fieldErrs := assetschema.BindForm(schema, r.FormValue)
	// Overlay the operator's raw input so a rejected save re-renders
	// what they just typed (not the on-disk values).
	for _, f := range schema.Fields() {
		if f.ReadOnly {
			continue
		}
		data.Values[f.Path] = strings.TrimSpace(r.FormValue(f.Path))
	}
	if len(fieldErrs) > 0 {
		data.FieldErrors = make(map[string]string, len(fieldErrs))
		msgs := make([]string, 0, len(fieldErrs))
		for _, fe := range fieldErrs {
			data.FieldErrors[fe.Path] = fe.Message
			msgs = append(msgs, fe.Message)
		}
		data.Error = strings.Join(msgs, "; ")
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "asset_schema_config.html", data)
		return
	}

	existing, err := os.ReadFile(data.ConfigPath)
	if err != nil {
		data.Error = "Failed to read existing config: " + err.Error()
		w.WriteHeader(http.StatusInternalServerError)
		s.render(w, "asset_schema_config.html", data)
		return
	}

	patches := schemaPatches(schema, values)
	// Field-allowlist guard: a patch may only touch a top-level key the
	// schema actually declares. Derived from the schema so it stays in
	// lockstep — a typo'd or out-of-schema path fails loudly before any
	// write rather than silently corrupting a protected field.
	if err := schemaTopLevelGuard(schema).Check(topLevelPatchKeys(patches)); err != nil {
		data.Error = "Refused: " + err.Error()
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "asset_schema_config.html", data)
		return
	}

	patched, err := applyYAMLPatches(existing, patches)
	if err != nil {
		data.Error = "Failed to apply form edits: " + err.Error()
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "asset_schema_config.html", data)
		return
	}

	if err := validateProjectConfigEdit(s.configDir(), projectID, patched); err != nil {
		data.Error = "Validation failed: " + err.Error()
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "asset_schema_config.html", data)
		return
	}

	backupPath, err := writeProjectConfigAtomic(data.ConfigPath, patched)
	if err != nil {
		data.Error = "Failed to write config: " + err.Error()
		w.WriteHeader(http.StatusInternalServerError)
		s.render(w, "asset_schema_config.html", data)
		return
	}
	data.BackupPath = backupPath

	if s.configReloader != nil {
		if err := s.configReloader.Reload(); err != nil {
			data.Error = "Saved, but reload failed: " + err.Error()
			if backupPath != "" {
				data.Error += "\nBackup: " + backupPath
			}
			w.WriteHeader(http.StatusConflict)
			s.render(w, "asset_schema_config.html", data)
			return
		}
	} else if s.projectReg != nil {
		if err := s.projectReg.Load(s.configDir()); err != nil {
			data.Error = "Saved, but registry reload failed: " + err.Error()
			if backupPath != "" {
				data.Error += "\nBackup: " + backupPath
			}
			w.WriteHeader(http.StatusConflict)
			s.render(w, "asset_schema_config.html", data)
			return
		}
	}

	s.writeProjectSaveAudit(r, projectID, len(patches))

	data.Success = "Project config saved and reloaded."
	if backupPath != "" {
		data.Success += " Backup: " + backupPath
	}
	// Re-render from the canonical on-disk state so the operator sees
	// any normalisation the validator applied.
	if reloaded := s.currentProjectValues(data.Schema, data.ConfigPath); reloaded != nil {
		data.Values = reloaded
	}
	s.render(w, "asset_schema_config.html", data)
}

// projectSchemaConfigData builds the initial render-state, mirroring the
// guided form's path-handling so the two views agree on validity.
// Returns data with .Error set when the project / config dir is invalid;
// callers short-circuit rendering in that case.
func (s *Server) projectSchemaConfigData(projectID string) ProjectSchemaConfigData {
	data := ProjectSchemaConfigData{
		Title:       "Project Config (schema): " + projectID,
		CurrentPage: "projects",
		ProjectID:   projectID,
		Schema:      assetschema.ProjectSchema(),
		Values:      map[string]string{},
	}
	if projectID == "" || strings.Contains(projectID, "/") || strings.Contains(projectID, string(os.PathSeparator)) {
		data.Error = "Invalid project id"
		return data
	}
	configDir := s.configDir()
	if configDir == "" {
		data.Error = "Registry config directory is not configured"
		return data
	}
	data.ConfigPath = configDir + "/projects/" + projectID + ".yaml"
	if _, err := os.Stat(data.ConfigPath); err != nil {
		data.Error = "Project config not found: " + err.Error()
		return data
	}
	if vals := s.currentProjectValues(data.Schema, data.ConfigPath); vals != nil {
		data.Values = vals
	}
	return data
}

// currentProjectValues decodes the project YAML and maps it to the
// schema's path→display values for form pre-fill. Returns nil on a read
// or decode error (the caller keeps the empty map).
func (s *Server) currentProjectValues(schema assetschema.AssetSchema, configPath string) map[string]string {
	content, err := os.ReadFile(configPath)
	if err != nil {
		return nil
	}
	var doc map[string]any
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return nil
	}
	return assetschema.CurrentValues(schema, doc)
}

// schemaPatches converts bound, type-checked field values into the
// yaml.Node patches the surgical patcher consumes. Body-backed fields
// (prose that lives in a markdown body, not the frontmatter) are skipped
// — they route through the body editor, not the node patcher. Read-only
// fields never reach here (BindForm drops them). A non-provided optional
// value patches with RemoveIfEmpty so clearing a field deletes its key
// rather than leaving `key: ""` litter.
func schemaPatches(s assetschema.AssetSchema, values []assetschema.FieldValue) []yamlPatch {
	out := make([]yamlPatch, 0, len(values))
	for _, v := range values {
		if f, ok := s.FieldByPath(v.Path); ok && f.IsBody() {
			continue
		}
		out = append(out, yamlPatch{
			Path:          strings.Split(v.Path, "."),
			Value:         v.Value,
			RemoveIfEmpty: !v.Provided,
		})
	}
	return out
}

// schemaTopLevelGuard builds the field-allowlist of top-level YAML keys
// the schema is permitted to write, derived from the schema itself so it
// can't drift. Read-only and body-backed fields are excluded (they never
// produce a frontmatter patch).
func schemaTopLevelGuard(s assetschema.AssetSchema) *fieldguard.Guard {
	seen := map[string]struct{}{}
	keys := make([]string, 0)
	for _, f := range s.Fields() {
		if f.ReadOnly || f.IsBody() {
			continue
		}
		top := f.Path
		if i := strings.IndexByte(top, '.'); i >= 0 {
			top = top[:i]
		}
		if _, ok := seen[top]; ok {
			continue
		}
		seen[top] = struct{}{}
		keys = append(keys, top)
	}
	return fieldguard.Allowlist(keys...)
}

// writeProjectSaveAudit records one admin-audit row for a successful
// schema-form save, closing the audit gap the design calls out (not all
// project save handlers recorded one). No-op when no audit repo is
// wired.
func (s *Server) writeProjectSaveAudit(r *http.Request, projectID string, patchedFields int) {
	if s.adminAuditRepo == nil {
		return
	}
	principal := adminPrincipal(r)
	if principal == "" || principal == "unknown" {
		principal = "ui-admin"
	}
	_ = s.adminAuditRepo.Insert(r.Context(), &persistence.AdminAuditEntry{
		Principal: principal,
		Source:    "ui",
		Action:    "project.config.save",
		Target:    projectID,
		After:     fmt.Sprintf(`{"patched_fields":%d}`, patchedFields),
		IP:        clientIP(r),
		UserAgent: r.UserAgent(),
	})
}
