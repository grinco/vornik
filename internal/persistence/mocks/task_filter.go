package mocks

import "vornik.io/vornik/internal/persistence"

// FilterTasks applies the persistence.TaskFilter matching semantics a
// real backend (postgres/sqlite) enforces to an in-memory slice.
// MockTaskRepository.List is a pure script (whatever ListFunc returns),
// so a test's own hand-rolled ListFunc closure is the only thing
// standing in for real WHERE-clause behavior. The Outcome Inbox design
// review flagged the resulting false-pass risk directly (review finding
// 1/9): a ListFunc that only inspects filter.Status silently keeps
// "working" even after the caller switches to filter.Statuses, because
// nothing forces the closure to look at the new field — the contract
// test would pass without ever exercising the Statuses semantics it
// claims to cover.
//
// FilterTasks is the shared, correct reference implementation tests can
// delegate to instead of hand-rolling matching logic. It mirrors
// postgres/sqlite exactly:
//   - ProjectID / ProjectIDs are both AND-applied when set (same as the
//     real repos: ProjectIDs doesn't replace ProjectID).
//   - IDs restricts to the given primary keys when non-empty.
//   - Statuses wins over Status when both are set; an empty non-nil
//     Statuses is "no constraint from this field" (never an empty
//     IN() that matches nothing) — see persistence.TaskFilter.Statuses.
//   - UpdatedBefore is an exclusive upper bound on UpdatedAt.
func FilterTasks(tasks []*persistence.Task, f persistence.TaskFilter) []*persistence.Task {
	out := make([]*persistence.Task, 0, len(tasks))
	for _, t := range tasks {
		if t == nil {
			continue
		}
		if f.ProjectID != nil && t.ProjectID != *f.ProjectID {
			continue
		}
		if len(f.ProjectIDs) > 0 && !containsString(f.ProjectIDs, t.ProjectID) {
			continue
		}
		if len(f.IDs) > 0 && !containsString(f.IDs, t.ID) {
			continue
		}
		if len(f.Statuses) > 0 {
			if !containsStatus(f.Statuses, t.Status) {
				continue
			}
		} else if f.Status != nil && t.Status != *f.Status {
			continue
		}
		if f.UpdatedBefore != nil && !t.UpdatedAt.Before(*f.UpdatedBefore) {
			continue
		}
		out = append(out, t)
	}
	return out
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func containsStatus(haystack []persistence.TaskStatus, needle persistence.TaskStatus) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
