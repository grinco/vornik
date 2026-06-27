package dispatcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/hallucination"
	"vornik.io/vornik/internal/persistence"
)

// chatAuditTurn collects the per-turn signal during Process /
// ProcessStreaming and writes one chat_audit_log row at the end.
// nil-safe: when a.chatAuditRepo is unset every method on this
// struct is a no-op.
type chatAuditTurn struct {
	agent        *Agent
	startedAt    time.Time
	systemPrompt string
	userMessage  string
	model        string
	roleUsed     string
	toolCalls    []chatAuditToolCall
	iterations   int
	tokensIn     int
	tokensOut    int
	costUSD      float64
	// id is pre-allocated at turn start so tasks created mid-turn
	// (via the create_task tool) can stamp the same id on
	// tasks.chat_turn_id BEFORE the audit row exists. Insert at the
	// end of the turn uses this id rather than generating a fresh
	// one — keeping the task → audit-row link stable even when the
	// audit insert later fails (e.g. UTF-8 truncation).
	id string
	// hallucinationSignals accumulates EVERY scan the dispatcher
	// runs against the reply candidate, including the initial scan
	// + any retry scan. Persisted as JSON on the audit row when
	// non-empty so the /ui/admin/chat-audit page can surface what
	// the detector flagged. Empty (the common case) means no
	// detector fired on this turn.
	hallucinationSignals []hallucination.Signal
}

// chatTurnIDContextKey is the context key under which the dispatcher
// stashes the current turn's chat_audit_log.id. Unexported — readers
// use ChatTurnIDFromContext.
type chatTurnIDContextKey struct{}

// WithChatTurnID returns a derived context carrying the given chat
// turn id. No-op when id is empty so non-chat call paths (API
// retention, autonomous scheduler) don't accidentally propagate a
// stale value.
func WithChatTurnID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, chatTurnIDContextKey{}, id)
}

// ChatTurnIDFromContext returns the chat_audit_log.id stashed by
// WithChatTurnID, or empty string when this code path isn't running
// inside a dispatcher turn (API callers, autonomous tasks).
func ChatTurnIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(chatTurnIDContextKey{}).(string)
	return v
}

