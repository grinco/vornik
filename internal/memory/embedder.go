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

	"vornik.io/vornik/internal/llmspend"
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

	// spend records one task_llm_usage row per BILLED provider call. Exported
	// setter only (SetSpend) so it cannot be left silently unset by a struct
	// literal; a zero value reports itself rather than billing nothing quietly.
	spend llmspend.Recorder

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
	// Usage is present on OpenAI-compatible endpoints. Absent on some
	// (including several local servers), which is what TokensEstimated is for.
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

// embedUsageProbe is the usage block alone. It is unmarshalled SEPARATELY from
// the vectors so a body whose `data` is unusable still yields its token count:
// the provider charged either way, and a parse failure must not launder the
// spend (see recordEmbedUsage).
type embedUsageProbe struct {
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

type bedrockTitanEmbeddingRequest struct {
	InputText  string `json:"inputText"`
	Dimensions int    `json:"dimensions,omitempty"`
}

type bedrockTitanEmbeddingResponse struct {
	Embedding []float32 `json:"embedding"`
	// InputTextTokenCount is Titan's own count — a MEASUREMENT, so rows built
	// from it are not flagged estimated. Cohere's embed response carries no
	// equivalent, which is the asymmetry TokensEstimated exists to record.
	InputTextTokenCount int `json:"inputTextTokenCount"`
}

// cohereMaxBatch is Cohere's hard per-request text limit on Bedrock. Exceeding it
// is a 400, not a truncation, so the encoder splits regardless of maxEmbedBatch.
const cohereMaxBatch = 96

const maxEmbedBatch = 512

// embedInputType distinguishes stored content from a search query.
//
// Cohere's models are trained ASYMMETRICALLY: a document and a query about that
// document get different instructions, and embedding a query with the document
// instruction lands it in the wrong region of the space. Titan has no such notion
// and must not receive the field at all.
type embedInputType string

const (
	embedDocument    embedInputType = "search_document"
	embedSearchQuery embedInputType = "search_query"
)

// bedrockCohereEmbeddingRequest is Cohere's Bedrock body — a texts ARRAY, unlike
// Titan's single inputText.
type bedrockCohereEmbeddingRequest struct {
	Texts []string `json:"texts"`
	// InputType is required by Cohere v3+ and is the asymmetry described above.
	InputType string `json:"input_type"`
	// EmbeddingTypes selects the return representation; "float" gives the dense
	// vectors pgvector stores.
	EmbeddingTypes  []string `json:"embedding_types,omitempty"`
	OutputDimension int      `json:"output_dimension,omitempty"`
}

// bedrockCohereEmbeddingResponse nests vectors under the requested embedding
// type, unlike Titan's flat "embedding" field.
type bedrockCohereEmbeddingResponse struct {
	Embeddings struct {
		Float [][]float32 `json:"float"`
	} `json:"embeddings"`
}

// usesCohere reports whether the configured Bedrock model is a Cohere embedder.
func (e *Embedder) usesCohere() bool {
	return strings.HasPrefix(strings.TrimSpace(e.cfg.EmbeddingModel), "cohere.embed")
}

func (e *Embedder) usesBedrock() bool {
	return strings.EqualFold(strings.TrimSpace(e.cfg.EmbeddingProvider), "bedrock")
}

func (e *Embedder) configured() bool {
	if e.usesBedrock() {
		return strings.TrimSpace(e.cfg.EmbeddingModel) != "" && strings.TrimSpace(e.cfg.BedrockRegion) != ""
	}
	return strings.TrimSpace(e.cfg.EmbeddingEndpoint) != "" && strings.TrimSpace(e.cfg.EmbeddingModel) != ""
}

// Model returns the configured embedding model id, or "" when the embedder is
// not configured.
//
// Callers that PERSIST a vector must store this alongside it: vectors from
// different models occupy different spaces, so a stored vector is only
// comparable to another produced by the same model. The knowledge-skill dedup
// preflight (LLD 2026-07-07-knowledge-skill-store-design §12.2) relies on this
// to decide whether a stored embedding can be reused or must be recomputed.
func (e *Embedder) Model() string {
	if !e.configured() {
		return ""
	}
	return strings.TrimSpace(e.cfg.EmbeddingModel)
}

// Provider reports the embedding transport actually in use, so the deployment's
// resolved embedder is observable rather than merely configured.
//
// It exists because a config value is a statement of intent, not evidence: a
// benchmark had to label its runs with the model an operator typed at a command
// line, since nothing in the product reported the one in force. Two providers then
// produced byte-identical retrieval metrics — impossible for two different
// embedders — and nothing could say which vectors had been queried.
//
// An empty EmbeddingProvider is the OpenAI-compatible default, and it is named as
// such rather than returned empty: "openai" and "" mean different things to a
// caller, the second being "no embedder at all".
func (e *Embedder) Provider() string {
	if !e.configured() {
		return ""
	}
	if p := strings.TrimSpace(e.cfg.EmbeddingProvider); p != "" {
		return strings.ToLower(p)
	}
	return "openai"
}

// Dimension reports the configured vector width, 0 when unconfigured.
//
// Worth reporting alongside the model because a dimension that disagrees with the
// model fails EVERY insert, so it presents as a total memory outage rather than as
// degraded quality — and two different models sharing one width (both 1024, say)
// cannot be told apart by inspecting the stored vectors.
func (e *Embedder) Dimension() int {
	if !e.configured() {
		return 0
	}
	return e.cfg.EmbeddingDimension
}

// Embed sends texts to the configured embedding backend in batches of up to
// 512 and returns one []float32 per input text preserving order.
// Returns nil, nil when the backend is not configured or any network/HTTP error
// occurs so callers can degrade gracefully.
//
// scope says who the call is billed to and is required — see EmbedScope. An
// invalid scope returns an ERROR, deliberately not the (nil, nil) degrade: a
// caller that forgot to state its attribution is a programming error, and
// hiding it in the degrade path would make it indistinguishable from a flaky
// endpoint. Validation runs before any provider call so a rejected scope never
// spends money the ledger cannot then attribute.
//
// All texts in one call share one scope, which is what keeps a usage row's
// project_id true: a caller must not mix projects into a single batch.
func (e *Embedder) Embed(ctx context.Context, scope EmbedScope, texts []string) ([][]float32, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
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

		vecs, err := e.embedBatch(ctx, scope, batch)
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

// estimateEmbedTokens derives a token count from text length when the provider
// reports none (Bedrock Cohere reports none at all).
//
// bytes/4 is the widely used rough ratio for English prose. It is deliberately
// crude: the point is not precision, it is that the spend APPEARS. Every row it
// produces is flagged TokensEstimated so nothing downstream can mistake it for
// a provider measurement.
func estimateEmbedTokens(texts []string) int {
	total := 0
	for _, t := range texts {
		total += len(t)
	}
	if total == 0 {
		return 0
	}
	if est := total / 4; est > 0 {
		return est
	}
	// A very short text still cost at least one token.
	return 1
}

// embedTokensFromBody extracts a provider-reported prompt-token count.
//
// Unmarshalled independently of the vectors so an unusable `data` payload still
// yields the count: the charge is real whether or not we can use the response.
// Returns measured=false when the provider reported nothing, which is the
// caller's signal to estimate and flag.
func embedTokensFromBody(body []byte) (tokens int, measured bool) {
	var probe embedUsageProbe
	if err := json.Unmarshal(body, &probe); err != nil {
		return 0, false
	}
	if probe.Usage.PromptTokens > 0 {
		return probe.Usage.PromptTokens, true
	}
	// Some endpoints report only a total; for an embedding call that total IS
	// the prompt, since there are no completion tokens.
	if probe.Usage.TotalTokens > 0 {
		return probe.Usage.TotalTokens, true
	}
	return 0, false
}

// recordEmbedUsage writes one task_llm_usage row for one BILLED provider call.
//
// Called immediately after the provider returns and BEFORE the response is
// parsed or judged — the RECORD BEFORE CLASSIFYING invariant. The provider
// charged the moment it returned, so a row written only on the success path
// makes every degrade a silent spender; that is how the KG extractor laundered
// ~83% of its spend and how the reranker's spend stayed invisible.
//
// Never called for a cache hit: the caller short-circuits before the provider,
// so there is no charge to record and inventing a row would fabricate spend.
//
// Errors are swallowed. Failing to bill is a fidelity loss; failing to embed
// would be an outage, and ingestion must not stop because the ledger is down.
func (e *Embedder) recordEmbedUsage(ctx context.Context, scope EmbedScope, promptTokens int, estimated bool) {
	if e == nil || promptTokens <= 0 {
		return
	}
	// TaskID nil and StepID empty: embedding is not task-scoped, and a batch
	// spans many chunks so no single chunk owns the call. CompletionTokens is
	// omitted because an embedding call generates no output.
	e.spend.Record(ctx, llmspend.Input{
		ProjectID:       scope.ProjectID,
		Model:           strings.TrimSpace(e.cfg.EmbeddingModel),
		PromptTokens:    promptTokens,
		TokensEstimated: estimated,
	})
}

// SetSpend wires the billing recorder. Embedder is built by memory.NewManager
// from config alone, which has no usage repo, so the recorder arrives after
// construction — the one component where that is unavoidable. The field is
// unexported so the assignment is a method call the wiring law can find.
func (e *Embedder) SetSpend(r llmspend.Recorder) { e.spend = r }

// embedBatch calls the configured backend for a single batch and returns one
// vector per text.
func (e *Embedder) embedBatch(ctx context.Context, scope EmbedScope, texts []string) ([][]float32, error) {
	if e.usesBedrock() {
		return e.embedBatchBedrock(ctx, scope, texts)
	}
	return e.embedBatchOpenAICompat(ctx, scope, texts)
}

func (e *Embedder) embedBatchOpenAICompat(ctx context.Context, scope EmbedScope, texts []string) ([][]float32, error) {
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
		// A 200 whose body we could not even read was still charged, but we have
		// no token count and no text-independent way to know what was billed —
		// so estimate from the request we sent rather than record nothing.
		e.recordEmbedUsage(ctx, scope, estimateEmbedTokens(texts), true)
		return nil, nil
	}

	// BILL FIRST. The status was 200, so the provider charged; everything below
	// is us deciding whether we can USE what we paid for, and that decision must
	// not be able to erase the charge.
	//
	// WHERE THE BILLING BOUNDARY SITS, and why it is here rather than earlier.
	// The invariant is "a CHARGE produces a row", not "every provider contact
	// produces a row". A non-200 — 429, 500, 502, 503 — means no inference was
	// delivered and nothing was charged, so recording an estimate for it would
	// FABRICATE spend. That is the same class of error as billing a cache hit,
	// just in the other direction, and a ledger that invents charges is no more
	// evidence than one that loses them. All three transports draw the line in
	// the same place: transport failure records nothing, a delivered response we
	// cannot parse records an estimate.
	//
	// Pinned by TestEmbedUsage_TransportFailureRecordsNothing, so a future reader
	// resolving this ambiguity by guessing has to argue with a test.
	if tokens, measured := embedTokensFromBody(body); measured {
		e.recordEmbedUsage(ctx, scope, tokens, false)
	} else {
		e.recordEmbedUsage(ctx, scope, estimateEmbedTokens(texts), true)
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

func (e *Embedder) embedBatchBedrock(ctx context.Context, scope EmbedScope, texts []string) ([][]float32, error) {
	if e.bedrockInitErr != nil || e.bedrockClient == nil {
		return nil, nil
	}
	// Cohere accepts a batch per call; Titan is one text per call. Batching is not
	// just tidier — the serial Titan loop is why a 3,187-chunk ingest is slow.
	if e.usesCohere() {
		return e.embedBatchCohere(ctx, scope, texts, embedDocument)
	}
	vecs := make([][]float32, len(texts))
	for i, text := range texts {
		// One row per Titan call, because Titan bills per call — the loop is
		// N charges, not one.
		vec, err := e.embedOneBedrock(ctx, scope, text)
		if err != nil || len(vec) == 0 {
			return nil, nil
		}
		vecs[i] = vec
	}
	return vecs, nil
}

// embedBatchCohere sends texts to Cohere on Bedrock, splitting at the provider's
// per-request limit.
func (e *Embedder) embedBatchCohere(ctx context.Context, scope EmbedScope, texts []string, kind embedInputType) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += cohereMaxBatch {
		end := start + cohereMaxBatch
		if end > len(texts) {
			end = len(texts)
		}
		body, err := json.Marshal(bedrockCohereEmbeddingRequest{
			Texts:           texts[start:end],
			InputType:       string(kind),
			EmbeddingTypes:  []string{"float"},
			OutputDimension: e.cfg.EmbeddingDimension,
		})
		if err != nil {
			return nil, err
		}
		res, err := e.bedrockClient.InvokeModel(ctx, &bedrockruntime.InvokeModelInput{
			ModelId:     aws.String(e.cfg.EmbeddingModel),
			ContentType: aws.String("application/json"),
			Accept:      aws.String("application/json"),
			Body:        body,
		})
		if err != nil {
			return nil, err
		}
		// Cohere's embed response carries NO token count, so this is the
		// estimate-and-flag path. Recorded before the body is parsed: Bedrock
		// charged for the invocation regardless of what came back.
		e.recordEmbedUsage(ctx, scope, estimateEmbedTokens(texts[start:end]), true)
		var resp bedrockCohereEmbeddingResponse
		if err := json.Unmarshal(res.Body, &resp); err != nil {
			return nil, err
		}
		if len(resp.Embeddings.Float) != end-start {
			return nil, fmt.Errorf("cohere returned %d vectors for %d texts",
				len(resp.Embeddings.Float), end-start)
		}
		out = append(out, resp.Embeddings.Float...)
	}
	return out, nil
}

// EmbedQuery embeds a SEARCH QUERY rather than stored content.
//
// Separate from Embed because Cohere needs the query instruction to place a query
// in the same region as the documents that answer it; using the document
// instruction for queries measurably degrades retrieval. For providers with no
// such asymmetry (Titan, OpenAI-compatible endpoints) this is exactly Embed, so
// callers can use it unconditionally.
func (e *Embedder) EmbedQuery(ctx context.Context, scope EmbedScope, query string) ([]float32, error) {
	// Validated here as well as in Embed: the Cohere branch below reaches the
	// provider WITHOUT going through Embed, so relying on Embed's check would
	// leave query-time Cohere spend unattributed — the one path that would have
	// kept the original defect alive.
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if !e.configured() || strings.TrimSpace(query) == "" {
		return nil, nil
	}
	if e.usesBedrock() && e.usesCohere() {
		vecs, err := e.embedBatchCohere(ctx, scope, []string{query}, embedSearchQuery)
		if err != nil {
			return nil, err
		}
		if len(vecs) == 0 {
			return nil, nil
		}
		return vecs[0], nil
	}
	vecs, err := e.Embed(ctx, scope, []string{query})
	if err != nil || len(vecs) == 0 {
		return nil, err
	}
	return vecs[0], nil
}

func (e *Embedder) embedOneBedrock(ctx context.Context, scope EmbedScope, text string) ([]float32, error) {
	if !strings.HasPrefix(strings.TrimSpace(e.cfg.EmbeddingModel), "amazon.titan-embed-text") {
		// Cohere goes through embedBatchCohere; anything else is a mis-set model id
		// and must fail loudly rather than produce garbage vectors.
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
		// Charged, unusable. Estimate rather than lose the spend.
		e.recordEmbedUsage(ctx, scope, estimateEmbedTokens([]string{text}), true)
		return nil, err
	}
	if resp.InputTextTokenCount > 0 {
		e.recordEmbedUsage(ctx, scope, resp.InputTextTokenCount, false)
	} else {
		e.recordEmbedUsage(ctx, scope, estimateEmbedTokens([]string{text}), true)
	}
	return resp.Embedding, nil
}
