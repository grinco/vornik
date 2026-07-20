package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/memory"
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

	ok := embeddingProberAdapter{}.ProbeEmbedding(context.Background(), memory.Config{EmbeddingEndpoint: srv.URL, EmbeddingModel: "bge-m3:latest"})
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

	if (embeddingProberAdapter{}).ProbeEmbedding(context.Background(), memory.Config{EmbeddingEndpoint: srv.URL, EmbeddingModel: "bge-m3:latest"}) {
		t.Fatal("a non-2xx embedding endpoint must probe unreachable")
	}
}

func TestEmbeddingProberAdapter_EmptyEndpointOrModel(t *testing.T) {
	if (embeddingProberAdapter{}).ProbeEmbedding(context.Background(), memory.Config{EmbeddingModel: "bge-m3:latest"}) {
		t.Fatal("empty endpoint must probe unreachable")
	}
	if (embeddingProberAdapter{}).ProbeEmbedding(context.Background(), memory.Config{EmbeddingEndpoint: "http://127.0.0.1:1"}) {
		t.Fatal("empty model must probe unreachable")
	}
}

type featureDoctorProbeProvider struct {
	models      []chat.ModelInfo
	listErr     error
	completeErr error
	model       string
	completed   bool
}

func (p *featureDoctorProbeProvider) Complete(context.Context, []chat.Message) (*chat.ChatResponse, error) {
	p.completed = true
	if p.completeErr != nil {
		return nil, p.completeErr
	}
	resp := &chat.ChatResponse{}
	resp.Choices = append(resp.Choices, struct {
		Index        int          `json:"index"`
		Message      chat.Message `json:"message"`
		FinishReason string       `json:"finish_reason"`
	}{Message: chat.Message{Role: "assistant", Content: "ok"}})
	return resp, nil
}

func (p *featureDoctorProbeProvider) CompleteWithTools(ctx context.Context, _ []chat.Message, _ []chat.Tool) (*chat.ChatResponse, error) {
	return p.Complete(ctx, nil)
}

func (p *featureDoctorProbeProvider) CompleteWithToolsStream(ctx context.Context, _ []chat.Message, _ []chat.Tool, _ chat.StreamCallback) (*chat.ChatResponse, error) {
	return p.Complete(ctx, nil)
}

func (p *featureDoctorProbeProvider) Model() string            { return p.model }
func (p *featureDoctorProbeProvider) SetMetrics(*chat.Metrics) {}

func (p *featureDoctorProbeProvider) WithModel(model string) chat.Provider {
	cp := *p
	cp.model = model
	return &cp
}

func (p *featureDoctorProbeProvider) ListModels(context.Context) ([]chat.ModelInfo, error) {
	return p.models, p.listErr
}

func TestModelPingerAdapter_CatalogHitSkipsCompletionProbe(t *testing.T) {
	provider := &featureDoctorProbeProvider{
		models: []chat.ModelInfo{{ID: "openai.gpt-oss-20b-1:0"}},
	}
	if !((modelPingerAdapter{provider: provider}).Reachable(context.Background(), "openai.gpt-oss-20b-1:0")) {
		t.Fatal("catalog-listed model should be reachable")
	}
	if provider.completed {
		t.Fatal("catalog hit should not spend a completion probe")
	}
}

func TestModelPingerAdapter_CompletionProbeWhenCatalogMisses(t *testing.T) {
	provider := &featureDoctorProbeProvider{}
	if !((modelPingerAdapter{provider: provider}).Reachable(context.Background(), "openai.gpt-oss-20b-1:0")) {
		t.Fatal("model with successful completion probe should be reachable even when catalog is empty")
	}
}

func TestModelPingerAdapter_CompletionProbeFailureIsUnreachable(t *testing.T) {
	provider := &featureDoctorProbeProvider{completeErr: errors.New("provider down")}
	if (modelPingerAdapter{provider: provider}).Reachable(context.Background(), "openai.gpt-oss-20b-1:0") {
		t.Fatal("model with failed catalog and failed completion probe should be unreachable")
	}
}
