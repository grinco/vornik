package chat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	bedrockdoc "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	bedrocktypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

// base64StdDecoder is a thin wrapper so the call sites can keep
// the rest of encoding/base64 out of their import sets.
func base64StdDecoder() *base64.Encoding { return base64.StdEncoding }

// openAIMessagesToBedrock translates the OpenAI-shaped chat history
// into Bedrock Converse's (system, messages) split. The Converse API
// requires:
//
//   - The system content lives in a separate `system` array, NOT
//     interleaved with the conversation.
//   - Each conversation message has role ∈ {user, assistant}; tool
//     results land as `user`-role messages with toolResult content
//     blocks (tool calls land as `assistant` with toolUse blocks).
//
// Phase 1 scope: text content only — multimodal blocks (image_url) and
// tool blocks (toolUse / toolResult) are silently dropped with a
// caller-visible note in the converted text. Phase 2 wires those.
//
// Returns (system, messages, err). Errors only on structurally
// impossible inputs (e.g. empty messages list); content-shape edge
// cases get coerced rather than rejected so a single odd block
// doesn't fail the whole call.
// openAIMessagesToBedrockWithCache is the cache-aware variant of
// openAIMessagesToBedrock. When strategy is non-nil and Mode is
// non-off, it appends a SystemContentBlockMemberCachePoint after
// the last system message that should be cached (auto mode: last
// system message; prefix mode: last message with CachePrefix
// true). Cache markers in the body-message stream
// (ContentBlockMemberCachePoint) aren't emitted today — the
// headline use-case is per-role system prompts whose stable
// prefix sits entirely in the system block.
//
// Bedrock's CachePointBlock TTL is fixed at the provider's
// default (5 min); strategy.TTL is informational at this layer
// (logged via observability, no wire-level effect).
func openAIMessagesToBedrockWithCache(in []Message, strategy *CacheStrategy, model string) ([]bedrocktypes.SystemContentBlock, []bedrocktypes.Message, error) {
	system, messages, err := openAIMessagesToBedrock(in)
	if err != nil || strategy == nil || !cacheStrategyActive(strategy) {
		return system, messages, err
	}
	if bedrockModelSupportsCaching(model) && shouldCacheSystem(in, strategy) && len(system) > 0 {
		system = append(system, &bedrocktypes.SystemContentBlockMemberCachePoint{
			Value: bedrocktypes.CachePointBlock{
				Type: bedrocktypes.CachePointTypeDefault,
			},
		})
	}
	return system, messages, nil
}

// bedrockModelSupportsCaching reports whether a Bedrock model id accepts
// CachePoint blocks. Bedrock prompt caching is limited to the Anthropic Claude
// and Amazon Nova families; sending a CachePoint to any other model is rejected
// upstream with PROVIDER_ERROR. This swarm routes many marketplace OSS models
// via kind:bedrock (zai.*, moonshotai.*, qwen.*, deepseek.*, nvidia.*) — none of
// which support cache points. Incident 2026-06-13: prompt_cache_mode=auto added
// a CachePoint unconditionally and broke the headmatch issue-fix reviewer on
// kimi-k2.5 (+ glm-5 fallback) — all 12 retries failed PROVIDER_ERROR.
//
// Conservative by design: an unrecognised model gets NO cache point (a safe
// no-op), never a guess that could re-trigger the provider error. Substring
// match tolerates region-prefixed inference-profile ids
// (us.anthropic.claude-..., eu.amazon.nova-...).
func bedrockModelSupportsCaching(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	return strings.Contains(m, "anthropic.claude") || strings.Contains(m, "amazon.nova")
}

// cacheStrategyActive reports whether the strategy turns caching
// on. Off / empty mode are both no-ops.
func cacheStrategyActive(s *CacheStrategy) bool {
	if s == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(s.Mode)) {
	case "", CacheModeOff:
		return false
	}
	return true
}

// shouldCacheSystemFromInput is the shared cache-decision helper
// for both Bedrock (Phase B) and Anthropic via claude_subscription
// (Phase C). Exported within the package so the
// claude_subscription_convert.go path can consult it without
// re-implementing the auto/prefix logic.
//
// Returns true when:
//   - strategy.Mode == "auto" AND at least one system message
//     exists (auto-mark the headline use-case), OR
//   - strategy.Mode == "prefix" AND at least one message with
//     CachePrefix=true is a system message (caller opt-in).
//
// The granular per-message position (cache marker inside body
// messages) is out of scope for the first MVP slice.
func shouldCacheSystemFromInput(in []Message, strategy *CacheStrategy) bool {
	if !cacheStrategyActive(strategy) {
		return false
	}
	hasSystem := false
	hasSystemPrefix := false
	for _, m := range in {
		if m.Role == "system" {
			hasSystem = true
			if m.CachePrefix {
				hasSystemPrefix = true
			}
		}
	}
	switch strings.ToLower(strings.TrimSpace(strategy.Mode)) {
	case CacheModeAuto:
		return hasSystem
	case CacheModePrefix:
		return hasSystemPrefix
	}
	return false
}

// shouldCacheSystem keeps the Bedrock-only entry point at its
// original name to avoid churn in the Bedrock converter; it now
// delegates to shouldCacheSystemFromInput.
func shouldCacheSystem(in []Message, strategy *CacheStrategy) bool {
	return shouldCacheSystemFromInput(in, strategy)
}

