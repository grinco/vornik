// HIGH-VALUE edge-case coverage for the Telegram channel's
// session-command, inline-keyboard, and callback-dispatch surfaces.
//
// These tests deliberately target branches the existing suite leaves
// uncovered (verified via -coverprofile against current behavior):
//   - handleCallbackQuery guard branches: nil query, unauthorized
//     user, rate-limit rejection, handler-error propagation.
//   - handleProjectCallback IDOR guards: unknown-project rejection and
//     access-denial via the per-user project allowlist.
//   - KeyboardGrid oversized-callback panic (the grid path; the
//     one-col path is already covered).
//   - DecodeCallback / EncodeCallback boundary cases (empty payload is
//     legal; exactly-64-byte encode is legal; 65 is not).
//   - Session-command argument validation that the existing tests
//     don't pin: /forget bounds (N>len, zero, negative), /pin
//     whitespace, /save multi-word name keeps only the first token,
//     /load and /search disabled-with-arg paths, /context formatting
//     with an active project + pinned instruction.
//   - handleNew clears the in-memory conversation.
//
// TESTS ONLY — no production changes. Prefix: TestSCE_ / TestKbd_ /
// TestCb_ so the file can be iterated with
//   go test ./internal/telegram/ -run 'TestSCE_|TestKbd_|TestCb_'

package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/registry"
)

// ---------------------------------------------------------------------
// Inline-keyboard primitives — uncovered branches.
// ---------------------------------------------------------------------

// The grid constructor must fail loudly (panic) on oversized
// callback_data exactly like the one-col path — otherwise Telegram
// silently strips the button at delivery and the operator sees a
// keyboard with a missing cell.
func TestKbd_GridPanicsOnOversizedCallback(t *testing.T) {
	huge := Button{Text: "X", Data: strings.Repeat("a", inlineButtonMaxBytes+1)}
	defer func() {
		r := recover()
		require.NotNil(t, r, "grid must panic on oversized callback_data, not strip silently")
		assert.Contains(t, r.(string), "exceeds Telegram cap")
	}()
	_ = KeyboardGrid(2, Button{Text: "ok", Data: "ns:a:1"}, huge)
}

// A 2-col grid with an odd button count lays out row-major with a
// trailing single-button row, and the partial row is emitted (the
// `len(current) > 0` flush branch).
func TestKbd_GridThreeButtonsTwoCols(t *testing.T) {
	kb := KeyboardGrid(2,
		Button{Text: "A", Data: "ns:a:1"},
		Button{Text: "B", Data: "ns:a:2"},
		Button{Text: "C", Data: "ns:a:3"},
	)
	require.Len(t, kb.InlineKeyboard, 2, "3 buttons / 2 cols → 2 rows")
	require.Len(t, kb.InlineKeyboard[0], 2)
	require.Len(t, kb.InlineKeyboard[1], 1, "trailing partial row carries the remainder")
	assert.Equal(t, "C", kb.InlineKeyboard[1][0].Text)
}

// EncodeCallback at exactly the 64-byte cap is legal; one byte over is
// rejected. Pins the boundary so a future off-by-one in the length
// guard fails this test.
func TestKbd_EncodeBoundaryAtCap(t *testing.T) {
	// "p:s:" is 4 bytes; pad the payload so the total is exactly 64.
	payload := strings.Repeat("x", inlineButtonMaxBytes-len("p:s:"))
	out, err := EncodeCallback("p", "s", payload)
	require.NoError(t, err, "exactly %d bytes must be accepted", inlineButtonMaxBytes)
	assert.Len(t, out, inlineButtonMaxBytes)

	_, err = EncodeCallback("p", "s", payload+"x")
	require.Error(t, err, "one byte over the cap must be rejected")
}

// An empty payload is legal: only ns and action are required to be
// non-empty (the dispatcher routes on ns/action; payload can be a
// no-arg sub-action like "project:menu:").
func TestKbd_DecodeEmptyPayloadIsOK(t *testing.T) {
	ns, action, payload, ok := DecodeCallback("project:menu:")
	require.True(t, ok, "trailing-empty payload must still decode")
	assert.Equal(t, "project", ns)
	assert.Equal(t, "menu", action)
	assert.Equal(t, "", payload)
}

