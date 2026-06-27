package ui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/dispatcher"
)

// stubChatDispatcher implements dispatcher.Doer for the chat
// UI tests. Returns a canned reply and captures the request the
// receiver passed in so assertions can verify the user turn was
// appended to history.
type stubChatDispatcher struct {
	mu       sync.Mutex
	reply    string
	captured []dispatcher.Request
}

func (s *stubChatDispatcher) Process(_ context.Context, req dispatcher.Request) dispatcher.Result {
	s.mu.Lock()
	s.captured = append(s.captured, req)
	s.mu.Unlock()
	return dispatcher.Result{
		Text:     s.reply,
		Messages: append(req.Messages, chat.Message{Role: "assistant", Content: s.reply}),
	}
}

func (s *stubChatDispatcher) ProcessStreaming(ctx context.Context, req dispatcher.Request, _ chat.StreamCallback) dispatcher.Result {
	return s.Process(ctx, req)
}

// chatTestServer wires a Server with a stub dispatcher and the
// swarm-fixture registry (which has a project "p1"). The fixture
// includes the embedded chat template + nav templates so the
// rendered output is exercised end-to-end.
func chatTestServer(t *testing.T, reply string) (*Server, *stubChatDispatcher) {
	t.Helper()
	root := writeSwarmFixture(t)
	server, _ := swarmEditServer(t, root)
	stub := &stubChatDispatcher{reply: reply}
	server.chatDispatcher = stub
	return server, stub
}

// TestChatPage_RendersComposer — GET /chat shows the empty
// state, the composer, and a session cookie. Operator-facing
// labels carry data-testid attributes so the tests don't break
// on copy edits.
func TestChatPage_RendersComposer(t *testing.T) {
	server, _ := chatTestServer(t, "")
	req := httptest.NewRequest(http.MethodGet, "/projects/p1/chat", nil)
	rec := httptest.NewRecorder()
	server.ChatPage(rec, req, "p1")

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `data-testid="chat-composer"`)
	assert.Contains(t, body, `data-testid="chat-send-button"`)
	assert.Contains(t, body, `data-testid="chat-empty-state"`)
	assert.Contains(t, body, `action="/ui/projects/p1/chat/messages"`)

	// Session cookie must be issued on the first GET.
	cookies := rec.Result().Cookies()
	var found *http.Cookie
	for _, c := range cookies {
		if c.Name == chatSessionCookie {
			found = c
		}
	}
	require.NotNil(t, found, "GET /chat must set the session cookie on first hit")
	assert.NotEmpty(t, found.Value)
	assert.True(t, found.HttpOnly, "session cookie must be HttpOnly")
}

// TestChatPostMessage_DispatchesAndRendersReply — happy path: a
// non-empty prompt POSTs, the stub dispatcher returns "hello
// from the agent", and the re-rendered page shows both the user
// turn and the assistant reply.
func TestChatPostMessage_DispatchesAndRendersReply(t *testing.T) {
	server, stub := chatTestServer(t, "hello from the agent")
	form := url.Values{}
	form.Set("prompt", "what's up?")
	req := httptest.NewRequest(http.MethodPost, "/projects/p1/chat/messages", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// Seed a session cookie so the same store bucket is hit on
	// the re-render.
	req.AddCookie(&http.Cookie{Name: chatSessionCookie, Value: "test-session"})

	rec := httptest.NewRecorder()
	server.ChatPostMessage(rec, req, "p1")

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "what&#39;s up?", "user turn must render in the message list (HTML-escaped)")
	assert.Contains(t, body, "hello from the agent", "assistant reply must render")

	// The dispatcher saw the right project + user message.
	require.Len(t, stub.captured, 1)
	got := stub.captured[0]
	assert.Equal(t, "p1", got.Project)
	if assert.NotEmpty(t, got.Messages) {
		assert.Equal(t, "user", got.Messages[len(got.Messages)-1].Role)
		assert.Equal(t, "what's up?", got.Messages[len(got.Messages)-1].Content)
	}
}

// TestChatPostMessage_EmptyPromptRendersError — an empty prompt
// is the operator hitting Send by accident; render an inline
// error rather than dispatching a no-op turn.
func TestChatPostMessage_EmptyPromptRendersError(t *testing.T) {
	server, stub := chatTestServer(t, "should not fire")
	form := url.Values{}
	form.Set("prompt", "   ")
	req := httptest.NewRequest(http.MethodPost, "/projects/p1/chat/messages", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: chatSessionCookie, Value: "test-session"})

	rec := httptest.NewRecorder()
	server.ChatPostMessage(rec, req, "p1")

	assert.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "Prompt is empty")
	assert.Empty(t, stub.captured, "empty prompt must NOT reach the dispatcher")
}

