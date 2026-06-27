package ui

// Form-driven swarm editor — Phase 1B v5. See
// https://docs.vornik.io
//
// v1 scope:
//   - Swarm-level scalars editable: displayName, leadRole,
//     rolePrelude.
//   - Per-role systemPrompt body editable via Markdown body
//     surgery (each `### <role>` subsection under
//     `## Role prompts` is rewritten).
//   - Per-role frontmatter (description, model, allowedTools,
//     runtime, etc.) is READ-ONLY in the form. Operators
//     iterate on those by editing the file directly. The form's
//     focus is the highest-leverage authoring surface — the
//     prompts — without dragging in sequence-aware yaml.Node
//     editing.
//
// Save flow:
//   1. Read existing SWARM.md.
//   2. SplitSwarmContent → frontmatter + body.
//   3. applyYAMLPatches(frontmatter, scalar patches) →
//      preserves comments and unmodified fields.
//   4. ReplaceSwarmRolePrompts(body, prompt map) → preserves
//      every non-prompts body section verbatim.
//   5. JoinSwarmContent → recombine.
//   6. ParseSwarmMarkdown to validate; Validate to check
//      registry-level invariants.
//   7. writeProjectConfigAtomic to the .md path.
//   8. configReloader / projectReg.Load.

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"vornik.io/vornik/internal/registry"
)

// SwarmEditData backs the swarm editor template.
type SwarmEditData struct {
	Title       string
	CurrentPage string
	SwarmID     string
	SwarmPath   string

	// AssistProjectID is the project the assistant should ground
	// on when the operator clicks AI Assist on a role prompt.
	// Set from the ?projectId= URL query so the link from the
	// project config form propagates context. Empty when the
	// editor is opened directly — the AI Assist buttons still
	// render but call out the missing project on click.
	AssistProjectID string

	Error      string
	Success    string
	BackupPath string

	// Swarm-level editable scalars.
	DisplayName string
	LeadRole    string
	RolePrelude string

	// RoleViews surfaces a row per role: read-only frontmatter
	// summary + the editable systemPrompt textarea.
	RoleViews []SwarmRoleView

	// ModelOptions feeds the per-role model dropdown when the
	// daemon's pricing table is wired. Empty list → roleModel
	// inputs fall back to free-text.
	ModelOptions []string
}

// SwarmRoleView is the per-role render struct: editable
// frontmatter fields (description, model, allowedTools) plus
// the systemPrompt textarea. Per-role frontmatter writes go
// through applyYAMLSequenceElementPatches keyed on the role's
// `name`.
type SwarmRoleView struct {
	Name          string
	Description   string
	Model         string
	ModelFallback string
	AllowedTools  string // newline-separated for the textarea
	SystemPrompt  string

	PromptFieldName        string
	DescFieldName          string
	ModelFieldName         string
	ModelFallbackFieldName string
	ToolsFieldName         string
}

// SwarmEdit renders the editor.
func (s *Server) SwarmEdit(w http.ResponseWriter, r *http.Request, swarmID string) {
	data := s.swarmEditData(swarmID)
	data.AssistProjectID = r.URL.Query().Get("projectId")
	data.AssistProjectID = assistProjectFromRequest(data.AssistProjectID, s.defaultAssistProjectForSwarm(swarmID))
	if data.Error != "" {
		w.WriteHeader(http.StatusNotFound)
	}
	s.render(w, "swarm_edit.html", data)
}

