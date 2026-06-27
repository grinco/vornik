package memory

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"vornik.io/vornik/internal/chat"
)

// memory_wave2_test.go — second-pass HIGH-VALUE unit tests for the
// pure / fake-injectable logic in internal/memory. All names are
// prefixed TestW2Mem so they can be run in isolation:
//
//	go test ./internal/memory/ -run TestW2Mem
//
// Focus areas (gaps left by the existing suite):
//   - Embedder cache + upstream interaction: partial-hit miss-index
//     reassembly, cache populate on success, Get/Put error tolerance,
//     batch-boundary keying. (The existing embedding_cache_test only
//     covers the all-hits and no-cache short-circuits.)
//   - chunkID dedup-keying field independence + delimiter safety.
//   - vectorLiteral <-> parseVectorLiteral round-trip fidelity.
//   - SoftMatchClaim fuzzy (jaccard) scoring path.
//   - isCompanionProducer character-class boundaries.
//   - cleanNarrative quote/whitespace edges not yet pinned.

// ----------------------------------------------------------------------
// Embedder cache + upstream integration
// ----------------------------------------------------------------------

// w2EmbedCache is a configurable fake EmbedCache. Unlike the existing
// fakeEmbedCache it can be primed to return errors from Get/Put so the
// "cache must never block upstream" contract can be asserted, and it
// records the exact (hash, model) keys it was asked to Put.
type w2EmbedCache struct {
	mu       sync.Mutex
	data     map[string][]float32
	getErr   bool
	putErr   bool
	gets     int
	puts     int
	putKeys  []string
	getCalls []string
}

func newW2EmbedCache() *w2EmbedCache {
	return &w2EmbedCache{data: map[string][]float32{}}
}

func (f *w2EmbedCache) Get(_ context.Context, hash, model string) ([]float32, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	f.getCalls = append(f.getCalls, hash+":"+model)
	if f.getErr {
		// Contract: storage error returns nil,false,err — the embedder
		// must treat this as a miss and fall through to upstream.
		return nil, false, context.DeadlineExceeded
	}
	v, ok := f.data[hash+":"+model]
	if !ok {
		return nil, false, nil
	}
	return v, true, nil
}

func (f *w2EmbedCache) Put(_ context.Context, hash, model string, vec []float32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts++
	if f.putErr {
		return context.DeadlineExceeded
	}
	f.putKeys = append(f.putKeys, hash+":"+model)
	f.data[hash+":"+model] = append([]float32(nil), vec...)
	return nil
}

