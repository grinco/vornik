package graph

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"vornik.io/vornik/internal/chat"
)

// fakeProvider returns scripted responses; nil entry yields a
// canned ChatResponse with whatever Content the caller stamped.
type fakeProvider struct {
	calls   atomic.Int32
	replies []reply
}

type reply struct {
	content string
	err     error
	// finishReason overrides the provider's finish_reason. Empty = "stop".
	// Scriptable since 2026-08-05: every fixture hardcoded "stop", so the
	// truncation path — the dominant real-world failure — was unreachable
	// from any test.
	finishReason string
}

func (f *fakeProvider) Complete(_ context.Context, _ []chat.Message) (*chat.ChatResponse, error) {
	idx := int(f.calls.Add(1)) - 1
	if idx >= len(f.replies) {
		return &chat.ChatResponse{}, nil
	}
	r := f.replies[idx]
	if r.err != nil {
		return nil, r.err
	}
	resp := &chat.ChatResponse{Model: "fake"}
	resp.Choices = append(resp.Choices, struct {
		Index        int          `json:"index"`
		Message      chat.Message `json:"message"`
		FinishReason string       `json:"finish_reason"`
	}{Message: chat.Message{Role: "assistant", Content: r.content}, FinishReason: finishReasonOr(r.finishReason)})
	// Stamp non-zero usage so cost-tracking tests downstream see
	// realistic shapes. Production providers always populate this;
	// pre-fix the fake returned zeros and recordStageUsage's
	// zero-token guard silently skipped every test.
	resp.Usage.PromptTokens = 100
	resp.Usage.CompletionTokens = 50
	resp.Usage.TotalTokens = 150
	return resp, nil
}

// finishReasonOr defaults a scripted finish_reason to "stop".
func finishReasonOr(s string) string {
	if s == "" {
		return "stop"
	}
	return s
}

func (f *fakeProvider) CompleteWithTools(context.Context, []chat.Message, []chat.Tool) (*chat.ChatResponse, error) {
	panic("not used")
}
func (f *fakeProvider) CompleteWithToolsStream(context.Context, []chat.Message, []chat.Tool, chat.StreamCallback) (*chat.ChatResponse, error) {
	panic("not used")
}
func (f *fakeProvider) Model() string            { return "fake" }
func (f *fakeProvider) SetMetrics(*chat.Metrics) {}

func TestTruncateUTF8BytesDoesNotSplitRune(t *testing.T) {
	got := truncateUTF8Bytes("abc€def", 5)
	if got != "abc" {
		t.Fatalf("got %q, want abc", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("truncated string is invalid UTF-8: %q", got)
	}
}

// capturingProvider records the most recent user-message content
// so tests can assert on the exact prompt body the stage built.
// Unlike fakeProvider it returns a single canned reply forever.
type capturingProvider struct {
	reply   string
	userMsg string
}

func (c *capturingProvider) Complete(_ context.Context, msgs []chat.Message) (*chat.ChatResponse, error) {
	for _, m := range msgs {
		if m.Role == "user" {
			c.userMsg = m.Content
		}
	}
	resp := &chat.ChatResponse{Model: "fake"}
	resp.Choices = append(resp.Choices, struct {
		Index        int          `json:"index"`
		Message      chat.Message `json:"message"`
		FinishReason string       `json:"finish_reason"`
	}{Message: chat.Message{Role: "assistant", Content: c.reply}, FinishReason: "stop"})
	return resp, nil
}
func (c *capturingProvider) CompleteWithTools(context.Context, []chat.Message, []chat.Tool) (*chat.ChatResponse, error) {
	panic("not used")
}
func (c *capturingProvider) CompleteWithToolsStream(context.Context, []chat.Message, []chat.Tool, chat.StreamCallback) (*chat.ChatResponse, error) {
	panic("not used")
}
func (c *capturingProvider) Model() string            { return "fake" }
func (c *capturingProvider) SetMetrics(*chat.Metrics) {}

// repeatingProvider returns the same scripted reply for every
// Complete call. Useful for the worker tests where one provider
// drives many pipeline runs in a single tick.
type repeatingProvider struct {
	calls   atomic.Int32
	content string
	err     error
	// finishReason overrides finish_reason ("" = "stop"). Lets a worker-level
	// test drive the truncation path end to end.
	finishReason string
}

func (r *repeatingProvider) Complete(_ context.Context, _ []chat.Message) (*chat.ChatResponse, error) {
	r.calls.Add(1)
	if r.err != nil {
		return nil, r.err
	}
	resp := &chat.ChatResponse{Model: "fake"}
	resp.Choices = append(resp.Choices, struct {
		Index        int          `json:"index"`
		Message      chat.Message `json:"message"`
		FinishReason string       `json:"finish_reason"`
	}{Message: chat.Message{Role: "assistant", Content: r.content}, FinishReason: finishReasonOr(r.finishReason)})
	resp.Usage.PromptTokens = 100
	resp.Usage.CompletionTokens = 50
	resp.Usage.TotalTokens = 150
	return resp, nil
}
func (r *repeatingProvider) CompleteWithTools(context.Context, []chat.Message, []chat.Tool) (*chat.ChatResponse, error) {
	panic("not used")
}
func (r *repeatingProvider) CompleteWithToolsStream(context.Context, []chat.Message, []chat.Tool, chat.StreamCallback) (*chat.ChatResponse, error) {
	panic("not used")
}
func (r *repeatingProvider) Model() string            { return "fake" }
func (r *repeatingProvider) SetMetrics(*chat.Metrics) {}

func newRepeatingProvider(content string) *repeatingProvider {
	return &repeatingProvider{content: content}
}

func newRepeatingErrorProvider(err error) *repeatingProvider {
	return &repeatingProvider{err: err}
}

// newTruncatingProvider always answers with finish_reason=length and empty
// content — the exact shape a reasoning model produces when it spends its
// whole budget thinking.
func newTruncatingProvider() *repeatingProvider {
	return &repeatingProvider{content: "", finishReason: "length"}
}

func TestExtract_HappyPathArrayResponse(t *testing.T) {
	chunk := "Vadim chose PostgreSQL 16 for the new ledger service."
	// PostgreSQL 16 spans bytes 12..25 in the chunk.
	body := `[
	  {"type":"PERSON","name":"Vadim","char_start":0,"char_end":5,"surface":"Vadim"},
	  {"type":"TECHNOLOGY","name":"PostgreSQL 16","char_start":12,"char_end":25,"surface":"PostgreSQL 16"}
	]`
	fp := &fakeProvider{replies: []reply{{content: body}}}
	ex := NewExtractor(fp, "fake")

	got, m, err := ex.Extract(context.Background(), chunk)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 candidates, got %d (%v)", len(got), got)
	}
	if got[0].Name != "Vadim" || got[1].Name != "PostgreSQL 16" {
		t.Errorf("unexpected names: %+v", got)
	}
	if m == nil || m.Model != "fake" {
		t.Errorf("metrics model not stamped: %+v", m)
	}
}

