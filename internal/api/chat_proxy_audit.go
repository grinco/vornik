package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
)

// recordChatAPIAudit writes one chat_audit_log row per external
// chat-proxy / ollama-proxy call so the operator-visible audit
// surface covers external API traffic. Mirrors recordChatAPIUsage's
// shape (same X-Vornik-Task-ID skip, same User-Agent fallback for
// session attribution) but persists conversational content rather
// than token / cost telemetry.
//
// Best-effort: a repo error or nil resp logs a warn and returns;
// the chat response is already on its way to the client and we
// don't fail the request because the audit row didn't land.
//
// Internal swarm calls (workflow-step LLM round-trips from agent
// containers) are skipped to keep chat_audit_log focused on
// conversational surfaces. Those calls are already audited at
// step granularity via task_llm_usage + tool_audit_log.
//
// startedAt is the start-of-call timestamp the caller captured
// before invoking the provider — gives the audit row's
// DurationMs a meaningful value (vs. recording 0 because we
// started timing after the LLM round-trip completed).
func (s *Server) recordChatAPIAudit(
	ctx context.Context,
	r *http.Request,
	startedAt time.Time,
	requestedModel string,
	messages []chat.Message,
	resp *chat.ChatResponse,
	costUSD float64,
) {
	if s.chatAuditRepo == nil || resp == nil {
		return
	}
	// Skip internal agent calls — they're already audited at step
	// granularity via tool_audit_log + task_llm_usage.
	if r.Header.Get("X-Vornik-Task-ID") != "" || r.Header.Get("X-Vornik-Execution-ID") != "" {
		return
	}

	systemPrompt, userMessage := extractAuditContent(messages)
	systemPromptHash := ""
	if systemPrompt != "" {
		sum := sha256.Sum256([]byte(systemPrompt))
		systemPromptHash = hex.EncodeToString(sum[:])
		// Persist the prompt body keyed by its hash so future
		// audit-log readers can resolve the hash → body. Best-
		// effort: a SavePrompt failure logs at debug and proceeds
		// (the row still records the hash so an operator can
		// reconcile later by reading the same prompt off another
		// row that succeeded).
		if err := s.chatAuditRepo.SavePrompt(ctx, systemPromptHash, systemPrompt); err != nil {
			s.logger.Debug().Err(err).
				Str("hash", systemPromptHash).
				Msg("chat audit: SavePrompt failed (best-effort)")
		}
	}

	responseText := extractAssistantText(resp)
	projectID, attribution := projectForCostAttribution(ctx, r, s.externalAPIBillingProjectID)
	// Same per-source counter the chat_proxy hot path bumps —
	// the audit path lands the row that backs the chat_audit_log
	// surface, so dropping it here would understate
	// fallback/anonymous traffic on the SaaS dashboard.
	s.apiMetrics.RecordCostAttribution(attribution)

	// chat_id format mirrors the dispatcher's "api:<principal>"
	// convention so future tools can group all calls from one
	// caller. principal is, in priority: the User-Agent
	// (distinguishes "Open WebUI" / "Python SDK" / "curl"), then
	// "anonymous" as the last resort.
	principal := r.Header.Get("User-Agent")
	if principal == "" {
		principal = "anonymous"
	}
	chatID := "api:" + principal

	effectiveModel := resp.Model
	if effectiveModel == "" {
		effectiveModel = requestedModel
	}

	entry := &persistence.ChatAuditEntry{
		Timestamp:        time.Now().UTC(),
		ChatID:           chatID,
		ProjectID:        projectID,
		RoleUsed:         "external_api",
		Model:            effectiveModel,
		SystemPromptHash: systemPromptHash,
		UserMessage:      truncateForAuditAPI(userMessage, 500),
		ToolCallsJSON:    "[]",
		Response:         truncateForAuditAPI(responseText, 500),
		Iterations:       1,
		DurationMs:       time.Since(startedAt).Milliseconds(),
		CostUSD:          costUSD,
	}
	if err := s.chatAuditRepo.Insert(ctx, entry); err != nil {
		s.logger.Warn().Err(err).
			Str("project_id", projectID).
			Str("chat_id", chatID).
			Msg("chat audit: Insert failed (proxy call)")
	}
}

// extractAuditContent walks the request messages and returns the
// system prompt (first system-role message; empty when absent) and
// the user message (last user-role message; empty when absent).
// "last user" rather than "first" because conversational clients
// send the full history every turn, and the latest user message is
// what was just asked.
func extractAuditContent(messages []chat.Message) (systemPrompt, userMessage string) {
	for _, m := range messages {
		if systemPrompt == "" && strings.EqualFold(m.Role, "system") {
			systemPrompt = m.Content
		}
		if strings.EqualFold(m.Role, "user") {
			userMessage = m.Content
		}
	}
	return systemPrompt, userMessage
}

// extractAssistantText pulls the assistant reply text out of a
// ChatResponse. Empty when the provider returned no choices or the
// first choice has no content.
func extractAssistantText(resp *chat.ChatResponse) string {
	if resp == nil || len(resp.Choices) == 0 {
		return ""
	}
	return resp.Choices[0].Message.Content
}

// truncateForAuditAPI caps long strings to a byte limit, suffixing
// the chop with "…(truncated)" so operators can spot truncation in
// the UI. Empty stays empty. Mirrors dispatcher.truncateForAudit
// but lives in this package so the api code doesn't import the
// dispatcher just for one helper.
func truncateForAuditAPI(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	const suffix = "…(truncated)"
	if limit <= len(suffix) {
		return s[:limit]
	}
	return s[:limit-len(suffix)] + suffix
}
