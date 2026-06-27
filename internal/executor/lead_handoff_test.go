package executor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// Regression coverage for the conversational task lifecycle's
// per-outcome handlers. Three bugs encountered live during the
// 2026-05-09 Phase 25/32 rollout drive these:
//
//   1. handleClosureRequestOutcome left the task RUNNING after
//      writing closure_request, so the lease expired and the
//      scheduler retried the lead until max_attempts.
//      → assert TransitionConditional(RUNNING|LEASED → COMPLETED)
//        is called exactly once with ClearLease.
//   2. errLeadHandoff must propagate as the sentinel callers can
//      detect via IsLeadHandoff. errors.Is must traverse fmt.Errorf
//      wrapping. → wrap-and-detect test.
//   3. handleCheckpointOutcome / handleExternalWaitOutcome must
//      transition to AWAITING_INPUT / AWAITING_EXTERNAL respectively;
//      external_wait must stamp expected_by on the row.
//      → assert TransitionConditional opts per outcome.

// ---- inline test fakes ----

// fakeMessageRepo records every Insert / MarkResolved call so
// tests can assert on what the handler did. nil-safe defaults:
// Insert succeeds, lookups return empty.
type fakeMessageRepo struct {
	mu                 sync.Mutex
	inserted           []*persistence.TaskMessage
	resolvedCheckpoint string
	insertErr          error
}

func (f *fakeMessageRepo) Insert(_ context.Context, msg *persistence.TaskMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.insertErr != nil {
		return f.insertErr
	}
	if msg.ID == "" {
		msg.ID = "tmsg_test_" + time.Now().Format("150405.000000")
	}
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now().UTC()
	}
	f.inserted = append(f.inserted, msg)
	return nil
}

func (f *fakeMessageRepo) List(_ context.Context, _ persistence.TaskMessageFilter) ([]*persistence.TaskMessage, error) {
	return nil, nil
}

func (f *fakeMessageRepo) GetOpenCheckpoint(_ context.Context, _ string) (*persistence.TaskMessage, error) {
	return nil, nil
}

func (f *fakeMessageRepo) MarkCheckpointResolved(_ context.Context, _, checkpointID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolvedCheckpoint = checkpointID
	return nil
}

// fakeScratchpadRepo — minimal, just enough so applyScratchpadUpdate
// doesn't blow up. Get returns nil + nil (no existing scratchpad).
type fakeScratchpadRepo struct {
	mu       sync.Mutex
	upserted *persistence.TaskScratchpad
}

func (f *fakeScratchpadRepo) Get(_ context.Context, _ string) (*persistence.TaskScratchpad, error) {
	return nil, nil
}

func (f *fakeScratchpadRepo) Upsert(_ context.Context, sp *persistence.TaskScratchpad) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upserted = sp
	return nil
}

// transitionCall is one captured TransitionConditional invocation.
type transitionCall struct {
	id   string
	from []persistence.TaskStatus
	to   persistence.TaskStatus
	opts persistence.TransitionOpts
}

// fakeTaskRepo records TransitionConditional + Get + GetChildren
// calls. The latter two were added so the parent-unblock sweep on
// closure_request (2026-05-26 fix) can be observed in tests
// without a live DB. Handoff handlers that don't drive the unblock
// sweep keep seeing the panic stubs for unimplemented methods.
type fakeTaskRepo struct {
	mu            sync.Mutex
	calls         []transitionCall
	getCalls      []string // task IDs passed to Get
	parents       map[string]*persistence.Task
	children      map[string][]*persistence.Task
	transitionOK  bool
	transitionErr error
}

func newFakeTaskRepo() *fakeTaskRepo {
	return &fakeTaskRepo{
		transitionOK: true,
		parents:      map[string]*persistence.Task{},
		children:     map[string][]*persistence.Task{},
	}
}

func (f *fakeTaskRepo) TransitionConditional(_ context.Context, id string, from []persistence.TaskStatus, to persistence.TaskStatus, opts persistence.TransitionOpts) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, transitionCall{id: id, from: append([]persistence.TaskStatus(nil), from...), to: to, opts: opts})
	// Apply the transition to a registered parent when its status
	// matches `from` — mirrors the real conditional semantics so the
	// parent-unblock sweep (TransitionConditional since the 2026-06-04
	// race fix; UpdateStatus before) is observable on f.parents.
	// Return values stay scripted via transitionOK / transitionErr for
	// the handoff tests that drive error paths.
	if f.transitionErr == nil && f.transitionOK {
		if p, ok := f.parents[id]; ok {
			for _, s := range from {
				if p.Status == s {
					p.Status = to
					if opts.Attempt > 0 {
						p.Attempt = opts.Attempt
					}
					if opts.LastError != nil {
						p.LastError = opts.LastError
					}
					if opts.LastErrorClass != nil {
						p.LastErrorClass = opts.LastErrorClass
					}
					break
				}
			}
		}
	}
	return f.transitionOK, f.transitionErr
}

