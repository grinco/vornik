package executor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
)

// taskRepoForDelegationTest implements just the persistence.TaskRepository
// methods createDelegatedTasks actually uses, with optional failure
// injection so the partial-failure rollback can be exercised.
type taskRepoForDelegationTest struct {
	mu      sync.Mutex
	created []*persistence.Task
	deleted []string
	failOn  int // 1-indexed: trip the failOn-th Create call. 0 disables.
	calls   int
}

func (r *taskRepoForDelegationTest) Create(_ context.Context, t *persistence.Task) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.failOn > 0 && r.calls == r.failOn {
		return errors.New("simulated db conflict")
	}
	r.created = append(r.created, t)
	return nil
}

func (r *taskRepoForDelegationTest) Delete(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deleted = append(r.deleted, id)
	return nil
}

// The rest of the interface is unused by createDelegatedTasks; if a
// future change adds another call, the test panics loud.
func (r *taskRepoForDelegationTest) Ping(context.Context) error { panic("not used") }
func (r *taskRepoForDelegationTest) Get(context.Context, string) (*persistence.Task, error) {
	panic("not used")
}
func (r *taskRepoForDelegationTest) GetByIdempotencyKey(context.Context, string, string) (*persistence.Task, error) {
	panic("not used")
}
func (r *taskRepoForDelegationTest) Update(context.Context, *persistence.Task) error {
	panic("not used")
}
func (r *taskRepoForDelegationTest) List(context.Context, persistence.TaskFilter) ([]*persistence.Task, error) {
	panic("not used")
}
func (r *taskRepoForDelegationTest) Count(context.Context, persistence.TaskFilter) (int64, error) {
	panic("not used")
}
func (r *taskRepoForDelegationTest) UpdateStatus(context.Context, string, persistence.TaskStatus) error {
	panic("not used")
}
func (r *taskRepoForDelegationTest) TransitionConditional(context.Context, string, []persistence.TaskStatus, persistence.TaskStatus, persistence.TransitionOpts) (bool, error) {
	panic("not used")
}
func (r *taskRepoForDelegationTest) TransitionToCancelled(context.Context, string) (bool, error) {
	panic("not used")
}
func (r *taskRepoForDelegationTest) RequeueTerminalTask(context.Context, string, int, int) (bool, error) {
	panic("not used")
}
func (r *taskRepoForDelegationTest) LeaseTask(context.Context, persistence.LeaseOptions) (*persistence.Task, error) {
	panic("not used")
}
func (r *taskRepoForDelegationTest) RenewLease(context.Context, string, string, int) error {
	panic("not used")
}
func (r *taskRepoForDelegationTest) ReleaseLease(context.Context, string, string, persistence.TaskStatus, persistence.ReleaseOptions) error {
	panic("not used")
}
func (r *taskRepoForDelegationTest) FindExpiredLeases(context.Context, int) ([]*persistence.Task, error) {
	panic("not used")
}
func (r *taskRepoForDelegationTest) CountByStatus(context.Context, string) (map[persistence.TaskStatus]int64, error) {
	panic("not used")
}
func (r *taskRepoForDelegationTest) CountRecentFailures(context.Context, string, []string, interface{}) (int, error) {
	panic("not used")
}
func (r *taskRepoForDelegationTest) GetChildren(context.Context, string) ([]*persistence.Task, error) {
	// createDelegatedTasks now consults this for the cumulative fan-out cap.
	// The partial-failure test exercises a fresh parent, so report no
	// pre-existing children.
	return nil, nil
}
func (r *taskRepoForDelegationTest) CountChildrenForParents(context.Context, []string) (map[string]int, error) {
	panic("not used")
}
func (r *taskRepoForDelegationTest) GetDependencies(context.Context, string) ([]*persistence.Task, error) {
	panic("not used")
}
func (r *taskRepoForDelegationTest) GetDependents(context.Context, string) ([]*persistence.Task, error) {
	panic("not used")
}

func TestCreateDelegatedTasks_HappyPath(t *testing.T) {
	// We can't satisfy the full TaskRepository interface here; use the
	// existing MockTaskRepo from executor_test.go which already does.
	e, _, _, _, tr := setup()

	parent := &persistence.Task{ID: "parent-1", ProjectID: "proj-1", Priority: 50}
	specs := []delegatedTaskSpec{
		{Prompt: "child 1 prompt", Role: "writer"},
		{Prompt: "child 2 prompt", Role: "researcher", Priority: 80}, // explicit priority
	}
	err := e.createDelegatedTasks(context.Background(), parent, specs, persistence.DelegationModeSequential)
	require.NoError(t, err)

	// Two children inserted in the mock.
	tr.mu.Lock()
	tasks := make([]*persistence.Task, 0, len(tr.tasks))
	for _, v := range tr.tasks {
		tasks = append(tasks, v)
	}
	tr.mu.Unlock()
	require.Len(t, tasks, 2)

	// Each child has the parent linked and the project copied.
	for _, c := range tasks {
		require.NotNil(t, c.ParentTaskID)
		assert.Equal(t, "parent-1", *c.ParentTaskID)
		assert.Equal(t, "proj-1", c.ProjectID)
		assert.Equal(t, persistence.TaskStatusQueued, c.Status)
		assert.Equal(t, persistence.TaskCreationSourceDelegation, c.CreationSource)
		require.NotNil(t, c.DelegationMode)
		assert.Equal(t, persistence.DelegationModeSequential, *c.DelegationMode)
		// No Workflow on these specs → project default (nil WorkflowID).
		assert.Nil(t, c.WorkflowID, "no spec.Workflow → child uses project default")
	}
}

