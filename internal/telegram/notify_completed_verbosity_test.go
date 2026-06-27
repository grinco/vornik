// Coverage for NotifyTaskCompleted's verbosity branches (silent,
// short, full / default) and the success / failure flag matrix. The
// existing leaf-transfer tests cover the watcher-routing branch;
// this fills in the per-chat message-shape decisions.

package telegram

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
)

// newNotifyTestBot wires a bot whose outgoing messages go to the
// telegramRecorder + the memWatcherRepo / per-chat verbosity setup
// each test needs.
func newNotifyTestBot(t *testing.T, allowedUsers map[int64]UserAccess) (*Bot, *telegramRecorder) {
	t.Helper()
	rec := newTelegramRecorder(t)
	chatClient := chat.NewClient("https://example.com", "k", "m")
	b, err := NewBot(BotConfig{Token: "tok", AllowedUsers: allowedUsers}, chatClient,
		WithHTTPClient(rec.server.Client()),
	)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	b.baseURL = rec.server.URL
	return b, rec
}

func TestNotifyTaskCompleted_FullModeSuccess(t *testing.T) {
	wrepo := newMemWatcherRepo()
	_ = wrepo.Watch(context.Background(), "task-1", 100)
	b, rec := newNotifyTestBot(t, nil)
	b.watcherRepo = wrepo
	b.setVerbosity(100, "full")

	b.NotifyTaskCompleted(context.Background(),
		&persistence.Task{ID: "task-1", ProjectID: "p1",
			Status: persistence.TaskStatusCompleted},
		true, "All tests green; deliverable.md attached")

	snap := rec.snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 message in full mode, got %d", len(snap))
	}
	if !strings.Contains(snap[0].Text, "✅") {
		t.Errorf("expected ✅ emoji in success msg; got %q", snap[0].Text)
	}
	if !strings.Contains(snap[0].Text, "task-1") {
		t.Errorf("expected task id in msg; got %q", snap[0].Text)
	}
	if !strings.Contains(snap[0].Text, "Status:") {
		t.Errorf("full mode should include 'Status:' header; got %q", snap[0].Text)
	}
}

func TestNotifyTaskCompleted_FullModeFailure(t *testing.T) {
	wrepo := newMemWatcherRepo()
	_ = wrepo.Watch(context.Background(), "task-2", 100)
	b, rec := newNotifyTestBot(t, nil)
	b.watcherRepo = wrepo
	b.setVerbosity(100, "full")

	b.NotifyTaskCompleted(context.Background(),
		&persistence.Task{ID: "task-2", ProjectID: "p1",
			Status: persistence.TaskStatusFailed},
		false, "crash in step 3")

	snap := rec.snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 message, got %d", len(snap))
	}
	if !strings.Contains(snap[0].Text, "❌") {
		t.Errorf("expected ❌ in failure msg; got %q", snap[0].Text)
	}
	if !strings.Contains(snap[0].Text, "Error:") {
		t.Errorf("expected 'Error:' in failure body; got %q", snap[0].Text)
	}
}

func TestNotifyTaskCompleted_ShortMode(t *testing.T) {
	wrepo := newMemWatcherRepo()
	_ = wrepo.Watch(context.Background(), "task-3", 100)
	b, rec := newNotifyTestBot(t, nil)
	b.watcherRepo = wrepo
	b.setVerbosity(100, "short")

	b.NotifyTaskCompleted(context.Background(),
		&persistence.Task{ID: "task-3", Status: persistence.TaskStatusCompleted},
		true, "done")

	snap := rec.snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 short message, got %d", len(snap))
	}
	// Short = single-line "✅ Task <id> COMPLETED" (no big body).
	if !strings.Contains(snap[0].Text, "COMPLETED") {
		t.Errorf("short mode: %q", snap[0].Text)
	}
	// Should NOT contain "Status:" — that's the full-mode prefix.
	if strings.Contains(snap[0].Text, "\nStatus:") {
		t.Errorf("short should not include full-mode body; got %q", snap[0].Text)
	}
}

