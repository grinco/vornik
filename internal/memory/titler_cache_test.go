package memory

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/chat"
)

// titlerCacheHitShortCircuits verifies the Phase E response cache:
// when Cache.Get returns a hit, the LLM provider is NOT called.
func TestTitler_CacheHit_SkipsProvider(t *testing.T) {
	fp := &titlerFakeProvider{}
	cache := newFakeResponseCache()
	tr := NewTitler(fp, "")
	tr.Cache = cache

	// Pre-seed cache with the exact key Title() will compute. The
	// Titler builds {system: titlerSystemPrompt, user: "FRAGMENT:\n"+body}.
	content := "Quarterly Sales Forecast"
	msgs := []chat.Message{
		{Role: "system", Content: titlerSystemPrompt},
		{Role: "user", Content: "FRAGMENT:\nSome Q3 content"},
	}
	key := ResponseCacheKey(tr.Model, ResponseCachePurposeTitler, msgs)
	_ = cache.Put(context.Background(), key, tr.Model, ResponseCachePurposeTitler, content, 100, 30)

	got, err := tr.Title(context.Background(), "Some Q3 content", "", "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != content {
		t.Errorf("got %q, want %q", got, content)
	}
	if fp.calls.Load() != 0 {
		t.Errorf("expected 0 provider calls on cache hit, got %d", fp.calls.Load())
	}
	if cache.gets != 1 || cache.puts != 1 {
		t.Errorf("expected gets=1 puts=1 (seeded), got gets=%d puts=%d", cache.gets, cache.puts)
	}
}

// titlerCacheMissPopulates verifies that a cache miss falls through
// to the LLM and writes the response back to the cache for the
// next call.
func TestTitler_CacheMiss_PopulatesCache(t *testing.T) {
	fp := &titlerFakeProvider{replies: []titlerReply{{content: "Fresh Topic"}}}
	cache := newFakeResponseCache()
	tr := NewTitler(fp, "")
	tr.Cache = cache

	got, err := tr.Title(context.Background(), "novel content", "", "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "Fresh Topic" {
		t.Errorf("got %q, want %q", got, "Fresh Topic")
	}
	if fp.calls.Load() != 1 {
		t.Errorf("expected 1 provider call on miss, got %d", fp.calls.Load())
	}
	if cache.puts != 1 {
		t.Errorf("expected miss to populate cache (puts=1), got %d", cache.puts)
	}

	// Second call with identical input must skip the provider.
	got2, err := tr.Title(context.Background(), "novel content", "", "")
	if err != nil {
		t.Fatalf("err on second call: %v", err)
	}
	if got2 != "Fresh Topic" {
		t.Errorf("second call: got %q, want %q", got2, "Fresh Topic")
	}
	if fp.calls.Load() != 1 {
		t.Errorf("second call should hit cache, but provider called again (total=%d)", fp.calls.Load())
	}
}

// titlerCacheNilFallback verifies that a nil cache leaves the
// existing behaviour untouched — every call hits the provider.
func TestTitler_NilCache_AlwaysCallsProvider(t *testing.T) {
	fp := &titlerFakeProvider{
		replies: []titlerReply{{content: "Title A"}, {content: "Title A"}},
	}
	tr := NewTitler(fp, "")
	tr.Cache = nil

	if _, err := tr.Title(context.Background(), "x", "", ""); err != nil {
		t.Fatalf("err: %v", err)
	}
	if _, err := tr.Title(context.Background(), "x", "", ""); err != nil {
		t.Fatalf("err: %v", err)
	}
	if fp.calls.Load() != 2 {
		t.Errorf("nil cache must always call provider; got %d", fp.calls.Load())
	}
}
