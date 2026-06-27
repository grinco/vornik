package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/bedrock"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
)

// --- helpers (prefixed with the source base name to avoid collisions) ---

// bedrockCovRuntimeClient builds a bedrockruntime client whose HTTP calls land
// on the supplied test server, signed with throwaway static credentials. This
// is the only seam needed to exercise complete()/Converse without a real AWS
// account or network egress.
func bedrockCovRuntimeClient(t *testing.T, endpoint string) *bedrockruntime.Client {
	t.Helper()
	return bedrockruntime.New(bedrockruntime.Options{
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider("AKIDTEST", "SECRET", ""),
		BaseEndpoint: aws.String(endpoint),
	})
}

// bedrockCovControlClient builds a bedrock (control-plane) client for the
// ListFoundationModels live-catalog path.
func bedrockCovControlClient(t *testing.T, endpoint string) *bedrock.Client {
	t.Helper()
	return bedrock.New(bedrock.Options{
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider("AKIDTEST", "SECRET", ""),
		BaseEndpoint: aws.String(endpoint),
	})
}

// bedrockCovEvent encodes a single Bedrock ConverseStream event frame in the
// vnd.amazon.eventstream wire format the SDK decoder expects.
func bedrockCovEvent(t *testing.T, eventType, payload string) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := eventstream.NewEncoder()
	msg := eventstream.Message{
		Headers: eventstream.Headers{
			{Name: ":message-type", Value: eventstream.StringValue("event")},
			{Name: ":event-type", Value: eventstream.StringValue(eventType)},
			{Name: ":content-type", Value: eventstream.StringValue("application/json")},
		},
		Payload: []byte(payload),
	}
	if err := enc.Encode(&buf, msg); err != nil {
		t.Fatalf("encode eventstream %s: %v", eventType, err)
	}
	return buf.Bytes()
}

// bedrockCovConverseJSON is the canonical non-streaming Converse response body.
func bedrockCovConverseJSON(text, stopReason string, in, out, total int) string {
	resp := map[string]any{
		"output": map[string]any{
			"message": map[string]any{
				"role":    "assistant",
				"content": []any{map[string]any{"text": text}},
			},
		},
		"stopReason": stopReason,
		"usage":      map[string]any{"inputTokens": in, "outputTokens": out, "totalTokens": total},
	}
	b, _ := json.Marshal(resp)
	return string(b)
}

// --- complete() / Complete() / CompleteWithTools() ---

// TestBedrockCov_Complete_Success drives the happy path end-to-end through the
// real SDK marshaller, asserting the request hits the Converse route for the
// pinned model and the JSON body parses back into Content + usage.
func TestBedrockCov_Complete_Success(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		if r.Header.Get("Authorization") == "" {
			t.Errorf("request was not SigV4-signed (no Authorization header)")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(bedrockCovConverseJSON("hello world", "end_turn", 11, 7, 18)))
	}))
	defer srv.Close()

	p := &BedrockProvider{model: "anthropic.claude-3-sonnet", region: "us-east-1", client: bedrockCovRuntimeClient(t, srv.URL)}
	resp, err := p.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if want := "/model/anthropic.claude-3-sonnet/converse"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if gotBody["messages"] == nil {
		t.Errorf("request body missing messages: %+v", gotBody)
	}
	if resp.Choices[0].Message.Content != "hello world" {
		t.Errorf("content = %q", resp.Choices[0].Message.Content)
	}
	if resp.Usage.TotalTokens != 18 || resp.Usage.PromptTokens != 11 || resp.Usage.CompletionTokens != 7 {
		t.Errorf("usage = %+v", resp.Usage)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", resp.Choices[0].FinishReason)
	}
}

// TestBedrockCov_CompleteWithTools_RequestShape asserts a tool offered through
// the OpenAI shape is translated into Bedrock's toolConfig on the wire.
func TestBedrockCov_CompleteWithTools_RequestShape(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(bedrockCovConverseJSON("ok", "end_turn", 1, 1, 2)))
	}))
	defer srv.Close()

	p := &BedrockProvider{model: "anthropic.claude-3", region: "us-east-1", client: bedrockCovRuntimeClient(t, srv.URL)}
	tools := []Tool{{
		Type: "function",
		Function: ToolFunction{
			Name:        "get_weather",
			Description: "lookup",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		},
	}}
	if _, err := p.CompleteWithTools(context.Background(), []Message{{Role: "user", Content: "weather?"}}, tools); err != nil {
		t.Fatalf("CompleteWithTools: %v", err)
	}
	tc, ok := gotBody["toolConfig"].(map[string]any)
	if !ok {
		t.Fatalf("request missing toolConfig: %+v", gotBody)
	}
	arr, _ := tc["tools"].([]any)
	if len(arr) != 1 {
		t.Fatalf("toolConfig.tools = %+v, want 1", arr)
	}
}

