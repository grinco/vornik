package chat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// This file adds high-value coverage for the OpenAI-compatible HTTP
// client's non-streaming hot path (doComplete) — the path every agent
// and chat-proxy call lands on when the configured backend is an
// OpenAI-compat gateway (bedrock-access-gateway, Vertex, Ollama,
// OpenRouter). The streaming variant's wire fields are covered in
// stream_request_fields_test.go; the construction-time max_tokens body
// in window_size_regression_test.go. The cases below target the
// remaining gaps: request translation of context-size / thinking /
// reasoning / per-request response_format / tools onto the non-stream
// wire body, response-side usage + prompt-cache token accounting,
// end-to-end 429 retry + 5xx error mapping through doComplete, the
// live ListModels filter + Ping paths, and a couple of router dispatch
// edges that the existing router_test.go doesn't pin.

// captureWireBody spins an httptest server that records the inbound
// request body + path/method/headers, replies with a minimal OK chat
// completion, and returns a teardown. The recorded body is decoded by
// the caller. Reuses the package's ChatResponse wire shape.
func captureWireBody(t *testing.T, onReq func(r *http.Request, body []byte)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if onReq != nil {
			onReq(r, body)
		}
		w.Header().Set("Content-Type", "application/json")
		encodeOKResponse(w)
	}))
}

// decodeWireRequest unmarshals a captured outbound body into a generic
// map so a test can assert on fields that the typed ChatRequest omits
// or shapes differently (options.num_ctx, thinking, etc.).
func decodeWireMap(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode wire body: %v (body=%s)", err, body)
	}
	return m
}

// --- Group A: request translation onto the non-stream wire body ------

// TestDoComplete_WireBody_ContextSizeBecomesNumCtx — WithContextSize
// must surface as options.num_ctx on the wire (the Ollama window knob).
func TestDoComplete_WireBody_ContextSizeBecomesNumCtx(t *testing.T) {
	var captured map[string]any
	srv := captureWireBody(t, func(_ *http.Request, b []byte) { captured = decodeWireMap(t, b) })
	defer srv.Close()

	c := NewClient(srv.URL, "k", "qwen3.6:35b", WithContextSize(262144))
	if _, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	opts, ok := captured["options"].(map[string]any)
	if !ok {
		t.Fatalf("options missing on wire body; got %v", captured["options"])
	}
	if got := opts["num_ctx"]; got != float64(262144) {
		t.Errorf("options.num_ctx = %v, want 262144", got)
	}
}

// TestDoComplete_WireBody_NoContextSizeOmitsOptions — without
// WithContextSize the options map must be omitted entirely (omitempty),
// not sent as null/empty, so providers that reject unknown keys stay happy.
func TestDoComplete_WireBody_NoContextSizeOmitsOptions(t *testing.T) {
	var captured map[string]any
	srv := captureWireBody(t, func(_ *http.Request, b []byte) { captured = decodeWireMap(t, b) })
	defer srv.Close()

	c := NewClient(srv.URL, "k", "gpt-4")
	if _, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, present := captured["options"]; present {
		t.Errorf("options must be omitted when context size unset; got %v", captured["options"])
	}
}

