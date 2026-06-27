package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/playbook"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/verifier"
)

var currentDateTimeNow = time.Now

type currentDateTimeContext struct {
	PromptLine string `json:"prompt_line"`
	Date       string `json:"date"`
	Time       string `json:"time"`
	Weekday    string `json:"weekday"`
	Timezone   string `json:"timezone"`
	RFC3339    string `json:"rfc3339"`
	UTC        string `json:"utc"`
}

func buildCurrentDateTimeContext(timezone string) currentDateTimeContext {
	loc := time.UTC
	tz := strings.TrimSpace(timezone)
	if tz != "" {
		if loaded, err := time.LoadLocation(tz); err == nil {
			loc = loaded
		} else {
			tz = ""
		}
	}
	if tz == "" {
		tz = "UTC"
	}
	nowUTC := currentDateTimeNow().UTC()
	local := nowUTC.In(loc)
	return currentDateTimeContext{
		PromptLine: fmt.Sprintf(
			"Current date/time context: today is %s, %s; current local time is %s %s (UTC: %s). Use these values for any \"today\", \"tomorrow\", \"yesterday\", or time-sensitive reasoning.",
			local.Format("Monday"),
			local.Format("January 2, 2006"),
			local.Format("15:04:05"),
			tz,
			nowUTC.Format(time.RFC3339),
		),
		Date:     local.Format("2006-01-02"),
		Time:     local.Format("15:04:05"),
		Weekday:  local.Format("Monday"),
		Timezone: tz,
		RFC3339:  local.Format(time.RFC3339),
		UTC:      nowUTC.Format(time.RFC3339),
	}
}

// executionPlan holds the resolved project, swarm, and workflow for an execution.
type executionPlan struct {
	project     *registry.Project
	swarm       *registry.Swarm
	workflow    *registry.Workflow
	worktreeDir string // isolated git worktree path; empty if worktrees are not enabled
	// stepOutputArtifacts carries the most recent step's harvested
	// outputs as {name, sourcePath} maps, where sourcePath is the
	// durable artifact-store StoragePath (which lives under an allowed
	// staging root). persistArtifacts writes it after each step; the
	// workflow loop reads it to bridge prior→next step (task e9a5).
	// The store path is the source of truth — NOT the agent's
	// container-relative path from result.json, which has no host
	// sourcePath and gets rejected by resolveStagingSrc. All steps of a
	// task share this one plan pointer, so this is the per-step handoff
	// channel.
	stepOutputArtifacts []map[string]string
}

// agentInputOpts carries optional context forwarded between workflow steps.
type agentInputOpts struct {
	// InputArtifacts lists files from the previous step's output.
	InputArtifacts []map[string]string
	// PreviousResult is the message from the preceding step's result.json.
	PreviousResult string
	// SystemPrompt overrides the default system prompt for this role.
	SystemPrompt string
	// StepPrompt overrides the workflow step prompt (used when gate
	// format instructions are appended).
	StepPrompt string
	// Permissions, when set, overrides the hardcoded permission defaults
	// in buildAgentInput with the actual swarm role config values.
	Permissions *registry.SwarmRolePermissions
	// AdaptiveCandidateWorkflows lists the workflow IDs the lead is
	// allowed to pick from when running the strict adaptive route
	// step. Injected into the agent's context.adaptiveCandidateWorkflows
	// so the prompt can refer to it. Empty/nil for non-adaptive
	// steps — the field is only set on the route step itself.
	AdaptiveCandidateWorkflows []string
	// ProjectTimezone is the project's configured IANA timezone for
	// agent-facing current date/time context. Empty or invalid falls
	// back to UTC.
	ProjectTimezone string
	// ResponseFormat mirrors the role-level swarm config field.
	// When non-empty, surfaced to the agent input as
	// config.responseFormat so the entrypoint can attach the
	// gateway-level response_format directive to its LLM request.
	// "json_object" is the only value the entrypoint currently
	// recognises; future shapes (json_schema, etc.) extend this
	// without breaking back-compat.
	ResponseFormat string
	// ResponseSchema, when non-nil, is the role's outputSchema
	// converted to standard JSON Schema. The agent runtime can pass
	// it to providers that support response_format: json_schema
	// (OpenAI, Bedrock Converse) or as a tool-call schema
	// (Anthropic). Surfaced at config.responseSchema in the agent
	// input so the runtime can pick it up additively without
	// breaking back-compat: a runtime that doesn't know what to do
	// with the field falls back to the existing responseFormat
	// behaviour. Item 7 of the deterministic-output-schema delivery
	// plan.
	ResponseSchema map[string]any
	// ResultEmissionTool, when non-nil, is the synthetic tool spec
	// the runtime can register so the LLM produces its result via
	// a tool call instead of a free-form JSON envelope. Tool-use
	// schema enforcement is the strongest portable guarantee —
	// every major provider validates tool-call args against the
	// parameters JSON Schema before returning the call. Surfaced at
	// config.resultEmissionTool. Item 9 of
	// https://docs.vornik.io
	ResultEmissionTool *registry.ToolSpec
	// ShapeRetryHint is the role-specific corrective text the
	// retry layer appends on shape-retry. Mirrors the
	// SwarmRole.ShapeRetryHint field; see retry.go for how it
	// composes with the generic prior-attempt anchor.
	ShapeRetryHint string
	// RecentActivityBlock, when non-empty, is appended to the
	// final prompt under a "## RECENT_ACTIVITY_24H" header.
	// Built upstream by buildRecentActivityBlock from the
	// trading_orders ledger so the strategist's next-tick
	// reasoning has structured fill/cancel/refusal context
	// without depending on an LLM-driven memory_search.
	RecentActivityBlock string
	// WatchlistQuotesBlock, when non-empty, is appended to the
	// final prompt under a "## WATCHLIST_QUOTES" header. Built
	// upstream by buildWatchlistQuotesBlock — pre-warms the
	// strategist's quote view in a single parallel fetch so
	// the role doesn't burn 16 sequential get_quote iterations
	// out of its tool budget on what's really a batch read.
	WatchlistQuotesBlock string
	// WatchlistIndicatorsBlock, when non-empty, is appended to
	// the final prompt under a "## WATCHLIST_INDICATORS" header.
	// Built upstream by buildWatchlistIndicatorsBlock — pre-warms
	// the strategist's indicator view in one parallel batch
	// (daily bars per symbol + in-process SMA/RSI/MACD math) so
	// the role doesn't burn ~80 sequential broker+TA tool calls
	// (16 symbols × 1 bars + 4 indicators) on what's really
	// deterministic arithmetic on a batch read.
	WatchlistIndicatorsBlock string
	// RecoveryContext, when non-nil, is the structured signal a
	// recovery step receives describing the prior step's failure.
	// Surfaced to the agent as context.recovery so the lead can
	// propose alternative approaches via a `decision` checkpoint.
	// nil for non-recovery steps. See
	// https://docs.vornik.io
	RecoveryContext *RecoveryContext

	// ComplexityTier carries the planner's complexity verdict to a
	// worker spawn so the executor can scale the role's tool-iteration
	// budget. Forwarded from executionState.ComplexityTier by the
	// workflow loop, mirroring RecoveryContext. Empty = standard. See
	// https://docs.vornik.io
	ComplexityTier string

	// CanonicalContext is the pre-loaded body of the project's
	// PROJECT_CONTEXT.md + USER_GUIDANCE.md (Layer 1 of the
	// context-discovery hardening LLD). Empty when the project
	// doesn't use the autonomy-context convention. Populated at
	// workspace-prep time so the agent finds it in task.json
	// instead of burning tool calls searching the workspace.
	CanonicalContext CanonicalContext
}

