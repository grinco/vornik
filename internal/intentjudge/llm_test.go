package intentjudge

import (
	"context"
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/chat"
)

// stubProvider is the minimal chat.Provider implementation used
// to drive Refine's parse / validate / error paths.
type stubProvider struct {
	resp *chat.ChatResponse
	err  error
}

func (s *stubProvider) Complete(_ context.Context, _ []chat.Message) (*chat.ChatResponse, error) {
	return s.resp, s.err
}
func (s *stubProvider) CompleteWithTools(_ context.Context, _ []chat.Message, _ []chat.Tool) (*chat.ChatResponse, error) {
	return s.resp, s.err
}
func (s *stubProvider) CompleteWithToolsStream(_ context.Context, _ []chat.Message, _ []chat.Tool, _ chat.StreamCallback) (*chat.ChatResponse, error) {
	return s.resp, s.err
}
func (s *stubProvider) Model() string              { return "stub" }
func (s *stubProvider) SetMetrics(_ *chat.Metrics) {}

func respWith(content string) *chat.ChatResponse {
	return &chat.ChatResponse{
		Choices: []struct {
			Index        int          `json:"index"`
			Message      chat.Message `json:"message"`
			FinishReason string       `json:"finish_reason"`
		}{
			{Index: 0, Message: chat.Message{Role: "assistant", Content: content}, FinishReason: "stop"},
		},
	}
}

// TestRefine_HappyPath — the LLM returns the expected JSON shape;
// the refiner produces a Verdict tagged Tier=TierLLM with parsed
// risk + confidence + recommendation + reasoning.
func TestRefine_HappyPath(t *testing.T) {
	r := &LLMRefiner{Provider: &stubProvider{resp: respWith(
		`{"risk":"high","confidence":0.85,"recommendation":"review","reasoning":"writes to a system path"}`,
	)}}
	h := Verdict{Risk: RiskMedium, Recommendation: RecommendReview, Reasoning: "tool name matched"}
	got, err := r.Refine(context.Background(), "run_shell", `{"cmd":"rm -rf /etc"}`, h)
	if err != nil {
		t.Fatalf("Refine: %v", err)
	}
	if got.Tier != TierLLM {
		t.Errorf("Tier = %q, want %q", got.Tier, TierLLM)
	}
	if got.Risk != RiskHigh {
		t.Errorf("Risk = %q, want high", got.Risk)
	}
	if got.Recommendation != RecommendReview {
		t.Errorf("Recommendation = %q, want review", got.Recommendation)
	}
	if got.Confidence != 0.85 {
		t.Errorf("Confidence = %v, want 0.85", got.Confidence)
	}
	if got.Reasoning != "writes to a system path" {
		t.Errorf("Reasoning = %q", got.Reasoning)
	}
}

// TestRefine_StripsMarkdownFence — models sometimes wrap JSON in
// ```json ... ``` despite the prompt forbidding it. The refiner
// must handle both fenced and bare output; failure mode would
// be every LLM call returning a parse error.
func TestRefine_StripsMarkdownFence(t *testing.T) {
	r := &LLMRefiner{Provider: &stubProvider{resp: respWith(
		"```json\n{\"risk\":\"low\",\"confidence\":0.9,\"recommendation\":\"approve\",\"reasoning\":\"ok\"}\n```",
	)}}
	got, err := r.Refine(context.Background(), "current_time", `{}`, Verdict{Risk: RiskLow})
	if err != nil {
		t.Fatalf("Refine: %v", err)
	}
	if got.Risk != RiskLow || got.Recommendation != RecommendApprove {
		t.Errorf("fenced parse failed: %+v", got)
	}
}

// TestRefine_RejectsInvalidRisk — the LLM hallucinated a risk
// level not in the rubric. We must error (not silently downgrade)
// so the caller falls back to the heuristic verdict rather than
// persisting nonsense.
func TestRefine_RejectsInvalidRisk(t *testing.T) {
	r := &LLMRefiner{Provider: &stubProvider{resp: respWith(
		`{"risk":"super-dangerous","confidence":0.5,"recommendation":"review","reasoning":"x"}`,
	)}}
	_, err := r.Refine(context.Background(), "t", `{}`, Verdict{})
	if err == nil || !strings.Contains(err.Error(), "invalid risk") {
		t.Errorf("err = %v, want invalid risk", err)
	}
}

