package dispatcher

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/chat"
)

// TestAgent_Process_DirectAnswerNoTools exercises the simplest happy
// path: the LLM returns plain text on the first turn → tool loop
// terminates immediately and Result.Text is the assistant reply.
func TestAgent_Process_DirectAnswerNoTools(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"r1","object":"chat.completion","model":"m",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hello there"},"finish_reason":"stop"}]
		}`))
	}))
	defer srv.Close()

	client := chat.NewClient(srv.URL, "k", "m")
	agent := NewAgent(client, nil, nil, nil, nil)

	result := agent.Process(context.Background(), Request{
		Messages: []chat.Message{{Role: "user", Content: "hi"}},
		Project:  "p1",
	})

	assert.NoError(t, result.Err)
	assert.Equal(t, "hello there", result.Text)
}

// TestAgent_Process_EmptyResponseTriggersDefaultText — if the model
// returns an empty assistant reply (small models like glm-flash do
// this on dropped-trailing-content), the dispatcher substitutes the
// "Done." default so the user sees something instead of a blank
// message.
func TestAgent_Process_EmptyResponseTriggersDefaultText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"r1","object":"chat.completion","model":"m",
			"choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}]
		}`))
	}))
	defer srv.Close()

	client := chat.NewClient(srv.URL, "k", "m")
	agent := NewAgent(client, nil, nil, nil, nil)

	result := agent.Process(context.Background(), Request{
		Messages: []chat.Message{{Role: "user", Content: "hi"}},
		Project:  "p1",
	})
	assert.NoError(t, result.Err)
	assert.Equal(t, "Done.", result.Text)
}

// TestAgent_Process_ExecutesToolThenAnswers covers the tool-call
// branch: turn 1 emits a tool call → dispatcher routes it to the
// tool executor → turn 2 returns plain text using the tool result.
func TestAgent_Process_ExecutesToolThenAnswers(t *testing.T) {
	var n int64
	var capturedSecondReq chat.ChatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := atomic.AddInt64(&n, 1)
		w.Header().Set("Content-Type", "application/json")
		switch c {
		case 1:
			// First call: emit a list_projects tool call.
			_, _ = w.Write([]byte(`{
				"id":"r1","object":"chat.completion","model":"m",
				"choices":[{
					"index":0,
					"message":{"role":"assistant","content":null,"tool_calls":[
						{"id":"tc1","type":"function","function":{"name":"list_projects","arguments":"{}"}}
					]},
					"finish_reason":"tool_calls"
				}]
			}`))
		default:
			// Second call: respond with text. Capture the second
			// request body so the test can assert the tool
			// message landed.
			_ = json.NewDecoder(r.Body).Decode(&capturedSecondReq)
			_, _ = w.Write([]byte(`{
				"id":"r2","object":"chat.completion","model":"m",
				"choices":[{"index":0,"message":{"role":"assistant","content":"final answer"},"finish_reason":"stop"}]
			}`))
		}
	}))
	defer srv.Close()

	client := chat.NewClient(srv.URL, "k", "m")
	agent := NewAgent(client, nil, nil, nil, nil)

	result := agent.Process(context.Background(), Request{
		Messages:        []chat.Message{{Role: "user", Content: "list my projects"}},
		Project:         "p1",
		AllowedProjects: []string{"*"},
	})
	require.NoError(t, result.Err)
	assert.Equal(t, "final answer", result.Text)
	// Two upstream calls happened.
	assert.Equal(t, int64(2), atomic.LoadInt64(&n))

	// The second request to the LLM carried the tool message — that's
	// the contract Process must uphold for the model to know the
	// tool's result. (We don't assert content; just that a tool role
	// appears among the messages.)
	haveTool := false
	for _, m := range capturedSecondReq.Messages {
		if m.Role == "tool" {
			haveTool = true
			break
		}
	}
	assert.True(t, haveTool, "second LLM request must include a tool-role message")
}

type streamingToolTurnProvider struct {
	calls int
}

func (p *streamingToolTurnProvider) Complete(context.Context, []chat.Message) (*chat.ChatResponse, error) {
	return nil, nil
}

func (p *streamingToolTurnProvider) CompleteWithTools(context.Context, []chat.Message, []chat.Tool) (*chat.ChatResponse, error) {
	return nil, nil
}

