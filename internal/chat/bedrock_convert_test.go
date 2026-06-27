package chat

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	bedrockdoc "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	bedrocktypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

// TestOpenAIMessagesToBedrock_SystemSplit — the headline contract:
// system messages move into the separate `system` array; user/
// assistant messages land in the `messages` slice with the right
// ConversationRole. This is what the rest of the conversion logic
// rests on.
func TestOpenAIMessagesToBedrock_SystemSplit(t *testing.T) {
	in := []Message{
		{Role: "system", Content: "you are a helpful assistant"},
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi"},
		{Role: "user", Content: "how are you?"},
	}
	system, msgs, err := openAIMessagesToBedrock(in)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(system) != 1 {
		t.Fatalf("expected 1 system block, got %d", len(system))
	}
	if sb, ok := system[0].(*bedrocktypes.SystemContentBlockMemberText); !ok {
		t.Errorf("system block is not text: %T", system[0])
	} else if sb.Value != "you are a helpful assistant" {
		t.Errorf("system text: got %q", sb.Value)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 conversation messages, got %d", len(msgs))
	}
	if msgs[0].Role != bedrocktypes.ConversationRoleUser ||
		msgs[1].Role != bedrocktypes.ConversationRoleAssistant ||
		msgs[2].Role != bedrocktypes.ConversationRoleUser {
		t.Errorf("role sequence wrong: %v %v %v", msgs[0].Role, msgs[1].Role, msgs[2].Role)
	}
}

// TestOpenAIMessagesToBedrock_RejectsAllSystem — Bedrock requires at
// least one user/assistant turn. A messages list with only system
// content must fail-fast at the converter so the SDK call doesn't
// 400 mid-roundtrip.
func TestOpenAIMessagesToBedrock_RejectsAllSystem(t *testing.T) {
	_, _, err := openAIMessagesToBedrock([]Message{
		{Role: "system", Content: "hi"},
		{Role: "system", Content: "hi again"},
	})
	if err == nil {
		t.Fatal("expected error for all-system input")
	}
	if !strings.Contains(err.Error(), "no user/assistant messages") {
		t.Errorf("error doesn't name the cause: %v", err)
	}
}

// TestOpenAIMessagesToBedrock_RejectsEmpty — defensive: a caller
// with an empty messages slice gets a clean error instead of a
// confusing AWS 400.
func TestOpenAIMessagesToBedrock_RejectsEmpty(t *testing.T) {
	_, _, err := openAIMessagesToBedrock(nil)
	if err == nil {
		t.Fatal("expected error for empty input")
	}
}

// TestOpenAIMessagesToBedrock_BlocksProduceSeparateContentBlocks —
// each input ContentBlock maps to its own Bedrock content block
// (text → ContentBlockMemberText, image_url with data URL →
// ContentBlockMemberImage). Bedrock's Converse API natively
// supports multi-block messages, so concatenation isn't required.
// Image blocks with un-parseable URLs (e.g. "data:image/png;base64,...")
// fall through to a fallback text note rather than being dropped.
func TestOpenAIMessagesToBedrock_BlocksProduceSeparateContentBlocks(t *testing.T) {
	in := []Message{
		{Role: "user", Blocks: []ContentBlock{
			TextBlock("first paragraph"),
			ImageBlock("data:image/png;base64,..."), // payload is "..." — base64 decode emits a fallback note
			TextBlock("second paragraph"),
		}},
	}
	_, msgs, err := openAIMessagesToBedrock(in)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	// Three blocks: text + image-fallback-note + text. The exact
	// order matters so the model sees the prose flow.
	if len(msgs[0].Content) != 3 {
		t.Fatalf("expected 3 content blocks, got %d (%+v)", len(msgs[0].Content), msgs[0].Content)
	}
	first, ok := msgs[0].Content[0].(*bedrocktypes.ContentBlockMemberText)
	if !ok || first.Value != "first paragraph" {
		t.Errorf("content[0]: got %T %q", msgs[0].Content[0], textOf(msgs[0].Content[0]))
	}
	last, ok := msgs[0].Content[2].(*bedrocktypes.ContentBlockMemberText)
	if !ok || last.Value != "second paragraph" {
		t.Errorf("content[2]: got %T %q", msgs[0].Content[2], textOf(msgs[0].Content[2]))
	}
}

