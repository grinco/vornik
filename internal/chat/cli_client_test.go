package chat

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseToolCall(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		wantOK   bool
		wantName string
		wantArgs string
	}{
		{
			name:     "bare envelope",
			input:    `{"tool_call":{"name":"search","arguments":{"q":"foo"}}}`,
			wantOK:   true,
			wantName: "search",
			wantArgs: `{"q":"foo"}`,
		},
		{
			name:     "envelope with preface prose",
			input:    `Sure, I'll look that up. {"tool_call":{"name":"search","arguments":{"q":"foo"}}}`,
			wantOK:   true,
			wantName: "search",
			wantArgs: `{"q":"foo"}`,
		},
		{
			name:     "empty arguments → defaulted to {}",
			input:    `{"tool_call":{"name":"ping"}}`,
			wantOK:   true,
			wantName: "ping",
			wantArgs: "{}",
		},
		{
			// Regression for the 13:15 dispatcher failure: with max_turns=1
			// the model sometimes emits the tool call AND a hallucinated
			// tool result after it, e.g.
			//   {"tool_call":{...}}
			//
			//   {"projects":[...]}
			// The parser used to grab outer-braces and fail json.Unmarshal;
			// json.Decoder stops at the first complete value.
			name:     "envelope followed by trailing hallucinated JSON",
			input:    `{"tool_call":{"name":"list_projects","arguments":{}}}` + "\n\n" + `{"projects":[{"id":"1"}]}`,
			wantOK:   true,
			wantName: "list_projects",
			wantArgs: "{}",
		},
		{
			name:   "prose only — no tool_call key present",
			input:  "I think the answer is 42.",
			wantOK: false,
		},
		{
			name:   "mentions the key but has no JSON object",
			input:  `Remember to set "tool_call" before running.`,
			wantOK: false,
		},
		{
			name:   "invalid JSON shape",
			input:  `{"tool_call": not valid}`,
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, args, ok := parseToolCall(tc.input)
			assert.Equal(t, tc.wantOK, ok)
			if !tc.wantOK {
				return
			}
			assert.Equal(t, tc.wantName, name)
			// Normalize JSON so whitespace doesn't make the test flaky.
			var got, want any
			require.NoError(t, json.Unmarshal(args, &got))
			require.NoError(t, json.Unmarshal([]byte(tc.wantArgs), &want))
			assert.Equal(t, want, got)
		})
	}
}

func TestApplyToolCallShim(t *testing.T) {
	t.Run("flips assistant text into tool call", func(t *testing.T) {
		resp := &ChatResponse{
			Choices: []struct {
				Index        int     `json:"index"`
				Message      Message `json:"message"`
				FinishReason string  `json:"finish_reason"`
			}{
				{
					Message:      Message{Role: "assistant", Content: `{"tool_call":{"name":"list","arguments":{"limit":5}}}`},
					FinishReason: "stop",
				},
			},
		}
		require.NoError(t, applyToolCallShim(resp))
		msg := resp.Choices[0].Message
		assert.Empty(t, msg.Content, "tool-call turns should not carry stray text")
		require.Len(t, msg.ToolCalls, 1)
		assert.Equal(t, "list", msg.ToolCalls[0].Function.Name)
		assert.Contains(t, msg.ToolCalls[0].Function.Arguments, "limit")
		assert.Equal(t, "tool_calls", resp.Choices[0].FinishReason)
	})

	t.Run("leaves plain text alone", func(t *testing.T) {
		resp := &ChatResponse{
			Choices: []struct {
				Index        int     `json:"index"`
				Message      Message `json:"message"`
				FinishReason string  `json:"finish_reason"`
			}{
				{Message: Message{Role: "assistant", Content: "The sky is blue."}, FinishReason: "stop"},
			},
		}
		require.NoError(t, applyToolCallShim(resp))
		assert.Equal(t, "The sky is blue.", resp.Choices[0].Message.Content)
		assert.Empty(t, resp.Choices[0].Message.ToolCalls)
		assert.Equal(t, "stop", resp.Choices[0].FinishReason)
	})
}

func TestRenderToolsForPrompt(t *testing.T) {
	tools := []Tool{
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "search",
				Description: "Search the web.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "ping",
				Description: "Check health.",
			},
		},
	}
	out := renderToolsForPrompt(tools)
	assert.Contains(t, out, "search")
	assert.Contains(t, out, "Search the web.")
	assert.Contains(t, out, `"q"`, "JSON-schema parameters must appear verbatim")
	assert.Contains(t, out, "ping")
	assert.Contains(t, out, "Check health.")
}

