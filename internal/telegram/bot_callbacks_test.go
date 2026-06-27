package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/registry"
)

// callbackRig wires a bot against a stub Telegram API that
// records every outbound call so tests can assert on the
// answerCallbackQuery + sendMessage round-trips. The stub is
// permissive — any URL returns 200 with a minimal success
// envelope — so the assertions live in the test bodies rather
// than the rig.
type callbackRig struct {
	mu       sync.Mutex
	requests []recordedRequest
	server   *httptest.Server
	bot      *Bot
}

type recordedRequest struct {
	path string
	body []byte
}

func newCallbackRig(t *testing.T) *callbackRig {
	t.Helper()
	rig := &callbackRig{}
	rig.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rig.mu.Lock()
		rig.requests = append(rig.requests, recordedRequest{path: r.URL.Path, body: body})
		rig.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "answerCallbackQuery"):
			_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1,"chat":{"id":1}}}`))
		}
	}))
	t.Cleanup(rig.server.Close)

	chatClient := chat.NewClient("https://api.example.com", "test-key", "gpt-4")
	bot, err := NewBot(BotConfig{Token: "test-token"}, chatClient, WithHTTPClient(rig.server.Client()))
	require.NoError(t, err)
	bot.baseURL = rig.server.URL
	rig.bot = bot
	return rig
}

// callsTo returns the recorded requests whose URL path contains
// the given fragment — handy for "did this hit answerCallbackQuery".
func (r *callbackRig) callsTo(pathFragment string) []recordedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []recordedRequest
	for _, req := range r.requests {
		if strings.Contains(req.path, pathFragment) {
			out = append(out, req)
		}
	}
	return out
}

// buildCallback constructs the unnamed-struct shape that
// handleCallbackQuery accepts. Reaches through json roundtrip so
// the test doesn't have to redeclare the anonymous-struct fields
// every time.
func buildCallback(t *testing.T, id string, chatID, userID int64, data string) *struct {
	ID   string `json:"id"`
	From struct {
		ID       int64  `json:"id"`
		Username string `json:"username,omitempty"`
	} `json:"from"`
	Message *struct {
		ID   int64 `json:"message_id"`
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
	} `json:"message"`
	Data string `json:"data"`
} {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"id":   id,
		"from": map[string]any{"id": userID},
		"message": map[string]any{
			"message_id": 100,
			"chat":       map[string]any{"id": chatID},
		},
		"data": data,
	})
	require.NoError(t, err)
	type cq struct {
		ID   string `json:"id"`
		From struct {
			ID       int64  `json:"id"`
			Username string `json:"username,omitempty"`
		} `json:"from"`
		Message *struct {
			ID   int64 `json:"message_id"`
			Chat struct {
				ID int64 `json:"id"`
			} `json:"chat"`
		} `json:"message"`
		Data string `json:"data"`
	}
	var got cq
	require.NoError(t, json.Unmarshal(raw, &got))
	out := struct {
		ID   string `json:"id"`
		From struct {
			ID       int64  `json:"id"`
			Username string `json:"username,omitempty"`
		} `json:"from"`
		Message *struct {
			ID   int64 `json:"message_id"`
			Chat struct {
				ID int64 `json:"id"`
			} `json:"chat"`
		} `json:"message"`
		Data string `json:"data"`
	}(got)
	return &out
}

func TestHandleCallbackQuery_MalformedDataAcksWithStaleToast(t *testing.T) {
	rig := newCallbackRig(t)
	cq := buildCallback(t, "cb-1", 100, 200, "garbage-no-delimiters")
	err := rig.bot.handleCallbackQuery(context.Background(), cq)
	require.NoError(t, err, "malformed callbacks should be ack'd silently — no error propagation")

	// Exactly one ack call with the stale-button message; no
	// sendMessage follow-up.
	acks := rig.callsTo("answerCallbackQuery")
	require.Len(t, acks, 1)
	assert.Contains(t, string(acks[0].body), "older version of the bot")
	assert.Empty(t, rig.callsTo("sendMessage"),
		"malformed callback must NOT send a follow-up message")
}

func TestHandleCallbackQuery_UnknownNamespaceAcksWithError(t *testing.T) {
	rig := newCallbackRig(t)
	cq := buildCallback(t, "cb-2", 100, 200, "unknown:action:x")
	err := rig.bot.handleCallbackQuery(context.Background(), cq)
	require.NoError(t, err)

	acks := rig.callsTo("answerCallbackQuery")
	require.Len(t, acks, 1)
	assert.Contains(t, string(acks[0].body), "isn't recognised")
}

func TestHandleCallbackQuery_ProjectSelect_SwitchesActiveProject(t *testing.T) {
	rig := newCallbackRig(t)
	cq := buildCallback(t, "cb-3", 100, 200, "project:select:my-asst")
	require.NoError(t, rig.bot.handleCallbackQuery(context.Background(), cq))

	assert.Equal(t, "my-asst", rig.bot.getActiveProject(100),
		"the click must flip the chat's active project to the payload")
	// One ack toast + one follow-up confirmation message.
	require.Len(t, rig.callsTo("answerCallbackQuery"), 1)
	require.Len(t, rig.callsTo("sendMessage"), 1)
	follow := rig.callsTo("sendMessage")[0]
	assert.Contains(t, string(follow.body), "Active project")
	assert.Contains(t, string(follow.body), "my-asst")
	assert.NotContains(t, string(follow.body), "<b>",
		"sendMessage does not set parse_mode, so callback confirmations must not expose raw HTML tags")
}

func TestHandleProjectCallback_MissingPayloadRejected(t *testing.T) {
	rig := newCallbackRig(t)
	err := rig.bot.handleProjectCallback(context.Background(), 100, 200, "cb-x", "select", "")
	require.NoError(t, err, "missing payload is reported via ack toast, not a Go-level error")
	acks := rig.callsTo("answerCallbackQuery")
	require.Len(t, acks, 1)
	assert.Contains(t, string(acks[0].body), "Missing project ID")
	assert.Empty(t, rig.callsTo("sendMessage"),
		"refused select must NOT send a confirmation — the user shouldn't see a fake 'switched' message")
}

func TestHandleProjectCallback_UnknownActionRejected(t *testing.T) {
	rig := newCallbackRig(t)
	err := rig.bot.handleProjectCallback(context.Background(), 100, 200, "cb-x", "mystery", "payload")
	require.NoError(t, err)
	acks := rig.callsTo("answerCallbackQuery")
	require.Len(t, acks, 1)
	assert.Contains(t, string(acks[0].body), "Unknown project action")
}

func TestSendProjectPicker_RendersInlineKeyboard(t *testing.T) {
	rig := newCallbackRig(t)
	projects := []*registry.Project{
		{ID: "alpha", DisplayName: "Alpha"},
		{ID: "beta"},
		{ID: "gamma", DisplayName: "Gamma Project"},
	}
	require.NoError(t, rig.bot.sendProjectPicker(context.Background(), 100, projects))

	require.Len(t, rig.callsTo("sendMessage"), 1)
	body := string(rig.callsTo("sendMessage")[0].body)
	// Inline keyboard must be present with three buttons. The
	// JSON layout has reply_markup.inline_keyboard[][]; assert
	// on the embedded callback_data values so the assertion is
	// resilient to row layout changes.
	assert.Contains(t, body, "project:select:alpha")
	assert.Contains(t, body, "project:select:beta")
	assert.Contains(t, body, "project:select:gamma")
	// Display name preferred over slug when available.
	assert.Contains(t, body, `"Alpha"`)
	assert.Contains(t, body, `"Gamma Project"`)
	assert.Contains(t, body, `"beta"`, "slug used when DisplayName is empty")
}

func TestSendProjectPicker_SkipsProjectsWithTooLongIDs(t *testing.T) {
	rig := newCallbackRig(t)
	// One slug that pushes the encoded callback over the 64-byte
	// cap; one normal slug. The picker must skip the long one
	// rather than crashing.
	huge := strings.Repeat("a", 60)
	projects := []*registry.Project{
		{ID: huge},
		{ID: "ok"},
	}
	require.NoError(t, rig.bot.sendProjectPicker(context.Background(), 100, projects))

	body := string(rig.callsTo("sendMessage")[0].body)
	assert.NotContains(t, body, huge,
		"over-long ID must be skipped — operator can still type /project <id> manually")
	assert.Contains(t, body, "project:select:ok",
		"the workable project must still surface alongside the skipped one")
}

func TestSendProjectPicker_NoProjectsErrors(t *testing.T) {
	rig := newCallbackRig(t)
	err := rig.bot.sendProjectPicker(context.Background(), 100, nil)
	require.Error(t, err, "an empty picker should NOT be sent — caller handles the no-projects message itself")
}

// TestHandleProject_NoActiveAndNoProjects — operator just stood
// up vornik with no projects configured; /project replies with a
// docs-pointer message rather than crashing the empty picker.
func TestHandleProject_NoActiveAndNoProjects(t *testing.T) {
	chatClient := chat.NewClient("https://api.example.com", "test-key", "gpt-4")
	bot, err := NewBot(BotConfig{Token: "test-token"}, chatClient)
	require.NoError(t, err)
	// No registry wired → getProjectList returns nil.
	got := handleProject(context.Background(), bot, 100, 0)
	assert.Contains(t, got, "no projects are configured",
		"empty-deployment reply must point the operator at vornikctl init or the from-template endpoint")
}

// TestHandleProject_ActiveProjectReturnsConfirmation — the canonical
// happy path when a project is already pinned. Returns the legacy
// "Active project: X" prose; the picker is for the no-active case.
func TestHandleProject_ActiveProjectReturnsConfirmation(t *testing.T) {
	chatClient := chat.NewClient("https://api.example.com", "test-key", "gpt-4")
	bot, err := NewBot(BotConfig{Token: "test-token"}, chatClient)
	require.NoError(t, err)
	bot.setActiveProject(100, "my-proj")
	got := handleProject(context.Background(), bot, 100, 0)
	assert.Equal(t, "Active project: my-proj", got)
}

// TestHandleProject_NoActiveSendsPickerSuppressesText — when the
// inline-keyboard picker fires, the legacy text-response path
// MUST be skipped; otherwise the user sees a picker followed by
// a stale "use /project <id>" sentence. Builds a real registry
// from YAML so getProjectList returns populated state.
func TestHandleProject_NoActiveSendsPickerSuppressesText(t *testing.T) {
	rig := newCallbackRig(t)

	root := t.TempDir()
	for _, sub := range []string{"projects", "swarms", "workflows"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, sub), 0o755))
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "projects", "p.yaml"),
		[]byte("projectId: my-asst\ndisplayName: My Assistant\nswarmId: swarm-1\ndefaultWorkflowId: wf-1\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "swarms", "s.md"),
		[]byte("---\nswarmId: swarm-1\nroles:\n  - name: worker\n    runtime:\n      image: fake-agent\n---\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "workflows", "wf.md"),
		[]byte("---\nworkflowId: wf-1\nentrypoint: run\nsteps:\n  run:\n    type: agent\n    role: worker\n    prompt: \"do work\"\n    on_success: done\nterminals:\n  done:\n    status: COMPLETED\n---\n"), 0o644))
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	rig.bot.registry = reg

	got := handleProject(context.Background(), rig.bot, 100, 0)
	assert.Equal(t, "", got,
		"empty-string sentinel must suppress the dispatch site's legacy sendMessage; the picker handler already sent the inline keyboard")
	require.Len(t, rig.callsTo("sendMessage"), 1)
	assert.Contains(t, string(rig.callsTo("sendMessage")[0].body), "project:select:my-asst",
		"picker payload must reach Telegram with the right callback_data")
}

// TestAnswerCallbackQuery_PropagatesHTTPError — when Telegram
// returns a non-2xx the helper must surface the error so the
// caller can log + decide whether to retry. Pins the error-path
// formatting so a future refactor can't silently swallow 4xx
// responses (which would leave operator buttons "loading"
// forever in production).
func TestAnswerCallbackQuery_PropagatesHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"description":"Bad Request"}`))
	}))
	t.Cleanup(server.Close)
	chatClient := chat.NewClient("https://api.example.com", "test-key", "gpt-4")
	bot, err := NewBot(BotConfig{Token: "test-token"}, chatClient, WithHTTPClient(server.Client()))
	require.NoError(t, err)
	bot.baseURL = server.URL

	err = bot.answerCallbackQuery(context.Background(), "cb-x", "any text", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 400")
}

