// Failure-path coverage for the chat proxies. These tests pin the
// contract that a client ALWAYS gets a well-formed terminal frame —
// never a hang on a half-open stream and never a bare 500 with an
// empty/opaque body. They focus on the gaps the existing
// chat_proxy_test.go / chat_endpoints_coverage_test.go suites leave:
//
//   - the streaming error frame's truncateOllamaErr (240-char) cap
//     and "error: " prefix on /api/chat and /api/generate,
//   - that the streaming error frame is the SOLE terminal frame (no
//     stray content delta precedes a provider-pre-flight failure),
//   - that every non-stream failure mode (no provider, empty
//     messages, malformed JSON, oversized body, provider error,
//     nil response) decodes into the canonical {error:{code,message}}
//     envelope with a non-empty code/message and a non-500 status.
//
// Reuses the package helpers (streamingStub, openaiStub, NewServer /
// WithChatProvider). New helpers/types are prefixed "ee" per the
// file-ownership rule.

package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// eeDecodeErrorEnvelope decodes the canonical respondError body
// shape ({"error":{"code","message"}}). Fails the test if the body
// isn't valid JSON in that shape — a bare/empty 500 body would not
// decode, which is exactly the regression these tests guard against.
func eeDecodeErrorEnvelope(t *testing.T, body []byte) (code, message string) {
	t.Helper()
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("error body is not a well-formed envelope: %v — raw=%s", err, string(body))
	}
	return env.Error.Code, env.Error.Message
}

// eeLastNDJSONFrame splits an x-ndjson body into frames and returns
// the trimmed frames plus the decoded final ollamaChatResponse. Used
// to assert the streaming terminal-frame contract.
func eeLastNDJSONFrame(t *testing.T, body string) ([]string, ollamaChatResponse) {
	t.Helper()
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		t.Fatal("streaming body is empty — client would hang on a half-open response")
	}
	frames := strings.Split(trimmed, "\n")
	var final ollamaChatResponse
	if err := json.Unmarshal([]byte(frames[len(frames)-1]), &final); err != nil {
		t.Fatalf("final NDJSON frame is not valid JSON: %v — line=%s", err, frames[len(frames)-1])
	}
	return frames, final
}

