package memory

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
)

func TestNewEmbedder_ClientTimeoutWired(t *testing.T) {
	e := NewEmbedder(Config{})
	if e == nil || e.client == nil || e.client.Timeout == 0 {
		t.Fatalf("expected non-nil embedder with timeout, got %+v", e)
	}
}

func TestEmbed_EmptyEndpointOrTexts(t *testing.T) {
	e := NewEmbedder(Config{})
	got, err := e.Embed(context.Background(), EmbedScope{ProjectID: "p1", CallSite: EmbedCallSiteIngest}, []string{"x"})
	if got != nil || err != nil {
		t.Fatalf("empty endpoint: got %v err %v", got, err)
	}
	e2 := NewEmbedder(Config{EmbeddingEndpoint: "http://x"})
	got, err = e2.Embed(context.Background(), EmbedScope{ProjectID: "p1", CallSite: EmbedCallSiteIngest}, nil)
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
	got, err := e.Embed(context.Background(), EmbedScope{ProjectID: "p1", CallSite: EmbedCallSiteIngest}, []string{"a", "b"})
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
	got, err := e.Embed(context.Background(), EmbedScope{ProjectID: "p1", CallSite: EmbedCallSiteIngest}, texts)
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
	got, err := e.Embed(context.Background(), EmbedScope{ProjectID: "p1", CallSite: EmbedCallSiteIngest}, []string{"x"})
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
	got, err := e.Embed(context.Background(), EmbedScope{ProjectID: "p1", CallSite: EmbedCallSiteIngest}, []string{"x"})
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
	got, _ := e.Embed(context.Background(), EmbedScope{ProjectID: "p1", CallSite: EmbedCallSiteIngest}, []string{"x"})
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
	got, _ := e.Embed(context.Background(), EmbedScope{ProjectID: "p1", CallSite: EmbedCallSiteIngest}, []string{"x"})
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
	got, err := e.Embed(context.Background(), EmbedScope{ProjectID: "p1", CallSite: EmbedCallSiteIngest}, []string{"t"})
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
	_, _ = e.Embed(context.Background(), EmbedScope{ProjectID: "p1", CallSite: EmbedCallSiteIngest}, []string{"t"})
	if !hit {
		t.Fatalf("server not hit")
	}
}

type fakeBedrockEmbedClient struct {
	body []byte
	err  error
}

func (f fakeBedrockEmbedClient) InvokeModel(context.Context, *bedrockruntime.InvokeModelInput, ...func(*bedrockruntime.Options)) (*bedrockruntime.InvokeModelOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &bedrockruntime.InvokeModelOutput{Body: f.body, ContentType: aws.String("application/json")}, nil
}

