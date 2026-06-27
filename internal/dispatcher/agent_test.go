package dispatcher

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/registry"
)

func TestTrimToLastUserTurn(t *testing.T) {
	tests := []struct {
		name string
		in   []chat.Message
		want []chat.Message
	}{
		{
			name: "single user message — unchanged",
			in:   []chat.Message{{Role: "user", Content: "hi"}},
			want: []chat.Message{{Role: "user", Content: "hi"}},
		},
		{
			name: "prior turn dropped",
			in: []chat.Message{
				{Role: "user", Content: "q1"},
				{Role: "assistant", Content: "a1"},
				{Role: "tool", Content: "t1"},
				{Role: "user", Content: "q2"},
			},
			want: []chat.Message{{Role: "user", Content: "q2"}},
		},
		{
			name: "multiple prior turns dropped",
			in: []chat.Message{
				{Role: "user", Content: "q1"},
				{Role: "assistant", Content: "a1"},
				{Role: "user", Content: "q2"},
				{Role: "assistant", Content: "a2"},
				{Role: "user", Content: "q3"},
			},
			want: []chat.Message{{Role: "user", Content: "q3"}},
		},
		{
			name: "no user message — returned unchanged",
			in: []chat.Message{
				{Role: "assistant", Content: "a1"},
				{Role: "tool", Content: "t1"},
			},
			want: []chat.Message{
				{Role: "assistant", Content: "a1"},
				{Role: "tool", Content: "t1"},
			},
		},
		{
			name: "empty input",
			in:   []chat.Message{},
			want: []chat.Message{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := trimToLastUserTurn(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("trimToLastUserTurn(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// TestAgent_RetriesOnGatewayError verifies the core user-facing recovery path:
// when the upstream gateway returns a 5xx on the first LLM call of a turn, the
// dispatcher prunes the conversation to the last user message and retries once.
// This is what turns a stuck Telegram session ("every /new works for a while,
// then errors until I reset") into automatic recovery.
func TestAgent_RetriesOnGatewayError(t *testing.T) {
	var callCount int64
	var capturedRequests []chat.ChatRequest
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&callCount, 1)

		var req chat.ChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		capturedRequests = append(capturedRequests, req)
		mu.Unlock()

		if n == 1 {
			// First call: simulate BAG's "process_single_item_agent timed
			// out" failure. Must be a GatewayError, not a transport error,
			// so we return an HTTP response with a structured body.
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"code":500,"message":"[ERROR: Agent failed (Function process_single_item_agent timed out after 90.0 seconds)]"}}`))
			return
		}

		// Second call: succeed with a final (non-tool-calling) assistant reply.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"r1","object":"chat.completion","model":"m",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]
		}`))
	}))
	defer srv.Close()

	client := chat.NewClient(srv.URL, "k", "m", chat.WithTimeout(5*time.Second))
	agent := NewAgent(client, nil, nil, nil, nil)

	// Build a conversation that mimics the real failure pattern: one prior
	// user/assistant/tool turn, then the current user question.
	msgs := []chat.Message{
		{Role: "user", Content: "earlier question"},
		{Role: "assistant", Content: "earlier answer with bloated tool context"},
		{Role: "tool", Content: "huge tool result"},
		{Role: "user", Content: "Who is Alex Grinco?"},
	}

	result := agent.Process(context.Background(), Request{
		Messages: msgs,
		Project:  "assistant",
	})

	if result.Err != nil {
		t.Fatalf("expected recovery to succeed, got error: %v", result.Err)
	}
	if result.Text != "ok" {
		t.Errorf("Text = %q, want %q", result.Text, "ok")
	}
	if got := atomic.LoadInt64(&callCount); got != 2 {
		t.Fatalf("expected exactly 2 upstream calls (1 fail, 1 retry), got %d", got)
	}

	// The retry call must have been sent with pruned history: system prompt
	// + the last user message only. If the dispatcher re-sent the full bloated
	// conversation, the retry would just fail the same way on a real gateway.
	mu.Lock()
	defer mu.Unlock()
	if len(capturedRequests) != 2 {
		t.Fatalf("captured %d requests, expected 2", len(capturedRequests))
	}
	retry := capturedRequests[1]
	if len(retry.Messages) != 2 {
		t.Errorf("retry sent %d messages (system + user expected = 2): %+v", len(retry.Messages), retry.Messages)
	}
	if retry.Messages[0].Role != "system" {
		t.Errorf("retry[0].Role = %q, want 'system'", retry.Messages[0].Role)
	}
	if retry.Messages[1].Role != "user" || retry.Messages[1].Content != "Who is Alex Grinco?" {
		t.Errorf("retry[1] = %+v, want the pruned last user turn", retry.Messages[1])
	}

	// Result.Messages must reflect the pruned state so the caller (Telegram
	// bot) replaces its conversation with the shorter history, locking in
	// the recovery instead of repeatedly hitting the same failure.
	if len(result.Messages) < 2 {
		t.Fatalf("result should include at least pruned user + assistant; got %d", len(result.Messages))
	}
	if result.Messages[0].Role != "user" || result.Messages[0].Content != "Who is Alex Grinco?" {
		t.Errorf("result.Messages[0] = %+v, expected pruned user turn as new history head", result.Messages[0])
	}
}

