package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/llmspend"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/pricing"
)

// maxChatProxyBodyBytes caps the size of an incoming chat-completions
// request to protect the daemon from a runaway agent shipping a
// multi-GB conversation. The dispatcher's own upstream cap is 32 MiB
// (internal/chat/client.go:maxChatResponseBytes); mirroring it here
// keeps the two ends in step.
const maxChatProxyBodyBytes = 32 * 1024 * 1024

// cacheRoleRe bounds a caller-supplied X-Vornik-Role before it becomes
// part of an OpenAI prompt_cache_key: a non-empty identifier ≤64 chars
// using only chars that are valid in OpenAI's prompt_cache_key AND free
// of the ':' key delimiter (so a crafted role can't alias another
// role's cache bucket). Real vornik roles ("researcher", "coder", …)
// all match; anything else is dropped rather than passed upstream.
var cacheRoleRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// validCacheRole reports whether role is safe to fold into a
// prompt_cache_key. Invalid → the caller skips cache-key steering.
func validCacheRole(role string) bool { return cacheRoleRe.MatchString(role) }

// cachingEnabled reports whether the daemon-wide prompt-cache kill-switch
// is on. Shared by the CacheStrategy default stamp and the OpenAI
// prompt_cache_key stamp so a single config knob (chat.prompt_cache_mode)
// governs ALL cache behaviour — empty / "off" disables both.
func (s *Server) cachingEnabled() bool {
	return s.promptCacheMode != "" && s.promptCacheMode != chat.CacheModeOff
}

