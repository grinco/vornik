package executor

import (
	"context"
	"path/filepath"
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
//   - Empty:    child task IDs whose latest COMPLETED execution contributed
//     nothing stageable (the T-06b5 failure class — a child that
//     "succeeded" but said nothing; must never be dropped). Covers
//     both zero artifacts and, when the consumer step sets
//     stage_child_artifacts_include, zero artifacts MATCHING that
//     glob — so an over-narrow glob is visible, not silent.
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
//
// includeGlob optionally narrows WHICH artifacts stage, matched against each
// artifact's OWN name (before the <childShortID>- staging prefix). Empty means
// "all of them" — step 2's canonical rule, unchanged. See
// registry.WorkflowStep.StageChildArtifactsInclude for why the narrowing exists
// (T-1089: per-child response transcripts doubling the consumer's input). A
// child that produced artifacts but none matching the glob is reported in
// summary.Empty, so an over-narrow glob surfaces through the same completeness
// contract instead of looking like full coverage.
func (e *Executor) gatherChildArtifacts(ctx context.Context, resumeChildren []*persistence.Task, includeGlob string) ([]map[string]string, childArtifactSummary) {
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
		arts = filterArtifactsByName(arts, includeGlob)
		if len(arts) == 0 {
			// The child DID produce artifacts, but none the consumer asked for.
			// Report it empty rather than dropping it: an over-narrow glob must
			// surface through the same completeness contract that catches a
			// genuinely empty child (T-06b5), not masquerade as full coverage.
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

// filterArtifactsByName keeps only artifacts whose Name matches glob. An empty
// glob is "keep everything" (the default, canonical rule).
//
// A malformed pattern is rejected at config load by
// registry.validateStageChildArtifacts, so filepath.Match cannot realistically
// error here; if it somehow does, the artifact is DROPPED rather than kept. That
// is the safe direction: a dropped child surfaces in inputArtifactsSummary.empty[]
// and the consumer's prompt contract makes it refuse to invent content, whereas
// silently keeping everything would hand the consumer the very input bloat the
// glob exists to prevent — invisibly.
func filterArtifactsByName(arts []*persistence.Artifact, glob string) []*persistence.Artifact {
	if glob == "" {
		return arts
	}
	out := make([]*persistence.Artifact, 0, len(arts))
	for _, a := range arts {
		if a == nil {
			continue
		}
		if ok, err := filepath.Match(glob, a.Name); err == nil && ok {
			out = append(out, a)
		}
	}
	return out
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
