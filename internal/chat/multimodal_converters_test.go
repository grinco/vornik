package chat

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestConvertOpenAIBlocksToAnthropic_Text — text blocks pass through
// 1:1, empty text blocks are dropped (Anthropic 400s on empty text).
func TestConvertOpenAIBlocksToAnthropic_Text(t *testing.T) {
	in := []ContentBlock{
		TextBlock("hello"),
		TextBlock(""), // dropped
		TextBlock("world"),
	}
	out, err := convertOpenAIBlocksToAnthropic(in)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 blocks (empty dropped), got %d", len(out))
	}
	if out[0]["type"] != "text" || out[0]["text"] != "hello" {
		t.Fatalf("first: %+v", out[0])
	}
	if out[1]["type"] != "text" || out[1]["text"] != "world" {
		t.Fatalf("second: %+v", out[1])
	}
}

// TestConvertOpenAIBlocksToAnthropic_DataURLImage — the dominant image
// case: data:image/jpeg;base64,XXX must split into the {type:"base64",
// media_type, data} source shape Anthropic expects.
func TestConvertOpenAIBlocksToAnthropic_DataURLImage(t *testing.T) {
	in := []ContentBlock{
		ImageBlock("data:image/jpeg;base64,/9j/4AAQ"),
	}
	out, err := convertOpenAIBlocksToAnthropic(in)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(out) != 1 || out[0]["type"] != "image" {
		t.Fatalf("unexpected: %+v", out)
	}
	src, ok := out[0]["source"].(map[string]any)
	if !ok {
		t.Fatalf("source missing or wrong type: %+v", out[0])
	}
	if src["type"] != "base64" || src["media_type"] != "image/jpeg" || src["data"] != "/9j/4AAQ" {
		t.Fatalf("source: %+v", src)
	}
}

// TestConvertOpenAIBlocksToAnthropic_RemoteURLImage — remote http(s)
// URLs are translated into Anthropic's {type:"url", url} source.
func TestConvertOpenAIBlocksToAnthropic_RemoteURLImage(t *testing.T) {
	in := []ContentBlock{ImageBlock("https://example.com/cat.jpg")}
	out, err := convertOpenAIBlocksToAnthropic(in)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	src := out[0]["source"].(map[string]any)
	if src["type"] != "url" || src["url"] != "https://example.com/cat.jpg" {
		t.Fatalf("source: %+v", src)
	}
}

