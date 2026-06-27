package memory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// classifyFakeProvider is a deterministic chat.Provider variant for
// classifier tests. Mirrors titlerFakeProvider in shape but emits a
// classifier-shaped (single-token) response by default.
type classifyFakeProvider struct {
	titlerFakeProvider
}

func newClassifyProvider(replies ...titlerReply) *classifyFakeProvider {
	cp := &classifyFakeProvider{}
	cp.replies = replies
	return cp
}

func TestClassifier_NilGuards(t *testing.T) {
	var nilC *Classifier
	got, err := nilC.Classify(context.Background(), "x", "s.md", "researcher", "", "")
	if got != ClassUnclassified || err == nil {
		t.Fatalf("nil receiver: %q %v", got, err)
	}
	c := &Classifier{}
	got, err = c.Classify(context.Background(), "x", "", "", "", "")
	if got != ClassUnclassified || err == nil {
		t.Fatalf("nil client: %q %v", got, err)
	}
}

func TestClassifier_EmptyContent(t *testing.T) {
	c := NewClassifier(newClassifyProvider(titlerReply{content: "research"}), "")
	got, err := c.Classify(context.Background(), "   \n\t ", "s.md", "researcher", "", "")
	if got != ClassUnclassified || err != nil {
		t.Fatalf("empty content: %q %v", got, err)
	}
}