// w2EmbedServer returns an httptest server that echoes one vector per
// input text whose single component is the FNV-ish hash of the text, so
// each text gets a distinct, verifiable vector. It records the batches
// it received so callers can assert exactly which texts reached the
// upstream (i.e. were genuine misses). Data is returned reversed to keep
// exercising the sort-by-index reassembly.
func w2EmbedServer(t *testing.T, batches *[][]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req embeddingRequest
		if err := json.Unmarshal(b, &req); err != nil {
			t.Errorf("bad request body: %v", err)
		}
		if batches != nil {
			*batches = append(*batches, append([]string(nil), req.Input...))
		}
		var resp embeddingResponse
		for i := len(req.Input) - 1; i >= 0; i-- {
			resp.Data = append(resp.Data, struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			}{Index: i, Embedding: []float32{w2TextMark(req.Input[i])}})
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

// w2Embedder builds an Embedder via NewEmbedder (so its *http.Client is
// wired) and attaches the given cache + endpoint/model. Struct-literal
// construction would leave client nil and panic on the upstream call.
func w2Embedder(endpoint, model string, cache EmbedCache) *Embedder {
	e := NewEmbedder(Config{EmbeddingEndpoint: endpoint, EmbeddingModel: model})
	e.Cache = cache
	return e
}

// w2TextMark maps a text to a deterministic float marker so the test can
// assert "this output slot holds the vector for that input text".
func w2TextMark(s string) float32 {
	var n int32 = 7
	for _, r := range s {
		n = n*31 + r
	}
	return float32(n)
}

func TestW2MemEmbedderPartialHitReassemblesByOriginalIndex(t *testing.T) {
	var batches [][]string
	srv := w2EmbedServer(t, &batches)
	defer srv.Close()

	cache := newW2EmbedCache()
	// Pre-seed cache for "b" and "d" only; "a", "c", "e" are misses.
	cache.data[ContentHash("b")+":m"] = []float32{100}
	cache.data[ContentHash("d")+":m"] = []float32{200}

	e := w2Embedder(srv.URL, "m", cache)
	in := []string{"a", "b", "c", "d", "e"}
	out, err := e.Embed(context.Background(), in)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(out) != 5 {
		t.Fatalf("len(out)=%d want 5", len(out))
	}
	// Cached slots keep their cached values at the ORIGINAL index.
	if len(out[1]) != 1 || out[1][0] != 100 {
		t.Errorf("out[1] (cached b) = %v, want [100]", out[1])
	}
	if len(out[3]) != 1 || out[3][0] != 200 {
		t.Errorf("out[3] (cached d) = %v, want [200]", out[3])
	}
	// Miss slots carry the upstream marker for THEIR text — proves the
	// missIndices remap put each upstream vector back at the right slot.
	for i, txt := range in {
		if i == 1 || i == 3 {
			continue
		}
		if len(out[i]) != 1 || out[i][0] != w2TextMark(txt) {
			t.Errorf("out[%d] (%q) = %v, want [%v]", i, txt, out[i], w2TextMark(txt))
		}
	}
	// Exactly one upstream batch carrying exactly the three misses, in order.
	if len(batches) != 1 {
		t.Fatalf("upstream batches=%d want 1", len(batches))
	}
	if want := []string{"a", "c", "e"}; !reflect.DeepEqual(batches[0], want) {
		t.Fatalf("upstream batch = %v, want %v (contiguous misses, original order)", batches[0], want)
	}
}

func TestW2MemEmbedderPopulatesCacheForMissesOnly(t *testing.T) {
	srv := w2EmbedServer(t, nil)
	defer srv.Close()

	cache := newW2EmbedCache()
	cache.data[ContentHash("hit")+":m"] = []float32{42}

	e := w2Embedder(srv.URL, "m", cache)
	if _, err := e.Embed(context.Background(), []string{"hit", "miss1", "miss2"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	// Only the two misses are written back; the pre-existing hit is not
	// re-Put (it never went through the upstream populate loop).
	if cache.puts != 2 {
		t.Fatalf("puts=%d want 2 (one per miss)", cache.puts)
	}
	wantKeys := map[string]bool{
		ContentHash("miss1") + ":m": true,
		ContentHash("miss2") + ":m": true,
	}
	for _, k := range cache.putKeys {
		if !wantKeys[k] {
			t.Errorf("unexpected Put key %q", k)
		}
		delete(wantKeys, k)
	}
	if len(wantKeys) != 0 {
		t.Errorf("missing Put keys: %v", wantKeys)
	}
	// The populated vector for miss1 must equal its upstream marker.
	if got := cache.data[ContentHash("miss1")+":m"]; len(got) != 1 || got[0] != w2TextMark("miss1") {
		t.Errorf("cached miss1 = %v, want [%v]", got, w2TextMark("miss1"))
	}
}

func TestW2MemEmbedderGetErrorTreatedAsMiss(t *testing.T) {
	var batches [][]string
	srv := w2EmbedServer(t, &batches)
	defer srv.Close()

	cache := newW2EmbedCache()
	cache.getErr = true // every Get errors
	// Even though we "have" the data, getErr forces a miss.
	cache.data[ContentHash("x")+":m"] = []float32{999}

	e := w2Embedder(srv.URL, "m", cache)
	out, err := e.Embed(context.Background(), []string{"x"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	// Get errored -> miss -> upstream -> marker (not the stale 999).
	if len(out) != 1 || out[0][0] != w2TextMark("x") {
		t.Fatalf("out=%v want upstream marker [%v]; Get error must not serve stale cache", out, w2TextMark("x"))
	}
	if len(batches) != 1 || len(batches[0]) != 1 || batches[0][0] != "x" {
		t.Fatalf("expected upstream fallthrough on Get error, batches=%v", batches)
	}
}

func TestW2MemEmbedderPutErrorDoesNotFailEmbed(t *testing.T) {
	srv := w2EmbedServer(t, nil)
	defer srv.Close()

	cache := newW2EmbedCache()
	cache.putErr = true // populate write fails

	e := w2Embedder(srv.URL, "m", cache)
	out, err := e.Embed(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("Put error must be swallowed, got err=%v", err)
	}
	if len(out) != 2 || out[0][0] != w2TextMark("a") || out[1][0] != w2TextMark("b") {
		t.Fatalf("results should still come back despite Put failure: %v", out)
	}
}

func TestW2MemEmbedderCacheKeyedByModelNamespace(t *testing.T) {
	// A vector cached under model "m1" must NOT satisfy a request for
	// model "m2": embeddings are model-bound. Same text, different model
	// -> miss -> upstream.
	var batches [][]string
	srv := w2EmbedServer(t, &batches)
	defer srv.Close()

	cache := newW2EmbedCache()
	cache.data[ContentHash("doc")+":m1"] = []float32{1}

	e := w2Embedder(srv.URL, "m2", cache)
	out, err := e.Embed(context.Background(), []string{"doc"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if out[0][0] != w2TextMark("doc") {
		t.Fatalf("expected upstream miss for model m2, got %v", out)
	}
	if len(batches) != 1 {
		t.Fatalf("expected one upstream batch (model mismatch is a miss), got %d", len(batches))
	}
	// The Get lookup was keyed on m2, not m1.
	if len(cache.getCalls) != 1 || cache.getCalls[0] != ContentHash("doc")+":m2" {
		t.Fatalf("Get key = %v, want %q", cache.getCalls, ContentHash("doc")+":m2")
	}
}

func TestW2MemEmbedderDuplicateTextsBothMissAndPopulate(t *testing.T) {
	// Two identical texts are independent slots in the result; with an
	// empty cache both are misses (the embedder does not de-dup within a
	// single Embed call), and the populate writes the same key twice
	// idempotently. Output ordering is preserved.
	var batches [][]string
	srv := w2EmbedServer(t, &batches)
	defer srv.Close()

	cache := newW2EmbedCache()
	e := w2Embedder(srv.URL, "m", cache)
	out, err := e.Embed(context.Background(), []string{"same", "same"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(out) != 2 || out[0][0] != w2TextMark("same") || out[1][0] != w2TextMark("same") {
		t.Fatalf("both slots should hold the marker: %v", out)
	}
	if len(batches) != 1 || len(batches[0]) != 2 {
		t.Fatalf("both duplicates should reach upstream as misses: %v", batches)
	}
}

func TestW2MemEmbedderUpstreamFailureReturnsNilNotPartial(t *testing.T) {
	// When the upstream batch errors mid-flight, Embed degrades to
	// (nil, nil) for the WHOLE call rather than returning a partially
	// populated slice — callers treat nil as "no embeddings this round".
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cache := newW2EmbedCache()
	cache.data[ContentHash("cached")+":m"] = []float32{5}
	e := w2Embedder(srv.URL, "m", cache)
	out, err := e.Embed(context.Background(), []string{"cached", "miss"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out != nil {
		t.Fatalf("expected nil on upstream failure, got %v", out)
	}
}

func TestW2MemEmbedderAllHitsSkipsUpstreamEntirely(t *testing.T) {
	// Mirrors the all-hits short-circuit but verifies via a LIVE server
	// that it is never contacted (the existing test pointed at a bogus
	// address). If the upstream were reached this would record a batch.
	var batches [][]string
	srv := w2EmbedServer(t, &batches)
	defer srv.Close()

	cache := newW2EmbedCache()
	cache.data[ContentHash("one")+":m"] = []float32{11}
	cache.data[ContentHash("two")+":m"] = []float32{22}

	e := w2Embedder(srv.URL, "m", cache)
	out, err := e.Embed(context.Background(), []string{"one", "two"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if out[0][0] != 11 || out[1][0] != 22 {
		t.Fatalf("cached values not returned: %v", out)
	}
	if len(batches) != 0 {
		t.Fatalf("upstream must not be contacted on all-hits; got %v", batches)
	}
	if cache.puts != 0 {
		t.Fatalf("no Puts on all-hits, got %d", cache.puts)
	}
}

func TestW2MemEmbedderMissesSpanBatchBoundaryWithCache(t *testing.T) {
	// 600 inputs, every even index pre-cached. The ~300 misses exceed
	// maxEmbedBatch (512)? No — 300 < 512, so they fit one batch. To
	// force a boundary we cache none and send 513 inputs: misses split
	// 512 + 1 across two upstream calls, and every slot must still carry
	// its own marker. This pins that the missIndices remap survives the
	// batch loop's start/end windowing.
	var batches [][]string
	srv := w2EmbedServer(t, &batches)
	defer srv.Close()

	cache := newW2EmbedCache()
	n := maxEmbedBatch + 1 // 513
	in := make([]string, n)
	for i := range in {
		in[i] = "tok-" + string(rune('A'+i%26)) + "-" + itoaW2(i)
	}
	e := w2Embedder(srv.URL, "m", cache)
	out, err := e.Embed(context.Background(), in)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(out) != n {
		t.Fatalf("len(out)=%d want %d", len(out), n)
	}
	if len(batches) != 2 {
		t.Fatalf("expected 2 upstream batches across the 512 boundary, got %d", len(batches))
	}
	// Spot-check the boundary slots: 511, 512 (first of second batch),
	// and the last index all carry their own text's marker.
	for _, i := range []int{0, maxEmbedBatch - 1, maxEmbedBatch, n - 1} {
		if len(out[i]) != 1 || out[i][0] != w2TextMark(in[i]) {
			t.Errorf("out[%d] = %v, want marker for %q ([%v])", i, out[i], in[i], w2TextMark(in[i]))
		}
	}
}

// itoaW2 is a tiny base-10 itoa so the boundary test can build unique
// inputs without importing strconv solely for one call site.
func itoaW2(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}

// ----------------------------------------------------------------------
// chunkID dedup-keying
// ----------------------------------------------------------------------

func TestW2MemChunkIDFieldIndependence(t *testing.T) {
	base := chunkID("proj", "art", "src.md", 0)
	cases := map[string]string{
		"project differs":  chunkID("PROJ", "art", "src.md", 0),
		"artifact differs": chunkID("proj", "ART", "src.md", 0),
		"source differs":   chunkID("proj", "art", "other.md", 0),
		"index differs":    chunkID("proj", "art", "src.md", 1),
	}
	seen := map[string]string{base: "base"}
	for label, id := range cases {
		if len(id) != 32 {
			t.Errorf("%s: id %q not 32 hex chars", label, id)
		}
		if prev, dup := seen[id]; dup {
			t.Errorf("%s collides with %s (both %q)", label, prev, id)
		}
		seen[id] = label
	}
}

func TestW2MemChunkIDColonDelimiterDoesNotAlias(t *testing.T) {
	// chunkID now escapes ':'/'\' within each segment, so a colon embedded in
	// a field can no longer shift the delimiter boundary and alias two
	// distinct tuples. (Pre-fix, both of these collapsed to "a:b:c:d:0" and
	// hashed identically — see BACKLOG 2026-06-18.)
	a := chunkID("a", "b:c", "d", 0)
	b := chunkID("a:b", "c", "d", 0)
	if a == b {
		t.Fatalf("colon in a field must not alias distinct tuples after escaping: %q == %q", a, b)
	}
	// Sanity: colon-free fields still key uniquely too.
	if chunkID("a", "bc", "d", 0) == chunkID("ab", "c", "d", 0) {
		t.Fatal("colon-free fields must not alias")
	}
}

// TestW2MemChunkIDEscapingPreservesColonFreeIDs pins backward-compat: for the
// normal (colon/backslash-free) inputs the escape is a no-op, so the id is
// byte-identical to the pre-escaping formula — existing chunks keep their ids.
func TestW2MemChunkIDEscapingPreservesColonFreeIDs(t *testing.T) {
	got := chunkID("proj", "art", "src.md", 3)
	want := sha256.Sum256([]byte("proj:art:src.md:3")) // the original unescaped raw
	if got != fmt.Sprintf("%x", want[:16]) {
		t.Fatalf("colon-free id changed (would churn existing chunks): got %s", got)
	}
}

func TestW2MemChunkIDDeterministicAcrossCalls(t *testing.T) {
	want := chunkID("p", "a", "s", 7)
	for i := 0; i < 5; i++ {
		if got := chunkID("p", "a", "s", 7); got != want {
			t.Fatalf("chunkID not deterministic: call %d gave %q, want %q", i, got, want)
		}
	}
}

// ----------------------------------------------------------------------
// vectorLiteral <-> parseVectorLiteral round-trip
// ----------------------------------------------------------------------

func TestW2MemVectorLiteralRoundTrip(t *testing.T) {
	cases := [][]float32{
		{0},
		{1, 2, 3},
		{-1.5, 0.25, 100.125},
		{0.1, 0.2, 0.3, 0.4, 0.5},
	}
	for _, vec := range cases {
		lit := vectorLiteral(vec)
		got := parseVectorLiteral(lit)
		if len(got) != len(vec) {
			t.Fatalf("round-trip len: %v -> %q -> %v", vec, lit, got)
		}
		for i := range vec {
			if got[i] != vec[i] {
				t.Errorf("round-trip[%d]: %q gave %v, want %v", i, lit, got[i], vec[i])
			}
		}
	}
}

func TestW2MemVectorLiteralEmptyRoundTrip(t *testing.T) {
	// Empty vector literalises to "[]" which parses back to nil (the
	// "no embedding" sentinel) — NOT a zero-length non-nil slice that a
	// caller might mistake for a real vector.
	if lit := vectorLiteral(nil); lit != "[]" {
		t.Fatalf("vectorLiteral(nil) = %q, want []", lit)
	}
	if got := parseVectorLiteral("[]"); got != nil {
		t.Fatalf("parseVectorLiteral([]) = %v, want nil", got)
	}
}

func TestW2MemParseVectorLiteralWhitespaceTolerant(t *testing.T) {
	// pgvector text format sometimes carries spaces after commas. The
	// per-element TrimSpace must absorb them.
	got := parseVectorLiteral("[ 1 , 2.5 ,-3 ]")
	want := []float32{1, 2.5, -3}
	if len(got) != 3 {
		t.Fatalf("parsed %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d]=%v want %v", i, got[i], want[i])
		}
	}
}

func TestW2MemParseVectorLiteralRejectsAnyBadElement(t *testing.T) {
	// A single unparseable element aborts the WHOLE vector to nil — the
	// caller treats a partial vector as "no embedding" rather than a
	// silently-truncated one. Covers the mid-slice and trailing-garbage
	// positions.
	for _, lit := range []string{"[1,2,x]", "[bad,2,3]", "[1,,3]", "[1 2 3]"} {
		if got := parseVectorLiteral(lit); got != nil {
			t.Errorf("parseVectorLiteral(%q) = %v, want nil (any bad element voids the vector)", lit, got)
		}
	}
}

// ----------------------------------------------------------------------
// NarrativeWriter.Write — prompt assembly reaches the LLM
// ----------------------------------------------------------------------

// w2CapturingProvider records the messages of the last Complete call so
// the test can assert what prompt body the NarrativeWriter assembled and
// sent. Embeds titlerFakeProvider to inherit the CompleteWithTools /
// Model / SetMetrics stubs that satisfy chat.Provider; overrides only
// Complete to capture + return a fixed, parseable narrative.
type w2CapturingProvider struct {
	titlerFakeProvider
	mu   sync.Mutex
	last []chat.Message
}

func (p *w2CapturingProvider) Complete(_ context.Context, msgs []chat.Message) (*chat.ChatResponse, error) {
	p.mu.Lock()
	p.last = append([]chat.Message(nil), msgs...)
	p.mu.Unlock()
	resp := &chat.ChatResponse{Model: "fake"}
	resp.Choices = append(resp.Choices, struct {
		Index        int          `json:"index"`
		Message      chat.Message `json:"message"`
		FinishReason string       `json:"finish_reason"`
	}{Message: chat.Message{Role: "assistant", Content: "  A trading project.  "}, FinishReason: "stop"})
	return resp, nil
}

func TestW2MemNarrativeWriteAssemblesTermsAndSampleIntoUserPrompt(t *testing.T) {
	capP := &w2CapturingProvider{}
	w := NewNarrativeWriter(capP, "")
	terms := []TermFrequency{{Term: "trading", Count: 9}, {Term: "ibkr", Count: 4}}
	got, err := w.Write(context.Background(), terms, "Submitted bracket order for NVDA.", "")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Response is cleaned (trimmed) before return.
	if got != "A trading project." {
		t.Fatalf("cleaned narrative = %q, want %q", got, "A trading project.")
	}
	capP.mu.Lock()
	msgs := capP.last
	capP.mu.Unlock()
	if len(msgs) != 2 || msgs[0].Role != "system" || msgs[1].Role != "user" {
		t.Fatalf("message shape = %+v", msgs)
	}
	if msgs[0].Content != narrativeSystemPrompt {
		t.Fatalf("system prompt not the canonical narrativeSystemPrompt")
	}
	user := msgs[1].Content
	// The assembled user message carries the ranked terms (term=count)
	// and the sample under its delimiter.
	for _, want := range []string{"TOP_TERMS:", "trading=9", "ibkr=4", "SAMPLE:", "Submitted bracket order for NVDA."} {
		if !strings.Contains(user, want) {
			t.Errorf("user prompt missing %q; got:\n%s", want, user)
		}
	}
}

// ----------------------------------------------------------------------
// SoftMatchClaim — fuzzy (jaccard) scoring path
// ----------------------------------------------------------------------

func TestW2MemSoftMatchClaimFuzzyMatchReturnsJaccardScore(t *testing.T) {
	// "alpha beta gamma" is NOT a substring of the audit, but shares
	// tokens {alpha,beta} with it. tokenSet(audit)={alpha,beta,delta,
	// epsilon}; intersection=2, union=5 -> jaccard=0.4. With threshold
	// 0.3 it matches and the returned score is the jaccard value (NOT
	// the 1.0 reserved for substring hits).
	ok, score := SoftMatchClaim("alpha beta gamma", "alpha beta delta epsilon", 0.3)
	if !ok {
		t.Fatalf("expected fuzzy match at threshold 0.3 (jaccard 0.4)")
	}
	if score == 1.0 {
		t.Fatalf("fuzzy match must not report substring score 1.0")
	}
	if score < 0.39 || score > 0.41 {
		t.Fatalf("score = %v, want ~0.4", score)
	}
}

func TestW2MemSoftMatchClaimThresholdExactlyOneDisablesFuzzy(t *testing.T) {
	// threshold>=1 short-circuits the fuzzy branch entirely: only an
	// exact substring can match. Heavy token overlap with NO substring
	// (interleaved/extra words break contiguity) is rejected.
	if ok, _ := SoftMatchClaim("alpha beta gamma", "alpha xxx beta yyy gamma", 1.0); ok {
		t.Fatal("threshold 1.0 must not fuzzy-match even on heavy overlap (only substring)")
	}
	// But a true substring still wins with score 1.0 at threshold 1.0.
	if ok, score := SoftMatchClaim("alpha beta", "see alpha beta here", 1.0); !ok || score != 1.0 {
		t.Fatalf("substring should still match at threshold 1.0: ok=%v score=%v", ok, score)
	}
}

func TestW2MemSoftMatchClaimZeroThresholdUsesDefault(t *testing.T) {
	// threshold<=0 falls back to DefaultSoftClaimThreshold. Pick inputs
	// whose jaccard sits below the default so we get a clean reject,
	// proving the default (not 0) is in force — a literal-0 threshold
	// would accept ANY non-empty overlap.
	if DefaultSoftClaimThreshold <= 0 {
		t.Skip("default threshold not positive; case not meaningful")
	}
	ok, _ := SoftMatchClaim("alpha beta gamma delta", "alpha zulu yankee xray whiskey", 0)
	if ok {
		t.Fatalf("low-overlap pair should reject under the positive default threshold")
	}
}

// ----------------------------------------------------------------------
// isCompanionProducer — character-class boundaries
// ----------------------------------------------------------------------

func TestW2MemIsCompanionProducerBoundaries(t *testing.T) {
	cases := []struct {
		role string
		want bool
	}{
		{"companion:claude-code", true},
		{"companion:codex2", true},  // digit allowed at i>0
		{"companion:a", true},       // single lowercase rest
		{"companion:", false},       // empty rest
		{"companion", false},        // no colon / too short
		{"companion:Claude", false}, // uppercase rejected
		{"companion:1codex", false}, // digit at position 0 rejected
		{"companion:-x", false},     // dash at position 0 rejected
		{"companion:a_b", false},    // underscore not allowed
		{"companion:a b", false},    // space not allowed
		{"researcher", false},       // unrelated role
		{"COMPANION:x", false},      // prefix is case-sensitive
	}
	for _, tc := range cases {
		if got := isCompanionProducer(tc.role); got != tc.want {
			t.Errorf("isCompanionProducer(%q) = %v, want %v", tc.role, got, tc.want)
		}
	}
}

// ----------------------------------------------------------------------
// cleanNarrative — quote / whitespace edges
// ----------------------------------------------------------------------

func TestW2MemCleanNarrativeStripsSingleQuotePair(t *testing.T) {
	if got := cleanNarrative("'a quoted summary'"); got != "a quoted summary" {
		t.Errorf("single-quote strip: %q", got)
	}
	if got := cleanNarrative(`"double quoted"`); got != "double quoted" {
		t.Errorf("double-quote strip: %q", got)
	}
}

func TestW2MemCleanNarrativeMismatchedQuotesNotStripped(t *testing.T) {
	// Leading quote but a different trailing char: the pair guard
	// (s[len-1]==s[0]) fails, so nothing is stripped beyond whitespace.
	if got := cleanNarrative(`"unterminated`); got != `"unterminated` {
		t.Errorf("mismatched quote should be preserved, got %q", got)
	}
	// A lone quote char (len 1) must not index out of range.
	if got := cleanNarrative(`"`); got != `"` {
		t.Errorf("lone quote: %q", got)
	}
}

func TestW2MemCleanNarrativeCollapsesNewlinesAndTabs(t *testing.T) {
	in := "An\n\nautomated   trading\tproject."
	if got := cleanNarrative(in); got != "An automated trading project." {
		t.Errorf("whitespace collapse: %q", got)
	}
}