// TestConvertOpenAIBlocksToAnthropic_RejectsMalformed — silently
// passing junk through to the API would produce confusing 400s.
// Reject upstream of the wire with descriptive errors.
func TestConvertOpenAIBlocksToAnthropic_RejectsMalformed(t *testing.T) {
	cases := []struct {
		name  string
		in    []ContentBlock
		wants string
	}{
		{"missing url", []ContentBlock{{Type: "image_url", ImageURL: &ImageURLContent{}}}, "missing url"},
		{"nil image_url", []ContentBlock{{Type: "image_url"}}, "missing url"},
		{"data url no comma", []ContentBlock{ImageBlock("data:image/jpeg;base64")}, "missing comma"},
		{"data url not base64", []ContentBlock{ImageBlock("data:image/jpeg;utf8,abc")}, "base64"},
		{"data url no media type", []ContentBlock{ImageBlock("data:;base64,abc")}, "media type"},
		{"file scheme", []ContentBlock{ImageBlock("file:///etc/passwd")}, "data: or http"},
		{"unknown block type", []ContentBlock{{Type: "audio_url"}}, "unsupported content block type"},
	}
	for _, tc := range cases {
		_, err := convertOpenAIBlocksToAnthropic(tc.in)
		if err == nil {
			t.Errorf("%s: expected error, got nil", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.wants) {
			t.Errorf("%s: error %q missing substring %q", tc.name, err.Error(), tc.wants)
		}
	}
}

// TestClaudeBuildRequestBody_TextOnlyRegression — the dominant existing
// case: text-only messages must produce the same wire shape as before
// the multimodal refactor. Without this guard, a refactor of the
// content path could subtly break every text turn.
func TestClaudeBuildRequestBody_TextOnlyRegression(t *testing.T) {
	c := &ClaudeSubscriptionClient{model: "claude-sonnet-4-6", maxTokens: 1024}
	msgs := []Message{
		{Role: "system", Content: "be concise"},
		{Role: "user", Content: "hi"},
	}
	body, err := c.buildRequestBody(msgs, nil, claudeAccountInfo{}, "sess-1")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var got struct {
		Messages []struct {
			Role    string           `json:"role"`
			Content []map[string]any `json:"content"`
		} `json:"messages"`
		System []map[string]any `json:"system"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("re-unmarshal: %v\n%s", err, body)
	}
	if len(got.Messages) != 1 || got.Messages[0].Role != "user" {
		t.Fatalf("messages: %+v", got.Messages)
	}
	c0 := got.Messages[0].Content[0]
	if c0["type"] != "text" || c0["text"] != "hi" {
		t.Fatalf("user content: %+v", c0)
	}
	// system has identity prelude + our system message
	if len(got.System) != 2 || got.System[1]["text"] != "be concise" {
		t.Fatalf("system: %+v", got.System)
	}
}

// TestClaudeBuildRequestBody_MultimodalUser — multimodal user turn:
// blocks must be translated to Anthropic content blocks (text + image
// with base64 source).
func TestClaudeBuildRequestBody_MultimodalUser(t *testing.T) {
	c := &ClaudeSubscriptionClient{model: "claude-sonnet-4-6", maxTokens: 1024}
	msgs := []Message{
		{Role: "user", Blocks: []ContentBlock{
			TextBlock("describe this"),
			ImageBlock("data:image/png;base64,iVBOR"),
		}},
	}
	body, err := c.buildRequestBody(msgs, nil, claudeAccountInfo{}, "s")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var got struct {
		Messages []struct {
			Content []map[string]any `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, body)
	}
	c0, c1 := got.Messages[0].Content[0], got.Messages[0].Content[1]
	if c0["type"] != "text" || c0["text"] != "describe this" {
		t.Fatalf("text block: %+v", c0)
	}
	if c1["type"] != "image" {
		t.Fatalf("expected image block, got %+v", c1)
	}
	src := c1["source"].(map[string]any)
	if src["media_type"] != "image/png" || src["data"] != "iVBOR" {
		t.Fatalf("image source: %+v", src)
	}
}

// TestClaudeBuildRequestBody_RejectsImageInSystem — Anthropic doesn't
// allow images in the system prompt; the converter must surface that
// before hitting the wire.
func TestClaudeBuildRequestBody_RejectsImageInSystem(t *testing.T) {
	c := &ClaudeSubscriptionClient{model: "claude-sonnet-4-6", maxTokens: 1024}
	msgs := []Message{
		{Role: "system", Blocks: []ContentBlock{
			TextBlock("be concise"),
			ImageBlock("data:image/png;base64,xx"),
		}},
		{Role: "user", Content: "hi"},
	}
	_, err := c.buildRequestBody(msgs, nil, claudeAccountInfo{}, "s")
	if err == nil {
		t.Fatalf("expected error for image in system, got nil")
	}
	if !strings.Contains(err.Error(), "system") || !strings.Contains(err.Error(), "image") {
		t.Fatalf("error mismatch: %v", err)
	}
}

// TestCodexBuildRequestBody_RejectsImage — the Codex Responses API
// can't accept images. Image blocks must produce a clear error so the
// dispatcher can route image-bearing requests elsewhere instead of
// silently dropping the user's input.
func TestCodexBuildRequestBody_RejectsImage(t *testing.T) {
	c := &CodexSubscriptionClient{model: "gpt-5.4", effortLevel: "low"}
	msgs := []Message{
		{Role: "user", Blocks: []ContentBlock{
			TextBlock("describe"),
			ImageBlock("data:image/png;base64,xx"),
		}},
	}
	_, err := c.buildRequestBody(msgs, nil)
	if err == nil {
		t.Fatalf("expected error for image input, got nil")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("error mismatch: %v", err)
	}
}