// TestOllamaChat_StreamErrorFrame_IsSoleTerminalFrame — when the
// provider fails before emitting any content (the common
// auth/rate-limit-on-first-call case), the streaming path must emit
// exactly ONE NDJSON frame: the done:true error frame. No empty
// content delta should precede it, and the frame must parse cleanly.
// Guards against a client rendering a stray empty assistant bubble
// or hanging waiting for a close.
func TestOllamaChat_StreamErrorFrame_IsSoleTerminalFrame(t *testing.T) {
	stub := &streamingStub{model: "stub", err: errors.New("boom on first token")}
	s := NewServer(WithChatProvider(stub), WithLogger(zerolog.Nop()))
	body := bytes.NewBufferString(`{"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/chat", body)
	rr := httptest.NewRecorder()
	s.OllamaChat(rr, req)

	// Headers were flushed at stream start, so the HTTP status is 200
	// even though the provider failed — the failure rides the frame,
	// never a bare 500.
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (error surfaces in the NDJSON frame, not the status line)", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Errorf("Content-Type = %q, want application/x-ndjson", ct)
	}
	frames, final := eeLastNDJSONFrame(t, rr.Body.String())
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want exactly 1 (the terminal error frame) — frames=%v", len(frames), frames)
	}
	if !final.Done {
		t.Error("the sole frame must carry done:true so the client closes the stream")
	}
	if !strings.HasPrefix(final.DoneReason, "error: ") {
		t.Errorf("DoneReason = %q, want an 'error: ' prefix so clients can detect the failure", final.DoneReason)
	}
	if !strings.Contains(final.DoneReason, "boom on first token") {
		t.Errorf("DoneReason = %q, should carry the upstream error text", final.DoneReason)
	}
}

// TestOllamaChat_StreamErrorFrame_TruncatesLongError — the streaming
// error frame routes err.Error() through truncateOllamaErr (240-char
// cap) so a multi-KB upstream stack trace doesn't blow up Open
// WebUI's inline error rendering. The existing stream-error test only
// asserts Contains on a short string; this pins the cap on the
// streaming path. A regression dropping the truncate call would ship
// the full error to the client.
func TestOllamaChat_StreamErrorFrame_TruncatesLongError(t *testing.T) {
	longErr := strings.Repeat("z", 1000)
	stub := &streamingStub{model: "stub", err: errors.New(longErr)}
	s := NewServer(WithChatProvider(stub), WithLogger(zerolog.Nop()))
	body := bytes.NewBufferString(`{"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/chat", body)
	rr := httptest.NewRecorder()
	s.OllamaChat(rr, req)

	_, final := eeLastNDJSONFrame(t, rr.Body.String())
	if !final.Done {
		t.Fatal("error frame must have done:true")
	}
	// DoneReason = "error: " + truncateOllamaErr(longErr). The
	// truncated payload is 240 chars + the ellipsis rune; it must be
	// far shorter than the 1000-char raw error.
	if !strings.HasPrefix(final.DoneReason, "error: ") {
		t.Fatalf("DoneReason = %q, want 'error: ' prefix", final.DoneReason)
	}
	payload := strings.TrimPrefix(final.DoneReason, "error: ")
	if !strings.HasSuffix(payload, "…") {
		t.Errorf("truncated payload should end with the ellipsis sentinel, got tail %q", eeTail(payload))
	}
	// 240 runs of 'z' + the multibyte ellipsis. Assert the raw error
	// did NOT pass through whole.
	if strings.Contains(payload, longErr) {
		t.Errorf("the full %d-char error leaked into the frame untruncated", len(longErr))
	}
	if zCount := strings.Count(payload, "z"); zCount != 240 {
		t.Errorf("truncated 'z' run = %d, want 240 (truncateOllamaErr cap)", zCount)
	}
}

// TestOllamaGenerate_StreamErrorFrame_TruncatesLongError — the
// /api/generate streaming path has its own copy of the error-frame
// block. Pin the same 240-cap + "error: " prefix there. Existing
// TestOllamaGenerate_StreamingProviderError only asserts Contains on
// a short string.
func TestOllamaGenerate_StreamErrorFrame_TruncatesLongError(t *testing.T) {
	longErr := strings.Repeat("q", 1000)
	stub := &streamingStub{model: "stub", err: errors.New(longErr)}
	s := NewServer(WithChatProvider(stub), WithLogger(zerolog.Nop()))
	body := bytes.NewBufferString(`{"prompt":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
	rr := httptest.NewRecorder()
	s.OllamaGenerate(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (error rides the NDJSON frame)", rr.Code)
	}
	trimmed := strings.TrimSpace(rr.Body.String())
	if trimmed == "" {
		t.Fatal("generate stream body empty — client would hang")
	}
	frames := strings.Split(trimmed, "\n")
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want exactly 1 terminal error frame — frames=%v", len(frames), frames)
	}
	var final ollamaGenerateResponse
	if err := json.Unmarshal([]byte(frames[0]), &final); err != nil {
		t.Fatalf("error frame is not valid JSON: %v — line=%s", err, frames[0])
	}
	if !final.Done {
		t.Error("generate error frame must have done:true")
	}
	if !strings.HasPrefix(final.DoneReason, "error: ") {
		t.Fatalf("DoneReason = %q, want 'error: ' prefix", final.DoneReason)
	}
	payload := strings.TrimPrefix(final.DoneReason, "error: ")
	if strings.Contains(payload, longErr) {
		t.Errorf("full %d-char error leaked untruncated into generate frame", len(longErr))
	}
	if !strings.HasSuffix(payload, "…") {
		t.Errorf("truncated payload should end with ellipsis, got tail %q", eeTail(payload))
	}
	if qCount := strings.Count(payload, "q"); qCount != 240 {
		t.Errorf("truncated 'q' run = %d, want 240", qCount)
	}
}

// TestOllamaChat_FailureModes_ReturnWellFormedEnvelope walks the
// non-stream failure surface of /api/chat and asserts each one lands
// as the canonical {error:{code,message}} envelope with a non-empty
// code + message and a 4xx/502 status — never a bare 500 and never an
// empty body. Table-driven so a new failure mode is one row away.
func TestOllamaChat_FailureModes_ReturnWellFormedEnvelope(t *testing.T) {
	type tcase struct {
		name        string
		provider    bool // wire a chat provider?
		providerErr error
		nilResp     bool
		body        string
		wantStatus  int
		wantCode    string
	}
	cases := []tcase{
		{
			name:       "no_provider_503",
			provider:   false,
			body:       `{"stream":false,"messages":[{"role":"user","content":"hi"}]}`,
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   "CHAT_NOT_CONFIGURED",
		},
		{
			name:       "malformed_json_400",
			provider:   true,
			body:       `{"messages":[`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "INVALID_JSON",
		},
		{
			name:       "empty_messages_400",
			provider:   true,
			body:       `{"stream":false,"messages":[]}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "EMPTY_MESSAGES",
		},
		{
			name:        "provider_error_502",
			provider:    true,
			providerErr: errors.New("upstream exploded"),
			body:        `{"stream":false,"messages":[{"role":"user","content":"hi"}]}`,
			wantStatus:  http.StatusBadGateway,
			wantCode:    "PROVIDER_ERROR",
		},
		{
			name:       "nil_response_502",
			provider:   true,
			nilResp:    true,
			body:       `{"stream":false,"messages":[{"role":"user","content":"hi"}]}`,
			wantStatus: http.StatusBadGateway,
			wantCode:   "PROVIDER_ERROR",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var s *Server
			if tc.provider {
				stub := &streamingStub{model: "stub", err: tc.providerErr}
				if !tc.nilResp && tc.providerErr == nil {
					stub.finalResp = buildOllamaOKResponse("stub", "ok")
				}
				s = NewServer(WithChatProvider(stub), WithLogger(zerolog.Nop()))
			} else {
				s = NewServer(WithLogger(zerolog.Nop()))
			}
			req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewBufferString(tc.body))
			rr := httptest.NewRecorder()
			s.OllamaChat(rr, req)

			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d — body=%s", rr.Code, tc.wantStatus, rr.Body.String())
			}
			if rr.Code == http.StatusInternalServerError {
				t.Fatal("must never return a bare 500 for a routed failure mode")
			}
			code, msg := eeDecodeErrorEnvelope(t, rr.Body.Bytes())
			if code != tc.wantCode {
				t.Errorf("error.code = %q, want %q", code, tc.wantCode)
			}
			if msg == "" {
				t.Error("error.message must be non-empty so the client can render a reason")
			}
		})
	}
}

