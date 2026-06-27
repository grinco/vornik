package cli

// CLI tests for `vornikctl task {message,directive,answer,amend,pause,
// resume,close,messages}` (coverage-gap sweep 2026-06-18, Tier 3).
// All state-mutating verbs funnel through postChat; this exercises the
// path/body construction for each, the answer-flag validation, the
// flip wrappers, the messages list render, and postChat's success/error
// branches. httptest-stubbed daemon + captured stdout.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func resetTaskChatFlags() {
	taskChatProject = "snake"
	taskChatContent, taskChatCheckpointID, taskChatChoice = "", "", ""
	taskChatNewBrief, taskChatReason, taskChatAuthor = "", "", ""
	taskChatJSON = false
}

// captureChat records the method, path and decoded JSON body of the
// single request the handler makes, and replies with respBody.
type capturedReq struct {
	method string
	path   string
	body   map[string]any
}

func chatStub(t *testing.T, status int, respBody any) (*httptest.Server, *capturedReq) {
	t.Helper()
	got := &capturedReq{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path = r.Method, r.URL.Path
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			_ = json.Unmarshal(b, &got.body)
		}
		w.WriteHeader(status)
		if respBody != nil {
			_ = json.NewEncoder(w).Encode(respBody)
		}
	}))
	return srv, got
}

func TestRunTaskMessage_PostsMessageKind(t *testing.T) {
	srv, got := chatStub(t, http.StatusOK, map[string]any{"status": "queued"})
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-dev")
	resetTaskChatFlags()
	taskChatContent, taskChatAuthor = "hello there", "operator"

	out, err := captureStdoutFunc(t, func() error { return runTaskMessage(taskMessageCmd, []string{"task-1"}) })
	if err != nil {
		t.Fatalf("runTaskMessage: %v", err)
	}
	if got.method != http.MethodPost || got.path != "/api/v1/projects/snake/tasks/task-1/messages" {
		t.Errorf("unexpected request: %s %s", got.method, got.path)
	}
	if got.body["kind"] != "message" || got.body["content"] != "hello there" {
		t.Errorf("body not forwarded: %+v", got.body)
	}
	if !strings.Contains(out, "status: queued") {
		t.Errorf("expected echoed response field, got:\n%s", out)
	}
}

func TestRunTaskDirective_PostsDirectiveKind(t *testing.T) {
	srv, got := chatStub(t, http.StatusOK, map[string]any{"ok": true})
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-dev")
	resetTaskChatFlags()
	taskChatContent = "focus on tests"

	if _, err := captureStdoutFunc(t, func() error { return runTaskDirective(taskDirectiveCmd, []string{"task-2"}) }); err != nil {
		t.Fatalf("runTaskDirective: %v", err)
	}
	if got.body["kind"] != "directive" {
		t.Errorf("expected directive kind, got %+v", got.body)
	}
	if got.path != "/api/v1/projects/snake/tasks/task-2/messages" {
		t.Errorf("unexpected path: %s", got.path)
	}
}

func TestRunTaskAnswer_RequiresContentOrChoice(t *testing.T) {
	resetTaskChatFlags() // both empty
	err := runTaskAnswer(taskAnswerCmd, []string{"task-3"})
	if err == nil || !strings.Contains(err.Error(), "requires --content") {
		t.Fatalf("expected validation error, got %v", err)
	}
}

func TestRunTaskAnswer_PostsToCheckpointEndpoint(t *testing.T) {
	srv, got := chatStub(t, http.StatusOK, map[string]any{"resolved": true})
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-dev")
	resetTaskChatFlags()
	taskChatCheckpointID, taskChatChoice = "cp-7", "approve"

	if _, err := captureStdoutFunc(t, func() error { return runTaskAnswer(taskAnswerCmd, []string{"task-3"}) }); err != nil {
		t.Fatalf("runTaskAnswer: %v", err)
	}
	if got.path != "/api/v1/projects/snake/tasks/task-3/messages/cp-7/answer" {
		t.Errorf("unexpected path: %s", got.path)
	}
	if got.body["choice"] != "approve" {
		t.Errorf("choice not forwarded: %+v", got.body)
	}
}

