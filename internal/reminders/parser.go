package reminders

// Natural-language reminder parser. Takes free-form operator text
// ("remind me in 3 hours to check the deploy", "every Monday at 9
// send the news digest") and turns it into a structured
// ReminderIntent the REST + CLI surfaces can commit to
// dispatcher_reminders.
//
// Supports both one-shot ("tomorrow at 9") and recurring inputs
// ("every Monday 9am", "daily until June 1"). Recurring intents
// carry a 5-field POSIX cron expression in CronExpr; the runner
// re-arms the row's fire_at after each delivery instead of
// going terminal. See cron.go for the validation surface and
// migration 67 for the storage shape.
//
// Failure modes are explicit: an unparseable input returns
// (nil, ErrUnparseable) so the caller can surface a clear "I
// couldn't tell what you meant" instead of guessing. A past
// fire_at returns (intent, ErrFireAtInPast) — the caller can
// either reject or auto-roll forward (the REST handler rolls to
// the next matching slot when the LLM produced a relative
// phrase like "tomorrow at 9" but the parse timestamp drifted).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/chat"
)

// Common parser errors. Wrapped so callers can errors.Is them.
var (
	// ErrUnparseable means the LLM couldn't extract a fire_at +
	// content from the input with sufficient confidence. The
	// caller surfaces the LLM's reason field to the operator
	// instead of guessing.
	ErrUnparseable = errors.New("reminder: input could not be parsed into a one-shot reminder")

	// ErrFireAtInPast means the LLM produced a timestamp earlier
	// than now. Caller decides whether to reject or auto-advance.
	ErrFireAtInPast = errors.New("reminder: parsed fire_at is in the past")

	// ErrLLMUnavailable wraps any chat-provider failure (HTTP
	// error, malformed response, context cancel). Caller falls
	// back to the manual form.
	ErrLLMUnavailable = errors.New("reminder: parser LLM is unavailable")
)

// IntentKind distinguishes one-shot reminders from recurring
// (cron-driven) ones. The runner consults this to decide whether
// to terminate the row after delivery or re-arm fire_at.
type IntentKind string

const (
	IntentKindOneShot   IntentKind = "one_shot"
	IntentKindRecurring IntentKind = "recurring"
)

// ReminderIntent is the structured shape the parser returns
// after a successful parse. The REST / CLI surfaces commit a
// dispatcher_reminders row from these fields; the LLM-only
// fields (Confidence, Reasoning) drive operator confirmation
// UX, not the row itself.
type ReminderIntent struct {
	// Kind discriminates one-shot vs recurring. One-shot rows
	// terminate after their single fire; recurring rows re-arm.
	Kind IntentKind

	// FireAt is the absolute UTC time of the NEXT fire. For
	// one-shot intents this is the single delivery time; for
	// recurring intents it's the next cron-computed slot,
	// already advanced past Now so the row inserts as pending
	// without an immediate-fire race.
	FireAt time.Time

	// Content is the message body to deliver. Set even when the
	// operator's input was just a time ("in 30 minutes"); the
	// parser substitutes a generic "Reminder" body in that case
	// and the confirmation flow lets the operator override.
	Content string

	// CronExpr carries a validated 5-field POSIX cron expression
	// when Kind=recurring; empty for one-shot intents. See
	// cron.go for the supported grammar — robfig's "standard"
	// parser, no seconds field, no @descriptors.
	CronExpr string

	// RecurrenceUntil bounds a recurring intent. nil = unbounded.
	// The runner falls through to a terminal MarkFired once
	// NextFireAt(CronExpr, now) > *RecurrenceUntil.
	RecurrenceUntil *time.Time

	// Confidence is the LLM's self-reported [0.0, 1.0] score.
	// Below ParseConfidenceThreshold the parser returns
	// ErrUnparseable rather than a guess.
	Confidence float64

	// Reasoning is the LLM's free-form explanation of how it
	// arrived at FireAt — shown in the confirmation prompt so
	// the operator can spot misinterpretation ("I parsed
	// 'tomorrow' as 2026-05-24, is that right?").
	Reasoning string
}

// IsRecurring is a convenience for callers that route on the
// intent's recurrence shape (REST handler, CLI confirmation,
// runner re-arm decision).
func (r *ReminderIntent) IsRecurring() bool {
	return r != nil && r.Kind == IntentKindRecurring
}

// ParseConfidenceThreshold is the minimum LLM-reported
// confidence below which we treat the parse as a miss. Tuned
// conservatively — the cost of a false-confident parse (wrong
// reminder fires at wrong time) far exceeds the cost of asking
// the operator to rephrase.
const ParseConfidenceThreshold = 0.6