func TestNotifyTaskCompleted_ShortModeFailureWithBody(t *testing.T) {
	wrepo := newMemWatcherRepo()
	_ = wrepo.Watch(context.Background(), "task-4", 100)
	b, rec := newNotifyTestBot(t, nil)
	b.watcherRepo = wrepo
	b.setVerbosity(100, "short")

	b.NotifyTaskCompleted(context.Background(),
		&persistence.Task{ID: "task-4", Status: persistence.TaskStatusFailed},
		false, "panic in step 7")

	snap := rec.snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 short failure message, got %d", len(snap))
	}
	if !strings.Contains(snap[0].Text, "FAILED") {
		t.Errorf("short failure: %q", snap[0].Text)
	}
	if !strings.Contains(snap[0].Text, "panic in step 7") {
		t.Errorf("short failure should include reason; got %q", snap[0].Text)
	}
}

func TestNotifyTaskCompleted_SilentMode(t *testing.T) {
	wrepo := newMemWatcherRepo()
	_ = wrepo.Watch(context.Background(), "task-5", 100)
	b, rec := newNotifyTestBot(t, nil)
	b.watcherRepo = wrepo
	b.setVerbosity(100, "silent")

	b.NotifyTaskCompleted(context.Background(),
		&persistence.Task{ID: "task-5", Status: persistence.TaskStatusCompleted},
		true, "done")

	if got := len(rec.snapshot()); got != 0 {
		t.Errorf("silent mode: got %d msgs, want 0", got)
	}
}

func TestNotifyTaskCompleted_LongMessageGetsTruncated(t *testing.T) {
	wrepo := newMemWatcherRepo()
	_ = wrepo.Watch(context.Background(), "task-6", 100)
	b, rec := newNotifyTestBot(t, nil)
	b.watcherRepo = wrepo
	b.setVerbosity(100, "full")

	huge := strings.Repeat("x", 5000)
	b.NotifyTaskCompleted(context.Background(),
		&persistence.Task{ID: "task-6", Status: persistence.TaskStatusCompleted},
		true, huge)

	snap := rec.snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 msg, got %d", len(snap))
	}
	// Should be truncated to ~500 + "…"
	if !strings.Contains(snap[0].Text, "…") {
		t.Errorf("expected truncation ellipsis in long success body; got len=%d",
			len(snap[0].Text))
	}
}

func TestNotifyTaskCompleted_MultipleWatchersAllNotified(t *testing.T) {
	wrepo := newMemWatcherRepo()
	_ = wrepo.Watch(context.Background(), "task-7", 100)
	_ = wrepo.Watch(context.Background(), "task-7", 200)
	_ = wrepo.Watch(context.Background(), "task-7", 300)
	b, rec := newNotifyTestBot(t, nil)
	b.watcherRepo = wrepo

	b.NotifyTaskCompleted(context.Background(),
		&persistence.Task{ID: "task-7", Status: persistence.TaskStatusCompleted},
		true, "done")

	snap := rec.snapshot()
	if len(snap) != 3 {
		t.Errorf("expected 3 watcher msgs, got %d", len(snap))
	}
}

func TestNotifyTaskCompleted_NotifTrackerRemembersMessageID(t *testing.T) {
	wrepo := newMemWatcherRepo()
	_ = wrepo.Watch(context.Background(), "task-8", 100)
	b, _ := newNotifyTestBot(t, nil)
	b.watcherRepo = wrepo
	if b.notifTracker == nil {
		t.Fatal("notifTracker should be initialised by NewBot")
	}

	b.NotifyTaskCompleted(context.Background(),
		&persistence.Task{ID: "task-8", ProjectID: "p1", Status: persistence.TaskStatusCompleted},
		true, "ok")

	// Recorder's fake message_id is 1. The tracker should now have
	// a mapping for (100, 1) → task-8 / p1.
	taskID, projID, ok := b.notifTracker.lookup(100, 1)
	if !ok {
		t.Fatal("notifTracker did not remember the message_id")
	}
	if taskID != "task-8" || projID != "p1" {
		t.Errorf("tracked: %s/%s, want task-8/p1", taskID, projID)
	}
}