// TestDoComplete_WireBody_ResponseFormatFromContext_JSONObject — item 7
// of the deterministic-output plan: a json_object directive stamped on
// ctx by the chat-proxy MUST reach the non-stream upstream wire body.
// Pre-fix, the non-stream Complete dropped it and the model emitted prose.
func TestDoComplete_WireBody_ResponseFormatFromContext_JSONObject(t *testing.T) {
	var captured map[string]any
	srv := captureWireBody(t, func(_ *http.Request, b []byte) { captured = decodeWireMap(t, b) })
	defer srv.Close()

	c := NewClient(srv.URL, "k", "gpt-4")
	ctx := WithRequestResponseFormatStruct(context.Background(), &ResponseFormat{Type: "json_object"})
	if _, err := c.Complete(ctx, []Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	rf, ok := captured["response_format"].(map[string]any)
	if !ok {
		t.Fatalf("response_format missing on wire; got %v", captured["response_format"])
	}
	if rf["type"] != "json_object" {
		t.Errorf("response_format.type = %v, want json_object", rf["type"])
	}
}

// TestDoComplete_WireBody_ResponseFormatFromContext_JSONSchema — the
// typed json_schema variant must carry its full schema body through to
// the wire (the OpenAI-compat passthrough path; bedrock translates it
// to a forced tool elsewhere).
func TestDoComplete_WireBody_ResponseFormatFromContext_JSONSchema(t *testing.T) {
	var captured map[string]any
	srv := captureWireBody(t, func(_ *http.Request, b []byte) { captured = decodeWireMap(t, b) })
	defer srv.Close()

	c := NewClient(srv.URL, "k", "gpt-4")
	rf := &ResponseFormat{Type: "json_schema", JSONSchema: &ResponseJSONSchema{
		Name:   "verdict",
		Schema: json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`),
		Strict: true,
	}}
	ctx := WithRequestResponseFormatStruct(context.Background(), rf)
	if _, err := c.CompleteWithTools(ctx, []Message{{Role: "user", Content: "hi"}}, nil); err != nil {
		t.Fatalf("CompleteWithTools: %v", err)
	}
	wireRF, ok := captured["response_format"].(map[string]any)
	if !ok {
		t.Fatalf("response_format missing; got %v", captured["response_format"])
	}
	js, ok := wireRF["json_schema"].(map[string]any)
	if !ok {
		t.Fatalf("json_schema missing; got %v", wireRF["json_schema"])
	}
	if js["name"] != "verdict" {
		t.Errorf("json_schema.name = %v, want verdict", js["name"])
	}
	if _, ok := js["schema"].(map[string]any); !ok {
		t.Errorf("json_schema.schema must carry the schema object; got %v", js["schema"])
	}
}

// TestDoComplete_WireBody_NoContextResponseFormat_StaysAbsent — when
// ctx carries no directive the wire body must NOT sprout a
// response_format key, preserving the free-form default for the
// back-compat callers that build a custom ChatRequest.
func TestDoComplete_WireBody_NoContextResponseFormat_StaysAbsent(t *testing.T) {
	var captured map[string]any
	srv := captureWireBody(t, func(_ *http.Request, b []byte) { captured = decodeWireMap(t, b) })
	defer srv.Close()

	c := NewClient(srv.URL, "k", "gpt-4")
	if _, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, present := captured["response_format"]; present {
		t.Errorf("response_format must stay absent without a ctx directive; got %v", captured["response_format"])
	}
}

// TestDoComplete_WireBody_ToolsCarried — CompleteWithTools must place
// the tool definitions (and their JSON-schema parameters) on the wire.
func TestDoComplete_WireBody_ToolsCarried(t *testing.T) {
	var sent ChatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&sent)
		w.Header().Set("Content-Type", "application/json")
		encodeOKResponse(w)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", "gpt-4")
	tools := []Tool{{
		Type: "function",
		Function: ToolFunction{
			Name:        "create_task",
			Description: "Create a task",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"title":{"type":"string"}}}`),
		},
	}}
	if _, err := c.CompleteWithTools(context.Background(), []Message{{Role: "user", Content: "hi"}}, tools); err != nil {
		t.Fatalf("CompleteWithTools: %v", err)
	}
	if len(sent.Tools) != 1 {
		t.Fatalf("wire tools count = %d, want 1", len(sent.Tools))
	}
	if sent.Tools[0].Function.Name != "create_task" {
		t.Errorf("wire tool name = %q, want create_task", sent.Tools[0].Function.Name)
	}
	if !strings.Contains(string(sent.Tools[0].Function.Parameters), `"title"`) {
		t.Errorf("wire tool parameters lost the schema body: %s", sent.Tools[0].Function.Parameters)
	}
}

