package chat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestBuildRequestBody_IdentityPromptFirst — the leading system block
// MUST be the exact Claude Code identity string. Anthropic's
// OAuth abuse filter returns terse 429s without it.
func TestBuildRequestBody_IdentityPromptFirst(t *testing.T) {
	c := NewClaudeSubscriptionClient("claude-sonnet-4-6")
	raw, err := c.buildRequestBody(
		[]Message{
			{Role: "system", Content: "you are a lead agent"},
			{Role: "user", Content: "hi"},
		},
		nil,
		claudeAccountInfo{},
		"test-session",
	)
	if err != nil {
		t.Fatal(err)
	}

	var body struct {
		System []struct{ Type, Text string } `json:"system"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.System) != 2 {
		t.Fatalf("want 2 system blocks, got %d", len(body.System))
	}
	if body.System[0].Text != claudeIdentitySystemPrompt {
		t.Errorf("first system block mismatch:\n got: %q\nwant: %q",
			body.System[0].Text, claudeIdentitySystemPrompt)
	}
	if body.System[1].Text != "you are a lead agent" {
		t.Errorf("second system block mismatch: %q", body.System[1].Text)
	}
}

// TestBuildRequestBody_IdentityPromptEvenWithoutSystem — the identity
// string must be present even when the caller provides no system
// messages at all.
func TestBuildRequestBody_IdentityPromptEvenWithoutSystem(t *testing.T) {
	c := NewClaudeSubscriptionClient("claude-sonnet-4-6")
	raw, err := c.buildRequestBody(
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		claudeAccountInfo{},
		"s",
	)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		System []struct{ Type, Text string } `json:"system"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.System) != 1 || body.System[0].Text != claudeIdentitySystemPrompt {
		t.Fatalf("want single identity block, got %+v", body.System)
	}
}

// TestBuildRequestBody_MetadataUserID — the stringified blob must
// contain device_id/account_uuid/session_id with the values passed,
// matching the CLI's 186-byte format.
func TestBuildRequestBody_MetadataUserID(t *testing.T) {
	c := NewClaudeSubscriptionClient("claude-sonnet-4-6")
	raw, err := c.buildRequestBody(
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		claudeAccountInfo{
			UserID:      "abc123",
			AccountUUID: "aaaa-bbbb",
		},
		"sess-xyz",
	)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	var inner struct {
		DeviceID    string `json:"device_id"`
		AccountUUID string `json:"account_uuid"`
		SessionID   string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(body.Metadata.UserID), &inner); err != nil {
		t.Fatalf("user_id is not valid JSON string: %v (raw: %q)", err, body.Metadata.UserID)
	}
	if inner.DeviceID != "abc123" || inner.AccountUUID != "aaaa-bbbb" || inner.SessionID != "sess-xyz" {
		t.Errorf("fields mismatch: %+v", inner)
	}
}

// TestBuildRequestBody_ToolCalls_ConvertToAnthropicShape — assistant
// tool calls become tool_use blocks, tool results become tool_result
// blocks on user role, and consecutive same-role messages merge.
func TestBuildRequestBody_ToolCalls_ConvertToAnthropicShape(t *testing.T) {
	c := NewClaudeSubscriptionClient("claude-sonnet-4-6")
	raw, err := c.buildRequestBody(
		[]Message{
			{Role: "user", Content: "read foo.txt"},
			{
				Role:    "assistant",
				Content: "",
				ToolCalls: []ToolCall{{
					ID:       "call_1",
					Type:     "function",
					Function: FunctionCall{Name: "file_read", Arguments: `{"path":"foo.txt"}`},
				}},
			},
			{Role: "tool", ToolCallID: "call_1", Content: "hello world"},
		},
		[]Tool{{
			Type: "function",
			Function: ToolFunction{
				Name:        "file_read",
				Description: "read a file",
				Parameters:  json.RawMessage(`{"type":"object"}`),
			},
		}},
		claudeAccountInfo{},
		"s",
	)
	if err != nil {
		t.Fatal(err)
	}

	var body struct {
		Messages []struct {
			Role    string
			Content []map[string]any
		}
		Tools []map[string]any
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	// Expected alternation: user → assistant(tool_use) → user(tool_result).
	// The tool_result is a user-role message, but since the prior entry
	// was assistant it doesn't merge — three distinct messages is the
	// right shape for Anthropic's role-alternation invariant.
	if len(body.Messages) != 3 {
		t.Fatalf("want 3 messages (user, assistant, user with tool_result), got %d", len(body.Messages))
	}
	roles := []string{body.Messages[0].Role, body.Messages[1].Role, body.Messages[2].Role}
	want := []string{"user", "assistant", "user"}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("role[%d] want %q got %q", i, want[i], roles[i])
		}
	}
	if body.Messages[1].Content[0]["type"] != "tool_use" {
		t.Errorf("assistant content[0] should be tool_use, got %v", body.Messages[1].Content[0])
	}
	if body.Messages[2].Content[0]["type"] != "tool_result" {
		t.Errorf("third message content[0] should be tool_result, got %v", body.Messages[2].Content[0])
	}
	if len(body.Tools) != 1 {
		t.Fatalf("want 1 tool, got %d", len(body.Tools))
	}
	if _, ok := body.Tools[0]["input_schema"]; !ok {
		t.Error("tool should use input_schema key (not parameters)")
	}
}