// ChatCompletions serves POST /api/v1/chat/completions — an internal
// OpenAI-compatible proxy. Agent containers already know how to POST
// to this shape (they were built against bedrock-access-gateway), so
// forwarding through the dispatcher's chat.Provider is a drop-in way
// to route their traffic through whatever backend the daemon is
// configured with — HTTP, Claude CLI, or future additions — without
// touching the agent image.
//
// Intentional non-goals for the MVP:
//
//   - Streaming (stream:true). Agents currently POST and read the full
//     body; SSE is extra plumbing for no current win.
//   - Per-request model override. The request's "model" field is
//     logged but ignored — the provider is single-model. Operators
//     pick the model via chat.model in config.yaml.
//   - Non-tool Complete() variant detection. We always call
//     CompleteWithTools, which handles the no-tools case by passing
//     a nil slice.
func (s *Server) ChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use POST")
		return
	}
	if s.chatProvider == nil {
		respondError(w, http.StatusServiceUnavailable, "CHAT_NOT_CONFIGURED",
			"chat provider not wired; set chat.enabled + provider in config.yaml")
		return
	}

	// LimitReader caps the body before we allocate; a broken agent
	// shipping a 10GB payload should hit EOF mid-read, not OOM the pod.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxChatProxyBodyBytes+1))
	if err != nil {
		respondError(w, http.StatusBadRequest, "READ_FAILED", err.Error())
		return
	}
	if int64(len(body)) > maxChatProxyBodyBytes {
		respondError(w, http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE",
			fmt.Sprintf("request body exceeds %d bytes", maxChatProxyBodyBytes))
		return
	}

	var req chat.ChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		respondError(w, http.StatusBadRequest, "INVALID_JSON",
			"request body is not valid JSON: "+err.Error())
		return
	}
	if len(req.Messages) == 0 {
		respondError(w, http.StatusBadRequest, "EMPTY_MESSAGES",
			"messages must be a non-empty array")
		return
	}
	// 2026-05-16 OpenAI-SDK compat: clients send stream:true by
	// default for many integrations. We don't speak SSE yet, so
	// rejecting up front with a typed error is honest. Silently
	// returning a buffered JSON body causes openai-python to
	// surface a confusing 500-like decode error.
	if req.Stream {
		respondError(w, http.StatusBadRequest, "STREAMING_NOT_SUPPORTED",
			"streaming responses (stream:true) are not yet implemented on this endpoint; retry with stream:false")
		return
	}
	ctx := r.Context()
	// Stamp Deprecation: true + rate-limited warn when the
	// client presents X-Vornik-Project-ID alongside a DB-backed
	// API key. The DB key's bound project wins for attribution
	// (cost_attribution.go); the header is silently ignored.
	// This surface tells the client to drop the redundant
	// header.
	s.maybeWarnLegacyHeaderShadowed(w, r)
	projectID := r.Header.Get("X-Vornik-Project-ID")
	if projectID != "" {
		// Reject scoped keys claiming a project they don't own. Without
		// this an agent running under project-A's API key could supply
		// `X-Vornik-Project-ID: project-B` and inherit project-B's
		// priority lane, jumping the queue.
		if !requestAllowsProject(r, projectID) {
			respondError(w, http.StatusForbidden, "FORBIDDEN", "API key not authorised for the requested project")
			return
		}
		if s.projectRegistry != nil {
			if project := s.projectRegistry.GetProject(projectID); project != nil {
				ctx = chat.WithRequestPriority(ctx, project.DefaultPriority)
			}
		}
	}
	taskID, executionID := r.Header.Get("X-Vornik-Task-ID"), r.Header.Get("X-Vornik-Execution-ID")
	recordExchange := false
	if taskID != "" || executionID != "" {
		// Validate that the task / execution actually belong to a
		// project the caller is allowed to touch. Without this an
		// agent under project-A's key could pass a taskID belonging to
		// project-B and the OnQueued / OnStart hooks would happily
		// flip project-B's task state machine via UpdateStatus —
		// cross-project IDOR.
		if taskID != "" && s.taskRepo != nil {
			task, err := s.taskRepo.Get(r.Context(), taskID)
			if err != nil || task == nil {
				respondError(w, http.StatusForbidden, "FORBIDDEN", "task not found or not accessible")
				return
			}
			if !requestAllowsProject(r, task.ProjectID) {
				respondError(w, http.StatusForbidden, "FORBIDDEN", "API key not authorised for the task's project")
				return
			}
		}
		if executionID != "" && s.executionRepo != nil {
			exec, err := s.executionRepo.Get(r.Context(), executionID)
			if err != nil || exec == nil {
				respondError(w, http.StatusForbidden, "FORBIDDEN", "execution not found or not accessible")
				return
			}
			if !requestAllowsProject(r, exec.ProjectID) {
				respondError(w, http.StatusForbidden, "FORBIDDEN", "API key not authorised for the execution's project")
				return
			}
			// Recording is decided here, on the execution's OWN project — the
			// authz-checked value, not the header — and read per request from
			// the registry snapshot (llm-exchange record/replay design §2.2).
			recordExchange = s.projectRecordsExchanges(exec.ProjectID)
		}
		// NOTE 2026-05-10: previously this wired chat.QueueHooks to
		// flip task.status QUEUED↔RUNNING based on chat-proxy queue
		// position. That was a CATEGORY ERROR — task.status is the
		// scheduler/lease source of truth (QUEUED = "available to
		// claim"; RUNNING = "leased by an executor"), not a chat
		// concurrency indicator. Setting status=QUEUED on a leased
		// task while the lease was still held caused RenewLease's
		// status guard `status IN (LEASED,RUNNING,WAITING_FOR_CHILDREN)`
		// to fail, returning ErrLeaseNotFound. After 3 consecutive
		// failures (3s) the executor escalated and cancelled the
		// execution. Live evidence: T-6f55 (the narodni-divadlo
		// research task) failed 3× this way mid-LLM-call on
		// 2026-05-10. Trigger tasks_lease_audit_after_update (v27)
		// caught a fresh occurrence on T-94fd within seconds. Hook
		// removed; chat-queue depth telemetry would belong in a
		// separate metric, not in task.status.
		_ = taskID
		_ = executionID
	}

	// Per-request model override: when the client specifies a model
	// AND the Provider supports swapping one (chat.ModelOverridable),
	// route this request through a cloned Provider pinned to the
	// requested model. This is what lets a swarm role that un-comments
	// `model: claude-opus-4-6` in its YAML actually run on Opus while
	// everything else keeps running on whatever chat.model is.
	//
	// Always call WithModel when the client supplied one, even if it
	// equals the Provider's currently-reported Model(). For a Router,
	// Model() returns the *fallback's* pinned model — skipping the
	// override here when req.Model happened to match would short-
	// circuit prefix routing and dump the request straight onto the
	// fallback. Concretely: a request for "google/gemini-2.5-pro"
	// landed on the Bedrock fallback (with chat.model pinned there)
	// instead of being routed by the "google/" prefix to Vertex.
	//
	// Providers that don't implement ModelOverridable silently ignore
	// the request's model field and use their configured default.
	provider := s.chatProvider
	if req.Model != "" {
		if o, ok := provider.(chat.ModelOverridable); ok {
			provider = o.WithModel(req.Model)
		}
	}

	// Per-request options (response_format, max_tokens) ride on the
	// context so providers that support them honour the request,
	// while providers that don't still see a no-op. Both fields are
	// directly off the OpenAI-shaped ChatRequest the agent harness
	// posts; without this propagation the Provider was ignoring them
	// (every Bedrock call used the construction-time max_tokens
	// regardless of what the role config asked for, observed
	// 2026-05-07).
	if req.ResponseFormat != nil && req.ResponseFormat.Type != "" {
		// Stamp the full struct so bedrock's json_schema path can
		// reach the schema body; the helper also stamps the Type
		// shorthand so providers that only check the kind keep
		// working unchanged.
		ctx = chat.WithRequestResponseFormatStruct(ctx, req.ResponseFormat)
	}
	if req.MaxTokens > 0 {
		ctx = chat.WithRequestMaxTokens(ctx, req.MaxTokens)
	}
	if req.CacheStrategy != nil && req.CacheStrategy.Mode != "" && req.CacheStrategy.Mode != chat.CacheModeOff {
		ctx = chat.WithRequestCacheStrategy(ctx, req.CacheStrategy)
	} else if s.cachingEnabled() {
		// Daemon-wide default kicks in when the request didn't pin
		// a strategy. Lets operators turn caching on with a single
		// config knob without touching every caller. Bedrock +
		// Anthropic honour it; other providers no-op.
		ctx = chat.WithRequestCacheStrategy(ctx, &chat.CacheStrategy{Mode: s.promptCacheMode})
	}
	// OpenAI auto-cache steering (BACKLOG "OpenAI-compat prompt_cache_key
	// passthrough"): derive a per-(project, role) cache key so requests
	// sharing an identical static prefix land on the same server-side
	// cache-affinity bucket upstream. The agent posts its role in
	// X-Vornik-Role; project comes from the already-resolved projectID.
	//
	// Wire isolation: req.PromptCacheKey is set only inside
	// (*chat.Client).doComplete, which is reached only by the
	// OpenAI-compatible *Client provider. Bedrock (chat.BedrockProvider),
	// Anthropic (chat.ClaudeSubscriptionClient) and codex
	// (chat.CodexSubscriptionClient) are distinct Provider types with
	// their own CompleteWithTools that never call doComplete and never
	// marshal ChatRequest — so the field cannot ride their wire. Stamping
	// the ctx value here is therefore a true no-op for them.
	//
	// Gated on the same cachingEnabled() kill-switch as CacheStrategy
	// above so one config knob governs ALL cache behaviour — an operator
	// who turns caching off must not still steer OpenAI auto-caching.
	//
	// The role is caller-supplied and NOT authz-checked, so it is
	// validated against a strict identifier charset before flowing into
	// OpenAI's own-validated prompt_cache_key field: a malformed role
	// would otherwise 400 every OpenAI-routed call (self-DoS) or alias
	// another role's bucket via an injected ':'. projectID is the
	// daemon-resolved, authz-checked value (not the raw header) and vornik
	// project IDs are themselves strict identifiers, so only the caller-
	// controlled role half needs guarding here.
	if s.cachingEnabled() && projectID != "" {
		role := r.Header.Get("X-Vornik-Role")
		switch {
		case role == "":
			// No role header (e.g. a pre-feature agent image) — no steering.
			// Absence is the normal degraded state, so nothing to log.
		case validCacheRole(role):
			ctx = chat.WithRequestPromptCacheKey(ctx, projectID+":"+role)
		default:
			// Drop-on-invalid keeps availability, but a role that keeps
			// failing validation silently loses cache steering — log it so
			// an unexpected agent role format is debuggable beyond a
			// hit-rate regression. Cap the logged value so an oversized
			// header can't bloat the log line.
			logged := role
			if len(logged) > 64 {
				logged = logged[:64] + "…"
			}
			s.logger.Debug().
				Str("role", logged).
				Msg("dropped malformed X-Vornik-Role from prompt_cache_key steering")
		}
	}

	// This is the agent-container entry: MODEL_UNHEALTHY on these calls is
	// owned by the executor's role modelFallback + both-down→PARK recovery, so
	// the router-level non-swarm FallbackProvider must NOT also fire here.
	// Marking the ctx keeps the two fallback mechanisms from overlapping (LLD
	// 2026-07-18-nonswarm-llm-fallback-design.md §4).
	ctx = chat.WithoutModelFallback(ctx)

	// Pass through to whichever Provider is wired. CompleteWithTools
	// is the universal method — an empty tools slice makes it
	// equivalent to Complete(), so we always go through this path to
	// keep telemetry dimensions consistent.
	auditStart := time.Now()
	// Feature #3 Phase B follow-up: emit llm_call_started so the
	// live view shows "agent is thinking" for the duration of the
	// upstream call. Per-token streaming would be tighter, but
	// the agent runtime doesn't stream today (chat-proxy refuses
	// stream:true above), so coarse-grained start/finish is the
	// best v1 surface. Nil-safe when the publisher isn't wired.
	s.emitLLMCallStarted(ctx, executionID, req.Model)
	resp, err := provider.CompleteWithTools(ctx, req.Messages, req.Tools)
	if err != nil {
		if recordExchange {
			// A replay of a step that met a provider error must reproduce it,
			// so the error envelope is the recorded response (design §2.2).
			s.recordLLMExchange(context.WithoutCancel(ctx), r, executionID, req, nil, err, time.Since(auditStart))
		}
		// Per-route queue overflow surfaces as 503 — distinct from
		// 429 (upstream pushed back) and 502 (upstream errored).
		// 503 means "the daemon refused to queue further" so HA
		// clients can back off without misreading upstream
		// availability. See chat.BoundedRouteProvider (rate-limit
		// hardening sub-item 4).
		if chat.IsRouteOverflow(err) {
			s.logger.Warn().Err(err).
				Str("client_model", req.Model).
				Msg("chat proxy: route queue overflow — returning 503")
			respondError(w, http.StatusServiceUnavailable, "ROUTE_OVERFLOW", err.Error())
			return
		}
		// Circuit breaker open for this (route, model) — the model is failing
		// hard, so we fast-rejected without an upstream call (LLD 2026-07-11-
		// model-health). Distinct 503 code so the executor skips its retry
		// ladder and fails over immediately instead of hammering a dead model.
		if chat.IsModelUnhealthy(err) {
			s.logger.Warn().Err(err).
				Str("client_model", req.Model).
				Msg("chat proxy: model circuit open — returning 503 MODEL_UNHEALTHY")
			respondError(w, http.StatusServiceUnavailable, "MODEL_UNHEALTHY", err.Error())
			return
		}
		// Context-window overflow is a deterministic 400, not an upstream
		// outage — sanitizing it into PROVIDER_ERROR (as before 2026-07-12)
		// made the executor infra-retry an identical oversized prompt ~12
		// times per execution. Return a distinct code so the agent can
		// compact-and-retry and the retry ladder stays out of it. The
		// message is generic (no upstream echo) — the code is the signal.
		if chat.IsContextOverflow(err) {
			s.logger.Warn().Err(err).
				Str("client_model", req.Model).
				Int("message_count", len(req.Messages)).
				Msg("chat proxy: prompt exceeds model context window — returning 400 CONTEXT_OVERFLOW")
			s.emitLLMCallFinishedErr(ctx, executionID, req.Model, err, time.Since(auditStart))
			respondError(w, http.StatusBadRequest, "CONTEXT_OVERFLOW",
				"prompt exceeds the model's context window — compact the conversation or reduce input")
			return
		}
		// 502 rather than 500: from the agent's point of view this is
		// an upstream failure (Claude rate-limited, CLI subprocess
		// crashed, gateway 5xx, etc.) — not a bug in vornik's handler.
		s.logger.Warn().Err(err).
			Str("client_model", req.Model).
			Int("message_count", len(req.Messages)).
			Int("tool_count", len(req.Tools)).
			Msg("chat proxy: provider returned error")
		s.emitLLMCallFinishedErr(ctx, executionID, req.Model, err, time.Since(auditStart))
		// Don't forward raw upstream error strings to the
		// external caller — provider errors frequently echo
		// internal routing info, model names, and occasionally
		// credentials from misconfigured auth middleware. Log
		// the detail; return a sanitized envelope.
		respondError(w, http.StatusBadGateway, "PROVIDER_ERROR", "upstream provider returned an error")
		return
	}
	if resp == nil {
		s.logger.Warn().
			Str("client_model", req.Model).
			Int("message_count", len(req.Messages)).
			Int("tool_count", len(req.Tools)).
			Msg("chat proxy: provider returned nil response")
		respondError(w, http.StatusBadGateway, "PROVIDER_ERROR", "provider returned nil response")
		return
	}

	// Fill in Model on the response when the provider left it blank —
	// the agent container reads this for its own logs and gets upset
	// when it's empty. The Provider.Model() getter is the stable
	// per-instance default (post-override, if one was applied).
	if resp.Model == "" {
		resp.Model = provider.Model()
	}
	// Surface the context-budget tier to the client via response
	// header + bump the per-project counter. Computed off the actual
	// prompt-token count the upstream provider reported, divided by
	// the deployment's configured chatContextBudget. Disabled when
	// no budget is configured — header omitted, metric skipped, no
	// behavioural change to the legacy proxy contract.
	s.attachContextTier(w, projectID, resp)
	// 2026-05-16 OpenAI-SDK compat: openai-python's schema validator
	// rejects responses with id="" or object!="chat.completion" or
	// created==0 (the field is non-optional in newer SDKs). Providers
	// vary in which of these they set, so we backfill defensively.
	// The id format matches OpenAI's "chatcmpl-<nanos>" convention so
	// audit-log searches across multiple deployments stay readable.
	// MUST run before encode (mutates the response body).
	if resp.ID == "" {
		resp.ID = fmt.Sprintf("chatcmpl-vornik-%d", time.Now().UnixNano())
	}
	if resp.Object == "" {
		resp.Object = "chat.completion"
	}
	if resp.Created == 0 {
		resp.Created = time.Now().Unix()
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		// Response already partially written — nothing more we can do
		// than log. The agent will see a truncated body and error out
		// on its end, which is an acceptable failure mode for an
		// unusual I/O error at this stage.
		s.logger.Warn().Err(err).Msg("chat proxy: failed to encode response")
	}

	// Telemetry is fire-and-forget AFTER the response is flushed.
	// recordChatAPIUsage + recordChatAPIAudit are blocking Postgres
	// writes and emitLLMCallFinished publishes to live pub/sub — none
	// of it must stall the client (these taps were added in Feature #3
	// and made external /api/chat + /v1/chat/completions calls feel
	// laggy because they ran pre-WriteHeader). Detached context because
	// r.Context() is cancelled the moment this handler returns; reading
	// r's headers from the goroutine is safe (net/http does not reuse
	// the Request after the handler returns). Regression-guarded by
	// TestChatCompletions_ResponseDoesNotWaitForSlowUsageSink.
	telemetryCtx := context.WithoutCancel(ctx)
	cost := s.computeChatCallCost(req.Model, resp)
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				s.logger.Error().Interface("panic", rec).Msg("chat proxy: telemetry goroutine panicked")
			}
		}()
		// Cost for external (non-agent) callers; internal agent calls
		// (X-Vornik-Task-ID / X-Vornik-Execution-ID) are skipped inside
		// the recorders and bill themselves via /api/v1/internal/llm-usage.
		s.recordChatAPIUsage(telemetryCtx, r, req.Model, resp)
		s.recordChatAPIAudit(telemetryCtx, r, auditStart, req.Model, req.Messages, resp, cost)
		s.emitLLMCallFinished(telemetryCtx, executionID, req.Model, resp, time.Since(auditStart))
		if recordExchange {
			s.recordLLMExchange(telemetryCtx, r, executionID, req, resp, nil, time.Since(auditStart))
		}
	}()
}

