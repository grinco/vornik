package executor

import (
	"context"
	"sort"

	"vornik.io/vornik/internal/persistence"
)

// childArtifactSummary is the deterministic completeness report produced
// alongside a child-artifact gather. It is surfaced to the resuming step
// (as inputArtifactsSummary) so a workflow can note incomplete children in
// its output rather than silently proceeding.
//
//   - Expected: number of delegation-engine children considered.
//   - Staged:   number of artifact entries emitted.
//   - Missing:  child task IDs with NO COMPLETED execution.
//   - Empty:    child task IDs whose latest COMPLETED execution produced
//     ZERO artifacts (the T-06b5 failure class — a child that
//     "succeeded" but contributed nothing; must never be dropped).
type childArtifactSummary struct {
	Expected int
	Staged   int
	Missing  []string
	Empty    []string
}

// childShortIDLen is the number of trailing characters of a child task id
// used to prefix its staged artifact names. Mirrors the existing
// right(task_id, 12) convention used elsewhere for operator-visible short
// ids, giving enough entropy to keep two children's identically-named
// artifacts distinct in the flat artifacts/in/ directory.
const childShortIDLen = 12

// childShortID returns the last childShortIDLen characters of a task id
// (or the whole id if shorter). Used as the collision-avoidance prefix on
// staged artifact names.
func childShortID(taskID string) string {
	if len(taskID) <= childShortIDLen {
		return taskID
	}
	return taskID[len(taskID)-childShortIDLen:]
}

// gatherChildArtifacts deterministically collects the output artifacts of a
// resuming step's delegated children from the durable store, staging them
// for injection into the parent step's artifacts/in/. There is NO LLM in
// this path and the result is fully reproducible.
//
// Behavior (see the delegated-child-artifact-handoff design, §3.1–§3.3):
//
//  1. resumeChildren is filtered to delegation-engine children only —
//     those with DelegationMode != nil. This is the airtight
//     delegation-only discriminator: createDelegatedTasks and the
//     strict-adaptive route stamp DelegationMode, whereas call_project /
//     spawn_project callees (CreationSource=DELEGATION but DelegationMode
//     nil) and checkpoint-retry children (neither set) are excluded. So a
//     cross-project call or a retry can never stage unintended artifacts.
//  2. For each remaining child, its SINGLE latest COMPLETED execution is
//     selected: max created_at among that task's COMPLETED executions,
//     tiebroken by executionID (unique) so a created_at collision is still
//     deterministic. ALL artifacts of that one execution are staged (one
//     canonical rule; no cross-execution per-name mixing).
//  3. Each staged entry is {name: "<childShortID>-<artifactName>",
//     sourcePath: <artifact StoragePath>}. The childShortID prefix
//     prevents cross-child collisions in the flat artifacts/in/ directory
//     (two children both emitting findings.md stay distinct, no overwrite).
//  4. Ordering is deterministic: children by (created_at, id), then a
//     child's artifacts by name.
//
// A child with no COMPLETED execution is reported in summary.Missing; a
// child whose latest COMPLETED execution produced zero artifacts is
// reported in summary.Empty. Neither is silently dropped.
func (e *Executor) gatherChildArtifacts(ctx context.Context, resumeChildren []*persistence.Task) ([]map[string]string, childArtifactSummary) {
	// 1. Filter to delegation-engine children, then order deterministically
	// by (created_at, id) — the stable dispatch key.
	delegChildren := make([]*persistence.Task, 0, len(resumeChildren))
	for _, c := range resumeChildren {
		if c == nil || c.DelegationMode == nil {
			continue
		}
		delegChildren = append(delegChildren, c)
	}
	sort.SliceStable(delegChildren, func(i, j int) bool {
		a, b := delegChildren[i], delegChildren[j]
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.ID < b.ID
	})

	summary := childArtifactSummary{Expected: len(delegChildren)}
	entries := make([]map[string]string, 0)

	for _, child := range delegChildren {
		latest := e.latestCompletedExecution(ctx, child.ID)
		if latest == nil {
			// No COMPLETED execution — the child never delivered.
			summary.Missing = append(summary.Missing, child.ID)
			continue
		}

		arts := e.completedExecutionArtifacts(ctx, latest.ID)
		if len(arts) == 0 {
			// COMPLETED but contributed nothing — the T-06b5 failure class.
			summary.Empty = append(summary.Empty, child.ID)
			continue
		}

		// Deterministic per-child order: by artifact name.
		sort.SliceStable(arts, func(i, j int) bool { return arts[i].Name < arts[j].Name })

		short := childShortID(child.ID)
		for _, a := range arts {
			entries = append(entries, map[string]string{
				"name":       short + "-" + a.Name,
				"sourcePath": a.StoragePath,
			})
		}
	}

	summary.Staged = len(entries)
	if len(entries) == 0 {
		// Normalize to nil so callers/tests see an empty (not zero-cap) slice
		// only when there is genuinely nothing to stage.
		entries = nil
	}
	return entries, summary
}

// latestCompletedExecution returns the child task's single latest COMPLETED
// execution — max created_at, tiebroken by executionID (unique) so a
// created_at collision is still deterministic — or nil if the task has no
// COMPLETED execution. COMPLETED is enforced here (not only via the repo
// filter) so the selection rule is authoritative regardless of the backing
// store.
func (e *Executor) latestCompletedExecution(ctx context.Context, taskID string) *persistence.Execution {
	tid := taskID
	execs, err := e.execRepo.List(ctx, persistence.ExecutionFilter{TaskID: &tid})
	if err != nil {
		e.logger.Warn().Err(err).
			Str("child_task_id", taskID).
			Msg("gather_child_artifacts: failed to list child executions")
		return nil
	}

	var best *persistence.Execution
	for _, ex := range execs {
		if ex == nil || ex.Status != persistence.ExecutionStatusCompleted {
			continue
		}
		if best == nil ||
			ex.CreatedAt.After(best.CreatedAt) ||
			(ex.CreatedAt.Equal(best.CreatedAt) && ex.ID > best.ID) {
			best = ex
		}
	}
	return best
}

// completedExecutionArtifacts lists all artifacts of a single execution
// (the child's chosen latest-COMPLETED execution). Scoping by ExecutionID
// stages exactly that one execution's outputs — no cross-execution mixing.
func (e *Executor) completedExecutionArtifacts(ctx context.Context, execID string) []*persistence.Artifact {
	eid := execID
	arts, err := e.artifactRepo.List(ctx, persistence.ArtifactFilter{ExecutionID: &eid})
	if err != nil {
		e.logger.Warn().Err(err).
			Str("execution_id", execID).
			Msg("gather_child_artifacts: failed to list execution artifacts")
		return nil
	}
	out := make([]*persistence.Artifact, 0, len(arts))
	for _, a := range arts {
		if a == nil {
			continue
		}
		out = append(out, a)
	}
	return out
}