func TestExtract_StripsCodeFences(t *testing.T) {
	chunk := "ACME quoted $9,500."
	fenced := "```json\n[" +
		`{"type":"VENDOR","name":"ACME","char_start":0,"char_end":4,"surface":"ACME"}` +
		"]\n```"
	fp := &fakeProvider{replies: []reply{{content: fenced}}}
	ex := NewExtractor(fp, "")

	got, _, err := ex.Extract(context.Background(), chunk)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(got) != 1 || got[0].Type != "VENDOR" {
		t.Fatalf("fence-strip parse failed: %+v", got)
	}
}

func TestExtract_AcceptsObjectWrapping(t *testing.T) {
	chunk := "Quarterly review on 2026-05-15."
	body := `{"entities":[
	  {"type":"DATE","name":"2026-05-15","char_start":20,"char_end":30,"surface":"2026-05-15"}
	]}`
	fp := &fakeProvider{replies: []reply{{content: body}}}
	ex := NewExtractor(fp, "")

	got, _, err := ex.Extract(context.Background(), chunk)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(got) != 1 || got[0].Type != "DATE" {
		t.Fatalf("object-wrapped parse failed: %+v", got)
	}
}

func TestExtract_DropsUnknownTypeAndEmptyName(t *testing.T) {
	chunk := "noise body"
	body := `[
	  {"type":"GADGET","name":"thing","char_start":0,"char_end":5,"surface":"noise"},
	  {"type":"PERSON","name":"   ","char_start":0,"char_end":5,"surface":"noise"},
	  {"type":"FACT","name":"valid one","char_start":0,"char_end":5,"surface":"noise"}
	]`
	fp := &fakeProvider{replies: []reply{{content: body}}}
	ex := NewExtractor(fp, "")

	got, _, err := ex.Extract(context.Background(), chunk)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(got) != 1 || got[0].Name != "valid one" {
		t.Fatalf("validation drop failed: %+v", got)
	}
}

func TestExtract_ResetsOutOfRangeOffsets(t *testing.T) {
	chunk := "short"
	// char_end = 99 is well past chunk length (5).
	body := `[{"type":"FACT","name":"x","char_start":0,"char_end":99,"surface":"short"}]`
	fp := &fakeProvider{replies: []reply{{content: body}}}
	ex := NewExtractor(fp, "")

	got, _, err := ex.Extract(context.Background(), chunk)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(got))
	}
	if got[0].CharStart != 0 || got[0].CharEnd != 0 || got[0].Surface != "" {
		t.Errorf("expected span reset on out-of-range offsets, got %+v", got[0])
	}
}