func TestClassifier_HappyPath(t *testing.T) {
	cases := map[string]ContentClass{
		"research":       ClassResearch,
		"spec":           ClassSpec,
		"decision":       ClassDecision,
		"commit_msg":     ClassCommitMsg,
		"diagnostic":     ClassDiagnostic,
		"external_fetch": ClassExternalFetch,
		"summary":        ClassSummary,
		"unclassified":   ClassUnclassified,
	}
	for raw, want := range cases {
		t.Run(raw, func(t *testing.T) {
			fp := newClassifyProvider(titlerReply{content: raw})
			c := NewClassifier(fp, "")
			got, err := c.Classify(context.Background(), "some content here", "doc.md", "researcher", "", "")
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
	}
}

func TestClassifier_TruncatesLongContent(t *testing.T) {
	long := strings.Repeat("alpha beta ", 2000) // ~22KB
	fp := newClassifyProvider(titlerReply{content: "research"})
	c := NewClassifier(fp, "")
	c.MaxPreviewBytes = 64
	got, err := c.Classify(context.Background(), long, "s.md", "r", "", "")
	if err != nil || got != ClassResearch {
		t.Fatalf("got %q %v", got, err)
	}
	// captured prompt should reflect the truncation.
	if len(fp.replies) > 0 && fp.calls.Load() != 1 {
		t.Fatalf("expected 1 call, got %d", fp.calls.Load())
	}
}

func TestClassifier_UnrecognisedClassReturnsErr(t *testing.T) {
	fp := newClassifyProvider(titlerReply{content: "fictional_class"})
	c := NewClassifier(fp, "")
	got, err := c.Classify(context.Background(), "content", "s.md", "r", "", "")
	if got != ClassUnclassified {
		t.Fatalf("got %q", got)
	}
	if err == nil || !strings.Contains(err.Error(), "unrecognised class") {
		t.Fatalf("err: %v", err)
	}
}

func TestClassifier_LLMErrorPropagates(t *testing.T) {
	fp := newClassifyProvider(
		titlerReply{err: errors.New("upstream 503")},
		titlerReply{err: errors.New("upstream 503")},
	)
	c := NewClassifier(fp, "")
	c.MaxAttempts = 2
	got, err := c.Classify(context.Background(), "content", "s.md", "r", "", "")
	if got != ClassUnclassified || err == nil {
		t.Fatalf("got %q %v", got, err)
	}
}

func TestClassifier_NoChoicesPropagates(t *testing.T) {
	c := NewClassifier(&emptyChoiceProvider{}, "")
	got, err := c.Classify(context.Background(), "x", "s.md", "r", "", "")
	if got != ClassUnclassified || err == nil || !strings.Contains(err.Error(), "no choices") {
		t.Fatalf("got %q %v", got, err)
	}
}

func TestClassifier_RetriesThenSucceeds(t *testing.T) {
	fp := newClassifyProvider(
		titlerReply{err: errors.New("transient")},
		titlerReply{content: "decision"},
	)
	c := NewClassifier(fp, "")
	c.MaxAttempts = 2
	got, err := c.Classify(context.Background(), "x", "s.md", "reviewer", "", "")
	if err != nil || got != ClassDecision {
		t.Fatalf("got %q %v", got, err)
	}
	if fp.calls.Load() != 2 {
		t.Fatalf("expected 2 calls, got %d", fp.calls.Load())
	}
}

func TestClassifier_ContextCancelMidRetry(t *testing.T) {
	c := NewClassifier(&slowProvider{delay: 200 * time.Millisecond}, "")
	c.Timeout = 30 * time.Millisecond
	c.MaxAttempts = 3
	start := time.Now()
	got, err := c.Classify(context.Background(), "x", "s", "r", "", "")
	if time.Since(start) > 300*time.Millisecond {
		t.Fatalf("timeout not enforced")
	}
	if got != ClassUnclassified || err == nil {
		t.Fatalf("got %q %v", got, err)
	}
}

func TestClassifier_RecordsUsage(t *testing.T) {
	fp := newClassifyProvider(titlerReply{content: "spec"})
	rec := &fakeUsageRecorder{}
	c := NewClassifier(fp, "fake-model")
	c.LLMUsage = rec
	c.Pricing = fixedPricing{}
	got, err := c.Classify(context.Background(), "spec content", "doc.md", "analyst", "p1", "c1")
	if err != nil || got != ClassSpec {
		t.Fatalf("got %q %v", got, err)
	}
	if len(rec.rows) != 1 {
		t.Fatalf("expected 1 usage row, got %d", len(rec.rows))
	}
	row := rec.rows[0]
	if row.Role != "memory_classifier" {
		t.Fatalf("role: %q", row.Role)
	}
	if row.ProjectID != "p1" || row.StepID != "c1" {
		t.Fatalf("attribution: %+v", row)
	}
	if row.PromptTokens == 0 || row.CompletionTokens == 0 {
		t.Fatalf("tokens not recorded: %+v", row)
	}
	if row.CostUSD == 0 {
		t.Fatalf("cost not computed: %v", row.CostUSD)
	}
}

func TestClassifier_RecordsUsageOnUnusableResponse(t *testing.T) {
	// Model spent tokens but emitted a bad class → we still want to
	// bill the spend so the dashboard isn't undercounted.
	fp := newClassifyProvider(titlerReply{content: "garbage_class_name"})
	rec := &fakeUsageRecorder{}
	c := NewClassifier(fp, "")
	c.LLMUsage = rec
	got, err := c.Classify(context.Background(), "x", "s.md", "r", "p", "c")
	if got != ClassUnclassified || err == nil {
		t.Fatalf("got %q %v", got, err)
	}
	if len(rec.rows) != 1 {
		t.Fatalf("expected 1 usage row even on bad response, got %d", len(rec.rows))
	}
}

func TestBuildClassifierUserPrompt(t *testing.T) {
	// All fields present.
	got := buildClassifierUserPrompt("body text", "doc.md", "researcher")
	for _, want := range []string{"Source: doc.md", "Producer role: researcher", "FRAGMENT:\nbody text"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// Source only.
	got = buildClassifierUserPrompt("body", "doc.md", "")
	if strings.Contains(got, "Producer role:") {
		t.Fatalf("should omit empty role: %q", got)
	}
	// Role only.
	got = buildClassifierUserPrompt("body", "", "researcher")
	if strings.Contains(got, "Source:") {
		t.Fatalf("should omit empty source: %q", got)
	}
	// Neither.
	got = buildClassifierUserPrompt("body", "", "")
	if !strings.HasPrefix(got, "FRAGMENT:") {
		t.Fatalf("should not prepend blank lines: %q", got)
	}
	// Whitespace-only hints treated as empty.
	got = buildClassifierUserPrompt("body", "   ", "\t")
	if strings.Contains(got, "Source:") || strings.Contains(got, "Producer role:") {
		t.Fatalf("whitespace hints should be skipped: %q", got)
	}
}

func TestParseClassifierResponse(t *testing.T) {
	cases := map[string]struct {
		want ContentClass
		ok   bool
	}{
		// Happy path: exact token.
		"research":     {ClassResearch, true},
		"decision":     {ClassDecision, true},
		"unclassified": {ClassUnclassified, true},
		// Case-insensitive.
		"RESEARCH": {ClassResearch, true},
		"Decision": {ClassDecision, true},
		// Trailing/leading noise.
		" research.":       {ClassResearch, true},
		"OUTPUT: research": {ClassResearch, true},
		"CLASS: spec":      {ClassSpec, true},
		"`research`":       {ClassResearch, true},
		`"research"`:       {ClassResearch, true},
		// Code fences.
		"```\nresearch\n```":     {ClassResearch, true},
		"```json\nresearch\n```": {ClassResearch, true},
		// First-line only (model adds trailing rationale).
		"research\nReason: tagged from the H1 heading": {ClassResearch, true},
		// Unknown / invalid.
		"":               {ClassUnclassified, false},
		"made-up-class":  {ClassUnclassified, false},
		"research, spec": {ClassUnclassified, false},
		"  ":             {ClassUnclassified, false},
	}
	for in, tc := range cases {
		t.Run(in, func(t *testing.T) {
			got, ok := parseClassifierResponse(in)
			if got != tc.want || ok != tc.ok {
				t.Errorf("parseClassifierResponse(%q) = (%q, %v), want (%q, %v)",
					in, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestTruncForClassifier(t *testing.T) {
	if got := truncForClassifier("short", 100); got != "short" {
		t.Fatalf("under: %q", got)
	}
	if got := truncForClassifier("alphabet", 3); got != "alp…" {
		t.Fatalf("over: %q", got)
	}
}