// RecoveryContext carries the failure shape the on_fail handler
// captured from the prior step so a downstream recovery step can
// propose alternatives. The shape generalizes beyond verifier
// blocks — broker rejections, pandoc errors, budget exhaustion,
// hallucination flags all populate different fields based on the
// FailureClass.
type RecoveryContext struct {
	// FailedStep is the workflow step ID whose execution prompted
	// the recovery. The lead's prompt uses it to anchor the
	// proposal ("the researcher step hit X").
	FailedStep string `json:"failed_step,omitempty"`
	// FailureClass tags the broad failure shape so the lead's
	// per-class playbook picks the right alternative template.
	// One of:
	//   "verifier_block"      — permanent fetch blocks (auth_required, captcha)
	//   "verifier_other"      — other verifier violations
	//   "agent_error"         — container exit / output schema mismatch / etc.
	//   "tool_error"          — a specific tool call failed
	//   "budget_exhausted"    — soft/hard cap hit mid-step
	//   "pandoc_error"        — format conversion failed
	//   "hallucination_flagged" — hallucination detector blocked the step
	// Future failure classes extend this list.
	FailureClass string `json:"failure_class,omitempty"`
	// FailureReason is the human-readable error string from the
	// failing step. Echoed into the lead's prompt verbatim.
	FailureReason string `json:"failure_reason,omitempty"`
	// BlockedURLs is the structured signal for verifier_block
	// class — permanent fetch blocks the lead can swap for
	// alternative sources. nil for non-verifier failures.
	BlockedURLs []verifier.BlockedURL `json:"blocked_urls,omitempty"`
	// LearnedRemediations is the (advisory) continuous-learning
	// overlay: worker-mined recovery instincts for this project +
	// failure class, surfaced so the lead can weigh "what has resolved
	// this class here before" alongside the static alternative
	// templates. Populated by runLeadPlanning ONLY when the
	// instinct.consumers.failure_playbooks gate is on AND the executor
	// has an instinct repo wired; nil otherwise, so with the gate off
	// the prompt is byte-for-byte unchanged. ADVISORY: it never replaces
	// the lead's judgement or auto-pivots recovery — the operator still
	// approves the checkpoint.
	LearnedRemediations []playbook.LearnedRemediation `json:"learned_remediations,omitempty"`
}

// delegatedTaskSpec represents a child task requested by an agent in its result.json.
type delegatedTaskSpec struct {
	Prompt   string `json:"prompt"`
	Role     string `json:"role"`
	Priority int    `json:"priority"`
	// Workflow optionally pins the workflow the child runs. Empty = the project
	// default. Lets a decomposer route its subtasks to a purpose-built workflow
	// (e.g. issue-fix's decompose targets `issue-subtask`) instead of the
	// project default, without changing the project-wide default.
	Workflow string `json:"workflow"`
}

// resolveExecutionPlan builds the execution plan from the workflow resolver or falls
// back to a default single-step workflow when no resolver is configured.
func (e *Executor) resolveExecutionPlan(ctx context.Context, task *persistence.Task, execution *persistence.Execution) (plan *executionPlan, err error) {
	if e.workflows == nil {
		workflowID := taskWorkflowID(task)
		execution.WorkflowID = workflowID
		return &executionPlan{
			project: nil,
			swarm: &registry.Swarm{
				ID: "default-swarm",
				Roles: []registry.SwarmRole{{
					Name: "worker",
					Runtime: registry.SwarmRoleRuntime{
						Image: e.config.RuntimeImage,
					},
				}},
			},
			workflow: &registry.Workflow{
				ID:         workflowID,
				Entrypoint: "execute",
				Steps: map[string]registry.WorkflowStep{
					"execute": {
						Type:      "agent",
						Role:      "worker",
						Prompt:    "Process task " + task.ID,
						OnSuccess: "complete",
					},
				},
				Terminals: map[string]registry.WorkflowTerminal{
					"complete": {Status: "COMPLETED"},
				},
			},
		}, nil
	}

	// Defensive: recover from typed-nil interface (e.g. (*Registry)(nil)
	// wrapped in WorkflowResolver). This shouldn't happen with the call-site
	// guard in container.go, but prevents a SIGSEGV if it does.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("workflow resolver panic (likely nil registry): %v", r)
		}
	}()

	project := e.workflows.GetProject(task.ProjectID)
	if project == nil {
		return nil, fmt.Errorf("project %s not found in workflow resolver", task.ProjectID)
	}
	swarm := e.workflows.GetSwarm(project.SwarmID)
	if swarm == nil {
		return nil, fmt.Errorf("swarm %s not found for project %s", project.SwarmID, project.ID)
	}

	workflowID := project.DefaultWorkflowID
	if task.WorkflowID != nil && *task.WorkflowID != "" && *task.WorkflowID != "-" {
		workflowID = *task.WorkflowID
	}
	workflow := e.workflows.GetWorkflow(workflowID)
	if workflow == nil {
		return nil, fmt.Errorf("workflow %s not found for project %s", workflowID, project.ID)
	}

	// Validate that every agent role in the workflow exists in the swarm.
	// For plan steps, if the named role is absent but swarm.LeadRole is set,
	// substitute it — this allows a shared "adaptive" workflow (role: "lead")
	// to work with any swarm regardless of what the lead role is called.
	swarmRoles := make(map[string]struct{}, len(swarm.Roles))
	for _, r := range swarm.Roles {
		swarmRoles[r.Name] = struct{}{}
	}
	for stepID, step := range workflow.Steps {
		if (step.Type == "agent" || step.Type == "plan") && step.Role != "" {
			if _, ok := swarmRoles[step.Role]; !ok {
				if step.Type == "plan" && swarm.LeadRole != "" {
					// Rewrite the step's role to the swarm's configured lead role.
					s := workflow.Steps[stepID]
					s.Role = swarm.LeadRole
					workflow.Steps[stepID] = s
				} else {
					return nil, fmt.Errorf("workflow %s step %s requires role %q not present in swarm %s (project %s)",
						workflowID, stepID, step.Role, swarm.ID, project.ID)
				}
			}
		}
	}

	execution.WorkflowID = workflow.ID
	// Pin the execution to this exact workflow content so resume after
	// a mid-execution YAML edit fails cleanly with WORKFLOW_DRIFT
	// instead of silently running half-old, half-new state.
	if hash := workflow.Hash(); hash != "" {
		execution.WorkflowRevision = hash
	}

	// Workflow-snapshot pinning: if the execution already has a
	// snapshot, deserialize it and use that body — the live workflow
	// may have drifted since the execution started, and the snapshot
	// is the authoritative source for the step graph this run was
	// scheduled against. If no snapshot exists yet (fresh execution
	// or pre-feature row), capture one now from the live workflow so
	// future resumes see it.
	if e.execRepo != nil && execution.ID != "" {
		if data, err := e.execRepo.GetWorkflowSnapshot(ctx, execution.ID); err == nil && len(data) > 0 {
			snap := &registry.Workflow{}
			if uerr := json.Unmarshal(data, snap); uerr == nil && snap.ID != "" {
				if liveHash, snapHash := workflow.Hash(), snap.Hash(); liveHash != snapHash && liveHash != "" {
					e.logger.Warn().
						Str("execution_id", execution.ID).
						Str("workflow_id", workflow.ID).
						Str("live_hash", liveHash).
						Str("snapshot_hash", snapHash).
						Msg("workflow drifted since execution started — using pinned snapshot for replay")
				}
				workflow = snap
			} else {
				e.logger.Warn().
					Str("execution_id", execution.ID).
					Err(uerr).
					Msg("workflow snapshot unmarshal failed — falling back to live workflow")
			}
		} else {
			// No snapshot yet — capture one now so future resumes
			// can pin to this exact workflow body. Best-effort: a
			// failed write is logged but doesn't fail the execution
			// (the live workflow + the existing hash drift guard
			// still cover us).
			if data, merr := json.Marshal(workflow); merr == nil {
				if werr := e.execRepo.SetWorkflowSnapshot(ctx, execution.ID, data); werr != nil {
					e.logger.Warn().
						Str("execution_id", execution.ID).
						Err(werr).
						Msg("workflow snapshot write failed — execution continues without pinning")
				}
			}
		}
	}

	return &executionPlan{project: project, swarm: swarm, workflow: workflow}, nil
}