// TestChatPage_DisabledWhenNoDispatcher — a deployment without
// a chat dispatcher shows a banner and a disabled composer
// instead of crashing.
func TestChatPage_DisabledWhenNoDispatcher(t *testing.T) {
	root := writeSwarmFixture(t)
	server, _ := swarmEditServer(t, root) // no WithChatDispatcher

	req := httptest.NewRequest(http.MethodGet, "/projects/p1/chat", nil)
	rec := httptest.NewRecorder()
	server.ChatPage(rec, req, "p1")

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "Chat is not configured")
	assert.Contains(t, body, "disabled")
}

// TestChatPostMessage_DispatcherErrorSurfacedInBanner — when
// the receiver returns an error (project not found in the
// session store path, for example) the page re-renders with the
// error banner instead of returning 500.
func TestChatPostMessage_DispatcherErrorSurfacedInBanner(t *testing.T) {
	// Use a dispatcher that returns an error on the result. The
	// receiver propagates it.
	stub := &erroringDispatcher{}
	root := writeSwarmFixture(t)
	server, _ := swarmEditServer(t, root)
	server.chatDispatcher = stub

	form := url.Values{}
	form.Set("prompt", "hi")
	req := httptest.NewRequest(http.MethodPost, "/projects/p1/chat/messages", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: chatSessionCookie, Value: "test-session-err"})

	rec := httptest.NewRecorder()
	server.ChatPostMessage(rec, req, "p1")

	assert.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "Chat dispatch failed")
}

// erroringDispatcher is a stub Doer whose Process returns a
// non-nil Err — the receiver surfaces that as the Receive return
// value.
type erroringDispatcher struct{}

func (e *erroringDispatcher) Process(_ context.Context, _ dispatcher.Request) dispatcher.Result {
	return dispatcher.Result{Err: errBoom}
}

func (e *erroringDispatcher) ProcessStreaming(ctx context.Context, req dispatcher.Request, _ chat.StreamCallback) dispatcher.Result {
	return e.Process(ctx, req)
}

// errBoom is a sentinel kept at package scope so the dispatcher
// stub doesn't allocate a fresh one per call.
var errBoom = chatTestErr("boom")

type chatTestErr string

func (e chatTestErr) Error() string { return string(e) }

// TestChatStoreFor_CachesPerProject — repeated lookups for the
// same project hand out the same SessionStore so a follow-up
// POST sees the prior turn's history.
func TestChatStoreFor_CachesPerProject(t *testing.T) {
	server := &Server{}
	a := server.chatStoreFor("p1")
	b := server.chatStoreFor("p1")
	c := server.chatStoreFor("p2")

	assert.NotNil(t, a)
	assert.Same(t, a, b, "same project must reuse the existing store")
	assert.NotSame(t, a, c, "different projects must get distinct stores")
}

// TestNewChatSessionID_HexAndLen — the cookie value is a 32-char
// hex string (16 bytes encoded). Empty strings would let two
// browsers share history; this guard catches a future refactor
// that breaks the random source.
func TestNewChatSessionID_HexAndLen(t *testing.T) {
	id := newChatSessionID()
	assert.NotEmpty(t, id)
	assert.GreaterOrEqual(t, len(id), 30, "session id must be at least 30 hex chars to be unique")
	// Two consecutive ids must differ.
	other := newChatSessionID()
	assert.NotEqual(t, id, other, "session ids must vary across calls")
}

func TestNewChatSessionID_FailsClosedOnRandError(t *testing.T) {
	orig := chatRandRead
	chatRandRead = func(_ []byte) (int, error) { return 0, errors.New("entropy unavailable") }
	defer func() { chatRandRead = orig }()

	assert.Equal(t, "", newChatSessionID(), "rand failure must not fall back to predictable IDs")

	server, _ := chatTestServer(t, "")
	req := httptest.NewRequest(http.MethodGet, "/projects/p1/chat", nil)
	rec := httptest.NewRecorder()
	server.ChatPage(rec, req, "p1")
	assert.Empty(t, rec.Result().Cookies(), "no session cookie should be issued without entropy")
	assert.Contains(t, rec.Body.String(), "Chat session could not be created securely")
}

// TestRenderDeliverableLinksForWebChat — bridge helper renders
// the same shape as the conversation package's helper. Smoke
// test that the wiring stays connected.
func TestRenderDeliverableLinksForWebChat(t *testing.T) {
	got := renderDeliverableLinksForWebChat("https://x", "p1", []string{"deliverable.md"})
	assert.Contains(t, got, "Download: deliverable.md")
	assert.Contains(t, got, "https://x/ui/projects/p1/artifacts/raw?path=deliverable.md")

	empty := renderDeliverableLinksForWebChat("", "p1", nil)
	assert.Equal(t, "", empty)
}

