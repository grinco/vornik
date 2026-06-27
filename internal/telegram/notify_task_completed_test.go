// Package telegram: tests for NotifyTaskCompleted — fan-out of
// task-completion notifications to watchers. Focuses on the
// pre-fan-out branches that don't need a wired watcher repo.
package telegram

import (
	"context"
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// stubWatcherRepo implements persistence.TaskWatcherRepository.
type stubWatcherRepo struct {
	getFn      func(ctx context.Context, taskID string) ([]int64, error)
	watchFn    func(ctx context.Context, taskID string, chatID int64) error
	removeFn   func(ctx context.Context, taskID string) error
	watchCalls []watchCall
	removed    []string
}

type watchCall struct {
	TaskID string
	ChatID int64
}

func (s *stubWatcherRepo) Watch(ctx context.Context, taskID string, chatID int64) error {
	s.watchCalls = append(s.watchCalls, watchCall{TaskID: taskID, ChatID: chatID})
	if s.watchFn != nil {
		return s.watchFn(ctx, taskID, chatID)
	}
	return nil
}
func (s *stubWatcherRepo) GetWatchers(ctx context.Context, taskID string) ([]int64, error) {
	if s.getFn != nil {
		return s.getFn(ctx, taskID)
	}
	return nil, nil
}
func (s *stubWatcherRepo) RemoveWatchers(ctx context.Context, taskID string) error {
	s.removed = append(s.removed, taskID)
	if s.removeFn != nil {
		return s.removeFn(ctx, taskID)
	}
	return nil
}

func TestNotifyTaskCompleted_NoWatcherRepo(t *testing.T) {
	bot, calls, cleanup := makeAutopilotBot(t)
	defer cleanup()
	bot.NotifyTaskCompleted(context.Background(),
		&persistence.Task{ID: "t1", ProjectID: "p1"}, true, "all done")
	if len(*calls) != 0 {
		t.Errorf("expected no API calls without watcher repo; got %d", len(*calls))
	}
}

func TestNotifyTaskCompleted_GetWatchersError(t *testing.T) {
	bot, calls, cleanup := makeAutopilotBot(t)
	defer cleanup()
	bot.watcherRepo = &stubWatcherRepo{
		getFn: func(_ context.Context, _ string) ([]int64, error) {
			return nil, errors.New("db down")
		},
	}
	bot.NotifyTaskCompleted(context.Background(),
		&persistence.Task{ID: "t1", ProjectID: "p1"}, true, "")
	if len(*calls) != 0 {
		t.Errorf("expected no API calls when GetWatchers fails; got %d", len(*calls))
	}
}

func TestNotifyTaskCompleted_NoWatchers(t *testing.T) {
	bot, calls, cleanup := makeAutopilotBot(t)
	defer cleanup()
	bot.watcherRepo = &stubWatcherRepo{
		getFn: func(_ context.Context, _ string) ([]int64, error) { return nil, nil },
	}
	bot.NotifyTaskCompleted(context.Background(),
		&persistence.Task{ID: "t1", ProjectID: "p1"}, true, "")
	if len(*calls) != 0 {
		t.Errorf("no watchers → no send; got %d", len(*calls))
	}
}

func TestNotifyTaskCompleted_ShortMode_Success(t *testing.T) {
	bot, calls, cleanup := makeAutopilotBot(t)
	defer cleanup()
	bot.watcherRepo = &stubWatcherRepo{
		getFn: func(_ context.Context, _ string) ([]int64, error) {
			return []int64{111}, nil
		},
	}
	bot.mu.Lock()
	bot.verbosity[111] = "short"
	bot.mu.Unlock()
	bot.NotifyTaskCompleted(context.Background(),
		&persistence.Task{ID: "task_20260519010101_abcdef0123456789", ProjectID: "p1"},
		true, "")
	if len(*calls) != 1 {
		t.Fatalf("expected 1 call; got %d", len(*calls))
	}
	if !strings.Contains((*calls)[0].Body, "COMPLETED") {
		t.Errorf("expected COMPLETED in body; got %q", (*calls)[0].Body)
	}
}

func TestNotifyTaskCompleted_ShortMode_FailureIncludesReason(t *testing.T) {
	bot, calls, cleanup := makeAutopilotBot(t)
	defer cleanup()
	bot.watcherRepo = &stubWatcherRepo{
		getFn: func(_ context.Context, _ string) ([]int64, error) {
			return []int64{111}, nil
		},
	}
	bot.mu.Lock()
	bot.verbosity[111] = "short"
	bot.mu.Unlock()
	bot.NotifyTaskCompleted(context.Background(),
		&persistence.Task{ID: "task_20260519010101_abcdef0123456789", ProjectID: "p1"},
		false, "container OOM")
	if len(*calls) != 1 {
		t.Fatalf("expected 1 call; got %d", len(*calls))
	}
	body := (*calls)[0].Body
	if !strings.Contains(body, "FAILED") {
		t.Errorf("expected FAILED in body; got %q", body)
	}
	if !strings.Contains(body, "container OOM") {
		t.Errorf("expected reason in body; got %q", body)
	}
}

func TestNotifyTaskCompleted_SilentMode_NoSend(t *testing.T) {
	bot, calls, cleanup := makeAutopilotBot(t)
	defer cleanup()
	bot.watcherRepo = &stubWatcherRepo{
		getFn: func(_ context.Context, _ string) ([]int64, error) {
			return []int64{111}, nil
		},
	}
	bot.mu.Lock()
	bot.verbosity[111] = "silent"
	bot.mu.Unlock()
	bot.NotifyTaskCompleted(context.Background(),
		&persistence.Task{ID: "t1", ProjectID: "p1"}, true, "all good")
	if len(*calls) != 0 {
		t.Errorf("silent mode should skip the push; got %d calls", len(*calls))
	}
}

func TestNotifyTaskCompleted_DefaultMode_FullPath(t *testing.T) {
	bot, calls, cleanup := makeAutopilotBot(t)
	defer cleanup()
	bot.watcherRepo = &stubWatcherRepo{
		getFn: func(_ context.Context, _ string) ([]int64, error) {
			return []int64{111}, nil
		},
	}
	// No verbosity set → defaults to "full".
	bot.NotifyTaskCompleted(context.Background(),
		&persistence.Task{ID: "task_20260519010101_abcdef0123456789", ProjectID: "p1"},
		true, "the deliverable lives at output/report.md")
	if len(*calls) == 0 {
		t.Fatalf("expected at least one call; got none")
	}
	body := (*calls)[0].Body
	if !strings.Contains(body, "COMPLETED") {
		t.Errorf("expected COMPLETED in body; got %q", body)
	}
}
