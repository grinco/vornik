package reminders

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/chat"
)

// stubProvider returns a fixed string from Complete + records
// the last prompt for assertions. The non-Complete methods
// satisfy chat.Provider but aren't exercised by the parser.
type stubProvider struct {
	response   string
	err        error
	lastPrompt string
}

func (s *stubProvider) Complete(_ context.Context, messages []chat.Message) (*chat.ChatResponse, error) {
	if len(messages) > 0 {
		s.lastPrompt = messages[len(messages)-1].Content
	}
	if s.err != nil {
		return nil, s.err
	}
	resp := &chat.ChatResponse{}
	resp.Choices = append(resp.Choices, struct {
		Index        int          `json:"index"`
		Message      chat.Message `json:"message"`
		FinishReason string       `json:"finish_reason"`
	}{Message: chat.Message{Role: "assistant", Content: s.response}, FinishReason: "stop"})
	return resp, nil
}

func (s *stubProvider) CompleteWithTools(_ context.Context, _ []chat.Message, _ []chat.Tool) (*chat.ChatResponse, error) {
	return nil, nil
}

func (s *stubProvider) CompleteWithToolsStream(_ context.Context, _ []chat.Message, _ []chat.Tool, _ chat.StreamCallback) (*chat.ChatResponse, error) {
	return nil, nil
}

func (s *stubProvider) Model() string              { return "stub" }
func (s *stubProvider) SetMetrics(_ *chat.Metrics) {}

// TestParse_HappyPath: a well-formed LLM response with a future
// fire_at + good confidence yields a populated intent.
func TestParse_HappyPath(t *testing.T) {
	now := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	resp := `{
  "kind": "one_shot",
  "fire_at_utc": "2026-05-24T15:00:00Z",
  "content": "check the deploy",
  "confidence": 0.92,
  "reasoning": "operator said 'in 3 hours'; 12:00 UTC + 3h = 15:00 UTC"
}`
	p := &LLMParser{Provider: &stubProvider{response: resp}}
	got, err := p.Parse(context.Background(), ParserInput{Text: "in 3 hours check the deploy", Now: now})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Content != "check the deploy" {
		t.Errorf("Content = %q, want 'check the deploy'", got.Content)
	}
	want := time.Date(2026, 5, 24, 15, 0, 0, 0, time.UTC)
	if !got.FireAt.Equal(want) {
		t.Errorf("FireAt = %v, want %v", got.FireAt, want)
	}
	if got.Confidence < ParseConfidenceThreshold {
		t.Errorf("Confidence = %v, want >= %v", got.Confidence, ParseConfidenceThreshold)
	}
}

// TestParse_RecurringHappyPath: an LLM response naming a 5-field
// cron + a content message yields a Kind=recurring intent with
// FireAt advanced to the next slot.
func TestParse_RecurringHappyPath(t *testing.T) {
	// 2026-05-24 16:00 UTC is a Sunday; next Monday 09:00 UTC
	// is 2026-05-25 09:00.
	now := time.Date(2026, 5, 24, 16, 0, 0, 0, time.UTC)
	resp := `{
  "kind": "recurring",
  "cron_expr": "0 9 * * 1",
  "content": "send the news digest",
  "confidence": 0.95,
  "reasoning": "operator said 'every Monday at 9'"
}`
	p := &LLMParser{Provider: &stubProvider{response: resp}}
	got, err := p.Parse(context.Background(), ParserInput{Text: "every Monday at 9 send the news digest", Now: now})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Kind != IntentKindRecurring {
		t.Errorf("Kind = %q, want recurring", got.Kind)
	}
	if got.CronExpr != "0 9 * * 1" {
		t.Errorf("CronExpr = %q, want '0 9 * * 1'", got.CronExpr)
	}
	wantFireAt := time.Date(2026, 5, 25, 9, 0, 0, 0, time.UTC)
	if !got.FireAt.Equal(wantFireAt) {
		t.Errorf("FireAt = %s, want %s (next cron slot after now)", got.FireAt, wantFireAt)
	}
	if got.Content != "send the news digest" {
		t.Errorf("Content = %q", got.Content)
	}
}

