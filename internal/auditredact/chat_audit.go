package auditredact

import (
	"context"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secrets"
)

// ChatAudit decorates a persistence.ChatAuditRepository so everything the chat
// path persists is scanned and redacted BEFORE it is stored, at the one seam
// every writer passes through (chat-audit retention and redaction design §3).
//
// Two writers reach this store — the dispatcher's turn audit and the chat
// proxy's — and neither scanned. That is the same shape as the three
// tool-audit writers of which only one did, which is the incident the
// repository-boundary rule came from (2026-08-20). Every code path that
// rebuilds the repository set must re-apply this decoration.
//
// A chat system prompt carries whatever was rendered into it: skill text,
// tool-credential carryover, a secret pasted into a role prompt. The row
// carries what a person typed and what the model said back.
type ChatAudit struct {
	inner    persistence.ChatAuditRepository
	detector secrets.Detector
	audit    persistence.SecretRedactionAuditRepository
	logger   *zerolog.Logger
}

// NewChatAudit wraps inner. A nil detector is a pass-through, so CE paths and
// tests that never wire secret scanning still persist chat audit.
func NewChatAudit(inner persistence.ChatAuditRepository, detector secrets.Detector, audit persistence.SecretRedactionAuditRepository, logger *zerolog.Logger) *ChatAudit {
	return &ChatAudit{inner: inner, detector: detector, audit: audit, logger: logger}
}

// RedactText scans body and returns it with every finding replaced by a
// [REDACTED:type] marker, plus the per-type counts.
func (c *ChatAudit) RedactText(body string) (string, map[string]int) {
	if c == nil || c.detector == nil || body == "" {
		return body, nil
	}
	findings := c.detector.Scan([]byte(body))
	if len(findings) == 0 {
		return body, nil
	}
	return string(secrets.Redact([]byte(body), findings)), secrets.CountByType(findings)
}

// SavePrompt redacts, then stores; the hash the inner repository returns is of
// what was stored, because it hashes the body it is handed (design §3.1).
func (c *ChatAudit) SavePrompt(ctx context.Context, body string) (string, error) {
	clean, counts := c.RedactText(body)
	if len(counts) > 0 && c.logger != nil {
		c.logger.Warn().Interface("by_type", counts).
			Str("checkpoint", secrets.CheckpointToolAudit).
			Msg("secrets: chat system prompt scanned — redacting before persist")
	}
	c.record(ctx, counts)
	return c.inner.SavePrompt(ctx, clean)
}

// Insert redacts the row's free text before it is stored.
//
// Only the three free-text fields are touched. The identity fields — ID,
// ChatID, UserID, ProjectID, RoleUsed, Model — are load-bearing for origin
// resolution (chatorigin reads tasks.chat_turn_id against this table to decide
// where a finished task's result is delivered), and a redaction pass that
// rewrote one would break delivery rather than protect anything.
func (c *ChatAudit) Insert(ctx context.Context, entry *persistence.ChatAuditEntry) error {
	if entry == nil || c.detector == nil {
		return c.inner.Insert(ctx, entry)
	}
	total := map[string]int{}
	redact := func(field *string) {
		clean, counts := c.RedactText(*field)
		*field = clean
		for k, n := range counts {
			total[k] += n
		}
	}
	// Redact a COPY, never the caller's struct — the same rule, and the same
	// reasoning, as Repo.Log's `clean := *entry` above. A decorator that edits
	// its argument makes the redaction visible at a distance: a caller that
	// retries the write, or logs the entry afterwards, sees a struct it did
	// not write. Shallow is sufficient ONLY while every field redacted here is
	// a value type — all three are strings, so replacing them aliases nothing
	// back. A slice or map field carrying scannable content would need a deep
	// copy.
	clean := *entry
	redact(&clean.UserMessage)
	redact(&clean.Response)
	redact(&clean.ToolCallsJSON)
	if len(total) > 0 && c.logger != nil {
		c.logger.Warn().Str("chat_id", clean.ChatID).Interface("by_type", total).
			Str("checkpoint", secrets.CheckpointToolAudit).
			Msg("secrets: chat audit row scanned — redacting before persist")
	}
	c.record(ctx, total)
	return c.inner.Insert(ctx, &clean)
}

// GetPrompt delegates unchanged: bodies are stored redacted.
func (c *ChatAudit) GetPrompt(ctx context.Context, hash string) (string, error) {
	return c.inner.GetPrompt(ctx, hash)
}

// List delegates unchanged.
func (c *ChatAudit) List(ctx context.Context, filter persistence.ChatAuditFilter) ([]*persistence.ChatAuditEntry, error) {
	return c.inner.List(ctx, filter)
}

// GetByID delegates unchanged.
func (c *ChatAudit) GetByID(ctx context.Context, id string) (*persistence.ChatAuditEntry, error) {
	return c.inner.GetByID(ctx, id)
}

// GetChatAuditsByTurnIDs delegates unchanged.
func (c *ChatAudit) GetChatAuditsByTurnIDs(ctx context.Context, turnIDs []string) (map[string]persistence.ChatAuditEntry, error) {
	return c.inner.GetChatAuditsByTurnIDs(ctx, turnIDs)
}

// Inner returns the wrapped repository — the idempotency check the container's
// rebuild path needs, mirroring Repo.Inner.
func (c *ChatAudit) Inner() persistence.ChatAuditRepository { return c.inner }

func (c *ChatAudit) record(ctx context.Context, counts map[string]int) {
	if c.audit == nil || len(counts) == 0 {
		return
	}
	events := make([]persistence.SecretRedactionEvent, 0, len(counts))
	for findingType, n := range counts {
		if n > 0 {
			events = append(events, persistence.SecretRedactionEvent{
				Checkpoint: secrets.CheckpointToolAudit, FindingType: findingType, Count: n, Source: "live",
			})
		}
	}
	if len(events) == 0 {
		return
	}
	if err := c.audit.Record(ctx, events); err != nil && c.logger != nil {
		c.logger.Warn().Err(err).Msg("secrets: recording chat-audit redaction events failed")
	}
}