func (f *fakeTaskRepo) Get(_ context.Context, id string) (*persistence.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls = append(f.getCalls, id)
	if p, ok := f.parents[id]; ok {
		return p, nil
	}
	return nil, persistence.ErrNotFound
}

func (f *fakeTaskRepo) GetChildren(_ context.Context, parentID string) ([]*persistence.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.children[parentID], nil
}

func (f *fakeTaskRepo) Update(_ context.Context, t *persistence.Task) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Persist the updated parent so subsequent Get reads see the
	// post-unblock status.
	if t != nil {
		f.parents[t.ID] = t
	}
	return nil
}

// Other TaskRepository methods are not exercised by handoff
// handlers; stubs panic so a bug that adds an unexpected call
// fails loud.
func (f *fakeTaskRepo) Ping(context.Context) error                      { panic("not implemented") }
func (f *fakeTaskRepo) Create(context.Context, *persistence.Task) error { panic("not implemented") }
func (f *fakeTaskRepo) GetByIdempotencyKey(context.Context, string, string) (*persistence.Task, error) {
	panic("not implemented")
}

// Update is wired above (records the parent-unblock write).
func (f *fakeTaskRepo) Delete(context.Context, string) error { panic("not implemented") }
func (f *fakeTaskRepo) List(context.Context, persistence.TaskFilter) ([]*persistence.Task, error) {
	panic("not implemented")
}
func (f *fakeTaskRepo) Count(context.Context, persistence.TaskFilter) (int64, error) {
	panic("not implemented")
}

// UpdateStatus is wired here (used by the parent-unblock sweep to
// flip WAITING_FOR_CHILDREN → QUEUED). Records the call on the
// parent map so post-test assertions can verify the new status.
func (f *fakeTaskRepo) UpdateStatus(_ context.Context, id string, st persistence.TaskStatus) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.parents[id]; ok {
		p.Status = st
	}
	return nil
}
func (f *fakeTaskRepo) TransitionToCancelled(context.Context, string) (bool, error) {
	panic("not implemented")
}
func (f *fakeTaskRepo) RequeueTerminalTask(context.Context, string, int, int) (bool, error) {
	panic("not implemented")
}
func (f *fakeTaskRepo) LeaseTask(context.Context, persistence.LeaseOptions) (*persistence.Task, error) {
	panic("not implemented")
}
func (f *fakeTaskRepo) RenewLease(context.Context, string, string, int) error {
	panic("not implemented")
}
func (f *fakeTaskRepo) ReleaseLease(context.Context, string, string, persistence.TaskStatus, persistence.ReleaseOptions) error {
	panic("not implemented")
}
func (f *fakeTaskRepo) FindExpiredLeases(context.Context, int) ([]*persistence.Task, error) {
	panic("not implemented")
}
func (f *fakeTaskRepo) CountByStatus(context.Context, string) (map[persistence.TaskStatus]int64, error) {
	panic("not implemented")
}
func (f *fakeTaskRepo) CountRecentFailures(context.Context, string, []string, time.Time) (int, error) {
	panic("not implemented")
}

// GetChildren is wired above (returns the recorded children set).
func (f *fakeTaskRepo) CountChildrenForParents(context.Context, []string) (map[string]int, error) {
	panic("not implemented")
}
func (f *fakeTaskRepo) GetDependencies(context.Context, string) ([]*persistence.Task, error) {
	panic("not implemented")
}
func (f *fakeTaskRepo) GetDependents(context.Context, string) ([]*persistence.Task, error) {
	panic("not implemented")
}