// Parser is the dependency-injectable parser interface. The
// production impl is LLMParser; tests inject stubs that return
// fixed intents.
type Parser interface {
	Parse(ctx context.Context, input ParserInput) (*ReminderIntent, error)
}

// ParserInput bundles the operator's text plus the contextual
// hints the LLM needs to resolve relative phrasings ("tomorrow"
// → date depends on operator's timezone). All fields except
// Text are optional; defaults below apply.
type ParserInput struct {
	// Text is the free-form natural-language input from the
	// operator. Required.
	Text string

	// Now is the wall-clock reference the LLM resolves relative
	// times against. Zero-value defaults to time.Now().UTC() —
	// tests pin it for reproducibility.
	Now time.Time

	// OperatorTimezone is the IANA timezone (e.g. "Europe/Prague")
	// the operator's relative expressions are anchored to. Empty
	// falls back to UTC — most operators want their reminder to
	// fire at local time, not UTC, so production callers should
	// pass the operator's TZ when known (from operator profile,
	// later roadmap item).
	OperatorTimezone string

	// DefaultContentWhenEmpty is the fallback body when the
	// operator's input is pure time ("remind me in 30 minutes").
	// Empty defaults to "Reminder".
	DefaultContentWhenEmpty string
}

// LLMParser is the production parser. Wraps a chat.Provider with
// a strict JSON-schema prompt; falls back to ErrLLMUnavailable
// on any provider error.
type LLMParser struct {
	Provider chat.Provider
}

// NewLLMParser constructs a parser over the supplied chat
// provider. Returns nil when the provider is nil — callers
// nil-check and surface "natural-language scheduling disabled"
// rather than passing the nil parser around.
func NewLLMParser(p chat.Provider) *LLMParser {
	if p == nil {
		return nil
	}
	return &LLMParser{Provider: p}
}

// Parse runs the natural-language text through the LLM with the
// canonical extraction prompt + JSON schema. Returns
// (*ReminderIntent, nil) on success; one of the sentinel errors
// otherwise so the caller can decide whether to roll forward,
// reject, or fall back to the manual form.
func (p *LLMParser) Parse(ctx context.Context, in ParserInput) (*ReminderIntent, error) {
	if p == nil || p.Provider == nil {
		return nil, ErrLLMUnavailable
	}
	if strings.TrimSpace(in.Text) == "" {
		return nil, ErrUnparseable
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tz := in.OperatorTimezone
	if tz == "" {
		tz = "UTC"
	}
	defaultContent := in.DefaultContentWhenEmpty
	if defaultContent == "" {
		defaultContent = "Reminder"
	}

	prompt := buildParserPrompt(in.Text, now, tz, defaultContent)
	resp, err := p.Provider.Complete(ctx, []chat.Message{{Role: "user", Content: prompt}})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrLLMUnavailable, err)
	}
	if resp == nil || len(resp.Choices) == 0 || strings.TrimSpace(resp.Choices[0].Message.Content) == "" {
		return nil, fmt.Errorf("%w: empty response", ErrLLMUnavailable)
	}
	return decodeParserResponse(resp.Choices[0].Message.Content, now)
}

// buildParserPrompt assembles the strict-JSON prompt the LLM
// answers. Held verbatim here so the contract stays in one place
// — tests pin the schema by round-tripping through this prompt.
func buildParserPrompt(text string, now time.Time, tz, defaultContent string) string {
	return fmt.Sprintf(`You are a reminder-parsing assistant. Convert the operator's natural-language input into a structured reminder.

The operator's current time is %s (timezone: %s). Resolve all relative time expressions ("tomorrow", "in 3 hours", "next Monday") against this reference.

Operator input:
"""
%s
"""

Reply with ONLY a JSON object matching this exact schema (no surrounding text, no markdown fences):

{
  "kind": "one_shot" | "recurring" | "unparseable",
  "fire_at_utc": "<RFC3339 UTC timestamp; required for one_shot; omit or empty for recurring>",
  "cron_expr": "<5-field POSIX cron expression; required for recurring (e.g. '0 9 * * 1' for every Monday 09:00); empty for one_shot>",
  "recurrence_until_utc": "<RFC3339 UTC timestamp; optional end of the recurrence window; empty for unbounded>",
  "content": "<message body to deliver; if the operator only gave a time, use %q>",
  "confidence": <float 0.0-1.0; below 0.6 means you don't know>,
  "reasoning": "<one-sentence explanation of how you arrived at the schedule>"
}

Rules:
- kind="one_shot" for any single absolute or relative time ("tomorrow at 9", "in 3 hours", "next Tuesday 14:00").
- kind="recurring" for any "every X" / "daily" / "weekly" / "weekday morning" / cron-shaped input. Emit cron_expr ONLY in this case. Use a 5-field expression: "minute hour day-of-month month day-of-week". Day-of-week is 0-6 with 0=Sunday.
- kind="unparseable" if you cannot extract a schedule with confidence >= 0.6.
- fire_at_utc MUST be in the future relative to the operator's current time when kind=one_shot.
- Resolve the cron's wall-clock fields against the operator's timezone above when applicable (the daemon recomputes fire_at_utc from cron_expr at insert time).
- recurrence_until_utc is OPTIONAL; only set it if the operator named an end ("until June 1", "for the next two weeks").
- content must be a single short message (no newlines).
- Output JSON only, no commentary.`, now.Format(time.RFC3339), tz, text, defaultContent)
}

