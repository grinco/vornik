package persistence

import "context"

// MaxRequestRootWalkDepth bounds ResolveRequestRoots's per-level walk so
// that unexpected data (a ParentTaskID cycle, which should never occur
// but data heals imperfectly in practice) can't spin forever. Generous
// relative to any real workflow fan-out depth.
const MaxRequestRootWalkDepth = 25

// TaskLister is the minimal TaskRepository surface ResolveRequestRoots
// needs. Declared narrowly (rather than requiring the full
// TaskRepository interface) so callers — and tests — can supply a
// lighter fake without implementing every TaskRepository method.
// persistence.TaskRepository satisfies this trivially.
type TaskLister interface {
	List(ctx context.Context, filter TaskFilter) ([]*Task, error)
}

// ResolveRequestRoots batch-resolves each task's request-root — the
// topmost ParentTaskID ancestor, or the task itself when it has no
// parent — for every task in tasks. Returns a map keyed by the INPUT
// task's ID (not the intermediate levels) to its root *Task.
//
// A request (Outcome Inbox design §5.3) is the root of a ParentTaskID
// chain; the attention queue's assembly folds each returned task to its
// request-root so a parent + its children render as one card, not N.
//
// Batched, not per-task (review finding 2): each level collects the
// distinct ParentTaskIDs still needing resolution across the WHOLE
// input set and resolves them with one List(TaskFilter{IDs: ...}) call
// — so N tasks whose deepest chain is D levels cost at most D
// round-trips total, never N×D (a per-task ancestor walk).
//
// maxDepth <= 0 defaults to MaxRequestRootWalkDepth. A task whose
// parent row is missing (e.g. deleted) resolves to the last task that
// was found — the walk simply stops there rather than erroring.
func ResolveRequestRoots(ctx context.Context, repo TaskLister, tasks []*Task, maxDepth int) (map[string]*Task, error) {
	if maxDepth <= 0 {
		maxDepth = MaxRequestRootWalkDepth
	}

	// current[originalTaskID] tracks that task's currently-known
	// ancestor; starts as the task itself and advances one
	// ParentTaskID hop per loop iteration.
	current := make(map[string]*Task, len(tasks))
	for _, t := range tasks {
		if t == nil {
			continue
		}
		current[t.ID] = t
	}

	// done tracks chains that have reached a root (ParentTaskID nil)
	// or hit a missing parent row — no more work for that chain.
	done := make(map[string]bool, len(tasks))

	for depth := 0; depth < maxDepth; depth++ {
		needed := pendingParentIDs(current, done)
		if len(needed) == 0 {
			break // every chain reached a root
		}
		parents, err := repo.List(ctx, TaskFilter{IDs: needed})
		if err != nil {
			return nil, err
		}
		advanceOneLevel(current, done, byTaskID(parents))
	}

	return current, nil
}

// pendingParentIDs collects the distinct ParentTaskIDs still needing
// resolution across every not-done chain in current, marking any chain
// that has already reached a root (ParentTaskID nil) as done in place.
func pendingParentIDs(current map[string]*Task, done map[string]bool) []string {
	need := make(map[string]struct{}, len(current))
	for id, t := range current {
		if done[id] {
			continue
		}
		if t.ParentTaskID == nil || *t.ParentTaskID == "" {
			done[id] = true
			continue
		}
		need[*t.ParentTaskID] = struct{}{}
	}
	ids := make([]string, 0, len(need))
	for id := range need {
		ids = append(ids, id)
	}
	return ids
}

// byTaskID indexes a task slice by ID, skipping nils.
func byTaskID(tasks []*Task) map[string]*Task {
	out := make(map[string]*Task, len(tasks))
	for _, t := range tasks {
		if t != nil {
			out[t.ID] = t
		}
	}
	return out
}

// advanceOneLevel moves every not-done chain in current one ParentTaskID
// hop forward using the just-resolved parents (keyed by ID). A chain
// whose parent row is missing (deleted) is marked done — the walk stops
// there, treating the current task as that chain's root.
func advanceOneLevel(current map[string]*Task, done map[string]bool, resolved map[string]*Task) {
	for id, t := range current {
		if done[id] {
			continue
		}
		parent, ok := resolved[*t.ParentTaskID]
		if !ok {
			done[id] = true
			continue
		}
		current[id] = parent
	}
}