// TestOpenAIMessagesToBedrock_DataURLImageInlined — happy-path
// multimodal: a real PNG data URL becomes a ContentBlockMemberImage
// with the bytes inlined. The resulting wire shape is what Bedrock
// Converse expects for vision-capable models.
func TestOpenAIMessagesToBedrock_DataURLImageInlined(t *testing.T) {
	// 1x1 transparent PNG.
	png := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNgYAAAAAMAASsJTYQAAAAASUVORK5CYII="
	in := []Message{{Role: "user", Blocks: []ContentBlock{
		TextBlock("describe this image:"),
		ImageBlock(png),
	}}}
	_, msgs, err := openAIMessagesToBedrock(in)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(msgs[0].Content) != 2 {
		t.Fatalf("expected text + image blocks, got %d", len(msgs[0].Content))
	}
	img, ok := msgs[0].Content[1].(*bedrocktypes.ContentBlockMemberImage)
	if !ok {
		t.Fatalf("content[1] is not an image block: %T", msgs[0].Content[1])
	}
	if img.Value.Format != bedrocktypes.ImageFormatPng {
		t.Errorf("format: got %v, want png", img.Value.Format)
	}
	src, ok := img.Value.Source.(*bedrocktypes.ImageSourceMemberBytes)
	if !ok {
		t.Fatalf("source is not bytes: %T", img.Value.Source)
	}
	if len(src.Value) == 0 {
		t.Error("decoded image bytes are empty")
	}
}

// textOf is a tiny helper for diagnostic test failures — extracts
// the text from a content block when it's a text member, empty
// string otherwise.
func textOf(b bedrocktypes.ContentBlock) string {
	if tb, ok := b.(*bedrocktypes.ContentBlockMemberText); ok {
		return tb.Value
	}
	return ""
}

// TestOpenAIMessagesToBedrock_AggregatesParallelToolResults — the
// regression for the 2026-05-07 production
// ValidationException: when the assistant emits N parallel
// tool_use blocks, the next user message MUST contain all N
// matching toolResult blocks. Splitting them into separate user
// messages produces a 400 from Bedrock. Reproduces the user's
// "Expected toolResult blocks at messages.6.content for the
// following Ids: functions.memory_search:3" failure.
func TestOpenAIMessagesToBedrock_AggregatesParallelToolResults(t *testing.T) {
	in := []Message{
		{Role: "user", Content: "search memory for 3 things"},
		// Assistant emits 3 parallel tool calls.
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "call_1", Type: "function", Function: FunctionCall{Name: "memory_search", Arguments: `{"q":"a"}`}},
			{ID: "call_2", Type: "function", Function: FunctionCall{Name: "memory_search", Arguments: `{"q":"b"}`}},
			{ID: "call_3", Type: "function", Function: FunctionCall{Name: "memory_search", Arguments: `{"q":"c"}`}},
		}},
		// Three separate tool-result messages — Bedrock requires
		// these to land in ONE user message.
		{Role: "tool", ToolCallID: "call_1", Content: "result-a"},
		{Role: "tool", ToolCallID: "call_2", Content: "result-b"},
		{Role: "tool", ToolCallID: "call_3", Content: "result-c"},
	}
	_, msgs, err := openAIMessagesToBedrock(in)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages (user, assistant, user-with-3-toolResults), got %d", len(msgs))
	}
	// msgs[2] must be a user message with 3 tool result blocks.
	if msgs[2].Role != bedrocktypes.ConversationRoleUser {
		t.Errorf("msgs[2] role: got %v", msgs[2].Role)
	}
	if len(msgs[2].Content) != 3 {
		t.Fatalf("msgs[2] content blocks: got %d, want 3 toolResult blocks", len(msgs[2].Content))
	}
	wantIDs := map[string]bool{"call_1": false, "call_2": false, "call_3": false}
	for _, b := range msgs[2].Content {
		tr, ok := b.(*bedrocktypes.ContentBlockMemberToolResult)
		if !ok {
			t.Fatalf("msgs[2] block is not a toolResult: %T", b)
		}
		id := stringOf(tr.Value.ToolUseId)
		if _, expected := wantIDs[id]; !expected {
			t.Errorf("unexpected tool result id %q", id)
		}
		wantIDs[id] = true
	}
	for id, seen := range wantIDs {
		if !seen {
			t.Errorf("missing tool result for %q", id)
		}
	}
}

// TestBuildUserContentBlocks_DocumentDataURL — happy path: a PDF
// data URL becomes a ContentBlockMemberDocument with the bytes
// inlined and the DocumentFormat picked from the MIME type.
func TestBuildUserContentBlocks_DocumentDataURL(t *testing.T) {
	// Tiny PDF header bytes; not a valid PDF but enough to
	// validate the inlining path.
	pdfBytes := []byte("%PDF-1.4\n%fake")
	dataURL := "data:application/pdf;base64," + base64.StdEncoding.EncodeToString(pdfBytes)
	m := Message{Role: "user", Blocks: []ContentBlock{
		{Type: "document_url", DocumentURL: &DocumentURLContent{URL: dataURL, Name: "spec.pdf"}},
	}}
	blocks := buildUserContentBlocks(m)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	doc, ok := blocks[0].(*bedrocktypes.ContentBlockMemberDocument)
	if !ok {
		t.Fatalf("block is not a Document: %T", blocks[0])
	}
	if doc.Value.Format != bedrocktypes.DocumentFormatPdf {
		t.Errorf("format: got %v, want pdf", doc.Value.Format)
	}
	if got := stringOf(doc.Value.Name); got != "spec.pdf" {
		t.Errorf("name: got %q", got)
	}
	src, ok := doc.Value.Source.(*bedrocktypes.DocumentSourceMemberBytes)
	if !ok || len(src.Value) == 0 {
		t.Fatalf("doc bytes empty or wrong source: %T", doc.Value.Source)
	}
}