// TestRefine_RejectsInvalidRecommendation — same defense as
// invalid risk, for the recommendation enum. Important because
// the dispatcher rate-limits / blocks on the recommendation —
// silently accepting "delete" or "" would be a real safety hole.
func TestRefine_RejectsInvalidRecommendation(t *testing.T) {
	r := &LLMRefiner{Provider: &stubProvider{resp: respWith(
		`{"risk":"low","confidence":0.5,"recommendation":"yolo","reasoning":"x"}`,
	)}}
	_, err := r.Refine(context.Background(), "t", `{}`, Verdict{})
	if err == nil || !strings.Contains(err.Error(), "invalid recommendation") {
		t.Errorf("err = %v, want invalid recommendation", err)
	}
}

// TestRefine_ProviderErrorPropagates — chat provider transport
// errors must surface up, not silently turn into a zero verdict.
func TestRefine_ProviderErrorPropagates(t *testing.T) {
	r := &LLMRefiner{Provider: &stubProvider{err: errors.New("upstream 503")}}
	_, err := r.Refine(context.Background(), "t", `{}`, Verdict{})
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("err = %v, want propagated upstream error", err)
	}
}

// TestRefine_ConfidenceClamped — the LLM may emit confidence
// values outside [0,1]. We clamp rather than reject so a slightly
// off-prompt model doesn't fail every refinement.
func TestRefine_ConfidenceClamped(t *testing.T) {
	r := &LLMRefiner{Provider: &stubProvider{resp: respWith(
		`{"risk":"low","confidence":1.5,"recommendation":"approve","reasoning":"x"}`,
	)}}
	got, err := r.Refine(context.Background(), "t", `{}`, Verdict{})
	if err != nil {
		t.Fatalf("Refine: %v", err)
	}
	if got.Confidence != 1.0 {
		t.Errorf("Confidence = %v, want clamped to 1.0", got.Confidence)
	}
}

// TestRefine_NilProviderReturnsError — defensive: a nil refiner
// or nil provider must error, not panic. Callers may call Refine
// without checking the refiner's wiring state.
func TestRefine_NilProviderReturnsError(t *testing.T) {
	var r *LLMRefiner
	_, err := r.Refine(context.Background(), "t", `{}`, Verdict{})
	if err == nil {
		t.Error("nil receiver: expected error, got nil")
	}
	r = &LLMRefiner{Provider: nil}
	_, err = r.Refine(context.Background(), "t", `{}`, Verdict{})
	if err == nil {
		t.Error("nil provider: expected error, got nil")
	}
}

