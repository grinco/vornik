package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

func strPtr(s string) *string { return &s }

// TestResolveRootTask_WalksUp confirms a subtask resolves to its
// topmost ancestor. The fixture is a 3-deep chain (root → mid →
// leaf) so the walker visits both parents before stopping.
func TestResolveRootTask_WalksUp(t *testing.T) {
	root := &persistence.Task{ID: "task_root"}
	mid := &persistence.Task{ID: "task_mid", ParentTaskID: strPtr("task_root")}
	leaf := &persistence.Task{ID: "task_leaf", ParentTaskID: strPtr("task_mid")}
	tr := &mocks.MockTaskRepository{
		GetFunc: func(ctx context.Context, id string) (*persistence.Task, error) {
			switch id {
			case "task_root":
				return root, nil
			case "task_mid":
				return mid, nil
			}
			return nil, nil
		},
	}
	b := &Bot{taskRepo: tr}
	got := b.resolveRootTask(context.Background(), leaf)
	if got == nil || got.ID != "task_root" {
		t.Errorf("expected root resolution, got %+v", got)
	}
}

// TestResolveRootTask_NoParentIsRoot covers the no-op case — a task
// without a parent IS the root, return it verbatim.
func TestResolveRootTask_NoParentIsRoot(t *testing.T) {
	root := &persistence.Task{ID: "task_root"}
	b := &Bot{taskRepo: &mocks.MockTaskRepository{}}
	if got := b.resolveRootTask(context.Background(), root); got != root {
		t.Errorf("expected verbatim root, got %+v", got)
	}
}

// TestResolveRootTask_CycleSafe ensures a malformed self-cycle in
// the data doesn't trap the walker in an unbounded loop — the
// visited set + 10-depth cap kick in.
func TestResolveRootTask_CycleSafe(t *testing.T) {
	a := &persistence.Task{ID: "task_a", ParentTaskID: strPtr("task_b")}
	b := &persistence.Task{ID: "task_b", ParentTaskID: strPtr("task_a")}
	tr := &mocks.MockTaskRepository{
		GetFunc: func(ctx context.Context, id string) (*persistence.Task, error) {
			if id == "task_a" {
				return a, nil
			}
			return b, nil
		},
	}
	bot := &Bot{taskRepo: tr}
	// We don't care which node is reported as "root"; only that the
	// call returns within bounded time. If the cycle guard breaks,
	// this test hangs the suite.
	if got := bot.resolveRootTask(context.Background(), a); got == nil {
		t.Error("expected non-nil result even on cycle")
	}
}

// TestResolveRootTask_NilRepoSafe is the operator-misconfiguration
// guard — a bot built without a task repo still answers the
// question by returning the input task verbatim.
func TestResolveRootTask_NilRepoSafe(t *testing.T) {
	b := &Bot{}
	task := &persistence.Task{ID: "task_x", ParentTaskID: strPtr("task_y")}
	if got := b.resolveRootTask(context.Background(), task); got != task {
		t.Errorf("nil repo should pass through input task")
	}
}

// TestEnsureTaskThread_SubtaskConsolidatesToRoot verifies that a
// subtask's first lifecycle event reuses the ROOT task's topic
// rather than creating its own. createForumTopic must fire at most
// once across (root event, subtask event) and the second call must
// return the same thread_id as the first.
func TestEnsureTaskThread_SubtaskConsolidatesToRoot(t *testing.T) {
	var createCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "createForumTopic") {
			createCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_thread_id":555}}`))
			return
		}
		if strings.Contains(r.URL.Path, "sendMessage") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	root := &persistence.Task{ID: "task_rootABC", ProjectID: "p1"}
	child := &persistence.Task{ID: "task_childXYZ", ProjectID: "p1", ParentTaskID: strPtr("task_rootABC")}

	repo := newStubThreadRepo()
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(ctx context.Context, id string) (*persistence.Task, error) {
			if id == "task_rootABC" {
				return root, nil
			}
			return nil, nil
		},
	}
	bot := newTestBotWithForum(t, server,
		WithForumChatID(-1, 0),
		WithTelegramThreadRepository(repo),
		WithTaskRepository(taskRepo),
	)

	// First call: root task — creates a topic.
	tidRoot, rootOut, err := bot.ensureTaskThread(context.Background(), root)
	if err != nil {
		t.Fatalf("ensureTaskThread(root): %v", err)
	}
	if rootOut.ID != root.ID {
		t.Errorf("root resolution wrong: %s", rootOut.ID)
	}

	// Second call: subtask. Must NOT create a new topic; must return
	// the same thread_id and identify the root.
	tidChild, rootForChild, err := bot.ensureTaskThread(context.Background(), child)
	if err != nil {
		t.Fatalf("ensureTaskThread(child): %v", err)
	}
	if tidChild != tidRoot {
		t.Errorf("subtask thread_id should match root's: got %d want %d", tidChild, tidRoot)
	}
	if rootForChild.ID != root.ID {
		t.Errorf("subtask must report root.ID=%s, got %s", root.ID, rootForChild.ID)
	}
	if got := createCalls.Load(); got != 1 {
		t.Errorf("createForumTopic should be called exactly once for the tree; got %d", got)
	}
	// Stored row must be keyed by the ROOT id, not the child.
	if stored, err := repo.GetByTask(context.Background(), root.ID); err != nil || stored == nil {
		t.Errorf("expected stored row for root.ID; err=%v stored=%v", err, stored)
	}
}

