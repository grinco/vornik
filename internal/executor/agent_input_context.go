package executor

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/promptblock"
)

// extractAgentPayloadContext parses task.Payload into the pieces the agent
// input builder needs: the task type, the user prompt (context.prompt, falling
// back to taskType for the API path where the prompt is stored as taskType),
// and the attached input-file list + extractions. Extracted from
// buildAgentInput (behaviour-preserving). A non-empty but unparseable payload
// yields a WARNING user prompt so the agent + tool audit see it rather than a
// silent generic prompt.
func extractAgentPayloadContext(task *persistence.Task) (taskType, userPrompt string, inputFiles []string, inputExtractions []map[string]any) {
	taskType = "test-task"
	if len(task.Payload) == 0 {
		return taskType, userPrompt, inputFiles, inputExtractions
	}
	var payload map[string]any
	if err := json.Unmarshal(task.Payload, &payload); err != nil {
		userPrompt = fmt.Sprintf("[WARNING: task payload could not be parsed (%v) — task has no context]", err)
		return taskType, userPrompt, inputFiles, inputExtractions
	}
	if v, ok := payload["taskType"].(string); ok && v != "" {
		taskType = v
	}
	if ctx, ok := payload["context"].(map[string]any); ok {
		if v, ok := ctx["prompt"].(string); ok && v != "" {
			userPrompt = v
		}
		// inputFiles / inputExtractions: tolerant of any []any shape JSON
		// unmarshal produces; non-conforming entries are skipped.
		if raw, ok := ctx["inputFiles"].([]any); ok {
			for _, v := range raw {
				if s, ok := v.(string); ok && s != "" {
					inputFiles = append(inputFiles, s)
				}
			}
		}
		if raw, ok := ctx["inputExtractions"].([]any); ok {
			for _, v := range raw {
				if m, ok := v.(map[string]any); ok {
					inputExtractions = append(inputExtractions, m)
				}
			}
		}
	}
	if userPrompt == "" && taskType != "" && taskType != "test-task" {
		userPrompt = taskType
	}
	return taskType, userPrompt, inputFiles, inputExtractions
}

// assembleAgentPrompt builds the final prompt string the model reads, plus the
// time context (also surfaced structurally in the context map). Extracted from
// buildAgentInput (behaviour-preserving).
//
// stepPromptArg is the workflow step prompt; opts.StepPrompt overrides it (e.g.
// with gate-format instructions appended). For adaptive roles (PreviousResult
// set) the role instructions lead and the user task is framed as reference —
// role identity must win, or the first plan role follows the task verbatim
// instead of its own role. Non-adaptive single-step keeps the legacy ordering
// (step instructions, then task). The time line, attached-files block, and the
// recent-activity / watchlist / recovery reference blocks are appended AFTER
// the main prompt so the role's own instructions read first.
func assembleAgentPrompt(task *persistence.Task, stepPromptArg string, opts *agentInputOpts, userPrompt string, inputFiles []string, inputExtractions []map[string]any) (string, currentDateTimeContext) {
	stepPrompt := stepPromptArg
	// Rewrite known host paths to the canonical container path so the agent
	// reaches the staged file regardless of what the dispatcher LLM embedded.
	if len(inputFiles) > 0 && userPrompt != "" {
		userPrompt = rewriteInputPathsInPrompt(userPrompt, inputFiles)
	}
	if opts != nil && opts.StepPrompt != "" {
		stepPrompt = opts.StepPrompt
	}

	isAdaptiveRole := opts != nil && opts.PreviousResult != ""
	var prompt string
	switch {
	case stepPrompt != "" && userPrompt != "":
		if isAdaptiveRole {
			prompt = stepPrompt + "\n\n" +
				"--- Original task (for reference; follow your role instructions above, not this text verbatim) ---\n" +
				userPrompt
		} else {
			prompt = stepPrompt + "\n\n--- Task ---\n" + userPrompt
		}
	case userPrompt != "":
		prompt = userPrompt
	case stepPrompt != "":
		prompt = stepPrompt
	default:
		prompt = "Process task " + task.ID
	}

	timeContext := buildCurrentDateTimeContext("")
	if opts != nil {
		timeContext = buildCurrentDateTimeContext(opts.ProjectTimezone)
	}
	prompt = timeContext.PromptLine + "\n\n" + prompt

	// Authoritative ATTACHED FILES block — always wins over any path the
	// dispatcher LLM put in the user prompt. The staged index comes from the
	// artifacts the executor actually staged (stageInputArtifacts has already
	// rewritten each entry's "path" to the container path, and it runs BEFORE
	// task.json is written), so the prompt cannot claim a file is unavailable
	// when it is sitting in the workspace — the customer-reported "facts
	// document exists but was never analyzed" failure (2026-08-03).
	if len(inputFiles) > 0 {
		prompt += "\n\n" + buildAttachedFilesBlockStaged(inputFiles, inputExtractions, stagedArtifactIndex(opts))
	}
	if opts == nil {
		return prompt, timeContext
	}
	// Reference blocks (data, not directives) — appended after the main prompt.
	if opts.RecentActivityBlock != "" {
		prompt += "\n\n## RECENT_ACTIVITY_24H\n" + opts.RecentActivityBlock
	}
	if opts.WatchlistQuotesBlock != "" {
		prompt += "\n\n## WATCHLIST_QUOTES\n" + opts.WatchlistQuotesBlock
	}
	if opts.WatchlistIndicatorsBlock != "" {
		prompt += "\n\n## WATCHLIST_INDICATORS\n" + opts.WatchlistIndicatorsBlock
	}
	// Recovery context: render the structured failure signal into the prompt
	// the model reads (the structured copy still lands under context.recovery
	// for programmatic consumers). See https://docs.vornik.io §2.
	if opts.RecoveryContext != nil {
		prompt += "\n\n" + buildRecoveryContextBlock(opts.RecoveryContext)
	}
	return prompt, timeContext
}

