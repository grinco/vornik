package ui

import (
	"context"

	"vornik.io/vornik/internal/persistence"
)

// Bounds on the descendant walk. A task tree is normally a handful of
// delegated children one or two levels deep; these caps exist so a corrupt
// parent/child cycle or a pathological fan-out cannot turn a page render into
// an unbounded query storm. Both are deliberately generous relative to real
// workflows — hitting either means something is wrong, not that a legitimate
// tree was truncated.
const (
	lineageMaxTasks = 200
	lineageMaxDepth = 8
)

// descendantTaskIDs returns the ids of every task beneath rootID that belongs
// to projectID, breadth first, excluding rootID itself.
//
// Why this exists (2026-08-05 janka-companion incident): a chat request became
// a routing task that delegated the real work to a child. Everything the
// operator wanted — a 31 KB report — was attached to the child, while the
// parent owned two ~300-byte router responses. Every task-scoped artifact read
// filtered on task_id alone, so the parent page showed the scraps and hid the
// deliverable, and the operator concluded the work had produced nothing.
//
// THE PROJECT FILTER IS A TENANCY BOUNDARY, NOT AN OPTIMISATION.
// tasks.parent_task_id legitimately spans projects: a cross-project call
// (executor/call_project.go) creates the callee with ProjectID = the TARGET
// project and ParentTaskID = the CALLER's task, and
// TaskRepository.GetChildren carries no project predicate, so those children
// come back from this walk. Callers authorize against the ROOT task's project
// (uiRequireProjectScope) and nothing downstream re-checks a descendant — so
// without this filter the walk would hand a caller another tenant's artifacts.
// Children in a different project are skipped AND not traversed, so the walk
// cannot re-enter this project through a foreign subtree either.
//
// Cycle-safe (a visited set, so a parent/child cycle terminates) and bounded
// by lineageMaxTasks / lineageMaxDepth. A repo error mid-walk truncates the
// result rather than failing the caller: a partial artifact list degrades the
// page, an error blanks it.
func descendantTaskIDs(ctx context.Context, repo persistence.TaskRepository, rootID, projectID string) []string {
	if repo == nil || rootID == "" || projectID == "" {
		return nil
	}
	visited := map[string]bool{rootID: true}
	var out []string
	frontier := []string{rootID}

	for depth := 0; depth < lineageMaxDepth && len(frontier) > 0; depth++ {
		var next []string
		for _, parentID := range frontier {
			children, err := repo.GetChildren(ctx, parentID)
			if err != nil {
				// Best effort: keep what we have rather than blanking the page.
				continue
			}
			for _, c := range children {
				if c == nil || c.ID == "" || visited[c.ID] {
					continue
				}
				visited[c.ID] = true
				// Tenancy boundary: don't return it and don't descend through it.
				if c.ProjectID != projectID {
					continue
				}
				out = append(out, c.ID)
				next = append(next, c.ID)
				if len(out) >= lineageMaxTasks {
					return out
				}
			}
		}
		frontier = next
	}
	return out
}

// artifactInTaskTree reports whether an artifact owned by ownerTaskID may be
// acted on from the page for rootID — true when it IS rootID's own artifact,
// or when its owner is a descendant of rootID within projectID.
//
// This is the authorization predicate behind "Send to chat", and it replaces a
// strict `*artifact.TaskID == taskID` equality. The equality was what made a
// delegated child's deliverable unsendable from the parent page even once it
// was listed there. Relaxing it to the task tree keeps the property that
// mattered: an artifact belonging to an UNRELATED task still cannot be reached
// by guessing its id, because an unrelated task is not in this tree.
//
// projectID is load-bearing, not defence in depth. The old equality implied
// same-project transitively (the artifact belonged to THIS task); the tree does
// not, because parent_task_id spans projects via cross-project calls. Callers
// must pass the ROOT task's project — the one they authorized the requester
// against — so a cross-project callee's artifact is never in scope.
func artifactInTaskTree(ctx context.Context, repo persistence.TaskRepository, rootID, projectID, ownerTaskID string) bool {
	if ownerTaskID == "" || rootID == "" || projectID == "" {
		return false
	}
	if ownerTaskID == rootID {
		return true
	}
	for _, id := range descendantTaskIDs(ctx, repo, rootID, projectID) {
		if id == ownerTaskID {
			return true
		}
	}
	return false
}