func openAIMessagesToBedrock(in []Message) ([]bedrocktypes.SystemContentBlock, []bedrocktypes.Message, error) {
	if len(in) == 0 {
		return nil, nil, fmt.Errorf("bedrock: cannot send empty messages list")
	}

	var system []bedrocktypes.SystemContentBlock
	var messages []bedrocktypes.Message

	for _, m := range in {
		switch m.Role {
		case "system":
			text := flattenMessageText(m)
			if text == "" {
				continue
			}
			system = append(system, &bedrocktypes.SystemContentBlockMemberText{Value: text})

		case "user":
			content := buildUserContentBlocks(m)
			if len(content) == 0 {
				continue
			}
			messages = append(messages, bedrocktypes.Message{
				Role:    bedrocktypes.ConversationRoleUser,
				Content: content,
			})

		case "assistant":
			content := buildAssistantContentBlocks(m)
			if len(content) == 0 {
				continue
			}
			messages = append(messages, bedrocktypes.Message{
				Role:    bedrocktypes.ConversationRoleAssistant,
				Content: content,
			})

		case "tool":
			// Tool results land as a USER-role message with
			// ContentBlockMemberToolResult — that's how Bedrock's
			// Converse API models the multi-turn tool loop. Without
			// this, a model that called a tool re-calls the same
			// tool every iteration because it never sees its own
			// result, and the agent's degenerate-loop detector
			// trips after 3 identical calls (observed 2026-05-07
			// when default:bedrock made the strategist's
			// tool-loop traffic land here).
			//
			// CRITICAL: when the assistant emitted N parallel
			// tool_use blocks, Bedrock REQUIRES the next user
			// message to contain all N matching toolResult blocks
			// in a single message. Splitting them across multiple
			// user messages produces:
			//   ValidationException: Expected toolResult blocks at
			//   messages.<N>.content for the following Ids: <id>
			// (observed 2026-05-07 — kimi-k2.5 emits parallel
			// memory_search calls + the converter previously
			// produced one user message per tool result, and
			// Bedrock 400'd every multi-tool iteration).
			//
			// Append the toolResult to the previous message when
			// it's already a user-role tool-result message; start
			// a new message otherwise.
			tr := buildToolResultBlock(m)
			if tr == nil {
				continue
			}
			if len(messages) > 0 && messages[len(messages)-1].Role == bedrocktypes.ConversationRoleUser && isAllToolResults(messages[len(messages)-1].Content) {
				prev := &messages[len(messages)-1]
				prev.Content = append(prev.Content, tr)
				continue
			}
			messages = append(messages, bedrocktypes.Message{
				Role:    bedrocktypes.ConversationRoleUser,
				Content: []bedrocktypes.ContentBlock{tr},
			})

		default:
			// Unknown role — coerce to user with a prefix so the
			// content isn't lost entirely.
			text := flattenMessageText(m)
			if text == "" {
				continue
			}
			messages = append(messages, bedrocktypes.Message{
				Role: bedrocktypes.ConversationRoleUser,
				Content: []bedrocktypes.ContentBlock{
					&bedrocktypes.ContentBlockMemberText{Value: "[" + m.Role + "] " + text},
				},
			})
		}
	}

	// Bedrock requires alternating user/assistant turns starting with
	// user. Collapse consecutive same-role turns by joining their text
	// with two newlines so the wire shape is valid.
	messages = collapseSameRoleTurns(messages)

	if len(messages) == 0 {
		return nil, nil, fmt.Errorf("bedrock: no user/assistant messages after conversion (input had %d messages, all system or empty)", len(in))
	}

	return system, messages, nil
}

// buildUserContentBlocks renders a user-role Message into the
// Bedrock content-block array. Recognised input types:
//
//   - text          — straight text
//   - image_url     — data: URL (inlined), http/https URL (fetched),
//     or anything else (text fallback note)
//   - document_url  — data: URL only; Bedrock requires inline bytes
//     for documents (no remote fetch yet — operators
//     who need PDFs from the web should fetch upstream
//     and pass the bytes as a data: URL)
//
// Unknown / malformed blocks fall through to a text fallback note
// rather than silently drop, so the model still sees there was
// meant to be an attachment.
func buildUserContentBlocks(m Message) []bedrocktypes.ContentBlock {
	var blocks []bedrocktypes.ContentBlock
	if m.Content != "" {
		blocks = append(blocks, &bedrocktypes.ContentBlockMemberText{Value: m.Content})
	}
	for _, b := range m.Blocks {
		switch b.Type {
		case "text":
			if b.Text == "" {
				continue
			}
			blocks = append(blocks, &bedrocktypes.ContentBlockMemberText{Value: b.Text})

		case "image_url":
			if b.ImageURL == nil {
				continue
			}
			img, note := imageURLToBedrock(b.ImageURL.URL)
			if img != nil {
				blocks = append(blocks, img)
				continue
			}
			if note != "" {
				blocks = append(blocks, &bedrocktypes.ContentBlockMemberText{Value: note})
			}

		case "document_url":
			if b.DocumentURL == nil {
				continue
			}
			doc, note := dataURLToBedrockDocument(b.DocumentURL.URL, b.DocumentURL.Name)
			if doc != nil {
				blocks = append(blocks, doc)
				continue
			}
			if note != "" {
				blocks = append(blocks, &bedrocktypes.ContentBlockMemberText{Value: note})
			}
		}
	}
	return blocks
}

// buildAssistantContentBlocks renders an assistant-role Message
// into Bedrock content blocks, including the assistant's own
// prior tool calls so a tool-loop conversation round-trips
// correctly. Bedrock's Converse API requires the assistant turn
// that requested a tool to carry the matching ToolUseBlock —
// without it, the next user turn's ToolResult has no anchor and
// the API rejects the conversation.
//
// An assistant with NO text and NO tool_calls produces no
// content blocks; the caller skips the message entirely (Bedrock
// rejects empty-content turns). Same shape as the user-side
// builder for symmetry.
func buildAssistantContentBlocks(m Message) []bedrocktypes.ContentBlock {
	var blocks []bedrocktypes.ContentBlock
	text := flattenMessageText(m)
	if text != "" {
		blocks = append(blocks, &bedrocktypes.ContentBlockMemberText{Value: text})
	}
	for _, tc := range m.ToolCalls {
		if tc.Function.Name == "" {
			continue
		}
		// Decode the arguments JSON-string into a Go value so the
		// SDK encoder produces a real JSON object (not a stringified
		// blob — Bedrock 400s on the latter, same constraint as the
		// tool-spec input schema).
		var args any = map[string]any{}
		if tc.Function.Arguments != "" {
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
				// Malformed arguments — fall back to an empty
				// object so the wire shape stays valid. The model
				// will see a tool call with no inputs, but at
				// least the conversation continues.
				args = map[string]any{}
			}
		}
		id := tc.ID
		if id == "" {
			// Bedrock requires a non-empty ToolUseId. The OpenAI
			// shape's ID is usually a server-generated string;
			// when missing, synthesise one so the matching
			// ToolResult can reference it.
			id = "tooluse_" + tc.Function.Name
		}
		blocks = append(blocks, &bedrocktypes.ContentBlockMemberToolUse{
			Value: bedrocktypes.ToolUseBlock{
				ToolUseId: aws.String(id),
				Name:      aws.String(tc.Function.Name),
				Input:     bedrockdoc.NewLazyDocument(args),
			},
		})
	}
	return blocks
}