// stagedArtifactIndex maps basename → container path for the artifacts staged
// into this step's workspace. Nil-safe: a nil opts (or no artifacts) yields nil,
// and the block then falls back to the conventional artifacts/in/ location.
func stagedArtifactIndex(opts *agentInputOpts) map[string]string {
	if opts == nil || len(opts.InputArtifacts) == 0 {
		return nil
	}
	out := make(map[string]string, len(opts.InputArtifacts))
	for _, art := range opts.InputArtifacts {
		name := art["name"]
		path := art["path"]
		if name == "" || path == "" {
			continue
		}
		out[filepath.Base(name)] = path
	}
	return out
}

// suppressesGuidanceBlock reports whether the owning swarm switched this
// guidance block off.
//
// Mirrors registry.Swarm.SuppressesGuidanceBlock rather than calling it,
// because by this point the executor holds the list and not the swarm. Both
// refuse to suppress an INVARIANT block: config validation rejects such a list
// at load, and this is the second lock — "the rule runs whatever the prompt
// says" should not depend on which door the value came through.
func (o *agentInputOpts) suppressesGuidanceBlock(name string) bool {
	if o == nil || !promptblock.Suppressible(name) {
		return false
	}
	for _, listed := range o.SuppressedGuidanceBlocks {
		if strings.TrimSpace(listed) == name {
			return true
		}
	}
	return false
}

