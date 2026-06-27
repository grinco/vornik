package scheduler

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// ---------- stubs ----------

// stubMsgRepo is a hand-rolled TaskMessageRepository — the
// persistence/mocks package doesn't ship one yet. Keeps the
// per-method functions optional so each test stays terse.
type stubMsgRepo struct {
	insertCalls atomic.Int32
	insertErr   error
	listResult  []*persistence.TaskMessage
	listErr     error
}

func (s *stubMsgRepo) Insert(_ context.Context, msg *persistence.TaskMessage) error {
	s.insertCalls.Add(1)
	if s.insertErr != nil {
		return s.insertErr
	}
	if msg != nil && msg.ID == "" {
		msg.ID = "msg-stub"
	}
	return nil
}

func (s *stubMsgRepo) List(_ context.Context, _ persistence.TaskMessageFilter) ([]*persistence.TaskMessage, error) {
	return s.listResult, s.listErr
}

func (s *stubMsgRepo) GetOpenCheckpoint(_ context.Context, _ string) (*persistence.TaskMessage, error) {
	return nil, nil
}

func (s *stubMsgRepo) MarkCheckpointResolved(_ context.Context, _, _ string) error { return nil }

// stubWake captures Wake() calls. atomic so the monitor's
// requeue goroutine doesn't race the test assertion.
type stubWake struct{ n atomic.Int32 }

func (s *stubWake) Wake() { s.n.Add(1) }

// taskPtr lets us hand a *time.Time into Task.ExpectedBy
// without a local variable per call site.
func timePtr(t time.Time) *time.Time { return &t }

// ---------- constructor + defaulting ----------

func TestNewExternalWaitMonitor_DefaultsInterval(t *testing.T) {
	m := NewExternalWaitMonitor(nil, nil, nil, nil, 0, zerolog.Nop())
	if m.interval != 60*time.Second {
		t.Errorf("expected default interval 60s, got %v", m.interval)
	}
}

func TestNewExternalWaitMonitor_HonoursNonZeroInterval(t *testing.T) {
	m := NewExternalWaitMonitor(nil, nil, nil, nil, 7*time.Second, zerolog.Nop())
	if m.interval != 7*time.Second {
		t.Errorf("expected interval 7s, got %v", m.interval)
	}
}

// ---------- Start / Stop lifecycle ----------

// TestStartStop_BasicLifecycle covers Start → tick → Stop. The
// ticker fires `scanOnce`, which we observe via stubbed repos.
// Cleanup confirms Stop closes the channel and waits.
func TestStartStop_BasicLifecycle(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			return nil, nil
		},
	}
	m := NewExternalWaitMonitor(repo, repo, &stubMsgRepo{}, nil, 5*time.Millisecond, zerolog.Nop())
	m.Start(context.Background())
	// Wait long enough for at least one tick.
	time.Sleep(20 * time.Millisecond)
	m.Stop()
}

func TestStart_IdempotentSecondCallIsNoop(t *testing.T) {
	m := NewExternalWaitMonitor(nil, nil, nil, nil, time.Hour, zerolog.Nop())
	m.Start(context.Background())
	// Capture the channel from the first start.
	m.stopMu.Lock()
	first := m.stopCh
	m.stopMu.Unlock()
	m.Start(context.Background()) // should be a no-op
	m.stopMu.Lock()
	second := m.stopCh
	m.stopMu.Unlock()
	if first != second {
		t.Error("second Start should not replace the running goroutine's stopCh")
	}
	m.Stop()
}

func TestStop_IdempotentSecondCallIsNoop(t *testing.T) {
	m := NewExternalWaitMonitor(nil, nil, nil, nil, time.Hour, zerolog.Nop())
	m.Start(context.Background())
	m.Stop()
	// Second Stop must not panic / hang.
	m.Stop()
}

func TestStop_BeforeStartIsNoop(t *testing.T) {
	m := NewExternalWaitMonitor(nil, nil, nil, nil, time.Hour, zerolog.Nop())
	m.Stop() // should be a no-op
}

