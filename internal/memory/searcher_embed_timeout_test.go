package memory

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestEmbedQueryWithTimeout covers the bounded query-embedding path
// added for the 2026-05-30 "recall hangs ~50s, returns ~nothing"
// incident: a slow/contended embedding backend must NOT block
// interactive recall for the embedder's full 60s HTTP timeout — it
// degrades to keyword-only (nil vector) within queryEmbedTimeout.
func TestEmbedQueryWithTimeout(t *testing.T) {
	t.Run("happy path returns the vector", func(t *testing.T) {
		want := []float32{0.1, 0.2, 0.3}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{{"index": 0, "embedding": want}},
			})
		}))
		defer srv.Close()

		cfg := Config{EmbeddingEndpoint: srv.URL, EmbeddingModel: "test"}
		s := NewSearcher(cfg, nil, NewEmbedder(cfg))
		got := s.embedQueryWithTimeout(context.Background(), "p1", "hello")
		if len(got) != 3 {
			t.Fatalf("happy path: got %v, want a 3-dim vector", got)
		}
	})

	t.Run("slow backend degrades to nil within the timeout", func(t *testing.T) {
		// Server blocks until the request context is cancelled (or 3s).
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(3 * time.Second):
			}
		}))
		defer srv.Close()

		cfg := Config{EmbeddingEndpoint: srv.URL, EmbeddingModel: "test"}
		s := NewSearcher(cfg, nil, NewEmbedder(cfg))
		s.queryEmbedTimeout = 150 * time.Millisecond

		start := time.Now()
		got := s.embedQueryWithTimeout(context.Background(), "p1", "hello")
		elapsed := time.Since(start)

		if got != nil {
			t.Errorf("slow backend must degrade to nil (keyword-only), got %v", got)
		}
		// Bound generously (timeout 150ms) to stay non-flaky while still
		// proving we don't wait the embedder's 60s HTTP timeout.
		if elapsed > 2*time.Second {
			t.Errorf("query embed not bounded by queryEmbedTimeout: took %s", elapsed)
		}
	})

	t.Run("no embedder configured returns nil", func(t *testing.T) {
		s := NewSearcher(Config{}, nil, nil)
		if got := s.embedQueryWithTimeout(context.Background(), "p1", "hello"); got != nil {
			t.Errorf("unconfigured embedder should return nil, got %v", got)
		}
	})
}