// TestAnswerCallbackQuery_NoTokenIsNoOp — when the bot is not
// fully wired (test config without a real token), the helper
// should silently no-op rather than dial api.telegram.org and
// fail. Mirrors the other write paths' behaviour.
func TestAnswerCallbackQuery_NoTokenIsNoOp(t *testing.T) {
	chatClient := chat.NewClient("https://api.example.com", "test-key", "gpt-4")
	bot, err := NewBot(BotConfig{Token: ""}, chatClient)
	// NewBot may refuse an empty token; if so, this test is
	// moot — the no-op branch is unreachable in production.
	if err != nil {
		t.Skipf("NewBot refuses empty token (%v); no-op branch is unreachable from real config", err)
	}
	require.NoError(t, bot.answerCallbackQuery(context.Background(), "cb-x", "ignored", false))
}

// TestHandleProject_PickerSendFailureFallsBackToProse — when the
// inline-keyboard send fails (network issue, Telegram down), the
// handler must still return a useful text response listing the
// available project slugs so the operator has a manual recovery
// path. Anchors the fallback branch.
func TestHandleProject_PickerSendFailureFallsBackToProse(t *testing.T) {
	// Server that 500s every request — simulates Telegram outage
	// while the bot's getProjectList still returns projects.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	chatClient := chat.NewClient("https://api.example.com", "test-key", "gpt-4")
	bot, err := NewBot(BotConfig{Token: "test-token"}, chatClient, WithHTTPClient(server.Client()))
	require.NoError(t, err)
	bot.baseURL = server.URL

	root := t.TempDir()
	for _, sub := range []string{"projects", "swarms", "workflows"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, sub), 0o755))
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "projects", "p.yaml"),
		[]byte("projectId: alpha\nswarmId: swarm-1\ndefaultWorkflowId: wf-1\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "swarms", "s.md"),
		[]byte("---\nswarmId: swarm-1\nroles:\n  - name: worker\n    runtime:\n      image: fake-agent\n---\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "workflows", "wf.md"),
		[]byte("---\nworkflowId: wf-1\nentrypoint: run\nsteps:\n  run:\n    type: agent\n    role: worker\n    prompt: \"do work\"\n    on_success: done\nterminals:\n  done:\n    status: COMPLETED\n---\n"), 0o644))
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	bot.registry = reg

	got := handleProject(context.Background(), bot, 100, 0)
	assert.Contains(t, got, "alpha",
		"picker-failure fallback must still surface the project slug so the operator can /project <id> manually")
	assert.Contains(t, got, "/project <id>",
		"fallback should restate the manual command for clarity")
}

func TestSendProjectPicker_CapsAtTwelveWithHintText(t *testing.T) {
	rig := newCallbackRig(t)
	projects := make([]*registry.Project, 15)
	for i := range projects {
		projects[i] = &registry.Project{ID: "p" + string(rune('a'+i))}
	}
	require.NoError(t, rig.bot.sendProjectPicker(context.Background(), 100, projects))

	body := string(rig.callsTo("sendMessage")[0].body)
	assert.Contains(t, body, "Showing first 12 of 15",
		"with >12 projects the message must call out that the picker is truncated, with a hint for the rest")
}