// TestStart_CtxCancelExitsLoop covers the `<-ctx.Done()` branch
// of the for-loop select.
func TestStart_CtxCancelExitsLoop(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			return nil, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := NewExternalWaitMonitor(repo, repo, &stubMsgRepo{}, nil, time.Hour, zerolog.Nop())
	m.Start(ctx)
	// Cancel ctx; the goroutine should exit. Give it a beat.
	cancel()
	// Wait for goroutine to exit via stop. The done channel is
	// nilled by Stop, so we synchronise by calling Stop() — it
	// triggers the close path even though ctx already fired.
	m.Stop()
}

// ---------- scanOnce ----------

// TestScanOnce_ListErrorIsLogged covers the warn-and-skip branch
// when findExpired's underlying List fails.
func TestScanOnce_ListErrorIsLogged(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			return nil, errors.New("db down")
		},
	}
	m := NewExternalWaitMonitor(repo, repo, &stubMsgRepo{}, nil, time.Hour, zerolog.Nop())
	// Direct call (not via Start) so the test stays deterministic.
	m.scanOnce(context.Background())
}

func TestScanOnce_RequeuesExpiredTasks(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)
	tasks := []*persistence.Task{
		{ID: "t1", Status: persistence.TaskStatusAwaitingExternal, ExpectedBy: timePtr(past)},
		{ID: "t2", Status: persistence.TaskStatusAwaitingExternal, ExpectedBy: timePtr(future)}, // not yet
		{ID: "t3", Status: persistence.TaskStatusAwaitingExternal, ExpectedBy: nil},             // parked indefinitely
	}
	calls := 0
	var mu sync.Mutex
	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, filter persistence.TaskFilter) ([]*persistence.Task, error) {
			mu.Lock()
			calls++
			callNum := calls
			mu.Unlock()
			if callNum == 1 {
				return tasks, nil
			}
			// scanClosureGrace also lists; return empty.
			_ = filter
			return nil, nil
		},
		TransitionConditionalFunc: func(_ context.Context, id string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			if id != "t1" {
				t.Errorf("only the past-deadline task should transition, got %q", id)
			}
			return true, nil
		},
	}
	wake := &stubWake{}
	m := NewExternalWaitMonitor(repo, repo, &stubMsgRepo{}, wake, time.Hour, zerolog.Nop())
	m.scanOnce(context.Background())
	if wake.n.Load() != 1 {
		t.Errorf("expected Wake called once for the requeued task, got %d", wake.n.Load())
	}
}

// ---------- findExpired ----------

func TestFindExpired_NilRepo(t *testing.T) {
	m := &ExternalWaitMonitor{} // taskRepo nil
	out, err := m.findExpired(context.Background(), 10)
	if err != nil || out != nil {
		t.Errorf("nil repo should yield (nil, nil), got (%v, %v)", out, err)
	}
}

func TestFindExpired_ListError(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			return nil, errors.New("db down")
		},
	}
	m := NewExternalWaitMonitor(repo, repo, nil, nil, time.Hour, zerolog.Nop())
	if _, err := m.findExpired(context.Background(), 10); err == nil {
		t.Fatal("expected error")
	}
}

func TestFindExpired_FiltersByExpectedBy(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)
	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			return []*persistence.Task{
				{ID: "expired", ExpectedBy: timePtr(past)},
				{ID: "future", ExpectedBy: timePtr(future)},
				{ID: "no-deadline"}, // nil ExpectedBy
			}, nil
		},
	}
	m := NewExternalWaitMonitor(repo, repo, nil, nil, time.Hour, zerolog.Nop())
	out, err := m.findExpired(context.Background(), 10)
	if err != nil {
		t.Fatalf("findExpired: %v", err)
	}
	if len(out) != 1 || out[0].ID != "expired" {
		t.Errorf("expected only expired task, got %+v", out)
	}
}

// ---------- requeue ----------

func TestRequeue_RejectsInvalidTransition(t *testing.T) {
	// A QUEUED task can't be re-queued via external_deadline; the
	// ValidateTransition guard catches it before any DB call.
	repo := &mocks.MockTaskRepository{
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			t.Error("transition must not run for invalid pre-state")
			return false, nil
		},
	}
	m := NewExternalWaitMonitor(repo, repo, &stubMsgRepo{}, nil, time.Hour, zerolog.Nop())
	m.requeue(context.Background(), &persistence.Task{
		ID: "t1", Status: persistence.TaskStatusQueued, // wrong starting status
		ExpectedBy: timePtr(time.Now().Add(-time.Hour)),
	})
}