// (markChatProxyWorkQueued / markChatProxyWorkRunning removed
// 2026-05-10 — see the QueueHooks comment above for why.)

// computeChatCallCost returns the USD cost of one chat-proxy /
// ollama-proxy call using the daemon's pricing table. Returns 0
// when no pricing is configured, the response is nil, or the
// effective model isn't in the price list — matching
// recordChatAPIUsage's "cost row still lands with CostUSD=0" rule
// so operators see the token counts even without a price.
//
// `effectiveModel` is the post-resolution model name (resp.Model
// when set, else the requested model). Callers picked the right
// one before invoking this helper.
func (s *Server) computeChatCallCost(effectiveModel string, resp *chat.ChatResponse) float64 {
	if resp == nil {
		return 0
	}
	table := s.pricingTableLoaded()
	if table == nil {
		return 0
	}
	return table.CostUSDWithCache(effectiveModel,
		resp.Usage.PromptTokens,
		resp.Usage.CompletionTokens,
		resp.Usage.CacheCreationTokens,
		resp.Usage.CacheReadTokens)
}

// pricingTableLoaded returns the daemon's pricing.Table, loading
// it once on first use. Used by the chat-proxy + Ollama-proxy
// usage recorders to compute per-call cost. Errors and missing
// paths are logged and treated as "no pricing" — the call still
// goes through, the cost row still lands, just with CostUSD=0
// so the operator sees the token counts even without a price.
func (s *Server) pricingTableLoaded() *pricing.Table {
	if s.pricingPath == "" {
		return nil
	}
	s.pricingTableOnce.Do(func() {
		t, err := pricing.Load(s.pricingPath)
		if err != nil {
			s.logger.Warn().Err(err).Str("path", s.pricingPath).
				Msg("chat proxy: pricing.yaml load failed — usage rows will carry CostUSD=0")
			return
		}
		s.pricingTableCache = t
	})
	return s.pricingTableCache
}

