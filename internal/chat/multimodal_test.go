package chat

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestMessageMarshalJSON_TextFastPath covers the dominant case: a plain
// text message must serialize to {"role":"user","content":"hi"} — string
// content, not an array. Anything else breaks every existing caller.
func TestMessageMarshalJSON_TextFastPath(t *testing.T) {
	m := Message{Role: "user", Content: "hello"}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"role":"user","content":"hello"}`
	if string(data) != want {
		t.Fatalf("got %s, want %s", data, want)
	}
}

// TestMessageMarshalJSON_EmptyEmitsEmptyString covers the case where
// neither Content nor Blocks is set. Some upstream providers reject a
// missing content field outright, so we emit an empty string.
func TestMessageMarshalJSON_EmptyEmitsEmptyString(t *testing.T) {
	m := Message{Role: "system"}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"role":"system","content":""}`
	if string(data) != want {
		t.Fatalf("got %s, want %s", data, want)
	}
}

// TestMessageMarshalJSON_BlocksTakePrecedence verifies that when Blocks
// is set, content is emitted as a JSON array — and Content (string) is
// ignored. Operators who set both should see the multimodal form win;
// the alternative (silent loss of image data) is worse.
func TestMessageMarshalJSON_BlocksTakePrecedence(t *testing.T) {
	m := Message{
		Role:    "user",
		Content: "this string should be ignored",
		Blocks: []ContentBlock{
			TextBlock("describe"),
			ImageBlock("data:image/jpeg;base64,AAAA"),
		},
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got struct {
		Role    string         `json:"role"`
		Content []ContentBlock `json:"content"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("re-unmarshal: %v\npayload: %s", err, data)
	}
	if got.Role != "user" {
		t.Fatalf("role: got %q want user", got.Role)
	}
	if len(got.Content) != 2 {
		t.Fatalf("expected 2 blocks, got %d: %s", len(got.Content), data)
	}
	if got.Content[0].Type != "text" || got.Content[0].Text != "describe" {
		t.Fatalf("text block wrong: %+v", got.Content[0])
	}
	if got.Content[1].Type != "image_url" || got.Content[1].ImageURL == nil ||
		got.Content[1].ImageURL.URL != "data:image/jpeg;base64,AAAA" {
		t.Fatalf("image block wrong: %+v", got.Content[1])
	}
	// The string content must NOT leak into the wire form.
	if strings.Contains(string(data), "this string should be ignored") {
		t.Fatalf("Content string leaked into wire when Blocks set: %s", data)
	}
}

// TestMessageMarshalJSON_ToolCallsPreserved confirms the auxiliary
// fields (tool_calls, tool_call_id, name) survive marshal in both the
// fast path and the blocks path. These are load-bearing for the
// dispatcher's tool-result message construction.
func TestMessageMarshalJSON_ToolCallsPreserved(t *testing.T) {
	m := Message{
		Role: "assistant",
		ToolCalls: []ToolCall{
			{
				ID:           "call_1",
				Type:         "function",
				Function:     FunctionCall{Name: "f", Arguments: "{}"},
				ExtraContent: json.RawMessage(`{"google":{"thought_signature":"sig"}}`),
			},
		},
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"tool_calls"`) {
		t.Fatalf("tool_calls missing: %s", data)
	}
	if !strings.Contains(string(data), `"call_1"`) {
		t.Fatalf("tool_call id missing: %s", data)
	}
	if !strings.Contains(string(data), `"thought_signature":"sig"`) {
		t.Fatalf("tool_call extra_content missing: %s", data)
	}
}

// TestMessageUnmarshalJSON_StringContent — the dominant inbound case:
// agent posts {"role":"user","content":"hello"}. Must populate Content
// and leave Blocks empty.
func TestMessageUnmarshalJSON_StringContent(t *testing.T) {
	in := []byte(`{"role":"user","content":"hello"}`)
	var m Message
	if err := json.Unmarshal(in, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.Content != "hello" {
		t.Fatalf("Content: got %q want %q", m.Content, "hello")
	}
	if len(m.Blocks) != 0 {
		t.Fatalf("Blocks should be empty for string content, got %d", len(m.Blocks))
	}
}

// TestMessageUnmarshalJSON_BlocksContent — multimodal inbound:
// {"role":"user","content":[{"type":"text",...},{"type":"image_url",...}]}.
// Must populate Blocks and leave Content empty.
func TestMessageUnmarshalJSON_BlocksContent(t *testing.T) {
	in := []byte(`{"role":"user","content":[
		{"type":"text","text":"what is in this picture?"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBOR"}}
	]}`)
	var m Message
	if err := json.Unmarshal(in, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.Content != "" {
		t.Fatalf("Content should be empty for blocks content, got %q", m.Content)
	}
	if len(m.Blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(m.Blocks))
	}
	if m.Blocks[0].Type != "text" || m.Blocks[0].Text != "what is in this picture?" {
		t.Fatalf("text block: %+v", m.Blocks[0])
	}
	if m.Blocks[1].Type != "image_url" || m.Blocks[1].ImageURL == nil ||
		m.Blocks[1].ImageURL.URL != "data:image/png;base64,iVBOR" {
		t.Fatalf("image block: %+v", m.Blocks[1])
	}
}

// TestMessageUnmarshalJSON_NullAndMissing covers two adjacent edge
// cases: an explicit "content":null and an entirely missing content
// field. Both must produce a zero-value Message without erroring —
// some providers omit content on tool-only assistant turns.
func TestMessageUnmarshalJSON_NullAndMissing(t *testing.T) {
	for _, in := range []string{
		`{"role":"assistant","content":null,"tool_calls":[{"id":"x","type":"function","function":{"name":"f","arguments":"{}"}}]}`,
		`{"role":"assistant","tool_calls":[{"id":"x","type":"function","function":{"name":"f","arguments":"{}"}}]}`,
	} {
		var m Message
		if err := json.Unmarshal([]byte(in), &m); err != nil {
			t.Fatalf("unmarshal %q: %v", in, err)
		}
		if m.Content != "" || len(m.Blocks) != 0 {
			t.Fatalf("expected empty content for %q, got %q / %d blocks", in, m.Content, len(m.Blocks))
		}
		if len(m.ToolCalls) != 1 {
			t.Fatalf("tool_calls lost in %q", in)
		}
	}
}

// TestMessageUnmarshalJSON_RejectsUnsupportedShape — an object or
// number for content should fail loudly. Silently coercing it would
// hide a real bug in whatever produced the payload.
func TestMessageUnmarshalJSON_RejectsUnsupportedShape(t *testing.T) {
	for _, in := range []string{
		`{"role":"user","content":42}`,
		`{"role":"user","content":{"foo":"bar"}}`,
	} {
		var m Message
		err := json.Unmarshal([]byte(in), &m)
		if err == nil {
			t.Fatalf("expected error for %q, got nil", in)
		}
	}
}

// TestMessageRoundTrip — marshal then unmarshal must be a fixed point
// for both the string fast path and the blocks path. Catches subtle
// bugs where MarshalJSON and UnmarshalJSON disagree on the wire form.
func TestMessageRoundTrip(t *testing.T) {
	cases := []Message{
		{Role: "user", Content: "hi"},
		{Role: "system", Content: "you are a helpful assistant"},
		{Role: "user", Blocks: []ContentBlock{
			TextBlock("describe this image"),
			ImageBlock("data:image/jpeg;base64,abc"),
		}},
		{Role: "assistant", ToolCalls: []ToolCall{
			{
				ID:           "c1",
				Type:         "function",
				Function:     FunctionCall{Name: "n", Arguments: `{"x":1}`},
				ExtraContent: json.RawMessage(`{"google":{"thought_signature":"sig"}}`),
			},
		}},
		{Role: "tool", Content: "result", ToolCallID: "c1"},
	}
	for i, in := range cases {
		data, err := json.Marshal(in)
		if err != nil {
			t.Fatalf("case %d marshal: %v", i, err)
		}
		var out Message
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatalf("case %d unmarshal: %v\npayload: %s", i, err, data)
		}
		if out.Role != in.Role || out.Content != in.Content || out.ToolCallID != in.ToolCallID {
			t.Fatalf("case %d field mismatch: in=%+v out=%+v", i, in, out)
		}
		if len(out.Blocks) != len(in.Blocks) {
			t.Fatalf("case %d block count: in=%d out=%d", i, len(in.Blocks), len(out.Blocks))
		}
		if len(out.ToolCalls) != len(in.ToolCalls) {
			t.Fatalf("case %d tool_calls count: in=%d out=%d", i, len(in.ToolCalls), len(out.ToolCalls))
		}
		if len(in.ToolCalls) > 0 && string(out.ToolCalls[0].ExtraContent) != string(in.ToolCalls[0].ExtraContent) {
			t.Fatalf("case %d tool_call extra_content mismatch: in=%s out=%s", i, in.ToolCalls[0].ExtraContent, out.ToolCalls[0].ExtraContent)
		}
	}
}

// TestEffectiveText — text-only fast path returns Content; multimodal
// case joins all text blocks with newlines and skips images. Used by
// callers that only care about the text payload (logs, regex parsers).
func TestEffectiveText(t *testing.T) {
	if got := (Message{Content: "hi"}).EffectiveText(); got != "hi" {
		t.Fatalf("string fast path: got %q want hi", got)
	}
	m := Message{Blocks: []ContentBlock{
		TextBlock("first"),
		ImageBlock("data:image/png;base64,xx"),
		TextBlock("second"),
	}}
	if got := m.EffectiveText(); got != "first\nsecond" {
		t.Fatalf("multimodal: got %q want \"first\\nsecond\"", got)
	}
}

// TestBuildDataURL — helper for inlining local image bytes into a data:
// URL. Empty input returns empty (caller's responsibility to skip).
func TestBuildDataURL(t *testing.T) {
	if got := BuildDataURL("image/jpeg", []byte("hi")); got != "data:image/jpeg;base64,aGk=" {
		t.Fatalf("got %q", got)
	}
	if got := BuildDataURL("", []byte("hi")); got != "" {
		t.Fatalf("empty mime should return empty, got %q", got)
	}
	if got := BuildDataURL("image/jpeg", nil); got != "" {
		t.Fatalf("empty bytes should return empty, got %q", got)
	}
}

// TestChatRequestMarshalContainsContentArray — end-to-end check that a
// ChatRequest with a multimodal Message serializes to a body the OpenAI-
// compat upstream will accept (content as array). This is the contract
// the http and vertex providers rely on.
func TestChatRequestMarshalContainsContentArray(t *testing.T) {
	req := ChatRequest{
		Model: "google/gemini-2.5-flash-lite",
		Messages: []Message{{
			Role: "user",
			Blocks: []ContentBlock{
				TextBlock("describe this"),
				ImageBlock("data:image/jpeg;base64,xyz"),
			},
		}},
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(data)
	if !strings.Contains(s, `"content":[`) {
		t.Fatalf("expected content array in wire form, got: %s", s)
	}
	if !strings.Contains(s, `"image_url"`) {
		t.Fatalf("missing image_url block: %s", s)
	}
	if !strings.Contains(s, `"data:image/jpeg;base64,xyz"`) {
		t.Fatalf("data URL missing: %s", s)
	}
}
