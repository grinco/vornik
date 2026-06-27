package a2a

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// memPushStore is an in-memory PushConfigStore for the endpoint tests.
type memPushStore struct {
	m map[string]persistence.A2APushConfig
}

func newMemPushStore() *memPushStore { return &memPushStore{m: map[string]persistence.A2APushConfig{}} }

func (s *memPushStore) Set(_ context.Context, cfg persistence.A2APushConfig) error {
	s.m[cfg.TaskID] = cfg
	return nil
}
func (s *memPushStore) Get(_ context.Context, taskID string) (*persistence.A2APushConfig, error) {
	cfg, ok := s.m[taskID]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	return &cfg, nil
}

// fakeTaskLookup maps a task id to a project so the scope check can be
// exercised. "demo-task" belongs to the published "demo" project; anything
// else belongs to "other".
type fakeTaskLookup struct{}

func (fakeTaskLookup) Get(_ context.Context, taskID string) (*persistence.Task, error) {
	if taskID == "missing" {
		return nil, persistence.ErrNotFound
	}
	proj := "other"
	if taskID == "demo-task" {
		proj = "demo"
	}
	return &persistence.Task{ID: taskID, ProjectID: proj}, nil
}

func pushConfigHandler(t *testing.T, store PushConfigStore) *Handler {
	t.Helper()
	h, _ := newTestHandler()
	h.PushConfigStore = store
	prev := streamDeps
	WireSSE(&SSEDeps{Tasks: fakeTaskLookup{}})
	t.Cleanup(func() { streamDeps = prev })
	return h
}

func TestPushConfig_SetThenGet(t *testing.T) {
	store := newMemPushStore()
	h := pushConfigHandler(t, store)

	// SET
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/a2a/v1/agents/demo/research/tasks/demo-task/pushNotificationConfig",
		strings.NewReader(`{"url":"https://caller.example.com/hook","token":"sek"}`))
	h.HandleAgentRoute(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("set: status %d, body %s", rec.Code, rec.Body.String())
	}
	if got := store.m["demo-task"].URL; got != "https://caller.example.com/hook" {
		t.Fatalf("store url = %q", got)
	}
	if store.m["demo-task"].Token != "sek" {
		t.Fatalf("token not persisted")
	}

	// GET — must NOT echo the token.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet,
		"/a2a/v1/agents/demo/research/tasks/demo-task/pushNotificationConfig", nil)
	h.HandleAgentRoute(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: status %d", rec.Code)
	}
	var resp pushConfigResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.URL != "https://caller.example.com/hook" || !resp.Configured {
		t.Errorf("get response = %+v", resp)
	}
	if strings.Contains(rec.Body.String(), "sek") {
		t.Errorf("GET must not echo the token; body = %s", rec.Body.String())
	}
}

func TestPushConfig_BadURLRejected(t *testing.T) {
	h := pushConfigHandler(t, newMemPushStore())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/a2a/v1/agents/demo/research/tasks/demo-task/pushNotificationConfig",
		strings.NewReader(`{"url":"http://127.0.0.1/x"}`))
	h.HandleAgentRoute(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad url: status %d want 422", rec.Code)
	}
}

func TestPushConfig_OutOfScopeTaskIs404(t *testing.T) {
	store := newMemPushStore()
	h := pushConfigHandler(t, store)
	// "other-task" belongs to project "other", not "demo" → 404, no write.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/a2a/v1/agents/demo/research/tasks/other-task/pushNotificationConfig",
		strings.NewReader(`{"url":"https://caller.example.com/hook"}`))
	h.HandleAgentRoute(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("out-of-scope: status %d want 404", rec.Code)
	}
	if _, ok := store.m["other-task"]; ok {
		t.Errorf("must not persist config for an out-of-scope task")
	}
}

func TestPushConfig_GetMissingIs404(t *testing.T) {
	h := pushConfigHandler(t, newMemPushStore())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/a2a/v1/agents/demo/research/tasks/demo-task/pushNotificationConfig", nil)
	h.HandleAgentRoute(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("no config set: status %d want 404", rec.Code)
	}
}

func TestPushConfig_UnsupportedWhenNoStore(t *testing.T) {
	h, _ := newTestHandler() // no PushConfigStore
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/a2a/v1/agents/demo/research/tasks/demo-task/pushNotificationConfig",
		strings.NewReader(`{"url":"https://caller.example.com/hook"}`))
	h.HandleAgentRoute(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no store: status %d want 503", rec.Code)
	}
}
