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

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
)

// maxEmbeddingResponseBytes caps upstream embedding responses so a misbehaving
// endpoint cannot exhaust daemon memory.
const maxEmbeddingResponseBytes = 32 * 1024 * 1024

// Embedder produces vectors from either an OpenAI-compatible embeddings
// endpoint or AWS Bedrock's native InvokeModel API.
// When Cache is non-nil, identical (content, model) pairs short-
// circuit the upstream call and return the cached vector.
type Embedder struct {
	cfg    Config
	client *http.Client
	// Cache is the optional embedding cache (LLM caching design
	// Phase D). Production wires NewEmbeddingCache(db); tests
	// leave it nil to exercise the upstream path. Nil disables
	// caching — every call hits the upstream endpoint exactly
	// as in the slice-0 behaviour.
	Cache EmbedCache

	bedrockClient  bedrockRuntimeEmbedClient
	bedrockInitErr error
}

type bedrockRuntimeEmbedClient interface {
	InvokeModel(ctx context.Context, params *bedrockruntime.InvokeModelInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.InvokeModelOutput, error)
}

// NewEmbedder creates an Embedder from the given Config.
func NewEmbedder(cfg Config) *Embedder {
	e := &Embedder{
		cfg: cfg,
		client: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
	if e.usesBedrock() {
		if strings.TrimSpace(cfg.BedrockRegion) == "" {
			e.bedrockInitErr = fmt.Errorf("bedrock region is required")
			return e
		}
		awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion(cfg.BedrockRegion))
		if err != nil {
			e.bedrockInitErr = err
			return e
		}
		e.bedrockClient = bedrockruntime.NewFromConfig(awsCfg)
	}
	return e
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

type bedrockTitanEmbeddingRequest struct {
	InputText  string `json:"inputText"`
	Dimensions int    `json:"dimensions,omitempty"`
}

type bedrockTitanEmbeddingResponse struct {
	Embedding []float32 `json:"embedding"`
}

const maxEmbedBatch = 512

func (e *Embedder) usesBedrock() bool {
	return strings.EqualFold(strings.TrimSpace(e.cfg.EmbeddingProvider), "bedrock")
}

func (e *Embedder) configured() bool {
	if e.usesBedrock() {
		return strings.TrimSpace(e.cfg.EmbeddingModel) != "" && strings.TrimSpace(e.cfg.BedrockRegion) != ""
	}
	return strings.TrimSpace(e.cfg.EmbeddingEndpoint) != "" && strings.TrimSpace(e.cfg.EmbeddingModel) != ""
}

// Embed sends texts to the configured embedding backend in batches of up to
// 512 and returns one []float32 per input text preserving order.
// Returns nil, nil when the backend is not configured or any network/HTTP error
// occurs so callers can degrade gracefully.
func (e *Embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if !e.configured() || len(texts) == 0 {
		return nil, nil
	}

	result := make([][]float32, len(texts))

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
		if len(misses) == 0 {
			return result, nil
		}
	} else {
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
			return nil, nil
		}
		if vecs == nil {
			return nil, nil
		}
		for i, v := range vecs {
			outIdx := missIndices[start+i]
			result[outIdx] = v
			if e.Cache != nil && len(v) > 0 && e.cfg.EmbeddingModel != "" {
				_ = e.Cache.Put(ctx, ContentHash(batch[i]), e.cfg.EmbeddingModel, v)
			}
		}
	}

	return result, nil
}

// embedBatch calls the configured backend for a single batch and returns one
// vector per text.
func (e *Embedder) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if e.usesBedrock() {
		return e.embedBatchBedrock(ctx, texts)
	}
	return e.embedBatchOpenAICompat(ctx, texts)
}

func (e *Embedder) embedBatchOpenAICompat(ctx context.Context, texts []string) ([][]float32, error) {
	reqBody := embeddingRequest{Model: e.cfg.EmbeddingModel, Input: texts}
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
		return nil, nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, nil
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

	vecs := make([][]float32, len(texts))
	for _, d := range embResp.Data {
		if d.Index >= 0 && d.Index < len(vecs) {
			vecs[d.Index] = d.Embedding
		}
	}
	return vecs, nil
}

func (e *Embedder) embedBatchBedrock(ctx context.Context, texts []string) ([][]float32, error) {
	if e.bedrockInitErr != nil || e.bedrockClient == nil {
		return nil, nil
	}
	vecs := make([][]float32, len(texts))
	for i, text := range texts {
		vec, err := e.embedOneBedrock(ctx, text)
		if err != nil || len(vec) == 0 {
			return nil, nil
		}
		vecs[i] = vec
	}
	return vecs, nil
}

func (e *Embedder) embedOneBedrock(ctx context.Context, text string) ([]float32, error) {
	if !strings.HasPrefix(strings.TrimSpace(e.cfg.EmbeddingModel), "amazon.titan-embed-text") {
		return nil, fmt.Errorf("unsupported Bedrock embedding model %q", e.cfg.EmbeddingModel)
	}
	body, err := json.Marshal(bedrockTitanEmbeddingRequest{
		InputText:  text,
		Dimensions: e.cfg.EmbeddingDimension,
	})
	if err != nil {
		return nil, err
	}
	out, err := e.bedrockClient.InvokeModel(ctx, &bedrockruntime.InvokeModelInput{
		ModelId:     aws.String(e.cfg.EmbeddingModel),
		ContentType: aws.String("application/json"),
		Accept:      aws.String("application/json"),
		Body:        body,
	})
	if err != nil {
		return nil, err
	}
	var resp bedrockTitanEmbeddingResponse
	if err := json.Unmarshal(out.Body, &resp); err != nil {
		return nil, err
	}
	return resp.Embedding, nil
}
