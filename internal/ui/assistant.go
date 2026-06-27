package ui

// LLM prompt-writing assistant (Phase 2 v1) — backs the
// "AI Assist" affordance attached to per-role / per-step prompt
// textareas in the swarm and workflow editors. See
// https://docs.vornik.io
//
// v1 surface:
//   - POST /assistant/draft handler returns a JSON suggestion.
//   - Actions: "draft" (compose from scratch) and "critique"
//     (advisory feedback on the current value).
//   - Attach points: swarm role systemPrompts + workflow agent
//     step prompts.
//   - Grounding: the project's PROJECT.md brief (Goal, Audience,
//     Success criteria, Out of scope, Risk & cadence) plus the
//     sibling roster (other role names + descriptions in the
//     swarm; other step ids + types + transitions in the
//     workflow) plus the current value of the field being
//     edited.
//   - Model resolution: swarm leadRole.model > daemon default.
//     A per-project assistant.model override is in the LLD but
//     not wired in v1.
//
// Deferred to v2 (per LLD open questions):
//   - Budget guard on assistant spend.
//   - Per-call usage logging into task_llm_usage.
//   - Sibling-prompt-body visibility (only names / descriptions
//     today; full bodies inflate context for large swarms).
//   - "Tighten" / "Expand" actions; "Check conflicts" mode.
//   - Brief and project-config field attach points.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/budget"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/pricing"
	"vornik.io/vornik/internal/registry"
)

// TaskLLMUsageSourceAuthoring identifies usage rows the
// authoring assistant writes. Surfaces in the spend dashboard
// alongside workflow_step + dispatcher rows so operators can
// see authoring cost without conflating it with task work.
const TaskLLMUsageSourceAuthoring = "_authoring"

// AssistantLLM is the chat-completion contract the assistant
// handler depends on. Implementations live outside the ui
// package — the production adapter wraps chat.Client; tests use
// a small mock. Keeping it narrow lets the handler stay
// model-agnostic.
type AssistantLLM interface {
	// Complete sends a system + user pair to the named model
	// and returns the assistant's reply text plus the prompt /
	// completion token counts so the handler can attribute the
	// call to the project's budget. Errors propagate to the
	// JSON response so the front-end can surface them.
	Complete(ctx context.Context, model, system, user string) (*AssistantResult, error)
}

// AssistantResult is what an AssistantLLM returns on success.
// Token counts feed the budget guard + spend logging; Text is
// what lands in the operator's textarea after Apply.
type AssistantResult struct {
	Text             string
	Model            string
	PromptTokens     int
	CompletionTokens int
	// CacheCreationTokens / CacheReadTokens propagate provider-
	// native prompt-prefix cache accounting (Bedrock / Anthropic)
	// so the `_authoring` source on /ui/spend shows the same
	// hit-ratio + savings columns the dispatcher and executor
	// surfaces already do. Zero on providers that don't emit
	// cache fields.
	CacheCreationTokens int
	CacheReadTokens     int
}

// assistantResponse is the JSON envelope the JS panel reads.
// Suggestion holds the LLM output on success; Error is set on
// failure (with a non-OK HTTP status alongside). Model is
// echoed back so operators see which model produced the text.
type assistantResponse struct {
	Suggestion string `json:"suggestion"`
	Model      string `json:"model"`
	Error      string `json:"error,omitempty"`
}

// Supported actions. Unknown actions are rejected with 400 so a
// typo in the front-end's POST never reaches the LLM.
const (
	assistantActionDraft     = "draft"
	assistantActionCritique  = "critique"
	assistantActionTighten   = "tighten"
	assistantActionExpand    = "expand"
	assistantActionConflicts = "conflicts"
)

// Supported kinds.
const (
	assistantKindSwarmRole    = "swarm_role"
	assistantKindWorkflowStep = "workflow_step"
	// Phase 2 v2 kinds — brief sections and the project
	// config's prose fields (currently just autonomy.goal).
	assistantKindBriefSection = "brief_section"
	assistantKindProjectField = "project_field"
)