// TestBedrockCov_Complete_MaxTokens covers the InferenceConfig precedence:
// the per-request ctx max_tokens wins over the construction-time default.
func TestBedrockCov_Complete_MaxTokens(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(bedrockCovConverseJSON("ok", "end_turn", 1, 1, 2)))
	}))
	defer srv.Close()

	p := &BedrockProvider{model: "m", region: "us-east-1", maxTokens: 100, client: bedrockCovRuntimeClient(t, srv.URL)}
	ctx := WithRequestMaxTokens(context.Background(), 4096)
	if _, err := p.Complete(ctx, []Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	ic, ok := gotBody["inferenceConfig"].(map[string]any)
	if !ok {
		t.Fatalf("missing inferenceConfig: %+v", gotBody)
	}
	if got := ic["maxTokens"].(float64); got != 4096 {
		t.Errorf("maxTokens = %v, want 4096 (ctx override)", got)
	}
}

// TestBedrockCov_Complete_APIError covers the error wrapping path: a 4xx from
// the service surfaces as a wrapped "bedrock Converse(model)" error and an
// error metric is recorded.
func TestBedrockCov_Complete_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Amzn-Errortype", "ValidationException")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"bad model"}`))
	}))
	defer srv.Close()

	reg := prometheus.NewRegistry()
	p := &BedrockProvider{model: "badmodel", region: "us-east-1", client: bedrockCovRuntimeClient(t, srv.URL)}
	p.SetMetrics(NewMetrics(reg))
	_, err := p.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err == nil {
		t.Fatal("expected error from 400 response")
	}
	if !strings.Contains(err.Error(), "bedrock Converse(badmodel)") {
		t.Errorf("error = %v, want wrapped with model", err)
	}
}

// TestBedrockCov_Complete_EmptyModel and _EmptyMessages cover the two guard
// clauses at the top of complete().
func TestBedrockCov_Complete_EmptyModel(t *testing.T) {
	p := &BedrockProvider{model: "", region: "us-east-1"}
	if _, err := p.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}}); !errors.Is(err, ErrEmptyModel) {
		t.Errorf("err = %v, want ErrEmptyModel", err)
	}
}

func TestBedrockCov_Complete_EmptyMessages(t *testing.T) {
	p := &BedrockProvider{model: "m", region: "us-east-1"}
	_, err := p.Complete(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "empty messages") {
		t.Errorf("err = %v, want empty-messages error", err)
	}
}

