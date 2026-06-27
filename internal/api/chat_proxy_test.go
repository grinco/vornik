package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// stubProvider records calls and returns a canned response or error.
// Satisfies chat.Provider just enough for the proxy handler's contract.
type stubProvider struct {
	lastMessages []chat.Message
	lastTools    []chat.Tool
	resp         *chat.ChatResponse
	err          error
}

func (s *stubProvider) Complete(_ context.Context, _ []chat.Message) (*chat.ChatResponse, error) {
	return s.resp, s.err
}

func (s *stubProvider) CompleteWithTools(_ context.Context, messages []chat.Message, tools []chat.Tool) (*chat.ChatResponse, error) {
	s.lastMessages = messages
	s.lastTools = tools
	return s.resp, s.err
}

func (s *stubProvider) CompleteWithToolsStream(_ context.Context, _ []chat.Message, _ []chat.Tool, _ chat.StreamCallback) (*chat.ChatResponse, error) {
	return s.resp, s.err
}

func (s *stubProvider) Model() string              { return "stub-model" }
func (s *stubProvider) SetMetrics(_ *chat.Metrics) {}

// overridableStubProvider adds per-request model override support so
// we can test the proxy's type-assertion path without pulling in the
// real CLIClient (which would require a claude binary).
type overridableStubProvider struct {
	stubProvider
	model string
}

func (o *overridableStubProvider) Model() string { return o.model }
func (o *overridableStubProvider) WithModel(m string) chat.Provider {
	clone := *o
	clone.model = m
	return &clone
}

func TestChatCompletions_MethodNotAllowed(t *testing.T) {
	s := &Server{logger: zerolog.Nop()}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/chat/completions", nil)
	w := httptest.NewRecorder()
	s.ChatCompletions(w, req)
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

func TestChatCompletions_NoProviderReturns503(t *testing.T) {
	s := &Server{logger: zerolog.Nop()}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	s.ChatCompletions(w, req)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "CHAT_NOT_CONFIGURED")
}

func TestChatCompletions_InvalidJSON(t *testing.T) {
	s := &Server{logger: zerolog.Nop(), chatProvider: &stubProvider{}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions",
		strings.NewReader("{not json"))
	w := httptest.NewRecorder()
	s.ChatCompletions(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "INVALID_JSON")
}

