package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// stubArtifactRepo is a minimal ArtifactRepository for the forum
// artifact-fanout tests. Only List is exercised — the other methods
// satisfy the interface but panic if hit, so any accidental call
// from production code shows up clearly in the test output.
type stubArtifactRepo struct {
	rows []*persistence.Artifact
}

func (s *stubArtifactRepo) Create(context.Context, *persistence.Artifact) error { panic("unused") }
func (s *stubArtifactRepo) Get(context.Context, string) (*persistence.Artifact, error) {
	panic("unused")
}
func (s *stubArtifactRepo) GetByHash(context.Context, string) (*persistence.Artifact, error) {
	panic("unused")
}
func (s *stubArtifactRepo) List(_ context.Context, _ persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
	return s.rows, nil
}
func (s *stubArtifactRepo) Delete(context.Context, string) error               { panic("unused") }
func (s *stubArtifactRepo) DeleteByExecutionID(context.Context, string) error  { panic("unused") }
func (s *stubArtifactRepo) UpdateTaskID(context.Context, string, string) error { panic("unused") }

// writeTempArtifact creates a small file in t.TempDir and returns
// its absolute path. Used by the artifact-fanout tests so
// sendDocumentToForum has a real file to stream.
func writeTempArtifact(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write tmp artifact: %v", err)
	}
	return p
}

// TestNotifyForumThread_DisabledReturnsFalse confirms the
// short-circuit when the forum surface is unconfigured — callers
// rely on the false return to keep the DM watcher fanout running
// in legacy single-channel setups.
func TestNotifyForumThread_DisabledReturnsFalse(t *testing.T) {
	bot := &Bot{} // no chatID, no threadRepo
	got := bot.notifyForumThread(context.Background(), &persistence.Task{ID: "task_x"}, true, "done")
	if got {
		t.Errorf("disabled forum must return false; got true")
	}
}

// TestNotifyForumThread_ReturnsTrueOnSuccess wires a stub Telegram
// server that ack's every call so the full happy path runs end to
// end. The bool return powers the DM-fanout-suppression branch
// in NotifyTaskCompleted, so it must be true precisely when the
// thread received the event.
func TestNotifyForumThread_ReturnsTrueOnSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "createForumTopic"):
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_thread_id":42}}`))
		case strings.Contains(r.URL.Path, "sendMessage"):
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
		case strings.Contains(r.URL.Path, "closeForumTopic"):
			_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	bot := newTestBotWithForum(t, server,
		WithForumChatID(-1, 0),
		WithTelegramThreadRepository(newStubThreadRepo()),
	)
	task := &persistence.Task{ID: "task_n1", Status: persistence.TaskStatusCompleted}
	if !bot.notifyForumThread(context.Background(), task, true, "done") {
		t.Error("expected true on success")
	}
}

// TestNotifyForumThread_SubtaskDoesNotCloseTopic ensures a
// terminating subtask under a still-running root leaves the topic
// open. Closing on the first subtask completion would lock the
// operator out of the rest of the tree.
func TestNotifyForumThread_SubtaskDoesNotCloseTopic(t *testing.T) {
	var closeCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "createForumTopic"):
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_thread_id":100}}`))
		case strings.Contains(r.URL.Path, "sendMessage"):
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
		case strings.Contains(r.URL.Path, "closeForumTopic"):
			closeCalls.Add(1)
			_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	root := &persistence.Task{ID: "task_root_open", ProjectID: "p1"}
	// Use FAILED so the close path would fire on a root with this
	// status — that proves the subtask gate is what's holding the
	// close back, not the status check upstream.
	child := &persistence.Task{
		ID: "task_child_done", ProjectID: "p1",
		ParentTaskID: strPtr("task_root_open"),
		Status:       persistence.TaskStatusFailed,
	}
	// Pre-seed the root's topic so ensureTaskThread consolidates
	// the child's event into it without going through createForumTopic.
	repo := newStubThreadRepo()
	if err := repo.Insert(context.Background(), &persistence.TelegramTaskThread{
		TaskID: root.ID, ChatID: -1, ThreadID: 100, TopicName: "p1 • _root_op",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	bot := newTestBotWithForum(t, server,
		WithForumChatID(-1, 0),
		WithTelegramThreadRepository(repo),
		WithTaskRepository(stubTaskRepoFor(map[string]*persistence.Task{root.ID: root})),
	)

	if !bot.notifyForumThread(context.Background(), child, true, "child done") {
		t.Fatal("expected delivery success")
	}
	if got := closeCalls.Load(); got != 0 {
		t.Errorf("subtask completion must NOT close the root's topic; closeForumTopic called %d times", got)
	}
}

// TestNotifyForumThread_RootClosesTopic mirrors the above with the
// root task terminating — closing IS expected when the topic owner
// reaches a terminal state.
func TestNotifyForumThread_RootClosesTopic(t *testing.T) {
	var closeCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "createForumTopic"):
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_thread_id":200}}`))
		case strings.Contains(r.URL.Path, "sendMessage"):
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
		case strings.Contains(r.URL.Path, "closeForumTopic"):
			closeCalls.Add(1)
			_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	bot := newTestBotWithForum(t, server,
		WithForumChatID(-1, 0),
		WithTelegramThreadRepository(newStubThreadRepo()),
	)
	// FAILED is in scheduler.IsTerminalTaskStatus (FAILED, CANCELLED,
	// CLOSED). COMPLETED is deliberately NOT terminal here — the
	// conversational lifecycle lets it be reopened — so this test
	// uses FAILED to actually exercise the close path.
	root := &persistence.Task{ID: "task_root_done", Status: persistence.TaskStatusFailed}
	if !bot.notifyForumThread(context.Background(), root, false, "boom") {
		t.Fatal("expected delivery success")
	}
	if got := closeCalls.Load(); got != 1 {
		t.Errorf("root terminal status must trigger one close; got %d", got)
	}
}