// TestBedrockCov_Complete_EmitResponseUnwrap exercises the json_schema
// enforcement path: the synthetic emit_response tool is registered + forced,
// the model "returns" its answer as that tool's arguments, and complete()
// unwraps it back into Message.Content (no leftover tool_call).
func TestBedrockCov_Complete_EmitResponseUnwrap(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		// Respond with a toolUse block carrying the answer payload.
		resp := map[string]any{
			"output": map[string]any{
				"message": map[string]any{
					"role": "assistant",
					"content": []any{map[string]any{
						"toolUse": map[string]any{
							"toolUseId": "tu_1",
							"name":      "answer_schema",
							"input":     map[string]any{"answer": "42"},
						},
					}},
				},
			},
			"stopReason": "tool_use",
			"usage":      map[string]any{"inputTokens": 5, "outputTokens": 5, "totalTokens": 10},
		}
		b, _ := json.Marshal(resp)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}))
	defer srv.Close()

	p := &BedrockProvider{model: "m", region: "us-east-1", client: bedrockCovRuntimeClient(t, srv.URL)}
	ctx := WithRequestResponseFormatStruct(context.Background(), &ResponseFormat{
		Type: "json_schema",
		JSONSchema: &ResponseJSONSchema{
			Name:   "answer_schema",
			Schema: json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`),
		},
	})
	resp, err := p.Complete(ctx, []Message{{Role: "user", Content: "what?"}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// Request must have forced ToolChoice to a specific tool.
	tc, _ := gotBody["toolConfig"].(map[string]any)
	if tc == nil || tc["toolChoice"] == nil {
		t.Errorf("expected forced toolChoice in request: %+v", gotBody)
	}
	// The single emit_response tool_call should be unwrapped into Content.
	if len(resp.Choices[0].Message.ToolCalls) != 0 {
		t.Errorf("emit_response should be unwrapped, got tool_calls=%+v", resp.Choices[0].Message.ToolCalls)
	}
	if !strings.Contains(resp.Choices[0].Message.Content, "42") {
		t.Errorf("unwrapped content = %q, want the answer payload", resp.Choices[0].Message.Content)
	}
}

// TestBedrockCov_Complete_DebugLogging walks the debug-level logging branch in
// complete() so the structured-log assembly is exercised.
func TestBedrockCov_Complete_DebugLogging(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(bedrockCovConverseJSON("hi", "end_turn", 1, 1, 2)))
	}))
	defer srv.Close()

	var logBuf bytes.Buffer
	logger := zerolog.New(&logBuf).Level(zerolog.DebugLevel)
	p := &BedrockProvider{model: "m", region: "us-east-1", logger: logger, client: bedrockCovRuntimeClient(t, srv.URL)}
	if _, err := p.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !strings.Contains(logBuf.String(), "converse completed") {
		t.Errorf("expected debug log line, got %q", logBuf.String())
	}
}

// TestBedrockCov_Complete_ToolCallAndReasoning drives the non-streaming
// response converter through a multi-block message: visible text, a
// reasoningContent block (must land in ReasoningContent, never Content), and a
// real toolUse block surfaced as a ToolCall with tool_calls finish reason.
func TestBedrockCov_Complete_ToolCallAndReasoning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"output": map[string]any{
				"message": map[string]any{
					"role": "assistant",
					"content": []any{
						map[string]any{"text": "Let me check."},
						map[string]any{"reasoningContent": map[string]any{
							"reasoningText": map[string]any{"text": "internal chain of thought"},
						}},
						map[string]any{"toolUse": map[string]any{
							"toolUseId": "tu_42",
							"name":      "get_weather",
							"input":     map[string]any{"city": "Prague"},
						}},
					},
				},
			},
			"stopReason": "tool_use",
			"usage":      map[string]any{"inputTokens": 12, "outputTokens": 8, "totalTokens": 20},
		}
		b, _ := json.Marshal(resp)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}))
	defer srv.Close()

	p := &BedrockProvider{model: "m", region: "us-east-1", client: bedrockCovRuntimeClient(t, srv.URL)}
	tools := []Tool{{Type: "function", Function: ToolFunction{
		Name:       "get_weather",
		Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
	}}}
	resp, err := p.CompleteWithTools(context.Background(), []Message{{Role: "user", Content: "weather?"}}, tools)
	if err != nil {
		t.Fatalf("CompleteWithTools: %v", err)
	}
	msg := resp.Choices[0].Message
	if msg.Content != "Let me check." {
		t.Errorf("content = %q", msg.Content)
	}
	if msg.ReasoningContent != "internal chain of thought" {
		t.Errorf("reasoning = %q", msg.ReasoningContent)
	}
	if strings.Contains(msg.Content, "chain of thought") {
		t.Errorf("reasoning leaked into visible content: %q", msg.Content)
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Function.Name != "get_weather" {
		t.Fatalf("tool calls = %+v", msg.ToolCalls)
	}
	if !strings.Contains(msg.ToolCalls[0].Function.Arguments, "Prague") {
		t.Errorf("tool args = %q, want city Prague", msg.ToolCalls[0].Function.Arguments)
	}
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", resp.Choices[0].FinishReason)
	}
}

// --- CompleteWithToolsStream + readBedrockStream ---

// TestBedrockCov_Stream_Success exercises the full ConverseStream success path:
// eventstream decoding, incremental onText callbacks, and final usage folding.
func TestBedrockCov_Stream_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/converse-stream") {
			t.Errorf("path = %q, want converse-stream", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		_, _ = w.Write(bedrockCovEvent(t, "messageStart", `{"role":"assistant"}`))
		_, _ = w.Write(bedrockCovEvent(t, "contentBlockDelta", `{"delta":{"text":"Hel"},"contentBlockIndex":0}`))
		_, _ = w.Write(bedrockCovEvent(t, "contentBlockDelta", `{"delta":{"text":"lo"},"contentBlockIndex":0}`))
		_, _ = w.Write(bedrockCovEvent(t, "contentBlockStop", `{"contentBlockIndex":0}`))
		_, _ = w.Write(bedrockCovEvent(t, "messageStop", `{"stopReason":"end_turn"}`))
		_, _ = w.Write(bedrockCovEvent(t, "metadata", `{"usage":{"inputTokens":9,"outputTokens":2,"totalTokens":11}}`))
	}))
	defer srv.Close()

	p := &BedrockProvider{model: "anthropic.claude-3", region: "us-east-1", client: bedrockCovRuntimeClient(t, srv.URL)}
	var seen []string
	resp, err := p.CompleteWithToolsStream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil,
		func(s string) { seen = append(seen, s) })
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if resp.Choices[0].Message.Content != "Hello" {
		t.Errorf("content = %q, want Hello", resp.Choices[0].Message.Content)
	}
	// onText is called with the *accumulated* text after every delta.
	if len(seen) != 2 || seen[0] != "Hel" || seen[1] != "Hello" {
		t.Errorf("onText sequence = %v, want [Hel Hello]", seen)
	}
	if resp.Usage.TotalTokens != 11 {
		t.Errorf("usage total = %d, want 11", resp.Usage.TotalTokens)
	}
}

// TestBedrockCov_Stream_ToolUseAndReasoning covers the tool-use delta
// accumulation (args JSON spread over multiple frames) plus reasoning-content
// deltas that must NOT stream through onText.
func TestBedrockCov_Stream_ToolUseAndReasoning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		_, _ = w.Write(bedrockCovEvent(t, "messageStart", `{"role":"assistant"}`))
		// reasoning delta (should land in ReasoningContent, not onText)
		_, _ = w.Write(bedrockCovEvent(t, "contentBlockDelta", `{"delta":{"reasoningContent":{"text":"thinking..."}},"contentBlockIndex":0}`))
		// tool use start + two arg fragments
		_, _ = w.Write(bedrockCovEvent(t, "contentBlockStart", `{"start":{"toolUse":{"toolUseId":"tu_9","name":"do_thing"}},"contentBlockIndex":1}`))
		_, _ = w.Write(bedrockCovEvent(t, "contentBlockDelta", `{"delta":{"toolUse":{"input":"{\"x\":"}},"contentBlockIndex":1}`))
		_, _ = w.Write(bedrockCovEvent(t, "contentBlockDelta", `{"delta":{"toolUse":{"input":"1}"}},"contentBlockIndex":1}`))
		_, _ = w.Write(bedrockCovEvent(t, "messageStop", `{"stopReason":"tool_use"}`))
	}))
	defer srv.Close()

	p := &BedrockProvider{model: "m", region: "us-east-1", client: bedrockCovRuntimeClient(t, srv.URL)}
	var textSeen string
	resp, err := p.CompleteWithToolsStream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil,
		func(s string) { textSeen = s })
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if textSeen != "" {
		t.Errorf("reasoning leaked into onText: %q", textSeen)
	}
	if resp.Choices[0].Message.ReasoningContent != "thinking..." {
		t.Errorf("reasoning = %q", resp.Choices[0].Message.ReasoningContent)
	}
	calls := resp.Choices[0].Message.ToolCalls
	if len(calls) != 1 || calls[0].Function.Name != "do_thing" || calls[0].Function.Arguments != `{"x":1}` {
		t.Errorf("tool calls = %+v, want one do_thing with args {\"x\":1}", calls)
	}
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", resp.Choices[0].FinishReason)
	}
}

// TestBedrockCov_Stream_FallbackToNonStreaming covers the branch where
// ConverseStream returns an initial error and the provider transparently
// retries with non-streaming Converse, invoking onText once with the full text.
func TestBedrockCov_Stream_FallbackToNonStreaming(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/converse-stream") {
			// Streaming not supported for this model.
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Amzn-Errortype", "ValidationException")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":"streaming unsupported"}`))
			return
		}
		// Non-streaming fallback.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(bedrockCovConverseJSON("fallback text", "end_turn", 3, 4, 7)))
	}))
	defer srv.Close()

	p := &BedrockProvider{model: "m", region: "us-east-1", client: bedrockCovRuntimeClient(t, srv.URL)}
	var calls int
	var lastText string
	resp, err := p.CompleteWithToolsStream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil,
		func(s string) { calls++; lastText = s })
	if err != nil {
		t.Fatalf("stream fallback: %v", err)
	}
	if resp.Choices[0].Message.Content != "fallback text" {
		t.Errorf("content = %q", resp.Choices[0].Message.Content)
	}
	if calls != 1 || lastText != "fallback text" {
		t.Errorf("onText calls=%d lastText=%q, want one call with full text", calls, lastText)
	}
}