// TestChatPage_NoTierBadgeWhenBudgetUnset — Slice 4 contract: a
// deployment without a chatContextBudget renders no badge so the
// chat surface stays byte-compatible with the legacy version. Pins
// the opt-out path.
func TestChatPage_NoTierBadgeWhenBudgetUnset(t *testing.T) {
	server, _ := chatTestServer(t, "")
	// Default chatContextBudget = 0 (option not applied).
	req := httptest.NewRequest(http.MethodGet, "/projects/p1/chat", nil)
	rec := httptest.NewRecorder()
	server.ChatPage(rec, req, "p1")

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.NotContains(t, body, `data-testid="chat-context-tier-badge"`,
		"no budget → no badge")
}

// TestChatPage_RendersTierBadgeWhenBudgetSet — happy path: budget
// configured + a session with history pre-seeded so the tier is
// computed against a real conversation. The rendered HTML carries
// the badge with the right data-tier attribute and the headroom %
// in the tooltip.
func TestChatPage_RendersTierBadgeWhenBudgetSet(t *testing.T) {
	server, _ := chatTestServer(t, "")
	server.chatContextBudget = 1_000
	// Pre-seed history that consumes ~80% of the budget — DEGRADING.
	store := server.chatStoreFor("p1")
	store.ContextBudget = 1_000 // re-stamp since the store was lazily made before the field change
	require.NoError(t, store.Append(context.Background(), channelMsg("sess-tier"),
		dispatcherResultWith([]chat.Message{
			{Role: "user", Content: strings.Repeat("x", 3_200)}, // 800 tokens
		})))

	req := httptest.NewRequest(http.MethodGet, "/projects/p1/chat", nil)
	req.AddCookie(&http.Cookie{Name: chatSessionCookie, Value: "sess-tier"})
	rec := httptest.NewRecorder()
	server.ChatPage(rec, req, "p1")

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `data-testid="chat-context-tier-badge"`)
	assert.Contains(t, body, `data-tier="degrading"`,
		"80%% used (3200 chars / 4 / 1000 budget) lands in the DEGRADING band")
	assert.Contains(t, body, "20% remaining",
		"tooltip must surface the exact headroom %% so operators don't have to guess the band")
}

// TestChatPage_TierBadgePeakOnEmptyHistory — empty conversation +
// configured budget renders PEAK (green). Pins the boundary so a new
// session never accidentally shows a degraded badge.
func TestChatPage_TierBadgePeakOnEmptyHistory(t *testing.T) {
	server, _ := chatTestServer(t, "")
	server.chatContextBudget = 1_000
	store := server.chatStoreFor("p1")
	store.ContextBudget = 1_000

	req := httptest.NewRequest(http.MethodGet, "/projects/p1/chat", nil)
	req.AddCookie(&http.Cookie{Name: chatSessionCookie, Value: "sess-empty"})
	rec := httptest.NewRecorder()
	server.ChatPage(rec, req, "p1")

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `data-tier="peak"`)
	assert.Contains(t, body, "100% remaining")
}

// TestWithChatContextBudget_OptionWiring — the option stamps the
// budget onto the server; subsequent chatStoreFor calls propagate
// it to new SessionStores. Pins the wire so a future option re-
// ordering can't silently disable the tier surface.
func TestWithChatContextBudget_OptionWiring(t *testing.T) {
	s := &Server{}
	WithChatContextBudget(123_456)(s)
	assert.Equal(t, 123_456, s.chatContextBudget)
	WithChatContextBudget(0)(s)
	assert.Equal(t, 123_456, s.chatContextBudget,
		"zero must NOT clobber a previously-set budget")
	WithChatContextBudget(-1)(s)
	assert.Equal(t, 123_456, s.chatContextBudget,
		"negative must NOT clobber a previously-set budget")
}

// TestChatEstimateTokens_CharsOver4 — pure helper sanity check.
func TestChatEstimateTokens_CharsOver4(t *testing.T) {
	assert.Equal(t, 0, chatEstimateTokens(nil))
	assert.Equal(t, 0, chatEstimateTokens([]chat.Message{}))
	assert.Equal(t, 25, chatEstimateTokens([]chat.Message{
		{Content: strings.Repeat("x", 100)},
	}))
}

// dispatcherResultWith builds a dispatcher.Result that the webchat
// SessionStore.Append accepts. Only Messages is read; the other
// fields stay zero.
func dispatcherResultWith(msgs []chat.Message) dispatcher.Result {
	return dispatcher.Result{Messages: msgs}
}

// channelMsg is a one-line constructor for the conversation message
// shape the webchat SessionStore.Append reads at test time.
func channelMsg(sessionID string) conversation.ChannelMessage {
	return conversation.ChannelMessage{SessionID: sessionID, Source: "ui-test"}
}