// TestBuildUserContentBlocks_DocumentMIMEMap — every Bedrock-
// supported document MIME type round-trips through the converter.
// New types added by a future SDK should land here too.
func TestBuildUserContentBlocks_DocumentMIMEMap(t *testing.T) {
	cases := map[string]bedrocktypes.DocumentFormat{
		"application/pdf":    bedrocktypes.DocumentFormatPdf,
		"text/csv":           bedrocktypes.DocumentFormatCsv,
		"application/msword": bedrocktypes.DocumentFormatDoc,
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document": bedrocktypes.DocumentFormatDocx,
		"application/vnd.ms-excel": bedrocktypes.DocumentFormatXls,
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet": bedrocktypes.DocumentFormatXlsx,
		"text/html":     bedrocktypes.DocumentFormatHtml,
		"text/plain":    bedrocktypes.DocumentFormatTxt,
		"text/markdown": bedrocktypes.DocumentFormatMd,
	}
	for mime, want := range cases {
		dataURL := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString([]byte("hello"))
		m := Message{Role: "user", Blocks: []ContentBlock{
			{Type: "document_url", DocumentURL: &DocumentURLContent{URL: dataURL, Name: "x"}},
		}}
		blocks := buildUserContentBlocks(m)
		if len(blocks) != 1 {
			t.Errorf("%s: got %d blocks", mime, len(blocks))
			continue
		}
		doc, ok := blocks[0].(*bedrocktypes.ContentBlockMemberDocument)
		if !ok {
			t.Errorf("%s: not a doc block (got %T)", mime, blocks[0])
			continue
		}
		if doc.Value.Format != want {
			t.Errorf("%s: got format %v, want %v", mime, doc.Value.Format, want)
		}
	}
}

// TestBuildUserContentBlocks_DocumentRejectsUnsupported — an
// unknown MIME type lands as a fallback text note rather than
// silently drop. The model still sees the attempt was made.
func TestBuildUserContentBlocks_DocumentRejectsUnsupported(t *testing.T) {
	dataURL := "data:application/x-tar;base64," + base64.StdEncoding.EncodeToString([]byte("tarbytes"))
	m := Message{Role: "user", Blocks: []ContentBlock{
		{Type: "document_url", DocumentURL: &DocumentURLContent{URL: dataURL, Name: "x.tar"}},
	}}
	blocks := buildUserContentBlocks(m)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 fallback note, got %d", len(blocks))
	}
	tb, ok := blocks[0].(*bedrocktypes.ContentBlockMemberText)
	if !ok || !strings.Contains(tb.Value, "unsupported MIME") {
		t.Errorf("expected text fallback with 'unsupported MIME', got %T %q", blocks[0], textOf(blocks[0]))
	}
}

// TestBuildUserContentBlocks_RemoteImageFetch — http(s) image
// URLs go through the bounded fetch path. Server returns a real
// PNG with Content-Type; converter inlines the bytes.
func TestBuildUserContentBlocks_RemoteImageFetch(t *testing.T) {
	// 1x1 PNG bytes.
	pngBytes := []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
		0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
		0x89, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x44, 0x41,
		0x54, 0x78, 0x9c, 0x62, 0x00, 0x01, 0x00, 0x00,
		0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00,
		0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae,
		0x42, 0x60, 0x82,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngBytes)
	}))
	defer server.Close()
	m := Message{Role: "user", Blocks: []ContentBlock{
		{Type: "image_url", ImageURL: &ImageURLContent{URL: server.URL + "/img.png"}},
	}}
	blocks := buildUserContentBlocks(m)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	img, ok := blocks[0].(*bedrocktypes.ContentBlockMemberImage)
	if !ok {
		t.Fatalf("not an image block: %T", blocks[0])
	}
	if img.Value.Format != bedrocktypes.ImageFormatPng {
		t.Errorf("format: got %v, want png", img.Value.Format)
	}
	src := img.Value.Source.(*bedrocktypes.ImageSourceMemberBytes)
	if len(src.Value) != len(pngBytes) {
		t.Errorf("inlined bytes len: got %d, want %d", len(src.Value), len(pngBytes))
	}
}

