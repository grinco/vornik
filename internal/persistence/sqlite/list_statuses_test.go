package sqlite_test

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// TestTaskList_ByStatuses — the outcome-inbox attention-query widening
// (https://docs.vornik.io §5.2): TaskFilter.Statuses
// selects a status IN (...) set in one round-trip, mirroring ProjectIDs.
// Covers the four documented semantics: Statuses-only, Status-only
// (unchanged legacy behavior), both-set precedence, and empty-slice-ignored.
func TestTaskList_ByStatuses(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := sqlite.NewTaskRepository(db.DB)

	project := "proj-statuses"
	statuses := []persistence.TaskStatus{
		persistence.TaskStatusAwaitingApproval,
		persistence.TaskStatusAwaitingInput,
		persistence.TaskStatusFailed,
		persistence.TaskStatusWaitingForChildren,
		persistence.TaskStatusAwaitingExternal, // decoy — must never surface via Statuses below
		persistence.TaskStatusCompleted,        // decoy
	}
	ids := make(map[persistence.TaskStatus]string, len(statuses))
	for i, st := range statuses {
		id := "t-" + string(st)
		ids[st] = id
		if err := repo.Create(ctx, &persistence.Task{
			ID:        id,
			ProjectID: project,
			Status:    persistence.TaskStatusQueued, // Create defaults; flip via UpdateStatus below
			CreatedAt: time.Now().UTC().Add(time.Duration(i) * time.Millisecond),
		}); err != nil {
			t.Fatalf("Create %s: %v", id, err)
		}
		if err := repo.UpdateStatus(ctx, id, st); err != nil {
			t.Fatalf("UpdateStatus %s: %v", id, err)
		}
	}

	t.Run("Statuses_selects_exactly_that_set", func(t *testing.T) {
		got, err := repo.List(ctx, persistence.TaskFilter{
			ProjectID: &project,
			Statuses: []persistence.TaskStatus{
				persistence.TaskStatusAwaitingApproval,
				persistence.TaskStatusAwaitingInput,
				persistence.TaskStatusFailed,
				persistence.TaskStatusWaitingForChildren,
			},
		})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		want := map[string]bool{
			ids[persistence.TaskStatusAwaitingApproval]:   true,
			ids[persistence.TaskStatusAwaitingInput]:      true,
			ids[persistence.TaskStatusFailed]:             true,
			ids[persistence.TaskStatusWaitingForChildren]: true,
		}
		if len(got) != len(want) {
			t.Fatalf("got %d tasks, want %d: %+v", len(got), len(want), got)
		}
		for _, tk := range got {
			if !want[tk.ID] {
				t.Errorf("unexpected task %s (status %s) in Statuses result — AWAITING_EXTERNAL/COMPLETED must be excluded", tk.ID, tk.Status)
			}
		}
	})

	t.Run("Status_only_is_unchanged", func(t *testing.T) {
		st := persistence.TaskStatusFailed
		got, err := repo.List(ctx, persistence.TaskFilter{ProjectID: &project, Status: &st})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 1 || got[0].ID != ids[persistence.TaskStatusFailed] {
			t.Fatalf("Status-only filter = %+v, want exactly [%s]", got, ids[persistence.TaskStatusFailed])
		}
	})

	t.Run("both_set_Statuses_wins", func(t *testing.T) {
		legacy := persistence.TaskStatusCompleted // would select the decoy alone if honored
		got, err := repo.List(ctx, persistence.TaskFilter{
			ProjectID: &project,
			Status:    &legacy,
			Statuses:  []persistence.TaskStatus{persistence.TaskStatusFailed, persistence.TaskStatusAwaitingInput},
		})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		want := map[string]bool{
			ids[persistence.TaskStatusFailed]:        true,
			ids[persistence.TaskStatusAwaitingInput]: true,
		}
		if len(got) != len(want) {
			t.Fatalf("both-set precedence: got %d, want %d: %+v", len(got), len(want), got)
		}
		for _, tk := range got {
			if !want[tk.ID] {
				t.Errorf("Statuses should have won over Status(COMPLETED): unexpected %s (%s)", tk.ID, tk.Status)
			}
		}
	})

	t.Run("empty_non_nil_Statuses_is_ignored_not_IN_empty", func(t *testing.T) {
		got, err := repo.List(ctx, persistence.TaskFilter{
			ProjectID: &project,
			Statuses:  []persistence.TaskStatus{},
		})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != len(statuses) {
			t.Fatalf("empty Statuses should return all %d project tasks (ignored, not IN()), got %d", len(statuses), len(got))
		}
	})

	t.Run("Count_honors_Statuses", func(t *testing.T) {
		n, err := repo.Count(ctx, persistence.TaskFilter{
			ProjectID: &project,
			Statuses:  []persistence.TaskStatus{persistence.TaskStatusFailed, persistence.TaskStatusAwaitingApproval},
		})
		if err != nil {
			t.Fatalf("Count: %v", err)
		}
		if n != 2 {
			t.Fatalf("Count with Statuses = %d, want 2", n)
		}
	})
}

// TestTaskList_ByIDs covers the batch by-ID filter added for
// persistence.ResolveRequestRoots's ancestor-walk (Outcome Inbox design
// §5.3): a single List(TaskFilter{IDs: ...}) resolves many task rows by
// primary key in one round-trip, instead of one Get per id.
func TestTaskList_ByIDs(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := sqlite.NewTaskRepository(db.DB)

	project := "proj-ids"
	var created []string
	for i, suffix := range []string{"a", "b", "c"} {
		id := "tid-" + suffix
		created = append(created, id)
		if err := repo.Create(ctx, &persistence.Task{
			ID:        id,
			ProjectID: project,
			Status:    persistence.TaskStatusQueued,
			CreatedAt: time.Now().UTC().Add(time.Duration(i) * time.Millisecond),
		}); err != nil {
			t.Fatalf("Create %s: %v", id, err)
		}
	}
	// Decoy row that must not be pulled in.
	if err := repo.Create(ctx, &persistence.Task{ID: "tid-decoy", ProjectID: project, Status: persistence.TaskStatusQueued}); err != nil {
		t.Fatalf("Create decoy: %v", err)
	}

	got, err := repo.List(ctx, persistence.TaskFilter{IDs: []string{created[0], created[2]}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List(IDs) = %d rows, want 2: %+v", len(got), got)
	}
	seen := map[string]bool{}
	for _, tk := range got {
		seen[tk.ID] = true
	}
	if !seen[created[0]] || !seen[created[2]] {
		t.Fatalf("List(IDs) missing expected ids: %+v", got)
	}
	if seen["tid-decoy"] || seen[created[1]] {
		t.Fatalf("List(IDs) pulled in an id outside the requested set: %+v", got)
	}
}