// EncodeCallback rejects a delimiter embedded in the payload only at
// the structural positions — payload delimiters are preserved by
// DecodeCallback. Confirm the encode/decode contract for a payload
// that itself carries a colon (encode succeeds, decode preserves it).
func TestKbd_EncodeDecodeColonInPayload(t *testing.T) {
	enc, err := EncodeCallback("trade", "approve", "ord:rev2")
	require.NoError(t, err)
	ns, action, payload, ok := DecodeCallback(enc)
	require.True(t, ok)
	assert.Equal(t, "trade", ns)
	assert.Equal(t, "approve", action)
	assert.Equal(t, "ord:rev2", payload, "payload colons survive the round-trip")
}

// ---------------------------------------------------------------------
// handleCallbackQuery — guard branches.
// ---------------------------------------------------------------------

// A nil CallbackQuery is a no-op (no panic, no outbound call). Defends
// the early-return guard at the top of the dispatcher.
func TestCb_NilQueryIsNoOp(t *testing.T) {
	rig := newCallbackRig(t)
	require.NoError(t, rig.bot.handleCallbackQuery(context.Background(), nil))
	assert.Empty(t, rig.callsTo("answerCallbackQuery"))
	assert.Empty(t, rig.callsTo("sendMessage"))
}

// An unauthorized user's click must be rejected at the SAME gate as a
// text message: an alert toast, no routing to the project handler.
func TestCb_UnauthorizedUserRejectedWithAlert(t *testing.T) {
	rig := newCallbackRig(t)
	// Lock the bot down to a single allowlisted user; the click comes
	// from a DIFFERENT user id.
	rig.bot.config.AllowedUsers = map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}}

	cq := buildCallback(t, "cb-unauth", 100, 999, "project:select:my-asst")
	require.NoError(t, rig.bot.handleCallbackQuery(context.Background(), cq))

	acks := rig.callsTo("answerCallbackQuery")
	require.Len(t, acks, 1)
	assert.Contains(t, string(acks[0].body), "not authorized")
	assert.Empty(t, rig.callsTo("sendMessage"),
		"a denied click must not reach the project handler's confirmation send")
	assert.Empty(t, rig.bot.getActiveProject(100),
		"a denied click must not flip the active project")
}

// The callback surface is rate-limited just like text input. When the
// limit is exhausted the click is ack'd with a rate-limit toast and
// never reaches the action handler.
func TestCb_RateLimitedClickRejected(t *testing.T) {
	rig := newCallbackRig(t)
	rig.bot.config.AllowedUsers = map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}}
	rig.bot.config.RateLimit = 1

	cq := buildCallback(t, "cb-rl", 100, 42, "project:select:my-asst")
	// First click consumes the single-request budget.
	require.NoError(t, rig.bot.handleCallbackQuery(context.Background(), cq))
	// Second click is over the limit.
	require.NoError(t, rig.bot.handleCallbackQuery(context.Background(), cq))

	acks := rig.callsTo("answerCallbackQuery")
	require.GreaterOrEqual(t, len(acks), 2)
	last := string(acks[len(acks)-1].body)
	assert.Contains(t, last, "Rate limit exceeded")
}

// When the per-namespace handler returns an error (here forced by
// pointing the bot at a server that 500s the ack), handleCallbackQuery
// surfaces the error to the caller AND makes a best-effort error ack.
// Defends the err != nil tail of the dispatcher.
func TestCb_HandlerErrorPropagates(t *testing.T) {
	// Bot pointed at a server that 500s every request. The
	// project:select handler's inner answerCallbackQuery then fails,
	// and the dispatcher must propagate that error to the caller
	// (rather than swallowing it and leaving the button "loading").
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	chatClient := chat.NewClient("https://api.example.com", "test-key", "gpt-4")
	bad, err := NewBot(BotConfig{Token: "test-token"}, chatClient, WithHTTPClient(server.Client()))
	require.NoError(t, err)
	bad.baseURL = server.URL

	// An unknown project action takes the handler's `return
	// answerCallbackQuery(...)` path (unlike select, which discards its
	// ack error). With the ack failing 500, the error must reach the
	// dispatcher and then the caller.
	cq := buildCallback(t, "cb-err", 100, 0, "project:badaction:x")
	dispErr := bad.handleCallbackQuery(context.Background(), cq)
	require.Error(t, dispErr, "an ack HTTP failure inside the handler must propagate, not be swallowed")
	assert.Contains(t, dispErr.Error(), "HTTP 500")
}