// TestDoComplete_WireBody_PathMethodContentType — the request must POST
// to <endpoint>/chat/completions with a JSON content type. A regression
// here (e.g. a normalizeEndpoint bug) would 404 every agent call.
func TestDoComplete_WireBody_PathMethodContentType(t *testing.T) {
	var gotPath, gotMethod, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod, gotCT = r.URL.Path, r.Method, r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		encodeOKResponse(w)
	}))
	defer srv.Close()

	// Endpoint deliberately carries a trailing /chat/completions +
	// slash so we exercise normalizeEndpoint's trimming.
	c := NewClient(srv.URL+"/chat/completions/", "k", "gpt-4")
	if _, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("path = %q, want /chat/completions", gotPath)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotCT != "application/json" {
		t.Errorf("content-type = %q, want application/json", gotCT)
	}
}

// --- Group B: response decode + usage / cache-token accounting -------

// TestDoComplete_DecodesCacheTokens — the prompt-cache token counts
// (cache_creation_tokens / cache_read_tokens) emitted by Bedrock /
// Anthropic-compat gateways must round-trip into ChatResponse.Usage so
// the spend dashboards and cache-hit ratio can read them.
func TestDoComplete_DecodesCacheTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"x","object":"chat.completion","model":"m",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1000,"completion_tokens":50,"total_tokens":1050,
				"cache_creation_tokens":800,"cache_read_tokens":600}
		}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", "m")
	resp, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Usage.PromptTokens != 1000 || resp.Usage.CompletionTokens != 50 {
		t.Errorf("token counts: prompt=%d completion=%d", resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
	}
	if resp.Usage.CacheCreationTokens != 800 {
		t.Errorf("cache_creation_tokens = %d, want 800", resp.Usage.CacheCreationTokens)
	}
	if resp.Usage.CacheReadTokens != 600 {
		t.Errorf("cache_read_tokens = %d, want 600", resp.Usage.CacheReadTokens)
	}
}

// TestDoComplete_UsageDrivesMetrics — with metrics wired, a successful
// completion carrying usage must not panic and must increment the
// per-model token counters (smoke-level; the counter vectors aren't
// exported for direct readback, matching record_metrics_test.go).
func TestDoComplete_UsageDrivesMetrics(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","model":"m",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":12,"completion_tokens":7,"total_tokens":19}}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", "m", WithMetrics(newRealMetrics()))
	resp, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Usage.TotalTokens != 19 {
		t.Errorf("total tokens = %d, want 19", resp.Usage.TotalTokens)
	}
}

// TestDoComplete_MalformedJSONBody_InvalidResponse — a 200 with a body
// that isn't a chat completion must surface ErrInvalidResponse, not a
// silent empty response the dispatcher would treat as a blank turn.
func TestDoComplete_MalformedJSONBody_InvalidResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","choices": "not-an-array"}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", "m")
	_, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("want ErrInvalidResponse, got %v", err)
	}
}

// --- Group C: error mapping + retry end-to-end through doComplete -----

// TestDoComplete_429RetriedThenSucceeds — the OpenAI-compat client opts
// into withRetryOn429 + withGenericBackoffOn429, so a transient 429
// (no Retry-After, the Vertex RESOURCE_EXHAUSTED shape) is retried and
// the second attempt's 200 is returned. Exercises the full doComplete
// retry wiring, not just the helper.
func TestDoComplete_429RetriedThenSucceeds(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			http.Error(w, `{"error":{"code":429,"message":"Resource exhausted"}}`, http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		encodeOKResponse(w)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", "m")
	resp, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("Complete should succeed after 429 retry: %v", err)
	}
	if resp.Choices[0].Message.Content != "ok" {
		t.Errorf("content = %q, want ok", resp.Choices[0].Message.Content)
	}
	if got := n.Load(); got != 2 {
		t.Errorf("want 2 attempts (429 then 200), got %d", got)
	}
}

