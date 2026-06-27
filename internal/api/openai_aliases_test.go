// Coverage for the /v1/... OpenAI-compatibility URL aliases.
// The canonical paths (/api/v1/chat/completions, /api/v1/models)
// have their own dedicated tests in chat_proxy_test.go; these
// tests verify the aliases reach the same handlers through the
// router.

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/config"
)

// openaiStub is a chat.Provider with no behaviour beyond
// satisfying the interface — the alias tests only care that the
// route reaches the handler, not what the handler then does.
type openaiStub struct{ model string }

func (o openaiStub) Complete(_ context.Context, _ []chat.Message) (*chat.ChatResponse, error) {
	return buildOKResponse(o.model), nil
}
func (o openaiStub) CompleteWithTools(_ context.Context, _ []chat.Message, _ []chat.Tool) (*chat.ChatResponse, error) {
	return buildOKResponse(o.model), nil
}
func (o openaiStub) CompleteWithToolsStream(_ context.Context, _ []chat.Message, _ []chat.Tool, _ chat.StreamCallback) (*chat.ChatResponse, error) {
	return buildOKResponse(o.model), nil
}
func (o openaiStub) Model() string              { return o.model }
func (o openaiStub) SetMetrics(_ *chat.Metrics) {}

// buildOKResponse fabricates a minimal ChatResponse with the
// anonymous-struct Choices shape the real chat package uses.
// Avoids needing to drop the named-Choice convenience that
// chat doesn't actually expose.
func buildOKResponse(model string) *chat.ChatResponse {
	resp := &chat.ChatResponse{Model: model}
	resp.Choices = append(resp.Choices, struct {
		Index        int          `json:"index"`
		Message      chat.Message `json:"message"`
		FinishReason string       `json:"finish_reason"`
	}{
		Message: chat.Message{Role: "assistant", Content: "ok"},
	})
	return resp
}

// TestOpenAIAlias_ChatCompletions verifies that
// POST /v1/chat/completions reaches the same handler as the
// canonical /api/v1/chat/completions. Third-party OpenAI SDKs
// default to base_url ending in /v1, so this alias is what
// unblocks them.
func TestOpenAIAlias_ChatCompletions(t *testing.T) {
	s := NewServer(WithChatProvider(openaiStub{model: "alias-model"}))
	cfg := &config.Config{}
	cfg.API.AuthEnabled = false
	handler := SetupRoutes(s, cfg)

	body := bytes.NewBufferString(`{"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — /v1/chat/completions alias should route to ChatCompletions; body=%s", rr.Code, rr.Body.String())
	}
	if !bytes.Contains(rr.Body.Bytes(), []byte("alias-model")) {
		t.Errorf("response body missing provider model: %s", rr.Body.String())
	}
}

// TestOpenAIAlias_Models verifies that GET /v1/models reaches
// the same handler as the canonical /api/v1/models. The stub
// provider doesn't implement ModelLister so the handler returns
// 501 with a typed envelope — proving the route reached the
// handler. A real provider in production returns 200 + the
// catalog.
func TestOpenAIAlias_Models(t *testing.T) {
	s := NewServer(WithChatProvider(openaiStub{model: "alias-model"}))
	cfg := &config.Config{}
	cfg.API.AuthEnabled = false
	handler := SetupRoutes(s, cfg)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	// Accept 200 (provider implements ModelLister with content) or
	// 501 (provider doesn't, but the alias reached the handler).
	// 404 would mean the alias isn't wired — what we're guarding
	// against.
	if rr.Code != http.StatusOK && rr.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 200 or 501 — /v1/models alias should route to ListModels; body=%s", rr.Code, rr.Body.String())
	}
}

// TestOpenAIAlias_NotWiredWithoutChatProvider — the aliases are
// only registered when a chat provider is configured. Without
// one, the alias must 404 (not 503) so a misconfigured client
// gets the same "where's the endpoint" signal as the canonical
// path would.
func TestOpenAIAlias_NotWiredWithoutChatProvider(t *testing.T) {
	s := NewServer() // no provider
	cfg := &config.Config{}
	cfg.API.AuthEnabled = false
	handler := SetupRoutes(s, cfg)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{}`))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 — alias should be unregistered when chat provider is off", rr.Code)
	}
}

