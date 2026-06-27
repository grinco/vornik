package chat

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseCodexStream(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"thread.started","thread_id":"tid-1"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"Hello"}}`,
		`{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":", world"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":42,"cached_input_tokens":20,"output_tokens":9}}`,
		"",
	}, "\n")

	var chunks []string
	resp, err := parseCodexStream(strings.NewReader(stream), func(s string) {
		chunks = append(chunks, s)
	})
	require.NoError(t, err)
	require.Len(t, resp.Choices, 1)
	assert.Equal(t, "Hello, world", resp.Choices[0].Message.Content)
	assert.Equal(t, "tid-1", resp.ID)
	assert.Equal(t, 42, resp.Usage.PromptTokens)
	assert.Equal(t, 9, resp.Usage.CompletionTokens)
	assert.Equal(t, 51, resp.Usage.TotalTokens)
	assert.Equal(t, []string{"Hello", "Hello, world"}, chunks)
}

func TestParseCodexStream_TurnFailedSurfacesError(t *testing.T) {
	// Real failure shape: model not permitted under ChatGPT account.
	stream := strings.Join([]string{
		`{"type":"thread.started","thread_id":"tid-fail"}`,
		`{"type":"turn.started"}`,
		`{"type":"error","message":"{\"type\":\"error\",\"status\":400,\"error\":{\"message\":\"model not supported\"}}"}`,
		`{"type":"turn.failed","error":{"message":"model not supported"}}`,
		"",
	}, "\n")
	_, err := parseCodexStream(strings.NewReader(stream), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "codex:")
	assert.Contains(t, err.Error(), "model not supported")
}

func TestParseCodexStream_NoAgentMessage(t *testing.T) {
	stream := `{"type":"thread.started","thread_id":"x"}` + "\n" +
		`{"type":"turn.completed","usage":{"input_tokens":10}}` + "\n"
	_, err := parseCodexStream(strings.NewReader(stream), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no agent_message")
}

func TestParseCodexStream_IgnoresUnknownItemTypes(t *testing.T) {
	// codex emits item.completed for non-message items too (tool_call,
	// plan, etc.) — the parser should skip those and still pick up the
	// actual agent_message.
	stream := strings.Join([]string{
		`{"type":"item.completed","item":{"id":"a","type":"reasoning","text":"thinking..."}}`,
		`{"type":"item.completed","item":{"id":"b","type":"tool_call","text":"ignored"}}`,
		`{"type":"item.completed","item":{"id":"c","type":"agent_message","text":"final reply"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":5,"output_tokens":2}}`,
		"",
	}, "\n")
	resp, err := parseCodexStream(strings.NewReader(stream), nil)
	require.NoError(t, err)
	assert.Equal(t, "final reply", resp.Choices[0].Message.Content)
}

func TestCodexCLIClient_ImplementsInterfaces(t *testing.T) {
	var _ Provider = NewCodexCLIClient("model-x")
	var _ ModelOverridable = NewCodexCLIClient("model-x")
}

func TestCodexCLIClient_WithModel(t *testing.T) {
	orig := NewCodexCLIClient("gpt-5.4-mini",
		WithCodexBinary("/usr/bin/codex"),
	)
	clone := orig.WithModel("gpt-5.3-codex")
	assert.Equal(t, "gpt-5.3-codex", clone.Model())
	assert.Equal(t, "gpt-5.4-mini", orig.Model(), "WithModel must not mutate the original")

	cc, ok := clone.(*CodexCLIClient)
	require.True(t, ok)
	assert.Equal(t, "/usr/bin/codex", cc.binary, "other fields should carry through")
}

func TestCodexPackMessages(t *testing.T) {
	c := NewCodexCLIClient("")
	req, err := c.packMessages([]Message{
		{Role: "system", Content: "Be concise."},
		{Role: "user", Content: "first?"},
		{Role: "assistant", Content: "first answer"},
		{Role: "user", Content: "follow-up?"},
	}, nil)
	require.NoError(t, err)
	assert.Contains(t, req.prompt, "SYSTEM INSTRUCTIONS:")
	assert.Contains(t, req.prompt, "Be concise.")
	assert.Contains(t, req.prompt, "PRIOR CONVERSATION:")
	assert.Contains(t, req.prompt, "first?")
	assert.Contains(t, req.prompt, "first answer")
	assert.Contains(t, req.prompt, "CURRENT USER MESSAGE:")
	assert.Contains(t, req.prompt, "follow-up?")
}

func TestCodexPackMessages_InjectsTools(t *testing.T) {
	c := NewCodexCLIClient("")
	req, err := c.packMessages([]Message{
		{Role: "user", Content: "hi"},
	}, []Tool{{Type: "function", Function: ToolFunction{Name: "ping", Description: "p"}}})
	require.NoError(t, err)
	assert.Contains(t, req.prompt, "tool_call")
	assert.Contains(t, req.prompt, "ping")
}