// TestBuildUserContentBlocks_RemoteImageHTTPError — the bounded
// fetch path surfaces transport / HTTP errors as text fallback
// notes rather than failing the whole call.
func TestBuildUserContentBlocks_RemoteImageHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer server.Close()
	m := Message{Role: "user", Blocks: []ContentBlock{
		{Type: "image_url", ImageURL: &ImageURLContent{URL: server.URL + "/missing.png"}},
	}}
	blocks := buildUserContentBlocks(m)
	if len(blocks) != 1 {
		t.Fatalf("expected fallback note, got %d blocks", len(blocks))
	}
	tb := blocks[0].(*bedrocktypes.ContentBlockMemberText)
	if !strings.Contains(tb.Value, "Not Found") && !strings.Contains(tb.Value, "404") {
		t.Errorf("expected HTTP error in note, got %q", tb.Value)
	}
}

// TestBuildUserContentBlocks_RemoteImageOversize — bodies over
// the 10 MB cap are rejected with a fallback note. Defensive: a
// hostile / misconfigured server can't OOM the daemon.
func TestBuildUserContentBlocks_RemoteImageOversize(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		// Stream way more than the 10 MB cap.
		junk := make([]byte, 1024)
		for i := range junk {
			junk[i] = 0x42
		}
		// 12 MB total
		for i := 0; i < 12*1024; i++ {
			_, _ = w.Write(junk)
		}
	}))
	defer server.Close()
	m := Message{Role: "user", Blocks: []ContentBlock{
		{Type: "image_url", ImageURL: &ImageURLContent{URL: server.URL + "/big.png"}},
	}}
	blocks := buildUserContentBlocks(m)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 fallback note, got %d", len(blocks))
	}
	tb := blocks[0].(*bedrocktypes.ContentBlockMemberText)
	if !strings.Contains(tb.Value, "too large") && !strings.Contains(tb.Value, "exceeds") {
		t.Errorf("expected size-cap error in note, got %q", tb.Value)
	}
}

// TestOpenAIMessagesToBedrock_PreservesToolLoop — the four-turn
// tool-result loop (user → assistant-with-tool-call →
// tool-result → assistant-final-answer) must round-trip
// faithfully. Pre-2026-05-07 the converter dropped the
// role:"tool" message and let collapseSameRoleTurns merge the
// two consecutive assistants — but that destroyed the tool
// loop semantics, and the model re-called the same tool every
// iteration because it never saw its own result. New contract:
// tool messages become user-role ToolResult blocks, the
// assistant's tool_call gets serialised as a ToolUse block,
// and the four messages stay distinct (alternating roles).
func TestOpenAIMessagesToBedrock_PreservesToolLoop(t *testing.T) {
	in := []Message{
		{Role: "user", Content: "what time is it?"},
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "tooluse_1", Type: "function", Function: FunctionCall{Name: "current_time", Arguments: "{}"}},
		}},
		{Role: "tool", ToolCallID: "tooluse_1", Content: "2026-05-07T20:53:00Z"},
		{Role: "assistant", Content: "It's 2026-05-07 20:53 UTC."},
	}
	_, msgs, err := openAIMessagesToBedrock(in)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages (no collapse), got %d", len(msgs))
	}
	// Roles alternate: user → assistant → user(tool) → assistant.
	wantRoles := []bedrocktypes.ConversationRole{
		bedrocktypes.ConversationRoleUser,
		bedrocktypes.ConversationRoleAssistant,
		bedrocktypes.ConversationRoleUser,
		bedrocktypes.ConversationRoleAssistant,
	}
	for i, want := range wantRoles {
		if msgs[i].Role != want {
			t.Errorf("msgs[%d].Role: got %v, want %v", i, msgs[i].Role, want)
		}
	}
	// msgs[1] is the assistant's tool_use turn.
	tu, ok := msgs[1].Content[0].(*bedrocktypes.ContentBlockMemberToolUse)
	if !ok {
		t.Fatalf("msgs[1].Content[0] is not a ToolUse block: %T", msgs[1].Content[0])
	}
	if name := stringOf(tu.Value.Name); name != "current_time" {
		t.Errorf("tool use name: got %q", name)
	}
	// msgs[2] is the tool result.
	tr, ok := msgs[2].Content[0].(*bedrocktypes.ContentBlockMemberToolResult)
	if !ok {
		t.Fatalf("msgs[2].Content[0] is not a ToolResult block: %T", msgs[2].Content[0])
	}
	if id := stringOf(tr.Value.ToolUseId); id != "tooluse_1" {
		t.Errorf("tool result anchor: got %q", id)
	}
}

