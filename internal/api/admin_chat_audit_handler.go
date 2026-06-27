package api

// Admin chat-audit endpoint — mirrors AdminAuditList's shape so the
// CLI (`vornikctl admin chat-audit`) can read what the UI's
// /ui/admin/chat-audit shows. Same admin gate semantics as
// AdminAuditList: disabled → 404, missing key → 401, non-admin → 403.

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// AdminChatAuditEntryJSON is the on-the-wire shape for chat audit
// rows. Mirrors persistence.ChatAuditEntry; `omitempty` on the
// long-tail fields keeps the JSON tight when a turn was a one-shot
// reply with no tool loop.
type AdminChatAuditEntryJSON struct {
	ID                       string  `json:"id"`
	Timestamp                string  `json:"ts"`
	ChatID                   string  `json:"chat_id,omitempty"`
	UserID                   string  `json:"user_id,omitempty"`
	ProjectID                string  `json:"project_id,omitempty"`
	RoleUsed                 string  `json:"role_used,omitempty"`
	Model                    string  `json:"model,omitempty"`
	SystemPromptHash         string  `json:"system_prompt_hash,omitempty"`
	UserMessage              string  `json:"user_message,omitempty"`
	ToolCallsJSON            string  `json:"tool_calls_json,omitempty"`
	Response                 string  `json:"response,omitempty"`
	Iterations               int     `json:"iterations,omitempty"`
	DurationMs               int64   `json:"duration_ms,omitempty"`
	CostUSD                  float64 `json:"cost_usd,omitempty"`
	HallucinationSignalsJSON string  `json:"hallucination_signals_json,omitempty"`
}

// AdminChatAuditListResponse is the wire shape for
// GET /api/v1/admin/chat-audit.
type AdminChatAuditListResponse struct {
	Entries []AdminChatAuditEntryJSON `json:"entries"`
}

// AdminChatAuditList handles GET /api/v1/admin/chat-audit. Same gate
// matrix as AdminAuditList — disabled / missing key / non-admin all
// return their dedicated status. Filters: chat, project, since,
// limit. The drill-down body lookup uses the separate
// /api/v1/admin/chat-audit/prompts/{hash} endpoint.
func (s *Server) AdminChatAuditList(w http.ResponseWriter, r *http.Request) {
	if !s.adminConfig.Enabled {
		http.NotFound(w, r)
		return
	}
	if s.chatAuditRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "CHAT_AUDIT_DISABLED",
			"chat audit repository not wired")
		return
	}
	// D4 (audit 2026-06-10): route through requireAdminGate so the
	// auth-disabled override admits the trusted local operator instead
	// of 401-ing on the inline IsAdminKey check. Same auth-ON matrix.
	if !s.requireAdminGate(w, r) {
		return
	}

	q := r.URL.Query()
	limit := 50
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	filter := persistence.ChatAuditFilter{
		ChatID:    q.Get("chat"),
		ProjectID: q.Get("project"),
		PageSize:  limit,
	}
	if since := q.Get("since"); since != "" {
		if t, err := time.Parse(time.RFC3339, since); err == nil {
			filter.Since = t
		} else if t, err := time.Parse("2006-01-02", since); err == nil {
			filter.Since = t
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	entries, err := s.chatAuditRepo.List(ctx, filter)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL",
			"chat audit query failed")
		return
	}

	out := AdminChatAuditListResponse{
		Entries: make([]AdminChatAuditEntryJSON, 0, len(entries)),
	}
	for _, e := range entries {
		out.Entries = append(out.Entries, AdminChatAuditEntryJSON{
			ID:                       e.ID,
			Timestamp:                e.Timestamp.UTC().Format(time.RFC3339Nano),
			ChatID:                   e.ChatID,
			UserID:                   e.UserID,
			ProjectID:                e.ProjectID,
			RoleUsed:                 e.RoleUsed,
			Model:                    e.Model,
			SystemPromptHash:         e.SystemPromptHash,
			UserMessage:              e.UserMessage,
			ToolCallsJSON:            e.ToolCallsJSON,
			Response:                 e.Response,
			Iterations:               e.Iterations,
			DurationMs:               e.DurationMs,
			CostUSD:                  e.CostUSD,
			HallucinationSignalsJSON: e.HallucinationSignalsJSON,
		})
	}
	respondJSON(w, http.StatusOK, out)
}
