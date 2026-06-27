package ui

// Phase 3 — autonomy-gated wizard that generates SWARM.md +
// WORKFLOW.md drafts from a project's brief and an LLM
// authoring assistant. The handler at POST /projects/{id}/wizard
// runs two LLM calls (one shaping the swarm, one shaping the
// workflow with the generated role names in context), validates
// each output through ParseSwarmMarkdown / ParseWorkflowMarkdown
// + Validate, writes both files atomically, repoints the
// project.yaml's swarmId / defaultWorkflowId at them, and
// reloads the registry.
//
// Gating per the LLD's "Both, gated by autonomy level" answer:
//   - autonomy disabled / requireApproval=true: refuse with 403.
//     The tune-only variant (modify an existing template) is in
//     the LLD but doesn't ship in v1 — operators run the wizard
//     once with autonomy enabled, then iterate on the result
//     via the swarm + workflow editors.
//   - autonomy enabled + requireApproval=false: full generation.
//
// Never silent: the operator must navigate to the generated
// swarm / workflow editors to inspect the draft and click Save
// (or generate-then-tweak the prompts via the editors' AI
// Assist buttons). The wizard's success response is a JSON
// envelope with the new IDs so the front-end can link there.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"vornik.io/vornik/internal/fieldguard"
	"vornik.io/vornik/internal/registry"
)

// wizardRepointGuard is the field-allowlist for the wizard's
// project-YAML repoint: after generating a new SWARM.md + WORKFLOW.md
// it rewrites only the project's swarmId + defaultWorkflowId pointers.
// The guard refuses any other top-level key, so a future edit that
// adds a stray patch can't mutate identity/config through this path.
// See internal/fieldguard.
var wizardRepointGuard = fieldguard.Allowlist("swarmId", "defaultWorkflowId")

// wizardResponse is the JSON envelope returned on POST.
type wizardResponse struct {
	SwarmID    string `json:"swarmId,omitempty"`
	WorkflowID string `json:"workflowId,omitempty"`
	Error      string `json:"error,omitempty"`
}

// wizardSwarmEnvelope / wizardWorkflowEnvelope shape the LLM
// is asked to produce. Keeping the envelope small ("just one
// field per call") makes extraction more reliable than freeform
// markdown delimiters.
type wizardSwarmEnvelope struct {
	SwarmMD string `json:"swarm_md"`
}

type wizardWorkflowEnvelope struct {
	WorkflowMD string `json:"workflow_md"`
}