// TestStripJSONFence — small helper, lots of edge cases. Covers
// the shapes models actually produce: bare object, ```json“,
// bare ```, leading whitespace.
func TestStripJSONFence(t *testing.T) {
	cases := map[string]string{
		`{"a":1}`:                      `{"a":1}`,
		"```json\n{\"a\":1}\n```":      `{"a":1}`,
		"```\n{\"a\":1}\n```":          `{"a":1}`,
		"  ```json\n{\"a\":1}\n```   ": `{"a":1}`,
		`no fences here, just text`:    `no fences here, just text`,
	}
	for in, want := range cases {
		if got := stripJSONFence(in); got != want {
			t.Errorf("stripJSONFence(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeResponseCache is the smallest in-memory ResponseCache
// that exercises the refiner's Get/Put paths. records counts so
// tests can assert "the cache was actually called" without
// instrumenting the provider.
type fakeResponseCache struct {
	store  map[string]string
	getN   int
	putN   int
	putErr error
	getErr error
}

func newFakeResponseCache() *fakeResponseCache {
	return &fakeResponseCache{store: map[string]string{}}
}

func (f *fakeResponseCache) Get(_ context.Context, key string) (string, int, int, bool, error) {
	f.getN++
	if f.getErr != nil {
		return "", 0, 0, false, f.getErr
	}
	v, ok := f.store[key]
	return v, 0, 0, ok, nil
}

func (f *fakeResponseCache) Put(_ context.Context, key, _, _, content string, _, _ int) error {
	f.putN++
	if f.putErr != nil {
		return f.putErr
	}
	f.store[key] = content
	return nil
}

// recordingProvider wraps stubProvider with a call counter so tests
// can assert the LLM was NOT called on cache hit. Defaults to the
// same response shape as stubProvider.
type recordingProvider struct {
	stubProvider
	completeN int
}

func (r *recordingProvider) Complete(ctx context.Context, msgs []chat.Message) (*chat.ChatResponse, error) {
	r.completeN++
	return r.stubProvider.Complete(ctx, msgs)
}

// TestRefine_NilCache_NoOp — refiner without a cache wired
// behaves identically to pre-cache: one LLM call, no panic, no
// state observed.
func TestRefine_NilCache_NoOp(t *testing.T) {
	provider := &recordingProvider{stubProvider: stubProvider{
		resp: respWith(`{"risk":"low","confidence":0.9,"recommendation":"approve","reasoning":"ok"}`),
	}}
	r := &LLMRefiner{Provider: provider}
	if _, err := r.Refine(context.Background(), "current_time", `{}`, Verdict{Risk: RiskLow}); err != nil {
		t.Fatalf("Refine: %v", err)
	}
	if provider.completeN != 1 {
		t.Errorf("LLM called %d times, want 1", provider.completeN)
	}
}

// TestRefine_CacheMiss_PopulatesCache — first call with a cache
// wired should miss + populate. Asserts: LLM called once, Put
// called once, Get called once.
func TestRefine_CacheMiss_PopulatesCache(t *testing.T) {
	provider := &recordingProvider{stubProvider: stubProvider{
		resp: respWith(`{"risk":"high","confidence":0.9,"recommendation":"deny","reasoning":"shell"}`),
	}}
	cache := newFakeResponseCache()
	r := &LLMRefiner{Provider: provider, Cache: cache}
	if _, err := r.Refine(context.Background(), "run_shell", `{"cmd":"x"}`, Verdict{Risk: RiskMedium}); err != nil {
		t.Fatalf("Refine: %v", err)
	}
	if provider.completeN != 1 {
		t.Errorf("LLM called %d times, want 1", provider.completeN)
	}
	if cache.getN != 1 {
		t.Errorf("cache.Get called %d times, want 1", cache.getN)
	}
	if cache.putN != 1 {
		t.Errorf("cache.Put called %d times, want 1", cache.putN)
	}
	if len(cache.store) != 1 {
		t.Errorf("cache should hold 1 entry, has %d", len(cache.store))
	}
}

// TestRefine_CacheHit_SkipsLLM — second call with the same
// (tool, args, heuristic) inputs and a populated cache must NOT
// call the LLM. Pins the actual savings the expansion delivers.
func TestRefine_CacheHit_SkipsLLM(t *testing.T) {
	provider := &recordingProvider{stubProvider: stubProvider{
		resp: respWith(`{"risk":"high","confidence":0.9,"recommendation":"deny","reasoning":"shell"}`),
	}}
	cache := newFakeResponseCache()
	r := &LLMRefiner{Provider: provider, Cache: cache}

	// Warm the cache.
	tool, args, heuristic := "run_shell", `{"cmd":"x"}`, Verdict{Risk: RiskMedium}
	if _, err := r.Refine(context.Background(), tool, args, heuristic); err != nil {
		t.Fatalf("Refine 1: %v", err)
	}
	llmCallsAfterWarmup := provider.completeN

	// Same inputs → expect a hit. LLM call count must NOT increase.
	got, err := r.Refine(context.Background(), tool, args, heuristic)
	if err != nil {
		t.Fatalf("Refine 2: %v", err)
	}
	if got.Risk != RiskHigh {
		t.Errorf("cache hit produced wrong verdict: %+v", got)
	}
	if provider.completeN != llmCallsAfterWarmup {
		t.Errorf("LLM was called on cache hit: %d → %d", llmCallsAfterWarmup, provider.completeN)
	}
	if cache.getN < 2 {
		t.Errorf("cache.Get called %d times, want >= 2", cache.getN)
	}
}

// TestRefine_PutErrorSwallowed — a cache Put failure must NOT
// cause Refine to return an error. The verdict is still returned;
// only the cache write was lost. Same broken-cache discipline as
// memory.Titler / memory.Classifier.
func TestRefine_PutErrorSwallowed(t *testing.T) {
	provider := &recordingProvider{stubProvider: stubProvider{
		resp: respWith(`{"risk":"low","confidence":0.9,"recommendation":"approve","reasoning":"ok"}`),
	}}
	cache := newFakeResponseCache()
	cache.putErr = errors.New("cache disk full")
	r := &LLMRefiner{Provider: provider, Cache: cache}
	if _, err := r.Refine(context.Background(), "current_time", `{}`, Verdict{Risk: RiskLow}); err != nil {
		t.Errorf("cache Put error must not abort Refine: %v", err)
	}
}

// TestRefine_MalformedResponseNotCached — a malformed LLM reply
// must NOT poison the cache. Pins the "cache only fully-
// validated responses" contract from the Put branch.
func TestRefine_MalformedResponseNotCached(t *testing.T) {
	provider := &recordingProvider{stubProvider: stubProvider{
		resp: respWith(`not json at all`),
	}}
	cache := newFakeResponseCache()
	r := &LLMRefiner{Provider: provider, Cache: cache}
	if _, err := r.Refine(context.Background(), "x", `{}`, Verdict{Risk: RiskLow}); err == nil {
		t.Fatal("expected parse error on malformed response")
	}
	if cache.putN != 0 {
		t.Errorf("malformed response should NOT be cached; got %d puts", cache.putN)
	}
}