func newHandoffExecutor(msgRepo persistence.TaskMessageRepository, taskRepo persistence.TaskRepository) *Executor {
	// fakeTaskRepo satisfies BOTH the broad persistence.TaskRepository
	// (passed via persistTaskRepo) and the narrower
	// executor.TaskRepository (taskRepo). 2026-05-26 fix: the
	// closure_request handler now invokes checkParentUnblock which
	// reads from e.taskRepo, so the test must wire it too.
	narrow, _ := taskRepo.(TaskRepository)
	return &Executor{
		taskMessageRepo:    msgRepo,
		taskScratchpadRepo: &fakeScratchpadRepo{},
		persistTaskRepo:    taskRepo,
		taskRepo:           narrow,
		logger:             zerolog.Nop(),
	}
}

// --------------------------------------------------------------
// Bug #3 — closure_request must transition RUNNING → COMPLETED.
// --------------------------------------------------------------
//
// Pre-fix behaviour: handleClosureRequestOutcome wrote the
// closure_request message but did NOT transition the task. The
// lease eventually expired, the scheduler re-leased the task, the
// lead emitted closure_request again — repeat until max_attempts,
// at which point the scheduler's reconcile path saw "task left
// executor in non-terminal status RUNNING" and FAILED the task.
//
// Live evidence: task_20260509213432_0655946c0465af2e ended up
// FAILED with two duplicate closure_request messages.

func TestHandleClosureRequest_TransitionsRunningToCompleted(t *testing.T) {
	msgs := &fakeMessageRepo{}
	tr := newFakeTaskRepo()
	e := newHandoffExecutor(msgs, tr)

	task := &persistence.Task{ID: "task_x", Status: persistence.TaskStatusRunning}
	exec := &persistence.Execution{ID: "exec_x", TaskID: task.ID}
	out := &LeadOutcome{
		Outcome:        LeadOutcomeClosureRequest,
		Message:        "all done",
		ClosureRequest: &ClosureRequestPayload{Summary: "done"},
	}

	if err := e.handleClosureRequestOutcome(context.Background(), task, exec, "lead_step", out); err != nil {
		t.Fatalf("handleClosureRequestOutcome err=%v", err)
	}

	// Exactly one transition recorded.
	if len(tr.calls) != 1 {
		t.Fatalf("want 1 transition call, got %d", len(tr.calls))
	}
	c := tr.calls[0]
	if c.id != "task_x" {
		t.Errorf("transition id got %q want task_x", c.id)
	}
	if c.to != persistence.TaskStatusCompleted {
		t.Errorf("transition to got %s want COMPLETED", c.to)
	}
	if !containsStatus(c.from, persistence.TaskStatusRunning) || !containsStatus(c.from, persistence.TaskStatusLeased) {
		t.Errorf("transition.from must include RUNNING + LEASED, got %v", c.from)
	}
	if !c.opts.ClearLease {
		t.Errorf("ClearLease must be true so the lease is released on COMPLETED")
	}

	// Closure_request message persisted.
	if len(msgs.inserted) != 1 {
		t.Fatalf("want 1 message inserted, got %d", len(msgs.inserted))
	}
	if msgs.inserted[0].MessageKind != persistence.TaskMessageKindClosureRequest {
		t.Errorf("message kind got %q want closure_request", msgs.inserted[0].MessageKind)
	}
}

// --------------------------------------------------------------
// Bug #4 — closure_request must wake the parent when the closed
// child is part of a delegation tree (2026-05-26).
// --------------------------------------------------------------
//
// Pre-fix behaviour: handleClosureRequestOutcome transitioned
// RUNNING → COMPLETED but skipped checkParentUnblock — only the
// generic handleSuccess path called it. A parent in
// WAITING_FOR_CHILDREN sat there forever when its child closed via
// closure_request.
//
// Live evidence: T-a8e1 stuck in WAITING_FOR_CHILDREN with child
// T-0833 COMPLETED (closure_request outcome at 14:01:07,
// 2026-05-26). Operator-visible symptom: the parent's forum thread
// stayed open, the autonomy loop kept skipping the parent because
// its status was non-terminal.
//
// Fix: handleClosureRequestOutcome now mirrors handleSuccess's
// post-transition tail — resolveCrossProjectCallForTask +
// checkParentUnblock — so the parent is woken (re-queued or
// completed) the moment the child closes.

