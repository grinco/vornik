package mocks_test

import (
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// TestFilterTasks_StatusesSemantics is the "mock" leg of the three-way
// (postgres/sqlite/mock) Outcome Inbox contract: Statuses-only,
// Status-only unchanged, both-set precedence, empty-slice ignored.
// Mirrors internal/persistence/sqlite/list_statuses_test.go and
// internal/persistence/postgres/task_repository_statuses_test.go exactly
// so a divergence between any of the three surfaces as a test failure.
func TestFilterTasks_StatusesSemantics(t *testing.T) {
	mk := func(id string, st persistence.TaskStatus) *persistence.Task {
		return &persistence.Task{ID: id, ProjectID: "p1", Status: st}
	}
	all := []*persistence.Task{
		mk("approve", persistence.TaskStatusAwaitingApproval),
		mk("input", persistence.TaskStatusAwaitingInput),
		mk("failed", persistence.TaskStatusFailed),
		mk("blocked", persistence.TaskStatusWaitingForChildren),
		mk("external", persistence.TaskStatusAwaitingExternal), // decoy
		mk("done", persistence.TaskStatusCompleted),            // decoy
	}

	t.Run("Statuses_selects_exactly_that_set", func(t *testing.T) {
		got := mocks.FilterTasks(all, persistence.TaskFilter{
			Statuses: []persistence.TaskStatus{
				persistence.TaskStatusAwaitingApproval,
				persistence.TaskStatusAwaitingInput,
				persistence.TaskStatusFailed,
				persistence.TaskStatusWaitingForChildren,
			},
		})
		want := map[string]bool{"approve": true, "input": true, "failed": true, "blocked": true}
		if len(got) != len(want) {
			t.Fatalf("got %d, want %d: %+v", len(got), len(want), got)
		}
		for _, tk := range got {
			if !want[tk.ID] {
				t.Errorf("unexpected %s in Statuses result", tk.ID)
			}
		}
	})

	t.Run("Status_only_is_unchanged", func(t *testing.T) {
		st := persistence.TaskStatusFailed
		got := mocks.FilterTasks(all, persistence.TaskFilter{Status: &st})
		if len(got) != 1 || got[0].ID != "failed" {
			t.Fatalf("Status-only = %+v, want exactly [failed]", got)
		}
	})

	t.Run("both_set_Statuses_wins", func(t *testing.T) {
		legacy := persistence.TaskStatusCompleted
		got := mocks.FilterTasks(all, persistence.TaskFilter{
			Status:   &legacy,
			Statuses: []persistence.TaskStatus{persistence.TaskStatusFailed, persistence.TaskStatusAwaitingInput},
		})
		want := map[string]bool{"failed": true, "input": true}
		if len(got) != len(want) {
			t.Fatalf("got %d, want %d: %+v", len(got), len(want), got)
		}
		for _, tk := range got {
			if !want[tk.ID] {
				t.Errorf("Statuses should have won over Status(COMPLETED): unexpected %s", tk.ID)
			}
		}
	})

	t.Run("empty_non_nil_Statuses_is_ignored", func(t *testing.T) {
		got := mocks.FilterTasks(all, persistence.TaskFilter{Statuses: []persistence.TaskStatus{}})
		if len(got) != len(all) {
			t.Fatalf("empty Statuses should return all %d tasks (ignored, not IN()), got %d", len(all), len(got))
		}
	})

	t.Run("nil_Statuses_and_nil_Status_returns_everything", func(t *testing.T) {
		got := mocks.FilterTasks(all, persistence.TaskFilter{})
		if len(got) != len(all) {
			t.Fatalf("no filter should return all %d, got %d", len(all), len(got))
		}
	})
}

// TestFilterTasks_ProjectAndIDScoping covers the ProjectID/ProjectIDs/IDs
// predicates FilterTasks shares with the real repos.
func TestFilterTasks_ProjectAndIDScoping(t *testing.T) {
	tasks := []*persistence.Task{
		{ID: "a", ProjectID: "p1"},
		{ID: "b", ProjectID: "p2"},
		{ID: "c", ProjectID: "p3"},
	}

	t.Run("ProjectIDs_IN_set", func(t *testing.T) {
		got := mocks.FilterTasks(tasks, persistence.TaskFilter{ProjectIDs: []string{"p1", "p3"}})
		if len(got) != 2 {
			t.Fatalf("got %d, want 2: %+v", len(got), got)
		}
	})

	t.Run("IDs_restricts_to_primary_keys", func(t *testing.T) {
		got := mocks.FilterTasks(tasks, persistence.TaskFilter{IDs: []string{"b"}})
		if len(got) != 1 || got[0].ID != "b" {
			t.Fatalf("IDs filter = %+v, want exactly [b]", got)
		}
	})

	t.Run("ProjectID_and_ProjectIDs_both_apply", func(t *testing.T) {
		got := mocks.FilterTasks(tasks, persistence.TaskFilter{
			ProjectIDs: []string{"p1", "p2"},
		})
		if len(got) != 2 {
			t.Fatalf("got %d, want 2: %+v", len(got), got)
		}
	})
}

// TestFilterTasks_UpdatedBefore covers the exclusive-upper-bound
// predicate used by the FAILED 24h attention-queue window.
func TestFilterTasks_UpdatedBefore(t *testing.T) {
	now := time.Now()
	tasks := []*persistence.Task{
		{ID: "fresh", UpdatedAt: now},
		{ID: "stale", UpdatedAt: now.Add(-48 * time.Hour)},
	}
	cutoff := now.Add(-24 * time.Hour)
	got := mocks.FilterTasks(tasks, persistence.TaskFilter{UpdatedBefore: &cutoff})
	if len(got) != 1 || got[0].ID != "stale" {
		t.Fatalf("UpdatedBefore = %+v, want exactly [stale]", got)
	}
}

// TestFilterTasks_NilTaskSkipped defends against a nil entry in the
// seed slice (defensive parity with real repos, which never return nil
// rows) panicking the helper.
func TestFilterTasks_NilTaskSkipped(t *testing.T) {
	tasks := []*persistence.Task{nil, {ID: "a", ProjectID: "p1"}}
	got := mocks.FilterTasks(tasks, persistence.TaskFilter{})
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("got %+v, want exactly [a]", got)
	}
}