func TestBedrockCov_Stream_EmptyModel(t *testing.T) {
	p := &BedrockProvider{model: "", region: "us-east-1"}
	if _, err := p.CompleteWithToolsStream(context.Background(), []Message{{Role: "user", Content: "x"}}, nil, nil); !errors.Is(err, ErrEmptyModel) {
		t.Errorf("err = %v, want ErrEmptyModel", err)
	}
}

func TestBedrockCov_Stream_EmptyMessages(t *testing.T) {
	p := &BedrockProvider{model: "m", region: "us-east-1"}
	_, err := p.CompleteWithToolsStream(context.Background(), nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "empty messages") {
		t.Errorf("err = %v, want empty-messages error", err)
	}
}

// --- fetchLiveCatalog / ListModels live path ---

// TestBedrockCov_LiveCatalog_SuccessAndCache exercises a successful
// ListFoundationModels call, the text-modality filter, and the cache-hit
// fast path on the second call (one HTTP round-trip only).
func TestBedrockCov_LiveCatalog_SuccessAndCache(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		resp := map[string]any{
			"modelSummaries": []any{
				map[string]any{
					"modelId":          "anthropic.claude-3-sonnet",
					"providerName":     "Anthropic",
					"inputModalities":  []string{"TEXT"},
					"outputModalities": []string{"TEXT"},
				},
				map[string]any{ // filtered out: image output
					"modelId":          "amazon.titan-image",
					"providerName":     "Amazon",
					"inputModalities":  []string{"TEXT"},
					"outputModalities": []string{"IMAGE"},
				},
				map[string]any{ // filtered out: empty id
					"modelId":          "",
					"inputModalities":  []string{"TEXT"},
					"outputModalities": []string{"TEXT"},
				},
			},
		}
		b, _ := json.Marshal(resp)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}))
	defer srv.Close()

	p := &BedrockProvider{
		model: "m", region: "us-east-1",
		liveCatalog: &bedrockLiveCatalog{client: bedrockCovControlClient(t, srv.URL), ttl: time.Hour},
	}
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 1 || models[0].ID != "anthropic.claude-3-sonnet" {
		t.Fatalf("models = %+v, want one text model", models)
	}
	if models[0].Source != "live" || models[0].Provider != "bedrock" || models[0].OwnedBy != "Anthropic" {
		t.Errorf("model metadata = %+v", models[0])
	}
	// Second call must be served from cache (no new HTTP hit).
	if _, err := p.ListModels(context.Background()); err != nil {
		t.Fatalf("second ListModels: %v", err)
	}
	if hits != 1 {
		t.Errorf("ListFoundationModels HTTP hits = %d, want 1 (cache hit on 2nd call)", hits)
	}
}

