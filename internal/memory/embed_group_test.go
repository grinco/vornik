package memory

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// The load-bearing guard from §8.4 of the embed-spend-attribution design.
//
// Repository.DequeueEmbedBatch claims work ordered by enqueued_at with NO
// project filter (repository.go:370), so one batch routinely mixes projects. A
// single provider call for the whole batch could only name ONE project in its
// usage row — i.e. bill one tenant for another tenant's embeddings.
//
// An earlier round of the design asserted "the ingest worker already batches
// within a single project, so this constrains nothing today". That was false and
// was written without being checked, which is precisely why this is a test and
// not a sentence.

// recordingEmbedServer captures the texts of each inbound embed request so a
// test can assert how the batch was split across provider calls.
type recordingEmbedServer struct {
	mu    sync.Mutex
	calls [][]string
}

func (r *recordingEmbedServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		var parsed struct {
			Input []string `json:"input"`
		}
		_ = json.Unmarshal(body, &parsed)

		r.mu.Lock()
		r.calls = append(r.calls, parsed.Input)
		r.mu.Unlock()

		// One 1-dim vector per input, valued by position so the test can prove
		// vectors are scattered back to the right chunks.
		// index is REQUIRED: the embedder maps data entries back to inputs by
		// it, so omitting it collapses every vector onto input 0.
		resp := struct {
			Data []struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			} `json:"data"`
		}{}
		for i := range parsed.Input {
			resp.Data = append(resp.Data, struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			}{Index: i, Embedding: []float32{float32(i + 1)}})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

func (r *recordingEmbedServer) callTexts() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, len(r.calls))
	copy(out, r.calls)
	return out
}

func TestEmbedBatchByProject_SplitsMixedBatchPerProject(t *testing.T) {
	rec := &recordingEmbedServer{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	w := &Worker{
		embedder: NewEmbedder(Config{EmbeddingEndpoint: srv.URL, EmbeddingModel: "m"}),
	}

	// Interleaved on purpose: a naive implementation that splits on a project
	// CHANGE rather than grouping would make three calls, not two.
	chunks := []MemoryChunk{
		{ID: "c1", ProjectID: "alpha"},
		{ID: "c2", ProjectID: "beta"},
		{ID: "c3", ProjectID: "alpha"},
	}
	texts := []string{"alpha-one", "beta-one", "alpha-two"}

	vecs, err := w.embedBatchByProject(context.Background(), chunks, texts)
	if err != nil {
		t.Fatalf("embedBatchByProject: %v", err)
	}

	calls := rec.callTexts()
	if len(calls) != 2 {
		t.Fatalf("got %d provider calls, want 2 (one per project): %v", len(calls), calls)
	}
	// First-seen project order: alpha then beta, deterministic so this assertion
	// cannot flake on map iteration order.
	if len(calls[0]) != 2 || calls[0][0] != "alpha-one" || calls[0][1] != "alpha-two" {
		t.Errorf("first call should carry both alpha texts, got %v", calls[0])
	}
	if len(calls[1]) != 1 || calls[1][0] != "beta-one" {
		t.Errorf("second call should carry only beta's text, got %v", calls[1])
	}

	// Vectors must land against their own chunks. The stub values by position
	// within each call, so alpha-two (2nd in call 0) must come back as 2 at
	// index 2 — a scatter bug would put it at index 1.
	if len(vecs) != 3 {
		t.Fatalf("got %d vectors, want 3", len(vecs))
	}
	want := []float32{1, 1, 2}
	for i, wv := range want {
		if len(vecs[i]) != 1 || vecs[i][0] != wv {
			t.Errorf("vecs[%d] = %v, want [%v] — vectors were scattered to the wrong chunk", i, vecs[i], wv)
		}
	}
}

func TestEmbedBatchByProject_SingleProjectMakesOneCall(t *testing.T) {
	rec := &recordingEmbedServer{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	w := &Worker{
		embedder: NewEmbedder(Config{EmbeddingEndpoint: srv.URL, EmbeddingModel: "m"}),
	}
	chunks := []MemoryChunk{
		{ID: "c1", ProjectID: "alpha"},
		{ID: "c2", ProjectID: "alpha"},
	}
	if _, err := w.embedBatchByProject(context.Background(), chunks, []string{"one", "two"}); err != nil {
		t.Fatalf("embedBatchByProject: %v", err)
	}
	// The common case must not pay for the grouping: same call count as before
	// this change existed.
	if calls := rec.callTexts(); len(calls) != 1 {
		t.Errorf("single-project batch made %d calls, want 1: %v", len(calls), calls)
	}
}