// buildAgentInput constructs the task.json content for an agent container.
func buildAgentInput(task *persistence.Task, executionID, workflowID, swarmID, stepID, role, prompt string, opts *agentInputOpts) []byte {
	taskType, userPrompt, inputFiles, inputExtractions := extractAgentPayloadContext(task)
	finalPrompt, timeContext := assembleAgentPrompt(task, prompt, opts, userPrompt, inputFiles, inputExtractions)
	contextMap := buildAgentContextMap(taskType, finalPrompt, timeContext, opts)

	delegationAllowed := false
	allowedTools := []string{"file_read", "file_write", "run_shell", "current_time"}
	if opts != nil && opts.Permissions != nil {
		delegationAllowed = opts.Permissions.DelegationAllowed
		if len(opts.Permissions.AllowedTools) > 0 {
			allowedTools = opts.Permissions.AllowedTools
		}
	}

	input := map[string]any{
		"taskId":    task.ID,
		"projectId": task.ProjectID,
		"swarm": map[string]any{
			"swarmId": swarmID,
			"role":    role,
		},
		"workflow": map[string]any{
			"workflowId":  workflowID,
			"stepId":      stepID,
			"executionId": executionID,
		},
		"context": contextMap,
		"config": map[string]any{
			"timeoutSeconds": int(30 * time.Minute / time.Second),
			"permissions": map[string]any{
				"delegationAllowed": delegationAllowed,
				"allowedTools":      allowedTools,
			},
			// responseFormat is the role-level JSON-mode directive. Empty means a
			// free-form request; "json_object" tells the gateway to enforce a
			// parseable JSON object. Future "json_schema" flavours land alongside.
			"responseFormat": optResponseFormat(opts),
		},
	}
	// responseSchema (deterministic-output-schema item 7): surface the role's
	// JSON Schema so a runtime with provider-side schema enforcement can use it.
	// Additive — runtimes that don't recognise it fall back to responseFormat.
	if opts != nil && opts.ResponseSchema != nil {
		input["config"].(map[string]any)["responseSchema"] = opts.ResponseSchema
	}
	// resultEmissionTool (item 9): the strongest portable enforcement — the
	// model emits its result via a tool call whose args ARE result.json, which
	// every major provider validates against the declared schema before return.
	if opts != nil && opts.ResultEmissionTool != nil {
		input["config"].(map[string]any)["resultEmissionTool"] = opts.ResultEmissionTool
	}

	data, err := json.Marshal(input)
	if err != nil {
		return []byte("{}")
	}
	return data
}

// rewriteInputPathsInPrompt replaces references to a task's input
// host paths with the canonical container path under
// /app/workspace/artifacts/in/. Returns the rewritten prompt.
//
// The dispatcher's create_task snapshots input files into the
// durable artifact store, replacing the host path in
// task.Payload.inputFiles with the artifact store path. But the
// dispatcher LLM also has the original host path in its working
// memory (the bot's "[SYSTEM: user attached file at host path ...]"
// suffix) and sometimes embeds it in the user prompt. The agent
// then file_reads that host path inside its container, which fails.
// Rewrite saves the agent from following a stale instruction.
//
// We rewrite (a) exact host-path matches AND (b) any path whose
// basename matches an input-file basename. The basename rule
// catches LLM-hallucinated paths like /tmp/cv.pdf when cv.pdf is a
// real input file — operator-observed failure on
// task_20260516015937_*. The risk of false positives is small:
// agents rarely reference files by basename alone unless they ARE
// the attached file.
func rewriteInputPathsInPrompt(userPrompt string, inputFiles []string) string {
	if userPrompt == "" || len(inputFiles) == 0 {
		return userPrompt
	}
	out := userPrompt
	for _, src := range inputFiles {
		if src == "" {
			continue
		}
		base := filepath.Base(src)
		containerPath := "/app/workspace/artifacts/in/" + base
		// (a) exact host path → container path.
		if strings.Contains(out, src) {
			out = strings.ReplaceAll(out, src, containerPath)
		}
		// (b) any absolute /tmp/<base> or ./<base> reference →
		//     container path. Conservative: we only rewrite paths
		//     that LOOK like file references (start with / or ./),
		//     not bare basename mentions which might legitimately
		//     refer to something else.
		for _, prefix := range []string{"/tmp/", "./", "/workspace/", "/app/input/"} {
			candidate := prefix + base
			if candidate == containerPath {
				continue
			}
			if strings.Contains(out, candidate) {
				out = strings.ReplaceAll(out, candidate, containerPath)
			}
		}
	}
	return out
}