// ---------------------------------------------------------------------
// handleProjectCallback — IDOR guards.
// ---------------------------------------------------------------------

// project:select for a project that isn't in the registry is rejected
// with an "Unknown project" toast and does NOT flip the active
// project. Mirrors the /project text-command existence check.
func TestCb_ProjectSelectUnknownProjectRejected(t *testing.T) {
	rig := newCallbackRig(t)
	rig.bot.registry = loadOneProjectRegistry(t, "real-proj")

	cq := buildCallback(t, "cb-unknown", 100, 0, "project:select:ghost-proj")
	require.NoError(t, rig.bot.handleCallbackQuery(context.Background(), cq))

	acks := rig.callsTo("answerCallbackQuery")
	require.Len(t, acks, 1)
	assert.Contains(t, string(acks[0].body), "Unknown project")
	assert.Empty(t, rig.bot.getActiveProject(100),
		"an unknown project must not be pinned via a crafted/stale callback")
}

// project:select for a project the user is NOT cleared for is rejected
// with an authorization toast — the callback path must enforce the
// same per-user scope as the text path (no IDOR divergence).
func TestCb_ProjectSelectAccessDeniedRejected(t *testing.T) {
	rig := newCallbackRig(t)
	rig.bot.registry = loadOneProjectRegistry(t, "secret-proj")
	// User 42 is allowed onto the bot but scoped to a different project.
	rig.bot.config.AllowedUsers = map[int64]UserAccess{
		42: {Allowed: true, Projects: []string{"other-proj"}},
	}

	cq := buildCallback(t, "cb-denied", 100, 42, "project:select:secret-proj")
	require.NoError(t, rig.bot.handleCallbackQuery(context.Background(), cq))

	acks := rig.callsTo("answerCallbackQuery")
	require.Len(t, acks, 1)
	assert.Contains(t, string(acks[0].body), "not authorized for project")
	assert.Empty(t, rig.bot.getActiveProject(100),
		"a project outside the user's scope must not be pinned via callback")
}

// ---------------------------------------------------------------------
// Session-command argument validation — uncovered edges.
// ---------------------------------------------------------------------

// /forget N where N exceeds the conversation length clamps to the
// available count and reports 0 remaining (DropLast's n>len clamp).
func TestSCE_ForgetMoreThanLengthClamps(t *testing.T) {
	b, rec := newBotWithRecorder(t, BotConfig{
		Token:        "tok",
		AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}},
	})
	conv := b.getConversation(100)
	for i := 0; i < 3; i++ {
		conv.AddMessage(chat.Message{Role: "user", Content: "x"})
	}
	snap := runCommand(t, b, rec, 100, 42, "/forget 100")
	assert.Contains(t, snap[0].Text, "Dropped 3")
	assert.Contains(t, snap[0].Text, "0 remaining")
	assert.Equal(t, 0, conv.Len())
}

// /forget 0 is treated as invalid (the n <= 0 guard rejects it before
// touching the conversation).
func TestSCE_ForgetZeroRejected(t *testing.T) {
	b, rec := newBotWithRecorder(t, BotConfig{
		Token:        "tok",
		AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}},
	})
	snap := runCommand(t, b, rec, 100, 42, "/forget 0")
	assert.Contains(t, snap[0].Text, "positive integer")
}

// /forget with a negative number is rejected the same way (Atoi
// succeeds but the n <= 0 guard catches it).
func TestSCE_ForgetNegativeRejected(t *testing.T) {
	b, rec := newBotWithRecorder(t, BotConfig{
		Token:        "tok",
		AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}},
	})
	snap := runCommand(t, b, rec, 100, 42, "/forget -5")
	assert.Contains(t, snap[0].Text, "positive integer")
}