// buildAgentContextMap assembles the context.* block of the agent input.
// Extracted from buildAgentInput (behaviour-preserving). Empty optional fields
// are skipped so a project without a given convention doesn't see noisy
// task.json keys.
func buildAgentContextMap(taskType, prompt string, timeContext currentDateTimeContext, opts *agentInputOpts) map[string]any {
	contextMap := map[string]any{
		"taskType":        taskType,
		"prompt":          prompt,
		"currentDateTime": timeContext,
	}
	if opts == nil {
		return contextMap
	}
	if len(opts.InputArtifacts) > 0 {
		contextMap["inputArtifacts"] = opts.InputArtifacts
	}
	// inputArtifactsSummary rides alongside inputArtifacts only when a
	// stage_child_artifacts step gathered its delegated children on resume
	// (nil otherwise). Lets the consuming prompt surface missing/empty
	// children instead of silently proceeding (design §3.4).
	if opts.InputArtifactsSummary != nil {
		contextMap["inputArtifactsSummary"] = opts.InputArtifactsSummary
	}
	if opts.PreviousResult != "" {
		contextMap["previousStepResult"] = opts.PreviousResult
	}
	// Append canonical-context guidance whenever the pre-load populated
	// something, so the agent reads context.projectContext / userGuidance
	// before walking the workspace (LLD §3.2).
	if opts.SystemPrompt != "" || !opts.CanonicalContext.Empty() || len(opts.Skills) > 0 {
		// An operator may switch off ADVISORY blocks for their swarm (LLD 09
		// §13.3.1). suppressed() answers false for the invariant block whatever
		// the config says — config validation refuses such a list, and this is
		// the second lock on the same rule.
		suppressed := opts.suppressesGuidanceBlock
		sp := opts.SystemPrompt
		if !suppressed(promptblock.CanonicalContext) {
			sp = composeSystemPromptWithCanonicalContext(sp, opts.CanonicalContext)
		}
		// Learned skills are operator-approved, so they ride the trusted
		// directive channel (system prompt), appended after canonical
		// context (LLD 2026-07-07-knowledge-skill-store-design §4). Not a
		// daemon-authored block and so not suppressible here: an operator who
		// wants fewer skills manages the skill store.
		sp = composeSystemPromptWithSkillIndex(sp, opts.Skills)
		// Tool-budget guidance rides the binary, not the swarm preset, so an upgrade
		// reaches every existing deployment's agents (see tool_grant_prompt.go).
		if !suppressed(promptblock.ToolBudget) {
			sp = composeSystemPromptWithToolGrant(sp, opts.ToolGrantAvailable)
		}
		// Reporting integrity states an invariant every deployment enforces
		// (verifyRoleClaims). Inside this guard, NOT above it: when nothing else
		// composes a system prompt the entrypoint applies its own default, and
		// emitting a bare block here would REPLACE that default with three
		// sentences — a worse outcome than the gap it closes.
		//
		// Unconditional, and deliberately not routed through suppressed(): the
		// gate runs on every step of every deployment, so switching the block off
		// would remove the warning and not the rule.
		sp = composeSystemPromptWithClaimVerification(sp)
		// Workspace-git states a runtime fact: when the project is a worktree
		// its .git is mounted read-only, so git writes cannot land. Gated on
		// the condition rather than suppressed(), for the same reason as
		// reporting integrity — switching it off would remove the explanation
		// and leave the failure. Gated at all because, unlike that block, this
		// one is FALSE on a deployment that mounts no worktree.
		sp = composeSystemPromptWithWorkspaceGit(sp, opts.WorktreeGitReadOnly)
		contextMap["systemPrompt"] = sp
	}
	// Adaptive candidate list — the lead picks a value from this slice
	// verbatim; the executor validates the choice post-run.
	if len(opts.AdaptiveCandidateWorkflows) > 0 {
		contextMap["adaptiveCandidateWorkflows"] = opts.AdaptiveCandidateWorkflows
	}
	// Recovery shape: a prior step failed and the workflow routed here to
	// propose alternatives via a decision checkpoint.
	if opts.RecoveryContext != nil {
		contextMap["recovery"] = opts.RecoveryContext
	}
	// Canonical context — pre-loaded PROJECT_CONTEXT.md + USER_GUIDANCE.md
	// (context-discovery hardening LLD, Layer 1).
	if opts.CanonicalContext.ProjectContext != "" {
		contextMap["projectContext"] = opts.CanonicalContext.ProjectContext
	}
	if opts.CanonicalContext.UserGuidance != "" {
		contextMap["userGuidance"] = opts.CanonicalContext.UserGuidance
	}
	if opts.CanonicalContext.Source != "" {
		contextMap["projectContextSource"] = opts.CanonicalContext.Source
	}
	if len(opts.CanonicalContext.Truncated) > 0 {
		contextMap["projectContextTruncated"] = opts.CanonicalContext.Truncated
	}
	return contextMap
}
