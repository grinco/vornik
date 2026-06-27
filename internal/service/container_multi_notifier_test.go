package service

import (
	"context"
	"sync"
	"testing"

	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/persistence"
)

type fakeCompletionNotifier struct {
	mu        sync.Mutex
	callCount int
	lastTask  *persistence.Task
}

func (f *fakeCompletionNotifier) NotifyTaskCompleted(_ context.Context, task *persistence.Task, _ bool, _ string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callCount++
	f.lastTask = task
}

func (f *fakeCompletionNotifier) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.callCount
}

// TestNewMultiCompletionNotifier_AllNilReturnsNil — nothing wired
// means nothing to multiplex. Caller can branch on nil to skip
// SetCompletionNotifier entirely.
func TestNewMultiCompletionNotifier_AllNilReturnsNil(t *testing.T) {
	if got := newMultiCompletionNotifier(); got != nil {
		t.Errorf("no args: got %v, want nil", got)
	}
	if got := newMultiCompletionNotifier(nil, nil); got != nil {
		t.Errorf("all-nil: got %v, want nil", got)
	}
}

// TestNewMultiCompletionNotifier_Singleton — exactly one live
// notifier is returned directly (no multiplexer wrapper). Saves
// a goroutine and a method-call hop for the common single-channel
// deployment case.
func TestNewMultiCompletionNotifier_Singleton(t *testing.T) {
	n := &fakeCompletionNotifier{}
	got := newMultiCompletionNotifier(n)
	if got != n {
		t.Errorf("single live notifier should pass through; got %v", got)
	}
	got = newMultiCompletionNotifier(nil, n, nil)
	if got != n {
		t.Errorf("single live with nil padding should still pass through; got %v", got)
	}
}

// TestMultiCompletionNotifier_FansOut — every wired notifier gets
// the event. Each downstream filters by its own pending map, so
// fan-out is safe even if one notifier "owns" the task and the
// others don't.
func TestMultiCompletionNotifier_FansOut(t *testing.T) {
	a := &fakeCompletionNotifier{}
	b := &fakeCompletionNotifier{}
	c := &fakeCompletionNotifier{}
	multi := newMultiCompletionNotifier(a, b, c)

	task := &persistence.Task{ID: "task-x"}
	multi.NotifyTaskCompleted(context.Background(), task, true, "")
	multi.NotifyTaskCompleted(context.Background(), task, true, "")

	for name, n := range map[string]*fakeCompletionNotifier{"a": a, "b": b, "c": c} {
		if got := n.calls(); got != 2 {
			t.Errorf("notifier %s: calls = %d, want 2", name, got)
		}
		if n.lastTask != task {
			t.Errorf("notifier %s: lastTask = %v, want %v", name, n.lastTask, task)
		}
	}
}

// Compile-time assertion: ensure the multiplexer satisfies the
// executor's interface so SetCompletionNotifier accepts it.
var _ executor.CompletionNotifier = (*multiCompletionNotifier)(nil)