// briefSectionLookup maps the form's subjectId (the section
// heading) → a getter that returns that section's current
// body. Used both to validate the section name and to feed the
// LLM the current value for non-current-from-form requests.
var briefSectionGetters = map[string]func(*registry.ProjectBrief) string{
	"Goal":             func(b *registry.ProjectBrief) string { return b.Goal },
	"Audience":         func(b *registry.ProjectBrief) string { return b.Audience },
	"Success criteria": func(b *registry.ProjectBrief) string { return b.SuccessCriteria },
	"Out of scope":     func(b *registry.ProjectBrief) string { return b.OutOfScope },
	"Risk & cadence":   func(b *registry.ProjectBrief) string { return b.RiskCadence },
}

// projectFieldSubjects is the allowlist of project-config form
// fields the assistant accepts. Today only autonomy.goal; the
// list is a struct so future additions stay tightly scoped.
var projectFieldSubjects = map[string]struct{}{
	"autonomy.goal": {},
}

// AssistantSuggest handles POST /assistant/draft. Reads the
// form for context (kind, projectId, targetId, subjectId,
// action, currentValue), resolves the project + target +
// subject from the registry, builds the prompt, calls the LLM,
// and returns the suggestion as JSON.
func (s *Server) AssistantSuggest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeAssistantJSON(w, http.StatusMethodNotAllowed, assistantResponse{
			Error: "method not allowed",
		})
		return
	}
	if s.assistantLLM == nil {
		writeAssistantJSON(w, http.StatusServiceUnavailable, assistantResponse{
			Error: "assistant is not configured on this server",
		})
		return
	}
	if err := r.ParseForm(); err != nil {
		writeAssistantJSON(w, http.StatusBadRequest, assistantResponse{Error: "form parse: " + err.Error()})
		return
	}

	kind := r.FormValue("kind")
	projectID := r.FormValue("projectId")
	targetID := r.FormValue("targetId")
	subjectID := r.FormValue("subjectId")
	action := r.FormValue("action")
	currentValue := r.FormValue("currentValue")

	switch action {
	case assistantActionDraft, assistantActionCritique, assistantActionTighten, assistantActionExpand, assistantActionConflicts:
		// ok
	default:
		writeAssistantJSON(w, http.StatusBadRequest, assistantResponse{
			Error: fmt.Sprintf("unsupported action %q", action),
		})
		return
	}

	if s.projectReg == nil {
		writeAssistantJSON(w, http.StatusInternalServerError, assistantResponse{Error: "project registry not configured"})
		return
	}
	proj := s.projectReg.GetProject(projectID)
	// Project-scope guard. A request scoped to other projects must
	// not be able to ground the assistant on this project's private
	// brief / sibling prompts, nor charge its budget. Return the SAME
	// 404 as the missing-project case so a scope failure is
	// indistinguishable from a non-existent project (no existence
	// leak). Mirrors the sibling-handler convention; reuses the
	// api.RequestAllowsProject primitive (auth-disabled installs are
	// single-tenant and pass through unchanged). Blocks before
	// buildAssistantPrompt / budget / LLM.
	if proj == nil || !api.RequestAllowsProject(r, projectID) {
		writeAssistantJSON(w, http.StatusNotFound, assistantResponse{Error: "project not found"})
		return
	}

	system, user, model, err := s.buildAssistantPrompt(kind, proj, targetID, subjectID, action, currentValue)
	if err != nil {
		writeAssistantJSON(w, http.StatusBadRequest, assistantResponse{Error: err.Error()})
		return
	}

	// Budget guard. The assistant call counts against the same
	// per-project daily / monthly caps as task work, so a
	// project at its hard cap can't bypass via the authoring
	// surface. When no usage repo is wired (minimal test
	// deployments), budget.Check no-ops on a nil Repo.
	if blocked, reason := s.assistantBudgetBlocked(r.Context(), proj); blocked {
		writeAssistantJSON(w, http.StatusTooManyRequests, assistantResponse{
			Error: "budget exceeded: " + reason,
		})
		return
	}

	result, err := s.assistantLLM.Complete(r.Context(), model, system, user)
	if err != nil {
		writeAssistantJSON(w, http.StatusBadGateway, assistantResponse{
			Model: model,
			Error: err.Error(),
		})
		return
	}
	if result == nil {
		writeAssistantJSON(w, http.StatusBadGateway, assistantResponse{
			Model: model,
			Error: "assistant returned no result",
		})
		return
	}

	// Record usage so authoring spend lands on the spend
	// dashboard and feeds the budget guard's next check. Best-
	// effort: failure to record is logged but doesn't break the
	// response the operator sees.
	effectiveModel := assistantResultModel(model, result)
	s.recordAssistantUsage(r.Context(), proj, effectiveModel, result)

	writeAssistantJSON(w, http.StatusOK, assistantResponse{
		Suggestion: result.Text,
		Model:      effectiveModel,
	})
}