// buildToolResultBlock translates a role:"tool" Message into a
// Bedrock ContentBlockMemberToolResult. Returns nil when the
// message lacks a tool_call_id (which Bedrock requires to anchor
// the result to a prior toolUse block).
func buildToolResultBlock(m Message) bedrocktypes.ContentBlock {
	toolUseID := m.ToolCallID
	if toolUseID == "" {
		// Pre-fix this returned nil — Bedrock would then receive an
		// assistant turn whose toolUse block had no matching
		// toolResult block, producing a 400 ValidationException.
		// Synthesize a stable fallback ID from the tool name so the
		// result still anchors to a turn. Mirrors the same fallback
		// in buildAssistantContentBlocks ("tooluse_<name>") so an
		// assistant turn that had its ID dropped in transit and a
		// subsequent tool-result with an empty ID still pair up
		// correctly. Anonymous tool results (no name either) get a
		// generic "anonymous" tag — Bedrock accepts the conversation
		// but the model sees that the call is malformed and can
		// re-plan.
		if m.Name != "" {
			toolUseID = "tooluse_" + m.Name
		} else {
			toolUseID = "tooluse_anonymous"
		}
	}
	text := flattenMessageText(m)
	if text == "" {
		text = "(empty tool result)"
	}
	return &bedrocktypes.ContentBlockMemberToolResult{
		Value: bedrocktypes.ToolResultBlock{
			ToolUseId: aws.String(toolUseID),
			Content: []bedrocktypes.ToolResultContentBlock{
				&bedrocktypes.ToolResultContentBlockMemberText{Value: text},
			},
		},
	}
}

// imageURLToBedrock dispatches an image URL to the right inline
// path: data: URLs decode locally; http(s) URLs get a bounded
// in-process fetch (5s timeout, 10MB cap, MIME-type sniff against
// the Bedrock-supported set). Returns (nil, note) for anything
// that can't be inlined; the note surfaces in the prompt so the
// model still sees the model wasn't ignored.
//
// Bedrock Converse needs inline bytes (or an S3 ARN we don't have
// reach to construct). Doing the fetch in-process keeps the
// architecture contained — operators don't need to wire an
// upstream image proxy — at the cost of a small SSRF surface for
// the daemon. The size + timeout caps bound the blast radius;
// operators with stricter requirements can pre-encode to data:
// URLs upstream and skip the fetch path.
func imageURLToBedrock(url string) (bedrocktypes.ContentBlock, string) {
	if strings.HasPrefix(url, "data:") {
		return dataURLToBedrockImage(url)
	}
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		return fetchAndInlineImage(url)
	}
	return nil, "[image: unsupported URL scheme — only data: / http: / https: are honoured]"
}

// dataURLToBedrockImage decodes a data: URL
// (data:image/<fmt>;base64,<payload>) into a Bedrock
// ContentBlockMemberImage with the bytes inlined. Returns
// (nil, note) when the URL isn't a data: URL or the format isn't
// one Bedrock accepts; the note becomes a text block so the model
// still sees something.
//
// Bedrock supports png, jpeg, gif, webp; everything else gets a
// pass-through note.
func dataURLToBedrockImage(url string) (bedrocktypes.ContentBlock, string) {
	const prefix = "data:"
	if !strings.HasPrefix(url, prefix) {
		return nil, "[image: not a data: URL]"
	}
	rest := url[len(prefix):]
	commaIdx := strings.IndexByte(rest, ',')
	if commaIdx < 0 {
		return nil, "[image: malformed data URL]"
	}
	header := rest[:commaIdx]
	payload := rest[commaIdx+1:]
	if !strings.Contains(header, ";base64") {
		return nil, "[image: data URL is not base64-encoded]"
	}
	mediaType := strings.TrimSuffix(header, ";base64")
	format, ok := mimeToImageFormat(mediaType)
	if !ok {
		return nil, "[image: unsupported MIME type " + mediaType + "; Bedrock accepts png/jpeg/gif/webp]"
	}
	decoded, err := base64DecodeStd(payload)
	if err != nil {
		return nil, "[image: base64 decode failed: " + err.Error() + "]"
	}
	if len(decoded) == 0 {
		return nil, "[image: empty payload]"
	}
	return &bedrocktypes.ContentBlockMemberImage{
		Value: bedrocktypes.ImageBlock{
			Format: format,
			Source: &bedrocktypes.ImageSourceMemberBytes{Value: decoded},
		},
	}, ""
}

// fetchAndInlineImage downloads a remote image with bounded cost
// (5s timeout, 10MB body cap) and returns it as an inline-bytes
// Bedrock content block. Failures surface as text-fallback notes
// so a slow / 404 / oversized image doesn't kill the whole turn.
//
// Operator-facing safety:
//   - Timeout caps the wait — a hung server can't stall the chat.
//   - Body cap bounds the memory hit (a 10 MB image uses 13 MB
//     base64 + 10 MB raw; well within the daemon's footprint).
//   - Content-Type drives the format selection — server's claimed
//     type wins over a guess off the URL path.
//   - No redirect filtering; trust operators to sanitise URLs
//     upstream. SSRF mitigation is out of scope for this helper.
var imageFetchHTTPClient = &http.Client{Timeout: 5 * time.Second}

