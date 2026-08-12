package memory

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/llmspend"
	"vornik.io/vornik/internal/persistence"
)

// Slice 2 of the embed-spend-attribution design: the ledger write.
//
// The invariant these tests exist for is RECORD BEFORE CLASSIFYING THE RESULT.
// The provider charged the moment it returned; a row written only after the
// response parses turns every degrade path into a silent spender. That is not
// hypothetical on this codebase — it is how the KG extractor laundered ~83% of
// its spend into "nothing found", and how the reranker's spend stayed invisible
// while its degrade-to-RRF path was working as designed.

func embedderWithRecorder(t *testing.T, url string) (*Embedder, *fakeUsageRecorder) {
	t.Helper()
	rec := &fakeUsageRecorder{}
	e := NewEmbedder(Config{EmbeddingEndpoint: url, EmbeddingModel: "embed-m"})
	e.SetSpend(llmspend.New(rec, fixedPricing{},
		persistence.TaskLLMUsageSourceMemoryEmbedder, RoleEmbedder))
	return e, rec
}

func ingestScope() EmbedScope {
	return EmbedScope{ProjectID: "janka", CallSite: EmbedCallSiteIngest}
}

// TestEmbedUsage_MeasuredTokensFromProvider: when the provider reports token
// counts, they are recorded as measured and TokensEstimated stays false.
func TestEmbedUsage_MeasuredTokensFromProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.1]}],"usage":{"prompt_tokens":42}}`))
	}))
	defer srv.Close()

	e, rec := embedderWithRecorder(t, srv.URL)
	if _, err := e.Embed(context.Background(), ingestScope(), []string{"hello"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}

	if len(rec.rows) != 1 {
		t.Fatalf("got %d usage rows, want 1", len(rec.rows))
	}
	row := rec.rows[0]
	if row.PromptTokens != 42 {
		t.Errorf("PromptTokens = %d, want 42 (the provider's own count)", row.PromptTokens)
	}
	if row.TokensEstimated {
		t.Error("TokensEstimated = true for a provider-reported count — that would make a measurement look like a guess")
	}
	if row.Source != persistence.TaskLLMUsageSourceMemoryEmbedder {
		t.Errorf("Source = %q, want %q", row.Source, persistence.TaskLLMUsageSourceMemoryEmbedder)
	}
	if row.ProjectID != "janka" {
		t.Errorf("ProjectID = %q, want the scope's project", row.ProjectID)
	}
	// Retrieval- and ingest-side spend is not task-scoped; the reranker
	// established this and a synthetic task id could collide with a real one.
	if row.TaskID != nil {
		t.Errorf("TaskID = %v, want nil", *row.TaskID)
	}
	if row.CompletionTokens != 0 {
		t.Errorf("CompletionTokens = %d, want 0 — embeddings generate no output tokens", row.CompletionTokens)
	}
	// fixedPricing bills $1/1k prompt tokens, so 42 tokens = $0.042.
	if row.CostUSD <= 0 {
		t.Errorf("CostUSD = %v, want > 0 with a pricing table wired", row.CostUSD)
	}
}

// TestEmbedUsage_EstimatedWhenProviderReportsNoTokens: Bedrock Cohere returns no
// token count at all. The choice is estimate-and-flag or record nothing, and
// recording nothing is the original defect.
func TestEmbedUsage_EstimatedWhenProviderReportsNoTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.1]}]}`))
	}))
	defer srv.Close()

	e, rec := embedderWithRecorder(t, srv.URL)
	// Long enough that a bytes-based estimate is unambiguously non-zero.
	text := "the quick brown fox jumps over the lazy dog, repeatedly and at length"
	if _, err := e.Embed(context.Background(), ingestScope(), []string{text}); err != nil {
		t.Fatalf("Embed: %v", err)
	}

	if len(rec.rows) != 1 {
		t.Fatalf("got %d usage rows, want 1", len(rec.rows))
	}
	row := rec.rows[0]
	if !row.TokensEstimated {
		t.Error("TokensEstimated = false for a derived count — the ledger must not present a guess as a measurement")
	}
	if row.PromptTokens <= 0 {
		t.Errorf("PromptTokens = %d, want a positive estimate", row.PromptTokens)
	}
}

// TestEmbedUsage_RecordedEvenWhenTheResponseIsUnusable is the laundering guard,
// and the reason recording is placed where it is.
//
// A 200 with a body the vector parser cannot use means the provider charged and
// we got nothing usable. Embed correctly degrades to (nil, nil) — but the spend
// is real and must still appear. If this test fails, the embed path has the exact
// defect the KG extractor and the reranker each shipped once.
func TestEmbedUsage_RecordedEvenWhenTheResponseIsUnusable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Well-formed usage, unusable payload: `data` is not the expected shape.
		_, _ = w.Write([]byte(`{"data":"not-an-array","usage":{"prompt_tokens":99}}`))
	}))
	defer srv.Close()

	e, rec := embedderWithRecorder(t, srv.URL)
	vecs, err := e.Embed(context.Background(), ingestScope(), []string{"hello"})
	if err != nil {
		t.Fatalf("Embed should degrade, not error: %v", err)
	}
	if vecs != nil {
		t.Errorf("expected the degrade path (nil vectors), got %v", vecs)
	}
	if len(rec.rows) != 1 {
		t.Fatalf("got %d usage rows, want 1 — a charged call was laundered into 'nothing happened'", len(rec.rows))
	}
}