// SwarmSave applies the form edits and writes a new SWARM.md.
func (s *Server) SwarmSave(w http.ResponseWriter, r *http.Request, swarmID string) {
	data := s.swarmEditData(swarmID)
	data.AssistProjectID = r.URL.Query().Get("projectId")
	data.AssistProjectID = assistProjectFromRequest(data.AssistProjectID, s.defaultAssistProjectForSwarm(swarmID))
	if data.Error != "" {
		w.WriteHeader(http.StatusNotFound)
		s.render(w, "swarm_edit.html", data)
		return
	}

	if err := r.ParseForm(); err != nil {
		data.Error = "Failed to parse form: " + err.Error()
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "swarm_edit.html", data)
		return
	}

	// Overlay form values onto data so the post-failure re-
	// render shows the operator's most recent input.
	data.DisplayName = strings.TrimSpace(r.FormValue("displayName"))
	data.LeadRole = strings.TrimSpace(r.FormValue("leadRole"))
	data.RolePrelude = r.FormValue("rolePrelude")
	promptUpdates := make(map[string]string, len(data.RoleViews))
	// rolePatches[roleName] → patches to apply inside that role's
	// element. Built per-role; applied via the sequence helper
	// after the swarm-level frontmatter pass.
	rolePatches := make(map[string][]yamlPatch, len(data.RoleViews))
	for i, view := range data.RoleViews {
		newPrompt := r.FormValue("rolePrompt_" + view.Name)
		data.RoleViews[i].SystemPrompt = newPrompt
		promptUpdates[view.Name] = newPrompt

		newDesc := strings.TrimSpace(r.FormValue("roleDescription_" + view.Name))
		newModel := strings.TrimSpace(r.FormValue("roleModel_" + view.Name))
		newModelFallback := strings.TrimSpace(r.FormValue("roleModelFallback_" + view.Name))
		newToolsRaw := r.FormValue("roleAllowedTools_" + view.Name)
		newTools := splitChipList(newToolsRaw)
		data.RoleViews[i].Description = newDesc
		data.RoleViews[i].Model = newModel
		data.RoleViews[i].ModelFallback = newModelFallback
		data.RoleViews[i].AllowedTools = newToolsRaw

		rolePatches[view.Name] = []yamlPatch{
			{Path: []string{"description"}, Value: newDesc, RemoveIfEmpty: true},
			{Path: []string{"model"}, Value: newModel, RemoveIfEmpty: true},
			{Path: []string{"modelFallback"}, Value: newModelFallback, RemoveIfEmpty: true},
			{Path: []string{"permissions", "allowedTools"}, Value: newTools, RemoveIfEmpty: true},
		}
	}

	existing, err := os.ReadFile(data.SwarmPath)
	if err != nil {
		data.Error = "Failed to read existing swarm: " + err.Error()
		w.WriteHeader(http.StatusInternalServerError)
		s.render(w, "swarm_edit.html", data)
		return
	}

	frontmatter, body, err := registry.SplitSwarmContent(existing, filepath.Base(data.SwarmPath))
	if err != nil {
		data.Error = "Failed to split swarm file: " + err.Error()
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "swarm_edit.html", data)
		return
	}

	patches := []yamlPatch{
		{Path: []string{"displayName"}, Value: data.DisplayName, RemoveIfEmpty: true},
		{Path: []string{"leadRole"}, Value: data.LeadRole, RemoveIfEmpty: true},
		{Path: []string{"rolePrelude"}, Value: data.RolePrelude, RemoveIfEmpty: true},
	}
	newFM, err := applyYAMLPatches(frontmatter, patches)
	if err != nil {
		data.Error = "Failed to apply frontmatter edits: " + err.Error()
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "swarm_edit.html", data)
		return
	}

	// Per-role frontmatter (description, model, allowedTools)
	// goes through the sequence patcher; one pass per role so
	// each edits the matching element in the `roles:` sequence
	// without touching siblings.
	for _, view := range data.RoleViews {
		patches := rolePatches[view.Name]
		if len(patches) == 0 {
			continue
		}
		newFM, err = applyYAMLSequenceElementPatches(newFM, "roles", "name", view.Name, patches)
		if err != nil {
			data.Error = "Failed to apply role edits for " + view.Name + ": " + err.Error()
			w.WriteHeader(http.StatusBadRequest)
			s.render(w, "swarm_edit.html", data)
			return
		}
	}

	newBody, err := registry.ReplaceSwarmRolePrompts(body, promptUpdates)
	if err != nil {
		data.Error = "Failed to apply prompt edits: " + err.Error()
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "swarm_edit.html", data)
		return
	}

	joined := registry.JoinSwarmContent(newFM, newBody)

	// Validate via re-parse + struct Validate before disk write.
	parsed, err := registry.ParseSwarmMarkdown(joined, filepath.Base(data.SwarmPath))
	if err != nil {
		data.Error = trimSwarmParserPrefix(err.Error())
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "swarm_edit.html", data)
		return
	}
	if err := parsed.Validate(filepath.Base(data.SwarmPath)); err != nil {
		data.Error = err.Error()
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "swarm_edit.html", data)
		return
	}

	backupPath, err := writeProjectConfigAtomic(data.SwarmPath, joined)
	if err != nil {
		data.Error = "Failed to write swarm: " + err.Error()
		w.WriteHeader(http.StatusInternalServerError)
		s.render(w, "swarm_edit.html", data)
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
			s.render(w, "swarm_edit.html", data)
			return
		}
	} else if s.projectReg != nil {
		if err := s.projectReg.Load(s.configDir()); err != nil {
			data.Error = "Saved, but registry reload failed: " + err.Error()
			if backupPath != "" {
				data.Error += "\nBackup: " + backupPath
			}
			w.WriteHeader(http.StatusConflict)
			s.render(w, "swarm_edit.html", data)
			return
		}
	}

	data.Success = "Swarm saved and reloaded."
	if backupPath != "" {
		data.Success += " Backup: " + backupPath
	}
	// Refresh from the freshly-reloaded registry so the form
	// reflects any normalisation the loader applied.
	if s.projectReg != nil {
		if sw := s.projectReg.GetSwarm(swarmID); sw != nil {
			populateSwarmEditData(&data, sw)
			data.ModelOptions = appendRoleModelOptions(data.ModelOptions, data.RoleViews)
		}
	}
	s.render(w, "swarm_edit.html", data)
}