// /pin keeps the exact instruction text (the TrimPrefix only strips
// the leading "/pin " token, preserving internal spacing/casing).
func TestSCE_PinPreservesExactText(t *testing.T) {
	b, rec := newBotWithRecorder(t, BotConfig{
		Token:        "tok",
		AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}},
	})
	snap := runCommand(t, b, rec, 100, 42, "/pin   Always  reply  in JSON")
	assert.Contains(t, snap[0].Text, "persists across /new resets")
	pins := b.getConversation(100).PinnedMessages()
	require.Len(t, pins, 1)
	assert.Equal(t, "system", pins[0].Role, "pinned instructions are pinned as system messages")
	assert.Equal(t, "  Always  reply  in JSON", pins[0].Content,
		"only the '/pin ' prefix is stripped; the remaining text is preserved verbatim")
}

// /save with a multi-word argument uses only the first whitespace
// token as the save name (parts[1]) — the rest is ignored. Pins the
// arg-tokenisation contract so a name like "thread one" saves as
// "thread".
func TestSCE_SaveMultiWordUsesFirstToken(t *testing.T) {
	dir := t.TempDir()
	b, rec := newBotWithRecorder(t, BotConfig{
		Token:        "tok",
		SessionPath:  filepath.Join(dir, "s.json"),
		AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}},
	})
	conv := b.getConversation(100)
	conv.AddMessage(chat.Message{Role: "user", Content: "hi"})

	snap := runCommand(t, b, rec, 100, 42, "/save thread one two")
	last := snap[len(snap)-1].Text
	assert.Contains(t, last, `"thread"`, "only the first token becomes the save name")
	assert.Contains(t, last, "/load thread")

	// Confirm the save is retrievable under exactly "thread".
	names, err := chat.ListNamedSaves(b.config.SessionPath, 100)
	require.NoError(t, err)
	assert.Contains(t, names, "thread")
}

// /load <name> when session persistence is disabled (no SessionPath)
// reports the disabled message rather than attempting a filesystem
// read. Distinct from the no-arg list path (which lists names).
func TestSCE_LoadWithArgDisabledReportsDisabled(t *testing.T) {
	b, rec := newBotWithRecorder(t, BotConfig{
		Token:        "tok",
		AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}},
	})
	snap := runCommand(t, b, rec, 100, 42, "/load some-name")
	last := snap[len(snap)-1].Text
	assert.Contains(t, last, "disabled",
		"/load <name> with no session_path must report persistence disabled")
}

// /context with an active project and a pinned instruction renders the
// project name (not "(none)") and a non-zero Pinned count. Pins the
// formatting of the populated branch.
func TestSCE_ContextWithActiveProjectAndPin(t *testing.T) {
	b, rec := newBotWithRecorder(t, BotConfig{
		Token:        "tok",
		AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}},
	})
	b.setActiveProject(100, "trader-1")
	conv := b.getConversation(100)
	conv.AddMessage(chat.Message{Role: "user", Content: "hello"})
	conv.Pin(chat.Message{Role: "system", Content: "be terse"})

	snap := runCommand(t, b, rec, 100, 42, "/context")
	out := snap[0].Text
	assert.Contains(t, out, "Active project: trader-1")
	assert.Contains(t, out, "Pinned: 1")
	assert.NotContains(t, out, "(none)", "an active project must surface its name, not the placeholder")
	assert.Contains(t, out, "Persona: dispatcher", "no lead role wired → dispatcher persona")
}

// handleNew clears the in-memory conversation for the chat and confirms a
// fresh session.
func TestSCE_NewResetsConversation(t *testing.T) {
	b, rec := newBotWithRecorder(t, BotConfig{
		Token:        "tok",
		AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}},
	})
	conv := b.getConversation(100)
	conv.AddMessage(chat.Message{Role: "user", Content: "stuff"})
	require.Positive(t, conv.Len())

	snap := runCommand(t, b, rec, 100, 42, "/new")
	assert.Contains(t, snap[0].Text, "New session started")
	assert.Equal(t, 0, b.getConversation(100).Len(),
		"/new must clear the in-memory conversation")
}