func TestRequeue_NilPersistRepoSkipsTransition(t *testing.T) {
	repo := &mocks.MockTaskRepository{}
	msgs := &stubMsgRepo{}
	m := NewExternalWaitMonitor(repo, nil /* persistRepo */, msgs, nil, time.Hour, zerolog.Nop())
	m.requeue(context.Background(), &persistence.Task{
		ID: "t1", Status: persistence.TaskStatusAwaitingExternal,
		ExpectedBy: timePtr(time.Now().Add(-time.Hour)),
	})
	if msgs.insertCalls.Load() != 1 {
		t.Errorf("system message should still write even when persistRepo is nil: got %d insert calls",
			msgs.insertCalls.Load())
	}
}

func TestRequeue_MessageInsertErrorIsSwallowed(t *testing.T) {
	msgs := &stubMsgRepo{insertErr: errors.New("disk full")}
	repo := &mocks.MockTaskRepository{
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			return true, nil
		},
	}
	wake := &stubWake{}
	m := NewExternalWaitMonitor(repo, repo, msgs, wake, time.Hour, zerolog.Nop())
	m.requeue(context.Background(), &persistence.Task{
		ID: "t1", Status: persistence.TaskStatusAwaitingExternal,
		ExpectedBy: timePtr(time.Now().Add(-time.Hour)),
	})
	// Transition still ran, Wake still fired.
	if wake.n.Load() != 1 {
		t.Errorf("Wake must fire even when message insert errored: %d", wake.n.Load())
	}
}

func TestRequeue_TransitionError(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			return false, errors.New("db down")
		},
	}
	wake := &stubWake{}
	m := NewExternalWaitMonitor(repo, repo, &stubMsgRepo{}, wake, time.Hour, zerolog.Nop())
	m.requeue(context.Background(), &persistence.Task{
		ID: "t1", Status: persistence.TaskStatusAwaitingExternal,
		ExpectedBy: timePtr(time.Now().Add(-time.Hour)),
	})
	if wake.n.Load() != 0 {
		t.Errorf("Wake must NOT fire on transition error: %d", wake.n.Load())
	}
}

func TestRequeue_LostRace(t *testing.T) {
	// TransitionConditional returns (false, nil) when the row's
	// status drifted between read and write. Monitor should
	// silently move on — not an error.
	repo := &mocks.MockTaskRepository{
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			return false, nil
		},
	}
	wake := &stubWake{}
	m := NewExternalWaitMonitor(repo, repo, &stubMsgRepo{}, wake, time.Hour, zerolog.Nop())
	m.requeue(context.Background(), &persistence.Task{
		ID: "t1", Status: persistence.TaskStatusAwaitingExternal,
		ExpectedBy: timePtr(time.Now().Add(-time.Hour)),
	})
	if wake.n.Load() != 0 {
		t.Errorf("Wake must NOT fire when conditional transition didn't match: %d", wake.n.Load())
	}
}

func TestRequeue_HappyPath(t *testing.T) {
	msgs := &stubMsgRepo{}
	transCalls := 0
	repo := &mocks.MockTaskRepository{
		TransitionConditionalFunc: func(_ context.Context, _ string, from []persistence.TaskStatus, to persistence.TaskStatus, opts persistence.TransitionOpts) (bool, error) {
			transCalls++
			if len(from) != 1 || from[0] != persistence.TaskStatusAwaitingExternal {
				t.Errorf("from set wrong: %v", from)
			}
			if to != persistence.TaskStatusQueued {
				t.Errorf("destination wrong: %v", to)
			}
			if !opts.ClearLease {
				t.Errorf("ClearLease should be true")
			}
			return true, nil
		},
	}
	wake := &stubWake{}
	m := NewExternalWaitMonitor(repo, repo, msgs, wake, time.Hour, zerolog.Nop())
	m.requeue(context.Background(), &persistence.Task{
		ID: "t1", Status: persistence.TaskStatusAwaitingExternal,
		ExpectedBy: timePtr(time.Now().Add(-time.Hour)),
	})
	if transCalls != 1 {
		t.Errorf("expected one TransitionConditional call, got %d", transCalls)
	}
	if msgs.insertCalls.Load() != 1 {
		t.Errorf("expected one Insert call, got %d", msgs.insertCalls.Load())
	}
	if wake.n.Load() != 1 {
		t.Errorf("expected Wake fired once, got %d", wake.n.Load())
	}
}