// WizardGenerate is the POST handler. Authority chain:
//  1. method = POST.
//  2. project exists in the registry.
//  3. project has a PROJECT.md brief.
//  4. project.Autonomy.Enabled && !RequireApproval (full-
//     generation gate).
//  5. assistant LLM is configured.
//
// On success: writes 2 new files, repoints project.yaml,
// reloads the registry, returns 200 JSON.
func (s *Server) WizardGenerate(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeWizardJSON(w, http.StatusMethodNotAllowed, wizardResponse{Error: "method not allowed"})
		return
	}
	if s.projectReg == nil {
		writeWizardJSON(w, http.StatusInternalServerError, wizardResponse{Error: "project registry not configured"})
		return
	}
	if projectID == "" || strings.Contains(projectID, "/") || strings.Contains(projectID, string(os.PathSeparator)) {
		writeWizardJSON(w, http.StatusNotFound, wizardResponse{Error: "invalid project id"})
		return
	}
	proj := s.projectReg.GetProject(projectID)
	if proj == nil {
		writeWizardJSON(w, http.StatusNotFound, wizardResponse{Error: "project not found"})
		return
	}
	if proj.Brief == nil {
		writeWizardJSON(w, http.StatusBadRequest, wizardResponse{Error: "project has no brief — create one before running the wizard"})
		return
	}
	if !proj.Autonomy.Enabled || proj.Autonomy.RequireApproval {
		writeWizardJSON(w, http.StatusForbidden, wizardResponse{Error: "wizard requires autonomy.enabled=true and requireApproval=false"})
		return
	}
	if s.assistantLLM == nil {
		writeWizardJSON(w, http.StatusServiceUnavailable, wizardResponse{Error: "assistant LLM not configured"})
		return
	}
	if err := r.ParseForm(); err != nil {
		writeWizardJSON(w, http.StatusBadRequest, wizardResponse{Error: "form parse: " + err.Error()})
		return
	}

	// Pick the IDs. Operator override wins; otherwise default
	// to <projectId>-swarm / <projectId>-wf.
	swarmID := strings.TrimSpace(r.FormValue("swarmId"))
	if swarmID == "" {
		swarmID = projectID + "-swarm"
	}
	wfID := strings.TrimSpace(r.FormValue("workflowId"))
	if wfID == "" {
		wfID = projectID + "-wf"
	}
	if !safeWizardArtifactID(swarmID) {
		writeWizardJSON(w, http.StatusBadRequest, wizardResponse{Error: "invalid swarm id"})
		return
	}
	if !safeWizardArtifactID(wfID) {
		writeWizardJSON(w, http.StatusBadRequest, wizardResponse{Error: "invalid workflow id"})
		return
	}

	// Model resolution mirrors the assistant: project override
	// → swarm leadRole → daemon default. The current swarm
	// (seed-swarm) gives us a model; the generated swarm is
	// what we're producing.
	var currentSwarm *registry.Swarm
	if proj.SwarmID != "" {
		currentSwarm = s.projectReg.GetSwarm(proj.SwarmID)
	}
	model := resolveAssistantModelForProject(proj, currentSwarm, s.assistantDefaultModel)
	if model == "" {
		writeWizardJSON(w, http.StatusInternalServerError, wizardResponse{Error: "no assistant model resolvable for this project"})
		return
	}

	configDir := s.configDir()
	if configDir == "" {
		writeWizardJSON(w, http.StatusInternalServerError, wizardResponse{Error: "config dir not configured"})
		return
	}
	swarmPath := filepath.Join(configDir, "swarms", swarmID+".md")
	wfPath := filepath.Join(configDir, "workflows", wfID+".md")
	if _, err := os.Stat(swarmPath); err == nil {
		writeWizardJSON(w, http.StatusConflict, wizardResponse{Error: "swarm already exists: " + swarmID})
		return
	} else if !os.IsNotExist(err) {
		writeWizardJSON(w, http.StatusInternalServerError, wizardResponse{Error: "stat swarm: " + err.Error()})
		return
	}
	if _, err := os.Stat(wfPath); err == nil {
		writeWizardJSON(w, http.StatusConflict, wizardResponse{Error: "workflow already exists: " + wfID})
		return
	} else if !os.IsNotExist(err) {
		writeWizardJSON(w, http.StatusInternalServerError, wizardResponse{Error: "stat workflow: " + err.Error()})
		return
	}
	if blocked, reason := s.assistantBudgetBlocked(r.Context(), proj); blocked {
		writeWizardJSON(w, http.StatusTooManyRequests, wizardResponse{Error: "budget exceeded: " + reason})
		return
	}

	// Call 1: generate the SWARM.md.
	swarmMD, err := s.wizardGenerateSwarm(r.Context(), proj, model, swarmID)
	if err != nil {
		writeWizardJSON(w, http.StatusBadGateway, wizardResponse{Error: err.Error()})
		return
	}
	if blocked, reason := s.assistantBudgetBlocked(r.Context(), proj); blocked {
		writeWizardJSON(w, http.StatusTooManyRequests, wizardResponse{Error: "budget exceeded: " + reason})
		return
	}

	// Call 2: generate the WORKFLOW.md. The user prompt
	// includes the generated swarm so role: fields match real
	// role names rather than the LLM inventing names.
	wfMD, err := s.wizardGenerateWorkflow(r.Context(), proj, model, wfID, swarmMD)
	if err != nil {
		writeWizardJSON(w, http.StatusBadGateway, wizardResponse{Error: err.Error()})
		return
	}

	// Write files.
	if _, err := writeProjectConfigAtomic(swarmPath, []byte(swarmMD)); err != nil {
		writeWizardJSON(w, http.StatusInternalServerError, wizardResponse{Error: "write swarm: " + err.Error()})
		return
	}
	if _, err := writeProjectConfigAtomic(wfPath, []byte(wfMD)); err != nil {
		writeWizardJSON(w, http.StatusInternalServerError, wizardResponse{Error: "write workflow: " + err.Error()})
		return
	}

	// Repoint project.yaml at the new artifacts.
	projectPath := filepath.Join(configDir, "projects", projectID+".yaml")
	existing, err := os.ReadFile(projectPath)
	if err != nil {
		writeWizardJSON(w, http.StatusInternalServerError, wizardResponse{Error: "read project: " + err.Error()})
		return
	}
	repointPatches := []yamlPatch{
		{Path: []string{"swarmId"}, Value: swarmID},
		{Path: []string{"defaultWorkflowId"}, Value: wfID},
	}
	// Field-allowlist guard: the wizard repoint may only rewrite the
	// two artifact pointers — never anything else in the project YAML
	// (projectId/identity, autonomy, budget, …). Refuses before write
	// so a future stray patch can't ride the repoint into a protected
	// field.
	if err := wizardRepointGuard.Check(topLevelPatchKeys(repointPatches)); err != nil {
		writeWizardJSON(w, http.StatusInternalServerError, wizardResponse{Error: "repoint refused: " + err.Error()})
		return
	}
	patched, err := applyYAMLPatches(existing, repointPatches)
	if err != nil {
		writeWizardJSON(w, http.StatusInternalServerError, wizardResponse{Error: "patch project: " + err.Error()})
		return
	}
	if _, err := writeProjectConfigAtomic(projectPath, patched); err != nil {
		writeWizardJSON(w, http.StatusInternalServerError, wizardResponse{Error: "write project: " + err.Error()})
		return
	}

	// Reload so the new artifacts are live.
	if s.configReloader != nil {
		if err := s.configReloader.Reload(); err != nil {
			writeWizardJSON(w, http.StatusConflict, wizardResponse{Error: "saved, but reload failed: " + err.Error()})
			return
		}
	} else {
		if err := s.projectReg.Load(configDir); err != nil {
			writeWizardJSON(w, http.StatusConflict, wizardResponse{Error: "saved, but registry reload failed: " + err.Error()})
			return
		}
	}

	writeWizardJSON(w, http.StatusOK, wizardResponse{
		SwarmID:    swarmID,
		WorkflowID: wfID,
	})
}

