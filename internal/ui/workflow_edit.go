package ui

// Form-driven workflow editor — Phase 1B v6, mirror of the
// swarm editor (v5). See web-authoring-ux-design.md.
//
// v1 scope:
//   - Workflow-level scalars editable: displayName, version,
//     entrypoint (dropdown of steps + terminals), maxStepVisits,
//     maxIterations, maxWallClock.
//   - Per-step prompt editable via Markdown body surgery
//     (`### <step-id>` subsections under `## Prompts`).
//   - Per-step frontmatter (type, role, on_success, on_fail,
//     timeout, retryPolicy, gates) shown read-only.
//   - Terminals shown read-only.
//
// Save flow matches the swarm editor's: split file, yaml.Node
// surgery on frontmatter, body surgery on prompts, re-parse to
// validate, write atomically, reload.

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"vornik.io/vornik/internal/registry"
)

// WorkflowEditData backs the workflow editor template.
type WorkflowEditData struct {
	Title        string
	CurrentPage  string
	WorkflowID   string
	WorkflowPath string

	// AssistProjectID is the project the assistant grounds on
	// for AI Assist clicks on step prompts. Sourced from
	// ?projectId= so the link from the project config form
	// carries context through.
	AssistProjectID string

	Error      string
	Success    string
	BackupPath string

	// Editable scalars.
	DisplayName string
	// Description is a free-form one-paragraph summary. Required
	// by the workflow_md_shape doctor check and surfaced in the
	// dashboard / picker, so we round-trip it through the form
	// (textarea — multi-line, capped at registry.WorkflowDescriptionMaxLen).
	Description   string
	Version       string
	Entrypoint    string
	MaxStepVisits int
	MaxIterations int
	MaxWallClock  string

	// CleanupArtifactsRaw is the operator-facing textarea string —
	// one artifact path per line, as Workflow.CleanupArtifacts is
	// rendered on GET and parsed back on POST. Surfaced here so an
	// edit of any other field round-trips this list verbatim
	// rather than silently stripping it (the 2026-05-18 regression).
	//
	// TODO(workflow-md-design): the long-term shape is a structural
	// round-trip in registry/workflow_md.go that preserves every
	// unknown frontmatter key by default, removing the need to
	// surface each one as an explicit form field. Tracked in
	// https://docs.vornik.io
	CleanupArtifactsRaw string

	// EntrypointOptions is the union of step ids + terminal ids
	// — both are valid entrypoints per Validate. Surfaced as a
	// dropdown so operators don't have to remember which targets
	// exist.
	EntrypointOptions []string

	// StepViews + TerminalViews drive the read-only summary
	// rows + the editable per-step prompt textareas.
	StepViews     []WorkflowStepView
	TerminalViews []WorkflowTerminalView
}

// WorkflowStepView is a per-step row: read-only frontmatter
// summary + editable prompt textarea (agent steps only).
type WorkflowStepView struct {
	ID              string
	Type            string
	Role            string
	OnSuccess       string
	OnFail          string
	Timeout         string
	RetryMaxRetries int
	RetryBackoff    string
	GatesCount      int
	SystemPrompt    string
	PromptFieldName string
	// IsAgent toggles the prompt textarea in the template —
	// gate / approval steps have no prompt to render.
	IsAgent bool
}

// WorkflowTerminalView is a terminal table row (read-only).
type WorkflowTerminalView struct {
	ID      string
	Status  string
	Message string
}

// WorkflowEdit renders the editor.
func (s *Server) WorkflowEdit(w http.ResponseWriter, r *http.Request, workflowID string) {
	data := s.workflowEditData(workflowID)
	data.AssistProjectID = r.URL.Query().Get("projectId")
	data.AssistProjectID = assistProjectFromRequest(data.AssistProjectID, s.defaultAssistProjectForWorkflow(workflowID))
	if data.Error != "" {
		w.WriteHeader(http.StatusNotFound)
	}
	s.render(w, "workflow_edit.html", data)
}