// TestSendArtifactsToForum_FiltersOutputClass exercises the filter:
// only OUTPUT-class artifacts ship into the thread, and *-response.md
// dumps are excluded even when classified OUTPUT.
func TestSendArtifactsToForum_FiltersOutputClass(t *testing.T) {
	var docCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "sendDocument") {
			docCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	outputFile := writeTempArtifact(t, "report.txt", "output body")
	intermFile := writeTempArtifact(t, "scratch.txt", "intermediate body")
	rawDump := writeTempArtifact(t, "agent-response.md", "raw dump body")
	repo := &stubArtifactRepo{rows: []*persistence.Artifact{
		{ID: "a1", Name: "report.txt", StoragePath: outputFile, ArtifactClass: persistence.ArtifactClassOutput},
		{ID: "a2", Name: "scratch.txt", StoragePath: intermFile, ArtifactClass: persistence.ArtifactClassIntermediate},
		{ID: "a3", Name: "agent-response.md", StoragePath: rawDump, ArtifactClass: persistence.ArtifactClassOutput},
	}}

	bot := newTestBotWithForum(t, server,
		WithForumChatID(-1, 0),
		WithTelegramThreadRepository(newStubThreadRepo()),
		WithArtifactRepository(repo),
	)
	bot.sendArtifactsToForum(context.Background(), "task_xyz", 555)
	if got := docCalls.Load(); got != 1 {
		t.Errorf("expected exactly one upload (report.txt); got %d", got)
	}
}

// TestSendDocumentToForum_GuardsForumDisabled exercises the early
// returns: a bot without forum config or with thread_id 0 must
// refuse rather than emit a malformed multipart request.
func TestSendDocumentToForum_GuardsForumDisabled(t *testing.T) {
	// No chat ID + no thread repo → forum disabled. Must error
	// before touching the file system.
	bot := &Bot{}
	if err := bot.sendDocumentToForum(context.Background(), 1, "/does/not/matter", "x"); err == nil {
		t.Error("expected error when forum disabled")
	}
	// Forum enabled but caller passed thread_id 0 → still an error,
	// for the same "don't send to the root chat by accident" reason
	// sendForumMessage guards.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("must not hit telegram with thread_id 0")
	}))
	defer server.Close()
	bot = newTestBotWithForum(t, server,
		WithForumChatID(-1, 0),
		WithTelegramThreadRepository(newStubThreadRepo()),
	)
	if err := bot.sendDocumentToForum(context.Background(), 0, "/any", "x"); err == nil {
		t.Error("expected error when thread_id is 0")
	}
}

// TestNotifyForumThread_ReturnsFalseOnEnsureThreadError forces
// ensureTaskThread to fail (createForumTopic returns an HTTP
// error) and confirms the function returns false so the caller's
// fallback (DM watcher artifact fanout) re-engages.
func TestNotifyForumThread_ReturnsFalseOnEnsureThreadError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Every endpoint 500s — createForumTopic in particular,
		// which is the call ensureTaskThread depends on.
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	bot := newTestBotWithForum(t, server,
		WithForumChatID(-1, 0),
		WithTelegramThreadRepository(newStubThreadRepo()),
	)
	got := bot.notifyForumThread(context.Background(), &persistence.Task{ID: "task_err"}, true, "done")
	if got {
		t.Error("expected false when ensureTaskThread fails")
	}
}

// TestSendDocumentToForum_IncludesThreadID verifies the multipart
// payload carries message_thread_id so Telegram routes the file to
// the consolidated thread rather than dumping it at the chat root.
func TestSendDocumentToForum_IncludesThreadID(t *testing.T) {
	var sawThreadID atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "sendDocument") {
			http.NotFound(w, r)
			return
		}
		// Peek at the multipart body. We don't need full parsing —
		// just confirm the field name is present (the value would
		// follow on a subsequent line).
		if err := r.ParseMultipartForm(32 << 20); err == nil {
			if v := r.FormValue("message_thread_id"); v == "999" {
				sawThreadID.Store(true)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer server.Close()

	path := writeTempArtifact(t, "x.txt", "hi")
	bot := newTestBotWithForum(t, server,
		WithForumChatID(-1, 0),
		WithTelegramThreadRepository(newStubThreadRepo()),
	)
	if err := bot.sendDocumentToForum(context.Background(), 999, path, "caption"); err != nil {
		t.Fatalf("sendDocumentToForum: %v", err)
	}
	if !sawThreadID.Load() {
		t.Error("expected message_thread_id=999 in multipart form")
	}
}

// stubTaskRepoFor returns a MockTaskRepository whose Get serves
// from a fixed lookup map. Used by tests that need the bot to walk
// parent chains via Get without rolling a fresh fake per case.
func stubTaskRepoFor(byID map[string]*persistence.Task) persistence.TaskRepository {
	return &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			if t, ok := byID[id]; ok {
				return t, nil
			}
			return nil, persistence.ErrNotFound
		},
	}
}
