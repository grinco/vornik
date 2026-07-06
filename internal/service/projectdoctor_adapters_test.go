package service

import (
	"context"
	"errors"
	"sync"
	"testing"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/projectdoctor"
	"vornik.io/vornik/internal/taskcreate"
)

// pingProv is a chat.Provider that also implements chat.Pinger. The
// embedded nil chat.Provider is never dereferenced — doctorModelPinger
// only ever calls Ping, which is defined on this type.
type pingProv struct {
	chat.Provider
	err error
}

func (p pingProv) Ping(_ context.Context) error { return p.err }

// TestDoctorModelPinger_UsesProviderPing is a regression for the
// final-review finding: an earlier fix built a fresh HTTP client from
// config and returned nil for provider "router", so the model check
// reported Unknown (Required) and false-blocked completeness on the
// deployed router config. The correct probe asserts the shared
// provider to chat.Pinger — every backend family (http/router/cli/
// subscription) implements it, and the LoggingProvider/QueuedProvider
// wrappers forward Ping — so one assertion covers all backends,
// including router.
func TestDoctorModelPinger_UsesProviderPing(t *testing.T) {
	// A pingable provider yields a non-nil pinger whose Ping delegates
	// to the provider's Ping.
	called := errors.New("ping reached provider")
	mp := doctorModelPinger(pingProv{err: called})
	if mp == nil {
		t.Fatal("pingable provider must yield a non-nil model pinger")
	}
	if err := mp.Ping(context.Background()); !errors.Is(err, called) {
		t.Fatalf("model pinger must delegate to the provider's Ping: got %v", err)
	}
	// A reachable provider (nil ping error) reports success.
	if err := doctorModelPinger(pingProv{}).Ping(context.Background()); err != nil {
		t.Fatalf("reachable provider: got %v", err)
	}
	// A nil provider (chat disabled) yields a nil pinger so checkModel
	// degrades to a non-blocking neutral.
	if doctorModelPinger(nil) != nil {
		t.Fatal("nil provider must yield a nil pinger")
	}
}

func TestChatPinger(t *testing.T) {
	p := newChatPinger(func(_ context.Context) error { return nil })
	if err := p.Ping(context.Background()); err != nil {
		t.Fatalf("ok ping: %v", err)
	}
	p = newChatPinger(func(_ context.Context) error { return errors.New("boom") })
	if err := p.Ping(context.Background()); err == nil {
		t.Fatal("failing ping must return error")
	}
}

func TestSmokeRunner_LatestTracksTriggered(t *testing.T) {
	sr := newSmokeRunner(nil, nil, nil) // creator nil => Trigger errors, but Latest map logic is under test
	if _, ok := sr.Latest("p"); ok {
		t.Fatal("no smoke yet => Latest false")
	}
	sr.recordLast("p", "task_9") // internal helper the Trigger path uses
	st, ok := sr.Latest("p")
	if !ok || st.TaskID != "task_9" {
		t.Fatalf("Latest = %+v ok=%v", st, ok)
	}
}

// stubTaskRepo returns one fixed (task, err) pair from Get and panics
// if any other TaskRepository method is called (embedded nil
// interface) — mirrors internal/postmortem/explainer_test.go's
// stubTaskRepo pattern.
type stubTaskRepo struct {
	persistence.TaskRepository
	task *persistence.Task
	err  error
}

func (s stubTaskRepo) Get(_ context.Context, _ string) (*persistence.Task, error) {
	return s.task, s.err
}