// TestChatProxy_FailureModes_ReturnWellFormedEnvelope is the
// /v1/chat/completions counterpart. Same contract: every failure
// mode is a typed envelope, never a bare 500. Also pins the
// 2026-05-29 sanitization rule on the provider-error path (raw
// upstream text must NOT leak to the wire) at the envelope level.
func TestChatProxy_FailureModes_ReturnWellFormedEnvelope(t *testing.T) {
	const secretErr = "upstream said: api_key=sk-leak-me"
	type tcase struct {
		name        string
		provider    bool
		providerErr error
		nilResp     bool
		oversize    bool
		body        string
		wantStatus  int
		wantCode    string
		forbidLeak  string // substring that must NOT appear in the body
	}
	cases := []tcase{
		{
			name:       "no_provider_503",
			provider:   false,
			body:       `{"messages":[{"role":"user","content":"hi"}]}`,
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   "CHAT_NOT_CONFIGURED",
		},
		{
			name:       "malformed_json_400",
			provider:   true,
			body:       `{"messages":[`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "INVALID_JSON",
		},
		{
			name:       "empty_messages_400",
			provider:   true,
			body:       `{"messages":[]}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "EMPTY_MESSAGES",
		},
		{
			name:       "streaming_unsupported_400",
			provider:   true,
			body:       `{"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "STREAMING_NOT_SUPPORTED",
		},
		{
			name:        "provider_error_502_sanitized",
			provider:    true,
			providerErr: errors.New(secretErr),
			body:        `{"messages":[{"role":"user","content":"hi"}]}`,
			wantStatus:  http.StatusBadGateway,
			wantCode:    "PROVIDER_ERROR",
			forbidLeak:  "sk-leak-me",
		},
		{
			name:       "nil_response_502",
			provider:   true,
			nilResp:    true,
			body:       `{"messages":[{"role":"user","content":"hi"}]}`,
			wantStatus: http.StatusBadGateway,
			wantCode:   "PROVIDER_ERROR",
		},
		{
			name:       "oversized_body_413",
			provider:   true,
			oversize:   true,
			wantStatus: http.StatusRequestEntityTooLarge,
			wantCode:   "BODY_TOO_LARGE",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var s *Server
			if tc.provider {
				stub := &streamingStub{model: "stub", err: tc.providerErr}
				if !tc.nilResp && tc.providerErr == nil {
					stub.finalResp = buildOllamaOKResponse("stub", "ok")
				}
				s = NewServer(WithChatProvider(stub), WithLogger(zerolog.Nop()))
			} else {
				s = NewServer(WithLogger(zerolog.Nop()))
			}

			var reqBody *bytes.Reader
			if tc.oversize {
				// One byte over the cap: a valid-ish JSON wrapper around
				// a huge string so the size check fires before the JSON
				// decode would.
				filler := bytes.Repeat([]byte{'a'}, maxChatProxyBodyBytes+1)
				reqBody = bytes.NewReader(filler)
			} else {
				reqBody = bytes.NewReader([]byte(tc.body))
			}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", reqBody)
			rr := httptest.NewRecorder()
			s.ChatCompletions(rr, req)

			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d — body=%s", rr.Code, tc.wantStatus, rr.Body.String())
			}
			if rr.Code == http.StatusInternalServerError {
				t.Fatal("must never return a bare 500 for a routed failure mode")
			}
			code, msg := eeDecodeErrorEnvelope(t, rr.Body.Bytes())
			if code != tc.wantCode {
				t.Errorf("error.code = %q, want %q", code, tc.wantCode)
			}
			if msg == "" {
				t.Error("error.message must be non-empty")
			}
			if tc.forbidLeak != "" && strings.Contains(rr.Body.String(), tc.forbidLeak) {
				t.Errorf("sanitization breach: raw upstream substring %q leaked into the wire envelope", tc.forbidLeak)
			}
		})
	}
}

// tail returns the last few characters of s for diagnostic output,
// safe on short/multibyte strings.
func eeTail(s string) string {
	r := []rune(s)
	if len(r) <= 6 {
		return s
	}
	return string(r[len(r)-6:])
}