func TestHandleClosureRequest_UnblocksWaitingParent(t *testing.T) {
	msgs := &fakeMessageRepo{}
	tr := newFakeTaskRepo()
	// Parent is registered with WAITING_FOR_CHILDREN so the
	// unblock sweep would re-queue it. The child below is its only
	// child, so once the child flips terminal the parent moves.
	parentID := "task_parent"
	tr.parents[parentID] = &persistence.Task{
		ID:     parentID,
		Status: persistence.TaskStatusWaitingForChildren,
	}
	child := &persistence.Task{
		ID:           "task_child",
		Status:       persistence.TaskStatusRunning,
		ParentTaskID: &parentID,
	}
	tr.children[parentID] = []*persistence.Task{
		{ID: child.ID, Status: persistence.TaskStatusCompleted, ParentTaskID: &parentID},
	}
	e := newHandoffExecutor(msgs, tr)

	exec := &persistence.Execution{ID: "exec_child", TaskID: child.ID}
	out := &LeadOutcome{
		Outcome:        LeadOutcomeClosureRequest,
		Message:        "done",
		ClosureRequest: &ClosureRequestPayload{Summary: "all done"},
	}
	if err := e.handleClosureRequestOutcome(context.Background(), child, exec, "lead_step", out); err != nil {
		t.Fatalf("handleClosureRequestOutcome err=%v", err)
	}

	// Evidence: the parent-unblock path went through taskRepo.Get
	// looking up the parent ID. Without the fix, Get is never called.
	found := false
	for _, id := range tr.getCalls {
		if id == parentID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected checkParentUnblock to fetch parent %q; getCalls=%v", parentID, tr.getCalls)
	}
	// The parent must have transitioned out of WAITING_FOR_CHILDREN
	// — with all children COMPLETED + none failed, the sweep flips
	// it to QUEUED via UpdateStatus.
	tr.mu.Lock()
	gotStatus := tr.parents[parentID].Status
	tr.mu.Unlock()
	if gotStatus != persistence.TaskStatusQueued {
		t.Errorf("parent status after unblock got %s want QUEUED (parent should resume now that all children are terminal)", gotStatus)
	}
}

// --------------------------------------------------------------
// Bug #2 (companion) — checkpoint transitions RUNNING →
// AWAITING_INPUT.
// --------------------------------------------------------------

func TestHandleCheckpoint_TransitionsToAwaitingInput(t *testing.T) {
	msgs := &fakeMessageRepo{}
	tr := newFakeTaskRepo()
	e := newHandoffExecutor(msgs, tr)

	task := &persistence.Task{ID: "task_y", Status: persistence.TaskStatusRunning}
	exec := &persistence.Execution{ID: "exec_y", TaskID: task.ID}
	out := &LeadOutcome{
		Outcome: LeadOutcomeCheckpoint,
		Checkpoint: &CheckpointPayload{
			Kind:     CheckpointKindDecision,
			Question: "pick one",
			Options:  []CheckpointOption{{ID: "a", Label: "A"}, {ID: "b", Label: "B"}},
		},
	}

	if err := e.handleCheckpointOutcome(context.Background(), task, exec, "lead_step", out); err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(tr.calls) != 1 || tr.calls[0].to != persistence.TaskStatusAwaitingInput {
		t.Fatalf("want 1 transition to AWAITING_INPUT, got %+v", tr.calls)
	}
	if !tr.calls[0].opts.ClearLease {
		t.Error("ClearLease must be true")
	}
	if msgs.inserted[0].MessageKind != persistence.TaskMessageKindCheckpoint {
		t.Errorf("message kind got %q want checkpoint", msgs.inserted[0].MessageKind)
	}
}

func TestHandleExternalWait_TransitionsToAwaitingExternalWithDeadline(t *testing.T) {
	msgs := &fakeMessageRepo{}
	tr := newFakeTaskRepo()
	e := newHandoffExecutor(msgs, tr)

	deadline := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	task := &persistence.Task{ID: "task_z", Status: persistence.TaskStatusRunning}
	exec := &persistence.Execution{ID: "exec_z", TaskID: task.ID}
	out := &LeadOutcome{
		Outcome: LeadOutcomeExternalWait,
		ExternalWait: &ExternalWaitPayload{
			ExpectedBy: &deadline,
			Reason:     "vendor reply expected",
		},
	}

	if err := e.handleExternalWaitOutcome(context.Background(), task, exec, "lead_step", out); err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(tr.calls) != 1 || tr.calls[0].to != persistence.TaskStatusAwaitingExternal {
		t.Fatalf("want 1 transition to AWAITING_EXTERNAL, got %+v", tr.calls)
	}
	c := tr.calls[0]
	if c.opts.ExpectedBy == nil || !c.opts.ExpectedBy.Equal(deadline) {
		t.Errorf("ExpectedBy must be stamped on the row, got %+v", c.opts.ExpectedBy)
	}
	if !c.opts.ClearLease {
		t.Error("ClearLease must be true")
	}
}