// TestDoComplete_429Exhausted_GatewayError — when every attempt 429s,
// doComplete must surface a *GatewayError(429) (Retryable) with the
// upstream error.message parsed out so dispatcher recovery + metrics
// see the real cause, not a bare retryableHTTPError.
func TestDoComplete_429Exhausted_GatewayError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"code":429,"message":"quota exceeded"}}`, http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", "m")
	_, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	var g *GatewayError
	if !errors.As(err, &g) {
		t.Fatalf("want *GatewayError, got %T: %v", err, err)
	}
	if g.Status != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", g.Status)
	}
	if !g.Retryable() {
		t.Errorf("429 GatewayError must be retryable")
	}
	if g.Message != "quota exceeded" {
		t.Errorf("message = %q, want parsed 'quota exceeded'", g.Message)
	}
}

// TestDoComplete_5xxNotRetried_SurfacesImmediately — the client sets
// withNo5xxRetry so a 5xx surfaces as a GatewayError on the FIRST
// attempt (the dispatcher's prune-and-retry handles 5xx, a blind client
// retry of the same bloated history would just fail again). Asserting
// the single-attempt count is the load-bearing part.
func TestDoComplete_5xxNotRetried_SurfacesImmediately(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"error":{"code":502,"message":"upstream down"}}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", "m")
	_, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	var g *GatewayError
	if !errors.As(err, &g) {
		t.Fatalf("want *GatewayError, got %T: %v", err, err)
	}
	if g.Status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", g.Status)
	}
	if got := n.Load(); got != 1 {
		t.Errorf("5xx must NOT be retried at the client layer; got %d attempts", got)
	}
}

// TestDoComplete_4xxMalformedErrorBody_BodyPreserved — a non-JSON error
// body on a 4xx must still produce a GatewayError: Message empty (no
// parseable error.message) but the raw Body carried for diagnostics.
func TestDoComplete_4xxMalformedErrorBody_BodyPreserved(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `<html>nginx 400</html>`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", "m")
	_, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	var g *GatewayError
	if !errors.As(err, &g) {
		t.Fatalf("want *GatewayError, got %T: %v", err, err)
	}
	if g.Retryable() {
		t.Errorf("400 must not be retryable")
	}
	if g.Message != "" {
		t.Errorf("message should be empty for an unparseable error body; got %q", g.Message)
	}
	if !strings.Contains(g.Body, "nginx 400") {
		t.Errorf("raw body must be preserved for diagnostics; got %q", g.Body)
	}
}

// TestDoComplete_ContextDeadline_RequestFailed — a per-request context
// deadline that fires mid-flight must surface ErrRequestFailed (the
// transport error path), not a GatewayError.
func TestDoComplete_ContextDeadline_RequestFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(80 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		encodeOKResponse(w)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", "m")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := c.Complete(ctx, []Message{{Role: "user", Content: "hi"}})
	if err == nil {
		t.Fatal("want error from context deadline")
	}
	if !errors.Is(err, ErrRequestFailed) {
		t.Errorf("want ErrRequestFailed wrap, got %v", err)
	}
	var g *GatewayError
	if errors.As(err, &g) {
		t.Errorf("transport error must not be a GatewayError; got %v", g)
	}
}

// --- Group D: ListModels live path + Ping --------------------------

// TestListModels_LiveFilterApplied — WithModelListFilter must drop
// non-matching models on the LIVE fetch path (the static path is
// covered in models_list_test.go). OpenRouter uses this to surface only
// :free models.
func TestListModels_LiveFilterApplied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[
			{"id":"deepseek/deepseek-r1:free","owned_by":"deepseek"},
			{"id":"openai/gpt-5","owned_by":"openai"},
			{"id":"qwen/qwen3:free","owned_by":"qwen"}
		]}`)
	}))
	defer srv.Close()

	freeOnly := func(m ModelInfo) bool { return strings.HasSuffix(m.ID, ":free") }
	c := NewClient(srv.URL, "k", "m", WithModelListFilter(freeOnly))
	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("filter should keep 2 :free models, got %d (%+v)", len(models), models)
	}
	for _, m := range models {
		if !strings.HasSuffix(m.ID, ":free") {
			t.Errorf("non-free model leaked through filter: %s", m.ID)
		}
		if m.Source != "live" {
			t.Errorf("live-fetched model Source = %q, want live", m.Source)
		}
	}
}