// decodeParserResponse interprets the LLM's JSON output. Pulled
// out as a separate function so tests can exercise the
// classification logic without standing up a real provider.
func decodeParserResponse(raw string, now time.Time) (*ReminderIntent, error) {
	var doc struct {
		Kind               string  `json:"kind"`
		FireAtUTC          string  `json:"fire_at_utc"`
		CronExpr           string  `json:"cron_expr"`
		RecurrenceUntilUTC string  `json:"recurrence_until_utc"`
		Content            string  `json:"content"`
		Confidence         float64 `json:"confidence"`
		Reasoning          string  `json:"reasoning"`
	}
	// Strip common markdown fence wrappers — some models stamp
	// ```json ... ``` despite the prompt. Cheap forgiveness.
	cleaned := stripJSONFences(raw)
	if err := json.Unmarshal([]byte(cleaned), &doc); err != nil {
		return nil, fmt.Errorf("%w: invalid JSON from parser LLM: %v", ErrUnparseable, err)
	}
	switch doc.Kind {
	case "unparseable":
		return nil, fmt.Errorf("%w: %s", ErrUnparseable, doc.Reasoning)
	case "one_shot", "recurring":
		// fall through to the body
	default:
		return nil, fmt.Errorf("%w: unknown kind %q", ErrUnparseable, doc.Kind)
	}
	if doc.Confidence < ParseConfidenceThreshold {
		return nil, fmt.Errorf("%w: confidence %.2f below threshold %.2f", ErrUnparseable, doc.Confidence, ParseConfidenceThreshold)
	}
	content := strings.TrimSpace(doc.Content)
	if content == "" {
		content = "Reminder"
	}
	intent := &ReminderIntent{
		Content:    content,
		Confidence: doc.Confidence,
		Reasoning:  doc.Reasoning,
	}

	if doc.Kind == "recurring" {
		intent.Kind = IntentKindRecurring
		cronExpr := strings.TrimSpace(doc.CronExpr)
		if cronExpr == "" {
			return nil, fmt.Errorf("%w: recurring intent missing cron_expr", ErrUnparseable)
		}
		if err := ValidateCronExpr(cronExpr); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUnparseable, err)
		}
		intent.CronExpr = cronExpr
		if strings.TrimSpace(doc.RecurrenceUntilUTC) != "" {
			until, err := time.Parse(time.RFC3339, doc.RecurrenceUntilUTC)
			if err != nil {
				return nil, fmt.Errorf("%w: bad recurrence_until_utc %q: %v", ErrUnparseable, doc.RecurrenceUntilUTC, err)
			}
			u := until.UTC()
			intent.RecurrenceUntil = &u
		}
		// First fire = next cron slot strictly after now. Lets
		// the storage layer commit the row in 'pending' with a
		// valid fire_at the heartbeat will pick up at the right
		// moment, no special-case insert path.
		next, err := NextFireAt(cronExpr, now)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUnparseable, err)
		}
		if intent.RecurrenceUntil != nil && next.After(*intent.RecurrenceUntil) {
			return nil, fmt.Errorf("%w: recurrence_until_utc precedes the first fire (cron=%q, until=%s)", ErrUnparseable, cronExpr, intent.RecurrenceUntil.Format(time.RFC3339))
		}
		intent.FireAt = next
		return intent, nil
	}

	// kind == "one_shot"
	intent.Kind = IntentKindOneShot
	fireAt, err := time.Parse(time.RFC3339, doc.FireAtUTC)
	if err != nil {
		return nil, fmt.Errorf("%w: bad fire_at_utc %q: %v", ErrUnparseable, doc.FireAtUTC, err)
	}
	intent.FireAt = fireAt.UTC()
	if !fireAt.After(now) {
		// Return the intent alongside the error so callers that
		// want to roll forward have something to work with.
		return intent, ErrFireAtInPast
	}
	return intent, nil
}

// stripJSONFences removes ```json ... ``` wrappers some models
// emit despite the "no markdown fences" prompt instruction.
// Idempotent on already-bare JSON.
func stripJSONFences(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```JSON")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}
