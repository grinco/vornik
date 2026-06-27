package chat

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewClient(t *testing.T) {
	tests := []struct {
		name         string
		endpoint     string
		wantEndpoint string
		apiKey       string
		model        string
		opts         []ClientOption
		wantTimeout  time.Duration
	}{
		{
			name:         "basic client",
			endpoint:     "https://api.openai.com",
			wantEndpoint: "https://api.openai.com",
			apiKey:       "test-key",
			model:        "gpt-4",
			wantTimeout:  DefaultTimeout, // default
		},
		{
			name:         "with custom timeout",
			endpoint:     "https://api.openai.com",
			wantEndpoint: "https://api.openai.com",
			apiKey:       "test-key",
			model:        "gpt-4",
			opts:         []ClientOption{WithTimeout(60 * time.Second)},
			wantTimeout:  60 * time.Second,
		},
		{
			name:         "with custom http client",
			endpoint:     "https://api.openai.com",
			wantEndpoint: "https://api.openai.com",
			apiKey:       "test-key",
			model:        "gpt-4",
			opts: []ClientOption{
				WithHTTPClient(&http.Client{Timeout: 45 * time.Second}),
			},
			wantTimeout: DefaultTimeout, // timeout field, not httpClient.Timeout
		},
		{
			name:         "multiple options",
			endpoint:     "https://api.openai.com",
			wantEndpoint: "https://api.openai.com",
			apiKey:       "test-key",
			model:        "gpt-4",
			opts: []ClientOption{
				WithTimeout(45 * time.Second),
				WithHTTPClient(&http.Client{Timeout: 90 * time.Second}),
			},
			wantTimeout: 45 * time.Second,
		},
		{
			name:         "preserves v1 in endpoint",
			endpoint:     "http://127.0.0.1:11434/api/v1",
			wantEndpoint: "http://127.0.0.1:11434/api/v1",
			apiKey:       "test-key",
			model:        "gpt-4",
			wantTimeout:  DefaultTimeout,
		},
		{
			name:         "strips chat/completions suffix only",
			endpoint:     "http://127.0.0.1:11434/api/v1/chat/completions",
			wantEndpoint: "http://127.0.0.1:11434/api/v1",
			apiKey:       "test-key",
			model:        "gpt-4",
			wantTimeout:  DefaultTimeout,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := NewClient(tt.endpoint, tt.apiKey, tt.model, tt.opts...)

			if client.endpoint != tt.wantEndpoint {
				t.Errorf("endpoint = %q, want %q", client.endpoint, tt.wantEndpoint)
			}
			if client.apiKey != tt.apiKey {
				t.Errorf("apiKey = %q, want %q", client.apiKey, tt.apiKey)
			}
			if client.model != tt.model {
				t.Errorf("model = %q, want %q", client.model, tt.model)
			}
			if client.timeout != tt.wantTimeout {
				t.Errorf("timeout = %v, want %v", client.timeout, tt.wantTimeout)
			}
			if client.httpClient == nil {
				t.Error("httpClient should not be nil")
			}
		})
	}
}

func TestClient_Complete_Success(t *testing.T) {
	// Create mock server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("unexpected authorization header: %s", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected content type: %s", r.Header.Get("Content-Type"))
		}

		var req ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("failed to decode request: %v", err)
		}
		if req.Model != "gpt-4" {
			t.Errorf("unexpected model: %s", req.Model)
		}

		resp := ChatResponse{
			ID:     "chatcmpl-test123",
			Object: "chat.completion",
			Model:  "gpt-4",
			Choices: []struct {
				Index        int     `json:"index"`
				Message      Message `json:"message"`
				FinishReason string  `json:"finish_reason"`
			}{
				{
					Index:        0,
					Message:      Message{Role: "assistant", Content: "Hello, world!"},
					FinishReason: "stop",
				},
			},
		}
		resp.Usage.PromptTokens = 10
		resp.Usage.CompletionTokens = 5
		resp.Usage.TotalTokens = 15

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewClient(server.URL, "test-key", "gpt-4")

	messages := []Message{
		{Role: "user", Content: "Hello"},
	}

	resp, err := client.Complete(context.Background(), messages)
	if err != nil {
		t.Fatalf("Complete failed: %v", err)
	}

	if resp.ID != "chatcmpl-test123" {
		t.Errorf("ID = %q, want %q", resp.ID, "chatcmpl-test123")
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("len(Choices) = %d, want 1", len(resp.Choices))
	}
	if resp.Choices[0].Message.Content != "Hello, world!" {
		t.Errorf("Content = %q, want %q", resp.Choices[0].Message.Content, "Hello, world!")
	}
}