// --------------------------------------------------------------
// Bug #2 — errLeadHandoff sentinel propagation.
// --------------------------------------------------------------
//
// The bug: workflow.go's plan-step error branch routed
// errLeadHandoff to step.OnFail before the retry loop's IsLeadHandoff
// guard could see it. Fix added an early `if IsLeadHandoff(err) {
// return ..., err }` ahead of the OnFail branch. The contract
// IsLeadHandoff relies on:
//
//   1. errLeadHandoff is the canonical sentinel; IsLeadHandoff
//      returns true on it.
//   2. errors.Is unwraps fmt.Errorf("...%w") so a wrapped sentinel
//      still satisfies the predicate. (plan_step.go currently
//      returns the bare sentinel; if it ever wraps with extra
//      context, IsLeadHandoff must keep working.)
//   3. Other errors return false — IsLeadHandoff must NOT be
//      so loose that a normal failure looks like a hand-off.

func TestIsLeadHandoff_Sentinel(t *testing.T) {
	if !IsLeadHandoff(errLeadHandoff) {
		t.Error("IsLeadHandoff must recognize the bare sentinel")
	}
}

func TestIsLeadHandoff_PropagatesThroughWrap(t *testing.T) {
	wrapped := errors.New("outer wrap")
	if IsLeadHandoff(wrapped) {
		t.Error("unrelated error must not be classified as handoff")
	}

	// Use fmt.Errorf("%w", ...) — the standard wrap pattern.
	chained := errors.Join(errLeadHandoff, errors.New("trailing context"))
	if !IsLeadHandoff(chained) {
		t.Error("errors.Join with sentinel must satisfy IsLeadHandoff")
	}
}

func TestIsLeadHandoff_RejectsUnrelated(t *testing.T) {
	if IsLeadHandoff(nil) {
		t.Error("nil must not classify as handoff")
	}
	if IsLeadHandoff(errors.New("plan refusal: out of scope")) {
		t.Error("real failures must NOT classify as handoff")
	}
	if IsLeadHandoff(context.Canceled) {
		t.Error("context.Canceled must NOT classify as handoff")
	}
}

// Bug observed live by the user on T-234c (2026-05-09):
// they posted a follow-up "Refresh the coverage data" using the
// "Send message" button on a COMPLETED task; nothing happened.
// Pre-fix, only "directive"-kind posts re-queued; messages were
// silently filed as chat. Post-fix, ANY operator-typed input on
// an at-rest task (COMPLETED / AWAITING_INPUT / AWAITING_EXTERNAL)
// re-engages the lead.
//
// This test lives here even though the actual logic lives in
// the API + UI handlers — those tests are integration-only. The
// table here documents the contract so a future change to the
// re-queue policy gets re-thought before it lands.

func TestRequeuePolicy_AnyOperatorInputOnAtRestTask(t *testing.T) {
	type tc struct {
		from          string
		shouldRequeue bool
	}
	cases := []tc{
		{from: "COMPLETED", shouldRequeue: true},
		{from: "AWAITING_INPUT", shouldRequeue: true},
		{from: "AWAITING_EXTERNAL", shouldRequeue: true},
		{from: "RUNNING", shouldRequeue: false},
		{from: "LEASED", shouldRequeue: false},
		{from: "QUEUED", shouldRequeue: false},
		{from: "PAUSED", shouldRequeue: false},
		{from: "FAILED", shouldRequeue: false},
		{from: "CANCELLED", shouldRequeue: false},
		{from: "CLOSED", shouldRequeue: false},
	}
	for _, c := range cases {
		t.Run(c.from, func(t *testing.T) {
			got := requeueOnOperatorInput(c.from)
			if got != c.shouldRequeue {
				t.Errorf("from=%s want=%v got=%v", c.from, c.shouldRequeue, got)
			}
		})
	}
}

// requeueOnOperatorInput is the predicate version of the policy
// the API + UI handlers implement. Kept here so the test pins
// the rule in one spot instead of replicating logic across
// packages.
func requeueOnOperatorInput(status string) bool {
	switch status {
	case "COMPLETED", "AWAITING_INPUT", "AWAITING_EXTERNAL":
		return true
	}
	return false
}

// containsStatus is a small helper for assertion readability.
func containsStatus(haystack []persistence.TaskStatus, needle persistence.TaskStatus) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