// buildAttachedFilesBlock formats the authoritative "files are here"
// trailer that every agent run with inputs gets appended to its
// prompt. Sits AFTER the user prompt + step instructions so it
// reads as ground truth the agent should trust over anything in
// the upstream prompt that might say otherwise.
//
// Two output shapes:
//
//	(a) File HAS an extraction summary → the block deliberately
//	    OMITS the staged container path. Instead it directs the
//	    agent to mcp__vornik__document_* with the artifact_id +
//	    extracted_document_id. This is the load-bearing change:
//	    prior versions listed the path AND the trailer; in
//	    practice the lead role's LLM ignored the trailer,
//	    called file_read on the staged binary, and the
//	    base64-encoded ~17 MB EPUB blew the next LLM call past
//	    the 32 MB chat-proxy cap. Omitting the path entirely
//	    removes the temptation — there is no path for the LLM
//	    to file_read.
//
//	(b) No extraction summary → legacy shape (staged path,
//	    agent reads via file_read). Operators uploading
//	    unsupported MIME types still get their files reachable.
//
// Matching is by basename: the dispatcher records artifact_id +
// storage_path, the executor stages by basename, so we cross-
// reference on the staged filename. Mirrors the email channel's
// channel-side enrichment so the LLM sees the same trailer
// shape regardless of channel.
// buildRecoveryContextBlock renders a RecoveryContext into a
// labelled prompt block the agent reads inline. The lead-planning
// path surfaces recovery via its own high-salience banner
// (buildPlanningPromptWithContext); plain agent steps route through
// buildAgentInput, where the recovery signal was previously stored
// under context.recovery with no reader. This block matches the
// trailing-reference-block style of the other ## sections so the
// role's own instructions read first.
// see https://docs.vornik.io §2
func buildRecoveryContextBlock(rc *RecoveryContext) string {
	var sb strings.Builder
	sb.WriteString("## RECOVERY_CONTEXT\n")
	sb.WriteString("A prior step failed and the workflow routed to you to keep the task alive. Propose an alternative approach instead of repeating the failed work.\n")
	if rc.FailedStep != "" {
		fmt.Fprintf(&sb, "- failed_step: %s\n", rc.FailedStep)
	}
	if rc.FailureClass != "" {
		fmt.Fprintf(&sb, "- failure_class: %s\n", rc.FailureClass)
	}
	if rc.FailureReason != "" {
		fmt.Fprintf(&sb, "- failure_reason: %s\n", truncateForPrompt(rc.FailureReason, 600))
	}
	if len(rc.BlockedURLs) > 0 {
		sb.WriteString("- blocked_urls:\n")
		for _, blk := range rc.BlockedURLs {
			fmt.Fprintf(&sb, "    - %s (reason: %s)\n", blk.URL, blk.Reason)
		}
	}
	if block := learnedRemediationsBlock(rc.LearnedRemediations); block != "" {
		sb.WriteString(block)
		sb.WriteString("\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// learnedRemediationsBlock renders the continuous-learning overlay as a
// labelled sub-section for the recovery prompt. Returns "" when there are no
// remediations, so with the failure_playbooks gate off (the caller never
// populates rc.LearnedRemediations) the prompt is byte-for-byte unchanged.
//
// Two tiers:
//   - ADVISORY (default) — "previously resolved here by …", so the lead
//     weighs the historical signal but still proposes its own alternative.
//   - DIRECTIVE (v2 auto-apply, AutoApplied=true) — a high-confidence,
//     allowlisted remediation rendered as "apply this proven remediation".
//     The operator's recovery approval gate still stands; the directive
//     changes emphasis, not authority.
//
// Auto-applied remediations are listed first (most actionable), then the
// advisory ones. With auto-apply off, every remediation is advisory and the
// block matches the prior wording.
func learnedRemediationsBlock(rems []playbook.LearnedRemediation) string {
	if len(rems) == 0 {
		return ""
	}
	var directive, advisory []playbook.LearnedRemediation
	for _, r := range rems {
		if r.AutoApplied {
			directive = append(directive, r)
		} else {
			advisory = append(advisory, r)
		}
	}
	var sb strings.Builder
	if len(directive) > 0 {
		sb.WriteString("- apply_these_proven_remediations (auto-applied: high-confidence fixes for this exact failure here — prefer them unless you have a concrete reason not to):\n")
		for _, r := range directive {
			fmt.Fprintf(&sb, "    - %s (confidence %.2f, %d resolved / %d regressed)\n",
				r.Action, r.Confidence, r.SupportCount, r.ContradictCount)
		}
	}
	if len(advisory) > 0 {
		sb.WriteString("- similar_failures_previously_resolved_here (advisory, weigh but do not blindly repeat):\n")
		for _, r := range advisory {
			fmt.Fprintf(&sb, "    - %s (confidence %.2f, %d resolved / %d regressed)\n",
				r.Action, r.Confidence, r.SupportCount, r.ContradictCount)
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

func buildAttachedFilesBlock(inputFiles []string, extractions []map[string]any) string {
	if len(inputFiles) == 0 {
		return ""
	}
	byBasename := indexExtractionsByBasename(inputFiles, extractions)

	// Partition inputs into "has extraction" (memory-only access)
	// vs "no extraction" (file_read access). This keeps the
	// two shapes in their own labelled subsections so the agent
	// can't confuse one set's path semantics for the other.
	var (
		legacy    []string
		extracted []extractedAttachment
	)
	for _, p := range inputFiles {
		if p == "" {
			continue
		}
		base := filepath.Base(p)
		if ext, ok := byBasename[base]; ok {
			extracted = append(extracted, extractedAttachment{
				Filename:            base,
				Title:               stringField(ext, "title"),
				Author:              stringField(ext, "author"),
				SectionCount:        intField(ext, "section_count"),
				ChunksIngested:      intField(ext, "chunks_ingested"),
				ExtractedDocumentID: stringField(ext, "extracted_document_id"),
				ArtifactID:          stringField(ext, "artifact_id"),
			})
			continue
		}
		legacy = append(legacy, p)
	}

	var sb strings.Builder
	if len(extracted) > 0 {
		sb.WriteString("## ATTACHED DOCUMENTS (already in project memory)\n")
		sb.WriteString("These documents have been extracted into structured text + indexed into project memory at task-creation time. The raw binary is NOT staged in the container — access the content via mcp__vornik__document_get_outline / document_read_section / document_get_metadata (use the extracted_document_id below), or via memory_search for cross-document queries. Do NOT attempt to file_read these documents — there is no staged file path.\n")
		for _, e := range extracted {
			sb.WriteString("- ")
			if e.Title != "" {
				sb.WriteString(e.Title)
				if e.Author != "" {
					sb.WriteString(" by ")
					sb.WriteString(e.Author)
				}
			} else {
				sb.WriteString(e.Filename)
			}
			sb.WriteString("\n")
			fmt.Fprintf(&sb, "    filename: %s; %d sections, %d chunks; artifact_id=%s; extracted_document_id=%s\n",
				e.Filename, e.SectionCount, e.ChunksIngested, e.ArtifactID, e.ExtractedDocumentID)
		}
	}
	if len(legacy) > 0 {
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString("## ATTACHED FILES\n")
		sb.WriteString("The following files are staged inside the container. Read them at these paths regardless of any other path mentioned in the task prompt above:\n")
		for _, p := range legacy {
			sb.WriteString("- /app/workspace/artifacts/in/")
			sb.WriteString(filepath.Base(p))
			sb.WriteString("\n")
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

// extractedAttachment is the per-input shape buildAttachedFilesBlock
// emits in the "already in memory" branch. Kept inline (not in a
// separate file) so the block-building logic is one read away.
type extractedAttachment struct {
	Filename            string
	Title               string
	Author              string
	SectionCount        int
	ChunksIngested      int
	ExtractedDocumentID string
	ArtifactID          string
}

// indexExtractionsByBasename joins the artifact-id-keyed
// extractions slice against the basename-keyed inputFiles list,
// using the artifact's storage_path basename when present. Falls
// back to a heuristic match on the extraction's title when no
// path is recorded.
func indexExtractionsByBasename(inputFiles []string, extractions []map[string]any) map[string]map[string]any {
	out := make(map[string]map[string]any, len(extractions))
	if len(extractions) == 0 {
		return out
	}
	// Each extraction landed in the dispatcher with the
	// art.StoragePath value the executor stages — the dispatcher
	// doesn't record the storage_path explicitly, but it persists
	// in the same order as inputFiles. Match positionally first,
	// then fall back to title-derived heuristics if the slice
	// lengths don't agree.
	if len(extractions) == len(inputFiles) {
		for i, p := range inputFiles {
			out[filepath.Base(p)] = extractions[i]
		}
		return out
	}
	// Best-effort: just stamp the first extraction onto every
	// input file when the counts disagree (the input-files set
	// shrinks when the dispatcher resolves artifact-id literals
	// to paths but the extractions map keeps every successful
	// extract). Operator-visible signal still attached; chunk
	// counts are total-document accurate.
	for _, p := range inputFiles {
		out[filepath.Base(p)] = extractions[0]
	}
	return out
}

// stringField pulls a string field from a map[string]any with
// "" fallback. Defensive against the dispatcher persisting the
// payload as raw JSON (numbers become json.Number; strings stay
// strings).
func stringField(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// intField extracts an integer from a map[string]any tolerating
// both int and float64 (json.Unmarshal default) shapes.
func intField(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

// optResponseFormat extracts the response-format directive from
// agentInputOpts, returning "" when opts is nil or the field is
// unset. Centralised so the buildAgentInput wiring stays clean
// even if the field ever gains shape (e.g. json_schema with
// inline schema body).
func optResponseFormat(opts *agentInputOpts) string {
	if opts == nil {
		return ""
	}
	return opts.ResponseFormat
}

// appendSchemaPromptIfEnabled mutates opts.StepPrompt to include the
// role's outputSchema rendered as prose, but only when the role has
// opted in via InjectSchemaIntoPrompt. The render is deterministic
// (sorted property names, sorted plausibility-rule when-clauses) so
// the same schema produces byte-identical prompt output across runs —
// important for the LLM gateway's prompt cache.
//
// No-op when the role is nil, has no outputSchema, or hasn't flipped
// the opt-in flag. Called from every agentInputOpts construction site
// (workflow.go's regular agent step, plan_step.go's lead-planning,
// plan_step.go's plan-spawn loop) so a migrated role behaves
// consistently no matter which path invokes it.
//
// Item 6 phase 2 of https://docs.vornik.io
func appendSchemaPromptIfEnabled(opts *agentInputOpts, role *registry.SwarmRole) {
	if opts == nil || role == nil || role.OutputSchema == nil {
		return
	}
	if !role.InjectSchemaIntoPrompt {
		return
	}
	rendered := role.OutputSchema.RenderForPrompt()
	if rendered == "" {
		return
	}
	if opts.StepPrompt == "" {
		opts.StepPrompt = rendered
		return
	}
	opts.StepPrompt = opts.StepPrompt + "\n\n" + rendered
}

// applyRoleSchemaOpts wires every output-schema-derived agentInputOpts
// field from a role config in one place:
//   - JSON-Schema body for provider-side response_format enforcement
//     (item 7)
//   - synthetic tool spec for tool-use schema enforcement (item 9)
//   - rendered prose appended to the prompt when the role opted in
//     (item 6.2)
//
// Centralised so the three agentInputOpts construction sites
// (workflow.go's regular agent step, plan_step.go's lead-planning,
// plan_step.go's plan-spawn loop) can't drift on what they wire.
//
// No-op when the role is nil or has no outputSchema.
func applyRoleSchemaOpts(opts *agentInputOpts, role *registry.SwarmRole) {
	if opts == nil || role == nil || role.OutputSchema == nil {
		return
	}
	opts.ResponseSchema = role.OutputSchema.ToJSONSchema()
	opts.ResultEmissionTool = role.OutputSchema.ToToolSpec(role.Name)
	appendSchemaPromptIfEnabled(opts, role)
}

// effectiveResponseFormat picks the gateway response_format
// directive for a role. Honors an explicit role-level
// `responseFormat:` config when set; otherwise picks the strongest
// enforcement the role's declarations justify:
//
//   - outputSchema present → "json_schema" (item 7). Strongest
//     provider-side guarantee: OpenAI + Bedrock Converse honor it
//     natively (Bedrock via synthetic tool_choice forcing), the
//     Anthropic path translates to tool-use forcing in
//     claude_subscription.buildRequestBody, and providers that
//     don't recognise the directive transparently fall back to the
//     json_object nudge via ResponseFormatFromContext's Type
//     shorthand.
//   - RequiredOutputKeys / PlausibilityRules (no schema) →
//     "json_object" (item 8). Closes the "model wrapped JSON in a
//     markdown fence" failure class for legacy roles without
//     forcing them through schema enforcement they haven't
//     declared.
//
// Returns "" only when the role neither requests json-mode
// explicitly nor declares any output shape — preserving the
// free-form behaviour writer/dispatcher/vision rely on today.
//
// Operator-set responseFormat wins over both defaults — escape
// hatch for a role whose provider rejects strict json_schema and
// the operator wants json_object fallback without dropping the
// schema (schema still drives validateRequiredOutputKeys +
// plausibility post-receipt; only the provider directive
// changes).
//
// Items 7 and 8 of https://docs.vornik.io
func effectiveResponseFormat(role *registry.SwarmRole) string {
	if role == nil {
		return ""
	}
	if role.ResponseFormat != "" {
		return role.ResponseFormat
	}
	if role.OutputSchema != nil && role.OutputSchema.ToJSONSchema() != nil {
		return "json_schema"
	}
	if len(role.RequiredOutputKeys) > 0 || len(role.PlausibilityRules) > 0 {
		return "json_object"
	}
	return ""
}

// findSwarmRole locates a role by name within a swarm configuration.
func findSwarmRole(swarm *registry.Swarm, roleName string) (*registry.SwarmRole, error) {
	if swarm == nil {
		return nil, fmt.Errorf("swarm config is not available")
	}
	for i := range swarm.Roles {
		if swarm.Roles[i].Name == roleName {
			return &swarm.Roles[i], nil
		}
	}
	return nil, fmt.Errorf("swarm role %s not found in swarm %s", roleName, swarm.ID)
}

// taskWorkflowID extracts the workflow ID from a task, falling back to a default.
// Sanitizes LLM-generated placeholder values (e.g. "-") to the default.
func taskWorkflowID(task *persistence.Task) string {
	if task != nil && task.WorkflowID != nil && *task.WorkflowID != "" && *task.WorkflowID != "-" {
		return *task.WorkflowID
	}
	return "default-workflow"
}

// buildRouteCorrectiveHint formats the addendum appended to the
// route step's prompt when the lead's first attempt either picked
// a workflow outside the project's candidate list (badPick != "")
// or failed to emit a selected_workflow at all (badPick == "" —
// typically a prose refusal asking for "missing config"). Listing
// the allowed values verbatim is what lets the second attempt
// recover without the operator widening the candidate list.
func buildRouteCorrectiveHint(badPick string, candidates []string) string {
	if badPick == "" {
		return fmt.Sprintf(
			"\n\nROUTE CORRECTION: Your previous response did not include a `selected_workflow` field. Refusal is not allowed — the candidate list IS the configuration; do not request additional files. You MUST pick EXACTLY ONE of: %v. Emit {\"selected_workflow\": \"<one-of-those>\", \"reason\": \"...\"} and nothing else.",
			candidates,
		)
	}
	return fmt.Sprintf(
		"\n\nROUTE CORRECTION: Your previous response selected workflow %q, which is NOT in the allowed list. You must pick EXACTLY ONE of: %v. Emit {\"selected_workflow\": \"<one-of-those>\", \"reason\": \"...\"} and nothing else for the workflow field.",
		badPick, candidates,
	)
}

// maxRouteDepth caps how many consecutive ROUTE-source ancestors a
// task may have. Mirrors maxCheckpointDepth: a misconfigured router
// (e.g. project default == the routing workflow itself, or a
// candidate workflow whose entrypoint also emits selected_workflow)
// can otherwise chain forever, burning ~3k LLM tokens per link.
// Three is generous enough to allow legitimate nested routing
// patterns while turning any pathological loop into a single failed
// task with a clear error class.
const maxRouteDepth = 3

// delegateSelectedWorkflow creates exactly one child task running
// the lead-chosen workflow. Used by the strict adaptive path: when
// the lead's result.json carries `selected_workflow`, the executor
// validates the choice against the project's candidate list and
// delegates the real work via this helper. Parent's payload (the
// original task instruction) is copied verbatim onto the child so
// the chosen workflow runs against the same input.
//
// Validation is strict (post-2026-05-15 incident): the requested
// workflow MUST be in project.AdaptiveCandidateWorkflows. Picks
// outside the list return an error rather than silently falling
// back to project.DefaultWorkflowID — when the default is the
// routing workflow itself (assistant project: defaultWorkflowId:
// adaptive, candidates: [research]), the silent fallback created an
// unbounded child chain (each child re-ran the router, picked the
// same out-of-list workflow, fell back to adaptive, recursed). The
// caller is expected to handle the bad pick by retrying the route
// step with a corrective hint (see workflow.go) before giving up.
//
// Two additional loop guards (Fix A + B from the same incident):
//
//  1. Same-workflow guard: refuse to spawn a child whose workflow_id
//     equals the parent's. This catches the pathological case where
//     the candidate list contains the routing workflow itself.
//
//  2. Depth cap: refuse to spawn when the parent already has
//     maxRouteDepth consecutive ROUTE ancestors. This catches any
//     remaining loop topology (e.g. a candidate workflow that itself
//     emits selected_workflow back to the router) that slips past the
//     same-workflow check.
//
// Returns the workflow_id used so the caller can log what landed.
func (e *Executor) delegateSelectedWorkflow(ctx context.Context, parent *persistence.Task, project *registry.Project, requested string) (string, error) {
	if !slices.Contains(project.AdaptiveCandidateWorkflows, requested) {
		return "", fmt.Errorf("strict adaptive: selected workflow %q not in candidate list %v", requested, project.AdaptiveCandidateWorkflows)
	}
	chosen := requested
	if parent.WorkflowID != nil && chosen == *parent.WorkflowID {
		return "", fmt.Errorf("strict adaptive: refusing to spawn child running parent's own workflow %q — would create a routing loop", chosen)
	}
	depth, err := e.countRouteDepth(ctx, parent)
	if err != nil {
		// Best-effort: a DB error on the depth walk shouldn't stop a
		// legitimate first-level route. Log and proceed; the next
		// failure's check will still cap the chain.
		e.logger.Warn().Err(err).Str("task_id", parent.ID).
			Msg("strict adaptive: depth walk failed, proceeding with delegation anyway")
		depth = 0
	}
	if depth >= maxRouteDepth {
		return "", fmt.Errorf("strict adaptive: route depth %d reached cap %d — refusing further delegation", depth, maxRouteDepth)
	}

	mode := persistence.DelegationModeSequential
	child := &persistence.Task{
		ID:             persistence.GenerateID("task"),
		ProjectID:      parent.ProjectID,
		WorkflowID:     &chosen,
		ParentTaskID:   &parent.ID,
		CreationSource: persistence.TaskCreationSourceRoute,
		DelegationMode: &mode,
		Status:         persistence.TaskStatusQueued,
		Priority:       parent.Priority,
		Payload:        parent.Payload,
		Attempt:        1,
		MaxAttempts:    parent.MaxAttempts,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	if err := e.taskRepo.Create(ctx, child); err != nil {
		return "", fmt.Errorf("create child task: %w", err)
	}
	return chosen, nil
}

// countRouteDepth walks task.ParentTaskID upward and returns the
// number of consecutive ROUTE-source ancestors. The walk stops at
// the first non-route task (typically the original USER /
// AUTONOMOUS / DELEGATION task) and at any chain longer than
// maxRouteDepth*2 to bound a malformed cycle. A nil ParentTaskID
// terminates cleanly with depth=0.
//
// Sibling of countCheckpointDepth in checkpoint.go.
func (e *Executor) countRouteDepth(ctx context.Context, t *persistence.Task) (int, error) {
	depth := 0
	cursor := t
	guard := maxRouteDepth*2 + 1
	for cursor != nil && cursor.ParentTaskID != nil && guard > 0 {
		guard--
		parent, err := e.taskRepo.Get(ctx, *cursor.ParentTaskID)
		if err != nil {
			return depth, err
		}
		if parent == nil {
			return depth, nil
		}
		if parent.CreationSource != persistence.TaskCreationSourceRoute {
			return depth, nil
		}
		depth++
		cursor = parent
	}
	return depth, nil
}

// Delegation guard defaults (N4). Phase 1 of the LLD hardcoded these;
// they are now operator-tunable via executor.Config but keep the same
// documented values when unset.
// See https://docs.vornik.io §3 (Delegation Limits).
const (
	defaultDelegationDepthLimit  = 5
	defaultDelegationFanOutLimit = 20
)

// delegationGuardError is the typed signal raised when an N4 delegation
// guard rejects a batch (depth / fan-out / cycle). It carries the
// failure class so callers stamp last_error_class consistently, and
// implements error so it bubbles through the normal step-error path and
// fails the parent — children are never created on a guard violation.
// See https://docs.vornik.io §3.
type delegationGuardError struct {
	reason string // "depth" | "fanout" | "cycle"
	msg    string
}

func (e *delegationGuardError) Error() string { return e.msg }

// FailureClass lets handleFailure stamp last_error_class without a type
// switch on the concrete error.
func (e *delegationGuardError) FailureClass() string {
	return persistence.TaskFailureClassDelegationGuard
}

// parseDelegationMode maps an agent-supplied mode string onto the
// persistence enum. Unknown / empty input falls back to PARALLEL — the
// operator-chosen default (2026-05-30): an agent that omits the field
// gets concurrent children. This is NOT the old unbounded fan-out —
// the N4 fan-out guard (default 20) bounds the batch, and the depth +
// cycle guards still apply. An agent must set "sequential" explicitly
// to opt into the serial dependency chain.
// See https://docs.vornik.io §3.
func parseDelegationMode(s string) persistence.DelegationMode {
	switch persistence.DelegationMode(strings.ToUpper(strings.TrimSpace(s))) {
	case persistence.DelegationModeSequential:
		return persistence.DelegationModeSequential
	case persistence.DelegationModeFanOut:
		return persistence.DelegationModeFanOut
	default:
		return persistence.DelegationModeParallel
	}
}

func (e *Executor) delegationDepthLimit() int {
	if e.config != nil && e.config.DelegationDepthLimit > 0 {
		return e.config.DelegationDepthLimit
	}
	return defaultDelegationDepthLimit
}

func (e *Executor) delegationFanOutLimit() int {
	if e.config != nil && e.config.DelegationFanOutLimit > 0 {
		return e.config.DelegationFanOutLimit
	}
	return defaultDelegationFanOutLimit
}

// countDelegationDepth returns how many delegation levels deep the
// children of `parent` would sit: the count of DELEGATION-source tasks
// in parent's lineage INCLUDING parent itself. A root USER task that
// delegates produces depth-0 children; a child of that (DELEGATION
// source) that delegates produces depth-1 children; and so on. When
// `depth >= limit` further delegation is refused, so a limit of 5
// permits five nested delegation levels.
//
// The walk is bounded by depthLimit*2+1 so a malformed lineage cycle in
// the stored rows can't spin forever (the cycle guard runs first and
// catches a true loop; this bound is belt-and-suspenders).
//
// Sibling of countRouteDepth (route-step variant) — this one keys on
// DELEGATION source rather than ROUTE.
// See https://docs.vornik.io §3 (Depth Limit).
func (e *Executor) countDelegationDepth(ctx context.Context, parent *persistence.Task) (int, error) {
	depth := 0
	if parent.CreationSource == persistence.TaskCreationSourceDelegation {
		depth++
	}
	cursor := parent
	guard := e.delegationDepthLimit()*2 + 2
	for cursor != nil && cursor.ParentTaskID != nil && guard > 0 {
		guard--
		anc, err := e.taskRepo.Get(ctx, *cursor.ParentTaskID)
		if err != nil {
			return depth, err
		}
		if anc == nil {
			return depth, nil
		}
		if anc.CreationSource == persistence.TaskCreationSourceDelegation {
			depth++
		}
		cursor = anc
	}
	return depth, nil
}

// ancestorIDs returns the set of task IDs on parent's lineage chain,
// including parent itself. Used by the circular-dependency guard: a
// delegation that would (re-)target an ancestor already in the chain is
// the durable-store analog of "A delegates to B which delegates back to
// A". The walk is bounded the same way as countDelegationDepth.
// See https://docs.vornik.io §3 (Circular Dependency Detection).
func (e *Executor) ancestorIDs(ctx context.Context, parent *persistence.Task) (map[string]struct{}, error) {
	seen := map[string]struct{}{parent.ID: {}}
	cursor := parent
	guard := e.delegationDepthLimit()*2 + 2
	for cursor != nil && cursor.ParentTaskID != nil && guard > 0 {
		guard--
		pid := *cursor.ParentTaskID
		if _, ok := seen[pid]; ok {
			// The stored lineage already loops — surface it as a cycle.
			return seen, &delegationGuardError{
				reason: "cycle",
				msg:    fmt.Sprintf("delegation cycle detected in lineage of task %s (ancestor %s repeats)", parent.ID, pid),
			}
		}
		seen[pid] = struct{}{}
		anc, err := e.taskRepo.Get(ctx, pid)
		if err != nil {
			return seen, err
		}
		if anc == nil {
			return seen, nil
		}
		cursor = anc
	}
	return seen, nil
}

// createDelegatedTasks creates child tasks from an agent's delegation request.
// Delegated tasks use the project's default workflow rather than inheriting the
// parent's workflow_id — the parent may be running a specialized workflow (e.g.
// scout, roadmap-revision) whose roles don't match the child's needs.
//
// The requested mode selects the child execution topology:
//
//   - SEQUENTIAL (explicit opt-in): children form a serial dependency
//     chain (child[i] depends on child[i-1]) so the lease query releases
//     them one at a time — a sequence of dependent steps.
//   - PARALLEL (the default — unknown/empty falls here): children carry no
//     inter-sibling dependency, so every child is immediately leasable and
//     they run concurrently, bounded by the N4 fan-out guard (default 20)
//     and the existing per-project concurrency caps (not bypassed).
//   - FAN_OUT: same independent-children topology as PARALLEL. The audit found
//     the LLD's FAN_OUT wording underspecified beyond "parallel with a batch
//     cap"; we implement it as PARALLEL dispatch governed by the fan-out batch
//     guard below, and document that reading in the LLD.
//
// Before creating any child, the N4 guards run (depth / fan-out / cycle). On a
// guard violation a *delegationGuardError is returned and NO child is created.
//
// On partial failure (some children created, then a repo error), any children
// that were successfully inserted are deleted before returning — leaving the
// parent in a clean state for retry rather than stuck waiting for ghost children.
//
// See https://docs.vornik.io §3.
func (e *Executor) createDelegatedTasks(ctx context.Context, parent *persistence.Task, specs []delegatedTaskSpec, mode persistence.DelegationMode) error {
	// --- N4 guard: fan-out (cumulative per-parent child count) ---
	// The cap is over the parent's LIFETIME, not a single batch: a parent
	// that delegates across multiple batches (retries, or a multi-step
	// workflow that delegates from several steps) must not collectively
	// exceed the limit. Per-batch is the degenerate case (existing == 0).
	// Counts the direct children already created for this parent.
	// See https://docs.vornik.io §3.
	fanOutLimit := e.delegationFanOutLimit()
	existingChildren, err := e.taskRepo.GetChildren(ctx, parent.ID)
	if err != nil {
		return fmt.Errorf("delegation fan-out check failed: %w", err)
	}
	if total := len(existingChildren) + len(specs); total > fanOutLimit {
		e.metrics.RecordDelegationGuardRejection("fanout")
		return &delegationGuardError{
			reason: "fanout",
			msg: fmt.Sprintf("delegation fan-out would reach %d direct children (existing %d + %d new) exceeding the per-parent limit %d (task %s) — refusing batch",
				total, len(existingChildren), len(specs), fanOutLimit, parent.ID),
		}
	}

	// --- N4 guard: circular dependency (runs BEFORE depth) ---
	// A child task is brand-new (fresh ID), so it cannot itself appear in
	// the parent's lineage. The cycle guard's job is to reject a lineage
	// that ALREADY loops (a malformed stored chain) before we extend it,
	// catching the "A→B→…→A" class that the route-step guard handles for
	// its own source. It must run before the depth walk: a true cycle
	// would otherwise inflate the depth count and mis-report as a depth
	// violation. ancestorIDs returns a *delegationGuardError on a loop.
	if _, err := e.ancestorIDs(ctx, parent); err != nil {
		var ge *delegationGuardError
		if errors.As(err, &ge) {
			e.metrics.RecordDelegationGuardRejection("cycle")
			return ge
		}
		return fmt.Errorf("delegation cycle check failed: %w", err)
	}

	// --- N4 guard: depth ---
	depthLimit := e.delegationDepthLimit()
	depth, err := e.countDelegationDepth(ctx, parent)
	if err != nil {
		return fmt.Errorf("delegation depth walk failed: %w", err)
	}
	if depth >= depthLimit {
		e.metrics.RecordDelegationGuardRejection("depth")
		return &delegationGuardError{
			reason: "depth",
			msg:    fmt.Sprintf("delegation depth %d reached limit %d (task %s) — refusing further delegation", depth, depthLimit, parent.ID),
		}
	}

	childMode := mode
	if childMode == "" {
		// Empty defaults to PARALLEL (operator-chosen default, matching
		// parseDelegationMode). Bounded by the fan-out guard above.
		childMode = persistence.DelegationModeParallel
	}

	var created []string   // IDs of successfully inserted children
	var prevChildID string // serial-chain predecessor (SEQUENTIAL only)
	for _, spec := range specs {
		priority := spec.Priority
		if priority <= 0 {
			priority = parent.Priority
		}
		payload, _ := json.Marshal(map[string]any{
			"context": map[string]any{
				"prompt": spec.Prompt,
			},
		})
		var childWorkflowID *string
		if wf := strings.TrimSpace(spec.Workflow); wf != "" {
			childWorkflowID = &wf
		}
		child := &persistence.Task{
			ID:             persistence.GenerateID("task"),
			ProjectID:      parent.ProjectID,
			WorkflowID:     childWorkflowID, // nil = project default; pinned when the spec sets a workflow
			ParentTaskID:   &parent.ID,
			CreationSource: persistence.TaskCreationSourceDelegation,
			DelegationMode: &childMode,
			Status:         persistence.TaskStatusQueued,
			Priority:       priority,
			Payload:        payload,
			Attempt:        1,
			MaxAttempts:    3,
			CreatedAt:      time.Now(),
			UpdatedAt:      time.Now(),
		}
		// SEQUENTIAL topology: chain each child behind its predecessor so
		// the lease query (which gates on tasks.dependencies) releases
		// them one at a time. PARALLEL / FAN_OUT leave Dependencies empty
		// so every child is immediately leasable — concurrency is then
		// bounded by the per-project caps the scheduler already enforces.
		// See https://docs.vornik.io §3.
		if childMode == persistence.DelegationModeSequential && prevChildID != "" {
			child.Dependencies = []string{prevChildID}
		}
		if err := e.taskRepo.Create(ctx, child); err != nil {
			// Roll back already-created children so the parent is not left
			// waiting for tasks that will never exist.
			for _, id := range created {
				if delErr := e.taskRepo.Delete(ctx, id); delErr != nil {
					e.logger.Warn().Err(delErr).Str("child_id", id).
						Msg("createDelegatedTasks: failed to clean up child after partial failure")
				}
			}
			return fmt.Errorf("failed to create delegated task: %w", err)
		}
		created = append(created, child.ID)
		prevChildID = child.ID
	}
	e.logger.Info().
		Str("parent_task_id", parent.ID).
		Str("delegation_mode", string(childMode)).
		Int("child_count", len(created)).
		Int("delegation_depth", depth).
		Msg("createDelegatedTasks: children created")
	return nil
}

// generateExecutionID creates a unique execution ID.
func generateExecutionID(_ string) string {
	return persistence.GenerateID("exec")
}

// generateArtifactID creates a unique artifact ID.
func generateArtifactID(_ string) string {
	return persistence.GenerateID("artifact")
}
