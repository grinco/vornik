package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// maxEmbeddingResponseBytes caps upstream embedding responses so a misbehaving
// endpoint cannot exhaust daemon memory.
const maxEmbeddingResponseBytes = 32 * 1024 * 1024

// Embedder calls an OpenAI-compatible embeddings endpoint to produce vectors.
// When Cache is non-nil, identical (content, model) pairs short-
// circuit the upstream HTTP call and return the cached vector.
type Embedder struct {
	cfg    Config
	client *http.Client
	// Cache is the optional embedding cache (LLM caching design
	// Phase D). Production wires NewEmbeddingCache(db); tests
	// leave it nil to exercise the upstream path. Nil disables
	// caching — every call hits the upstream endpoint exactly
	// as in the slice-0 behaviour.
	Cache EmbedCache
}

// NewEmbedder creates an Embedder from the given Config.
func NewEmbedder(cfg Config) *Embedder {
	return &Embedder{
		cfg: cfg,
		client: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

// embeddingRequest is the JSON body for the embeddings API.
type embeddingRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

// embeddingResponse is the JSON body returned by the embeddings API.
type embeddingResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

const maxEmbedBatch = 512

// Embed sends texts to the embedding endpoint in batches of up to 512 and
// returns one []float32 per input text preserving order.
// Returns nil, nil when the endpoint is empty or any network/HTTP error occurs
// so callers can degrade gracefully.
//
// Cache short-circuit (Phase D): when e.Cache is non-nil, each
// text is hashed and looked up against (hash, model). Hits are
// served from cache; misses are batched into the upstream
// embedBatch call. The result array preserves input order; cache
// populates run after the upstream returns. Cache errors are
// best-effort — a broken cache must never block the upstream call.
func (e *Embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if e.cfg.EmbeddingEndpoint == "" || len(texts) == 0 {
		return nil, nil
	}

	result := make([][]float32, len(texts))

	// Phase-D cache short-circuit. Compute hashes once, look up
	// each text; uncached indices land in a contiguous batch sent
	// to embedBatch. Fresh slices are allocated for misses /
	// missIndices to avoid aliasing into the caller's texts
	// argument (append on a re-sliced texts[:0] would mutate the
	// caller's backing array).
	var misses []string
	var missIndices []int
	if e.Cache != nil && e.cfg.EmbeddingModel != "" {
		misses = make([]string, 0, len(texts))
		missIndices = make([]int, 0, len(texts))
		for i, t := range texts {
			hash := ContentHash(t)
			if vec, ok, err := e.Cache.Get(ctx, hash, e.cfg.EmbeddingModel); err == nil && ok {
				result[i] = vec
				continue
			}
			misses = append(misses, t)
			missIndices = append(missIndices, i)
		}
		// Every text served from cache → skip the upstream call.
		if len(misses) == 0 {
			return result, nil
		}
	} else {
		// No cache → every text is a "miss" (i.e. needs upstream).
		// Reuse texts directly via index aliasing — the upstream
		// loop only reads `misses[start:end]`, never writes, so
		// no caller-visible mutation.
		misses = texts
		missIndices = make([]int, len(texts))
		for i := range texts {
			missIndices[i] = i
		}
	}

	for start := 0; start < len(misses); start += maxEmbedBatch {
		end := start + maxEmbedBatch
		if end > len(misses) {
			end = len(misses)
		}
		batch := misses[start:end]

		vecs, err := e.embedBatch(ctx, batch)
		if err != nil {
			// Degrade gracefully on any error — embedding failures must not
			// block task completion.
			return nil, nil
		}
		if vecs == nil {
			return nil, nil
		}
		for i, v := range vecs {
			outIdx := missIndices[start+i]
			result[outIdx] = v
			// Cache populate on success. Best-effort — errors
			// don't propagate.
			if e.Cache != nil && len(v) > 0 && e.cfg.EmbeddingModel != "" {
				_ = e.Cache.Put(ctx, ContentHash(batch[i]), e.cfg.EmbeddingModel, v)
			}
		}
	}

	return result, nil
}

// embedBatch calls the API for a single batch and returns one vector per text.
func (e *Embedder) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	reqBody := embeddingRequest{
		Model: e.cfg.EmbeddingModel,
		Input: texts,
	}
	b, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal embedding request: %w", err)
	}

	url := strings.TrimRight(e.cfg.EmbeddingEndpoint, "/") + "/v1/embeddings"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("create embedding request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if e.cfg.EmbeddingAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.cfg.EmbeddingAPIKey)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, nil // network error → degrade
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, nil // non-200 → degrade
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxEmbeddingResponseBytes))
	if err != nil {
		return nil, nil
	}

	var embResp embeddingResponse
	if err := json.Unmarshal(body, &embResp); err != nil {
		return nil, nil
	}
	if embResp.Error != nil {
		return nil, nil
	}

	// The API returns items potentially out of order; sort by index.
	vecs := make([][]float32, len(texts))
	for _, d := range embResp.Data {
		if d.Index >= 0 && d.Index < len(vecs) {
			vecs[d.Index] = d.Embedding
		}
	}
	return vecs, nil
}