const imageFetchMaxBytes = 10 * 1024 * 1024 // 10 MB

func fetchAndInlineImage(url string) (bedrocktypes.ContentBlock, string) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, "[image: malformed URL " + truncateForLog(url, 80) + ": " + err.Error() + "]"
	}
	resp, err := imageFetchHTTPClient.Do(req)
	if err != nil {
		return nil, "[image: fetch " + truncateForLog(url, 80) + " failed: " + truncateForLog(err.Error(), 60) + "]"
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "[image: fetch " + truncateForLog(url, 80) + " returned HTTP " + http.StatusText(resp.StatusCode) + "]"
	}
	mediaType := resp.Header.Get("Content-Type")
	// Strip any "; charset=…" suffix.
	if idx := strings.IndexByte(mediaType, ';'); idx >= 0 {
		mediaType = strings.TrimSpace(mediaType[:idx])
	}
	format, ok := mimeToImageFormat(mediaType)
	if !ok {
		return nil, "[image: " + truncateForLog(url, 60) + " has unsupported Content-Type " + mediaType + "; Bedrock accepts png/jpeg/gif/webp]"
	}
	body, err := readWithCap(resp.Body, imageFetchMaxBytes)
	if err != nil {
		return nil, "[image: fetch body too large or read failed: " + truncateForLog(err.Error(), 80) + "]"
	}
	if len(body) == 0 {
		return nil, "[image: empty body]"
	}
	return &bedrocktypes.ContentBlockMemberImage{
		Value: bedrocktypes.ImageBlock{
			Format: format,
			Source: &bedrocktypes.ImageSourceMemberBytes{Value: body},
		},
	}, ""
}

// mimeToImageFormat normalises a Content-Type / data-URL media
// type to a Bedrock ImageFormat. Returns (zero, false) for
// unsupported types so the caller emits a fallback note.
func mimeToImageFormat(mediaType string) (bedrocktypes.ImageFormat, bool) {
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "image/png":
		return bedrocktypes.ImageFormatPng, true
	case "image/jpeg", "image/jpg":
		return bedrocktypes.ImageFormatJpeg, true
	case "image/gif":
		return bedrocktypes.ImageFormatGif, true
	case "image/webp":
		return bedrocktypes.ImageFormatWebp, true
	default:
		return "", false
	}
}

// dataURLToBedrockDocument decodes a data: URL carrying a non-image
// attachment (PDF / DOCX / CSV / TXT / MD / HTML / XLS / XLSX /
// DOC) into a Bedrock ContentBlockMemberDocument. Bedrock's
// DocumentBlock requires a Name (operator-supplied, surfaced to
// the model — keep it neutral; the field is vulnerable to prompt
// injection per AWS's docs). When name is empty, falls back to
// "document".
func dataURLToBedrockDocument(url, name string) (bedrocktypes.ContentBlock, string) {
	const prefix = "data:"
	if !strings.HasPrefix(url, prefix) {
		return nil, "[document: only data: URLs supported (operators with remote PDFs should fetch + base64-encode upstream)]"
	}
	rest := url[len(prefix):]
	commaIdx := strings.IndexByte(rest, ',')
	if commaIdx < 0 {
		return nil, "[document: malformed data URL]"
	}
	header := rest[:commaIdx]
	payload := rest[commaIdx+1:]
	if !strings.Contains(header, ";base64") {
		return nil, "[document: data URL is not base64-encoded]"
	}
	mediaType := strings.TrimSuffix(header, ";base64")
	format, ok := mimeToDocumentFormat(mediaType)
	if !ok {
		return nil, "[document: unsupported MIME type " + mediaType + "; Bedrock accepts pdf/csv/doc/docx/xls/xlsx/html/txt/md]"
	}
	decoded, err := base64DecodeStd(payload)
	if err != nil {
		return nil, "[document: base64 decode failed: " + err.Error() + "]"
	}
	if len(decoded) == 0 {
		return nil, "[document: empty payload]"
	}
	docName := strings.TrimSpace(name)
	if docName == "" {
		docName = "document"
	}
	return &bedrocktypes.ContentBlockMemberDocument{
		Value: bedrocktypes.DocumentBlock{
			Name:   aws.String(docName),
			Format: format,
			Source: &bedrocktypes.DocumentSourceMemberBytes{Value: decoded},
		},
	}, ""
}

// mimeToDocumentFormat normalises a media type to a Bedrock
// DocumentFormat enum. Mirrors the SDK's
// types.DocumentFormat constants.
func mimeToDocumentFormat(mediaType string) (bedrocktypes.DocumentFormat, bool) {
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "application/pdf":
		return bedrocktypes.DocumentFormatPdf, true
	case "text/csv":
		return bedrocktypes.DocumentFormatCsv, true
	case "application/msword":
		return bedrocktypes.DocumentFormatDoc, true
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return bedrocktypes.DocumentFormatDocx, true
	case "application/vnd.ms-excel":
		return bedrocktypes.DocumentFormatXls, true
	case "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		return bedrocktypes.DocumentFormatXlsx, true
	case "text/html":
		return bedrocktypes.DocumentFormatHtml, true
	case "text/plain":
		return bedrocktypes.DocumentFormatTxt, true
	case "text/markdown":
		return bedrocktypes.DocumentFormatMd, true
	default:
		return "", false
	}
}