// TestOpenAIMessagesToBedrock_CollapsesConsecutiveSameRole — even
// after the tool-loop fix, the collapse function still serves a
// purpose for genuinely consecutive same-role turns (e.g. an
// adapter dropping an assistant turn, leaving two users in a row).
// Bedrock rejects two of the same role consecutively; the
// converter merges their text content so the wire shape is
// valid.
func TestOpenAIMessagesToBedrock_CollapsesConsecutiveSameRole(t *testing.T) {
	in := []Message{
		{Role: "user", Content: "first question"},
		{Role: "user", Content: "second question"},
	}
	_, msgs, err := openAIMessagesToBedrock(in)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message after collapse, got %d", len(msgs))
	}
	tb := msgs[0].Content[0].(*bedrocktypes.ContentBlockMemberText)
	if !strings.Contains(tb.Value, "first question") || !strings.Contains(tb.Value, "second question") {
		t.Errorf("merged text missing one half: %q", tb.Value)
	}
}

// stringOf is a one-line aws.ToString equivalent so the test file
// doesn't need to import the AWS sdk's helpers package twice.
func stringOf(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// TestBedrockOutputToChatResponse_ReasoningContent — Bedrock
// thinking-models (kimi-k2-thinking, Anthropic Claude with
// thinking budgets) emit a separate ContentBlockMemberReasoningContent
// alongside the visible text. The converter must extract the
// reasoning text into Message.ReasoningContent WITHOUT polluting
// Content — downstream parsers (gates, schema validators, the
// hallucination judge) only read Content and would silently
// regress if reasoning text leaked in.
func TestBedrockOutputToChatResponse_ReasoningContent(t *testing.T) {
	reasoningText := "Let me think... 2+2 is 4."
	out := &bedrocktypes.ConverseOutputMemberMessage{
		Value: bedrocktypes.Message{
			Role: bedrocktypes.ConversationRoleAssistant,
			Content: []bedrocktypes.ContentBlock{
				&bedrocktypes.ContentBlockMemberReasoningContent{
					Value: &bedrocktypes.ReasoningContentBlockMemberReasoningText{
						Value: bedrocktypes.ReasoningTextBlock{
							Text: aws.String(reasoningText),
						},
					},
				},
				&bedrocktypes.ContentBlockMemberText{Value: "4"},
			},
		},
	}
	resp := bedrockOutputToChatResponse(out, "kimi", bedrocktypes.StopReasonEndTurn, nil, "")
	if resp.Choices[0].Message.Content != "4" {
		t.Errorf("Content polluted by reasoning: got %q, want %q",
			resp.Choices[0].Message.Content, "4")
	}
	if resp.Choices[0].Message.ReasoningContent != reasoningText {
		t.Errorf("ReasoningContent: got %q, want %q",
			resp.Choices[0].Message.ReasoningContent, reasoningText)
	}
}

// TestBedrockOutputToChatResponse_RedactedReasoning — the encrypted
// CoT path: model returned a redacted block (raw bytes that aren't
// human-readable). Surfaces as a marker, not the raw bytes.
func TestBedrockOutputToChatResponse_RedactedReasoning(t *testing.T) {
	out := &bedrocktypes.ConverseOutputMemberMessage{
		Value: bedrocktypes.Message{
			Role: bedrocktypes.ConversationRoleAssistant,
			Content: []bedrocktypes.ContentBlock{
				&bedrocktypes.ContentBlockMemberReasoningContent{
					Value: &bedrocktypes.ReasoningContentBlockMemberRedactedContent{
						Value: []byte{0x01, 0x02, 0x03},
					},
				},
				&bedrocktypes.ContentBlockMemberText{Value: "answer"},
			},
		},
	}
	resp := bedrockOutputToChatResponse(out, "claude", bedrocktypes.StopReasonEndTurn, nil, "")
	if resp.Choices[0].Message.Content != "answer" {
		t.Errorf("Content: got %q, want %q", resp.Choices[0].Message.Content, "answer")
	}
	if resp.Choices[0].Message.ReasoningContent != "[redacted reasoning]" {
		t.Errorf("redacted reasoning marker: got %q", resp.Choices[0].Message.ReasoningContent)
	}
}

// TestBedrockOutputToChatResponse_MultipleReasoningBlocks — when
// the model emits multiple reasoning blocks (rare but possible),
// they concatenate with double-newline separators in
// ReasoningContent. Documents the precedence so a future change
// that switches to e.g. single-newline doesn't silently break
// observability dashboards that parse the field.
func TestBedrockOutputToChatResponse_MultipleReasoningBlocks(t *testing.T) {
	out := &bedrocktypes.ConverseOutputMemberMessage{
		Value: bedrocktypes.Message{
			Role: bedrocktypes.ConversationRoleAssistant,
			Content: []bedrocktypes.ContentBlock{
				&bedrocktypes.ContentBlockMemberReasoningContent{
					Value: &bedrocktypes.ReasoningContentBlockMemberReasoningText{
						Value: bedrocktypes.ReasoningTextBlock{Text: aws.String("step 1")},
					},
				},
				&bedrocktypes.ContentBlockMemberReasoningContent{
					Value: &bedrocktypes.ReasoningContentBlockMemberReasoningText{
						Value: bedrocktypes.ReasoningTextBlock{Text: aws.String("step 2")},
					},
				},
				&bedrocktypes.ContentBlockMemberText{Value: "done"},
			},
		},
	}
	resp := bedrockOutputToChatResponse(out, "test", bedrocktypes.StopReasonEndTurn, nil, "")
	want := "step 1\n\nstep 2"
	if resp.Choices[0].Message.ReasoningContent != want {
		t.Errorf("multi-block reasoning: got %q, want %q",
			resp.Choices[0].Message.ReasoningContent, want)
	}
}

// TestBedrockOutputToChatResponse_Basic — happy path: text content,
// usage tokens, end_turn stop reason → openai-shaped response.
func TestBedrockOutputToChatResponse_Basic(t *testing.T) {
	out := &bedrocktypes.ConverseOutputMemberMessage{
		Value: bedrocktypes.Message{
			Role: bedrocktypes.ConversationRoleAssistant,
			Content: []bedrocktypes.ContentBlock{
				&bedrocktypes.ContentBlockMemberText{Value: "hello"},
			},
		},
	}
	usage := &bedrocktypes.TokenUsage{
		InputTokens:  aws.Int32(42),
		OutputTokens: aws.Int32(7),
		TotalTokens:  aws.Int32(49),
	}
	resp := bedrockOutputToChatResponse(out, "test-model", bedrocktypes.StopReasonEndTurn, usage, "req-1")
	if resp.Model != "test-model" {
		t.Errorf("model: got %q", resp.Model)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices: got %d", len(resp.Choices))
	}
	if resp.Choices[0].Message.Content != "hello" {
		t.Errorf("content: got %q", resp.Choices[0].Message.Content)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason: got %q", resp.Choices[0].FinishReason)
	}
	if resp.Usage.PromptTokens != 42 || resp.Usage.CompletionTokens != 7 || resp.Usage.TotalTokens != 49 {
		t.Errorf("usage: got %+v", resp.Usage)
	}
}

// TestBedrockStopReasonMapping — the OpenAI-compat finish_reason
// labels MUST stay stable; downstream gates and dashboards key off
// them. Catches a future SDK enum change that would silently shift
// the mapping.
func TestBedrockStopReasonMapping(t *testing.T) {
	cases := map[bedrocktypes.StopReason]string{
		bedrocktypes.StopReasonEndTurn:             "stop",
		bedrocktypes.StopReasonStopSequence:        "stop",
		bedrocktypes.StopReasonMaxTokens:           "length",
		bedrocktypes.StopReasonContentFiltered:     "content_filter",
		bedrocktypes.StopReasonToolUse:             "tool_calls",
		bedrocktypes.StopReasonGuardrailIntervened: "stop",
	}
	for in, want := range cases {
		got := bedrockStopReasonToOpenAI(in)
		if got != want {
			t.Errorf("%s → got %q, want %q", in, got, want)
		}
	}
	if bedrockStopReasonToOpenAI(bedrocktypes.StopReason("unknown_future_value")) != "stop" {
		t.Error("unknown stop reason should default to \"stop\"")
	}
}

// TestOpenAIToolsToBedrock_BasicFunction — happy path: a single
// function tool with a JSON-Schema parameters block translates into
// a ToolMemberToolSpec carrying name + description + the schema as
// a smithy lazy document. Catches future SDK changes that break the
// document constructor wiring.
func TestOpenAIToolsToBedrock_BasicFunction(t *testing.T) {
	tools := []Tool{
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "get_quote",
				Description: "Fetch the latest quote for a symbol.",
				Parameters: []byte(`{
					"type": "object",
					"properties": {"symbol": {"type": "string"}},
					"required": ["symbol"]
				}`),
			},
		},
	}
	out, err := openAIToolsToBedrock(tools)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(out))
	}
	spec, ok := out[0].(*bedrocktypes.ToolMemberToolSpec)
	if !ok {
		t.Fatalf("tool[0] is not a ToolMemberToolSpec: %T", out[0])
	}
	if got := aws.ToString(spec.Value.Name); got != "get_quote" {
		t.Errorf("name: got %q", got)
	}
	if got := aws.ToString(spec.Value.Description); got != "Fetch the latest quote for a symbol." {
		t.Errorf("description: got %q", got)
	}
	jsonSchema, ok := spec.Value.InputSchema.(*bedrocktypes.ToolInputSchemaMemberJson)
	if !ok || jsonSchema.Value == nil {
		t.Fatalf("input schema is not a JSON document: %T", spec.Value.InputSchema)
	}
}

