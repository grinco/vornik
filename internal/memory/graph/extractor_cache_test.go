package graph

import (
	"context"
	"sync"
	"testing"

	"vornik.io/vornik/internal/chat"
)

// fakeExtractCache is the in-memory test double for graph.ResponseCache.
// Mirrors memory.fakeResponseCache shape — graph can't import memory
// without a cycle, so this is duplicated locally.
type fakeExtractCache struct {
	mu   sync.Mutex
	rows map[string]fakeExtractRow
	gets int
	puts int
}

type fakeExtractRow struct {
	content    string
	promptTok  int
	completion int
}

func newFakeExtractCache() *fakeExtractCache {
	return &fakeExtractCache{rows: map[string]fakeExtractRow{}}
}

func (f *fakeExtractCache) Get(_ context.Context, key string) (string, int, int, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	r, ok := f.rows[key]
	if !ok {
		return "", 0, 0, false, nil
	}
	return r.content, r.promptTok, r.completion, true, nil
}

func (f *fakeExtractCache) Put(_ context.Context, key, _, _, content string, pt, ct int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts++
	f.rows[key] = fakeExtractRow{content: content, promptTok: pt, completion: ct}
	return nil
}

// validExtractorJSON is the canned reply the extractor parses into
// one Candidate. Matches the JSON shape parseCandidates accepts.
const validExtractorJSON = `[{"type":"PERSON","name":"Alice","char_start":0,"char_end":5,"surface":"Alice"}]`

func TestExtractor_CacheHit_SkipsProvider(t *testing.T) {
	fp := &fakeProvider{}
	cache := newFakeExtractCache()
	body := "Alice met Bob in Paris."
	ex := NewExtractor(fp, "")
	ex.Cache = cache

	msgs := []chat.Message{
		{Role: "system", Content: extractorSystemPrompt},
		{Role: "user", Content: "CHUNK:\n" + body},
	}
	key := responseCacheKey(ex.Model, responseCachePurposeKGExtract, msgs)
	_ = cache.Put(context.Background(), key, ex.Model, responseCachePurposeKGExtract, validExtractorJSON, 200, 50)

	cands, metrics, err := ex.Extract(context.Background(), body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(cands) != 1 || cands[0].Name != "Alice" {
		t.Errorf("unexpected candidates: %+v", cands)
	}
	if fp.calls.Load() != 0 {
		t.Errorf("cache hit must not call provider; got %d", fp.calls.Load())
	}
	// Metrics surface the cached token counts so downstream cost
	// math stays correct.
	if metrics == nil || metrics.PromptTokens != 200 || metrics.CompletionTokens != 50 {
		t.Errorf("cached metrics not surfaced: %+v", metrics)
	}
}

func TestExtractor_CacheMiss_PopulatesCache(t *testing.T) {
	fp := &fakeProvider{replies: []reply{{content: validExtractorJSON}}}
	cache := newFakeExtractCache()
	body := "Bob travelled to Vienna."
	ex := NewExtractor(fp, "")
	ex.Cache = cache

	cands, _, err := ex.Extract(context.Background(), body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(cands) != 1 {
		t.Errorf("expected 1 candidate, got %d", len(cands))
	}
	if cache.puts != 1 {
		t.Errorf("expected miss to populate cache; puts=%d", cache.puts)
	}

	// Second identical call must hit cache and skip provider.
	_, _, err = ex.Extract(context.Background(), body)
	if err != nil {
		t.Fatalf("err on second call: %v", err)
	}
	if fp.calls.Load() != 1 {
		t.Errorf("second call should hit cache; provider total=%d", fp.calls.Load())
	}
}

func TestExtractor_NilCache_AlwaysCallsProvider(t *testing.T) {
	fp := &fakeProvider{
		replies: []reply{{content: validExtractorJSON}, {content: validExtractorJSON}},
	}
	ex := NewExtractor(fp, "")
	ex.Cache = nil

	if _, _, err := ex.Extract(context.Background(), "x"); err != nil {
		t.Fatalf("err: %v", err)
	}
	if _, _, err := ex.Extract(context.Background(), "x"); err != nil {
		t.Fatalf("err: %v", err)
	}
	if fp.calls.Load() != 2 {
		t.Errorf("nil cache must always call provider; got %d", fp.calls.Load())
	}
}

func TestResponseCacheKey_GraphMatchesShape(t *testing.T) {
	msgs := []chat.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "u"},
	}
	k1 := responseCacheKey("m", "p", msgs)
	k2 := responseCacheKey("m", "p", msgs)
	if k1 != k2 {
		t.Errorf("key not deterministic: %q vs %q", k1, k2)
	}
	if len(k1) != 64 {
		t.Errorf("expected 64-char hex, got %d", len(k1))
	}
}
