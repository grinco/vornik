package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestEmbeddingProberAdapter_ReachableWhenEndpointServesModel is the
// API-layer half of the 2026-06-12 incident guard: the embedding-model
// reachability probe must hit the dedicated embedding endpoint
// (<endpoint>/v1/embeddings) and report reachable when a non-empty vector
// comes back — independent of the chat-provider catalog.
func TestEmbeddingProberAdapter_ReachableWhenEndpointServesModel(t *testing.T) {
	var gotPath, gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		gotModel = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.1,0.2,0.3]}]}`))
	}))
	defer srv.Close()

	ok := embeddingProberAdapter{}.ProbeEmbedding(context.Background(), srv.URL, "", "bge-m3:latest")
	if !ok {
		t.Fatal("a model served at the embedding endpoint must probe reachable")
	}
	if gotPath != "/v1/embeddings" {
		t.Fatalf("probe must hit /v1/embeddings, got %q", gotPath)
	}
	if !strings.Contains(gotModel, "bge-m3:latest") {
		t.Fatalf("probe must request the configured model, body was %q", gotModel)
	}
}

func TestEmbeddingProberAdapter_UnreachableOnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model not found", http.StatusNotFound)
	}))
	defer srv.Close()

	if (embeddingProberAdapter{}).ProbeEmbedding(context.Background(), srv.URL, "", "bge-m3:latest") {
		t.Fatal("a non-2xx embedding endpoint must probe unreachable")
	}
}

func TestEmbeddingProberAdapter_EmptyEndpointOrModel(t *testing.T) {
	if (embeddingProberAdapter{}).ProbeEmbedding(context.Background(), "", "", "bge-m3:latest") {
		t.Fatal("empty endpoint must probe unreachable")
	}
	if (embeddingProberAdapter{}).ProbeEmbedding(context.Background(), "http://127.0.0.1:1", "", "") {
		t.Fatal("empty model must probe unreachable")
	}
}