// TestOpenAIToolsToBedrock_EmptyReturnsNil — caller convenience: a
// nil/empty tool list returns (nil, nil) so callers can leave
// ConverseInput.ToolConfig untouched without a special-case branch.
func TestOpenAIToolsToBedrock_EmptyReturnsNil(t *testing.T) {
	out, err := openAIToolsToBedrock(nil)
	if err != nil || out != nil {
		t.Errorf("nil input: got (%v, %v), want (nil, nil)", out, err)
	}
	out, err = openAIToolsToBedrock([]Tool{})
	if err != nil || out != nil {
		t.Errorf("empty input: got (%v, %v), want (nil, nil)", out, err)
	}
}

// TestOpenAIToolsToBedrock_NonObjectSchemaWrapped — Bedrock requires
// the top-level schema to be an object. A bare-string-schema (legal
// in JSON Schema but uncommon in OpenAI tools) gets wrapped under a
// {value: <schema>} convention so the SDK's strict validation
// accepts the request.
func TestOpenAIToolsToBedrock_NonObjectSchemaWrapped(t *testing.T) {
	tools := []Tool{
		{
			Type: "function",
			Function: ToolFunction{
				Name:       "echo",
				Parameters: []byte(`{"type": "string"}`),
			},
		},
	}
	out, err := openAIToolsToBedrock(tools)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	spec := out[0].(*bedrocktypes.ToolMemberToolSpec)
	js := spec.Value.InputSchema.(*bedrocktypes.ToolInputSchemaMemberJson)
	raw, err := js.Value.MarshalSmithyDocument()
	if err != nil {
		t.Fatalf("marshal smithy doc: %v", err)
	}
	var unmarshalled map[string]any
	if err := json.Unmarshal(raw, &unmarshalled); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}
	if unmarshalled["type"] != "object" {
		t.Errorf("non-object schema not wrapped: %v", unmarshalled)
	}
	props, ok := unmarshalled["properties"].(map[string]any)
	if !ok || props["value"] == nil {
		t.Errorf("missing wrapped value property: %v", unmarshalled)
	}
}