func TestRequeue_NilWakeIsNoop(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			return true, nil
		},
	}
	m := NewExternalWaitMonitor(repo, repo, &stubMsgRepo{}, nil /* wake */, time.Hour, zerolog.Nop())
	m.requeue(context.Background(), &persistence.Task{
		ID: "t1", Status: persistence.TaskStatusAwaitingExternal,
		ExpectedBy: timePtr(time.Now().Add(-time.Hour)),
	})
}

func TestRequeue_NilMsgRepoSkipsInsert(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			return true, nil
		},
	}
	wake := &stubWake{}
	m := NewExternalWaitMonitor(repo, repo, nil /* msgRepo */, wake, time.Hour, zerolog.Nop())
	m.requeue(context.Background(), &persistence.Task{
		ID: "t1", Status: persistence.TaskStatusAwaitingExternal,
		ExpectedBy: timePtr(time.Now().Add(-time.Hour)),
	})
	if wake.n.Load() != 1 {
		t.Errorf("Wake should still fire when msgRepo is nil: %d", wake.n.Load())
	}
}

// ---------- scanClosureGrace ----------

func TestScanClosureGrace_NilRepos(t *testing.T) {
	// All three nil-repo guard branches: taskRepo, persistRepo, msgRepo.
	cases := []struct {
		name string
		make func() *ExternalWaitMonitor
	}{
		{
			"nil taskRepo",
			func() *ExternalWaitMonitor {
				return NewExternalWaitMonitor(nil, &mocks.MockTaskRepository{}, &stubMsgRepo{}, nil, time.Hour, zerolog.Nop())
			},
		},
		{
			"nil persistRepo",
			func() *ExternalWaitMonitor {
				return NewExternalWaitMonitor(&mocks.MockTaskRepository{}, nil, &stubMsgRepo{}, nil, time.Hour, zerolog.Nop())
			},
		},
		{
			"nil msgRepo",
			func() *ExternalWaitMonitor {
				return NewExternalWaitMonitor(&mocks.MockTaskRepository{}, &mocks.MockTaskRepository{}, nil, nil, time.Hour, zerolog.Nop())
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := c.make()
			m.scanClosureGrace(context.Background()) // must not panic
		})
	}
}

func TestScanClosureGrace_ListError(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			return nil, errors.New("db down")
		},
	}
	m := NewExternalWaitMonitor(repo, repo, &stubMsgRepo{}, nil, time.Hour, zerolog.Nop())
	m.scanClosureGrace(context.Background()) // logs warn, returns
}

func TestScanClosureGrace_RecentTasksSkipped(t *testing.T) {
	// Task updated within the grace window → fast-path skip.
	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			return []*persistence.Task{
				{ID: "t1", Status: persistence.TaskStatusCompleted, UpdatedAt: time.Now()},
			}, nil
		},
	}
	msgs := &stubMsgRepo{}
	m := NewExternalWaitMonitor(repo, repo, msgs, nil, time.Hour, zerolog.Nop())
	m.scanClosureGrace(context.Background())
	if msgs.insertCalls.Load() != 0 {
		t.Errorf("recent task must NOT trigger Insert: %d", msgs.insertCalls.Load())
	}
}

func TestScanClosureGrace_MessageListError(t *testing.T) {
	old := time.Now().Add(-30 * 24 * time.Hour)
	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			return []*persistence.Task{
				{ID: "t1", Status: persistence.TaskStatusCompleted, UpdatedAt: old},
			}, nil
		},
	}
	msgs := &stubMsgRepo{listErr: errors.New("msg db down")}
	m := NewExternalWaitMonitor(repo, repo, msgs, nil, time.Hour, zerolog.Nop())
	m.scanClosureGrace(context.Background())
	// No transition should be attempted.
	if msgs.insertCalls.Load() != 0 {
		t.Error("Insert must not fire when message list errors")
	}
}

