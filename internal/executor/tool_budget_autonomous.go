package executor

import (
	"context"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence"
)

// budgetCheckpointWalkCap bounds the checkpoint parent-chain walk in
// resolveBudgetAutonomous. Empirically a checkpoint chain is <=3 deep (a task
// rarely checkpoints more than a couple of times); 16 is an arbitrary safety
// margin against a pathological or cyclic chain, not an expected value.
const budgetCheckpointWalkCap = 16

// taskGetter loads a task by id. It matches persistence.TaskRepository.Get (and
// the executor's narrow TaskRepository), so e.taskRepo.Get satisfies it; a fake
// exercises the chain walk in tests without a database.
type taskGetter func(ctx context.Context, id string) (*persistence.Task, error)

// resolveBudgetAutonomous reports whether a task should be held to the tighter
// AutonomyMaxFactor for tool-budget purposes (dynamic-tool-budget follow-ups §5).
//
// A CHECKPOINT continuation inherits the autonomousness of its ORIGIN — the
// first non-checkpoint ancestor — so an operator-initiated (User) task keeps its
// operator budget headroom across checkpoint-retries, while a checkpoint of
// autonomous work stays autonomous. A non-checkpoint task uses the direct
// CreationSource test (the unchanged fast path; zero extra work for the common
// case).
//
// Fail-safe: any inability to resolve the origin (no parent, lookup error,
// depth-cap/cycle) returns true (autonomous). That is the conservative default —
// it matches today's behavior and can never WIDEN the budget beyond it.
func resolveBudgetAutonomous(ctx context.Context, task *persistence.Task, get taskGetter, logger zerolog.Logger) bool {
	if task == nil {
		return true
	}
	if task.CreationSource != persistence.TaskCreationSourceCheckpoint {
		return task.CreationSource != persistence.TaskCreationSourceUser
	}
	cur := task
	for i := 0; i < budgetCheckpointWalkCap; i++ {
		if cur.ParentTaskID == nil || *cur.ParentTaskID == "" {
			return true // no origin to resolve → fail safe
		}
		parent, err := get(ctx, *cur.ParentTaskID)
		if err != nil || parent == nil {
			return true // lookup miss → fail safe
		}
		if parent.CreationSource != persistence.TaskCreationSourceCheckpoint {
			return parent.CreationSource != persistence.TaskCreationSourceUser
		}
		cur = parent
	}
	// A chain longer than the cap (or a cycle) is pathological; fail safe and
	// leave a breadcrumb for diagnosis. DEBUG, not WARN — the path is safe.
	logger.Debug().
		Str("task_id", task.ID).
		Int("walk_cap", budgetCheckpointWalkCap).
		Msg("tool_budget: checkpoint chain exceeded walk cap; failing safe to autonomous")
	return true
}

// budgetAutonomous is the executor-bound wrapper used at the two tool-budget
// sites (the iteration budget in container.go and its coupled step-timeout twin
// in workflow.go). Both MUST use it so iteration and timeout budgets move
// together (parent design §6.1).
func (e *Executor) budgetAutonomous(ctx context.Context, task *persistence.Task) bool {
	return resolveBudgetAutonomous(ctx, task, e.taskRepo.Get, e.logger)
}