// TestCodexBuildRequestBody_TextOnlyRegression — text-only Codex turns
// must produce the same shape as before, including the input_text/
// output_text wrappers and instructions extraction.
func TestCodexBuildRequestBody_TextOnlyRegression(t *testing.T) {
	c := &CodexSubscriptionClient{model: "gpt-5.4", effortLevel: "medium"}
	msgs := []Message{
		{Role: "system", Content: "be concise"},
		{Role: "user", Content: "hi"},
	}
	body, err := c.buildRequestBody(msgs, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var got struct {
		Instructions string           `json:"instructions"`
		Input        []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, body)
	}
	if got.Instructions != "be concise" {
		t.Fatalf("instructions: %q", got.Instructions)
	}
	if len(got.Input) != 1 {
		t.Fatalf("input count: %d", len(got.Input))
	}
	if got.Input[0]["type"] != "message" || got.Input[0]["role"] != "user" {
		t.Fatalf("first input: %+v", got.Input[0])
	}
}

// TestCodexBuildRequestBody_BlocksTextOnlyAccepted — a Blocks-form
// message that contains ONLY text blocks is acceptable for Codex; the
// converter must extract the joined text and proceed without erroring.
// This guards the "uniform multimodal API" path: a caller that always
// uses Blocks shouldn't fail on text-only Codex calls.
func TestCodexBuildRequestBody_BlocksTextOnlyAccepted(t *testing.T) {
	c := &CodexSubscriptionClient{model: "gpt-5.4", effortLevel: "low"}
	msgs := []Message{
		{Role: "user", Blocks: []ContentBlock{TextBlock("hi"), TextBlock("again")}},
	}
	body, err := c.buildRequestBody(msgs, nil)
	if err != nil {
		t.Fatalf("blocks-text-only should succeed: %v", err)
	}
	if !strings.Contains(string(body), `"hi`) {
		t.Fatalf("text not preserved: %s", body)
	}
}

// TestEstimateTokens_BlocksAccountedFor — the conversation token
// estimator must count text blocks AND charge a per-image budget.
// Without the image charge, a multimodal conversation looks artificially
// small and we'd OOM the upstream context window.
func TestEstimateTokens_BlocksAccountedFor(t *testing.T) {
	conv := NewConversation("test", 100)
	// 100-char text message → 25 tokens
	conv.AddMessage(Message{Role: "user", Content: strings.Repeat("a", 100)})
	textOnly := conv.EstimateTokens()
	if textOnly < 20 || textOnly > 30 {
		t.Fatalf("text-only estimate off: got %d, expected ~25", textOnly)
	}
	// Add a multimodal message: 100 chars text + 1 image block
	conv.AddMessage(Message{Role: "user", Blocks: []ContentBlock{
		TextBlock(strings.Repeat("b", 100)),
		ImageBlock("data:image/jpeg;base64,xyz"),
	}})
	withImage := conv.EstimateTokens()
	// Should add ~25 tokens for text + ~800 for image = ~825.
	if withImage-textOnly < 800 {
		t.Fatalf("image budget not applied: textOnly=%d withImage=%d delta=%d (expected >= 800)",
			textOnly, withImage, withImage-textOnly)
	}
}

// TestChatProxy_PassthroughMultimodal — the daemon's chat-completions
// proxy must accept and forward multimodal content unchanged. Body
// parsing is the only Content-aware step in the proxy; the rest is
// handed to the provider. This test verifies parsing accepts the array
// form.
func TestChatProxy_ParsesMultimodalRequest(t *testing.T) {
	body := []byte(`{
		"model": "google/gemini-2.5-flash-lite",
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "describe"},
				{"type": "image_url", "image_url": {"url": "data:image/png;base64,abc"}}
			]}
		]
	}`)
	var req ChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("proxy unmarshal: %v", err)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("messages: %d", len(req.Messages))
	}
	if len(req.Messages[0].Blocks) != 2 {
		t.Fatalf("blocks not parsed: %+v", req.Messages[0])
	}
	// Re-marshal to confirm it still serialises as the multimodal form
	// (not collapsed to a string mid-flight).
	out, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if !strings.Contains(string(out), `"image_url"`) {
		t.Fatalf("image_url lost in round-trip: %s", out)
	}
}
