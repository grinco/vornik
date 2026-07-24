package persistence

import "context"

// MaxRequestRootWalkDepth bounds the per-level ancestor walk so that
// unexpected data (a ParentTaskID cycle, which should never occur but
// data heals imperfectly in practice) can't spin forever. Generous
// relative to any real workflow fan-out depth.
const MaxRequestRootWalkDepth = 25

// WalkOutcome classifies why an ancestor walk terminated for a given
// input task. It is the audit-grade companion to the request-root
// resolution: the agent-write policy (gateway.agent_writes, LLD
// 2026-07-22) both gates on it (user mode permits ONLY clean_root) and
// records it (every mode audits the outcome).
//
// The four values below are the outcomes an in-repo walk can produce.
// The api/audit layer adds two more that are NOT walk results —
// "not_walked" (off mode performs no walk) and "error" (a repo failure
// aborted the walk) — kept out of this enum so a repo error surfaces as
// a returned err, not a per-input classification.
type WalkOutcome string

const (
	// WalkOutcomeCleanRoot — the walk reached a genuine ParentTaskID==nil
	// root. This is the ONLY complete outcome; the request-root is
	// authoritative. complete ⟺ this value.
	WalkOutcomeCleanRoot WalkOutcome = "clean_root"
	// WalkOutcomeMissingParent — a ParentTaskID pointed at a row that
	// doesn't exist (deleted / never-persisted). The walk stopped at the
	// last-found task; the true root is unknown.
	WalkOutcomeMissingParent WalkOutcome = "missing_parent"
	// WalkOutcomeCycle — a ParentTaskID chain revisited an already-seen
	// task id (corrupt data). Detected explicitly and stopped early,
	// before the depth bound.
	WalkOutcomeCycle WalkOutcome = "cycle"
	// WalkOutcomeDepthExhausted — the walk hit maxDepth without reaching a
	// nil-parent root (a legitimately very deep tree, or an undetected
	// pathology). The true root is unknown.
	WalkOutcomeDepthExhausted WalkOutcome = "depth_exhausted"
)

// TaskLister is the minimal TaskRepository surface the ancestor walk
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
// This is the BEST-EFFORT grouping surface: a task whose parent row is
// missing (deleted) resolves to the last task that was found, a cycle
// resolves to whatever node the walk stopped on, and neither is
// distinguished from a genuine root — all fine for grouping. Callers
// that must tell a genuine root apart from an incomplete walk (the
// agent-write authorization path) MUST use
// ResolveRequestRootsWithCompleteness instead and inspect the
// WalkOutcome — do NOT infer authorization from this map.
//
// It is a thin, behaviour-preserving wrapper over
// ResolveRequestRootsWithCompleteness that discards the per-input
// outcome, so the signature (and every existing caller, e.g. the
// attention-queue grouping in internal/ui/inbox.go) is unchanged.
//
// The intentional contract is byte-identical roots for any ACYCLIC input
// (all real data). On corrupt CYCLIC data the resolved node can differ
// from the pre-completeness implementation — the completeness walk stops
// at the first revisited id rather than running to the depth bound — but
// that is best-effort grouping of data that should never exist, and
// stopping early is strictly an improvement, not a regression.
func ResolveRequestRoots(ctx context.Context, repo TaskLister, tasks []*Task, maxDepth int) (map[string]*Task, error) {
	roots, _, err := ResolveRequestRootsWithCompleteness(ctx, repo, tasks, maxDepth)
	return roots, err
}

// ResolveRequestRootsWithCompleteness is the completeness-returning
// variant of ResolveRequestRoots (LLD 2026-07-22 agent-write policy,
// review I-R3): identical batched walk, plus a per-input WalkOutcome so
// an authorization caller can distinguish a genuine ParentTaskID==nil
// root (WalkOutcomeCleanRoot) from an incomplete/ambiguous walk
// (missing_parent | cycle | depth_exhausted). "complete" in the design
// is exactly `outcome[id] == WalkOutcomeCleanRoot`; any other value is
// an authorization refuse (fail-closed).
//
// Batched, not per-task (Outcome Inbox review finding 2): each level
// collects the distinct ParentTaskIDs still needing resolution across
// the WHOLE input set and resolves them with one List(TaskFilter{IDs:
// ...}) call — so N tasks whose deepest chain is D levels cost at most D
// round-trips total, never N×D.
//
// maxDepth <= 0 defaults to MaxRequestRootWalkDepth. A repo List error
// aborts the whole walk and returns (nil, nil, err) — the authorization
// caller treats that as fail-closed.
func ResolveRequestRootsWithCompleteness(ctx context.Context, repo TaskLister, tasks []*Task, maxDepth int) (map[string]*Task, map[string]WalkOutcome, error) {
	if maxDepth <= 0 {
		maxDepth = MaxRequestRootWalkDepth
	}

	// current[originalTaskID] tracks that task's currently-known ancestor;
	// starts as the task itself and advances one ParentTaskID hop per loop
	// iteration.
	current := make(map[string]*Task, len(tasks))
	// outcome[originalTaskID] is the terminal classification for that
	// chain, set once when the chain stops advancing.
	outcome := make(map[string]WalkOutcome, len(tasks))
	// seen[originalTaskID] is the set of task ids visited on that chain, for
	// explicit cycle detection (a revisited id ⇒ cycle, before the depth
	// bound).
	seen := make(map[string]map[string]bool, len(tasks))
	// done tracks chains that have terminated (root, missing parent, or
	// cycle) — no more work for that chain.
	done := make(map[string]bool, len(tasks))

	for _, t := range tasks {
		if t == nil {
			continue
		}
		current[t.ID] = t
		seen[t.ID] = map[string]bool{t.ID: true}
	}

	for depth := 0; depth < maxDepth; depth++ {
		needed := pendingParentIDs(current, done, outcome)
		if len(needed) == 0 {
			break // every chain terminated
		}
		parents, err := repo.List(ctx, TaskFilter{IDs: needed})
		if err != nil {
			return nil, nil, err
		}
		advanceOneLevel(current, done, outcome, seen, byTaskID(parents))
	}

	// Any chain still not done after maxDepth levels ran out of budget
	// without reaching a nil-parent root: depth-exhausted (incomplete).
	for id := range current {
		if !done[id] {
			done[id] = true
			outcome[id] = WalkOutcomeDepthExhausted
		}
	}

	return current, outcome, nil
}