// TestEmbedUsage_CacheHitRecordsNothing: the cache short-circuits before the
// provider, so no money was spent and no row belongs in the ledger. Recording
// one would invent spend — the double-counting risk the design review raised
// about placing recording inside a wrapper.
func TestEmbedUsage_CacheHitRecordsNothing(t *testing.T) {
	var providerCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.4]}],"usage":{"prompt_tokens":7}}`))
	}))
	defer srv.Close()

	e, rec := embedderWithRecorder(t, srv.URL)
	e.Cache = newFakeEmbedCache()

	// First call populates the cache and bills once.
	if _, err := e.Embed(context.Background(), ingestScope(), []string{"same text"}); err != nil {
		t.Fatalf("first Embed: %v", err)
	}
	if len(rec.rows) != 1 {
		t.Fatalf("after the miss: got %d rows, want 1", len(rec.rows))
	}

	// Second call is a pure cache hit: no provider call, so no new row.
	if _, err := e.Embed(context.Background(), ingestScope(), []string{"same text"}); err != nil {
		t.Fatalf("second Embed: %v", err)
	}
	if providerCalls != 1 {
		t.Fatalf("provider called %d times, want 1 — the cache did not serve the second call", providerCalls)
	}
	if len(rec.rows) != 1 {
		t.Errorf("cache hit added a usage row (%d total) — that invents spend that never happened", len(rec.rows))
	}
}

// TestEmbedUsage_NilRecorderIsSafe: an unwired recorder must not break
// embedding. Failing to bill is a fidelity loss; failing to embed is an outage.
func TestEmbedUsage_NilRecorderIsSafe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.2]}],"usage":{"prompt_tokens":5}}`))
	}))
	defer srv.Close()

	e := NewEmbedder(Config{EmbeddingEndpoint: srv.URL, EmbeddingModel: "embed-m"})
	vecs, err := e.Embed(context.Background(), ingestScope(), []string{"hello"})
	if err != nil {
		t.Fatalf("Embed with no recorder: %v", err)
	}
	if len(vecs) != 1 || len(vecs[0]) != 1 {
		t.Errorf("embedding must still work without a recorder, got %v", vecs)
	}
}

// TestEmbedderUsageWiring_IsGuardedBySource pins that the production wiring line
// exists at all.
//
// Three times on this codebase a call site made real LLM calls and recorded no
// spend, and every one was a WIRING failure rather than a logic failure: the
// instinct distiller's field was assigned only in a test, and the reranker had
// no field at all. A nil recorder records nothing without complaining, so the
// only thing standing between "billed" and "silently free" is one assignment in
// the container — and nothing in the type system notices when it disappears.
//
// This asserts the assignment is present in the container source. It is a blunt
// check, deliberately: a subtler one that mocked the container would pass while
// the real wiring was gone.
func TestEmbedderUsageWiring_IsGuardedBySource(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "service", "container_scheduler.go"))
	if err != nil {
		t.Fatalf("read container_scheduler.go: %v", err)
	}
	// NOTE: this guard SURVIVES the llmspend migration where the registry entries
	// for other components did not, and the difference is real. Titler and friends
	// take their Recorder as a CONSTRUCTOR argument, so the compiler forces it.
	// The Embedder is built by memory.NewManager from config alone — which has no
	// usage repo — so its recorder can only arrive afterwards via SetSpend, and a
	// call that must happen but is not a constructor argument is exactly what a
	// compiler cannot check. Hence: still text-guarded, deliberately.
	for _, want := range []string{
		"mgr.Embedder.SetSpend(",
		"persistence.TaskLLMUsageSourceMemoryEmbedder, memory.RoleEmbedder",
	} {
		if !strings.Contains(string(src), want) {
			t.Errorf("container_scheduler.go no longer contains %q — embedding spend is unbilled again", want)
		}
	}
}

// TestEmbedUsage_TransportFailureRecordsNothing pins the other side of the
// billing boundary, and it exists because a reviewer read the asymmetry as a bug.
//
// A 200 we cannot parse DOES bill (the provider delivered and charged) —
// TestEmbedUsage_RecordedEvenWhenTheResponseIsUnusable covers that. A non-200
// does NOT: no inference was delivered, so nothing was charged, and recording an
// estimate would fabricate spend. Inventing charges is the same class of error as
// billing a cache hit, and a ledger that makes up numbers is no better evidence
// than one that loses them.
//
// Without this test the asymmetry looks like an oversight and the "fix" — record
// on every provider contact — would quietly start inflating the bill.
func TestEmbedUsage_TransportFailureRecordsNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"rate limited", http.StatusTooManyRequests},
		{"server error", http.StatusInternalServerError},
		{"bad gateway", http.StatusBadGateway},
		{"rejected request", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			e, rec := embedderWithRecorder(t, srv.URL)
			vecs, err := e.Embed(context.Background(), ingestScope(), []string{"hello"})
			if err != nil {
				t.Fatalf("Embed should degrade quietly, not error: %v", err)
			}
			if vecs != nil {
				t.Errorf("expected the degrade path, got %v", vecs)
			}
			if len(rec.rows) != 0 {
				t.Errorf("HTTP %d produced %d usage rows — no inference was delivered, so billing it "+
					"fabricates spend", tc.status, len(rec.rows))
			}
		})
	}
}