// TestBuildRequestBody_RoleAlternation_MergesConsecutiveSameRole —
// Anthropic rejects messages[] where two adjacent entries share a
// role. Tool results (emitted as user-role tool_result blocks) after
// a user-role prior entry must merge into one message.
func TestBuildRequestBody_RoleAlternation_MergesConsecutiveSameRole(t *testing.T) {
	c := NewClaudeSubscriptionClient("claude-sonnet-4-6")
	raw, err := c.buildRequestBody(
		[]Message{
			{Role: "user", Content: "first"},
			{Role: "user", Content: "second"},
			{Role: "assistant", Content: "reply"},
		},
		nil,
		claudeAccountInfo{},
		"s",
	)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Messages []struct {
			Role    string
			Content []map[string]any
		}
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Messages) != 2 {
		t.Fatalf("want 2 merged messages, got %d: %+v", len(body.Messages), body.Messages)
	}
	if len(body.Messages[0].Content) != 2 {
		t.Fatalf("want user message merged to 2 content blocks, got %d", len(body.Messages[0].Content))
	}
}

// TestParseMessagesSSE_TextAndToolUse — exercises the three
// content_block kinds the server emits (text, tool_use, thinking)
// and the completion signals (message_delta stop_reason → finish
// reason, message_delta usage → output_tokens top-up).
func TestParseMessagesSSE_TextAndToolUse(t *testing.T) {
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_01","model":"claude-sonnet-4-6","usage":{"input_tokens":50,"output_tokens":0}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello "}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"world"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tool_1","name":"file_read","input":{}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"foo\"}"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":1}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":42}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	var streamed []string
	resp, err := parseClaudeMessagesSSE(strings.NewReader(sse), func(s string) { streamed = append(streamed, s) })
	if err != nil {
		t.Fatal(err)
	}
	if resp.Usage.PromptTokens != 50 || resp.Usage.CompletionTokens != 42 {
		t.Errorf("usage mismatch: %+v", resp.Usage)
	}
	if resp.Usage.TotalTokens != 92 {
		t.Errorf("total tokens want 92, got %d", resp.Usage.TotalTokens)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("want 1 choice, got %d", len(resp.Choices))
	}
	msg := resp.Choices[0].Message
	if msg.Content != "Hello world" {
		t.Errorf("content mismatch: %q", msg.Content)
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("want 1 tool call, got %d", len(msg.ToolCalls))
	}
	tc := msg.ToolCalls[0]
	if tc.ID != "tool_1" || tc.Function.Name != "file_read" || tc.Function.Arguments != `{"path":"foo"}` {
		t.Errorf("tool call mismatch: %+v", tc)
	}
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish reason should be tool_calls on stop_reason=tool_use, got %q", resp.Choices[0].FinishReason)
	}
	if len(streamed) < 2 {
		t.Errorf("streaming callback should fire on each text_delta, fired %d times", len(streamed))
	}
}