// TestBedrockCov_LiveCatalog_ErrorFallsBackToStatic covers the failure branch
// where the control-plane call errors and ListModels returns the static list.
func TestBedrockCov_LiveCatalog_ErrorFallsBackToStatic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Amzn-Errortype", "AccessDeniedException")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"no ListFoundationModels permission"}`))
	}))
	defer srv.Close()

	static := []ModelInfo{{ID: "anthropic.claude-static"}}
	p := &BedrockProvider{
		model: "m", region: "us-east-1",
		staticModelList: static,
		logger:          zerolog.Nop(),
		liveCatalog:     &bedrockLiveCatalog{client: bedrockCovControlClient(t, srv.URL), ttl: time.Hour},
	}
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 1 || models[0].ID != "anthropic.claude-static" {
		t.Fatalf("models = %+v, want static fallback", models)
	}
	if models[0].Source != "static" || models[0].Provider != "bedrock" {
		t.Errorf("static model metadata = %+v", models[0])
	}
	// lastErr should have been recorded on the catalog.
	if p.liveCatalog.lastErr == nil {
		t.Errorf("expected liveCatalog.lastErr to be set after failure")
	}
}

// TestBedrockCov_FetchLiveCatalog_NotConfigured covers the nil-client guard.
func TestBedrockCov_FetchLiveCatalog_NotConfigured(t *testing.T) {
	p := &BedrockProvider{model: "m", region: "us-east-1", liveCatalog: &bedrockLiveCatalog{ttl: time.Hour}}
	_, err := p.fetchLiveCatalog(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Errorf("err = %v, want not-configured error", err)
	}
}