func TestChatCompletions_EmptyMessages(t *testing.T) {
	s := &Server{logger: zerolog.Nop(), chatProvider: &stubProvider{}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[]}`))
	w := httptest.NewRecorder()
	s.ChatCompletions(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "EMPTY_MESSAGES")
}

func TestChatCompletions_ForwardsToProvider(t *testing.T) {
	stub := &stubProvider{
		resp: &chat.ChatResponse{
			ID:    "resp-1",
			Model: "claude-sonnet-4-6",
			Choices: []struct {
				Index        int          `json:"index"`
				Message      chat.Message `json:"message"`
				FinishReason string       `json:"finish_reason"`
			}{
				{
					Index:        0,
					Message:      chat.Message{Role: "assistant", Content: "hello back"},
					FinishReason: "stop",
				},
			},
		},
	}
	s := &Server{logger: zerolog.Nop(), chatProvider: stub}

	body := `{
		"model": "ignored-by-proxy",
		"messages": [
			{"role": "system", "content": "be concise"},
			{"role": "user", "content": "hi"}
		],
		"tools": [
			{"type":"function","function":{"name":"ping","description":"p","parameters":{}}}
		]
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.ChatCompletions(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	// Messages + tools delivered to the provider untouched.
	require.Len(t, stub.lastMessages, 2)
	assert.Equal(t, "system", stub.lastMessages[0].Role)
	assert.Equal(t, "user", stub.lastMessages[1].Role)
	require.Len(t, stub.lastTools, 1)
	assert.Equal(t, "ping", stub.lastTools[0].Function.Name)

	// Response round-trips as OpenAI-compatible JSON.
	var resp chat.ChatResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.Equal(t, "claude-sonnet-4-6", resp.Model)
	require.Len(t, resp.Choices, 1)
	assert.Equal(t, "hello back", resp.Choices[0].Message.Content)
}

func TestChatCompletions_FillsInModelFromProviderWhenBlank(t *testing.T) {
	stub := &stubProvider{
		resp: &chat.ChatResponse{
			Choices: []struct {
				Index        int          `json:"index"`
				Message      chat.Message `json:"message"`
				FinishReason string       `json:"finish_reason"`
			}{
				{Message: chat.Message{Role: "assistant", Content: "ok"}, FinishReason: "stop"},
			},
			// Model deliberately empty — some providers don't populate it.
		},
	}
	s := &Server{logger: zerolog.Nop(), chatProvider: stub}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"x"}]}`))
	w := httptest.NewRecorder()
	s.ChatCompletions(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var resp chat.ChatResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.Equal(t, "stub-model", resp.Model, "proxy should fill Model from Provider.Model() when blank")
}

func TestChatCompletions_ProviderError502(t *testing.T) {
	stub := &stubProvider{err: assertError("upstream went pop")}
	s := &Server{logger: zerolog.Nop(), chatProvider: stub}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"x"}]}`))
	w := httptest.NewRecorder()
	s.ChatCompletions(w, req)
	assert.Equal(t, http.StatusBadGateway, w.Code)
	assert.Contains(t, w.Body.String(), "PROVIDER_ERROR")
	// Sanitized envelope (2026-05-29 fix): provider error
	// strings stay in the logger; the wire response only
	// gets a stable message so upstream secrets/internal
	// routing info don't leak to external callers.
	assert.NotContains(t, w.Body.String(), "upstream went pop",
		"provider err.Error() must not be forwarded verbatim")
	assert.Contains(t, w.Body.String(), "upstream provider returned an error")
}

// TestChatCompletions_RouteOverflow503 — the per-route bounded
// queue (hardening sub-item 4) surfaces as HTTP 503 with code
// ROUTE_OVERFLOW so HA clients distinguish "daemon refused to
// queue further" (503) from "upstream rate-limited us" (429)
// and "upstream errored" (502).
func TestChatCompletions_RouteOverflow503(t *testing.T) {
	stub := &stubProvider{err: &chat.RouteOverflowError{Route: "bedrock", Depth: 8}}
	s := &Server{logger: zerolog.Nop(), chatProvider: stub}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"x"}]}`))
	w := httptest.NewRecorder()
	s.ChatCompletions(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "ROUTE_OVERFLOW")
	assert.Contains(t, w.Body.String(), "bedrock")
}

func TestChatCompletions_NilProviderResponse502(t *testing.T) {
	stub := &stubProvider{}
	s := &Server{logger: zerolog.Nop(), chatProvider: stub}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"x"}]}`))
	w := httptest.NewRecorder()
	s.ChatCompletions(w, req)

	assert.Equal(t, http.StatusBadGateway, w.Code)
	assert.Contains(t, w.Body.String(), "PROVIDER_ERROR")
	assert.Contains(t, w.Body.String(), "provider returned nil response")
}

func TestChatCompletions_PerRequestModelOverride(t *testing.T) {
	// A provider whose default model is "stub-default" — the request
	// asks for "claude-opus-4-6", which should be honored.
	stub := &overridableStubProvider{
		stubProvider: stubProvider{
			resp: &chat.ChatResponse{
				Choices: []struct {
					Index        int          `json:"index"`
					Message      chat.Message `json:"message"`
					FinishReason string       `json:"finish_reason"`
				}{
					{Message: chat.Message{Role: "assistant", Content: "ok"}, FinishReason: "stop"},
				},
				// Deliberately leave Model blank; proxy should fill it
				// from the OVERRIDDEN provider's Model(), not the
				// original's.
			},
		},
		model: "stub-default",
	}
	s := &Server{logger: zerolog.Nop(), chatProvider: stub}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions",
		strings.NewReader(`{"model":"claude-opus-4-6","messages":[{"role":"user","content":"x"}]}`))
	w := httptest.NewRecorder()
	s.ChatCompletions(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	// The proxy fills Model from the cloned provider's Model(), which
	// is the requested override — NOT the configured default.
	var resp chat.ChatResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.Equal(t, "claude-opus-4-6", resp.Model,
		"override should propagate through to the response Model field")

	// Default provider is untouched: the clone is a fresh struct, the
	// request's model didn't bleed back into `stub`.
	assert.Equal(t, "stub-default", stub.model,
		"override must not mutate the dispatcher's provider")
}