// TestParseMessagesSSE_ErrorEvent — a terminal error event is
// surfaced to the caller rather than producing a partial response.
func TestParseMessagesSSE_ErrorEvent(t *testing.T) {
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"x","model":"m"}}`,
		``,
		`event: error`,
		`data: {"type":"error","error":{"type":"overloaded_error","message":"try again"}}`,
		``,
	}, "\n")

	if _, err := parseClaudeMessagesSSE(strings.NewReader(sse), nil); err == nil {
		t.Fatal("want error, got nil")
	} else if !strings.Contains(err.Error(), "overloaded_error") {
		t.Errorf("error should mention upstream type, got: %v", err)
	}
}

// TestSessionID_UUIDv4Shape — the session ID generator must produce
// a canonical 8-4-4-4-12 UUID-v4 so the server's optional validator
// doesn't reject a hex-only string.
func TestSessionID_UUIDv4Shape(t *testing.T) {
	id := newClaudeSessionID()
	if len(id) != 36 {
		t.Fatalf("want 36-char UUID, got %d: %q", len(id), id)
	}
	parts := strings.Split(id, "-")
	wantLens := []int{8, 4, 4, 4, 12}
	if len(parts) != len(wantLens) {
		t.Fatalf("want 5 groups, got %d: %q", len(parts), id)
	}
	for i, p := range parts {
		if len(p) != wantLens[i] {
			t.Errorf("group %d len want %d got %d: %q", i, wantLens[i], len(p), p)
		}
	}
	// v4: third group must start with '4'.
	if !strings.HasPrefix(parts[2], "4") {
		t.Errorf("group 2 should start with version nibble 4, got %q", parts[2])
	}
}

// TestAccountResolver_ReadsClaudeJSON — the resolver pulls userID
// and oauthAccount.accountUuid from the CLI's install state file.
func TestAccountResolver_ReadsClaudeJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	payload := `{"userID":"abcdef0123","oauthAccount":{"accountUuid":"uuid-1","emailAddress":"x@y.z"}}`
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &claudeAccountResolver{path: path}
	got := r.resolve()
	if got.UserID != "abcdef0123" || got.AccountUUID != "uuid-1" {
		t.Errorf("resolved info mismatch: %+v", got)
	}
}

// TestAccountResolver_MissingFile_ReturnsEmpty — missing or malformed
// files must not panic; they return zero-value info so the client can
// still assemble a request (the blob will just be shorter).
func TestAccountResolver_MissingFile_ReturnsEmpty(t *testing.T) {
	r := &claudeAccountResolver{path: filepath.Join(t.TempDir(), "does-not-exist")}
	got := r.resolve()
	if got.UserID != "" || got.AccountUUID != "" {
		t.Errorf("want zero info for missing file, got %+v", got)
	}
}

// TestAuth_ReloadsFromDiskWhenNearExpiry — simulates the real-world
// race where the interactive `claude` CLI refreshes the token while
// vornik has a stale cached copy. The on-disk file is rewritten
// externally; our Token() call must pick up the rotated values rather
// than using the stale cached access_token.
func TestAuth_ReloadsFromDiskWhenNearExpiry(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".credentials.json")
	// Start with an access token that expires in ~30s (< our 60s
	// refresh window, so we'll consider it expiring).
	writeCreds(t, path, "OLD", "R_OLD", time.Now().Add(30*time.Second))
	mgr := newClaudeAuthManager(path)
	// Prime the cache so the first Token() call proceeds through the
	// "reload before refresh" branch rather than the initial load.
	if err := mgr.loadLocked(); err != nil {
		t.Fatal(err)
	}

	// Simulate the CLI rotating tokens out from under us.
	writeCreds(t, path, "NEW", "R_NEW", time.Now().Add(time.Hour))

	tok, err := mgr.Token(context.Background())
	if err != nil {
		t.Fatalf("Token returned error: %v", err)
	}
	if tok != "NEW" {
		t.Errorf("want reloaded token NEW, got %q", tok)
	}
}

// TestAuth_FreshCachedTokenIsReturnedAsIs — sanity check: when the
// cached token has plenty of runway, we return it without touching
// the disk. No infinite loops, no spurious refreshes.
func TestAuth_FreshCachedTokenIsReturnedAsIs(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".credentials.json")
	writeCreds(t, path, "FRESH", "R", time.Now().Add(time.Hour))
	mgr := newClaudeAuthManager(path)

	tok, err := mgr.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "FRESH" {
		t.Errorf("want FRESH, got %q", tok)
	}
}

// TestAuth_InvalidGrantDetection — the helper that decides whether
// to retry-with-reload must match the two shapes the Claude OAuth
// server produces (error field "invalid_grant", or the human-
// readable "Refresh token not found" suffix in the error body).
func TestAuth_InvalidGrantDetection(t *testing.T) {
	cases := map[string]bool{
		`oauth/token returned 400: invalid_grant`:                      true,
		`{"error":"invalid_grant","error_description":"..."}`:          true,
		`oauth/token returned 400: Refresh token not found or invalid`: true,
		`oauth/token returned 500: internal server error`:              false,
	}
	for msg, want := range cases {
		got := isClaudeInvalidGrant(errors.New(msg))
		if got != want {
			t.Errorf("msg=%q want=%v got=%v", msg, want, got)
		}
	}
	if isClaudeInvalidGrant(nil) {
		t.Error("nil error should not match")
	}
}

// writeCreds writes a .credentials.json with the given access_token,
// refresh_token, and expiry. Used by auth tests to set up on-disk
// state before exercising the manager.
func writeCreds(t *testing.T, path, access, refresh string, expiry time.Time) {
	t.Helper()
	raw := `{"claudeAiOauth":{"accessToken":"` + access +
		`","refreshToken":"` + refresh +
		`","expiresAt":` + strconv.FormatInt(expiry.UnixMilli(), 10) + `}}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestClient_SetsAllExpectedHeaders — exercises the full request-
// building pipeline against a fake upstream and verifies the
// headers the abuse filter is known to check. If any of these drift
// we'll catch it here before shipping.
func TestClient_SetsAllExpectedHeaders(t *testing.T) {
	credsDir := t.TempDir()
	credsPath := filepath.Join(credsDir, ".credentials.json")
	claudeJSON := filepath.Join(credsDir, ".claude.json")
	if err := os.WriteFile(credsPath, []byte(`{"claudeAiOauth":{"accessToken":"tok","refreshToken":"r","expiresAt":99999999999999}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudeJSON, []byte(`{"userID":"deadbeef","oauthAccount":{"accountUuid":"abcd"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var captured *http.Request
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r
		capturedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\n"+
			`data: {"type":"message_start","message":{"id":"x","model":"m","usage":{"input_tokens":1,"output_tokens":0}}}`+"\n\n"+
			"event: content_block_start\n"+
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`+"\n\n"+
			"event: content_block_delta\n"+
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`+"\n\n"+
			"event: content_block_stop\n"+
			`data: {"type":"content_block_stop","index":0}`+"\n\n"+
			"event: message_delta\n"+
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`+"\n\n"+
			"event: message_stop\n"+
			`data: {"type":"message_stop"}`+"\n\n")
	}))
	defer srv.Close()

	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)

	c := NewClaudeSubscriptionClient("claude-sonnet-4-6",
		WithClaudeSubscriptionAuthPath(credsPath),
	)
	// Swap the account resolver to read our fixture file instead of
	// the user's real ~/.claude.json. This is the only test-only
	// mutation; production code never touches the field after ctor.
	c.account = &claudeAccountResolver{path: claudeJSON}

	if _, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatal(err)
	}
	if captured == nil {
		t.Fatal("upstream never saw a request")
	}
	if !strings.HasSuffix(captured.URL.Path, "/v1/messages") || captured.URL.RawQuery != "beta=true" {
		t.Errorf("URL shape wrong: path=%q query=%q", captured.URL.Path, captured.URL.RawQuery)
	}
	checks := map[string]string{
		"Authorization":     "Bearer tok",
		"Anthropic-Version": claudeAPIVersion,
		"Anthropic-Beta":    claudeOAuthBeta,
		"Anthropic-Dangerous-Direct-Browser-Access": "true",
		"User-Agent":          claudeDefaultUserAgent,
		"X-App":               "cli",
		"X-Stainless-Lang":    "js",
		"X-Stainless-Runtime": "node",
	}
	for h, want := range checks {
		if got := captured.Header.Get(h); got != want {
			t.Errorf("header %q: want %q, got %q", h, want, got)
		}
	}
	if sid := captured.Header.Get("X-Claude-Code-Session-Id"); len(sid) != 36 {
		t.Errorf("X-Claude-Code-Session-Id should be UUID, got %q", sid)
	}

	// Body spot-check: identity prompt + user_id blob with our fake values.
	var body struct {
		System []struct {
			Text string `json:"text"`
		} `json:"system"`
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(capturedBody, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.System) == 0 || body.System[0].Text != claudeIdentitySystemPrompt {
		t.Errorf("identity prompt missing from first system block: %+v", body.System)
	}
	if !strings.Contains(body.Metadata.UserID, `"device_id":"deadbeef"`) ||
		!strings.Contains(body.Metadata.UserID, `"account_uuid":"abcd"`) {
		t.Errorf("metadata.user_id missing expected fields: %q", body.Metadata.UserID)
	}
}

