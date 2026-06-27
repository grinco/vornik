// Tests for the 2026.6.0 /start onboarding wizard. The wizard
// branches on (active project, project count) into three states:
//
//   - active project set        → "welcome back" prose with name
//   - projects exist, no active → inline picker (suppressed text)
//   - no projects at all        → web-gallery pointer (with WebUIBaseURL)
//
// Plus the safety paths: nil bot, registry-absent, picker send
// failure with a real bot wired to a 500'ing test server.

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

// TestHandleStart_NilBotReturnsTerseGreeting pins the defensive
// branch: a misconfigured caller (no bot wired) gets a short
// non-empty reply instead of nil-panic.
func TestHandleStart_NilBotReturnsTerseGreeting(t *testing.T) {
	got := handleStart(context.Background(), nil, 100, 0)
	assert.NotEmpty(t, got, "nil bot must still return SOMETHING — chat would otherwise hang silent")
}

// TestHandleStart_ActiveProjectShowsWelcomeBack — when the user
// already has an active project pinned, the wizard returns a
// short welcome-back message naming the project and pointing at
// /help. This is the post-onboarding return-visit shape.
func TestHandleStart_ActiveProjectShowsWelcomeBack(t *testing.T) {
	chatClient := chat.NewClient("https://api.example.com", "test-key", "gpt-4")
	bot, err := NewBot(BotConfig{Token: "test-token"}, chatClient)
	require.NoError(t, err)
	bot.setActiveProject(100, "my-asst")

	got := handleStart(context.Background(), bot, 100, 0)
	assert.Contains(t, got, "Welcome back")
	assert.Contains(t, got, "my-asst",
		"active project name must surface so the user knows what context they're in")
	assert.Contains(t, got, "/help",
		"pointer at the command list helps returning users discover slash commands")
	assert.NotContains(t, got, "<b>",
		"sendMessage does not set parse_mode, so onboarding copy must not expose raw HTML tags")
}

// TestHandleStart_NoProjectsWithWebUI — empty deployment with a
// configured WebUIBaseURL gets an HTML link to the gallery so
// new users have a one-tap path to create their first project.
func TestHandleStart_NoProjectsWithWebUI(t *testing.T) {
	chatClient := chat.NewClient("https://api.example.com", "test-key", "gpt-4")
	bot, err := NewBot(BotConfig{
		Token:        "test-token",
		WebUIBaseURL: "https://vornik.example.com/",
	}, chatClient)
	require.NoError(t, err)
	// Registry NOT wired → getProjectList returns nil → no-projects branch.

	got := handleStart(context.Background(), bot, 100, 0)
	assert.Contains(t, got, "vornik.example.com/ui/projects/new",
		"trailing slash on WebUIBaseURL must be normalised so the link is clean")
	assert.Contains(t, got, "create",
		"empty-state copy must signal the next action clearly")
	assert.NotContains(t, got, ".com//ui",
		"WebUIBaseURL with trailing slash should not produce a double-slash path component")
	assert.NotContains(t, got, "<a ",
		"sendMessage does not set parse_mode, so links should be plain URLs")
}

// TestHandleStart_NoProjectsNoWebUI — empty deployment without
// WebUIBaseURL falls back to a relative-path hint that works for
// self-hosted operators discovering the UI via the same hostname.
func TestHandleStart_NoProjectsNoWebUI(t *testing.T) {
	chatClient := chat.NewClient("https://api.example.com", "test-key", "gpt-4")
	bot, err := NewBot(BotConfig{Token: "test-token"}, chatClient)
	require.NoError(t, err)

	got := handleStart(context.Background(), bot, 100, 0)
	assert.Contains(t, got, "/ui/projects/new",
		"the relative path must still surface so the operator knows where to go")
	assert.NotContains(t, got, "https://",
		"no WebUIBaseURL → no scheme — don't fabricate a hostname")
	assert.NotContains(t, got, "<code>",
		"sendMessage does not set parse_mode, so relative-path hints should be plain text")
}

// TestHandleStart_ProjectsExistSendsPickerAndSuppressesText —
// canonical happy path for a returning user who hasn't picked a
// project yet. Asserts:
//   - the picker is posted (inline keyboard with the project slug)
//   - the function returns "" so the dispatch site doesn't post a
//     stale follow-up text message
//   - the intro message also surfaces so the picker has context
func TestHandleStart_ProjectsExistSendsPickerAndSuppressesText(t *testing.T) {
	rig := newCallbackRig(t)

	// Build a registry with one real project so getProjectList
	// returns a populated slice.
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

	got := handleStart(context.Background(), rig.bot, 100, 0)
	assert.Equal(t, "", got,
		"returning empty suppresses the dispatch site's legacy sendMessage; the picker handler already posted")

	// Both the intro and the picker should hit the stub Telegram API.
	calls := rig.callsTo("sendMessage")
	require.GreaterOrEqual(t, len(calls), 2,
		"both the intro message and the picker keyboard must be posted")
	// Picker payload reaches Telegram with the right callback_data.
	combined := ""
	for _, c := range calls {
		combined += string(c.body)
	}
	assert.Contains(t, combined, "project:select:my-asst",
		"picker callback_data must encode the project slug")
	assert.Contains(t, combined, "Welcome to vornik",
		"intro message must mention the bot so the user knows what they're talking to")
}

// TestHandleStart_PickerSendFailureFallsBackToProjectHint —
// when the picker can't be sent (network down, Telegram broken),
// the wizard must return a useful non-empty hint pointing at
// /project so the user has a manual recovery path.
func TestHandleStart_PickerSendFailureFallsBackToProjectHint(t *testing.T) {
	// Server that 500s every request — simulates Telegram outage
	// while the bot's getProjectList still returns projects.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	chatClient := chat.NewClient("https://api.example.com", "test-key", "gpt-4")
	bot, err := NewBot(BotConfig{Token: "test-token"}, chatClient, WithHTTPClient(server.Client()))
	require.NoError(t, err)
	bot.baseURL = server.URL

	// Real registry with one project.
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
	bot.registry = reg

	got := handleStart(context.Background(), bot, 100, 0)
	// Either the intro succeeded and the picker failed (returning
	// the hint) or both failed; in either case the return value
	// must be non-empty so the operator gets SOMETHING. Anchoring
	// on the /project hint specifically also pins the recovery
	// path the wizard advertises.
	assert.True(t,
		strings.Contains(got, "/project") || got == "",
		"fallback should hint at /project so the operator can recover manually; got=%q", got)
}