// TestPing_SucceedsViaModels — Ping piggybacks on the live /models
// probe; a healthy gateway returns nil.
func TestPing_SucceedsViaModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("Ping should hit /models, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"m"}]}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", "m")
	if err := c.Ping(context.Background()); err != nil {
		t.Errorf("Ping should succeed against a healthy gateway: %v", err)
	}
}

// TestPing_SurfacesModelsError — an unhealthy gateway (non-2xx on
// /models) must make Ping return an error so the daemon startup gate
// can hold the fallback offline.
func TestPing_SurfacesModelsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", "m")
	if err := c.Ping(context.Background()); err == nil {
		t.Error("Ping must surface a models-endpoint failure")
	}
}

// TestPing_StaticModelsShortCircuits — a client configured with a
// static catalog has nothing live to probe; Ping returns nil WITHOUT
// any HTTP call (Vertex's openapi surface 404s on /models).
func TestPing_StaticModelsShortCircuits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("static-models client must not make an HTTP Ping call; got %s %s", r.Method, r.URL.Path)
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", "m", WithStaticModelList([]ModelInfo{{ID: "m", Source: "static"}}))
	if err := c.Ping(context.Background()); err != nil {
		t.Errorf("static-models Ping must short-circuit to nil; got %v", err)
	}
}

// --- Group E: client WithModel clone + multimodal carry-through -----

// TestClient_WithModel_ClonesWithoutMutating — *Client.WithModel must
// return a copy pinned to the new model and leave the original's model
// untouched (two concurrent per-request overrides must not race on a
// shared field). This is the per-role model: dispatch primitive.
func TestClient_WithModel_ClonesWithoutMutating(t *testing.T) {
	c := NewClient("https://example.test", "k", "gpt-4")
	clone := c.WithModel("o3-mini")
	if clone.Model() != "o3-mini" {
		t.Errorf("clone model = %q, want o3-mini", clone.Model())
	}
	if c.Model() != "gpt-4" {
		t.Errorf("original model mutated to %q; WithModel must not mutate the receiver", c.Model())
	}
	// A nil receiver returns nil rather than panicking (guarded in the impl).
	var nilClient *Client
	if got := nilClient.WithModel("x"); got != (*Client)(nil) {
		t.Errorf("nil-receiver WithModel must return the nil client, got %v", got)
	}
}

// TestClient_WithModel_PinnedModelHitsWire — the cloned client must
// actually send the overridden model on the wire, not the original.
func TestClient_WithModel_PinnedModelHitsWire(t *testing.T) {
	var sent ChatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&sent)
		w.Header().Set("Content-Type", "application/json")
		encodeOKResponse(w)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", "gpt-4")
	pinned := c.WithModel("claude-sonnet-4-6")
	if _, err := pinned.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if sent.Model != "claude-sonnet-4-6" {
		t.Errorf("wire model = %q, want claude-sonnet-4-6", sent.Model)
	}
}