// ResolveLineageWithCompleteness walks a SINGLE task's ancestor chain and
// returns the full lineage task-ID set (the task itself + every ancestor up to
// the terminating node) plus the walk outcome — the taint-lineage write gate's
// input (taint-lineage-tracking-design.md §4.4). It reuses the SAME batched
// hop as ResolveRequestRootsWithCompleteness (one List per level; a single
// linear chain here) and the SAME WalkOutcome classification, so "complete" is
// exactly WalkOutcomeCleanRoot (D6): any other outcome (missing_parent | cycle
// | depth_exhausted | a repo error) is an incomplete walk the gate fails closed
// on.
//
// Unlike ResolveRequestRoots* which return only the ROOT, this returns EVERY
// task id visited, because a tainting step can live at any ancestor level and
// the gate's rollup must query them all. The returned slice always includes
// taskID first (even when the walk is incomplete, so the writing task's own
// rows are still consulted). A repo List error returns (the ids gathered so
// far, WalkOutcome(""), err) — the caller treats a non-nil err as fail-closed.
func ResolveLineageWithCompleteness(ctx context.Context, repo TaskLister, taskID string, maxDepth int) ([]string, WalkOutcome, error) {
	if maxDepth <= 0 {
		maxDepth = MaxRequestRootWalkDepth
	}
	if repo == nil || taskID == "" {
		return nil, WalkOutcomeMissingParent, nil
	}
	found, err := repo.List(ctx, TaskFilter{IDs: []string{taskID}})
	if err != nil {
		return nil, "", err
	}
	byID := byTaskID(found)
	cur, ok := byID[taskID]
	if !ok || cur == nil {
		// The writing task row itself is missing — nothing to walk; treat as an
		// incomplete lineage (fail-closed) but still surface the id.
		return []string{taskID}, WalkOutcomeMissingParent, nil
	}
	ids := []string{taskID}
	seen := map[string]bool{taskID: true}
	for depth := 0; depth < maxDepth; depth++ {
		if cur.ParentTaskID == nil || *cur.ParentTaskID == "" {
			return ids, WalkOutcomeCleanRoot, nil
		}
		parentID := *cur.ParentTaskID
		if seen[parentID] {
			return ids, WalkOutcomeCycle, nil
		}
		parents, perr := repo.List(ctx, TaskFilter{IDs: []string{parentID}})
		if perr != nil {
			return ids, "", perr
		}
		parent, pok := byTaskID(parents)[parentID]
		if !pok || parent == nil {
			return ids, WalkOutcomeMissingParent, nil
		}
		ids = append(ids, parentID)
		seen[parentID] = true
		cur = parent
	}
	return ids, WalkOutcomeDepthExhausted, nil
}

// pendingParentIDs collects the distinct ParentTaskIDs still needing
// resolution across every not-done chain in current, marking any chain
// that has reached a root (ParentTaskID nil) as done + clean_root in
// place.
func pendingParentIDs(current map[string]*Task, done map[string]bool, outcome map[string]WalkOutcome) []string {
	need := make(map[string]struct{}, len(current))
	for id, t := range current {
		if done[id] {
			continue
		}
		if t.ParentTaskID == nil || *t.ParentTaskID == "" {
			done[id] = true
			outcome[id] = WalkOutcomeCleanRoot
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
// whose parent row is missing (deleted) terminates as missing_parent; a
// chain whose parent is an already-seen id terminates as cycle. The
// current task is left as that chain's last-known node either way (so
// the best-effort grouping wrapper still gets a stable node).
func advanceOneLevel(current map[string]*Task, done map[string]bool, outcome map[string]WalkOutcome, seen map[string]map[string]bool, resolved map[string]*Task) {
	for id, t := range current {
		if done[id] {
			continue
		}
		parent, ok := resolved[*t.ParentTaskID]
		if !ok {
			done[id] = true
			outcome[id] = WalkOutcomeMissingParent
			continue
		}
		if seen[id][parent.ID] {
			done[id] = true
			outcome[id] = WalkOutcomeCycle
			continue
		}
		seen[id][parent.ID] = true
		current[id] = parent
	}
}
