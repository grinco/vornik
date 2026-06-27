package executor

// Inter-project cycle detection + depth limit. See
// https://docs.vornik.io → "Later — Inter-project cycle detection +
// depth limit". Two guardrails applied at call_project create time:
//
//   1. Depth cap. The depth counter starts at 0 on operator/
//      autonomy-created tasks and increments by 1 on every
//      call_project hop. A hop that would push depth past the
//      project's EffectiveMaxCallDepth is refused with
//      DEPTH_EXCEEDED.
//
//   2. Cycle detection. Walks the caller's parent chain
//      collecting project IDs; if the proposed callee is
//      already in the chain — i.e. an ancestor of the caller
//      — refuse with CYCLE_DETECTED.
//
// Both checks share a single lineage walk so the per-call cost
// stays O(current depth) regardless of which guard fires.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"vornik.io/vornik/internal/persistence"
)

// lineageWalkHardLimit caps the parent-chain walk so a corrupt
// task graph (a cycle in parent_task_id, which the schema doesn't
// forbid) can't infinite-loop the executor. Sized well above the
// DefaultMaxCallDepth so legitimate deep chains aren't truncated
// before the depth check fires.
const lineageWalkHardLimit = 256

// lineageInfo describes the ancestor chain of a caller task,
// surfaced to the depth + cycle guards in one shot.
type lineageInfo struct {
	// Depth is the count of CROSS-PROJECT hops in the chain
	// from the root to the caller (inclusive). A task created
	// directly by an operator → depth 0. A task created by one
	// call_project hop → depth 1. Increments only when the
	// parent and child project IDs differ — in-project
	// delegation doesn't bump depth.
	Depth int

	// AncestorProjects is the set of project IDs encountered
	// while walking from caller back to root. Includes the
	// caller's own project ID. Used by the cycle check: if the
	// proposed callee project is already in this set, a cycle
	// would form.
	AncestorProjects map[string]struct{}

	// LineagePath is the ordered list of project IDs (root
	// last, caller first) for audit logging. Bounded by
	// lineageWalkHardLimit.
	LineagePath []string

	// Truncated is true when the walker hit lineageWalkHardLimit
	// before reaching a root task. Indicates corrupt data; the
	// guard treats truncation as a "could not verify" and lets
	// the caller decide whether to refuse defensively.
	Truncated bool
}

// taskLineageGetter is the narrow interface walkCallerLineage
// needs. *persistence.TaskRepository satisfies it via Get.
type taskLineageGetter interface {
	Get(ctx context.Context, id string) (*persistence.Task, error)
}

// walkCallerLineage walks the parent_task_id chain from `caller`
// back toward the root, counting cross-project hops and
// collecting ancestor project IDs. Returns the populated
// lineageInfo or an error if a Get fails (other than ErrNotFound,
// which is treated as "chain terminates here" — a task whose
// ParentTaskID points at a deleted row).
//
// The walk is bounded by lineageWalkHardLimit so a corrupt
// parent_task_id loop can't run forever.
func walkCallerLineage(ctx context.Context, repo taskLineageGetter, caller *persistence.Task) (lineageInfo, error) {
	out := lineageInfo{
		AncestorProjects: map[string]struct{}{},
	}
	if caller == nil {
		return out, nil
	}
	// Seed with the caller itself — its project counts toward
	// the cycle-check ancestor set.
	out.AncestorProjects[caller.ProjectID] = struct{}{}
	out.LineagePath = append(out.LineagePath, caller.ProjectID)

	current := caller
	for hops := 0; hops < lineageWalkHardLimit; hops++ {
		if current.ParentTaskID == nil || *current.ParentTaskID == "" {
			return out, nil
		}
		parent, err := repo.Get(ctx, *current.ParentTaskID)
		if err != nil {
			if errors.Is(err, persistence.ErrNotFound) {
				// Parent row was deleted (project archive sweep,
				// manual cleanup). Treat the chain as terminating;
				// depth + cycle checks operate on what we found.
				return out, nil
			}
			return out, fmt.Errorf("walk lineage: get parent %q: %w", *current.ParentTaskID, err)
		}
		if parent == nil {
			return out, nil
		}
		// Count a hop only when the project changes — in-project
		// delegation doesn't burn cross-project budget and
		// shouldn't bump the depth meter.
		if parent.ProjectID != current.ProjectID {
			out.Depth++
		}
		if _, already := out.AncestorProjects[parent.ProjectID]; !already {
			out.AncestorProjects[parent.ProjectID] = struct{}{}
		}
		out.LineagePath = append(out.LineagePath, parent.ProjectID)
		current = parent
	}
	out.Truncated = true
	return out, nil
}

// readCarriedCallDepth extracts the chain-depth "header" carried on
// a task's payload — the `context.callDepth` field that
// buildCalleePayload stamps on every callee task (= the post-hop
// chain depth at creation time). This is the cross-boundary
// backstop to walkCallerLineage: the lineage walk derives depth
// from the stored parent_task_id chain, but that chain can be
// truncated (lineageWalkHardLimit) or broken (an ancestor row
// deleted by an archive sweep), which would silently reset the
// depth meter to 0 and let a runaway chain continue. The depth
// guard takes max(walked, carried) so a broken lineage can't
// defeat the cap.
//
// Defensive by construction: a nil task, absent/malformed payload,
// missing field, non-numeric value, or a negative number all
// resolve to 0 — a corrupt or hand-tampered header can only ever
// *fail to raise* the effective depth, never lower it below what
// the lineage walk independently established.
func readCarriedCallDepth(task *persistence.Task) int {
	if task == nil || len(task.Payload) == 0 {
		return 0
	}
	var envelope struct {
		Context struct {
			CallDepth *float64 `json:"callDepth"`
		} `json:"context"`
	}
	if err := json.Unmarshal(task.Payload, &envelope); err != nil {
		return 0
	}
	if envelope.Context.CallDepth == nil {
		return 0
	}
	d := int(*envelope.Context.CallDepth)
	if d < 0 {
		return 0
	}
	return d
}