func assistantResultModel(requested string, result *AssistantResult) string {
	if result != nil && strings.TrimSpace(result.Model) != "" {
		return strings.TrimSpace(result.Model)
	}
	return requested
}

func (s *Server) assistantBudgetBlocked(ctx context.Context, proj *registry.Project) (bool, string) {
	if s.llmUsageRepo == nil {
		return false, ""
	}
	decision, err := budget.Check(ctx, s.llmUsageRepo, proj, time.Now())
	if err != nil {
		return false, ""
	}
	return decision.Blocked, decision.Reason
}

// recordAssistantUsage writes a TaskLLMUsage row for the
// authoring call. Cost is computed via the pricing table when
// wired; without one, cost is zero (budget guard still trips
// on a busy project, just not on authoring-only spend).
func (s *Server) recordAssistantUsage(ctx context.Context, proj *registry.Project, model string, result *AssistantResult) {
	if s.llmUsageRepo == nil {
		return
	}
	cost := 0.0
	if s.assistantPricing != nil {
		cost = s.assistantPricing.CostUSDWithCache(model, result.PromptTokens, result.CompletionTokens, result.CacheCreationTokens, result.CacheReadTokens)
	}
	idBytes := make([]byte, 12)
	_, _ = rand.Read(idBytes)
	row := &persistence.TaskLLMUsage{
		ID:                  "auth_" + hex.EncodeToString(idBytes),
		ProjectID:           proj.ID,
		StepID:              "_authoring",
		Role:                "assistant",
		Model:               model,
		PromptTokens:        int64(result.PromptTokens),
		CompletionTokens:    int64(result.CompletionTokens),
		CacheCreationTokens: int64(result.CacheCreationTokens),
		CacheReadTokens:     int64(result.CacheReadTokens),
		Iterations:          1,
		CostUSD:             cost,
		Source:              TaskLLMUsageSourceAuthoring,
		RecordedAt:          time.Now().UTC(),
	}
	if err := s.llmUsageRepo.Record(ctx, row); err != nil && s.logger.GetLevel() <= 1 {
		s.logger.Warn().Err(err).Str("project_id", proj.ID).Msg("assistant usage record failed")
	}
}

// pricing.Table is opaque here — the daemon wiring decides
// whether to load and pass one.
var _ = pricing.Table{}

// buildAssistantPrompt resolves the target + subject from the
// registry, picks the model, and assembles the system + user
// messages. Returns an error when the target or subject
// doesn't resolve so the handler maps it to 400.
func (s *Server) buildAssistantPrompt(kind string, proj *registry.Project, targetID, subjectID, action, currentValue string) (system, user, model string, err error) {
	switch kind {
	case assistantKindSwarmRole:
		return s.buildSwarmRolePrompt(proj, targetID, subjectID, action, currentValue)
	case assistantKindWorkflowStep:
		return s.buildWorkflowStepPrompt(proj, targetID, subjectID, action, currentValue)
	case assistantKindBriefSection:
		return s.buildBriefSectionPrompt(proj, subjectID, action, currentValue)
	case assistantKindProjectField:
		return s.buildProjectFieldPrompt(proj, subjectID, action, currentValue)
	default:
		return "", "", "", fmt.Errorf("unsupported kind %q", kind)
	}
}