// TestOpenAIToolsToBedrock_RequiresName — a tool spec without a name
// is malformed; fail fast at the converter so the SDK doesn't
// reject the whole batch with an opaque message.
func TestOpenAIToolsToBedrock_RequiresName(t *testing.T) {
	tools := []Tool{{Type: "function", Function: ToolFunction{Name: ""}}}
	_, err := openAIToolsToBedrock(tools)
	if err == nil {
		t.Fatal("expected error for missing name")
	}
}

// TestOpenAIToolsToBedrock_SkipsNonFunctionType — forward-compat: a
// future tool type that isn't "function" is silently skipped, not
// a hard failure, so a single odd tool entry doesn't kill the rest.
func TestOpenAIToolsToBedrock_SkipsNonFunctionType(t *testing.T) {
	tools := []Tool{
		{Type: "future_thing", Function: ToolFunction{Name: "x"}},
		{Type: "function", Function: ToolFunction{Name: "real",
			Parameters: []byte(`{"type":"object","properties":{}}`)}},
	}
	out, err := openAIToolsToBedrock(tools)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 tool kept, got %d", len(out))
	}
	spec := out[0].(*bedrocktypes.ToolMemberToolSpec)
	if aws.ToString(spec.Value.Name) != "real" {
		t.Errorf("wrong tool kept: %v", aws.ToString(spec.Value.Name))
	}
}

// TestExtractToolCallsFromContent_Basic — happy path: a
// ContentBlockMemberToolUse round-trips into a ToolCall with the
// tool ID, name, and JSON-string arguments.
func TestExtractToolCallsFromContent_Basic(t *testing.T) {
	blocks := []bedrocktypes.ContentBlock{
		&bedrocktypes.ContentBlockMemberText{Value: "let me check"},
		&bedrocktypes.ContentBlockMemberToolUse{
			Value: bedrocktypes.ToolUseBlock{
				Name:      aws.String("get_quote"),
				ToolUseId: aws.String("call_abc"),
				Input:     bedrockdoc.NewLazyDocument(map[string]any{"symbol": "AAPL"}),
			},
		},
	}
	calls, _, err := extractToolCallsFromContent(blocks)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	if calls[0].ID != "call_abc" {
		t.Errorf("id: got %q", calls[0].ID)
	}
	if calls[0].Type != "function" {
		t.Errorf("type: got %q", calls[0].Type)
	}
	if calls[0].Function.Name != "get_quote" {
		t.Errorf("name: got %q", calls[0].Function.Name)
	}
	// Arguments is a JSON-encoded string per the OpenAI shape.
	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments not valid JSON: %v / %q", err, calls[0].Function.Arguments)
	}
	if args["symbol"] != "AAPL" {
		t.Errorf("argument value: got %v", args["symbol"])
	}
}