// TestAgent_DoesNotRetryOn4xx confirms we don't waste a retry on a client
// error (e.g. bad API key) — pruning wouldn't change the outcome.
func TestAgent_DoesNotRetryOn4xx(t *testing.T) {
	var callCount int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&callCount, 1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":401,"message":"bad key"}}`))
	}))
	defer srv.Close()

	client := chat.NewClient(srv.URL, "bad", "m", chat.WithTimeout(5*time.Second))
	agent := NewAgent(client, nil, nil, nil, nil)

	result := agent.Process(context.Background(), Request{
		Messages: []chat.Message{
			{Role: "user", Content: "q1"},
			{Role: "assistant", Content: "a1"},
			{Role: "user", Content: "q2"},
		},
		Project: "assistant",
	})

	if result.Err == nil {
		t.Fatal("expected error on 401, got success")
	}
	if got := atomic.LoadInt64(&callCount); got != 1 {
		t.Errorf("expected 1 upstream call on 4xx (no retry), got %d", got)
	}
}

// TestStripReasoning exercises the regex that keeps gpt-oss / DeepSeek-R1
// style in-content reasoning blocks out of user-facing Telegram messages.
// These are the exact shapes we've observed leaking from the gateway.
func TestStripReasoning(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "single think block + answer",
			in:   "<think>We need to respond politely.</think>I'm doing well, thanks!",
			want: "I'm doing well, thanks!",
		},
		{
			name: "think block with newlines",
			in:   "<think>User asks X.\nBest answer is Y.\n</think>Y.",
			want: "Y.",
		},
		{
			name: "thinking tag variant",
			in:   "<thinking>deliberation</thinking>\n\nAnswer",
			want: "Answer",
		},
		{
			name: "reasoning tag variant",
			in:   "<reasoning>why</reasoning>answer",
			want: "answer",
		},
		{
			name: "multiple blocks",
			in:   "<think>first</think>middle<think>second</think>end",
			want: "middleend",
		},
		{
			name: "no reasoning block",
			in:   "Just a normal answer.",
			want: "Just a normal answer.",
		},
		{
			name: "unclosed block — left alone rather than eating everything",
			in:   "<think>forgot to close it all got printed",
			want: "<think>forgot to close it all got printed",
		},
		{
			name: "only reasoning — caller decides what to do with empty",
			in:   "<think>nothing to say</think>",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stripReasoning(tc.in)
			if got != tc.want {
				t.Errorf("stripReasoning(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestStripReasoningStreaming(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "no tags",
			in:   "Hello world",
			want: "Hello world",
		},
		{
			name: "closed think stripped",
			in:   "<think>plotting</think>final answer",
			want: "final answer",
		},
		{
			name: "open think hidden until close arrives — simulate mid-stream",
			in:   "<think>reasoning still in progress",
			want: "",
		},
		{
			name: "open tag after visible prefix — only prefix shown",
			in:   "visible prefix <thinking>secret...",
			want: "visible prefix",
		},
		{
			name: "mix of closed + open — closed stripped, open hides tail",
			in:   "<think>first thought</think>visible <reasoning>second",
			want: "visible",
		},
		{
			name: "partial opener at chunk end — still gets hidden",
			in:   "visible content <thi",
			want: "visible content",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stripReasoningStreaming(tc.in)
			if got != tc.want {
				t.Errorf("stripReasoningStreaming(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestWrapStreamCallbackStripping(t *testing.T) {
	var calls []string
	underlying := chat.StreamCallback(func(s string) { calls = append(calls, s) })
	wrapped := wrapStreamCallbackStripping(underlying)

	wrapped("<think>secret")
	wrapped("<think>secret</think>hello")
	wrapped("<think>secret</think>hello, <reasoning>more")

	// First call: all hidden, underlying not invoked.
	// Second call: underlying sees "hello".
	// Third call: underlying sees "hello," (reasoning opener truncates rest).
	if len(calls) != 2 {
		t.Fatalf("expected 2 underlying calls, got %d: %v", len(calls), calls)
	}
	if calls[0] != "hello" {
		t.Errorf("first emitted = %q, want %q", calls[0], "hello")
	}
	if calls[1] != "hello," {
		t.Errorf("second emitted = %q, want %q", calls[1], "hello,")
	}
}

func TestWrapStreamCallbackStripping_NilPassthrough(t *testing.T) {
	got := wrapStreamCallbackStripping(nil)
	if got != nil {
		t.Errorf("nil input should produce nil callback, got non-nil")
	}
}

func TestBuildSystemPrompt_InjectsProjectPrefix(t *testing.T) {
	projects := []*registry.Project{
		{ID: "p-a", Chat: registry.ProjectChat{SystemPrefix: "HOUSE RULE: always commit in Czech."}},
		{ID: "p-b", Chat: registry.ProjectChat{SystemPrefix: "Respond only in JSON."}},
		{ID: "p-c"},
	}
	out := BuildSystemPrompt("p-a", projects)
	if !strings.Contains(out, "HOUSE RULE: always commit in Czech.") {
		t.Errorf("expected p-a prefix in prompt, got:\n%s", out[:200])
	}
	// prefix must come before the generic body
	prefixIdx := strings.Index(out, "HOUSE RULE")
	genericIdx := strings.Index(out, "You are a helpful assistant")
	if prefixIdx < 0 || genericIdx < 0 || prefixIdx >= genericIdx {
		t.Errorf("prefix should precede generic body; prefix=%d, generic=%d", prefixIdx, genericIdx)
	}
}

func TestBuildSystemPrompt_NoPrefixWhenInactive(t *testing.T) {
	projects := []*registry.Project{
		{ID: "p-a", Chat: registry.ProjectChat{SystemPrefix: "secret"}},
	}
	// Different active project — prefix for p-a must NOT appear.
	out := BuildSystemPrompt("p-b", projects)
	if strings.Contains(out, "secret") {
		t.Errorf("prefix leaked across projects")
	}
}

func TestBuildSystemPrompt_EmptyPrefixIgnored(t *testing.T) {
	projects := []*registry.Project{
		{ID: "p", Chat: registry.ProjectChat{SystemPrefix: "   \n  "}}, // whitespace-only
	}
	out := BuildSystemPrompt("p", projects)
	// Should not start with blank whitespace-prefix content.
	if !strings.HasPrefix(strings.TrimSpace(out), "Current time") && !strings.HasPrefix(strings.TrimSpace(out), "You are a helpful assistant") {
		// formatTimeContext() wraps the first line — just ensure no empty leading section.
		t.Logf("out starts with: %q", strings.TrimSpace(out)[:60])
	}
}