// buildBriefSectionPrompt grounds a brief-section assist call
// on the project's OTHER brief sections + the current value.
// targetID is the project id (already resolved by the caller),
// so this function only validates the subject and lays out the
// sibling sections in the user prompt.
func (s *Server) buildBriefSectionPrompt(proj *registry.Project, sectionName, action, currentValue string) (string, string, string, error) {
	if _, ok := briefSectionGetters[sectionName]; !ok {
		return "", "", "", fmt.Errorf("unknown brief section %q", sectionName)
	}

	var user strings.Builder
	fmt.Fprintf(&user, "# Project brief context\n\n")
	fmt.Fprintf(&user, "You are editing the `## %s` section of project `%s`'s brief.\n\n", sectionName, proj.ID)
	user.WriteString("## Other brief sections (for context)\n\n")
	if proj.Brief == nil {
		user.WriteString("(no brief exists yet — this section is being authored from scratch)\n\n")
	} else {
		// Render every section EXCEPT the one being edited so
		// the assistant can echo the audience back into the
		// goal without telling it to.
		for _, other := range []string{"Goal", "Audience", "Success criteria", "Out of scope", "Risk & cadence"} {
			if other == sectionName {
				continue
			}
			body := strings.TrimSpace(briefSectionGetters[other](proj.Brief))
			if body == "" {
				continue
			}
			fmt.Fprintf(&user, "### %s\n\n%s\n\n", other, body)
		}
	}
	user.WriteString("## Current section value\n\n")
	if strings.TrimSpace(currentValue) == "" {
		user.WriteString("(empty)\n")
	} else {
		user.WriteString("```\n")
		user.WriteString(currentValue)
		user.WriteString("\n```\n")
	}

	// Briefs have no embedded model but the project may have a
	// per-project override (and a swarm with a leadRole.model).
	var sw *registry.Swarm
	if proj.SwarmID != "" && s.projectReg != nil {
		sw = s.projectReg.GetSwarm(proj.SwarmID)
	}
	model := resolveAssistantModelForProject(proj, sw, s.assistantDefaultModel)
	return assistantSystemPrompt(action, fmt.Sprintf("brief section `## %s`", sectionName)), user.String(), model, nil
}

// buildProjectFieldPrompt grounds a project-config-form-field
// assist call. v1 supports autonomy.goal — the highest-leverage
// prose field on the project page. Resolves model through the
// project's swarm leadRole (when set) for parity with
// swarm_role.
func (s *Server) buildProjectFieldPrompt(proj *registry.Project, subjectID, action, currentValue string) (string, string, string, error) {
	if _, ok := projectFieldSubjects[subjectID]; !ok {
		return "", "", "", fmt.Errorf("unknown project field %q", subjectID)
	}

	var user strings.Builder
	writeBriefGrounding(&user, proj)
	fmt.Fprintf(&user, "## Project field context\n\n")
	switch subjectID {
	case "autonomy.goal":
		user.WriteString("You are editing the `autonomy.goal` field of the project's YAML config.\n")
		user.WriteString("This text grounds the autonomy loop — every tick the lead reads it alongside current project state and decides whether to schedule a new task.\n")
	}
	user.WriteString("\n## Current value\n\n")
	if strings.TrimSpace(currentValue) == "" {
		user.WriteString("(empty)\n")
	} else {
		user.WriteString("```\n")
		user.WriteString(currentValue)
		user.WriteString("\n```\n")
	}

	// Model resolution mirrors swarm_role: project override
	// first, then swarm leadRole.model, then daemon default.
	var sw *registry.Swarm
	if proj.SwarmID != "" && s.projectReg != nil {
		sw = s.projectReg.GetSwarm(proj.SwarmID)
	}
	model := resolveAssistantModelForProject(proj, sw, s.assistantDefaultModel)
	return assistantSystemPrompt(action, "project autonomy goal"), user.String(), model, nil
}