func TestParseStreamJSON(t *testing.T) {
	// Synthetic stream-json in the shape Claude Code emits: one JSON
	// object per line, "assistant" events carry content blocks, "result"
	// carries the usage rollup.
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"abc"}`,
		`{"type":"assistant","message":{"id":"msg_1","model":"claude-opus-4-7","role":"assistant","content":[{"type":"text","text":"Hello"}]}}`,
		`{"type":"assistant","message":{"id":"msg_1","role":"assistant","content":[{"type":"text","text":", world"}]}}`,
		`{"type":"result","subtype":"success","total_cost_usd":0.001,"usage":{"input_tokens":42,"output_tokens":9}}`,
		"",
	}, "\n")

	var chunks []string
	resp, err := parseStreamJSON(strings.NewReader(stream), func(s string) {
		chunks = append(chunks, s)
	})
	require.NoError(t, err)
	require.Len(t, resp.Choices, 1)
	assert.Equal(t, "Hello, world", resp.Choices[0].Message.Content)
	assert.Equal(t, "claude-opus-4-7", resp.Model)
	assert.Equal(t, "msg_1", resp.ID)
	assert.Equal(t, 42, resp.Usage.PromptTokens)
	assert.Equal(t, 9, resp.Usage.CompletionTokens)
	assert.Equal(t, 51, resp.Usage.TotalTokens)
	assert.Equal(t, []string{"Hello", "Hello, world"}, chunks,
		"onText should receive monotonically-growing accumulated text")
}

func TestParseStreamJSON_NoText(t *testing.T) {
	// Stream without any assistant text should return an error — otherwise
	// the dispatcher would receive an empty ChatResponse and try to
	// interpret nothing as a tool call.
	stream := `{"type":"system","subtype":"init"}` + "\n" +
		`{"type":"result","subtype":"success","usage":{"input_tokens":1,"output_tokens":0}}` + "\n"
	_, err := parseStreamJSON(strings.NewReader(stream), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no assistant text")
}

func TestPackMessages(t *testing.T) {
	c := NewCLIClient("claude-opus-4-7")
	req, err := c.packMessages([]Message{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "First question?"},
		{Role: "assistant", Content: "First answer."},
		{Role: "user", Content: "Follow-up?"},
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, "You are helpful.", req.systemPrompt)
	// Latest user turn is the direct prompt; history is prepended.
	assert.Contains(t, req.userTurn, "Follow-up?")
	assert.Contains(t, req.userTurn, "First question?")
	assert.Contains(t, req.userTurn, "First answer.")
}

func TestPackMessages_ToolsGetAppendedToSystem(t *testing.T) {
	c := NewCLIClient("")
	tools := []Tool{{Type: "function", Function: ToolFunction{Name: "foo", Description: "Do foo."}}}
	req, err := c.packMessages([]Message{
		{Role: "system", Content: "Be concise."},
		{Role: "user", Content: "hi"},
	}, tools)
	require.NoError(t, err)
	// System prompt carries: original + shim instruction + tool catalog.
	assert.Contains(t, req.systemPrompt, "Be concise.")
	assert.Contains(t, req.systemPrompt, "tool_call")
	assert.Contains(t, req.systemPrompt, "foo")
}

// Compile-time assertion covered by provider.go's _ = (*Client)(nil);
// the CLIClient equivalent lives at the bottom of cli_client.go. This
// test just makes sure the expected provider interface methods stay
// callable, which catches accidental method-set divergence.
func TestCLIClient_ImplementsProvider(t *testing.T) {
	var _ Provider = NewCLIClient("m")
	var _ ModelOverridable = NewCLIClient("m")
}

func TestCLIClient_WithModel(t *testing.T) {
	orig := NewCLIClient("claude-sonnet-4-6",
		WithCLIBinary("/usr/bin/claude"),
	)
	clone := orig.WithModel("claude-opus-4-6")

	// Clone has the new model; original is untouched — the proxy
	// relies on this to serve two concurrent requests with two
	// different models without one stomping on the other.
	assert.Equal(t, "claude-opus-4-6", clone.Model())
	assert.Equal(t, "claude-sonnet-4-6", orig.Model())

	// Other fields carry through unchanged. We check the binary path
	// as a stand-in for "the rest of the config was preserved".
	cc, ok := clone.(*CLIClient)
	require.True(t, ok)
	assert.Equal(t, "/usr/bin/claude", cc.binary)
	assert.Equal(t, 1, cc.maxTurns) // default, preserved through the clone
}

func TestClient_WithModel(t *testing.T) {
	orig := NewClient("https://example.test/v1", "KEY", "model-a",
		WithTimeout(42*time.Second))
	clone := orig.WithModel("model-b")

	assert.Equal(t, "model-b", clone.Model())
	assert.Equal(t, "model-a", orig.Model())

	cc, ok := clone.(*Client)
	require.True(t, ok)
	assert.Equal(t, "KEY", cc.apiKey)
	assert.Equal(t, 42*time.Second, cc.timeout)
}