// swarmEditData builds the initial render-state. Mirrors the
// project-config form pattern: invalid ID → 404; swarm not in
// registry → 404; otherwise populate from the in-memory swarm.
func (s *Server) swarmEditData(swarmID string) SwarmEditData {
	data := SwarmEditData{
		Title:       "Swarm: " + swarmID,
		CurrentPage: "swarms",
		SwarmID:     swarmID,
	}
	if swarmID == "" || strings.Contains(swarmID, "/") || strings.Contains(swarmID, string(os.PathSeparator)) {
		data.Error = "Invalid swarm id"
		return data
	}
	configDir := s.configDir()
	if configDir == "" {
		data.Error = "Registry config directory is not configured"
		return data
	}
	if s.projectReg == nil {
		data.Error = "Project registry not configured"
		return data
	}
	sw := s.projectReg.GetSwarm(swarmID)
	if sw == nil {
		data.Error = "Swarm not found"
		return data
	}
	data.SwarmPath = filepath.Join(configDir, "swarms", swarmID+".md")
	if _, err := os.Stat(data.SwarmPath); err != nil {
		data.Error = "Swarm file not found: " + err.Error()
		return data
	}
	populateSwarmEditData(&data, sw)
	if s.assistantPricing != nil {
		data.ModelOptions = s.assistantPricing.IDs()
	}
	data.ModelOptions = appendRoleModelOptions(data.ModelOptions, data.RoleViews)
	return data
}

func appendRoleModelOptions(options []string, roles []SwarmRoleView) []string {
	for _, role := range roles {
		options = appendOptionIfMissing(options, role.Model)
		options = appendOptionIfMissing(options, role.ModelFallback)
	}
	return options
}

// populateSwarmEditData copies field values from a parsed
// *Swarm into the form data struct. AllowedTools is rendered
// as a newline-separated string so the textarea is paste-
// friendly (same convention as the project config form's
// chip-list fields).
func populateSwarmEditData(data *SwarmEditData, sw *registry.Swarm) {
	data.DisplayName = sw.DisplayName
	data.LeadRole = sw.LeadRole
	data.RolePrelude = sw.RolePrelude
	data.RoleViews = make([]SwarmRoleView, 0, len(sw.Roles))
	for _, r := range sw.Roles {
		data.RoleViews = append(data.RoleViews, SwarmRoleView{
			Name:                   r.Name,
			Description:            r.Description,
			Model:                  r.Model,
			ModelFallback:          r.ModelFallback,
			AllowedTools:           strings.Join(r.Permissions.AllowedTools, "\n"),
			SystemPrompt:           r.SystemPrompt,
			PromptFieldName:        "rolePrompt_" + r.Name,
			DescFieldName:          "roleDescription_" + r.Name,
			ModelFieldName:         "roleModel_" + r.Name,
			ModelFallbackFieldName: "roleModelFallback_" + r.Name,
			ToolsFieldName:         "roleAllowedTools_" + r.Name,
		})
	}
}

// trimSwarmParserPrefix strips the "SWARM.md <file>:" lead from
// parser error messages so the inline form banner shows just
// the actionable part.
func trimSwarmParserPrefix(msg string) string {
	if i := strings.Index(msg, ": "); i >= 0 && strings.HasPrefix(msg, "SWARM.md") {
		return strings.TrimSpace(msg[i+2:])
	}
	return msg
}

// Ensure registry import isn't dropped if every reference above
// is renamed away in a future refactor.
var _ = fmt.Sprintf