// TestOpenAIAlias_CanonicalStillWorks defends the canonical
// /api/v1/... paths against accidental removal during the alias
// addition. A regression here would break agent containers that
// hardcode the canonical surface.
func TestOpenAIAlias_CanonicalStillWorks(t *testing.T) {
	s := NewServer(WithChatProvider(openaiStub{model: "canonical-model"}))
	cfg := &config.Config{}
	cfg.API.AuthEnabled = false
	handler := SetupRoutes(s, cfg)

	body := bytes.NewBufferString(`{"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", body)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 — canonical /api/v1/chat/completions must still route", rr.Code)
	}
}

// TestChatCompletions_StreamTrueRejected pins the typed error
// for stream:true. openai-python sends this by default for many
// integrations; returning a buffered JSON body silently
// surfaced as a 500 on the client side (operator-observed
// 2026-05-16). The handler now returns a clear 400 +
// STREAMING_NOT_SUPPORTED envelope so the SDK shows the right
// remediation.
func TestChatCompletions_StreamTrueRejected(t *testing.T) {
	s := NewServer(WithChatProvider(openaiStub{model: "stub-model"}))
	body := bytes.NewBufferString(`{"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	rr := httptest.NewRecorder()
	s.ChatCompletions(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("stream:true: status = %d, want 400", rr.Code)
	}
	if !bytes.Contains(rr.Body.Bytes(), []byte("STREAMING_NOT_SUPPORTED")) {
		t.Errorf("body should mention STREAMING_NOT_SUPPORTED, got %q", rr.Body.String())
	}
}

// TestChatCompletions_FillsIDObjectCreated pins the
// OpenAI-canonical envelope fields. openai-python's response
// validator requires non-empty id, object="chat.completion", and
// non-zero created — providers can leave these blank, so the
// proxy backfills.
func TestChatCompletions_FillsIDObjectCreated(t *testing.T) {
	stub := openaiStub{model: "stub-model"}
	s := NewServer(WithChatProvider(stub))
	body := bytes.NewBufferString(`{"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	rr := httptest.NewRecorder()
	s.ChatCompletions(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID == "" {
		t.Error("id is empty — OpenAI SDKs reject responses without an id")
	}
	if resp.Object != "chat.completion" {
		t.Errorf("object: got %q, want chat.completion", resp.Object)
	}
	if resp.Created == 0 {
		t.Error("created is zero — newer OpenAI SDKs treat this as malformed")
	}
}

// TestModelsListing_V1ReturnsOpenAICanonicalShape verifies
// /v1/models emits {object:"list", data:[{object:"model",...}]}
// the way openai-python expects. The /api/v1/ alias keeps the
// internal {models:[...]} shape covered by the canonical test.
func TestModelsListing_V1ReturnsOpenAICanonicalShape(t *testing.T) {
	prov := modelListingStub{
		openaiStub: openaiStub{model: "gpt-4"},
		models: []chat.ModelInfo{
			{ID: "gpt-4", Provider: "openai", OwnedBy: "openai"},
			{ID: "claude-opus-4-7", Provider: "anthropic", OwnedBy: "anthropic"},
		},
	}
	s := NewServer(WithChatProvider(prov))
	cfg := &config.Config{}
	cfg.API.AuthEnabled = false
	handler := SetupRoutes(s, cfg)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}

	var resp struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v — body=%s", err, rr.Body.String())
	}
	if resp.Object != "list" {
		t.Errorf("object: got %q, want list", resp.Object)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("data: got %d entries, want 2", len(resp.Data))
	}
	for _, m := range resp.Data {
		if m.Object != "model" {
			t.Errorf("entry %q: object = %q, want model", m.ID, m.Object)
		}
		if m.Created == 0 {
			t.Errorf("entry %q: created = 0 (must backfill to time.Now)", m.ID)
		}
	}
}

// TestModelsListing_CanonicalShapeUnchanged defends the
// internal /api/v1/models surface against accidental coercion
// to the OpenAI shape. CLI consumers parse {models:[...]} and
// would break otherwise.
func TestModelsListing_CanonicalShapeUnchanged(t *testing.T) {
	prov := modelListingStub{
		openaiStub: openaiStub{model: "gpt-4"},
		models:     []chat.ModelInfo{{ID: "gpt-4", Provider: "openai"}},
	}
	s := NewServer(WithChatProvider(prov))
	cfg := &config.Config{}
	cfg.API.AuthEnabled = false
	handler := SetupRoutes(s, cfg)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/models", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if !bytes.Contains(rr.Body.Bytes(), []byte(`"models":`)) {
		t.Errorf("canonical /api/v1/models must return {models:[...]}, got %s", rr.Body.String())
	}
}

// modelListingStub satisfies chat.Provider + chat.ModelLister so
// the OpenAI-shape models test can exercise the full code path
// (not the DISCOVERY_UNSUPPORTED 501 branch).
type modelListingStub struct {
	openaiStub
	models []chat.ModelInfo
}

func (m modelListingStub) ListModels(_ context.Context) ([]chat.ModelInfo, error) {
	return m.models, nil
}
