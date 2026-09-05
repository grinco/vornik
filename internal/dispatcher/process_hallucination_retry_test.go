package dispatcher

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/hallucination"
)

// A reply that names a task id in the executor's format which no tool output
// grounded: taskIDNotFoundRule emits SeverityHigh, which is the blocking tier
// the dispatcher's continuation logic acts on.
const ungroundedTaskReply = "I scheduled it as task_20260905000000_deadbeefdeadbeef, check back later."

// hallucinationLoopServer scripts the model: every reply is one of the given
// texts in order, and the request bodies are captured so the test can read
// what the loop sent back after a hallucination.
func hallucinationLoopServer(t *testing.T, replies ...string) (*httptest.Server, func() []chat.ChatRequest) {
	t.Helper()
	var mu sync.Mutex
	var reqs []chat.ChatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chat.ChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		reqs = append(reqs, req)
		n := len(reqs)
		mu.Unlock()
		if n > len(replies) {
			t.Errorf("model called %d times, only %d replies scripted", n, len(replies))
			http.Error(w, "unscripted call", http.StatusInternalServerError)
			return
		}
		body, _ := json.Marshal(map[string]any{
			"id": "r", "object": "chat.completion", "model": "m",
			"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": replies[n-1]}, "finish_reason": "stop"}},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []chat.ChatRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]chat.ChatRequest(nil), reqs...)
	}
}

// TestProcess_HallucinationRetriesOnceThenBanners pins the dispatcher's
// continuation behaviour at the LOOP level, which no test did before
// 2026-09-05 (only the formatters were tested). Written against the current
// agent_process.go before the pipeline-points conversion, as that design's
// task order requires (2026-09-04-pipeline-points-design.md §3.3, §8 step 0),
// so a conversion that moves the retry or the banner is a failure here.
//
// First blocking reply → one synthetic user turn carrying the retry prompt
// and a second model call. Second blocking reply → the banner-prefixed text
// and no third call.
func TestProcess_HallucinationRetriesOnceThenBanners(t *testing.T) {
	srv, requests := hallucinationLoopServer(t, ungroundedTaskReply, ungroundedTaskReply)
	agent := NewAgent(chat.NewClient(srv.URL, "k", "m"), nil, nil, nil, nil,
		WithHallucinationDetector(hallucination.NewDefault()))

	result := agent.Process(context.Background(), Request{
		Messages: []chat.Message{{Role: "user", Content: "schedule the report"}},
		Project:  "p1",
	})
	require.NoError(t, result.Err)

	reqs := requests()
	require.Len(t, reqs, 2, "exactly one retry: two model calls, never a third")
	last := reqs[1].Messages[len(reqs[1].Messages)-1]
	assert.Equal(t, "user", last.Role, "the retry is a synthetic user turn")
	assert.True(t, strings.HasPrefix(last.Content, "Your previous reply contained the following unsupported claim(s)"), "retry prompt: %q", last.Content)
	assert.Contains(t, last.Content, "task_20260905000000_deadbeefdeadbeef")

	assert.True(t, strings.HasPrefix(result.Text, "[hallucination warning]"), "second blocking reply is bannered, not retried: %q", result.Text)
	assert.True(t, strings.HasSuffix(result.Text, ungroundedTaskReply), "the banner is PREPENDED to the model's text: %q", result.Text)
}

// TestProcess_HallucinationRetryThatSucceedsIsClean — the retry turn's reply
// is grounded, so the operator sees it as-is: no banner, two calls.
func TestProcess_HallucinationRetryThatSucceedsIsClean(t *testing.T) {
	srv, requests := hallucinationLoopServer(t, ungroundedTaskReply, "I could not find that task; nothing was scheduled.")
	agent := NewAgent(chat.NewClient(srv.URL, "k", "m"), nil, nil, nil, nil,
		WithHallucinationDetector(hallucination.NewDefault()))

	result := agent.Process(context.Background(), Request{
		Messages: []chat.Message{{Role: "user", Content: "schedule the report"}},
		Project:  "p1",
	})
	require.NoError(t, result.Err)
	assert.Len(t, requests(), 2)
	assert.Equal(t, "I could not find that task; nothing was scheduled.", result.Text)
}

// TestProcess_GroundedReplyIsNotRetried — a task id the tool output did
// produce is not a hallucination: one call, text unchanged.
func TestProcess_GroundedReplyIsNotRetried(t *testing.T) {
	srv, requests := hallucinationLoopServer(t, ungroundedTaskReply)
	agent := NewAgent(chat.NewClient(srv.URL, "k", "m"), nil, nil, nil, nil,
		WithHallucinationDetector(hallucination.NewDefault()))

	result := agent.Process(context.Background(), Request{
		Messages: []chat.Message{
			{Role: "user", Content: "schedule the report"},
			{Role: "assistant", ToolCalls: []chat.ToolCall{{ID: "tc1", Type: "function", Function: chat.FunctionCall{Name: "create_task", Arguments: `{}`}}}},
			{Role: "tool", ToolCallID: "tc1", Name: "create_task", Content: "created task_20260905000000_deadbeefdeadbeef"},
		},
		Project: "p1",
	})
	require.NoError(t, result.Err)
	assert.Len(t, requests(), 1)
	assert.Equal(t, ungroundedTaskReply, result.Text)
}