func TestChatCompletions_NonOverridableProviderIgnoresRequestModel(t *testing.T) {
	// Plain stubProvider doesn't implement ModelOverridable — the
	// proxy should fall through to the default Model() for the
	// response fill-in.
	stub := &stubProvider{
		resp: &chat.ChatResponse{
			Choices: []struct {
				Index        int          `json:"index"`
				Message      chat.Message `json:"message"`
				FinishReason string       `json:"finish_reason"`
			}{
				{Message: chat.Message{Role: "assistant", Content: "ok"}, FinishReason: "stop"},
			},
		},
	}
	s := &Server{logger: zerolog.Nop(), chatProvider: stub}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions",
		strings.NewReader(`{"model":"whatever","messages":[{"role":"user","content":"x"}]}`))
	w := httptest.NewRecorder()
	s.ChatCompletions(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var resp chat.ChatResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.Equal(t, "stub-model", resp.Model,
		"non-overridable provider's default model should win")
}

type blockingProxyProvider struct {
	firstStarted chan struct{}
	releaseFirst chan struct{}
}

func (b *blockingProxyProvider) Complete(ctx context.Context, messages []chat.Message) (*chat.ChatResponse, error) {
	return b.CompleteWithTools(ctx, messages, nil)
}

func (b *blockingProxyProvider) CompleteWithTools(ctx context.Context, messages []chat.Message, _ []chat.Tool) (*chat.ChatResponse, error) {
	if len(messages) > 0 && messages[0].Content == "first" {
		close(b.firstStarted)
		select {
		case <-b.releaseFirst:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &chat.ChatResponse{
		Choices: []struct {
			Index        int          `json:"index"`
			Message      chat.Message `json:"message"`
			FinishReason string       `json:"finish_reason"`
		}{
			{Message: chat.Message{Role: "assistant", Content: "ok"}, FinishReason: "stop"},
		},
	}, nil
}

func (b *blockingProxyProvider) CompleteWithToolsStream(ctx context.Context, messages []chat.Message, tools []chat.Tool, _ chat.StreamCallback) (*chat.ChatResponse, error) {
	return b.CompleteWithTools(ctx, messages, tools)
}

func (b *blockingProxyProvider) Model() string              { return "stub-model" }
func (b *blockingProxyProvider) SetMetrics(_ *chat.Metrics) {}

// TestChatCompletions_DoesNotMutateTaskStatusViaQueueHooks — the
// chat proxy used to flip task.status QUEUED↔RUNNING based on
// chat-queue position to surface "waiting for LLM" in the UI.
// That broke long LLM calls: a leased+RUNNING task got flipped
// to QUEUED while its lease was still held, then RenewLease's
// `status IN (LEASED,RUNNING,WAITING_FOR_CHILDREN)` guard
// rejected the renewal and the executor escalated to FAILED
// after 3 consecutive renewal misses (T-6f55, 2026-05-10).
// Removing the hook is the fix; this test guards against
// re-introducing it.
func TestChatCompletions_DoesNotMutateTaskStatusViaQueueHooks(t *testing.T) {
	provider := &blockingProxyProvider{
		firstStarted: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}

	var mu sync.Mutex
	var taskStatuses []persistence.TaskStatus
	var execStatuses []persistence.ExecutionStatus

	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			require.Equal(t, "task-2", id)
			return &persistence.Task{ID: id, ProjectID: "test-project"}, nil
		},
		UpdateStatusFunc: func(_ context.Context, id string, status persistence.TaskStatus) error {
			mu.Lock()
			taskStatuses = append(taskStatuses, status)
			mu.Unlock()
			return nil
		},
	}
	execRepo := &mocks.MockExecutionRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Execution, error) {
			require.Equal(t, "exec-2", id)
			return &persistence.Execution{ID: id, ProjectID: "test-project"}, nil
		},
		UpdateStatusFunc: func(_ context.Context, id string, status persistence.ExecutionStatus) error {
			mu.Lock()
			execStatuses = append(execStatuses, status)
			mu.Unlock()
			return nil
		},
	}
	s := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
		WithExecutionRepository(execRepo),
		WithChatProvider(chat.NewQueuedProvider(provider, 1)),
	)

	firstDone := make(chan struct{})
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions",
			strings.NewReader(`{"messages":[{"role":"user","content":"first"}]}`))
		w := httptest.NewRecorder()
		s.ChatCompletions(w, req)
		close(firstDone)
	}()

	select {
	case <-provider.firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first request did not start")
	}

	secondDone := make(chan struct{})
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions",
			strings.NewReader(`{"messages":[{"role":"user","content":"second"}]}`))
		req.Header.Set("X-Vornik-Task-ID", "task-2")
		req.Header.Set("X-Vornik-Execution-ID", "exec-2")
		w := httptest.NewRecorder()
		s.ChatCompletions(w, req)
		require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
		close(secondDone)
	}()

	// Give the second request a moment to enter the queue, then
	// release the first so both can complete.
	time.Sleep(50 * time.Millisecond)
	close(provider.releaseFirst)

	for _, done := range []chan struct{}{firstDone, secondDone} {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("request did not finish")
		}
	}

	mu.Lock()
	defer mu.Unlock()
	assert.Empty(t, taskStatuses, "chat proxy must NOT mutate task.status — that's the lease-loss bug")
	assert.Empty(t, execStatuses, "chat proxy must NOT mutate execution.status either")
}