func TestScanClosureGrace_NoClosureMessages(t *testing.T) {
	old := time.Now().Add(-30 * 24 * time.Hour)
	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			return []*persistence.Task{
				{ID: "t1", Status: persistence.TaskStatusCompleted, UpdatedAt: old},
			}, nil
		},
	}
	msgs := &stubMsgRepo{listResult: nil} // empty
	m := NewExternalWaitMonitor(repo, repo, msgs, nil, time.Hour, zerolog.Nop())
	m.scanClosureGrace(context.Background())
}

func TestScanClosureGrace_ClosureRequestStillFresh(t *testing.T) {
	// Task is old enough but the closure_request itself is recent.
	old := time.Now().Add(-30 * 24 * time.Hour)
	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			return []*persistence.Task{
				{ID: "t1", Status: persistence.TaskStatusCompleted, UpdatedAt: old},
			}, nil
		},
	}
	msgs := &stubMsgRepo{listResult: []*persistence.TaskMessage{
		{ID: "m1", CreatedAt: time.Now()}, // fresh
	}}
	m := NewExternalWaitMonitor(repo, repo, msgs, nil, time.Hour, zerolog.Nop())
	m.scanClosureGrace(context.Background())
}

func TestScanClosureGrace_TransitionError(t *testing.T) {
	old := time.Now().Add(-30 * 24 * time.Hour)
	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			return []*persistence.Task{
				{ID: "t1", Status: persistence.TaskStatusCompleted, UpdatedAt: old},
			}, nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			return false, errors.New("db down")
		},
	}
	msgs := &stubMsgRepo{listResult: []*persistence.TaskMessage{
		{ID: "m1", CreatedAt: old},
	}}
	m := NewExternalWaitMonitor(repo, repo, msgs, nil, time.Hour, zerolog.Nop())
	m.scanClosureGrace(context.Background())
	// No audit message should fire when the transition errored.
	if msgs.insertCalls.Load() != 0 {
		t.Error("audit Insert must not fire when transition errored")
	}
}

func TestScanClosureGrace_TransitionLostRace(t *testing.T) {
	old := time.Now().Add(-30 * 24 * time.Hour)
	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			return []*persistence.Task{
				{ID: "t1", Status: persistence.TaskStatusCompleted, UpdatedAt: old},
			}, nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			return false, nil // lost race — operator answered
		},
	}
	msgs := &stubMsgRepo{listResult: []*persistence.TaskMessage{
		{ID: "m1", CreatedAt: old},
	}}
	m := NewExternalWaitMonitor(repo, repo, msgs, nil, time.Hour, zerolog.Nop())
	m.scanClosureGrace(context.Background())
	if msgs.insertCalls.Load() != 0 {
		t.Error("audit Insert must not fire when transition didn't match")
	}
}

func TestScanClosureGrace_Happy(t *testing.T) {
	old := time.Now().Add(-30 * 24 * time.Hour)
	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			return []*persistence.Task{
				{ID: "t1", Status: persistence.TaskStatusCompleted, UpdatedAt: old},
			}, nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, from []persistence.TaskStatus, to persistence.TaskStatus, opts persistence.TransitionOpts) (bool, error) {
			if to != persistence.TaskStatusClosed {
				t.Errorf("expected destination CLOSED, got %v", to)
			}
			if !opts.SetClosedAtNow || opts.ClosedBy == nil || *opts.ClosedBy != "system_after_grace_period" {
				t.Errorf("unexpected opts: %+v", opts)
			}
			_ = from
			return true, nil
		},
	}
	msgs := &stubMsgRepo{listResult: []*persistence.TaskMessage{
		{ID: "m1", CreatedAt: old}, // older than cutoff
	}}
	m := NewExternalWaitMonitor(repo, repo, msgs, nil, time.Hour, zerolog.Nop())
	m.scanClosureGrace(context.Background())
	if msgs.insertCalls.Load() != 1 {
		t.Errorf("expected one audit Insert, got %d", msgs.insertCalls.Load())
	}
}