// buildSwarmRolePrompt grounds a role-prompt assist call on the
// project brief + the swarm's sibling roles + the current value.
func (s *Server) buildSwarmRolePrompt(proj *registry.Project, swarmID, roleName, action, currentValue string) (string, string, string, error) {
	sw := s.projectReg.GetSwarm(swarmID)
	if sw == nil {
		return "", "", "", fmt.Errorf("swarm %q not found", swarmID)
	}
	var target *registry.SwarmRole
	siblings := []registry.SwarmRole{}
	for i := range sw.Roles {
		r := &sw.Roles[i]
		if r.Name == roleName {
			target = r
			continue
		}
		siblings = append(siblings, *r)
	}
	if target == nil {
		return "", "", "", fmt.Errorf("role %q not found in swarm %q", roleName, swarmID)
	}

	model := resolveAssistantModelForProject(proj, sw, s.assistantDefaultModel)

	var user strings.Builder
	writeBriefGrounding(&user, proj)
	fmt.Fprintf(&user, "## Swarm role context\n\n")
	fmt.Fprintf(&user, "You are editing the system prompt for role `%s` in swarm `%s`.\n", target.Name, sw.ID)
	if target.Description != "" {
		fmt.Fprintf(&user, "Role description: %s\n", target.Description)
	}
	if target.Model != "" {
		fmt.Fprintf(&user, "Role model: %s\n", target.Model)
	}
	if len(target.Permissions.AllowedTools) > 0 {
		fmt.Fprintf(&user, "Role allowed tools: %s\n", strings.Join(target.Permissions.AllowedTools, ", "))
	}
	user.WriteString("\n## Sibling roles in this swarm\n\n")
	if len(siblings) == 0 {
		user.WriteString("(none)\n")
	} else {
		// conflicts mode wants the FULL sibling bodies so the
		// LLM can spot contradictions. Other modes get just
		// names + descriptions to keep the context window
		// manageable on large swarms.
		fullBodies := action == assistantActionConflicts
		for _, r := range siblings {
			if fullBodies && strings.TrimSpace(r.SystemPrompt) != "" {
				fmt.Fprintf(&user, "### `%s` — %s\n\n```\n%s\n```\n\n", r.Name, fallbackString(r.Description, "(no description)"), r.SystemPrompt)
			} else {
				fmt.Fprintf(&user, "- `%s`: %s\n", r.Name, fallbackString(r.Description, "(no description)"))
			}
		}
	}
	user.WriteString("\n## Current role prompt\n\n")
	if strings.TrimSpace(currentValue) == "" {
		user.WriteString("(empty)\n")
	} else {
		user.WriteString("```\n")
		user.WriteString(currentValue)
		user.WriteString("\n```\n")
	}

	return assistantSystemPrompt(action, "role system prompt"), user.String(), model, nil
}