// project:select with a whitespace-only payload trims to empty and is
// rejected with the "Missing project ID" toast — the TrimSpace guard
// must catch a payload that's syntactically present but blank.
func TestCb_ProjectSelectWhitespacePayloadRejected(t *testing.T) {
	rig := newCallbackRig(t)
	err := rig.bot.handleProjectCallback(context.Background(), 100, 0, "cb-ws", "select", "   ")
	require.NoError(t, err)
	acks := rig.callsTo("answerCallbackQuery")
	require.Len(t, acks, 1)
	assert.Contains(t, string(acks[0].body), "Missing project ID")
	assert.Empty(t, rig.bot.getActiveProject(100))
}

// /save whose write fails (SessionPath points at a path that can't be
// created — a file used as a directory) surfaces the failure to the
// operator rather than reporting a phantom success.
func TestSCE_SaveWriteFailureReported(t *testing.T) {
	dir := t.TempDir()
	// Create a regular FILE, then use it as if it were the session
	// directory root — SaveNamedConversation's mkdir/open underneath
	// it must fail.
	blocker := filepath.Join(dir, "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o644))

	b, rec := newBotWithRecorder(t, BotConfig{
		Token:        "tok",
		SessionPath:  filepath.Join(blocker, "sessions.json"),
		AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}},
	})
	conv := b.getConversation(100)
	conv.AddMessage(chat.Message{Role: "user", Content: "hi"})

	snap := runCommand(t, b, rec, 100, 42, "/save thread-x")
	last := snap[len(snap)-1].Text
	assert.Contains(t, last, "Save failed",
		"a write error must be surfaced, not masked as a successful save")
}

// KeyboardOneCol / KeyboardGrid with zero buttons produce an empty
// keyboard (no rows) rather than panicking — a defensive shape for a
// caller that filtered every candidate out.
func TestKbd_EmptyButtonSetProducesNoRows(t *testing.T) {
	one := KeyboardOneCol()
	assert.Empty(t, one.InlineKeyboard, "no buttons → no rows in one-col")

	grid := KeyboardGrid(3)
	assert.Empty(t, grid.InlineKeyboard, "no buttons → no rows in grid")
}

// DecodeCallback treats a bare delimiter and truncated shapes as
// malformed (ok=false) — extends the existing malformed-table with
// shapes that stress the second-delimiter search specifically.
func TestKbd_DecodeMoreMalformedShapes(t *testing.T) {
	for _, data := range []string{
		":",    // single delimiter, empty ns and (missing) action
		"a:",   // ns present but no second delimiter at all
		":b:c", // empty ns even though both delimiters exist
	} {
		t.Run(data, func(t *testing.T) {
			_, _, _, ok := DecodeCallback(data)
			assert.False(t, ok, "%q must not decode", data)
		})
	}

	// Contrast: a whitespace ns is non-empty per the decoder's check,
	// so it DOES decode — the decoder is structural, not semantic.
	ns, action, payload, ok := DecodeCallback(" :b:c")
	require.True(t, ok, "whitespace ns is structurally non-empty, so it decodes")
	assert.Equal(t, " ", ns)
	assert.Equal(t, "b", action)
	assert.Equal(t, "c", payload)
}

// ---------------------------------------------------------------------
// helpers local to this file
// ---------------------------------------------------------------------

// loadOneProjectRegistry builds a minimal-but-valid registry holding a
// single project with the given ID, so GetProject(id) is non-nil and
// the callback existence guard can be exercised.
func loadOneProjectRegistry(t *testing.T, projectID string) *registry.Registry {
	t.Helper()
	root := t.TempDir()
	for _, sub := range []string{"projects", "swarms", "workflows"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, sub), 0o755))
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "projects", "p.yaml"),
		[]byte("projectId: "+projectID+"\nswarmId: swarm-1\ndefaultWorkflowId: wf-1\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "swarms", "s.md"),
		[]byte("---\nswarmId: swarm-1\nroles:\n  - name: worker\n    runtime:\n      image: fake-agent\n---\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "workflows", "wf.md"),
		[]byte("---\nworkflowId: wf-1\nentrypoint: run\nsteps:\n  run:\n    type: agent\n    role: worker\n    prompt: \"do work\"\n    on_success: done\nterminals:\n  done:\n    status: COMPLETED\n---\n"), 0o644))
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	return reg
}