func TestClient_Complete_Error(t *testing.T) {
	tests := []struct {
		name        string
		endpoint    string
		model       string
		messages    []Message
		serverError bool
		statusCode  int
		wantErr     error
	}{
		{
			name:     "empty endpoint",
			endpoint: "",
			model:    "gpt-4",
			messages: []Message{{Role: "user", Content: "test"}},
			wantErr:  ErrEmptyEndpoint,
		},
		{
			name:     "empty model",
			endpoint: "https://api.openai.com",
			model:    "",
			messages: []Message{{Role: "user", Content: "test"}},
			wantErr:  ErrEmptyModel,
		},
		{
			name:     "empty messages",
			endpoint: "https://api.openai.com",
			model:    "gpt-4",
			messages: []Message{},
			wantErr:  ErrEmptyMessages,
		},
		{
			name:        "server error",
			endpoint:    "will-be-set",
			model:       "gpt-4",
			messages:    []Message{{Role: "user", Content: "test"}},
			serverError: true,
			statusCode:  http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			endpoint := tt.endpoint
			if tt.serverError {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(tt.statusCode)
					_ = json.NewEncoder(w).Encode(map[string]interface{}{
						"error": map[string]interface{}{
							"code":    401,
							"message": "Invalid API key",
						},
					})
				}))
				defer server.Close()
				endpoint = server.URL
			}

			client := NewClient(endpoint, "test-key", tt.model)
			_, err := client.Complete(context.Background(), tt.messages)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("error = %v, want %v", err, tt.wantErr)
				}
			} else if err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}

func TestClient_Complete_NoAPIKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("should not have authorization header when no API key")
		}
		resp := ChatResponse{
			ID:     "test",
			Model:  "gpt-4",
			Object: "chat.completion",
			Choices: []struct {
				Index        int     `json:"index"`
				Message      Message `json:"message"`
				FinishReason string  `json:"finish_reason"`
			}{{Index: 0, Message: Message{Role: "assistant", Content: "ok"}}},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewClient(server.URL, "", "gpt-4")
	_, err := client.Complete(context.Background(), []Message{{Role: "user", Content: "test"}})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestClient_Complete_CustomAuthHeader covers the WithAuthHeader option
// used by the Vertex AI sub-provider. Vertex's OpenAI-compat endpoint
// accepts the API key only via `X-Goog-Api-Key: <raw-key>` (no Bearer
// prefix). A regression here would silently route Vertex requests as
// `Authorization: Bearer <key>`, which Vertex rejects as expired OAuth.
func TestClient_Complete_CustomAuthHeader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization header should not be set for custom-auth client, got %q", got)
		}
		if got := r.Header.Get("X-Goog-Api-Key"); got != "vertex-key" {
			t.Errorf("X-Goog-Api-Key = %q, want %q", got, "vertex-key")
		}
		resp := ChatResponse{
			ID:    "test",
			Model: "google/gemini-2.5-pro",
			Choices: []struct {
				Index        int     `json:"index"`
				Message      Message `json:"message"`
				FinishReason string  `json:"finish_reason"`
			}{{Index: 0, Message: Message{Role: "assistant", Content: "ok"}}},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewClient(server.URL, "vertex-key", "google/gemini-2.5-pro",
		WithAuthHeader("X-Goog-Api-Key", ""),
	)
	if _, err := client.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestClient_Complete_CustomAuthHeader_EmptyKey ensures that even when a
// custom auth header is configured, an empty api_key suppresses the header
// entirely — same contract as the default Authorization path. Protects
// self-hosted/proxy deployments that hit Vertex-compat endpoints without
// authentication.
func TestClient_Complete_CustomAuthHeader_EmptyKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Goog-Api-Key"); got != "" {
			t.Errorf("X-Goog-Api-Key should be unset when api_key is empty, got %q", got)
		}
		resp := ChatResponse{
			ID: "test", Model: "m",
			Choices: []struct {
				Index        int     `json:"index"`
				Message      Message `json:"message"`
				FinishReason string  `json:"finish_reason"`
			}{{Index: 0, Message: Message{Role: "assistant", Content: "ok"}}},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewClient(server.URL, "", "m", WithAuthHeader("X-Goog-Api-Key", ""))
	if _, err := client.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestClient_Complete_ContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		resp := ChatResponse{ID: "test"}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewClient(server.URL, "key", "gpt-4")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	_, err := client.Complete(ctx, []Message{{Role: "user", Content: "test"}})
	if err == nil {
		t.Error("expected error from context cancellation")
	}
}