// readWithCap reads at most maxBytes from r. Returns an error
// when the stream produced more than maxBytes (a partial body
// would silently truncate the document/image).
func readWithCap(r io.Reader, maxBytes int) ([]byte, error) {
	limited := io.LimitReader(r, int64(maxBytes)+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if len(body) > maxBytes {
		return nil, fmt.Errorf("body exceeds %d bytes", maxBytes)
	}
	return body, nil
}

// base64DecodeStd is a tiny wrapper around the std library's
// StdEncoding so the fallback note path can return a meaningful
// error string without pulling encoding/base64 directly into
// every caller.
func base64DecodeStd(s string) ([]byte, error) {
	return base64StdDecoder().DecodeString(s)
}

// truncateForLog mirrors truncateForPrompt's contract for use in
// caller-visible image-fallback notes — keeps the sentinel from
// blowing up the prompt if a model decides to embed a 4KB SVG
// data URL we can't decode.
func truncateForLog(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// syntheticEmitResponseName returns the tool name the Bedrock
// request builder pinned via ToolChoice for the json_schema
// enforcement path, or "" when no json_schema directive is on ctx
// (no unwrap needed). Centralised so the request builder and the
// response unwrapper agree on which tool to look for — drifting
// names would surface as a silent pass-through of the model's
// tool_call to the agent harness's dispatcher, which would then
// 404 on the unknown tool.
//
// Mirrors claude_subscription_convert.go's syntheticEmitResultName
// for provider symmetry; the only divergence is the default name
// ("emit_response" for Bedrock, "emit_result" for Anthropic — both
// match buildJSONSchemaEnforcementTool / buildRequestBodyCtx
// defaults).
func syntheticEmitResponseName(ctx context.Context) string {
	rf := ResponseFormatStructFromContext(ctx)
	if rf == nil || rf.Type != "json_schema" || rf.JSONSchema == nil {
		return ""
	}
	if len(rf.JSONSchema.Schema) == 0 {
		return ""
	}
	name := strings.TrimSpace(rf.JSONSchema.Name)
	if name == "" {
		name = "emit_response"
	}
	return name
}

// unwrapEmitResponseToolCall rewrites a Bedrock Converse response
// in-place when the model emitted exactly one tool_call matching the
// synthetic emit_response tool registered by buildConverseInput. The
// tool's arguments — already validated by the SDK's tool-input
// validator against the declared schema — become the visible
// Message.Content, ToolCalls is cleared, and FinishReason flips from
// "tool_calls" to "stop". The agent harness then reads Content as a
// plain JSON reply and the downstream gates
// (validateRequiredOutputKeys, plausibility) run on it unchanged.
//
// Mutates the response only when the unwrap is unambiguous: the
// response must have a single Choice whose Message has a single
// ToolCall named `name`. Multi-tool-call responses (the model
// called file_read mid-turn before emitting the final tool_call)
// are NOT unwrapped — the agent harness's tool dispatch loop runs
// next iteration and the emit_response call lands on a subsequent
// turn after the file_read result. The unwrap path then fires on
// that later turn.
//
// Returns true when an unwrap happened (for test assertions and
// debug logging). No-op when name is "", out is nil, or the
// response doesn't match the unwrap-eligible shape.
//
// Symmetric with claude_subscription_convert.go's
// unwrapEmitResultToolCall — keep the two in lock-step so swapping
// providers doesn't change the observable Message shape.
func unwrapEmitResponseToolCall(out *ChatResponse, name string) bool {
	if out == nil || name == "" {
		return false
	}
	if len(out.Choices) != 1 {
		return false
	}
	choice := &out.Choices[0]
	if len(choice.Message.ToolCalls) != 1 {
		return false
	}
	tc := choice.Message.ToolCalls[0]
	if tc.Function.Name != name {
		return false
	}
	args := tc.Function.Arguments
	if strings.TrimSpace(args) == "" {
		args = "{}"
	}
	// Preserve any text the model emitted alongside the tool call
	// (rare with tool_choice=specific tool, but Bedrock allows the
	// model to emit a text block before its tool_use). JSON args
	// land first so the agent harness's jq-based result.json
	// extractor parses the reply as-is; pre-existing text becomes
	// a fallback suffix.
	if choice.Message.Content != "" {
		choice.Message.Content = args + "\n\n" + choice.Message.Content
	} else {
		choice.Message.Content = args
	}
	choice.Message.ToolCalls = nil
	choice.FinishReason = "stop"
	return true
}

// buildJSONSchemaEnforcementTool wraps an OpenAI-shape
// ResponseJSONSchema as a synthetic OpenAI Tool whose input
// schema IS the user-supplied JSON Schema. The bedrock provider
// passes this through openAIToolsToBedrock so the SDK's
// tool-input validator enforces the schema at wire-receive time.
//
// Returns (tool, toolName, err). The name is what the caller
// passes to ToolChoiceMemberTool to force the model to call this
// specific tool. Defaults to "emit_response" when the OpenAI
// payload's name is empty (some clients send the schema without
// a name — the spec requires it but practice varies).
//
// Top-level type validation: Bedrock requires the tool input
// schema's top level to be an object. If the user-supplied
// schema is non-object, openAIToolsToBedrock's wrapper kicks in
// and the model sees a {value: <schema>} envelope. Documents the
// behaviour so a schema that DOES set type=object lands as-is.
func buildJSONSchemaEnforcementTool(rfs *ResponseJSONSchema) (Tool, string, error) {
	if rfs == nil || len(rfs.Schema) == 0 {
		return Tool{}, "", fmt.Errorf("response_format json_schema is missing the schema body")
	}
	name := strings.TrimSpace(rfs.Name)
	if name == "" {
		name = "emit_response"
	}
	desc := rfs.Description
	if desc == "" {
		desc = "Emit your response by calling this tool. The arguments object IS your response, validated against the schema."
	}
	return Tool{
		Type: "function",
		Function: ToolFunction{
			Name:        name,
			Description: desc,
			Parameters:  rfs.Schema,
		},
	}, name, nil
}

// isAllToolResults reports whether every content block in the
// slice is a ContentBlockMemberToolResult. Used by the converter
// to decide whether to merge a new tool result into the previous
// user message (Bedrock's invariant: parallel tool results from
// one assistant turn live in a single user message) vs. starting
// a fresh user message.
//
// Returns false on an empty slice — a nascent tool-result message
// is built fresh from the first matched tool input rather than
// merged into something empty.
func isAllToolResults(blocks []bedrocktypes.ContentBlock) bool {
	if len(blocks) == 0 {
		return false
	}
	for _, b := range blocks {
		if _, ok := b.(*bedrocktypes.ContentBlockMemberToolResult); !ok {
			return false
		}
	}
	return true
}

// appendJSONOnlySystemHint adds a final system message asking the
// model to respond with a single valid JSON object. Used when the
// caller passed response_format: "json_object" via context. The
// hint is appended (not prepended) so role-level system prompts
// stay in their canonical position; Bedrock concatenates all
// system blocks before serving anyway.
//
// Safe to call with messages that already have JSON instructions —
// duplication just reinforces the directive without breaking
// anything. The function returns a new slice; callers' input is
// not mutated.
const jsonOnlyHint = "Your final response MUST be a single valid JSON object — no markdown fences, no prose, no commentary. Emit ONLY the JSON."

func appendJSONOnlySystemHint(messages []Message) []Message {
	out := make([]Message, len(messages), len(messages)+1)
	copy(out, messages)
	out = append(out, Message{Role: "system", Content: jsonOnlyHint})
	return out
}

// flattenMessageText extracts the plain-text view of a Message, preferring the
// pre-flattened Content string when set and otherwise concatenating
// every text block from Blocks. Image / non-text blocks are silently
// skipped — phase 1 doesn't ship multimodal.
func flattenMessageText(m Message) string {
	if m.Content != "" {
		return m.Content
	}
	var sb strings.Builder
	for _, b := range m.Blocks {
		if b.Type == "text" && b.Text != "" {
			if sb.Len() > 0 {
				sb.WriteString("\n\n")
			}
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

// collapseSameRoleTurns joins consecutive same-role messages into one
// so Bedrock's strict alternating-role validator accepts the
// conversation. The common trigger is a tool-result followed by an
// assistant follow-up where phase 1 dropped the tool block —
// without the merge the assistant can land twice in a row.
//
// Pre-fix this collapsed via contentBlocksToText, which only returns
// text blocks. ToolResult / ToolUse / Image / Document blocks landed
// in the "" branch and were silently dropped via `continue` — the
// model would call a tool, never see the result, and re-call it on
// the next turn. Reproduced 2026-05-07 as the dominant cause of
// agents looping after the Bedrock switch. The new merge appends
// every content block (text and non-text) so tool results survive
// the role-collapse pass intact. Adjacent text blocks are still
// concatenated with "\n\n" for readability; non-text blocks are
// preserved verbatim.
func collapseSameRoleTurns(in []bedrocktypes.Message) []bedrocktypes.Message {
	if len(in) <= 1 {
		return in
	}
	out := make([]bedrocktypes.Message, 0, len(in))
	for _, m := range in {
		if len(out) == 0 || out[len(out)-1].Role != m.Role {
			out = append(out, m)
			continue
		}
		prev := &out[len(out)-1]
		// Try to fold a leading text block from the new message into
		// the trailing text block of the previous message — a small
		// readability win for purely textual merges. Everything else
		// (and any remaining new-text after the fold) is appended.
		newBlocks := m.Content
		if len(newBlocks) > 0 {
			if tbNew, ok := newBlocks[0].(*bedrocktypes.ContentBlockMemberText); ok && len(prev.Content) > 0 {
				if tbPrev, okPrev := prev.Content[len(prev.Content)-1].(*bedrocktypes.ContentBlockMemberText); okPrev {
					prev.Content[len(prev.Content)-1] = &bedrocktypes.ContentBlockMemberText{
						Value: tbPrev.Value + "\n\n" + tbNew.Value,
					}
					newBlocks = newBlocks[1:]
				}
			}
		}
		// Append the rest verbatim. This is what preserves
		// ToolResultBlock / ToolUseBlock / Image / Document content
		// when same-role messages collapse — pre-fix every non-text
		// block in this position was lost.
		prev.Content = append(prev.Content, newBlocks...)
	}
	return out
}

// bedrockOutputToChatResponse translates a Converse output back into
// the OpenAI-shaped ChatResponse the rest of vornik consumes. Sets
// Usage from the SDK's TokenUsage so cost metrics work without a
// separate accounting path.
//
// finish_reason mapping mirrors what other providers emit:
//   - end_turn / stop_sequence → "stop"
//   - max_tokens               → "length"
//   - content_filtered         → "content_filter"
//   - tool_use                 → "tool_calls" (phase 2 will populate
//     the message.tool_calls array; phase 1 is text-only so the
//     value is informational)
//   - guardrail_intervened     → "stop" (closest OpenAI analogue)
func bedrockOutputToChatResponse(out *bedrocktypes.ConverseOutputMemberMessage, model string, stop bedrocktypes.StopReason, usage *bedrocktypes.TokenUsage, requestID string) *ChatResponse {
	resp := &ChatResponse{
		ID:     requestID,
		Object: "chat.completion",
		Model:  model,
	}
	if usage != nil {
		resp.Usage.PromptTokens = int(aws.ToInt32(usage.InputTokens))
		resp.Usage.CompletionTokens = int(aws.ToInt32(usage.OutputTokens))
		resp.Usage.TotalTokens = int(aws.ToInt32(usage.TotalTokens))
		// LLM-caching phase A (observation-only): surface
		// provider-native cache usage. Bedrock reports both
		// CacheWriteInputTokens (tokens we paid to write into the
		// cache on this call) and CacheReadInputTokens (tokens
		// served from cache, billed at the ~10% cache rate). Even
		// when we don't emit cache annotations ourselves (phase A
		// is observation-only), some Bedrock models auto-cache
		// stable prefixes server-side — these show up here as a
		// free observability win.
		resp.Usage.CacheCreationTokens = int(aws.ToInt32(usage.CacheWriteInputTokens))
		resp.Usage.CacheReadTokens = int(aws.ToInt32(usage.CacheReadInputTokens))
	}

	var text strings.Builder
	var reasoning strings.Builder
	var toolCalls []ToolCall
	// extractionWarning surfaces partial-tool-call extraction failures
	// to the BedrockProvider caller. Pre-fix this was silently dropped
	// — operators couldn't see when a tool call's smithy document
	// failed to marshal because the dispatcher just received an empty
	// tool-call slice and treated it as "no tools needed".
	var extractionWarning string
	if out != nil {
		for _, b := range out.Value.Content {
			switch tb := b.(type) {
			case *bedrocktypes.ContentBlockMemberText:
				text.WriteString(tb.Value)
			case *bedrocktypes.ContentBlockMemberReasoningContent:
				// Reasoning content (kimi-k2-thinking, Claude
				// with thinking budgets) is the model's
				// internal CoT. Kept SEPARATE from the visible
				// text so downstream parsers (gates, the
				// hallucination judge, the strategist's
				// schema validator) see clean output.
				if rt, ok := tb.Value.(*bedrocktypes.ReasoningContentBlockMemberReasoningText); ok {
					if rt.Value.Text != nil && *rt.Value.Text != "" {
						if reasoning.Len() > 0 {
							reasoning.WriteString("\n\n")
						}
						reasoning.WriteString(*rt.Value.Text)
					}
				}
				// Redacted content (encrypted CoT) just gets
				// a marker — the bytes aren't human-readable
				// and we shouldn't try to surface them.
				if _, ok := tb.Value.(*bedrocktypes.ReasoningContentBlockMemberRedactedContent); ok {
					if reasoning.Len() > 0 {
						reasoning.WriteString("\n\n")
					}
					reasoning.WriteString("[redacted reasoning]")
				}
			}
		}
		// Tool-use blocks live in the same content array as text. We
		// extract them in a separate pass so a tool-only response
		// (text empty, tool_calls populated) lands cleanly — the
		// dispatcher's tool-call loop keys off Message.ToolCalls
		// being non-empty, not the FinishReason.
		//
		// Pre-fix the err branch was silently swallowed (toolCalls
		// stayed nil); a single malformed tool-call argument
		// produced an empty Message.ToolCalls and the dispatcher
		// returned "Done." even though the model had requested a
		// tool call. Now extractToolCallsFromContent returns the
		// partial set on failure so partial work survives. The
		// caller (BedrockProvider.complete) inspects the response's
		// internal extraction-error marker to log the drop with a
		// context-rich message.
		calls, rescued, extractErr := extractToolCallsFromContent(out.Value.Content)
		toolCalls = calls
		if extractErr != nil {
			extractionWarning = extractErr.Error()
		}
		// Rescue: when a ToolUse Name failed the sanity predicate,
		// the JSON-shaped Name was the model's actual intended
		// content. Append it to the assistant text so the
		// dispatcher's "no tool calls → emit final answer" path
		// surfaces what the model meant. Without this, every
		// retry would dispatch to "unknown tool: ..." and burn
		// the iteration budget on a silent 404.
		for _, c := range rescued {
			if text.Len() > 0 {
				text.WriteString("\n")
			}
			text.WriteString(c)
		}
		// Document-marshal failures fall through to a text-only
		// response: the FinishReason stays "tool_calls" so callers
		// can still detect the missed translation, but the loop
		// doesn't crash mid-tick.
	}

	resp.Choices = append(resp.Choices, struct {
		Index        int     `json:"index"`
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	}{
		Index: 0,
		Message: Message{
			Role:             "assistant",
			Content:          text.String(),
			ReasoningContent: reasoning.String(),
			ToolCalls:        toolCalls,
		},
		FinishReason: bedrockStopReasonToOpenAI(stop),
	})
	resp.ExtractionWarning = extractionWarning
	return resp
}

// openAIToolsToBedrock translates the OpenAI function-calling tool
// list into the Bedrock Converse toolConfig.Tools slice. Returns
// (nil, nil) when tools is empty so callers can leave
// ConverseInput.ToolConfig unset cleanly. Non-function tool types
// (OpenAI's tools array allows {"type": "function", ...} as the only
// shipped shape today, but the spec is forward-compatible) are
// silently dropped with a caller-visible note in the per-tool spec
// description.
//
// Bedrock requires the JSON Schema to be a top-level OBJECT. If a
// caller passes a non-object schema (rare, but spec-legal), we wrap
// it under {"type":"object","properties":{"value": <schema>}} so the
// SDK's strict validation accepts the request. The model sees a
// slightly different parameter shape; the convention is documented
// so future-Phase-3 tool-result handling can detect and unwrap it.
func openAIToolsToBedrock(tools []Tool) ([]bedrocktypes.Tool, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]bedrocktypes.Tool, 0, len(tools))
	for _, t := range tools {
		if t.Type != "function" && t.Type != "" {
			// Future tool types — skip but don't fail the whole call.
			continue
		}
		spec, err := buildToolSpec(t.Function)
		if err != nil {
			return nil, fmt.Errorf("tool %q: %w", t.Function.Name, err)
		}
		out = append(out, &bedrocktypes.ToolMemberToolSpec{Value: spec})
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// buildToolSpec wraps a single OpenAI ToolFunction into the Bedrock
// ToolSpecification shape. The JSON Schema in `parameters` is fed
// through smithy's document.NewLazyDocument so the SDK's wire-format
// encoder handles it (Bedrock expects a JSON object document, not a
// raw string).
func buildToolSpec(fn ToolFunction) (bedrocktypes.ToolSpecification, error) {
	if fn.Name == "" {
		return bedrocktypes.ToolSpecification{}, fmt.Errorf("function name is required")
	}
	// Decode the JSON Schema once so we can pass a structured value
	// to NewLazyDocument (it accepts arbitrary Go values; passing a
	// raw json.RawMessage would be re-marshaled as a string, not as
	// the JSON object Bedrock expects).
	var params any = map[string]any{"type": "object", "properties": map[string]any{}}
	if len(fn.Parameters) > 0 {
		if err := json.Unmarshal(fn.Parameters, &params); err != nil {
			return bedrocktypes.ToolSpecification{}, fmt.Errorf("parameters not valid JSON: %w", err)
		}
	}
	// Bedrock requires the top level of the schema to be an object —
	// non-object schemas wrapped under a `value` key so the SDK
	// validation accepts them.
	if m, ok := params.(map[string]any); !ok || m["type"] != "object" {
		params = map[string]any{
			"type":       "object",
			"properties": map[string]any{"value": params},
			"required":   []string{"value"},
		}
	}
	spec := bedrocktypes.ToolSpecification{
		Name:        aws.String(fn.Name),
		InputSchema: &bedrocktypes.ToolInputSchemaMemberJson{Value: bedrockdoc.NewLazyDocument(params)},
	}
	if fn.Description != "" {
		spec.Description = aws.String(fn.Description)
	}
	return spec, nil
}

// extractToolCallsFromContent walks a Bedrock content block list and
// returns the OpenAI ToolCall entries. Phase 1 ignored these; Phase
// 2 surfaces them so the dispatcher's tool-call loop sees the
// model's tool requests through the same `Message.ToolCalls` shape
// it gets from every other provider.
//
// Bedrock's ToolUseBlock carries the tool input as a smithy
// document.Interface. We re-marshal it via the document's
// UnmarshalSmithyDocument hook into a json.RawMessage so the
// downstream OpenAI shape's `arguments: string` field stays in the
// same JSON-string form callers expect.
//
// rescuedContent: when a ToolUseBlock's Name fails the hallucinated-
// tool-name predicate (looksLikeHallucinatedToolName — whitespace,
// quotes, braces, XML closers, i.e. shapes no real tool ever has),
// the call is NOT surfaced. Instead the offending Name is returned
// here so the caller can append it to the message Content. This
// rescues the model's intended JSON output from the silent-404 loop
// observed 2026-05-12 on minimax.minimax-m2.5: the model packed its
// final answer into the tool Name (instead of a content block),
// every retry dispatched to "unknown tool: ..." and burned 8+ min
// of wall-clock per occurrence (exec_20260512225546). With the
// rescue the dispatcher sees no tool calls → exits the loop → final
// answer is the JSON the model meant to emit; the hallucination
// detector still flags the malformed shape on the audit trail.
func extractToolCallsFromContent(blocks []bedrocktypes.ContentBlock) (calls []ToolCall, rescuedContent []string, firstErr error) {
	if len(blocks) == 0 {
		return nil, nil, nil
	}
	for _, b := range blocks {
		tu, ok := b.(*bedrocktypes.ContentBlockMemberToolUse)
		if !ok {
			continue
		}
		name := aws.ToString(tu.Value.Name)
		if looksLikeHallucinatedToolName(name) {
			rescuedContent = append(rescuedContent, name)
			continue
		}
		argsJSON, err := documentToJSON(tu.Value.Input)
		if err != nil {
			// Pre-fix this returned (nil, err) on the first failure,
			// silently dropping every subsequent valid tool call in
			// the same turn. Now: skip this one (with an empty
			// argument fallback so the call still surfaces if the
			// receiver wants to retry it), capture the first error,
			// and continue extracting the rest.
			if firstErr == nil {
				firstErr = fmt.Errorf("tool %q (%s): marshal arguments: %w",
					name, aws.ToString(tu.Value.ToolUseId), err)
			}
			continue
		}
		calls = append(calls, ToolCall{
			ID:   aws.ToString(tu.Value.ToolUseId),
			Type: "function",
			Function: FunctionCall{
				Name:      name,
				Arguments: string(argsJSON),
			},
		})
	}
	return calls, rescuedContent, firstErr
}

// looksLikeHallucinatedToolName reports whether a tool-use Name is
// almost certainly a structured-output blob the model emitted
// instead of using a content block. Real tool names are single-token
// identifiers (snake_case, dot.namespace, etc.) — none of them
// contain whitespace, quotes, braces, or XML closer fragments.
//
// Mirrors the hallucination detector's isHallucinatedToolName in
// internal/hallucination/rules.go but lives here to avoid pulling
// the detector package into the chat layer (one-way dependency:
// hallucination reads from audit; chat writes to audit).
func looksLikeHallucinatedToolName(name string) bool {
	if name == "" {
		return false
	}
	// XML closer leak — model templated a tool call as text.
	if strings.Contains(name, "</") {
		return true
	}
	// JSON or shell syntax in the name — the model packed structured
	// output where an identifier was expected.
	if strings.ContainsAny(name, " \t|;\"'`<>{}") {
		return true
	}
	return false
}

// documentToJSON marshals a smithy document.Interface into raw JSON
// bytes. Goes through MarshalSmithyDocument (the SDK's documented
// path that produces UTF-8 JSON) rather than UnmarshalSmithyDocument
// — the latter requires a concrete typed destination and 400s on
// generic interface{} / map[string]any targets.
func documentToJSON(d bedrockdoc.Interface) ([]byte, error) {
	if d == nil {
		return []byte(`{}`), nil
	}
	return d.MarshalSmithyDocument()
}

func bedrockStopReasonToOpenAI(s bedrocktypes.StopReason) string {
	switch s {
	case bedrocktypes.StopReasonEndTurn, bedrocktypes.StopReasonStopSequence:
		return "stop"
	case bedrocktypes.StopReasonMaxTokens:
		return "length"
	case bedrocktypes.StopReasonContentFiltered:
		return "content_filter"
	case bedrocktypes.StopReasonToolUse:
		return "tool_calls"
	case bedrocktypes.StopReasonGuardrailIntervened:
		return "stop"
	default:
		return "stop"
	}
}