// UpdatedSince is INCLUSIVE, matching the repos' `updated_at >= ?`. The
// dashboard counts failures with exactly that predicate, so an exclusive
// bound here would make the list drop a row the card counted — the same
// count/list disagreement the filter was added to fix.
func TestFilterTasks_UpdatedSince(t *testing.T) {
	cutoff := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	tasks := []*persistence.Task{
		{ID: "stale", UpdatedAt: cutoff.Add(-time.Second)},
		{ID: "exactly-at-cutoff", UpdatedAt: cutoff},
		{ID: "fresh", UpdatedAt: cutoff.Add(time.Second)},
	}

	got := mocks.FilterTasks(tasks, persistence.TaskFilter{UpdatedSince: &cutoff})

	if len(got) != 2 || got[0].ID != "exactly-at-cutoff" || got[1].ID != "fresh" {
		ids := make([]string, len(got))
		for i, g := range got {
			ids[i] = g.ID
		}
		t.Fatalf("UpdatedSince = %v, want [exactly-at-cutoff fresh]", ids)
	}
}

// Both bounds together describe a window, which is what a "between these
// times" view needs.
func TestFilterTasks_UpdatedSinceAndBeforeBoundAWindow(t *testing.T) {
	lo := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	hi := time.Date(2026, 8, 14, 14, 0, 0, 0, time.UTC)
	tasks := []*persistence.Task{
		{ID: "before", UpdatedAt: lo.Add(-time.Hour)},
		{ID: "inside", UpdatedAt: lo.Add(time.Hour)},
		{ID: "after", UpdatedAt: hi.Add(time.Hour)},
	}

	got := mocks.FilterTasks(tasks, persistence.TaskFilter{UpdatedSince: &lo, UpdatedBefore: &hi})

	if len(got) != 1 || got[0].ID != "inside" {
		t.Fatalf("window filter = %+v, want exactly [inside]", got)
	}
}

// A task that failed before failed_at existed has NULL, and must read as
// not-recent. Back-filling it from updated_at is precisely the bug.
func TestFilterTasks_FailedSinceTreatsUnknownAsNotRecent(t *testing.T) {
	cutoff := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	fresh := cutoff.Add(time.Minute)
	stale := cutoff.Add(-90 * 24 * time.Hour)
	tasks := []*persistence.Task{
		{ID: "unknown-when", UpdatedAt: fresh}, // touched recently, FailedAt nil
		{ID: "failed-long-ago", UpdatedAt: fresh, FailedAt: &stale},
		{ID: "failed-just-now", UpdatedAt: fresh, FailedAt: &fresh},
	}

	got := mocks.FilterTasks(tasks, persistence.TaskFilter{FailedSince: &cutoff})

	if len(got) != 1 || got[0].ID != "failed-just-now" {
		ids := make([]string, len(got))
		for i, g := range got {
			ids[i] = g.ID
		}
		t.Fatalf("FailedSince = %v, want only [failed-just-now]", ids)
	}
}
