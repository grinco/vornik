package memory

import (
	"context"
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

func TestNarrativeWriter_NilGuards(t *testing.T) {
	var nilW *NarrativeWriter
	if _, err := nilW.Write(context.Background(), nil, "", ""); err == nil {
		t.Error("nil receiver should error")
	}
	w := &NarrativeWriter{} // nil Client
	if _, err := w.Write(context.Background(), nil, "x", ""); err == nil {
		t.Error("nil client should error")
	}
}

func TestNarrativeWriter_EmptyInputReturnsEmpty(t *testing.T) {
	fp := &titlerFakeProvider{replies: []titlerReply{{content: "should not fire"}}}
	w := NewNarrativeWriter(fp, "")
	// No terms, no sample → user message is empty → short-circuit.
	got, err := w.Write(context.Background(), nil, "  ", "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
	if fp.calls.Load() != 0 {
		t.Errorf("provider must NOT be called with empty input (calls=%d)", fp.calls.Load())
	}
}

func TestNarrativeWriter_HappyPath(t *testing.T) {
	fp := &titlerFakeProvider{replies: []titlerReply{
		{content: "An automated equities trading project focused on IBKR bracket orders."},
	}}
	w := NewNarrativeWriter(fp, "")
	terms := []TermFrequency{{Term: "ibkr", Count: 42}, {Term: "bracket", Count: 18}}
	sample := "Submitted bracket order for NVDA 100 shares with 2% stop"
	got, err := w.Write(context.Background(), terms, sample, "proj-x")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(got, "IBKR") {
		t.Errorf("response should mention IBKR; got %q", got)
	}
}

func TestNarrativeWriter_StripsSurroundingQuotes(t *testing.T) {
	fp := &titlerFakeProvider{replies: []titlerReply{
		{content: `"A project about gardening."`},
	}}
	w := NewNarrativeWriter(fp, "")
	got, err := w.Write(context.Background(), []TermFrequency{{Term: "plants", Count: 5}}, "soil", "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "A project about gardening." {
		t.Errorf("got %q, want unquoted", got)
	}
}

func TestNarrativeWriter_CollapsesWhitespace(t *testing.T) {
	fp := &titlerFakeProvider{replies: []titlerReply{
		{content: "Line one.\n\nLine two."},
	}}
	w := NewNarrativeWriter(fp, "")
	got, err := w.Write(context.Background(), []TermFrequency{{Term: "x", Count: 1}}, "y", "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "Line one. Line two." {
		t.Errorf("got %q, want whitespace collapsed", got)
	}
}

func TestNarrativeWriter_RetriesOnError(t *testing.T) {
	fp := &titlerFakeProvider{replies: []titlerReply{
		{err: errors.New("transient")},
		{content: "second-attempt summary"},
	}}
	w := NewNarrativeWriter(fp, "")
	w.MaxAttempts = 2
	got, err := w.Write(context.Background(), []TermFrequency{{Term: "x", Count: 1}}, "y", "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "second-attempt summary" {
		t.Errorf("got %q", got)
	}
	if fp.calls.Load() != 2 {
		t.Errorf("expected 2 calls (retry), got %d", fp.calls.Load())
	}
}

func TestNarrativeWriter_RecordsUsageWhenConfigured(t *testing.T) {
	rec := &fakeUsageRecorder{}
	fp := &titlerFakeProvider{replies: []titlerReply{{content: "ok"}}}
	w := NewNarrativeWriter(fp, "test-model")
	w.LLMUsage = rec
	w.Pricing = fixedPricing{}
	if _, err := w.Write(context.Background(), []TermFrequency{{Term: "x", Count: 1}}, "y", "proj-a"); err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(rec.rows) != 1 {
		t.Fatalf("expected 1 usage row, got %d", len(rec.rows))
	}
	row := rec.rows[0]
	if row.ProjectID != "proj-a" {
		t.Errorf("ProjectID = %q, want proj-a", row.ProjectID)
	}
	if row.Role != narrativeRole {
		t.Errorf("Role = %q, want %s", row.Role, narrativeRole)
	}
	if row.Source != persistence.TaskLLMUsageSourceMemoryNarrative {
		t.Errorf("Source = %q", row.Source)
	}
	if row.PromptTokens != 120 || row.CompletionTokens != 30 {
		t.Errorf("token counts: prompt=%d completion=%d", row.PromptTokens, row.CompletionTokens)
	}
	if row.CostUSD <= 0 {
		t.Errorf("CostUSD = %f, want > 0", row.CostUSD)
	}
}

func TestNarrativeWriter_SkipsUsageWhenProjectIDEmpty(t *testing.T) {
	rec := &fakeUsageRecorder{}
	fp := &titlerFakeProvider{replies: []titlerReply{{content: "ok"}}}
	w := NewNarrativeWriter(fp, "")
	w.LLMUsage = rec
	if _, err := w.Write(context.Background(), []TermFrequency{{Term: "x", Count: 1}}, "y", ""); err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(rec.rows) != 0 {
		t.Errorf("usage row recorded with empty projectID: %v", rec.rows)
	}
}

func TestBuildNarrativeUserMessage_TermsThenSample(t *testing.T) {
	terms := []TermFrequency{{Term: "alpha", Count: 5}, {Term: "beta", Count: 2}}
	out := buildNarrativeUserMessage(terms, "the body text")
	if !strings.Contains(out, "TOP_TERMS:") {
		t.Errorf("missing TOP_TERMS marker: %q", out)
	}
	if !strings.Contains(out, "alpha=5") || !strings.Contains(out, "beta=2") {
		t.Errorf("missing term rendering: %q", out)
	}
	if !strings.Contains(out, "SAMPLE:") || !strings.Contains(out, "the body text") {
		t.Errorf("missing SAMPLE rendering: %q", out)
	}
}

func TestBuildNarrativeUserMessage_EmptyInputs(t *testing.T) {
	if got := buildNarrativeUserMessage(nil, ""); got != "" {
		t.Errorf("empty inputs should yield empty string, got %q", got)
	}
}

func TestBuildNarrativeUserMessage_TermsOnly(t *testing.T) {
	out := buildNarrativeUserMessage([]TermFrequency{{Term: "x", Count: 1}}, "")
	if !strings.Contains(out, "TOP_TERMS:") {
		t.Errorf("missing TOP_TERMS: %q", out)
	}
	if strings.Contains(out, "SAMPLE:") {
		t.Errorf("SAMPLE should be absent when sample empty: %q", out)
	}
}

func TestBuildNarrativeUserMessage_SampleOnly(t *testing.T) {
	out := buildNarrativeUserMessage(nil, "just the sample")
	if strings.Contains(out, "TOP_TERMS:") {
		t.Errorf("TOP_TERMS should be absent when no terms: %q", out)
	}
	if !strings.Contains(out, "just the sample") {
		t.Errorf("sample missing: %q", out)
	}
}

func TestCleanNarrative_PreservesPunctuation(t *testing.T) {
	// Narrative cleaner should NOT strip trailing periods; sentences
	// end in periods.
	got := cleanNarrative("This is a sentence.")
	if got != "This is a sentence." {
		t.Errorf("got %q, want unchanged", got)
	}
}

func TestCleanNarrative_HandlesAllEmptyCases(t *testing.T) {
	if got := cleanNarrative(""); got != "" {
		t.Errorf("empty input got %q", got)
	}
	if got := cleanNarrative("   "); got != "" {
		t.Errorf("whitespace input got %q", got)
	}
}