func TestChatCompletions_BodyTooLarge(t *testing.T) {
	// Craft a body one byte over the cap. Use bytes.NewReader so
	// ContentLength is known — LimitReader still wins regardless.
	over := make([]byte, maxChatProxyBodyBytes+2)
	for i := range over {
		over[i] = 'x'
	}
	s := &Server{logger: zerolog.Nop(), chatProvider: &stubProvider{}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", bytes.NewReader(over))
	w := httptest.NewRecorder()
	s.ChatCompletions(w, req)
	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	assert.Contains(t, w.Body.String(), "BODY_TOO_LARGE")
}

// TestChatCompletions_RejectsCrossProjectTaskID guards the IDOR fix
// in chat_proxy.go: an agent running under project-A's scoped key
// must NOT be able to flip a task that belongs to project-B by
// passing the cross-project taskID in X-Vornik-Task-ID. The header is
// validated against the caller's allowed-projects set before any
// queue-hook is registered.
func TestChatCompletions_RejectsCrossProjectTaskID(t *testing.T) {
	var updateCalled bool
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, ProjectID: "project-b"}, nil
		},
		UpdateStatusFunc: func(_ context.Context, _ string, _ persistence.TaskStatus) error {
			updateCalled = true
			return nil
		},
	}
	s := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
		WithChatProvider(&stubProvider{resp: &chat.ChatResponse{Model: "stub"}}),
	)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Vornik-Task-ID", "task-from-other-project")
	// Caller is scoped to project-a only.
	req = req.WithContext(context.WithValue(req.Context(), projectIDKey, []string{"project-a"}))

	w := httptest.NewRecorder()
	s.ChatCompletions(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())
	assert.False(t, updateCalled, "task UpdateStatus must not run on cross-project request")
}