// TestEnsureTaskThread_GrandfathersLegacyChildTopic covers the
// migration: a child task that ALREADY owns a topic (from before
// the consolidation shipped) keeps posting to its own topic. New
// hierarchies consolidate; existing ones don't get yanked.
func TestEnsureTaskThread_GrandfathersLegacyChildTopic(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no Telegram call expected on the grandfather path; saw %s", r.URL.Path)
	}))
	defer server.Close()

	// Pre-seed a legacy thread keyed by the CHILD id (mimics a
	// pre-migration topic created for the child directly).
	legacy := &persistence.TelegramTaskThread{
		TaskID: "task_childLegacy", ChatID: -1, ThreadID: 314, TopicName: "legacy",
	}
	repo := newStubThreadRepo()
	if err := repo.Insert(context.Background(), legacy); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	bot := newTestBotWithForum(t, server,
		WithForumChatID(-1, 0),
		WithTelegramThreadRepository(repo),
		// Note: no taskRepo wired — grandfather hit must short-circuit
		// before we ever try to resolve the root.
	)

	child := &persistence.Task{ID: "task_childLegacy", ParentTaskID: strPtr("task_root")}
	tid, root, err := bot.ensureTaskThread(context.Background(), child)
	if err != nil {
		t.Fatalf("ensureTaskThread: %v", err)
	}
	if tid != legacy.ThreadID {
		t.Errorf("grandfather must reuse legacy thread; got %d want %d", tid, legacy.ThreadID)
	}
	if root.ID != child.ID {
		t.Errorf("grandfather must report the child as its own owner; got %s", root.ID)
	}
}

// TestFormatTaskEvent_SubtaskPrefix verifies the [suffix] prefix
// appears on subtask events so the operator can attribute the
// message within a consolidated root topic.
func TestFormatTaskEvent_SubtaskPrefix(t *testing.T) {
	bot := &Bot{}
	task := &persistence.Task{
		ID:     "task_subXYZ12345678",
		Status: persistence.TaskStatusCompleted,
	}
	got := bot.formatTaskEvent(context.Background(), task, true, "done", true)
	if !strings.Contains(got, "↳ [XYZ12345") && !strings.Contains(got, "↳ [Z12345") && !strings.Contains(got, "↳ [12345678]") {
		// Suffix is "task ID's last 8 chars". For "task_subXYZ12345678"
		// the last 8 are "Z1234567" or similar — check loosely.
		// Strict: the prefix MUST be present.
		t.Errorf("expected subtask prefix marker, got: %s", got)
	}
	if !strings.HasPrefix(got, "↳ [") {
		t.Errorf("subtask event must START with the prefix, got: %s", got)
	}
	if !strings.Contains(got, "✅ Task completed") {
		t.Errorf("event body lost: %s", got)
	}
}

// TestFormatTaskEvent_RootNoPrefix confirms the prefix is omitted
// when isSubtask=false — root-task events look identical to the
// pre-consolidation format so legacy operators aren't surprised.
func TestFormatTaskEvent_RootNoPrefix(t *testing.T) {
	bot := &Bot{}
	task := &persistence.Task{
		ID:     "task_root",
		Status: persistence.TaskStatusCompleted,
	}
	got := bot.formatTaskEvent(context.Background(), task, true, "done", false)
	if strings.HasPrefix(got, "↳") {
		t.Errorf("root event must NOT carry subtask prefix, got: %s", got)
	}
	if !strings.HasPrefix(got, "✅ Task completed") {
		t.Errorf("expected vanilla completion header, got: %s", got)
	}
}
