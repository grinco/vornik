package executor

import (
	"encoding/json"
	"fmt"

	"vornik.io/vornik/internal/persistence"
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
	// dispatcher LLM put in the user prompt.
	if len(inputFiles) > 0 {
		prompt += "\n\n" + buildAttachedFilesBlock(inputFiles, inputExtractions)
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
	if opts.PreviousResult != "" {
		contextMap["previousStepResult"] = opts.PreviousResult
	}
	// Append canonical-context guidance whenever the pre-load populated
	// something, so the agent reads context.projectContext / userGuidance
	// before walking the workspace (LLD §3.2).
	if opts.SystemPrompt != "" || !opts.CanonicalContext.Empty() {
		contextMap["systemPrompt"] = composeSystemPromptWithCanonicalContext(opts.SystemPrompt, opts.CanonicalContext)
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