// TestChatCompletions_RejectsCrossProjectExecutionID — same guard for
// X-Vornik-Execution-ID. Without the fix the OnQueued / OnStart hooks
// would call executionRepo.UpdateStatus on an execution the caller's
// key has no rights to.
func TestChatCompletions_RejectsCrossProjectExecutionID(t *testing.T) {
	var updateCalled bool
	execRepo := &mocks.MockExecutionRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Execution, error) {
			return &persistence.Execution{ID: id, ProjectID: "project-b"}, nil
		},
		UpdateStatusFunc: func(_ context.Context, _ string, _ persistence.ExecutionStatus) error {
			updateCalled = true
			return nil
		},
	}
	s := NewServer(
		WithLogger(zerolog.Nop()),
		WithExecutionRepository(execRepo),
		WithChatProvider(&stubProvider{resp: &chat.ChatResponse{Model: "stub"}}),
	)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Vornik-Execution-ID", "exec-from-other-project")
	req = req.WithContext(context.WithValue(req.Context(), projectIDKey, []string{"project-a"}))

	w := httptest.NewRecorder()
	s.ChatCompletions(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())
	assert.False(t, updateCalled, "execution UpdateStatus must not run on cross-project request")
}

// assertError returns an error with the given message — used in tests
// where we want a predictable Error() string without importing errors.
type assertError string

func (e assertError) Error() string { return string(e) }

// TestRecordChatAPIUsage_PropagatesCacheTokens — the chat-proxy external_api
// path must persist the cache_creation + cache_read fields the provider
// surfaces, otherwise the spend dashboard's cache hit ratio + savings tile
// stay at zero for HA / external traffic even when Bedrock returned them.
func TestRecordChatAPIUsage_PropagatesCacheTokens(t *testing.T) {
	repo := &capturingLLMUsageRepo{}
	s := NewServer(
		WithLogger(zerolog.Nop()),
		WithLLMUsageRepository(repo),
	)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions",
		strings.NewReader(`{}`))
	req.Header.Set("X-Vornik-Project-ID", "proj-cache")

	resp := &chat.ChatResponse{Model: "claude-sonnet-4-6"}
	resp.Usage.PromptTokens = 100
	resp.Usage.CompletionTokens = 200
	resp.Usage.TotalTokens = 300
	resp.Usage.CacheCreationTokens = 50
	resp.Usage.CacheReadTokens = 400

	s.recordChatAPIUsage(req.Context(), req, "claude-sonnet-4-6", resp)

	row := repo.lastRecorded()
	require.NotNil(t, row, "recordChatAPIUsage must persist a row when llmUsageRepo is set")
	assert.Equal(t, int64(100), row.PromptTokens)
	assert.Equal(t, int64(200), row.CompletionTokens)
	assert.Equal(t, int64(50), row.CacheCreationTokens, "cache_creation_tokens must propagate")
	assert.Equal(t, int64(400), row.CacheReadTokens, "cache_read_tokens must propagate")
	assert.Equal(t, persistence.TaskLLMUsageSourceExternalAPI, row.Source)
}

// TestRecordChatAPIUsage_NoCacheFieldsSafeWithZeroes — a provider that
// doesn't populate cache fields (Vertex, Ollama, generic OpenAI) must
// still produce a row, and cache fields must be zero — no spurious values
// from the request path.
func TestRecordChatAPIUsage_NoCacheFieldsSafeWithZeroes(t *testing.T) {
	repo := &capturingLLMUsageRepo{}
	s := NewServer(
		WithLogger(zerolog.Nop()),
		WithLLMUsageRepository(repo),
	)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions",
		strings.NewReader(`{}`))
	req.Header.Set("X-Vornik-Project-ID", "proj-nocache")

	resp := &chat.ChatResponse{Model: "ollama-local"}
	resp.Usage.PromptTokens = 100
	resp.Usage.CompletionTokens = 100
	resp.Usage.TotalTokens = 200

	s.recordChatAPIUsage(req.Context(), req, "ollama-local", resp)

	row := repo.lastRecorded()
	require.NotNil(t, row)
	assert.Equal(t, int64(0), row.CacheCreationTokens)
	assert.Equal(t, int64(0), row.CacheReadTokens)
}