func safeWizardArtifactID(id string) bool {
	id = strings.TrimSpace(id)
	if id == "" || id == "." || id == ".." {
		return false
	}
	if filepath.IsAbs(id) || filepath.Clean(id) != id {
		return false
	}
	return !strings.Contains(id, "/") && !strings.Contains(id, `\`) && !strings.ContainsRune(id, 0)
}

// wizardGenerateSwarm calls the assistant LLM with the project
// brief + the target swarmId, parses the JSON envelope, and
// validates the SWARM.md by re-parsing it. Returns the raw
// SWARM.md content on success.
func (s *Server) wizardGenerateSwarm(ctx context.Context, proj *registry.Project, model, swarmID string) (string, error) {
	var user strings.Builder
	writeBriefGrounding(&user, proj)
	fmt.Fprintf(&user, "## Target swarm\n\nGenerate a SWARM.md for a swarm with id `%s` that delivers the project's brief.\n\n", swarmID)
	user.WriteString("## Output contract\n\nRespond with a single JSON object: `{\"swarm_md\": \"<full SWARM.md file content>\"}`.\n")
	user.WriteString("The SWARM.md must start with `---`, contain a `swarmId:` matching the target id, a `leadRole:`, and a `roles:` array. Every role gets a `name`, `description`, `systemPrompt`, and `runtime: {image: \"vornik-agent:latest\"}`. No other top-level keys.\n")

	system := wizardSystemPromptSwarm()
	result, err := s.assistantLLM.Complete(ctx, model, system, user.String())
	if err != nil {
		return "", fmt.Errorf("swarm generation LLM call failed: %w", err)
	}
	if result == nil || strings.TrimSpace(result.Text) == "" {
		return "", fmt.Errorf("swarm generation returned empty body")
	}

	var env wizardSwarmEnvelope
	if err := json.Unmarshal([]byte(result.Text), &env); err != nil {
		return "", fmt.Errorf("swarm generation: LLM response is not valid JSON: %w", err)
	}
	if env.SwarmMD == "" {
		return "", fmt.Errorf("swarm generation: empty swarm_md in response")
	}

	parsed, err := registry.ParseSwarmMarkdown([]byte(env.SwarmMD), swarmID+".md")
	if err != nil {
		return "", fmt.Errorf("swarm generation: generated SWARM.md does not parse: %v", err)
	}
	if parsed.ID != swarmID {
		return "", fmt.Errorf("swarm generation: generated swarmId %q does not match target %q", parsed.ID, swarmID)
	}
	if err := parsed.Validate(swarmID + ".md"); err != nil {
		return "", fmt.Errorf("swarm generation: generated SWARM.md fails validation: %v", err)
	}
	s.recordAssistantUsage(ctx, proj, model, result)
	return env.SwarmMD, nil
}

// wizardGenerateWorkflow is symmetric to wizardGenerateSwarm,
// but its user prompt also carries the generated swarm so the
// workflow's `role:` fields land on actual role names.
func (s *Server) wizardGenerateWorkflow(ctx context.Context, proj *registry.Project, model, wfID, swarmMD string) (string, error) {
	var user strings.Builder
	writeBriefGrounding(&user, proj)
	fmt.Fprintf(&user, "## Generated swarm (use these role names for `role:` fields)\n\n```\n%s\n```\n\n", swarmMD)
	fmt.Fprintf(&user, "## Target workflow\n\nGenerate a WORKFLOW.md with id `%s` that orchestrates the swarm above.\n\n", wfID)
	user.WriteString("## Output contract\n\nRespond with a single JSON object: `{\"workflow_md\": \"<full WORKFLOW.md file content>\"}`.\n")
	user.WriteString("The WORKFLOW.md must start with `---`, contain `workflowId:`, `entrypoint:`, a `steps:` map (each step has `type: agent`, `role:` matching a role in the generated swarm, `prompt:`, and `on_success:`), and a `terminals:` map with at least one terminal (`status: COMPLETED`).\n")

	system := wizardSystemPromptWorkflow()
	result, err := s.assistantLLM.Complete(ctx, model, system, user.String())
	if err != nil {
		return "", fmt.Errorf("workflow generation LLM call failed: %w", err)
	}
	if result == nil || strings.TrimSpace(result.Text) == "" {
		return "", fmt.Errorf("workflow generation returned empty body")
	}

	var env wizardWorkflowEnvelope
	if err := json.Unmarshal([]byte(result.Text), &env); err != nil {
		return "", fmt.Errorf("workflow generation: LLM response is not valid JSON: %w", err)
	}
	if env.WorkflowMD == "" {
		return "", fmt.Errorf("workflow generation: empty workflow_md in response")
	}

	parsed, err := registry.ParseWorkflowMarkdown([]byte(env.WorkflowMD), wfID+".md")
	if err != nil {
		return "", fmt.Errorf("workflow generation: generated WORKFLOW.md does not parse: %v", err)
	}
	if parsed.ID != wfID {
		return "", fmt.Errorf("workflow generation: generated workflowId %q does not match target %q", parsed.ID, wfID)
	}
	if err := parsed.Validate(wfID + ".md"); err != nil {
		return "", fmt.Errorf("workflow generation: generated WORKFLOW.md fails validation: %v", err)
	}
	s.recordAssistantUsage(ctx, proj, model, result)
	return env.WorkflowMD, nil
}

func wizardSystemPromptSwarm() string {
	return `You are a swarm architect. The user's brief describes a project; produce a SWARM.md that delivers it.

Rules:
- Two to four roles is the sweet spot for most projects. Default to a "lead" that plans + delegates, plus 1-3 specialists.
- Every role's systemPrompt should be a concise, specific prompt body — no preamble, no headings, just the role's instructions.
- Ground every role's systemPrompt in the brief: reference the audience, the success criteria, and the out-of-scope items.
- Output strictly the JSON envelope the user specifies. No prose around it.`
}

func wizardSystemPromptWorkflow() string {
	return `You are a workflow architect. The user's brief + the swarm definition describe a delivery shape; produce a WORKFLOW.md that orchestrates it.

Rules:
- One step per role at minimum. A reviewer + retry loop is welcome when the brief mentions verification.
- Every step.role MUST match a role name in the generated swarm.
- Every step needs a non-empty prompt body.
- Output strictly the JSON envelope the user specifies. No prose around it.`
}

func writeWizardJSON(w http.ResponseWriter, status int, body wizardResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