// TestParse_RecurringWithBound: "every day until June 1" produces
// a cron + recurrence_until pair.
func TestParse_RecurringWithBound(t *testing.T) {
	now := time.Date(2026, 5, 24, 16, 0, 0, 0, time.UTC)
	resp := `{
  "kind": "recurring",
  "cron_expr": "0 9 * * *",
  "recurrence_until_utc": "2026-06-01T00:00:00Z",
  "content": "morning check-in",
  "confidence": 0.9,
  "reasoning": "every day until June 1"
}`
	p := &LLMParser{Provider: &stubProvider{response: resp}}
	got, err := p.Parse(context.Background(), ParserInput{Text: "every day until June 1 morning check-in", Now: now})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.RecurrenceUntil == nil {
		t.Fatalf("RecurrenceUntil should be populated")
	}
	wantUntil := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if !got.RecurrenceUntil.Equal(wantUntil) {
		t.Errorf("RecurrenceUntil = %s, want %s", got.RecurrenceUntil, wantUntil)
	}
}

// TestParse_RecurringMissingCronRejected: kind=recurring without
// a cron_expr is a malformed LLM response; surface ErrUnparseable
// so the caller can ask the operator to rephrase.
func TestParse_RecurringMissingCronRejected(t *testing.T) {
	resp := `{
  "kind": "recurring",
  "cron_expr": "",
  "content": "x",
  "confidence": 0.9,
  "reasoning": "model forgot the cron"
}`
	p := &LLMParser{Provider: &stubProvider{response: resp}}
	_, err := p.Parse(context.Background(), ParserInput{Text: "every monday do the thing"})
	if !errors.Is(err, ErrUnparseable) {
		t.Errorf("err = %v, want ErrUnparseable on empty cron_expr", err)
	}
}

// TestParse_RecurringInvalidCronRejected: a garbage cron_expr
// must not commit; the validator surfaces ErrInvalidCron and the
// parser wraps it as ErrUnparseable for the caller's switch.
func TestParse_RecurringInvalidCronRejected(t *testing.T) {
	resp := `{
  "kind": "recurring",
  "cron_expr": "not a cron",
  "content": "x",
  "confidence": 0.9,
  "reasoning": "model hallucinated"
}`
	p := &LLMParser{Provider: &stubProvider{response: resp}}
	_, err := p.Parse(context.Background(), ParserInput{Text: "every X do Y"})
	if !errors.Is(err, ErrUnparseable) {
		t.Errorf("err = %v, want ErrUnparseable on invalid cron", err)
	}
}

// TestParse_RecurringBoundBeforeFirstFireRejected: a bound that
// already passed by the first scheduled fire is incoherent; the
// parser refuses so the caller doesn't commit a row that never
// fires.
func TestParse_RecurringBoundBeforeFirstFireRejected(t *testing.T) {
	now := time.Date(2026, 5, 24, 16, 0, 0, 0, time.UTC)
	resp := `{
  "kind": "recurring",
  "cron_expr": "0 9 * * 1",
  "recurrence_until_utc": "2026-05-24T17:00:00Z",
  "content": "x",
  "confidence": 0.9,
  "reasoning": "until before next monday"
}`
	p := &LLMParser{Provider: &stubProvider{response: resp}}
	_, err := p.Parse(context.Background(), ParserInput{Text: "every monday until 5pm today", Now: now})
	if !errors.Is(err, ErrUnparseable) {
		t.Errorf("err = %v, want ErrUnparseable when bound precedes first fire", err)
	}
}

// TestParse_OneShotIntentKindSet pins that a one-shot response
// stamps Kind=one_shot (regression guard for the new field).
func TestParse_OneShotIntentKindSet(t *testing.T) {
	now := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	resp := `{
  "kind": "one_shot",
  "fire_at_utc": "2026-05-24T15:00:00Z",
  "content": "x",
  "confidence": 0.9,
  "reasoning": "r"
}`
	p := &LLMParser{Provider: &stubProvider{response: resp}}
	got, err := p.Parse(context.Background(), ParserInput{Text: "in 3 hours", Now: now})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Kind != IntentKindOneShot {
		t.Errorf("Kind = %q, want one_shot", got.Kind)
	}
	if got.CronExpr != "" {
		t.Errorf("CronExpr = %q, want empty for one-shot", got.CronExpr)
	}
	if got.IsRecurring() {
		t.Errorf("IsRecurring should be false for one-shot")
	}
}

// TestParse_LowConfidenceRejected: a low-confidence parse is
// worse than no parse — we'd commit the wrong reminder at the
// wrong time. The threshold gate must trip.
func TestParse_LowConfidenceRejected(t *testing.T) {
	resp := `{
  "kind": "one_shot",
  "fire_at_utc": "2026-12-31T23:59:00Z",
  "content": "best guess",
  "confidence": 0.4,
  "reasoning": "very ambiguous"
}`
	p := &LLMParser{Provider: &stubProvider{response: resp}}
	_, err := p.Parse(context.Background(), ParserInput{Text: "do the thing some time"})
	if !errors.Is(err, ErrUnparseable) {
		t.Errorf("err = %v, want ErrUnparseable on low-confidence", err)
	}
}