func TestEmbed_BedrockTitanHappyPath(t *testing.T) {
	e := NewEmbedder(Config{EmbeddingProvider: "bedrock", EmbeddingModel: "amazon.titan-embed-text-v2:0", BedrockRegion: "eu-central-1", EmbeddingDimension: 1024})
	e.bedrockClient = fakeBedrockEmbedClient{body: []byte(`{"embedding":[0.1,0.2,0.3]}`)}
	e.bedrockInitErr = nil
	got, err := e.Embed(context.Background(), EmbedScope{ProjectID: "p1", CallSite: EmbedCallSiteIngest}, []string{"a", "b"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 2 || len(got[0]) != 3 || got[1][2] != 0.3 {
		t.Fatalf("unexpected vectors: %v", got)
	}
}

// capturingBedrockClient records every request body so a test can assert the
// wire shape, not just the decoded result.
type capturingBedrockClient struct {
	bodies   [][]byte
	modelIDs []string
	reply    func(reqNo int) []byte
	err      error
}

func (c *capturingBedrockClient) InvokeModel(_ context.Context, in *bedrockruntime.InvokeModelInput, _ ...func(*bedrockruntime.Options)) (*bedrockruntime.InvokeModelOutput, error) {
	if c.err != nil {
		return nil, c.err
	}
	c.bodies = append(c.bodies, in.Body)
	c.modelIDs = append(c.modelIDs, aws.ToString(in.ModelId))
	return &bedrockruntime.InvokeModelOutput{
		Body:        c.reply(len(c.bodies) - 1),
		ContentType: aws.String("application/json"),
	}, nil
}

// cohereReply builds a Cohere v4 response: embeddings nested under an
// embedding_types key, unlike Titan's flat "embedding".
func cohereReply(vectors ...[]float32) []byte {
	type floats struct {
		Float [][]float32 `json:"float"`
	}
	b, _ := json.Marshal(struct {
		Embeddings floats `json:"embeddings"`
	}{Embeddings: floats{Float: vectors}})
	return b
}

// newCohereEmbedder builds a Cohere-on-Bedrock embedder at 1024 dimensions —
// deliberately the same width as bge-m3 and Titan v2, so an embedder A/B varies
// the model without also varying the vector width.
func newCohereEmbedder(t *testing.T, c bedrockRuntimeEmbedClient) *Embedder {
	t.Helper()
	e := NewEmbedder(Config{
		EmbeddingProvider:  "bedrock",
		EmbeddingModel:     "cohere.embed-v4:0",
		EmbeddingDimension: 1024,
		BedrockRegion:      "us-east-1",
	})
	e.bedrockClient = c
	e.bedrockInitErr = nil
	return e
}

// TestEmbed_CohereRequestShape — Cohere's Bedrock body differs from Titan's in
// every field: a `texts` ARRAY rather than a single `inputText`, plus an
// `input_type` and an `output_dimension`. Sending Titan's shape to Cohere is a
// 400, which is why the model prefix has to select the encoder.
func TestEmbed_CohereRequestShape(t *testing.T) {
	c := &capturingBedrockClient{reply: func(int) []byte {
		return cohereReply([]float32{0.1, 0.2}, []float32{0.3, 0.4})
	}}
	e := newCohereEmbedder(t, c)

	got, err := e.Embed(context.Background(), EmbedScope{ProjectID: "p1", CallSite: EmbedCallSiteIngest}, []string{"alpha", "beta"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(got) != 2 || len(got[0]) != 2 {
		t.Fatalf("got %d vectors, first len %d", len(got), len(got[0]))
	}

	// ONE call for TWO texts: Cohere batches, unlike Titan's one-per-call. That is
	// a throughput property worth pinning — Titan's serial loop is why the local
	// ingest took 37 minutes.
	if len(c.bodies) != 1 {
		t.Fatalf("made %d calls for 2 texts, want 1 batched call", len(c.bodies))
	}
	var req map[string]any
	if err := json.Unmarshal(c.bodies[0], &req); err != nil {
		t.Fatalf("request is not JSON: %v", err)
	}
	texts, ok := req["texts"].([]any)
	if !ok || len(texts) != 2 {
		t.Errorf("texts = %v, want a 2-element array", req["texts"])
	}
	if req["inputText"] != nil {
		t.Error("request carries Titan's inputText field; Cohere rejects it")
	}
	if req["output_dimension"] != float64(1024) {
		t.Errorf("output_dimension = %v, want 1024", req["output_dimension"])
	}
}

// TestEmbed_CohereUsesDocumentInputType — stored content must be embedded as a
// DOCUMENT. Cohere is trained asymmetrically: embedding a document with the query
// instruction measurably degrades retrieval, so this is a quality invariant, not a
// cosmetic field.
func TestEmbed_CohereUsesDocumentInputType(t *testing.T) {
	c := &capturingBedrockClient{reply: func(int) []byte { return cohereReply([]float32{1}) }}
	e := newCohereEmbedder(t, c)

	if _, err := e.Embed(context.Background(), EmbedScope{ProjectID: "p1", CallSite: EmbedCallSiteIngest}, []string{"stored chunk"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	var req map[string]any
	_ = json.Unmarshal(c.bodies[0], &req)
	if req["input_type"] != "search_document" {
		t.Errorf("input_type = %v, want search_document for stored content", req["input_type"])
	}
}

// TestEmbedQuery_CohereUsesQueryInputType — the other half of the asymmetry. A
// query embedded as a document lands in the wrong region of the space, which is
// exactly the failure that would make Cohere look worse than it is and corrupt an
// embedder A/B.
func TestEmbedQuery_CohereUsesQueryInputType(t *testing.T) {
	c := &capturingBedrockClient{reply: func(int) []byte { return cohereReply([]float32{1}) }}
	e := newCohereEmbedder(t, c)

	if _, err := e.EmbedQuery(context.Background(), EmbedScope{ProjectID: "p1", CallSite: EmbedCallSiteIngest}, "where does Alice work"); err != nil {
		t.Fatalf("EmbedQuery: %v", err)
	}
	var req map[string]any
	_ = json.Unmarshal(c.bodies[0], &req)
	if req["input_type"] != "search_query" {
		t.Errorf("input_type = %v, want search_query for a query", req["input_type"])
	}
}

// TestEmbedQuery_TitanIgnoresInputType — Titan has no input_type concept, so
// EmbedQuery must behave exactly like Embed there rather than sending a field
// Titan would reject.
func TestEmbedQuery_TitanIgnoresInputType(t *testing.T) {
	c := &capturingBedrockClient{reply: func(int) []byte {
		b, _ := json.Marshal(bedrockTitanEmbeddingResponse{Embedding: []float32{0.5}})
		return b
	}}
	e := NewEmbedder(Config{
		EmbeddingProvider: "bedrock", EmbeddingModel: "amazon.titan-embed-text-v2:0",
		EmbeddingDimension: 1024, BedrockRegion: "us-east-1",
	})
	e.bedrockClient = c

	got, err := e.EmbedQuery(context.Background(), EmbedScope{ProjectID: "p1", CallSite: EmbedCallSiteIngest}, "a query")
	if err != nil {
		t.Fatalf("EmbedQuery: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d values", len(got))
	}
	var req map[string]any
	_ = json.Unmarshal(c.bodies[0], &req)
	if _, present := req["input_type"]; present {
		t.Error("Titan request carries input_type, which it does not accept")
	}
	if req["inputText"] != "a query" {
		t.Errorf("inputText = %v", req["inputText"])
	}
}

// TestEmbed_CohereBatchesAtProviderLimit — Cohere caps a request at 96 texts.
// Exceeding it is a hard 400, so the encoder must split rather than trusting the
// caller's batch size (maxEmbedBatch is 512).
func TestEmbed_CohereBatchesAtProviderLimit(t *testing.T) {
	texts := make([]string, 200)
	for i := range texts {
		texts[i] = "chunk"
	}
	c := &capturingBedrockClient{}
	// The reply mirrors the request's text count, so it is installed after the
	// client exists and can read back its own captured bodies.
	c.reply = func(n int) []byte {
		var req struct {
			Texts []string `json:"texts"`
		}
		_ = json.Unmarshal(c.bodies[n], &req)
		vecs := make([][]float32, len(req.Texts))
		for i := range vecs {
			vecs[i] = []float32{float32(i)}
		}
		return cohereReply(vecs...)
	}
	e := newCohereEmbedder(t, c)

	got, err := e.Embed(context.Background(), EmbedScope{ProjectID: "p1", CallSite: EmbedCallSiteIngest}, texts)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(got) != 200 {
		t.Fatalf("got %d vectors for 200 texts", len(got))
	}
	for i, b := range c.bodies {
		var req struct {
			Texts []string `json:"texts"`
		}
		_ = json.Unmarshal(b, &req)
		if len(req.Texts) > cohereMaxBatch {
			t.Errorf("call %d sent %d texts, over Cohere's %d limit",
				i, len(req.Texts), cohereMaxBatch)
		}
	}
}

// TestEmbed_UnsupportedBedrockModelStillRejected — the guard that catches a
// mis-set model id must survive the addition of a second encoder, or a typo
// silently produces garbage vectors.
func TestEmbed_UnsupportedBedrockModelStillRejected(t *testing.T) {
	c := &capturingBedrockClient{reply: func(int) []byte { return nil }}
	e := NewEmbedder(Config{
		EmbeddingProvider: "bedrock", EmbeddingModel: "meta.llama-not-an-embedder",
		EmbeddingDimension: 1024, BedrockRegion: "us-east-1",
	})
	e.bedrockClient = c

	if _, err := e.embedOneBedrock(context.Background(), ingestScope(), "x"); err == nil {
		t.Error("an unsupported Bedrock embedding model was accepted")
	}
}