// TestCreateDelegatedTasks_PinsWorkflow: a spec with Workflow set pins the
// child's WorkflowID (so a decomposer routes subtasks to a specific workflow,
// e.g. issue-fix → issue-subtask, not the project default).
func TestCreateDelegatedTasks_PinsWorkflow(t *testing.T) {
	e, _, _, _, tr := setup()
	parent := &persistence.Task{ID: "parent-wf", ProjectID: "proj-1", Priority: 50}
	specs := []delegatedTaskSpec{
		{Prompt: "subtask 1", Workflow: "issue-subtask"},
		{Prompt: "subtask 2", Workflow: " "}, // whitespace → treated as unset
	}
	require.NoError(t, e.createDelegatedTasks(context.Background(), parent, specs, persistence.DelegationModeSequential))

	tr.mu.Lock()
	var pinned, unset int
	for _, c := range tr.tasks {
		if c.WorkflowID != nil && *c.WorkflowID == "issue-subtask" {
			pinned++
		} else if c.WorkflowID == nil {
			unset++
		}
	}
	tr.mu.Unlock()
	assert.Equal(t, 1, pinned, "spec.Workflow=issue-subtask should pin the child WorkflowID")
	assert.Equal(t, 1, unset, "blank spec.Workflow should leave WorkflowID nil (project default)")
}

func TestCreateDelegatedTasks_PartialFailureRollsBackCreated(t *testing.T) {
	// Use the lightweight taskRepoForDelegationTest so we can inject a
	// mid-batch Create failure and capture Delete calls.
	repo := &taskRepoForDelegationTest{failOn: 2}
	e := &Executor{
		taskRepo: repo,
		logger:   zerolog.Nop(),
	}
	parent := &persistence.Task{ID: "p-1", ProjectID: "p", Priority: 50}
	specs := []delegatedTaskSpec{
		{Prompt: "c1", Role: "r"},
		{Prompt: "c2", Role: "r"}, // ← Create fails here
		{Prompt: "c3", Role: "r"}, // never reached
	}
	err := e.createDelegatedTasks(context.Background(), parent, specs, persistence.DelegationModeSequential)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to create delegated task")

	repo.mu.Lock()
	defer repo.mu.Unlock()
	// Only first child was created and tracked.
	require.Len(t, repo.created, 1)
	// Rollback deleted the one that was successfully created.
	assert.Equal(t, []string{repo.created[0].ID}, repo.deleted)
}

// TestCreateDelegatedTasks_CumulativeFanOutCap is the regression for the
// batch-4 review finding: the fan-out guard was per-BATCH, so a parent could
// exceed the limit by delegating across multiple batches (retries / multi-step
// workflows). The cap is now cumulative over the parent's lifetime — total
// direct children must stay within the limit. A 1-spec batch that pushes the
// parent past the limit passed pre-fix (1 <= limit) and must now be refused.
func TestCreateDelegatedTasks_CumulativeFanOutCap(t *testing.T) {
	e, _, _, _, tr := setup()
	parent := &persistence.Task{ID: "parent-1", ProjectID: "proj-1", Priority: 50}
	limit := e.delegationFanOutLimit()

	// Seed limit-1 children from "earlier batches" so the parent sits just
	// under the cap.
	for i := 0; i < limit-1; i++ {
		pid := parent.ID
		tr.AddTask(&persistence.Task{
			ID: fmt.Sprintf("seed-child-%d", i), ProjectID: "proj-1",
			ParentTaskID: &pid, CreationSource: persistence.TaskCreationSourceDelegation,
			Status: persistence.TaskStatusQueued,
		})
	}

	// Batch of 1 → reaches exactly the limit → allowed (boundary).
	err := e.createDelegatedTasks(context.Background(), parent,
		[]delegatedTaskSpec{{Prompt: "ok at the limit", Role: "writer"}},
		persistence.DelegationModeParallel)
	require.NoError(t, err, "reaching exactly the limit must be allowed")

	// One more 1-spec batch → would exceed the cumulative cap → refused, even
	// though the per-batch count (1) is well under the limit.
	err = e.createDelegatedTasks(context.Background(), parent,
		[]delegatedTaskSpec{{Prompt: "over the cumulative cap", Role: "writer"}},
		persistence.DelegationModeParallel)
	require.Error(t, err)
	var ge *delegationGuardError
	require.True(t, errors.As(err, &ge), "expected a delegationGuardError, got %v", err)
	assert.Equal(t, "fanout", ge.reason)
}