// TestParse_PastFireAtReturnsIntentAndError: callers may want
// to roll forward (e.g. "tomorrow at 9" parsed against a stale
// `now`). Return both the intent (so they can introspect) AND
// the error (so they know to validate or auto-advance).
func TestParse_PastFireAtReturnsIntentAndError(t *testing.T) {
	now := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	resp := `{
  "kind": "one_shot",
  "fire_at_utc": "2026-05-24T11:00:00Z",
  "content": "x",
  "confidence": 0.9,
  "reasoning": "ran an hour late"
}`
	p := &LLMParser{Provider: &stubProvider{response: resp}}
	got, err := p.Parse(context.Background(), ParserInput{Text: "remind me at 11", Now: now})
	if !errors.Is(err, ErrFireAtInPast) {
		t.Errorf("err = %v, want ErrFireAtInPast", err)
	}
	if got == nil {
		t.Errorf("intent should be populated even on past-fire_at so caller can decide to roll forward")
	}
}

// TestParse_EmptyContentFallsBackToDefault: pure-time inputs
// ("in 30 minutes") have no content; the parser substitutes the
// supplied default so the dispatcher_reminders row has something
// to deliver.
func TestParse_EmptyContentFallsBackToDefault(t *testing.T) {
	now := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	resp := `{
  "kind": "one_shot",
  "fire_at_utc": "2026-05-24T12:30:00Z",
  "content": "",
  "confidence": 0.9,
  "reasoning": "just a time"
}`
	p := &LLMParser{Provider: &stubProvider{response: resp}}
	got, err := p.Parse(context.Background(), ParserInput{Text: "in 30 minutes", Now: now, DefaultContentWhenEmpty: "Reminder"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Content != "Reminder" {
		t.Errorf("Content = %q, want fallback 'Reminder'", got.Content)
	}
}

// TestParse_MarkdownFenceTolerance: some models stamp JSON in
// triple-backtick fences despite the instruction not to. The
// stripper handles them so a real-world response doesn't break
// the parse.
func TestParse_MarkdownFenceTolerance(t *testing.T) {
	now := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	resp := "```json\n" + `{"kind":"one_shot","fire_at_utc":"2026-05-24T13:00:00Z","content":"x","confidence":0.9,"reasoning":"r"}` + "\n```"
	p := &LLMParser{Provider: &stubProvider{response: resp}}
	got, err := p.Parse(context.Background(), ParserInput{Text: "in 1 hour", Now: now})
	if err != nil {
		t.Fatalf("Parse: %v (raw=%q)", err, resp)
	}
	if got.Content != "x" {
		t.Errorf("Content lost in fence-strip: %q", got.Content)
	}
}

// TestParse_LLMErrorWrapped: a provider failure must surface as
// ErrLLMUnavailable so the caller can fall back to the manual
// form. errors.Is must work for the sentinel.
func TestParse_LLMErrorWrapped(t *testing.T) {
	p := &LLMParser{Provider: &stubProvider{err: errors.New("connection refused")}}
	_, err := p.Parse(context.Background(), ParserInput{Text: "anything"})
	if !errors.Is(err, ErrLLMUnavailable) {
		t.Errorf("err = %v, want wrapped ErrLLMUnavailable", err)
	}
}

// TestParse_EmptyInput: defensive — blank input shouldn't even
// hit the LLM; it would burn tokens on nothing.
func TestParse_EmptyInput(t *testing.T) {
	called := false
	p := &LLMParser{Provider: &stubProvider{response: `{"kind":"one_shot","fire_at_utc":"2026-05-24T13:00:00Z","content":"x","confidence":0.9,"reasoning":""}`}}
	p.Provider = stubCallTracker{inner: p.Provider, called: &called}
	_, err := p.Parse(context.Background(), ParserInput{Text: "   "})
	if !errors.Is(err, ErrUnparseable) {
		t.Errorf("err = %v, want ErrUnparseable for blank input", err)
	}
	if called {
		t.Errorf("blank input should not invoke the LLM")
	}
}

// stubCallTracker wraps a provider + flips called to true on
// any Complete call. Lets TestParse_EmptyInput assert no token
// burn on garbage input.
type stubCallTracker struct {
	inner  chat.Provider
	called *bool
}

func (s stubCallTracker) Complete(ctx context.Context, m []chat.Message) (*chat.ChatResponse, error) {
	*s.called = true
	return s.inner.Complete(ctx, m)
}
func (s stubCallTracker) CompleteWithTools(ctx context.Context, m []chat.Message, t []chat.Tool) (*chat.ChatResponse, error) {
	return s.inner.CompleteWithTools(ctx, m, t)
}
func (s stubCallTracker) CompleteWithToolsStream(ctx context.Context, m []chat.Message, t []chat.Tool, cb chat.StreamCallback) (*chat.ChatResponse, error) {
	return s.inner.CompleteWithToolsStream(ctx, m, t, cb)
}
func (s stubCallTracker) Model() string              { return s.inner.Model() }
func (s stubCallTracker) SetMetrics(m *chat.Metrics) {}

// TestParse_NilProvider returns ErrLLMUnavailable rather than
// panicking. Lets callers construct the parser eagerly and check
// the error path uniformly.
func TestParse_NilProvider(t *testing.T) {
	var p *LLMParser
	_, err := p.Parse(context.Background(), ParserInput{Text: "anything"})
	if !errors.Is(err, ErrLLMUnavailable) {
		t.Errorf("err = %v, want ErrLLMUnavailable", err)
	}
}

// TestParse_TimezoneInPrompt confirms the operator's TZ flows
// into the LLM prompt — a parse drift here would silently use
// UTC for everyone and surprise non-UTC operators with reminders
// firing at the wrong wall-clock time.
func TestParse_TimezoneInPrompt(t *testing.T) {
	stub := &stubProvider{response: `{"kind":"one_shot","fire_at_utc":"2026-05-24T07:00:00Z","content":"x","confidence":0.9,"reasoning":""}`}
	p := &LLMParser{Provider: stub}
	_, _ = p.Parse(context.Background(), ParserInput{
		Text:             "tomorrow at 9",
		Now:              time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC),
		OperatorTimezone: "Europe/Prague",
	})
	if !strings.Contains(stub.lastPrompt, "Europe/Prague") {
		t.Errorf("prompt missing operator timezone; lastPrompt does not contain 'Europe/Prague'")
	}
}

// TestParse_PromptContainsOperatorText pins the actual operator
// input flows into the prompt verbatim. Without this guard a
// future refactor that drops the input would silently produce
// the wrong reminder.
func TestParse_PromptContainsOperatorText(t *testing.T) {
	stub := &stubProvider{response: `{"kind":"one_shot","fire_at_utc":"2026-05-24T13:00:00Z","content":"x","confidence":0.9,"reasoning":""}`}
	p := &LLMParser{Provider: stub}
	const operatorText = "call mom at 3pm tomorrow"
	_, _ = p.Parse(context.Background(), ParserInput{Text: operatorText, Now: time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)})
	if !strings.Contains(stub.lastPrompt, operatorText) {
		t.Errorf("operator text not in prompt; lastPrompt=%q", stub.lastPrompt[:min(200, len(stub.lastPrompt))])
	}
}

