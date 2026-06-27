// Package telegram: tests for handleAutopilot — the /autopilot
// command branch. Uses a Telegram-API-rewriting transport so
// sendMessage doesn't dial out; we capture and assert the URL +
// body of the simulated API call.
package telegram

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"vornik.io/vornik/internal/chat"
)

// fakeAutonomyMgr implements the AutonomyController interface. Tests
// flip its state to script enable/disable behaviour.
type fakeAutonomyMgr struct {
	mu         sync.Mutex
	enabled    map[string]bool
	enableErr  error
	disableErr error
}

func (m *fakeAutonomyMgr) EnableProject(projectID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.enableErr != nil {
		return m.enableErr
	}
	if m.enabled == nil {
		m.enabled = map[string]bool{}
	}
	m.enabled[projectID] = true
	return nil
}

func (m *fakeAutonomyMgr) DisableProject(projectID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.disableErr != nil {
		return m.disableErr
	}
	if m.enabled != nil {
		delete(m.enabled, projectID)
	}
	return nil
}

func (m *fakeAutonomyMgr) IsAutonomyEnabled(projectID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.enabled[projectID]
}

// capturingHandler captures every Telegram API call as a list of
// (path, body) pairs for assertion.
type apiCall struct {
	Path string
	Body string
}

func newCapturingTelegramServer() (*httptest.Server, *[]apiCall) {
	calls := []apiCall{}
	mu := sync.Mutex{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := readAllBody(r)
		mu.Lock()
		calls = append(calls, apiCall{Path: r.URL.Path, Body: body})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	return srv, &calls
}

func readAllBody(r *http.Request) (string, error) {
	if r.Body == nil {
		return "", nil
	}
	buf := make([]byte, 4096)
	n, err := r.Body.Read(buf)
	_ = err
	return string(buf[:n]), nil
}

func makeAutopilotBot(t *testing.T) (*Bot, *[]apiCall, func()) {
	t.Helper()
	srv, calls := newCapturingTelegramServer()
	hc := &http.Client{
		Transport: &rewritingTransport{base: http.DefaultTransport, targetHost: srv.URL},
	}
	chatClient := chat.NewClient("https://api.example.com", "k", "gpt-4")
	bot, err := NewBot(BotConfig{Token: "tok"}, chatClient, WithHTTPClient(hc))
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	return bot, calls, srv.Close
}

func TestHandleAutopilot_NoActiveProject(t *testing.T) {
	bot, calls, cleanup := makeAutopilotBot(t)
	defer cleanup()
	_ = bot.handleAutopilot(context.Background(), 1, []string{"/autopilot"})
	if len(*calls) != 1 {
		t.Fatalf("expected 1 call; got %d", len(*calls))
	}
	if !strings.Contains((*calls)[0].Body, "No active project") {
		t.Errorf("expected no-project message; got %q", (*calls)[0].Body)
	}
}

func TestHandleAutopilot_NoManager(t *testing.T) {
	bot, calls, cleanup := makeAutopilotBot(t)
	defer cleanup()
	bot.mu.Lock()
	bot.activeProjects[1] = "p1"
	bot.mu.Unlock()
	_ = bot.handleAutopilot(context.Background(), 1, []string{"/autopilot"})
	if !strings.Contains((*calls)[0].Body, "not available") {
		t.Errorf("expected not-available message; got %q", (*calls)[0].Body)
	}
}

func TestHandleAutopilot_ReportsState(t *testing.T) {
	bot, calls, cleanup := makeAutopilotBot(t)
	defer cleanup()
	bot.mu.Lock()
	bot.activeProjects[1] = "p1"
	bot.mu.Unlock()
	mgr := &fakeAutonomyMgr{enabled: map[string]bool{"p1": true}}
	bot.autonomyMgr = mgr
	_ = bot.handleAutopilot(context.Background(), 1, []string{"/autopilot"})
	body := (*calls)[0].Body
	if !strings.Contains(body, "is on") {
		t.Errorf("expected 'is on' state report; got %q", body)
	}
}

func TestHandleAutopilot_EnableOn(t *testing.T) {
	bot, calls, cleanup := makeAutopilotBot(t)
	defer cleanup()
	bot.mu.Lock()
	bot.activeProjects[1] = "p1"
	bot.mu.Unlock()
	mgr := &fakeAutonomyMgr{}
	bot.autonomyMgr = mgr
	_ = bot.handleAutopilot(context.Background(), 1, []string{"/autopilot", "on"})
	if !mgr.IsAutonomyEnabled("p1") {
		t.Errorf("expected autopilot enabled for p1")
	}
	if !strings.Contains((*calls)[0].Body, "Autopilot enabled") {
		t.Errorf("expected enable message; got %q", (*calls)[0].Body)
	}
}

func TestHandleAutopilot_EnableError(t *testing.T) {
	bot, calls, cleanup := makeAutopilotBot(t)
	defer cleanup()
	bot.mu.Lock()
	bot.activeProjects[1] = "p1"
	bot.mu.Unlock()
	bot.autonomyMgr = &fakeAutonomyMgr{enableErr: errors.New("not configured")}
	_ = bot.handleAutopilot(context.Background(), 1, []string{"/autopilot", "on"})
	if !strings.Contains((*calls)[0].Body, "Failed to enable") {
		t.Errorf("expected enable error message; got %q", (*calls)[0].Body)
	}
}

func TestHandleAutopilot_Disable(t *testing.T) {
	bot, calls, cleanup := makeAutopilotBot(t)
	defer cleanup()
	bot.mu.Lock()
	bot.activeProjects[1] = "p1"
	bot.mu.Unlock()
	mgr := &fakeAutonomyMgr{enabled: map[string]bool{"p1": true}}
	bot.autonomyMgr = mgr
	_ = bot.handleAutopilot(context.Background(), 1, []string{"/autopilot", "off"})
	if mgr.IsAutonomyEnabled("p1") {
		t.Errorf("expected autopilot disabled for p1")
	}
	if !strings.Contains((*calls)[0].Body, "Autopilot disabled") {
		t.Errorf("expected disable message; got %q", (*calls)[0].Body)
	}
}

func TestHandleAutopilot_DisableError(t *testing.T) {
	bot, calls, cleanup := makeAutopilotBot(t)
	defer cleanup()
	bot.mu.Lock()
	bot.activeProjects[1] = "p1"
	bot.mu.Unlock()
	bot.autonomyMgr = &fakeAutonomyMgr{disableErr: errors.New("oops")}
	_ = bot.handleAutopilot(context.Background(), 1, []string{"/autopilot", "off"})
	if !strings.Contains((*calls)[0].Body, "Failed to disable") {
		t.Errorf("expected disable error; got %q", (*calls)[0].Body)
	}
}

func TestHandleAutopilot_UnknownArg(t *testing.T) {
	bot, calls, cleanup := makeAutopilotBot(t)
	defer cleanup()
	bot.mu.Lock()
	bot.activeProjects[1] = "p1"
	bot.mu.Unlock()
	bot.autonomyMgr = &fakeAutonomyMgr{}
	_ = bot.handleAutopilot(context.Background(), 1, []string{"/autopilot", "banana"})
	if !strings.Contains((*calls)[0].Body, "Usage:") {
		t.Errorf("expected usage hint; got %q", (*calls)[0].Body)
	}
}