// buildWorkflowStepPrompt grounds a step-prompt assist call on
// the project brief + the workflow's sibling steps + transitions
// + the current value.
func (s *Server) buildWorkflowStepPrompt(proj *registry.Project, workflowID, stepID, action, currentValue string) (string, string, string, error) {
	wf := s.projectReg.GetWorkflow(workflowID)
	if wf == nil {
		return "", "", "", fmt.Errorf("workflow %q not found", workflowID)
	}
	target, ok := wf.Steps[stepID]
	if !ok {
		return "", "", "", fmt.Errorf("step %q not found in workflow %q", stepID, workflowID)
	}

	// Workflows have no embedded model picker, but the project's
	// active swarm leadRole does — resolve through that path so
	// workflow_step assists use the same model as swarm_role.
	var sw *registry.Swarm
	if proj.SwarmID != "" && s.projectReg != nil {
		sw = s.projectReg.GetSwarm(proj.SwarmID)
	}
	model := resolveAssistantModelForProject(proj, sw, s.assistantDefaultModel)

	var user strings.Builder
	writeBriefGrounding(&user, proj)
	fmt.Fprintf(&user, "## Workflow step context\n\n")
	fmt.Fprintf(&user, "You are editing the prompt for step `%s` in workflow `%s` (entrypoint: `%s`).\n", stepID, wf.ID, wf.Entrypoint)
	fmt.Fprintf(&user, "Step type: %s\n", fallbackString(target.Type, "(unset)"))
	if target.Role != "" {
		fmt.Fprintf(&user, "Step role: %s\n", target.Role)
	}
	if target.OnSuccess != "" {
		fmt.Fprintf(&user, "On success → %s\n", target.OnSuccess)
	}
	if target.OnFail != "" {
		fmt.Fprintf(&user, "On fail → %s\n", target.OnFail)
	}
	user.WriteString("\n## Sibling steps in this workflow\n\n")
	siblings := []string{}
	for id := range wf.Steps {
		if id == stepID {
			continue
		}
		siblings = append(siblings, id)
	}
	if len(siblings) == 0 {
		user.WriteString("(none)\n")
	} else {
		for _, id := range sortStringsLocal(siblings) {
			st := wf.Steps[id]
			fmt.Fprintf(&user, "- `%s` (type=%s%s%s)\n", id,
				fallbackString(st.Type, "?"),
				roleSuffix(st.Role),
				onSuccessSuffix(st.OnSuccess),
			)
		}
	}
	user.WriteString("\n## Current step prompt\n\n")
	if strings.TrimSpace(currentValue) == "" {
		user.WriteString("(empty)\n")
	} else {
		user.WriteString("```\n")
		user.WriteString(currentValue)
		user.WriteString("\n```\n")
	}

	return assistantSystemPrompt(action, "agent step prompt"), user.String(), model, nil
}

// writeBriefGrounding writes the project brief sections to the
// user buffer when a brief exists. No-op when the project has
// no PROJECT.md attached — the assistant still has the sibling
// roster + current value to work with.
func writeBriefGrounding(buf *strings.Builder, proj *registry.Project) {
	buf.WriteString("# Project brief\n\n")
	if proj.Brief == nil {
		buf.WriteString("(no PROJECT.md attached to this project — work from sibling context only)\n\n")
		return
	}
	b := proj.Brief
	writeBriefSection(buf, "Goal", b.Goal)
	writeBriefSection(buf, "Audience", b.Audience)
	writeBriefSection(buf, "Success criteria", b.SuccessCriteria)
	writeBriefSection(buf, "Out of scope", b.OutOfScope)
	writeBriefSection(buf, "Risk & cadence", b.RiskCadence)
	buf.WriteString("\n")
}

func writeBriefSection(buf *strings.Builder, heading, body string) {
	body = strings.TrimSpace(body)
	if body == "" {
		return
	}
	fmt.Fprintf(buf, "## %s\n\n%s\n\n", heading, body)
}

// resolveAssistantModel picks the model to call the assistant
// with, considering only swarm-level overrides. Used by the
// model-resolution unit tests that don't have a project handy.
//
// Precedence:
//  1. Swarm leadRole's model — the project's "best" model in
//     practice, what the lead is already using.
//  2. Daemon default model — falls back so the assistant still
//     works on projects whose swarm doesn't override the model.
func resolveAssistantModel(sw *registry.Swarm, daemonDefault string) string {
	if sw == nil {
		return daemonDefault
	}
	for _, r := range sw.Roles {
		if r.Name == sw.LeadRole && r.Model != "" {
			return r.Model
		}
	}
	return daemonDefault
}