func TestExtract_ClampsSurfaceToActualSubstring(t *testing.T) {
	chunk := "Vadim chose PostgreSQL 16."
	// Model claims surface "Postgres" — actual substring is "PostgreSQL 16".
	body := `[{"type":"TECHNOLOGY","name":"PostgreSQL 16","char_start":12,"char_end":25,"surface":"Postgres"}]`
	fp := &fakeProvider{replies: []reply{{content: body}}}
	ex := NewExtractor(fp, "")

	got, _, err := ex.Extract(context.Background(), chunk)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(got) != 1 || got[0].Surface != "PostgreSQL 16" {
		t.Errorf("expected surface clamp to actual substring, got %+v", got[0])
	}
}

func TestExtract_EmptyContentReturnsEarly(t *testing.T) {
	fp := &fakeProvider{}
	ex := NewExtractor(fp, "")

	got, m, err := ex.Extract(context.Background(), "   ")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil candidates for empty input, got %+v", got)
	}
	if m == nil {
		t.Fatalf("expected non-nil metrics")
	}
	if fp.calls.Load() != 0 {
		t.Errorf("expected no LLM call for empty content, got %d", fp.calls.Load())
	}
}

func TestExtract_EmptyArrayValid(t *testing.T) {
	fp := &fakeProvider{replies: []reply{{content: "[]"}}}
	ex := NewExtractor(fp, "")

	got, _, err := ex.Extract(context.Background(), "uneventful chunk")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got != nil {
		t.Errorf("empty array should yield nil candidates (post-validate), got %+v", got)
	}
}

func TestExtract_MalformedJSONErrors(t *testing.T) {
	fp := &fakeProvider{replies: []reply{{content: "not json at all"}}}
	ex := NewExtractor(fp, "")

	_, _, err := ex.Extract(context.Background(), "anything")
	if err == nil || !strings.Contains(err.Error(), "JSON parse") {
		t.Errorf("expected JSON parse error, got %v", err)
	}
}

func TestExtract_PropagatesPermanentError(t *testing.T) {
	fp := &fakeProvider{replies: []reply{
		{err: errors.New("auth failure: 401 unauthorized")},
	}}
	ex := NewExtractor(fp, "")

	_, _, err := ex.Extract(context.Background(), "anything")
	if err == nil {
		t.Fatal("expected permanent error to bubble up")
	}
	if fp.calls.Load() != 1 {
		t.Errorf("expected 1 LLM call (no retry), got %d", fp.calls.Load())
	}
}

func TestExtract_NilClient(t *testing.T) {
	ex := &Extractor{}
	_, _, err := ex.Extract(context.Background(), "anything")
	if err == nil {
		t.Fatal("expected error when Client is nil")
	}
}

// Per-chunk outcome classification (2026-05-25 audit follow-on).
// Three distinct paths the metric must label correctly so future
// tuning (prompt, model) has a defensible before/after.

func TestExtract_Outcome_EmptyResponseLabel(t *testing.T) {
	// LLM returns a bare [] — the dominant 2026-05-25 audit
	// finding. Must report empty_response so dashboards can
	// distinguish it from the "filtered all" case.
	fp := &fakeProvider{replies: []reply{{content: "[]"}}}
	ex := NewExtractor(fp, "fake")
	cands, m, err := ex.Extract(context.Background(), "irrelevant content")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(cands) != 0 {
		t.Errorf("expected zero candidates, got %d", len(cands))
	}
	if m.Outcome != ExtractOutcomeEmptyResponse {
		t.Errorf("Outcome = %q, want %q", m.Outcome, ExtractOutcomeEmptyResponse)
	}
}

func TestExtract_Outcome_DroppedAllInvalidLabel(t *testing.T) {
	// LLM proposed two candidates but BOTH have types outside
	// the closed vocab — validateCandidates filters both. The
	// metric label must reflect "all dropped" not "empty".
	body := `[
	  {"type":"VEHICLE","name":"Škoda Epiq","char_start":0,"char_end":10,"surface":"Škoda Epiq"},
	  {"type":"CERTIFICATION","name":"ITIL","char_start":11,"char_end":15,"surface":"ITIL"}
	]`
	fp := &fakeProvider{replies: []reply{{content: body}}}
	ex := NewExtractor(fp, "fake")
	cands, m, err := ex.Extract(context.Background(), "Škoda Epiq ITIL")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(cands) != 0 {
		t.Errorf("expected zero candidates after vocab filter, got %d", len(cands))
	}
	if m.Outcome != ExtractOutcomeDroppedAllInvalid {
		t.Errorf("Outcome = %q, want %q", m.Outcome, ExtractOutcomeDroppedAllInvalid)
	}
}