func TestRunTaskAmend_PostsAmend(t *testing.T) {
	srv, got := chatStub(t, http.StatusOK, map[string]any{"amended": true})
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-dev")
	resetTaskChatFlags()
	taskChatNewBrief, taskChatReason = "new brief text", "scope changed"

	if _, err := captureStdoutFunc(t, func() error { return runTaskAmend(taskAmendCmd, []string{"task-4"}) }); err != nil {
		t.Fatalf("runTaskAmend: %v", err)
	}
	if got.path != "/api/v1/projects/snake/tasks/task-4/amend" {
		t.Errorf("unexpected path: %s", got.path)
	}
	if got.body["newBrief"] != "new brief text" {
		t.Errorf("newBrief not forwarded: %+v", got.body)
	}
}

func TestRunTaskFlip_PostsVerbWithNoBody(t *testing.T) {
	srv, got := chatStub(t, http.StatusOK, map[string]any{"status": "paused"})
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-dev")
	resetTaskChatFlags()

	out, err := captureStdoutFunc(t, func() error { return runTaskFlip("task-5", "pause") })
	if err != nil {
		t.Fatalf("runTaskFlip: %v", err)
	}
	if got.path != "/api/v1/projects/snake/tasks/task-5/pause" {
		t.Errorf("unexpected path: %s", got.path)
	}
	if !strings.Contains(out, "status: paused") {
		t.Errorf("expected echoed field, got:\n%s", out)
	}
}

func TestRunTaskClose_PostsClose(t *testing.T) {
	srv, got := chatStub(t, http.StatusOK, map[string]any{"closed": true})
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-dev")
	resetTaskChatFlags()
	taskChatReason = "done"

	if _, err := captureStdoutFunc(t, func() error { return runTaskClose(taskCloseCmd, []string{"task-6"}) }); err != nil {
		t.Fatalf("runTaskClose: %v", err)
	}
	if got.path != "/api/v1/projects/snake/tasks/task-6/close" {
		t.Errorf("unexpected path: %s", got.path)
	}
}

func TestPostChat_Non2xxIsAPIError(t *testing.T) {
	srv, _ := chatStub(t, http.StatusConflict, map[string]string{"error": "task already closed"})
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-dev")
	resetTaskChatFlags()
	taskChatContent = "x"

	_, err := captureStdoutFunc(t, func() error { return runTaskMessage(taskMessageCmd, []string{"task-7"}) })
	if err == nil {
		t.Fatal("expected an error on 409, got nil")
	}
}

func TestRunTaskMessages_TableAndEmptyAndError(t *testing.T) {
	// Table render.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/projects/snake/tasks/task-8/messages" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"messages": []map[string]any{
			{"id": "m1", "author_kind": "operator", "message_kind": "message", "content": "hi", "created_at": "2026-06-18T10:00:00Z"},
		}})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-dev")
	resetTaskChatFlags()

	out, err := captureStdoutFunc(t, func() error { return runTaskMessages(taskMessagesCmd, []string{"task-8"}) })
	if err != nil {
		t.Fatalf("runTaskMessages: %v", err)
	}
	if !strings.Contains(out, "operator") || !strings.Contains(out, "hi") {
		t.Errorf("messages table missing content:\n%s", out)
	}

	// Empty.
	srvEmpty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"messages": []any{}})
	}))
	defer srvEmpty.Close()
	t.Setenv("VORNIK_API_URL", srvEmpty.URL)
	outEmpty, err := captureStdoutFunc(t, func() error { return runTaskMessages(taskMessagesCmd, []string{"task-8"}) })
	if err != nil {
		t.Fatalf("runTaskMessages empty: %v", err)
	}
	if !strings.Contains(outEmpty, "(no messages)") {
		t.Errorf("expected empty marker, got:\n%s", outEmpty)
	}

	// Error.
	srvErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "no such task"})
	}))
	defer srvErr.Close()
	t.Setenv("VORNIK_API_URL", srvErr.URL)
	if _, err := captureStdoutFunc(t, func() error { return runTaskMessages(taskMessagesCmd, []string{"task-8"}) }); err == nil {
		t.Fatal("expected error on 404, got nil")
	}
}