// TestParseClaudeRetryAfterBody — body-embedded retry hint parsing
// for sub-item 8 step 2. The Anthropic OAuth surface uses a few
// distinct shapes depending on the route; this parser accepts all
// observed forms and rejects unparseable JSON gracefully (returning
// ok=false so the caller falls through to generic backoff).
func TestParseClaudeRetryAfterBody(t *testing.T) {
	cases := []struct {
		name string
		body string
		want time.Duration
		ok   bool
	}{
		{"empty body", "", 0, false},
		{"malformed json", "{not json", 0, false},
		{"no hint", `{"error":{"type":"rate_limit_error","message":"x"}}`, 0, false},
		{"error.retry_after seconds (int)", `{"error":{"retry_after":17}}`, 17 * time.Second, true},
		{"error.retry_after_seconds (legacy)", `{"error":{"retry_after_seconds":3}}`, 3 * time.Second, true},
		{"top-level retry_after", `{"retry_after":12}`, 12 * time.Second, true},
		{"fractional seconds preserved", `{"error":{"retry_after":1.5}}`, 1500 * time.Millisecond, true},
		{"zero is no-hint", `{"error":{"retry_after":0}}`, 0, false},
		{"negative is no-hint", `{"error":{"retry_after":-5}}`, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, ok := parseClaudeRetryAfterBody([]byte(c.body))
			if ok != c.ok {
				t.Errorf("ok = %v, want %v (d=%v)", ok, c.ok, d)
			}
			if d != c.want {
				t.Errorf("duration = %v, want %v", d, c.want)
			}
		})
	}
}