// TestDoComplete_MultimodalBlocksCarryToWire — a Message built with
// multimodal Blocks (text + image_url) must serialize as a content
// ARRAY on the OpenAI-compat wire so vision-capable upstreams (Vertex
// Gemini, the bedrock gateway) receive the image. End-to-end through a
// real Complete, complementing the unit-level MarshalJSON tests.
func TestDoComplete_MultimodalBlocksCarryToWire(t *testing.T) {
	var raw map[string]any
	srv := captureWireBody(t, func(_ *http.Request, b []byte) { raw = decodeWireMap(t, b) })
	defer srv.Close()

	c := NewClient(srv.URL, "k", "google/gemini-2.5-pro")
	msg := Message{Role: "user", Blocks: []ContentBlock{
		TextBlock("what is in this picture?"),
		ImageBlock("data:image/png;base64,iVBORw0KGgo="),
	}}
	if _, err := c.Complete(context.Background(), []Message{msg}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	msgs, ok := raw["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("messages malformed on wire: %v", raw["messages"])
	}
	first := msgs[0].(map[string]any)
	content, ok := first["content"].([]any)
	if !ok {
		t.Fatalf("multimodal message content must be an array on the wire; got %T %v", first["content"], first["content"])
	}
	if len(content) != 2 {
		t.Fatalf("want 2 content blocks on the wire, got %d", len(content))
	}
	img := content[1].(map[string]any)
	if img["type"] != "image_url" {
		t.Errorf("second block type = %v, want image_url", img["type"])
	}
}

// TestRouter_ListModels_SkipsNonListerSubs — the aggregator must
// silently skip sub-providers that don't implement ModelLister (e.g. a
// plain CLI provider) rather than erroring, while still returning the
// listers' catalogs.
func TestRouter_ListModels_SkipsNonListerSubs(t *testing.T) {
	lister := &listingStub{
		namedStubProvider: namedStubProvider{name: "http"},
		models:            []ModelInfo{{ID: "m-1", Source: "live"}},
	}
	plain := &namedStubProvider{name: "cli"} // no ListModels
	r, err := NewRouter(lister, nil,
		WithRouterSubs(map[string]Provider{"http": lister, "cli": plain}))
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	got := r.ListModels(context.Background())
	if len(got.Providers["http"]) != 1 {
		t.Errorf("lister sub should contribute its catalog; got %v", got.Providers["http"])
	}
	if _, present := got.Providers["cli"]; present {
		t.Errorf("non-lister sub must be skipped, not appear in Providers; got %v", got.Providers["cli"])
	}
	if len(got.Errors) != 0 {
		t.Errorf("skipping a non-lister sub must not register an error; got %v", got.Errors)
	}
}

// --- Group F: small invariants -------------------------------------

// TestTruncateLogString_Boundaries — the diagnostic body truncation
// used in every GatewayError path: at/under limit passes through; over
// limit gets the "...(truncated)" marker.
func TestTruncateLogString_Boundaries(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		limit int
		want  string
	}{
		{"under limit", "abc", 5, "abc"},
		{"at limit", "abcde", 5, "abcde"},
		{"over limit", "abcdef", 5, "abcde...(truncated)"},
		{"empty", "", 5, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := truncateLogString(c.in, c.limit); got != c.want {
				t.Errorf("truncateLogString(%q, %d) = %q, want %q", c.in, c.limit, got, c.want)
			}
		})
	}
}

// TestGatewayError_ErrorString — Error() prefers the parsed message
// when present, else falls back to the raw body, with the status code
// in both forms.
func TestGatewayError_ErrorString(t *testing.T) {
	withMsg := &GatewayError{Status: 500, Message: "boom", Body: "raw"}
	if !strings.Contains(withMsg.Error(), "boom") || !strings.Contains(withMsg.Error(), "500") {
		t.Errorf("Error() should include message + status; got %q", withMsg.Error())
	}
	bodyOnly := &GatewayError{Status: 503, Body: "service down"}
	if !strings.Contains(bodyOnly.Error(), "service down") || !strings.Contains(bodyOnly.Error(), "503") {
		t.Errorf("Error() should fall back to body + status; got %q", bodyOnly.Error())
	}
}