func TestError_Error(t *testing.T) {
	err := &Error{Code: 401, Message: "Unauthorized"}
	want := "chat error 401: Unauthorized"
	if err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
}

func TestGatewayError_Retryable(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   bool
	}{
		{"500 internal error", 500, true},
		{"502 bad gateway", 502, true},
		{"503 unavailable", 503, true},
		{"504 timeout", 504, true},
		{"429 rate limit", 429, true},
		{"400 bad request", 400, false},
		{"401 unauthorized", 401, false},
		{"403 forbidden", 403, false},
		{"404 not found", 404, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := &GatewayError{Status: tt.status}
			if got := g.Retryable(); got != tt.want {
				t.Errorf("Retryable() for status %d = %v, want %v", tt.status, got, tt.want)
			}
		})
	}
}

func TestPrepareRequestForProvider_AddsVertexSkipThoughtSignature(t *testing.T) {
	c := NewClient("https://example.test", "vertex-key", "google/gemini-3-pro",
		WithAuthHeader("X-Goog-Api-Key", ""))
	req := ChatRequest{Messages: []Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", ToolCalls: []ToolCall{{
			ID:       "call_1",
			Type:     "function",
			Function: FunctionCall{Name: "create_task", Arguments: "{}"},
		}}},
	}}

	c.prepareRequestForProvider(&req)

	got := req.Messages[1].ToolCalls[0].ExtraContent
	if string(got) != `{"google":{"thought_signature":"skip_thought_signature_validator"}}` {
		t.Fatalf("extra_content = %s", got)
	}
}

func TestPrepareRequestForProvider_PreservesExistingVertexThoughtSignature(t *testing.T) {
	c := NewClient("https://example.test", "vertex-key", "google/gemini-3-pro",
		WithAuthHeader("X-Goog-Api-Key", ""))
	req := ChatRequest{Messages: []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{
			ID:           "call_1",
			Type:         "function",
			Function:     FunctionCall{Name: "create_task", Arguments: "{}"},
			ExtraContent: json.RawMessage(`{"google":{"thought_signature":"real"}}`),
		}}},
	}}

	c.prepareRequestForProvider(&req)

	got := req.Messages[0].ToolCalls[0].ExtraContent
	if string(got) != `{"google":{"thought_signature":"real"}}` {
		t.Fatalf("extra_content = %s", got)
	}
}

// TestDoComplete_Returns5xxAsGatewayError verifies the chat client surfaces
// HTTP 500-class responses as *GatewayError so callers can detect them and
// retry. This is the failure mode where bedrock-access-gateway returns the
// "process_single_item_agent timed out" error body.
func TestDoComplete_Returns5xxAsGatewayError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":500,"message":"[ERROR: Agent failed]"}}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", "m")
	_, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var g *GatewayError
	if !errors.As(err, &g) {
		t.Fatalf("expected *GatewayError, got %T: %v", err, err)
	}
	if g.Status != 500 {
		t.Errorf("Status = %d, want 500", g.Status)
	}
	if !g.Retryable() {
		t.Errorf("Retryable() = false; 500 should be retryable")
	}
	if g.Message != "[ERROR: Agent failed]" {
		t.Errorf("Message = %q, want parsed error body", g.Message)
	}
}

// TestDoComplete_Returns4xxAsNonRetryable verifies that client-error responses
// still produce a GatewayError but are flagged non-retryable — retrying a 401
// after pruning would just fail again.
func TestDoComplete_Returns4xxAsNonRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":401,"message":"bad key"}}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "bad", "m")
	_, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	var g *GatewayError
	if !errors.As(err, &g) {
		t.Fatalf("expected *GatewayError, got %T", err)
	}
	if g.Retryable() {
		t.Errorf("401 must not be retryable")
	}
}