// resolveAssistantModelForProject extends resolveAssistantModel
// with a project-level override step at the top of the chain.
// Phase 2 v2 added project.assistant.model so operators can pin
// a specific authoring model independently of their worker
// swarm. The previous precedence (swarm leadRole → daemon
// default) still applies as the fallback.
func resolveAssistantModelForProject(proj *registry.Project, sw *registry.Swarm, daemonDefault string) string {
	if proj != nil && strings.TrimSpace(proj.Assistant.Model) != "" {
		return proj.Assistant.Model
	}
	return resolveAssistantModel(sw, daemonDefault)
}

// assistantSystemPrompt produces the system message for the
// given action + edit-surface kind label. Plain English so a
// future LLM swap doesn't have provider-specific tags to
// untangle.
func assistantSystemPrompt(action, surface string) string {
	switch action {
	case assistantActionCritique:
		return fmt.Sprintf(`You are a prompt-engineering coach helping an operator review a %s.

Read the project brief, the sibling roster, and the current draft. Then write a SHORT critique of the current draft. Use bullet points. Focus on:

- Vague or missing acceptance criteria.
- Hidden assumptions an LLM agent would mishandle.
- Conflicts with the brief's "out of scope" or "success criteria".
- Where the prompt could conflict with sibling prompts.

Do NOT write a new prompt — the operator wants feedback, not a rewrite. Be specific, actionable, and brief.`, surface)
	case assistantActionTighten:
		return fmt.Sprintf(`You are a prompt-engineering coach helping an operator tighten a %s.

Read the project brief, the sibling roster, and the current draft. Then rewrite the current draft as a TIGHTER version: same intent, fewer words, no redundant constraints, no filler phrasing. Constraints:

- Preserve every concrete requirement, acceptance criterion, and explicit tool / output-shape reference.
- Remove hedging, restated context, and rhetorical flourishes.
- Keep the result strictly shorter than the input.
- No preamble like "Here is the tighter version:" — emit only the prompt body itself.`, surface)
	case assistantActionConflicts:
		return fmt.Sprintf(`You are a prompt-engineering coach checking a %s for conflicts with its siblings.

The user prompt includes the FULL bodies of every sibling prompt (role / step / brief section, as applicable). Read them carefully and the current draft alongside, then report contradictions and overlaps. Use bullet points. Focus on:

- Direct contradictions: one prompt says "always X", another says "never X".
- Hidden assumptions: prompt A expects prompt B to produce a field that B doesn't actually emit.
- Duplicated work: two siblings doing the same job from different angles.
- Output-shape mismatches: A's caller expects {x, y}; A's prompt emits {x, z}.

Do NOT rewrite anything. Be specific — quote the offending lines verbatim when calling out a conflict.`, surface)
	case assistantActionExpand:
		return fmt.Sprintf(`You are a prompt-engineering coach helping an operator expand a %s.

Read the project brief, the sibling roster, and the current draft. Then rewrite the current draft as a more EXPLICIT version. Add the constraints a careful operator would add but the current draft elides. Constraints:

- Spell out acceptance criteria and output shape.
- Surface assumptions the current draft leaves implicit.
- Reference the brief's success criteria and out-of-scope items where relevant.
- Stay grounded — don't invent requirements the brief doesn't support.
- No preamble like "Here is the expanded version:" — emit only the prompt body itself.`, surface)
	default: // draft
		return fmt.Sprintf(`You are a prompt-engineering coach helping an operator write a %s.

Read the project brief, the sibling roster, and the current draft. Then write a fresh prompt body suitable for the role / step described. Constraints:

- Ground every instruction in the project brief's goal, audience, and success criteria.
- Avoid duplicating what sibling roles or steps already cover.
- Be specific about output shape and acceptance criteria.
- No preamble like "Here is the prompt:" — emit only the prompt body itself.`, surface)
	}
}

func fallbackString(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

func roleSuffix(role string) string {
	if role == "" {
		return ""
	}
	return ", role=" + role
}

func onSuccessSuffix(target string) string {
	if target == "" {
		return ""
	}
	return ", on_success=" + target
}

func writeAssistantJSON(w http.ResponseWriter, status int, body assistantResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