// chatAuditToolCall is the serialisable per-tool-call summary.
// Args + Result are truncated to keep row size bounded — operators
// who need the full payload can correlate by ts+chat_id with
// tool_audit_log (full) when that's wired for chat surfaces.
type chatAuditToolCall struct {
	Name   string `json:"name"`
	Args   string `json:"args,omitempty"`
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

func newChatAuditTurn(a *Agent) *chatAuditTurn {
	if a == nil || a.chatAuditRepo == nil {
		return nil
	}
	return &chatAuditTurn{
		agent:     a,
		startedAt: time.Now().UTC(),
		id:        persistence.GenerateID("chat"),
	}
}

// captureRequest stamps the request-side fields (system prompt,
// user message, role label). Called once per turn after the agent
// has resolved the prompt.
func (t *chatAuditTurn) captureRequest(systemPrompt, userMessage, roleUsed string) {
	if t == nil {
		return
	}
	t.systemPrompt = systemPrompt
	t.userMessage = userMessage
	t.roleUsed = roleUsed
}

// recordIteration bumps the iteration counter + accrues token /
// cost totals. Called after every LLM round-trip.
func (t *chatAuditTurn) recordIteration(resp *chat.ChatResponse, model string, costUSD float64) {
	if t == nil || resp == nil {
		return
	}
	t.iterations++
	if model != "" {
		t.model = model
	}
	t.tokensIn += resp.Usage.PromptTokens
	t.tokensOut += resp.Usage.CompletionTokens
	t.costUSD += costUSD
}

// recordHallucinationSignals appends one scan's signals to the
// turn's running list. The dispatcher's two scan sites (initial +
// retry) both call this so the audit row carries the FULL signal
// trace, not just the last scan. Nil-safe + empty-safe; called
// unconditionally from agent_process.go so a future addition of a
// third scan site doesn't need to remember to plumb through here.
func (t *chatAuditTurn) recordHallucinationSignals(signals []hallucination.Signal) {
	if t == nil || len(signals) == 0 {
		return
	}
	t.hallucinationSignals = append(t.hallucinationSignals, signals...)
}

// recordToolCall captures one tool invocation's name + truncated
// args/result/error. Called from the dispatcher's tool loop.
func (t *chatAuditTurn) recordToolCall(name, args, result, errStr string) {
	if t == nil {
		return
	}
	t.toolCalls = append(t.toolCalls, chatAuditToolCall{
		Name:   name,
		Args:   truncateForAudit(args, 1024),
		Result: truncateForAudit(result, 1024),
		Error:  truncateForAudit(errStr, 512),
	})
}

// finish persists the audit row. Pass the Request (for chat_id +
// project) and Result (for the final response text). Safe to call
// even when the turn errored; the response field carries the
// error string in that case so operators see what went wrong.
func (t *chatAuditTurn) finish(ctx context.Context, req Request, result Result) {
	if t == nil || t.agent == nil || t.agent.chatAuditRepo == nil {
		return
	}
	// Persist the prompt body keyed by its sha256 hex digest. The
	// row references the hash so the prompt body is stored once
	// across every turn that used it.
	hash := ""
	if t.systemPrompt != "" {
		sum := sha256.Sum256([]byte(t.systemPrompt))
		hash = hex.EncodeToString(sum[:])
		_ = t.agent.chatAuditRepo.SavePrompt(ctx, hash, t.systemPrompt)
	}

	responseText := result.Text
	if result.Err != nil && responseText == "" {
		responseText = "error: " + result.Err.Error()
	}

	toolCallsJSON, _ := json.Marshal(t.toolCalls)

	// Hallucination signals — JSON-encode the accumulated list when
	// non-empty so the audit row carries the detector trace. Empty
	// list collapses to "" rather than "[]" because the column
	// renderer (chat_audit_log.hallucination_signals_json) uses
	// presence/absence as the badge gate; "[]" would render as a
	// false-positive badge ("signals fired" with zero rows).
	hallucinationJSON := ""
	if len(t.hallucinationSignals) > 0 {
		if b, err := json.Marshal(t.hallucinationSignals); err == nil {
			hallucinationJSON = string(b)
		}
	}

	entry := &persistence.ChatAuditEntry{
		ID:                       t.id,
		Timestamp:                time.Now().UTC(),
		ChatID:                   resolveChatID(req),
		ProjectID:                req.Project,
		RoleUsed:                 t.roleUsed,
		Model:                    t.model,
		SystemPromptHash:         hash,
		UserMessage:              truncateForAudit(t.userMessage, 500),
		ToolCallsJSON:            string(toolCallsJSON),
		Response:                 truncateForAudit(responseText, 500),
		Iterations:               t.iterations,
		DurationMs:               time.Since(t.startedAt).Milliseconds(),
		CostUSD:                  t.costUSD,
		HallucinationSignalsJSON: hallucinationJSON,
	}
	// Best-effort: a failed audit insert must not kill the chat
	// reply. Log via the logger we already have so the operator
	// still sees the miss in journald.
	if err := t.agent.chatAuditRepo.Insert(ctx, entry); err != nil {
		t.agent.logger.Warn().
			Err(err).
			Str("chat_id", entry.ChatID).
			Msg("chat_audit: insert failed")
	}
}

// truncateForAudit caps long strings to a byte limit, suffixing the
// chop with "…(truncated)" so operators can spot truncation. Empty
// stays empty. Multi-byte sequences are NEVER split on a non-rune
// boundary: PostgreSQL's UTF-8 encoding rejects invalid byte
// sequences and the chat_audit_log insert dies with
// "invalid byte sequence for encoding UTF8: 0xf0 0x9f 0x93 0xe2"
// when an emoji lands on the cut (live evidence: T-0918 follow-up,
// 2026-05-21 17:20:51 — the audit row was dropped entirely, so
// the dispatcher's reply existed in Telegram with no auditable
// trace). After this fix the cut walks back to the previous rune
// boundary before appending the suffix.
func truncateForAudit(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	const suffix = "…(truncated)"
	if limit <= len(suffix) {
		return safeUTF8Prefix(s, limit)
	}
	return safeUTF8Prefix(s, limit-len(suffix)) + suffix
}

// safeUTF8Prefix returns the longest prefix of s that contains
// whole runes and is at most `n` bytes. Walks runes from the start
// rather than backing up from a byte index because the latter has
// to validate each byte's continuation status; rune iteration uses
// the same machinery PostgreSQL would reject on, but cleanly.
func safeUTF8Prefix(s string, n int) string {
	if n <= 0 || s == "" {
		return ""
	}
	if n >= len(s) {
		return s
	}
	used := 0
	for _, r := range s {
		sz := utf8.RuneLen(r)
		if sz < 0 {
			// Invalid rune in source string — bail with what we
			// have rather than emitting garbage.
			break
		}
		if used+sz > n {
			break
		}
		used += sz
	}
	return s[:used]
}

// modelFromResponse extracts the model identifier from a chat
// response. The provider may surface it as Model on the response;
// some return empty when the upstream didn't echo it back.
func modelFromResponse(resp *chat.ChatResponse) string {
	if resp == nil {
		return ""
	}
	return resp.Model
}

// resolveChatID picks the most-specific identifier for the audit
// row's chat_id column. Three buckets, in priority order:
//
//  1. Telegram-style int64 ChatID — kept as a bare numeric string
//     for back-compat with existing UI filters and operator
//     muscle memory.
//  2. OriginatingChannel + OriginatingSessionID — formatted as
//     "<channel>:<sessionID>" (e.g. "email:<thread-msgid>" or
//     "web-chat:<cookie-uuid>"). Set by ChannelReceiver in commit
//     ebc304e for the per-channel followup wiring; reusing it
//     here gives every non-Telegram conversation a meaningful
//     chat_id without a new field on Request.
//  3. Empty string — synthesised internal turn (autonomy loop,
//     retry-from-step, post-mortem builder). Same as pre-fix
//     behaviour for these paths.
//
// Origin: 2026-05-21 defect report — email chats showed an empty
// chat_id column so operators couldn't tell which inbound thread
// produced which audit row.
func resolveChatID(req Request) string {
	if req.ChatID != 0 {
		return formatChatID(req.ChatID)
	}
	if req.OriginatingChannel != "" && req.OriginatingSessionID != "" {
		return req.OriginatingChannel + ":" + req.OriginatingSessionID
	}
	return ""
}

// formatChatID stringifies the chat ID for storage. Telegram chat
// IDs are int64; future channels may use opaque strings, in which
// case the dispatcher will need to accept a string ID directly.
func formatChatID(id int64) string {
	if id == 0 {
		return ""
	}
	var b strings.Builder
	if id < 0 {
		b.WriteString("-")
		id = -id
	}
	// Avoid strconv for the same reason fmt.Sprintf would work:
	// keep this file's import list narrow.
	digits := make([]byte, 0, 20)
	for id > 0 {
		digits = append(digits, byte('0'+id%10))
		id /= 10
	}
	if len(digits) == 0 {
		digits = []byte{'0'}
	}
	for i := len(digits) - 1; i >= 0; i-- {
		b.WriteByte(digits[i])
	}
	return b.String()
}