// TestExtractToolCallsFromContent_RescuesHallucinatedName — the
// 2026-05-12 minimax.minimax-m2.5 regression: the model packed its
// final JSON answer into ToolUse.Name instead of a content block.
// extractToolCallsFromContent must NOT surface that as a real call
// (which would silently 404 in the dispatcher and burn the
// iteration budget) — it must return the offending name in
// rescuedContent so the caller can append it to the assistant text.
// The hallucination detector still flags the shape downstream.
func TestExtractToolCallsFromContent_RescuesHallucinatedName(t *testing.T) {
	hallucinated := `{"outcome":"continue","plan":{"steps":["researcher"],"rationale":"..."}}`
	blocks := []bedrocktypes.ContentBlock{
		&bedrocktypes.ContentBlockMemberText{Value: "ok"},
		&bedrocktypes.ContentBlockMemberToolUse{
			Value: bedrocktypes.ToolUseBlock{
				Name:      aws.String(hallucinated),
				ToolUseId: aws.String("call_bad"),
				Input:     bedrockdoc.NewLazyDocument(map[string]any{}),
			},
		},
	}
	calls, rescued, err := extractToolCallsFromContent(blocks)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("hallucinated ToolUse must NOT surface as a real call; got %d", len(calls))
	}
	if len(rescued) != 1 || rescued[0] != hallucinated {
		t.Fatalf("expected rescued content to carry the original name verbatim; got %#v", rescued)
	}
}

// TestExtractToolCallsFromContent_MixedRealAndHallucinated — a turn
// that includes both a legit tool call AND a hallucinated one must
// surface the legit one as a real call and the bad one as rescued
// content. The dispatcher then dispatches the real tool; the model's
// rescued JSON becomes content available alongside it.
func TestExtractToolCallsFromContent_MixedRealAndHallucinated(t *testing.T) {
	hallucinated := `{"outcome":"continue"}`
	blocks := []bedrocktypes.ContentBlock{
		&bedrocktypes.ContentBlockMemberToolUse{
			Value: bedrocktypes.ToolUseBlock{
				Name:      aws.String("file_read"),
				ToolUseId: aws.String("call_real"),
				Input:     bedrockdoc.NewLazyDocument(map[string]any{"path": "x.md"}),
			},
		},
		&bedrocktypes.ContentBlockMemberToolUse{
			Value: bedrocktypes.ToolUseBlock{
				Name:      aws.String(hallucinated),
				ToolUseId: aws.String("call_bad"),
				Input:     bedrockdoc.NewLazyDocument(map[string]any{}),
			},
		},
	}
	calls, rescued, err := extractToolCallsFromContent(blocks)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(calls) != 1 || calls[0].Function.Name != "file_read" {
		t.Fatalf("legit tool call dropped or wrong: %+v", calls)
	}
	if len(rescued) != 1 || rescued[0] != hallucinated {
		t.Fatalf("bad call not rescued: %#v", rescued)
	}
}

// TestLooksLikeHallucinatedToolName covers the shapes the predicate
// must catch — and the shapes it must NOT catch (snake_case,
// dot.namespaced, hyphen-cased identifiers are all legitimate
// tool-name shapes in this codebase).
func TestLooksLikeHallucinatedToolName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		// Legit identifiers — must NOT trip.
		{"file_read", false},
		{"mcp__broker__place_order", false},
		{"mcp__scraper__web_fetch", false},
		{"current_time", false},
		{"create_task", false},
		{"", false},
		// Hallucinated shapes — must trip.
		{`{"outcome":"continue"}`, true},
		{`function_calls\n<invoke>`, true},
		{"echo hello | grep x", true},
		{"<tool_call>plan</tool_call>", true},
		{"create_task; rm -rf /", true},
	}
	for _, c := range cases {
		got := looksLikeHallucinatedToolName(c.name)
		if got != c.want {
			t.Errorf("looksLikeHallucinatedToolName(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestExtractToolCallsFromContent_TextOnly — a response with no
// ToolUseBlock returns nil, no error. The dispatcher's tool-call
// loop keys off len(ToolCalls) > 0, so empty/nil are equivalent.
func TestExtractToolCallsFromContent_TextOnly(t *testing.T) {
	blocks := []bedrocktypes.ContentBlock{
		&bedrocktypes.ContentBlockMemberText{Value: "no tools today"},
	}
	calls, _, err := extractToolCallsFromContent(blocks)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(calls) != 0 {
		t.Errorf("expected 0 calls, got %d", len(calls))
	}
}

// TestFlattenMessageText_PrefersContent — when both Content and
// Blocks are populated (a caller bug, but observable), the flat
// Content string wins. Documents the precedence so future callers
// don't get surprised.
func TestFlattenMessageText_PrefersContent(t *testing.T) {
	m := Message{
		Content: "flat content",
		Blocks: []ContentBlock{
			TextBlock("block content"),
		},
	}
	if got := flattenMessageText(m); got != "flat content" {
		t.Errorf("expected flat content to win, got %q", got)
	}
}
