package memory

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/chat"
)

func TestClassifier_CacheHit_SkipsProvider(t *testing.T) {
	fp := newClassifyProvider()
	cache := newFakeResponseCache()
	c := NewClassifier(fp, "")
	c.Cache = cache

	user := buildClassifierUserPrompt("some content here", "doc.md", "researcher")
	msgs := []chat.Message{
		{Role: "system", Content: classifierSystemPrompt},
		{Role: "user", Content: user},
	}
	key := ResponseCacheKey(c.Model, ResponseCachePurposeClassifier, msgs)
	_ = cache.Put(context.Background(), key, c.Model, ResponseCachePurposeClassifier, "research", 50, 5)

	got, err := c.Classify(context.Background(), "some content here", "doc.md", "researcher", "", "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != ClassResearch {
		t.Errorf("got %q, want %q", got, ClassResearch)
	}
	if fp.calls.Load() != 0 {
		t.Errorf("expected 0 provider calls on cache hit, got %d", fp.calls.Load())
	}
}

func TestClassifier_CacheMiss_PopulatesCache(t *testing.T) {
	fp := newClassifyProvider(titlerReply{content: "decision"})
	cache := newFakeResponseCache()
	c := NewClassifier(fp, "")
	c.Cache = cache

	got, err := c.Classify(context.Background(), "approved: switch retry layer", "decision.md", "lead", "", "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != ClassDecision {
		t.Errorf("got %q, want %q", got, ClassDecision)
	}
	if cache.puts != 1 {
		t.Errorf("expected miss to populate cache (puts=1), got %d", cache.puts)
	}

	// Second call hits cache; provider must not be invoked again.
	got2, err := c.Classify(context.Background(), "approved: switch retry layer", "decision.md", "lead", "", "")
	if err != nil {
		t.Fatalf("err on second call: %v", err)
	}
	if got2 != ClassDecision {
		t.Errorf("got %q, want %q", got2, ClassDecision)
	}
	if fp.calls.Load() != 1 {
		t.Errorf("second call should hit cache; provider total=%d", fp.calls.Load())
	}
}

func TestClassifier_NilCache_AlwaysCallsProvider(t *testing.T) {
	fp := newClassifyProvider(titlerReply{content: "research"}, titlerReply{content: "research"})
	c := NewClassifier(fp, "")
	c.Cache = nil

	if _, err := c.Classify(context.Background(), "x", "f", "r", "", ""); err != nil {
		t.Fatalf("err: %v", err)
	}
	if _, err := c.Classify(context.Background(), "x", "f", "r", "", ""); err != nil {
		t.Fatalf("err: %v", err)
	}
	if fp.calls.Load() != 2 {
		t.Errorf("nil cache must always call provider; got %d", fp.calls.Load())
	}
}