func (p *streamingToolTurnProvider) CompleteWithToolsStream(_ context.Context, _ []chat.Message, _ []chat.Tool, onText chat.StreamCallback) (*chat.ChatResponse, error) {
	p.calls++
	resp := &chat.ChatResponse{}
	switch p.calls {
	case 1:
		resp.Choices = append(resp.Choices, struct {
			Index        int          `json:"index"`
			Message      chat.Message `json:"message"`
			FinishReason string       `json:"finish_reason"`
		}{
			Message: chat.Message{
				Role: "assistant",
				ToolCalls: []chat.ToolCall{{
					ID:   "tc1",
					Type: "function",
					Function: chat.FunctionCall{
						Name:      "list_projects",
						Arguments: "{}",
					},
				}},
			},
			FinishReason: "tool_calls",
		})
	default:
		if onText != nil {
			onText("Ta")
			onText("Task created.")
		}
		resp.Choices = append(resp.Choices, struct {
			Index        int          `json:"index"`
			Message      chat.Message `json:"message"`
			FinishReason string       `json:"finish_reason"`
		}{
			Message:      chat.Message{Role: "assistant", Content: "Task created."},
			FinishReason: "stop",
		})
	}
	return resp, nil
}

func (p *streamingToolTurnProvider) Model() string { return "stub-stream" }

func (p *streamingToolTurnProvider) SetMetrics(_ *chat.Metrics) {}

var _ chat.Provider = (*streamingToolTurnProvider)(nil)

func TestAgent_ProcessStreaming_ReemitsWholeTurnAccumulatedAfterTool(t *testing.T) {
	provider := &streamingToolTurnProvider{}
	agent := NewAgent(provider, nil, nil, nil, nil, WithMaxIterations(3))

	var got []string
	result := agent.ProcessStreaming(context.Background(), Request{
		Messages:        []chat.Message{{Role: "user", Content: "create a task"}},
		Project:         "p1",
		AllowedProjects: []string{"*"},
	}, func(accumulated string) {
		got = append(got, accumulated)
	})

	require.NoError(t, result.Err)
	require.Equal(t, "Task created.", result.Text)
	require.Len(t, got, 3)
	require.Contains(t, got[0], "[⏳ list projects]")
	require.True(t, strings.HasSuffix(got[1], "Ta"), "second callback should preserve first letters after tool status, got %q", got[1])
	require.True(t, strings.HasSuffix(got[2], "Task created."), "final callback = %q", got[2])
	require.Contains(t, got[2], "[⏳ list projects]")
}

// TestAgent_Process_IterationCapStopsRunaway covers the maxIterations
// guard: if the model keeps emitting tool calls without ever
// answering, the dispatcher exits after maxIterations with a
// user-facing cap message.
func TestAgent_Process_IterationCapStopsRunaway(t *testing.T) {
	var n int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&n, 1)
		w.Header().Set("Content-Type", "application/json")
		// Always emit a tool call → runaway loop.
		_, _ = w.Write([]byte(`{
			"id":"r","object":"chat.completion","model":"m",
			"choices":[{
				"index":0,
				"message":{"role":"assistant","content":null,"tool_calls":[
					{"id":"tc","type":"function","function":{"name":"list_projects","arguments":"{}"}}
				]},
				"finish_reason":"tool_calls"
			}]
		}`))
	}))
	defer srv.Close()

	client := chat.NewClient(srv.URL, "k", "m")
	agent := NewAgent(client, nil, nil, nil, nil, WithMaxIterations(3))

	result := agent.Process(context.Background(), Request{
		Messages:        []chat.Message{{Role: "user", Content: "loop forever"}},
		Project:         "p1",
		AllowedProjects: []string{"*"},
	})
	assert.NoError(t, result.Err)
	assert.Contains(t, strings.ToLower(result.Text), "tool step limit")
	// Exactly maxIterations LLM calls; the 3rd loop iteration's
	// response gets read but we don't make a 4th call.
	assert.Equal(t, int64(3), atomic.LoadInt64(&n))
}

// TestAgent_Process_EmptyChoicesIsError pins the malformed-response
// branch: zero choices in the LLM payload surfaces as Result.Err.
func TestAgent_Process_EmptyChoicesIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"r","object":"chat.completion","model":"m","choices":[]}`))
	}))
	defer srv.Close()

	client := chat.NewClient(srv.URL, "k", "m")
	agent := NewAgent(client, nil, nil, nil, nil)
	result := agent.Process(context.Background(), Request{
		Messages: []chat.Message{{Role: "user", Content: "hi"}},
		Project:  "p1",
	})
	assert.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), "empty response")
}
