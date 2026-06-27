package memory

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewEmbedder_ClientTimeoutWired(t *testing.T) {
	e := NewEmbedder(Config{})
	if e == nil || e.client == nil || e.client.Timeout == 0 {
		t.Fatalf("expected non-nil embedder with timeout, got %+v", e)
	}
}

func TestEmbed_EmptyEndpointOrTexts(t *testing.T) {
	e := NewEmbedder(Config{})
	got, err := e.Embed(context.Background(), []string{"x"})
	if got != nil || err != nil {
		t.Fatalf("empty endpoint: got %v err %v", got, err)
	}
	e2 := NewEmbedder(Config{EmbeddingEndpoint: "http://x"})
	got, err = e2.Embed(context.Background(), nil)
	if got != nil || err != nil {
		t.Fatalf("empty texts: got %v err %v", got, err)
	}
}

func TestEmbed_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer key123" {
			t.Errorf("auth header: %q", got)
		}
		b, _ := io.ReadAll(r.Body)
		var req embeddingRequest
		_ = json.Unmarshal(b, &req)
		// Return in reverse order to exercise the sort-by-index path.
		resp := embeddingResponse{}
		for i := len(req.Input) - 1; i >= 0; i-- {
			resp.Data = append(resp.Data, struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			}{Index: i, Embedding: []float32{float32(i), float32(i + 1)}})
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	e := NewEmbedder(Config{
		EmbeddingEndpoint: srv.URL + "/",
		EmbeddingModel:    "test-model",
		EmbeddingAPIKey:   "key123",
	})
	got, err := e.Embed(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 2 || got[0][0] != 0 || got[1][0] != 1 {
		t.Fatalf("ordering wrong: %v", got)
	}
}

func TestEmbed_BatchesOver512(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		b, _ := io.ReadAll(r.Body)
		var req embeddingRequest
		_ = json.Unmarshal(b, &req)
		resp := embeddingResponse{}
		for i := range req.Input {
			resp.Data = append(resp.Data, struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			}{Index: i, Embedding: []float32{1.0}})
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	e := NewEmbedder(Config{EmbeddingEndpoint: srv.URL, EmbeddingModel: "m"})
	texts := make([]string, 1025)
	for i := range texts {
		texts[i] = "t"
	}
	got, err := e.Embed(context.Background(), texts)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 1025 {
		t.Fatalf("len mismatch: %d", len(got))
	}
	if callCount != 3 {
		t.Fatalf("expected 3 batches, got %d", callCount)
	}
}

func TestEmbed_NetworkErrorDegrades(t *testing.T) {
	// Closed server → connection refused.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	e := NewEmbedder(Config{EmbeddingEndpoint: url, EmbeddingModel: "m"})
	got, err := e.Embed(context.Background(), []string{"x"})
	if got != nil || err != nil {
		t.Fatalf("expected nil,nil on network error; got %v %v", got, err)
	}
}

func TestEmbed_Non200Degrades(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	e := NewEmbedder(Config{EmbeddingEndpoint: srv.URL, EmbeddingModel: "m"})
	got, err := e.Embed(context.Background(), []string{"x"})
	if got != nil || err != nil {
		t.Fatalf("expected nil,nil on 500; got %v %v", got, err)
	}
}

func TestEmbed_BadJSONDegrades(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()
	e := NewEmbedder(Config{EmbeddingEndpoint: srv.URL, EmbeddingModel: "m"})
	got, _ := e.Embed(context.Background(), []string{"x"})
	if got != nil {
		t.Fatalf("expected nil on bad json, got %v", got)
	}
}

func TestEmbed_APIErrorFieldDegrades(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer srv.Close()
	e := NewEmbedder(Config{EmbeddingEndpoint: srv.URL, EmbeddingModel: "m"})
	got, _ := e.Embed(context.Background(), []string{"x"})
	if got != nil {
		t.Fatalf("expected nil on api error, got %v", got)
	}
}

func TestEmbed_OutOfRangeIndexDropped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := embeddingResponse{}
		// Mix valid and out-of-range/negative indexes.
		resp.Data = append(resp.Data,
			struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			}{Index: 0, Embedding: []float32{1}},
			struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			}{Index: -1, Embedding: []float32{9}},
			struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			}{Index: 99, Embedding: []float32{9}},
		)
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()
	e := NewEmbedder(Config{EmbeddingEndpoint: srv.URL, EmbeddingModel: "m"})
	got, err := e.Embed(context.Background(), []string{"t"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 1 || len(got[0]) != 1 || got[0][0] != 1 {
		t.Fatalf("unexpected: %v", got)
	}
}

func TestEmbed_TrailingSlashEndpoint(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		if !strings.HasSuffix(r.URL.Path, "/v1/embeddings") {
			t.Errorf("path: %s", r.URL.Path)
		}
		resp := embeddingResponse{Data: []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}{{Index: 0, Embedding: []float32{1}}}}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()
	// Many trailing slashes — must be normalised.
	e := NewEmbedder(Config{EmbeddingEndpoint: srv.URL + "///", EmbeddingModel: "m"})
	_, _ = e.Embed(context.Background(), []string{"t"})
	if !hit {
		t.Fatalf("server not hit")
	}
}