func TestExtract_Outcome_ProducedLabel(t *testing.T) {
	body := `[
	  {"type":"VENDOR","name":"ACME","char_start":0,"char_end":4,"surface":"ACME"}
	]`
	fp := &fakeProvider{replies: []reply{{content: body}}}
	ex := NewExtractor(fp, "fake")
	cands, m, err := ex.Extract(context.Background(), "ACME signed.")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(cands))
	}
	if m.Outcome != ExtractOutcomeProduced {
		t.Errorf("Outcome = %q, want %q", m.Outcome, ExtractOutcomeProduced)
	}
}

func TestClassifyExtractOutcome_TableDriven(t *testing.T) {
	cases := []struct {
		raw, validated int
		want           string
	}{
		{0, 0, ExtractOutcomeEmptyResponse},
		{5, 0, ExtractOutcomeDroppedAllInvalid},
		{5, 1, ExtractOutcomeProduced},
		{5, 5, ExtractOutcomeProduced},
		{1, 0, ExtractOutcomeDroppedAllInvalid},
	}
	for _, tc := range cases {
		got := classifyExtractOutcome(tc.raw, tc.validated)
		if got != tc.want {
			t.Errorf("classifyExtractOutcome(%d,%d) = %q, want %q",
				tc.raw, tc.validated, got, tc.want)
		}
	}
}

// TestExtract_TruncationIsAFailureNotAnEmptyResult is the regression test for
// the 2026-07-31 silent-loss finding: 83.3% of extractions were recorded as
// empty_response, the chunk marked extracted, and the content permanently
// absent from the graph.
//
// The mechanism was a chain of individually-reasonable steps. A reasoning
// model spends its whole token budget thinking and returns finish_reason=length
// with EMPTY content; parseCandidates treats an empty completion as a valid
// parse of zero entities; the pipeline returns nil error; the worker calls
// MarkExtracted. Nothing failed, so the design's "failed parse → retry →
// re-flag" self-healing never fired.
//
// The distinction this pins: a model that read the text and found nobody, and
// a model that never answered, must not produce the same outcome.
func TestExtract_TruncationIsAFailureNotAnEmptyResult(t *testing.T) {
	for _, tc := range []struct{ name, content string }{
		// The dominant real shape: reasoning consumed the budget.
		{"empty content", ""},
		// Partial output is worse, not better — it can parse and look complete.
		{"partial content", `[{"name":"Ada","type":"PERSON"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fp := &fakeProvider{replies: []reply{{content: tc.content, finishReason: "length"}}}
			ex := NewExtractor(fp, "")

			got, m, err := ex.Extract(context.Background(), "a chunk that names Ada Lovelace")

			if err == nil {
				t.Fatalf("a truncated completion must be an error, got candidates=%+v", got)
			}
			if !errors.Is(err, ErrTruncatedCompletion) {
				t.Errorf("error must be identifiable as truncation, got %v", err)
			}
			if got != nil {
				t.Errorf("no candidates may be reported from a truncated completion, got %+v", got)
			}
			if m == nil {
				t.Fatalf("metrics must be reported so the burned budget stays visible")
			}
			if m.Outcome != ExtractOutcomeTruncated {
				t.Errorf("outcome = %q, want %q — conflating this with empty_response is what hid the loss",
					m.Outcome, ExtractOutcomeTruncated)
			}
			if m.Outcome == ExtractOutcomeEmptyResponse {
				t.Error("truncation must never be reported as empty_response")
			}
			// The call burned tokens; the metric must say so or the cost of the
			// failure is invisible.
			if m.CompletionTokens == 0 {
				t.Error("completion tokens must be carried through so the wasted budget is measurable")
			}
		})
	}
}

// TestExtract_GenuinelyEmptyStaysEmpty is the other half: a model that answers
// properly and finds nothing is a normal, successful outcome and must NOT be
// turned into an error by the truncation change.
func TestExtract_GenuinelyEmptyStaysEmpty(t *testing.T) {
	fp := &fakeProvider{replies: []reply{{content: "[]", finishReason: "stop"}}}
	ex := NewExtractor(fp, "")

	got, m, err := ex.Extract(context.Background(), "uneventful chunk")
	if err != nil {
		t.Fatalf("a complete answer of zero entities is success, got %v", err)
	}
	if got != nil {
		t.Errorf("expected nil candidates, got %+v", got)
	}
	if m.Outcome != ExtractOutcomeEmptyResponse {
		t.Errorf("outcome = %q, want %q", m.Outcome, ExtractOutcomeEmptyResponse)
	}
}