// WorkflowSave applies the form edits and writes a new WORKFLOW.md.
func (s *Server) WorkflowSave(w http.ResponseWriter, r *http.Request, workflowID string) {
	data := s.workflowEditData(workflowID)
	data.AssistProjectID = r.URL.Query().Get("projectId")
	data.AssistProjectID = assistProjectFromRequest(data.AssistProjectID, s.defaultAssistProjectForWorkflow(workflowID))
	if data.Error != "" {
		w.WriteHeader(http.StatusNotFound)
		s.render(w, "workflow_edit.html", data)
		return
	}

	if err := r.ParseForm(); err != nil {
		data.Error = "Failed to parse form: " + err.Error()
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "workflow_edit.html", data)
		return
	}
	if err := validateWorkflowFormNumbers(r); err != nil {
		data.Error = err.Error()
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "workflow_edit.html", data)
		return
	}

	// Overlay form values.
	data.DisplayName = strings.TrimSpace(r.FormValue("displayName"))
	// Description: TrimSpace so accidental trailing whitespace
	// (from a copy-pasted summary) doesn't survive into the YAML.
	// The validator caps length on the registry side, but reject
	// here too so the operator sees a fast inline error rather
	// than a registry parse failure post-write.
	data.Description = strings.TrimSpace(r.FormValue("description"))
	if len(data.Description) > registry.WorkflowDescriptionMaxLen {
		data.Error = fmt.Sprintf("description must be ≤%d characters (got %d)", registry.WorkflowDescriptionMaxLen, len(data.Description))
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "workflow_edit.html", data)
		return
	}
	data.Version = strings.TrimSpace(r.FormValue("version"))
	data.Entrypoint = strings.TrimSpace(r.FormValue("entrypoint"))
	data.MaxStepVisits = parseFormInt(r.FormValue("maxStepVisits"))
	data.MaxIterations = parseFormInt(r.FormValue("maxIterations"))
	data.MaxWallClock = strings.TrimSpace(r.FormValue("maxWallClock"))
	data.CleanupArtifactsRaw = r.FormValue("cleanupArtifacts")
	cleanupArtifacts := splitChipList(data.CleanupArtifactsRaw)
	promptUpdates := make(map[string]string, len(data.StepViews))
	stepFrontmatterPatches := []yamlPatch{}
	for i, sv := range data.StepViews {
		// Per-step frontmatter edits. Apply to every step
		// regardless of type — gates and approvals also have
		// transitions and timeouts the operator might tweak.
		newRole := strings.TrimSpace(r.FormValue("stepRole_" + sv.ID))
		newOnSuccess := strings.TrimSpace(r.FormValue("stepOnSuccess_" + sv.ID))
		newOnFail := strings.TrimSpace(r.FormValue("stepOnFail_" + sv.ID))
		newTimeout := strings.TrimSpace(r.FormValue("stepTimeout_" + sv.ID))
		data.StepViews[i].Role = newRole
		data.StepViews[i].OnSuccess = newOnSuccess
		data.StepViews[i].OnFail = newOnFail
		data.StepViews[i].Timeout = newTimeout
		// Build patches with paths like steps.<stepID>.<field>.
		// workflow.Steps is a map[string]WorkflowStep, so the
		// existing applyYAMLPatches handles the descent via
		// mapping nodes — no sequence walk needed.
		stepFrontmatterPatches = append(stepFrontmatterPatches,
			yamlPatch{Path: []string{"steps", sv.ID, "role"}, Value: newRole, RemoveIfEmpty: true},
			yamlPatch{Path: []string{"steps", sv.ID, "on_success"}, Value: newOnSuccess, RemoveIfEmpty: true},
			yamlPatch{Path: []string{"steps", sv.ID, "on_fail"}, Value: newOnFail, RemoveIfEmpty: true},
			yamlPatch{Path: []string{"steps", sv.ID, "timeout"}, Value: newTimeout, RemoveIfEmpty: true},
		)
		// Prompts only land on agent steps.
		if !sv.IsAgent {
			continue
		}
		field := "stepPrompt_" + sv.ID
		newPrompt := r.FormValue(field)
		data.StepViews[i].SystemPrompt = newPrompt
		promptUpdates[sv.ID] = newPrompt
	}

	existing, err := os.ReadFile(data.WorkflowPath)
	if err != nil {
		data.Error = "Failed to read existing workflow: " + err.Error()
		w.WriteHeader(http.StatusInternalServerError)
		s.render(w, "workflow_edit.html", data)
		return
	}

	frontmatter, body, err := registry.SplitWorkflowContent(existing, filepath.Base(data.WorkflowPath))
	if err != nil {
		data.Error = "Failed to split workflow file: " + err.Error()
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "workflow_edit.html", data)
		return
	}

	patches := []yamlPatch{
		{Path: []string{"displayName"}, Value: data.DisplayName, RemoveIfEmpty: true},
		// Description sits next to displayName in the rendered YAML
		// — RemoveIfEmpty so clearing the textarea actually deletes
		// the key rather than leaving `description: ""` litter.
		{Path: []string{"description"}, Value: data.Description, RemoveIfEmpty: true},
		{Path: []string{"version"}, Value: data.Version, RemoveIfEmpty: true},
		{Path: []string{"entrypoint"}, Value: data.Entrypoint, RemoveIfEmpty: true},
		{Path: []string{"maxStepVisits"}, Value: data.MaxStepVisits, RemoveIfEmpty: true},
		{Path: []string{"maxIterations"}, Value: data.MaxIterations, RemoveIfEmpty: true},
		{Path: []string{"maxWallClock"}, Value: data.MaxWallClock, RemoveIfEmpty: true},
		// cleanup_artifacts is the artifact-cleanup whitelist the
		// executor applies before the workflow's first step. Must
		// round-trip through the editor verbatim — see the
		// 2026-05-18 regression where the absent form field
		// silently stripped the key on every save.
		{Path: []string{"cleanup_artifacts"}, Value: cleanupArtifacts, RemoveIfEmpty: true},
	}
	patches = append(patches, stepFrontmatterPatches...)
	newFM, err := applyYAMLPatches(frontmatter, patches)
	if err != nil {
		data.Error = "Failed to apply frontmatter edits: " + err.Error()
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "workflow_edit.html", data)
		return
	}

	newBody, err := registry.ReplaceWorkflowStepPrompts(body, promptUpdates)
	if err != nil {
		data.Error = "Failed to apply prompt edits: " + err.Error()
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "workflow_edit.html", data)
		return
	}

	joined := registry.JoinWorkflowContent(newFM, newBody)

	parsed, err := registry.ParseWorkflowMarkdown(joined, filepath.Base(data.WorkflowPath))
	if err != nil {
		data.Error = trimWorkflowParserPrefix(err.Error())
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "workflow_edit.html", data)
		return
	}
	if err := parsed.Validate(filepath.Base(data.WorkflowPath)); err != nil {
		data.Error = err.Error()
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "workflow_edit.html", data)
		return
	}

	backupPath, err := writeProjectConfigAtomic(data.WorkflowPath, joined)
	if err != nil {
		data.Error = "Failed to write workflow: " + err.Error()
		w.WriteHeader(http.StatusInternalServerError)
		s.render(w, "workflow_edit.html", data)
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
			s.render(w, "workflow_edit.html", data)
			return
		}
	} else if s.projectReg != nil {
		if err := s.projectReg.Load(s.configDir()); err != nil {
			data.Error = "Saved, but registry reload failed: " + err.Error()
			if backupPath != "" {
				data.Error += "\nBackup: " + backupPath
			}
			w.WriteHeader(http.StatusConflict)
			s.render(w, "workflow_edit.html", data)
			return
		}
	}

	data.Success = "Workflow saved and reloaded."
	if backupPath != "" {
		data.Success += " Backup: " + backupPath
	}
	if s.projectReg != nil {
		if wf := s.projectReg.GetWorkflow(workflowID); wf != nil {
			populateWorkflowEditData(&data, wf)
		}
	}
	s.render(w, "workflow_edit.html", data)
}