// recordChatAPIUsage writes a TaskLLMUsage row for a third-party
// chat-proxy call. Mirrors dispatcher.Agent.recordLLMUsage but
// keyed off external-API attribution rather than dispatcher
// session.
//
//   - projectID: from X-Vornik-Project-ID header when supplied,
//     else the daemon's externalAPIBillingProjectID, else
//     "_external" so the row never lands with an empty project
//     (which would fail the DB's NOT NULL constraint).
//   - sessionID: User-Agent fingerprint so audit queries can
//     group "Open WebUI traffic" vs "Python SDK" vs "curl".
//   - source: TaskLLMUsageSourceExternalAPI so the spend
//     dashboard can split external traffic from workflow steps.
//   - role: "external_api" — same convention as the dispatcher
//     uses "dispatcher".
//
// Internal swarm agent containers ALSO hit this proxy (every
// workflow step's LLM call routes through here), but they then
// flush a workflow_step row themselves via
// POST /api/v1/internal/llm-usage. To avoid double-billing, we
// skip recording here whenever the request carries
// X-Vornik-Task-ID or X-Vornik-Execution-ID — those headers are
// only ever set by vornik-internal agents (the daemon's
// container.go injects them; external clients have no way to
// produce a valid task/execution ID anyway, and the IDOR guards
// elsewhere in this file reject cross-project values).
//
// Best-effort: a repo error or nil resp is a warn log + return.
// The chat response is already on its way to the client; we
// don't fail the request because the cost row didn't land.
func (s *Server) recordChatAPIUsage(ctx context.Context, r *http.Request, model string, resp *chat.ChatResponse) {
	if s.llmUsageRepo == nil || resp == nil {
		return
	}
	// Internal swarm call — agent's entrypoint.sh will flush a
	// workflow_step row, double-recording here would inflate the
	// cost dashboard. Detect via task/execution headers; either
	// being non-empty marks the call as agent-originated.
	if r.Header.Get("X-Vornik-Task-ID") != "" || r.Header.Get("X-Vornik-Execution-ID") != "" {
		return
	}
	if resp.Usage.PromptTokens == 0 && resp.Usage.CompletionTokens == 0 {
		// Some providers (notably claude-cli) don't surface token
		// counts. Record nothing rather than write a row with all
		// zeros that would skew per-call averages. Operators
		// reading raw counts will see the gap.
		return
	}
	projectID, attribution := projectForCostAttribution(ctx, r, s.externalAPIBillingProjectID)
	s.warnOnAnonymousAttribution(r, attribution)
	s.apiMetrics.RecordCostAttribution(attribution)
	effectiveModel := resp.Model
	if effectiveModel == "" {
		effectiveModel = model
	}
	costUSD := s.computeChatCallCost(effectiveModel, resp)
	sessionID := r.Header.Get("User-Agent")
	if sessionID == "" {
		sessionID = "unknown-client"
	}
	// An external caller has no task to inherit attribution from, so the key is
	// resolved directly and the turn is billed against a session.
	s.externalAPISpend.Record(ctx, llmspend.Input{
		ProjectID:           projectID,
		Model:               effectiveModel,
		PromptTokens:        resp.Usage.PromptTokens,
		CompletionTokens:    resp.Usage.CompletionTokens,
		CostUSD:             &costUSD,
		SessionID:           &sessionID,
		APIKeyID:            strPtr(APIKeyIDFromContext(ctx)),
		CacheCreationTokens: resp.Usage.CacheCreationTokens,
		CacheReadTokens:     resp.Usage.CacheReadTokens,
	})
	s.observeChatCacheUsage(effectiveModel, "external_api",
		persistence.TaskLLMUsageSourceExternalAPI,
		int64(resp.Usage.CacheCreationTokens), int64(resp.Usage.CacheReadTokens))
}

// observeChatCacheUsage surfaces one recorded TaskLLMUsage row's
// prompt-cache token usage on Prometheus (audit N8). Computes the USD
// saved from the pricing table (read tokens served below full input
// rate). Nil-safe on both the metrics sink and the pricing table — a
// deployment without either still records the row, just without the
// Prometheus surface. Shared by the external-API proxy recorder and
// the internal workflow-step llm-usage handler so both traffic classes
// land on the same series (split by the source label).
// Takes the fields it needs rather than a ledger row: since billing moved into
// llmspend, the caller no longer builds a row to hand over, and reconstructing
// one purely to satisfy this signature would be a row that never reaches the
// ledger — an easy thing to mistake for a second write.
func (s *Server) observeChatCacheUsage(model, role, source string, cacheCreation, cacheRead int64) {
	if s.chatCacheMetrics == nil {
		return
	}
	if cacheCreation == 0 && cacheRead == 0 {
		return
	}
	var dollarsSaved float64
	if table := s.pricingTableLoaded(); table != nil {
		dollarsSaved = table.CacheSavingsUSD(model, int(cacheRead))
	}
	s.chatCacheMetrics.ObserveCacheUsage(
		model, role, source, cacheCreation, cacheRead, dollarsSaved)
}
