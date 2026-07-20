// Task 9: Phase A end-to-end smoke test. Drives the full task-kind path
// through the REAL fakes from Tasks 5/6 (stubRepo/fakeCreator/stubChannel/
// stubResolver defined in runner_test.go and runner_task_test.go) rather
// than the plan snippet's newFakeRepo/capturingChannel, which don't exist
// in this package. Proves runner.tickOnce's spawn path and
// CompletionNotifier.NotifyTaskCompleted's delivery/finalize path connect
// end to end: fire -> spawn task -> complete -> deliver -> re-arm.
//
// This is intentionally close to TestTaskKindFireCreatesTaskAndReArms
// (runner_task_test.go) and TestCompletionNotifierDeliversAndFinalizes
// (completion_notifier_test.go) individually, but neither of those tests
// chains the two stages together in one repo instance the way the real
// daemon does (Runner and CompletionNotifier share one
// persistence.ReminderRepository at runtime). This test is that single
// integration proof, not new unit coverage.
package reminders

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/persistence"
)

func TestTaskKindEndToEnd(t *testing.T) {
	repo := newStubRepo()
	creator := &fakeCreator{taskID: "task_1"}
	ch := &stubChannel{}
	resolver := &stubResolver{channels: map[string]conversation.Channel{"telegram": ch}}
	clock := func() time.Time { return time.Date(2026, 7, 20, 7, 0, 0, 0, time.UTC) }

	r := New(Config{
		Repo: repo, Creator: creator, DefaultTaskType: "research",
		Resolver: resolver, Logger: zerolog.Nop(), Clock: clock,
	})
	notifier := NewCompletionNotifier(repo, resolver, nil, zerolog.Nop(), clock)

	// 1) fire: a due recurring task-kind reminder spawns a task and
	// re-arms atomically (design §4.1).
	repo.queue = []*persistence.Reminder{{
		ID: "rem_1", Kind: persistence.ReminderKindTask, ProjectID: "news",
		Content: "Daily digest", CronExpr: "0 7 * * *",
		Status:  persistence.ReminderStatusFiring,
		Channel: "telegram", ChannelRef: "42",
	}}
	r.tickOnce(context.Background())

	if repo.spawnedTaskID != "task_1" || repo.spawnedID != "rem_1" {
		t.Fatalf("fire did not spawn task correctly: id=%s task=%s", repo.spawnedID, repo.spawnedTaskID)
	}
	if repo.spawnedNext == nil {
		t.Fatal("recurring reminder must re-arm with a non-nil nextFireAt")
	}

	// 2) completion -> delivery: the spawned task completes; the
	// notifier claims the delivery, sends exactly once, and finalizes
	// non-terminal (recurring re-arm, not fired/terminal).
	repo.claim = &persistence.Reminder{
		ID: "rem_1", Kind: persistence.ReminderKindTask, Channel: "telegram",
		ChannelRef: "42", Content: "Daily digest", CronExpr: "0 7 * * *",
	}
	notifier.NotifyTaskCompleted(context.Background(), &persistence.Task{ID: "task_1"}, true, "the digest")

	if len(ch.sent) != 1 {
		t.Fatalf("channel.Send count = %d, want exactly 1", len(ch.sent))
	}
	if !repo.finalized {
		t.Fatal("FinalizeDelivery must have been called")
	}
	if repo.finalizedTerminal {
		t.Fatal("recurring reminder must finalize with terminal=false (re-arm), not terminal=true")
	}
}