// TestSmokeRunner_Trigger_InFlightGuardSkipsCreate is the regression
// for the smoke-trigger race (companion review, 2026-07-04):
// Trigger used to call creator.Create unconditionally, so two
// concurrent POSTs for the same project both created tasks (double
// token-spend) — taskcreate.Creator.Create only dedups on
// IdempotencyKey, which the smoke runner never sets. Trigger must now
// re-check Latest (authoritative, not just the Doctor-level
// pre-check) before creating, and short-circuit without calling
// Create at all when a smoke task is already running. The creator
// here has no task repository wired, so if Create were invoked it
// would return a non-nil *taskcreate.Error — any error surfacing
// proves Create was reached.
func TestSmokeRunner_Trigger_InFlightGuardSkipsCreate(t *testing.T) {
	creator := taskcreate.New() // no WithTaskRepository: Create() errors if ever invoked
	tasks := stubTaskRepo{task: &persistence.Task{ID: "task_existing", Status: persistence.TaskStatusRunning}}
	sr := newSmokeRunner(creator, tasks, nil)
	sr.recordLast("p1", "task_existing")

	taskID, err := sr.Trigger(context.Background(), "p1", "prompt")
	if err != nil {
		t.Fatalf("Trigger must not call Create while a smoke task is in flight: %v", err)
	}
	if taskID != "task_existing" {
		t.Fatalf("taskID = %q, want the existing in-flight task id", taskID)
	}
}

// TestSmokeRunner_Latest_TransientLookupFailureIsUnknown is the
// regression for the second companion finding: a transient tasks.Get
// failure (DB/network hiccup) used to report StatusYellow+Running:true
// — indistinguishable from a real running task, and the UI smoke
// button disables forever on Meta.running. It must instead report
// StatusUnknown with Running:false (button re-enables, state reads
// honestly) while still preserving the recorded task id.
func TestSmokeRunner_Latest_TransientLookupFailureIsUnknown(t *testing.T) {
	tasks := stubTaskRepo{err: errors.New("db unavailable")}
	sr := newSmokeRunner(nil, tasks, nil)
	sr.recordLast("p1", "task_1")

	st, ok := sr.Latest("p1")
	if !ok {
		t.Fatal("Latest must report ok=true once a task id was recorded")
	}
	if st.Status != projectdoctor.StatusUnknown {
		t.Fatalf("Status = %v, want StatusUnknown on a transient lookup failure", st.Status)
	}
	if st.Running {
		t.Fatal("Running must be false so the smoke button re-enables after a transient lookup failure")
	}
	if st.TaskID != "task_1" {
		t.Fatalf("TaskID = %q, want the recorded task id preserved", st.TaskID)
	}
}

// countingTaskRepo records every Create and serves Get from an
// in-memory map, so a concurrency test can assert exactly how many
// tasks got created.
type countingTaskRepo struct {
	persistence.TaskRepository
	mu      sync.Mutex
	created int
	byID    map[string]*persistence.Task
}

func (c *countingTaskRepo) Create(_ context.Context, task *persistence.Task) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.created++
	if c.byID == nil {
		c.byID = map[string]*persistence.Task{}
	}
	c.byID[task.ID] = task
	return nil
}

func (c *countingTaskRepo) Get(_ context.Context, id string) (*persistence.Task, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.byID[id]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	return t, nil
}

// TestSmokeRunner_Trigger_ConcurrentCallsCreateExactlyOnce drives the
// race directly: N goroutines call Trigger for the same project at
// once with no prior smoke task recorded. Pre-fix, every goroutine
// could pass the in-flight check before any of them recorded a last
// task id, so all N called creator.Create. Post-fix, the per-project
// mutex serializes them and only the first actually creates a task —
// every other goroutine observes it via the authoritative Latest()
// re-check inside the lock.
func TestSmokeRunner_Trigger_ConcurrentCallsCreateExactlyOnce(t *testing.T) {
	repo := &countingTaskRepo{}
	creator := taskcreate.New(taskcreate.WithTaskRepository(repo))
	sr := newSmokeRunner(creator, repo, nil)

	const n = 20
	var wg sync.WaitGroup
	ids := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids[i], errs[i] = sr.Trigger(context.Background(), "p1", "prompt")
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Trigger[%d]: %v", i, err)
		}
	}
	first := ids[0]
	for i, id := range ids {
		if id != first {
			t.Fatalf("Trigger[%d] = %q, want every concurrent caller to observe the same task id %q", i, id, first)
		}
	}
	if repo.created != 1 {
		t.Fatalf("Create called %d times, want exactly 1 (smoke-trigger race regression)", repo.created)
	}
}
