package dispatcher

// The dispatcher's processing surface: Process, ProcessStreaming,
// the inner complete + doChatCall round-trip helpers, the LLM-usage
// recording path, the tool-set assembly path, and the small helpers
// that adapt their inputs (projectIDsFromRegistry, trimToLastUserTurn).
// Extracted from agent.go as part of the 2026-05-16 dispatcher split.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// recordLLMUsage persists one LLM-call row to task_llm_usage with
// source="dispatcher". Called after each successful LLM round-trip
// inside the tool-calling loop. Silent no-op when the repo isn't wired
// or the response carries no token data (some upstreams omit usage in
// the non-streaming path on partial errors).
//
// Note: the dispatcher isn't tied to a task or execution, so task_id and
// execution_id are NULL; session_id carries req.ChatID stringified. One
// row per LLM call, unlike workflow_step rows which aggregate across a
// container's internal tool loop.
func (a *Agent) recordLLMUsage(ctx context.Context, projectID string, chatID int64, resp *chat.ChatResponse) {
	if a == nil || a.llmUsageRepo == nil || resp == nil {
		return
	}
	if resp.Usage.PromptTokens == 0 && resp.Usage.CompletionTokens == 0 {
		return
	}
	model := resp.Model
	costUSD := 0.0
	if a.pricing != nil {
		costUSD = a.pricing.CostUSD(model, resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
	}
	// Route the row to the configured billing project when one is
	// set. Operators with a dedicated assistant project use this to
	// keep dispatcher chat overhead from polluting per-project cost
	// dashboards on projects that may have no automation enabled.
	billingProject := projectID
	if a.billingProjectID != "" {
		billingProject = a.billingProjectID
	}
	sessionID := strconv.FormatInt(chatID, 10)
	entry := &persistence.TaskLLMUsage{
		ID:                  persistence.GenerateID("llm"),
		ProjectID:           billingProject,
		TaskID:              nil,
		ExecutionID:         nil,
		StepID:              "",
		Role:                "dispatcher",
		Model:               model,
		PromptTokens:        int64(resp.Usage.PromptTokens),
		CompletionTokens:    int64(resp.Usage.CompletionTokens),
		Iterations:          1,
		CostUSD:             costUSD,
		Source:              persistence.TaskLLMUsageSourceDispatcher,
		SessionID:           &sessionID,
		RecordedAt:          time.Now().UTC(),
		CacheCreationTokens: int64(resp.Usage.CacheCreationTokens),
		CacheReadTokens:     int64(resp.Usage.CacheReadTokens),
	}
	if err := a.llmUsageRepo.Record(ctx, entry); err != nil {
		a.logger.Warn().Err(err).
			Str("project", projectID).
			Str("session", sessionID).
			Msg("dispatcher: failed to persist llm usage row")
	}
}

// allTools returns built-in dispatcher tools merged with MCP-discovered
// tools for the given project. MCP tools are project-scoped — a
// conversation pinned to project A must never see project B's tools.
//
// 2026.7.0 F12 — deferred loading: when the MCP catalog exceeds
// DefaultDeferredToolThreshold, MCP tools are hidden by default
// and `tool_search` surfaces in their place. Matches the model
// uncovers via tool_search expand into the visible set for the
// rest of the session (tracked per-chatID on the ToolExecutor).
// chatID=0 falls back to legacy "everything visible" behaviour
// since there's no session to anchor the expanded set to. tier
// signals the session's context-budget headroom — when DEGRADING
// or worse, deferral is forced regardless of catalog size.
func (a *Agent) allTools(projectID string, chatID int64, tier chat.ContextTier) []chat.Tool {
	builtin := DispatcherTools()
	var mcp []chat.Tool
	if a.mcpManager != nil && projectID != "" {
		mcp = a.mcpManager.Tools(projectID)
	}
	if a.toolExecutor == nil || a.toolExecutor.expanded == nil || chatID == 0 {
		// No session state to track expansions, or no expanded-
		// store wired — preserve legacy "everything visible"
		// behaviour. Sub-agent / per-task paths land here.
		return append(append(make([]chat.Tool, 0, len(builtin)+len(mcp)), builtin...), mcp...)
	}
	threshold := effectiveDeferralThreshold(DefaultDeferredToolThreshold, tier)
	return applyDeferredLoading(builtin, mcp, a.toolExecutor.expanded, chatID, threshold)
}

// Process runs one complete conversation turn:
//  1. Builds the system prompt with injected project context.
//  2. Calls the LLM with tool definitions.
//  3. Executes any tool calls and feeds results back.
//  4. Repeats until the LLM returns a final text response or the iteration cap is reached.
//
// The returned Result.Messages contains the full conversation history (without system
// prompt) and should replace the caller's session state.
func (a *Agent) Process(ctx context.Context, req Request) (result Result) {
	ctx = withOriginatingChannel(ctx, req.OriginatingChannel, req.OriginatingSessionID)
	if req.OperatorID != "" {
		ctx = WithOperatorID(ctx, req.OperatorID)
	}
	systemPrompt := req.LeadSystemPrompt
	roleLabel := "lead"
	if systemPrompt == "" {
		systemPrompt = BuildSystemPrompt(req.Project, req.Projects)
		roleLabel = "dispatcher"
	}
	systemPrompt = a.maybeInjectOperatorProfile(ctx, systemPrompt, req.OperatorID)
	audit := newChatAuditTurn(a)
	// Propagate the pre-allocated turn id through ctx so tools that
	// spawn tasks (create_task) can stamp chat_turn_id on them. No-op
	// when audit is nil (audit repo not wired).
	if audit != nil {
		ctx = WithChatTurnID(ctx, audit.id)
	}
	userMsg := ""
	if len(req.Messages) > 0 {
		for i := len(req.Messages) - 1; i >= 0; i-- {
			if req.Messages[i].Role == "user" {
				userMsg = req.Messages[i].Content
				break
			}
		}
	}
	audit.captureRequest(systemPrompt, userMsg, roleLabel)
	defer func() { audit.finish(ctx, req, result) }()
	tools := a.allTools(req.Project, req.ChatID, req.ContextTier)
	a.metrics.recordContextTier(req.Project, req.ContextTier, req.ContextHeadroomPct)

	// Work on a local copy so the caller's slice is not mutated.
	msgs := make([]chat.Message, len(req.Messages))
	copy(msgs, req.Messages)

	activeProject := req.Project
	// guardWarnings accumulates output-guard findings across every
	// tool call this turn. Rides back on Result so the UI / bot can
	// render a non-jargon banner. Always present (possibly empty);
	// no nil-vs-empty distinction the caller has to handle.
	var guardWarnings []GuardWarning
	// hallucinationRetried caps the in-loop retry-on-hallucination
	// at one. A second hallucination after the synthetic retry
	// prompt almost always indicates a model that can't ground its
	// claims — looping further only burns budget. The caller's
	// reply still carries a user-facing banner via the same fall-
	// through path.
	hallucinationRetried := false

	for i := 0; i < a.maxIterations; i++ {
		resp, updatedMsgs, err := a.complete(ctx, systemPrompt, msgs, tools, i == 0, nil)
		msgs = updatedMsgs
		if err != nil {
			return Result{
				Err:      fmt.Errorf("LLM call failed: %w", err),
				Messages: msgs,
			}
		}
		a.recordLLMUsage(ctx, activeProject, req.ChatID, resp)
		audit.recordIteration(resp, modelFromResponse(resp), 0)
		if len(resp.Choices) == 0 {
			return Result{
				Err:      fmt.Errorf("empty response from LLM"),
				Messages: msgs,
			}
		}

		assistantMsg := resp.Choices[0].Message
		msgs = append(msgs, assistantMsg)

		if len(assistantMsg.ToolCalls) == 0 {
			// Final text response — tool loop complete. Strip any
			// <think>/<reasoning> blocks gpt-oss-style models embed
			// in-content so the user sees the answer, not the model's
			// scratch pad.
			text := stripReasoning(assistantMsg.Content)
			if text == "" {
				text = "Done."
			}

			// Hallucination scan on the final reply. On High signals
			// without a prior retry, inject a synthetic user turn
			// asking the model to fix it and continue the loop.
			// On High signals with a prior retry already burned,
			// prepend a user-facing warning banner so the operator
			// knows the answer is unverified.
			if a.hallucinationDetector != nil {
				gc := a.buildChatGroundingContext(ctx, msgs, projectIDsFromRegistry(req.Projects), activeProject)
				signals := a.hallucinationDetector.Scan(text, gc)
				a.hallucinationMetrics.ObserveSignals(activeProject, signals)
				audit.recordHallucinationSignals(signals)
				blocking := retainBlockingSignals(signals)
				if len(blocking) > 0 {
					a.logger.Warn().
						Int64("chat_id", req.ChatID).
						Int("signals", len(signals)).
						Int("blocking", len(blocking)).
						Bool("retry_used", hallucinationRetried).
						Msg("dispatcher: hallucination detected in reply")
					if !hallucinationRetried {
						hallucinationRetried = true
						msgs = append(msgs, chat.Message{
							Role:    "user",
							Content: formatHallucinationRetryPrompt(blocking),
						})
						continue
					}
					text = formatUserWarningBanner(blocking) + text
				}
			}

			return Result{
				Text:          text,
				NewProject:    activeProject,
				Messages:      msgs,
				GuardWarnings: guardWarnings,
			}
		}

		// Execute each tool call in order.
		for _, tc := range assistantMsg.ToolCalls {
			a.logger.Info().
				Str("tool", tc.Function.Name).
				Msg("dispatcher: executing tool")
			a.metrics.recordToolCall(tc.Function.Name)

			// Intent judge: fire the heuristic verdict and (when
			// the refiner is wired + risk meets the floor) the
			// async LLM tier. The verdict is best-effort
			// telemetry in this slice — the dispatcher does not
			// block on it. A future iteration will gate tool
			// execution behind the recommendation.
			if a.intentJudge != nil {
				var chatIDPtr *int64
				if req.ChatID != 0 {
					id := req.ChatID
					chatIDPtr = &id
				}
				v := a.intentJudge.evaluate(ctx, activeProject, nil, nil, chatIDPtr,
					tc.Function.Name, tc.Function.Arguments, nil, a.logger)
				a.logger.Info().
					Str("tool", tc.Function.Name).
					Str("risk", string(v.Risk)).
					Str("rec", string(v.Recommendation)).
					Float64("confidence", v.Confidence).
					Msg("intent judge: heuristic verdict")
			}

			tr := a.toolExecutor.Execute(ctx, tc, activeProject, req.AllowedProjects, req.ChatID, req.FileSender)
			audit.recordToolCall(tc.Function.Name, tc.Function.Arguments, tr.Content, "")

			if tr.ProjectSwitch != nil {
				activeProject = tr.ProjectSwitch.ProjectID
				// When switching projects, always use the generic dispatcher
				// prompt — the lead persona is tied to the original project.
				systemPrompt = BuildSystemPrompt(activeProject, req.Projects)
			}

			// Output guard: scan the tool result before appending it
			// to conversation history. HIGH findings get redacted in
			// place (if configured); INFO/WARN pass through. The
			// LLM only ever sees the post-guard body.
			content := tr.Content
			if a.outputGuard != nil {
				var w GuardWarning
				content, w = a.outputGuard.applyOutputGuard(tc.Function.Name, tr.Content, tr.Provenance, a.metrics)
				if w.MaxSeverity != "" {
					a.logger.Warn().
						Str("tool", w.Tool).
						Str("severity", string(w.MaxSeverity)).
						Strs("kinds", w.Kinds).
						Bool("redacted", w.Redacted).
						Msg("output guard: findings on tool result")
					guardWarnings = append(guardWarnings, w)
				}
			}

			msgs = append(msgs, chat.Message{
				Role:       "tool",
				Content:    content,
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
			})
		}
	}

	// Iteration cap reached — return last partial state.
	a.logger.Warn().
		Int("cap", a.maxIterations).
		Msg("dispatcher: tool iteration cap reached")

	return Result{
		Text:          "I reached the tool step limit for this turn. Your request may need to be broken into smaller steps, or you can continue from where I left off.",
		Messages:      msgs,
		GuardWarnings: guardWarnings,
	}
}

// ProcessStreaming is like Process but streams LLM output to the caller via
// onText. The callback receives the accumulated text content during each LLM
// call; between calls (during tool execution) it receives a status line like
// "[running: create_task]". The final Result is identical to Process.
func (a *Agent) ProcessStreaming(ctx context.Context, req Request, onText chat.StreamCallback) (result Result) {
	ctx = withOriginatingChannel(ctx, req.OriginatingChannel, req.OriginatingSessionID)
	if req.OperatorID != "" {
		ctx = WithOperatorID(ctx, req.OperatorID)
	}
	systemPrompt := req.LeadSystemPrompt
	roleLabel := "lead"
	if systemPrompt == "" {
		systemPrompt = BuildSystemPrompt(req.Project, req.Projects)
		roleLabel = "dispatcher"
	}
	systemPrompt = a.maybeInjectOperatorProfile(ctx, systemPrompt, req.OperatorID)
	audit := newChatAuditTurn(a)
	// Propagate the pre-allocated turn id through ctx so tools that
	// spawn tasks (create_task) can stamp chat_turn_id on them.
	if audit != nil {
		ctx = WithChatTurnID(ctx, audit.id)
	}
	userMsg := ""
	if len(req.Messages) > 0 {
		for i := len(req.Messages) - 1; i >= 0; i-- {
			if req.Messages[i].Role == "user" {
				userMsg = req.Messages[i].Content
				break
			}
		}
	}
	audit.captureRequest(systemPrompt, userMsg, roleLabel)
	defer func() { audit.finish(ctx, req, result) }()
	tools := a.allTools(req.Project, req.ChatID, req.ContextTier)
	a.metrics.recordContextTier(req.Project, req.ContextTier, req.ContextHeadroomPct)

	msgs := make([]chat.Message, len(req.Messages))
	copy(msgs, req.Messages)

	activeProject := req.Project
	var guardWarnings []GuardWarning
	hallucinationRetried := false

	var streamAccum string
	emitStreamDelta := func(delta string) {
		if onText == nil || delta == "" {
			return
		}
		streamAccum += delta
		onText(streamAccum)
	}

	for i := 0; i < a.maxIterations; i++ {
		// Provider callbacks are accumulated per LLM call, while the
		// ChannelReceiver expects accumulated text for the whole turn.
		// Track each model-call prefix separately and re-emit a single
		// whole-turn stream so tool-use turns don't trim the beginning of
		// the next assistant message.
		var llmAccum string
		streamOnText := wrapStreamCallbackStripping(func(accumulated string) {
			if len(accumulated) <= len(llmAccum) {
				return
			}
			delta := accumulated[len(llmAccum):]
			llmAccum = accumulated
			emitStreamDelta(delta)
		})
		resp, updatedMsgs, err := a.complete(ctx, systemPrompt, msgs, tools, i == 0, streamOnText)
		msgs = updatedMsgs
		if err != nil {
			return Result{
				Err:      fmt.Errorf("LLM call failed: %w", err),
				Messages: msgs,
			}
		}
		a.recordLLMUsage(ctx, activeProject, req.ChatID, resp)
		audit.recordIteration(resp, modelFromResponse(resp), 0)
		if len(resp.Choices) == 0 {
			return Result{
				Err:      fmt.Errorf("empty response from LLM"),
				Messages: msgs,
			}
		}

		assistantMsg := resp.Choices[0].Message
		msgs = append(msgs, assistantMsg)

		if len(assistantMsg.ToolCalls) == 0 {
			text := stripReasoning(assistantMsg.Content)
			if text == "" {
				text = "Done."
			}
			if a.hallucinationDetector != nil {
				gc := a.buildChatGroundingContext(ctx, msgs, projectIDsFromRegistry(req.Projects), activeProject)
				signals := a.hallucinationDetector.Scan(text, gc)
				a.hallucinationMetrics.ObserveSignals(activeProject, signals)
				audit.recordHallucinationSignals(signals)
				blocking := retainBlockingSignals(signals)
				if len(blocking) > 0 {
					a.logger.Warn().
						Int64("chat_id", req.ChatID).
						Int("signals", len(signals)).
						Int("blocking", len(blocking)).
						Bool("retry_used", hallucinationRetried).
						Msg("dispatcher: hallucination detected in streaming reply")
					if !hallucinationRetried {
						hallucinationRetried = true
						msgs = append(msgs, chat.Message{
							Role:    "user",
							Content: formatHallucinationRetryPrompt(blocking),
						})
						continue
					}
					text = formatUserWarningBanner(blocking) + text
				}
			}
			return Result{
				Text:          text,
				NewProject:    activeProject,
				Messages:      msgs,
				GuardWarnings: guardWarnings,
			}
		}

		for _, tc := range assistantMsg.ToolCalls {
			a.logger.Info().Str("tool", tc.Function.Name).Msg("dispatcher: executing tool")

			if onText != nil {
				// Status marker bracketed in blank lines so the
				// receiver renders it as its own paragraph instead
				// of concatenating with the streamed text on either
				// side. Pre-fix the bare "[running: create_task]"
				// marker docked against the preceding/following
				// stream chunks (e.g. "...create_task]ask to
				// generate" — the "ask" is the "t" of "task" being
				// adjacent without separator).
				emitStreamDelta(fmt.Sprintf("\n\n%s\n\n", toolStatusMarker(tc.Function.Name)))
			}

			// Intent judge: mirror of the Process path. The
			// streaming path doesn't surface the verdict to the
			// user (no banner mid-stream); telemetry-only here.
			if a.intentJudge != nil {
				var chatIDPtr *int64
				if req.ChatID != 0 {
					id := req.ChatID
					chatIDPtr = &id
				}
				v := a.intentJudge.evaluate(ctx, activeProject, nil, nil, chatIDPtr,
					tc.Function.Name, tc.Function.Arguments, nil, a.logger)
				a.logger.Info().
					Str("tool", tc.Function.Name).
					Str("risk", string(v.Risk)).
					Str("rec", string(v.Recommendation)).
					Float64("confidence", v.Confidence).
					Msg("intent judge: heuristic verdict")
			}

			tr := a.toolExecutor.Execute(ctx, tc, activeProject, req.AllowedProjects, req.ChatID, req.FileSender)
			audit.recordToolCall(tc.Function.Name, tc.Function.Arguments, tr.Content, "")

			if tr.ProjectSwitch != nil {
				activeProject = tr.ProjectSwitch.ProjectID
				systemPrompt = BuildSystemPrompt(activeProject, req.Projects)
			}

			// Output guard: mirror of the non-streaming path. HIGH
			// findings get redacted in place (when configured);
			// lower severities ride back on Result.GuardWarnings.
			content := tr.Content
			if a.outputGuard != nil {
				var w GuardWarning
				content, w = a.outputGuard.applyOutputGuard(tc.Function.Name, tr.Content, tr.Provenance, a.metrics)
				if w.MaxSeverity != "" {
					a.logger.Warn().
						Str("tool", w.Tool).
						Str("severity", string(w.MaxSeverity)).
						Strs("kinds", w.Kinds).
						Bool("redacted", w.Redacted).
						Msg("output guard: findings on tool result")
					guardWarnings = append(guardWarnings, w)
				}
			}

			msgs = append(msgs, chat.Message{
				Role:       "tool",
				Content:    content,
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
			})
		}
	}

	a.logger.Warn().Int("cap", a.maxIterations).Msg("dispatcher: tool iteration cap reached")
	return Result{
		Text:          "I reached the tool step limit for this turn. Your request may need to be broken into smaller steps, or you can continue from where I left off.",
		Messages:      msgs,
		GuardWarnings: guardWarnings,
	}
}

// complete runs one LLM call. When retryEligible is true and the call fails
// with a retryable gateway error (5xx, 429), it prunes the conversation down
// to the last user turn and retries once — the common recovery for the case
// where stale tool results in prior turns have bloated the payload past what
// the upstream gateway will accept. Returns the (possibly pruned) message
// slice that was actually sent; callers should continue appending to that.
//
// If onText is non-nil, the streaming API variant is used.
func (a *Agent) complete(
	ctx context.Context,
	systemPrompt string,
	msgs []chat.Message,
	tools []chat.Tool,
	retryEligible bool,
	onText chat.StreamCallback,
) (*chat.ChatResponse, []chat.Message, error) {
	resp, err := a.doChatCall(ctx, systemPrompt, msgs, tools, onText)
	if err == nil {
		return resp, msgs, nil
	}
	if !retryEligible {
		return nil, msgs, err
	}
	var gwErr *chat.GatewayError
	if !errors.As(err, &gwErr) || !gwErr.Retryable() {
		return nil, msgs, err
	}
	pruned := trimToLastUserTurn(msgs)
	if len(pruned) == len(msgs) {
		// Nothing to prune — the payload size is already minimal, so
		// retrying would just hit the same upstream failure.
		return nil, msgs, err
	}
	a.logger.Warn().
		Err(err).
		Int("status", gwErr.Status).
		Int("orig_msgs", len(msgs)).
		Int("pruned_msgs", len(pruned)).
		Msg("dispatcher: gateway error — retrying with pruned history")
	retryResp, retryErr := a.doChatCall(ctx, systemPrompt, pruned, tools, onText)
	if retryErr != nil {
		return nil, msgs, retryErr
	}
	return retryResp, pruned, nil
}

// doChatCall builds the full message list with the system prompt and dispatches
// to streaming or non-streaming client methods based on onText.
func (a *Agent) doChatCall(
	ctx context.Context,
	systemPrompt string,
	msgs []chat.Message,
	tools []chat.Tool,
	onText chat.StreamCallback,
) (*chat.ChatResponse, error) {
	callMsgs := make([]chat.Message, 0, len(msgs)+1)
	callMsgs = append(callMsgs, chat.Message{Role: "system", Content: systemPrompt})
	callMsgs = append(callMsgs, msgs...)
	ctx = chat.WithRequestPriority(ctx, 0)
	if onText != nil {
		return a.chatClient.CompleteWithToolsStream(ctx, callMsgs, tools, onText)
	}
	return a.chatClient.CompleteWithTools(ctx, callMsgs, tools)
}

// projectIDsFromRegistry pulls the IDs out of a registry.Project
// snapshot for the dispatcher's hallucination grounding. The
// registry-side snapshot is already user-scoped (HandleMessage's
// getProjectListForUser), so feeding the IDs straight in
// gives the rule its negative-space membership check.
func projectIDsFromRegistry(projects []*registry.Project) []string {
	if len(projects) == 0 {
		return nil
	}
	out := make([]string, 0, len(projects))
	for _, p := range projects {
		if p == nil {
			continue
		}
		out = append(out, p.ID)
	}
	return out
}

// toolStatusMarker maps an internal tool identifier to the visible
// in-stream "[emoji <verb>]" status marker.
func toolStatusMarker(name string) string {
	return fmt.Sprintf("[%s %s]", toolStatusEmoji(name), humanizeToolName(name))
}

func toolStatusEmoji(name string) string {
	if strings.HasPrefix(name, "mcp__") {
		return "🔌"
	}
	switch name {
	case "memory_search", "memory_correct":
		return "🧠"
	case "create_task":
		return "📋"
	case "send_artifact":
		return "📎"
	case "send_message":
		return "✉️"
	case "summarize_thread":
		return "🧵"
	case "get_conversation_window":
		return "💬"
	case "file_read", "file_write", "file_edit", "read_many_files":
		return "📄"
	case "run_shell":
		return "⌨️"
	case "grep":
		return "🔎"
	case "glob":
		return "🗂️"
	case "current_time":
		return "🕒"
	}
	return "⏳"
}

// humanizeToolName maps internal tool identifiers to a short verb
// phrase suitable for the in-stream status marker
// the chat surface shows mid-turn. Unknown tools fall back to the
// raw name with underscores swapped for spaces — readable enough
// that adding a new tool doesn't require a same-PR humanizer update.
//
// MCP-bridge tool names have the shape `mcp__<server>__<tool>`;
// they get a special-case "using <server>/<tool>" rendering so the
// origin server is visible to the operator.
func humanizeToolName(name string) string {
	if strings.HasPrefix(name, "mcp__") {
		rest := strings.TrimPrefix(name, "mcp__")
		if idx := strings.Index(rest, "__"); idx > 0 {
			return fmt.Sprintf("using %s/%s", rest[:idx], rest[idx+2:])
		}
		return "using " + rest
	}
	switch name {
	case "create_task":
		return "creating task"
	case "send_artifact":
		return "sending artifact"
	case "send_message":
		return "sending message"
	case "memory_search":
		return "searching memory"
	case "memory_correct":
		return "correcting memory"
	case "summarize_thread":
		return "summarizing thread"
	case "get_conversation_window":
		return "loading conversation"
	case "file_read":
		return "reading file"
	case "file_write":
		return "writing file"
	case "file_edit":
		return "editing file"
	case "read_many_files":
		return "reading files"
	case "run_shell":
		return "running shell command"
	case "grep":
		return "searching files"
	case "glob":
		return "listing files"
	case "current_time":
		return "checking time"
	}
	return strings.ReplaceAll(name, "_", " ")
}

// trimToLastUserTurn returns a copy of msgs containing only messages from the
// most recent user turn onward. If no user message is present, msgs is returned
// unchanged (the caller treats that as "cannot prune further").
func trimToLastUserTurn(msgs []chat.Message) []chat.Message {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			out := make([]chat.Message, len(msgs)-i)
			copy(out, msgs[i:])
			return out
		}
	}
	return msgs
}