// TestDecodeParserResponse_GarbageJSONReturnsUnparseable pins
// the decoder's defensive path. Lifted out of Parse so future
// refactors can exercise it without standing up the provider.
func TestDecodeParserResponse_GarbageJSONReturnsUnparseable(t *testing.T) {
	now := time.Now()
	_, err := decodeParserResponse("this is not json at all", now)
	if !errors.Is(err, ErrUnparseable) {
		t.Errorf("err = %v, want ErrUnparseable on garbage JSON", err)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestNewLLMParser_NilProviderReturnsNil pins the constructor's
// defensive branch. Callers that pass a nil provider get nil
// back; the Parse caller then surfaces ErrLLMUnavailable via
// the nil-receiver path.
func TestNewLLMParser_NilProviderReturnsNil(t *testing.T) {
	if got := NewLLMParser(nil); got != nil {
		t.Errorf("NewLLMParser(nil) = %v, want nil", got)
	}
}

// TestNewLLMParser_NonNilProviderReturnsParser confirms the
// happy-path constructor.
func TestNewLLMParser_NonNilProviderReturnsParser(t *testing.T) {
	p := NewLLMParser(&stubProvider{response: ""})
	if p == nil {
		t.Fatalf("NewLLMParser(non-nil) returned nil")
	}
	if p.Provider == nil {
		t.Errorf("Provider field not set")
	}
}