// workflowEditData builds the initial render-state.
func (s *Server) workflowEditData(workflowID string) WorkflowEditData {
	data := WorkflowEditData{
		Title:       "Workflow: " + workflowID,
		CurrentPage: "workflows",
		WorkflowID:  workflowID,
	}
	if workflowID == "" || strings.Contains(workflowID, "/") || strings.Contains(workflowID, string(os.PathSeparator)) {
		data.Error = "Invalid workflow id"
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
	wf := s.projectReg.GetWorkflow(workflowID)
	if wf == nil {
		data.Error = "Workflow not found"
		return data
	}
	data.WorkflowPath = filepath.Join(configDir, "workflows", workflowID+".md")
	if _, err := os.Stat(data.WorkflowPath); err != nil {
		data.Error = "Workflow file not found: " + err.Error()
		return data
	}
	populateWorkflowEditData(&data, wf)
	return data
}

// populateWorkflowEditData copies the in-memory workflow into
// the render data, building step/terminal rows in a stable
// order (lexicographic by id) so the form doesn't reorder
// itself between renders.
func populateWorkflowEditData(data *WorkflowEditData, wf *registry.Workflow) {
	data.DisplayName = wf.DisplayName
	data.Description = wf.Description
	data.Version = wf.Version
	data.Entrypoint = wf.Entrypoint
	data.MaxStepVisits = wf.MaxStepVisits
	data.MaxIterations = wf.MaxIterations
	data.MaxWallClock = wf.MaxWallClock
	// Newline-separated for the textarea. Mirrors the swarm
	// editor's roleAllowedTools_<name> shape: operator types
	// one entry per line, splitChipList on save dedupes +
	// trims, RemoveIfEmpty deletes the whole key when the
	// textarea is empty.
	data.CleanupArtifactsRaw = strings.Join(wf.CleanupArtifacts, "\n")

	// Stable, lexicographic step + terminal ordering.
	stepIDs := make([]string, 0, len(wf.Steps))
	for id := range wf.Steps {
		stepIDs = append(stepIDs, id)
	}
	termIDs := make([]string, 0, len(wf.Terminals))
	for id := range wf.Terminals {
		termIDs = append(termIDs, id)
	}
	stepIDs = sortStringsLocal(stepIDs)
	termIDs = sortStringsLocal(termIDs)

	data.StepViews = make([]WorkflowStepView, 0, len(stepIDs))
	for _, id := range stepIDs {
		s := wf.Steps[id]
		data.StepViews = append(data.StepViews, WorkflowStepView{
			ID:              id,
			Type:            s.Type,
			Role:            s.Role,
			OnSuccess:       s.OnSuccess,
			OnFail:          s.OnFail,
			Timeout:         s.Timeout,
			RetryMaxRetries: s.RetryPolicy.MaxRetries,
			RetryBackoff:    s.RetryPolicy.Backoff,
			GatesCount:      len(s.Gates),
			SystemPrompt:    s.Prompt,
			PromptFieldName: "stepPrompt_" + id,
			IsAgent:         s.Type == "agent",
		})
	}
	data.TerminalViews = make([]WorkflowTerminalView, 0, len(termIDs))
	for _, id := range termIDs {
		t := wf.Terminals[id]
		data.TerminalViews = append(data.TerminalViews, WorkflowTerminalView{
			ID:      id,
			Status:  t.Status,
			Message: t.Message,
		})
	}

	// Entrypoint dropdown: every step + every terminal id.
	data.EntrypointOptions = make([]string, 0, len(stepIDs)+len(termIDs))
	data.EntrypointOptions = append(data.EntrypointOptions, stepIDs...)
	data.EntrypointOptions = append(data.EntrypointOptions, termIDs...)
}

// sortStringsLocal is a copy of the sort helper in
// swarm_md_body_edit.go's registry package — kept here to avoid
// importing sort for one call site. Small insertion sort.
func sortStringsLocal(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// trimWorkflowParserPrefix strips the noisy `WORKFLOW.md <file>:`
// lead from parser error messages so the inline form banner
// shows just the actionable reason.
func trimWorkflowParserPrefix(msg string) string {
	if i := strings.Index(msg, ": "); i >= 0 && strings.HasPrefix(msg, "WORKFLOW.md") {
		return strings.TrimSpace(msg[i+2:])
	}
	return msg
}
