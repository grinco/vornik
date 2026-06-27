// Package repotest provides backend-agnostic test cases for the
// persistence-package repository interfaces. Each Run* function
// takes a repo handle of the relevant interface type and exercises
// the protocol contract — round-trip writes/reads, idempotency,
// filter behaviour, and edge cases that should hold on every
// backend.
//
// The Postgres-side tests at internal/persistence/postgres delegate
// to these functions through small wiring tests; the SQLite-side
// tests at internal/persistence/sqlite do the same. Both backends
// thus prove they implement the SAME contract — a behaviour
// divergence surfaces as a test failure on whichever side drifted.
package repotest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RunTaskWatcherSuite exercises the persistence.TaskWatcherRepository
// contract end to end. The repo passed in must connect to an empty
// task_watchers table — the suite creates and removes rows as it
// runs.
func RunTaskWatcherSuite(t *testing.T, repo persistence.TaskWatcherRepository) {
	t.Helper()
	ctx := context.Background()

	t.Run("Watch_then_GetWatchers_returns_inserted_chat", func(t *testing.T) {
		taskID := uniqueID("task")
		if err := repo.Watch(ctx, taskID, 12345); err != nil {
			t.Fatalf("Watch: %v", err)
		}
		ids, err := repo.GetWatchers(ctx, taskID)
		if err != nil {
			t.Fatalf("GetWatchers: %v", err)
		}
		if len(ids) != 1 || ids[0] != 12345 {
			t.Fatalf("expected [12345], got %v", ids)
		}
	})

	t.Run("Watch_is_idempotent_on_duplicate_pair", func(t *testing.T) {
		taskID := uniqueID("task")
		if err := repo.Watch(ctx, taskID, 7); err != nil {
			t.Fatalf("Watch 1: %v", err)
		}
		if err := repo.Watch(ctx, taskID, 7); err != nil {
			t.Fatalf("Watch 2 (duplicate): %v — must be a no-op", err)
		}
		ids, err := repo.GetWatchers(ctx, taskID)
		if err != nil {
			t.Fatalf("GetWatchers: %v", err)
		}
		if len(ids) != 1 {
			t.Fatalf("duplicate Watch produced %d rows, want 1: %v", len(ids), ids)
		}
	})

	t.Run("GetWatchers_returns_empty_for_unknown_task", func(t *testing.T) {
		ids, err := repo.GetWatchers(ctx, uniqueID("nope"))
		if err != nil {
			t.Fatalf("GetWatchers: %v", err)
		}
		if len(ids) != 0 {
			t.Fatalf("expected empty slice for unknown task, got %v", ids)
		}
	})

	t.Run("RemoveWatchers_clears_every_row_for_task", func(t *testing.T) {
		taskID := uniqueID("task")
		for _, chat := range []int64{1, 2, 3} {
			if err := repo.Watch(ctx, taskID, chat); err != nil {
				t.Fatalf("Watch %d: %v", chat, err)
			}
		}
		if err := repo.RemoveWatchers(ctx, taskID); err != nil {
			t.Fatalf("RemoveWatchers: %v", err)
		}
		ids, err := repo.GetWatchers(ctx, taskID)
		if err != nil {
			t.Fatalf("GetWatchers: %v", err)
		}
		if len(ids) != 0 {
			t.Fatalf("RemoveWatchers left %d rows, want 0: %v", len(ids), ids)
		}
	})

	t.Run("RemoveWatchers_is_safe_when_no_rows_exist", func(t *testing.T) {
		if err := repo.RemoveWatchers(ctx, uniqueID("ghost")); err != nil {
			t.Fatalf("RemoveWatchers on absent task should succeed, got: %v", err)
		}
	})
}

// RunToolAuditSuite exercises the persistence.ToolAuditRepository
// contract.
func RunToolAuditSuite(t *testing.T, repo persistence.ToolAuditRepository) {
	t.Helper()
	ctx := context.Background()

	t.Run("Log_then_List_round_trips_entry", func(t *testing.T) {
		entry := &persistence.ToolAuditEntry{
			ID:          uniqueID("aud"),
			ProjectID:   "proj-a",
			TaskID:      "task-a",
			ExecutionID: "exec-a",
			StepID:      "step-a",
			ToolName:    "shell",
			ToolInput:   `{"cmd":"ls"}`,
			ToolOutput:  "ok",
			DurationMs:  42,
			CreatedAt:   time.Now().UTC().Truncate(time.Millisecond),
		}
		if err := repo.Log(ctx, entry); err != nil {
			t.Fatalf("Log: %v", err)
		}
		project := "proj-a"
		got, err := repo.List(ctx, persistence.ToolAuditFilter{ProjectID: &project})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) == 0 {
			t.Fatalf("expected at least one row")
		}
		var found bool
		for _, e := range got {
			if e.ID == entry.ID {
				found = true
				if e.ToolName != "shell" || e.DurationMs != 42 {
					t.Errorf("round-trip mismatch: %+v", e)
				}
			}
		}
		if !found {
			t.Fatalf("inserted row %s not present in List output", entry.ID)
		}
	})

	t.Run("Log_is_idempotent_on_duplicate_id", func(t *testing.T) {
		entry := &persistence.ToolAuditEntry{
			ID:          uniqueID("aud"),
			ProjectID:   "proj-b",
			TaskID:      "task-b",
			ExecutionID: "exec-b",
			ToolName:    "fetch",
			CreatedAt:   time.Now().UTC(),
		}
		if err := repo.Log(ctx, entry); err != nil {
			t.Fatalf("Log 1: %v", err)
		}
		if err := repo.Log(ctx, entry); err != nil {
			t.Fatalf("Log 2 (duplicate id): %v — must be a no-op", err)
		}
		project := "proj-b"
		got, _ := repo.List(ctx, persistence.ToolAuditFilter{ProjectID: &project})
		count := 0
		for _, e := range got {
			if e.ID == entry.ID {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("duplicate Log produced %d rows, want 1", count)
		}
	})

	t.Run("CountByTool_groups_by_tool_name", func(t *testing.T) {
		execID := uniqueID("exec")
		for i, name := range []string{"shell", "shell", "fetch"} {
			if err := repo.Log(ctx, &persistence.ToolAuditEntry{
				ID:          uniqueID("aud"),
				ProjectID:   "proj-c",
				TaskID:      "task-c",
				ExecutionID: execID,
				ToolName:    name,
				DurationMs:  int64(i),
				CreatedAt:   time.Now().UTC(),
			}); err != nil {
				t.Fatalf("Log: %v", err)
			}
		}
		counts, err := repo.CountByTool(ctx, execID)
		if err != nil {
			t.Fatalf("CountByTool: %v", err)
		}
		if counts["shell"] != 2 || counts["fetch"] != 1 {
			t.Fatalf("unexpected counts: %+v", counts)
		}
	})

	t.Run("List_filter_by_tool_name_isolates_rows", func(t *testing.T) {
		project := uniqueID("proj")
		taskID := uniqueID("task")
		_ = repo.Log(ctx, &persistence.ToolAuditEntry{
			ID: uniqueID("aud"), ProjectID: project, TaskID: taskID,
			ExecutionID: "e", ToolName: "fetch", CreatedAt: time.Now().UTC(),
		})
		_ = repo.Log(ctx, &persistence.ToolAuditEntry{
			ID: uniqueID("aud"), ProjectID: project, TaskID: taskID,
			ExecutionID: "e", ToolName: "shell", CreatedAt: time.Now().UTC(),
		})
		tool := "fetch"
		got, err := repo.List(ctx, persistence.ToolAuditFilter{
			ProjectID: &project, ToolName: &tool,
		})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 1 || got[0].ToolName != "fetch" {
			t.Fatalf("filter mishit: %+v", got)
		}
	})
}

// RunRecoveryEventSuite exercises persistence.RecoveryEventRepository against
// any backend (recovery-events-design.md).
func RunRecoveryEventSuite(t *testing.T, repo persistence.RecoveryEventRepository) {
	t.Helper()
	ctx := context.Background()

	t.Run("Record_then_ListRecent_round_trips", func(t *testing.T) {
		ev := &persistence.RecoveryEvent{
			ID: "rcv-1", ProjectID: "p1", TaskID: "t1", ExecutionID: "e1",
			WorkflowID: "dev-pipeline", TerminalID: "checkpoint",
			CreatedAt: time.Now().UTC(),
		}
		if err := repo.Record(ctx, ev); err != nil {
			t.Fatalf("Record: %v", err)
		}
		got, err := repo.ListRecent(ctx, "p1", 10)
		if err != nil {
			t.Fatalf("ListRecent: %v", err)
		}
		if len(got) != 1 || got[0].ID != "rcv-1" || got[0].TerminalID != "checkpoint" {
			t.Fatalf("round-trip mismatch: %+v", got)
		}
	})

	t.Run("Record_is_idempotent_on_duplicate_id", func(t *testing.T) {
		ev := &persistence.RecoveryEvent{ID: "rcv-dup", ProjectID: "p2", TaskID: "t2", ExecutionID: "e2", CreatedAt: time.Now().UTC()}
		if err := repo.Record(ctx, ev); err != nil {
			t.Fatalf("Record 1: %v", err)
		}
		if err := repo.Record(ctx, ev); err != nil {
			t.Fatalf("Record 2 (idempotent): %v", err)
		}
		got, _ := repo.ListRecent(ctx, "p2", 10)
		if len(got) != 1 {
			t.Fatalf("duplicate id should not double-insert: %+v", got)
		}
	})

	t.Run("ListRecent_unknown_project_empty", func(t *testing.T) {
		got, err := repo.ListRecent(ctx, "no-such", 10)
		if err != nil {
			t.Fatalf("ListRecent: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("expected empty, got %+v", got)
		}
	})
}

// RunArtifactSuite exercises the persistence.ArtifactRepository
// contract.
func RunArtifactSuite(t *testing.T, repo persistence.ArtifactRepository) {
	t.Helper()
	ctx := context.Background()

	t.Run("Create_then_Get_round_trips_artifact", func(t *testing.T) {
		hash := uniqueID("hash")
		size := int64(1024)
		mime := "text/plain"
		a := &persistence.Artifact{
			ID:                uniqueID("art"),
			ProjectID:         "proj-1",
			Name:              "result.txt",
			ArtifactClass:     persistence.ArtifactClassOutput,
			StoragePath:       "tasks/x/result.txt",
			SizeBytes:         &size,
			ContentHashSHA256: &hash,
			MimeType:          &mime,
			CreatedAt:         time.Now().UTC().Truncate(time.Millisecond),
			Origin:            persistence.ArtifactOriginTaskOutput,
		}
		if err := repo.Create(ctx, a); err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, err := repo.Get(ctx, a.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.ID != a.ID || got.Name != a.Name || got.StoragePath != a.StoragePath {
			t.Errorf("round-trip mismatch: %+v vs %+v", got, a)
		}
		if got.SizeBytes == nil || *got.SizeBytes != size {
			t.Errorf("size mismatch: %v", got.SizeBytes)
		}
		if got.Origin != a.Origin {
			t.Errorf("origin round-trip mismatch: got %q, want %q", got.Origin, a.Origin)
		}
	})

	t.Run("Origin_defaults_to_unknown_when_unset", func(t *testing.T) {
		// Insert an artifact with Origin unset; the repo must write "unknown"
		// (via the default-fill in Create) and return it on Get.
		a := &persistence.Artifact{
			ID:            uniqueID("art"),
			ProjectID:     "proj-1",
			Name:          "legacy.txt",
			ArtifactClass: persistence.ArtifactClassOutput,
			StoragePath:   "tasks/x/legacy.txt",
			CreatedAt:     time.Now().UTC(),
			// Origin intentionally left as zero value.
		}
		if err := repo.Create(ctx, a); err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, err := repo.Get(ctx, a.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Origin != persistence.ArtifactOriginUnknown {
			t.Errorf("Origin = %q, want %q (default fill)", got.Origin, persistence.ArtifactOriginUnknown)
		}
	})

	t.Run("Get_unknown_id_returns_ErrNotFound", func(t *testing.T) {
		_, err := repo.Get(ctx, uniqueID("missing"))
		if err == nil {
			t.Fatal("expected an error for missing id")
		}
		if err.Error() != persistence.ErrNotFound.Error() {
			t.Fatalf("expected ErrNotFound, got %v", err)
		}
	})

	t.Run("GetByHash_resolves_dedup_lookup", func(t *testing.T) {
		hash := uniqueID("h")
		a := &persistence.Artifact{
			ID:                uniqueID("art"),
			ProjectID:         "proj-1",
			Name:              "x.txt",
			ArtifactClass:     persistence.ArtifactClassInput,
			StoragePath:       "p",
			ContentHashSHA256: &hash,
			CreatedAt:         time.Now().UTC(),
		}
		if err := repo.Create(ctx, a); err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, err := repo.GetByHash(ctx, hash)
		if err != nil {
			t.Fatalf("GetByHash: %v", err)
		}
		if got.ID != a.ID {
			t.Errorf("GetByHash returned wrong row: %s", got.ID)
		}
	})

	t.Run("List_filter_by_project_isolates_rows", func(t *testing.T) {
		project := uniqueID("proj")
		for i := 0; i < 3; i++ {
			_ = repo.Create(ctx, &persistence.Artifact{
				ID:            uniqueID("art"),
				ProjectID:     project,
				Name:          "n",
				ArtifactClass: persistence.ArtifactClassOutput,
				StoragePath:   "p",
				CreatedAt:     time.Now().UTC().Add(time.Duration(i) * time.Millisecond),
			})
		}
		// One artifact in a different project — must not appear.
		_ = repo.Create(ctx, &persistence.Artifact{
			ID:            uniqueID("art"),
			ProjectID:     "other-project",
			Name:          "n",
			ArtifactClass: persistence.ArtifactClassOutput,
			StoragePath:   "p",
			CreatedAt:     time.Now().UTC(),
		})
		got, err := repo.List(ctx, persistence.ArtifactFilter{ProjectID: &project})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("project filter returned %d rows, want 3", len(got))
		}
	})

	// NOTE: UpdateTaskID isn't covered by the shared suite — its
	// happy path needs a real tasks row to satisfy the FK on
	// artifacts.task_id (Postgres enforces; SQLite has FK off in
	// phase 2). Per-backend tests cover the validation branches
	// (empty IDs) directly. Once a shared seed-helper for tasks
	// exists alongside TaskRepository, the happy path can rejoin
	// this suite.

	t.Run("Delete_removes_artifact", func(t *testing.T) {
		a := &persistence.Artifact{
			ID:            uniqueID("art"),
			ProjectID:     "p",
			Name:          "n",
			ArtifactClass: persistence.ArtifactClassLog,
			StoragePath:   "p",
			CreatedAt:     time.Now().UTC(),
		}
		_ = repo.Create(ctx, a)
		if err := repo.Delete(ctx, a.ID); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		_, err := repo.Get(ctx, a.ID)
		if err == nil {
			t.Fatal("expected error after Delete")
		}
	})
}

// uniqueID returns a per-call unique string for safe test IDs.
// Combines a monotonic counter with the wall clock so two calls in
// the same nanosecond (possible under parallel sub-tests) still
// produce distinct IDs.
func uniqueID(prefix string) string {
	n := atomic.AddUint64(&uniqueCounter, 1)
	return fmt.Sprintf("%s-%s-%d", prefix, time.Now().UTC().Format("150405.000000000"), n)
}

var uniqueCounter uint64

// jsonEqual returns true when two byte slices encode the same JSON
// document — ignoring whitespace and key-order differences. Used by
// suites that round-trip through a backend whose JSONB storage
// normalises (postgres) vs preserves verbatim (sqlite).
//
// Returns true for nil/empty inputs on both sides so callers don't
// need a separate "either both empty" branch.
func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

// RunTaskRepositorySuite exercises the persistence.TaskRepository
// contract: CRUD, status transitions, lease lifecycle, and the
// queries that drive the scheduler's lease pickup. The repo passed
// in must connect to an empty tasks table — cases create + delete
// their own rows via test-unique IDs.
//
// Concurrency assertions are correctness-only: "N leases over M
// tasks yield exactly min(N,M) unique IDs". The wall-clock shape
// of concurrent leasing differs by backend (Postgres parallel via
// SKIP LOCKED; SQLite serial via BEGIN IMMEDIATE) — pinning a
// timing assertion in the shared suite would force divergence, so
// timing-aware tests stay per-backend.
func RunTaskRepositorySuite(t *testing.T, repo persistence.TaskRepository) {
	t.Helper()
	ctx := context.Background()

	t.Run("Create_then_Get_round_trips_task", func(t *testing.T) {
		task := newQueuedTask(uniqueID("proj"))
		if err := repo.Create(ctx, task); err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, err := repo.Get(ctx, task.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.ID != task.ID || got.ProjectID != task.ProjectID {
			t.Errorf("round-trip mismatch: %+v vs %+v", got, task)
		}
		if got.Status != persistence.TaskStatusQueued {
			t.Errorf("expected status QUEUED, got %s", got.Status)
		}
	})

	t.Run("Get_unknown_id_returns_ErrNotFound", func(t *testing.T) {
		_, err := repo.Get(ctx, uniqueID("missing"))
		if err == nil {
			t.Fatal("expected ErrNotFound")
		}
		if err.Error() != persistence.ErrNotFound.Error() {
			t.Fatalf("expected ErrNotFound, got %v", err)
		}
	})

	t.Run("GetByIdempotencyKey_resolves_existing_task", func(t *testing.T) {
		project := uniqueID("proj")
		idem := uniqueID("idem")
		task := newQueuedTask(project)
		task.IdempotencyKey = &idem
		if err := repo.Create(ctx, task); err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, err := repo.GetByIdempotencyKey(ctx, project, idem)
		if err != nil {
			t.Fatalf("GetByIdempotencyKey: %v", err)
		}
		if got.ID != task.ID {
			t.Errorf("wrong task returned: %s", got.ID)
		}
	})

	t.Run("List_and_Count_filter_by_project", func(t *testing.T) {
		project := uniqueID("proj")
		for i := 0; i < 3; i++ {
			_ = repo.Create(ctx, newQueuedTask(project))
		}
		// Decoy task in a different project — must not appear.
		_ = repo.Create(ctx, newQueuedTask(uniqueID("other")))

		list, err := repo.List(ctx, persistence.TaskFilter{ProjectID: &project})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(list) != 3 {
			t.Fatalf("List returned %d tasks, want 3", len(list))
		}
		n, err := repo.Count(ctx, persistence.TaskFilter{ProjectID: &project})
		if err != nil {
			t.Fatalf("Count: %v", err)
		}
		if n != 3 {
			t.Errorf("Count returned %d, want 3", n)
		}
	})

	t.Run("UpdateStatus_persists_and_TransitionToCancelled_gates", func(t *testing.T) {
		task := newQueuedTask(uniqueID("proj"))
		_ = repo.Create(ctx, task)

		if err := repo.UpdateStatus(ctx, task.ID, persistence.TaskStatusRunning); err != nil {
			t.Fatalf("UpdateStatus: %v", err)
		}

		// Cancelling a RUNNING task should succeed.
		ok, err := repo.TransitionToCancelled(ctx, task.ID)
		if err != nil {
			t.Fatalf("TransitionToCancelled: %v", err)
		}
		if !ok {
			t.Fatal("TransitionToCancelled returned false on a RUNNING task")
		}
		// A second cancel attempt should be a no-op (already CANCELLED
		// is not in the eligible-from set).
		ok, err = repo.TransitionToCancelled(ctx, task.ID)
		if err != nil {
			t.Fatalf("TransitionToCancelled #2: %v", err)
		}
		if ok {
			t.Fatal("TransitionToCancelled on an already-CANCELLED task should return false")
		}
	})

	t.Run("Lease_then_Renew_then_Release_round_trip", func(t *testing.T) {
		project := uniqueID("proj")
		_ = repo.Create(ctx, newQueuedTask(project))

		leased, err := repo.LeaseTask(ctx, persistence.LeaseOptions{
			ProjectID:            project,
			LeaseHolder:          "test-holder",
			LeaseDurationSeconds: 300,
		})
		if err != nil {
			t.Fatalf("LeaseTask: %v", err)
		}
		if leased.Status != persistence.TaskStatusLeased {
			t.Errorf("expected LEASED, got %s", leased.Status)
		}
		if leased.LeaseID == nil || *leased.LeaseID == "" {
			t.Fatal("LeaseTask returned a task without a LeaseID")
		}
		// Second Lease against an empty queue (only one task created)
		// must return ErrNoTasksAvailable.
		_, err = repo.LeaseTask(ctx, persistence.LeaseOptions{
			ProjectID:            project,
			LeaseHolder:          "test-holder",
			LeaseDurationSeconds: 300,
		})
		if err == nil || err.Error() != persistence.ErrNoTasksAvailable.Error() {
			t.Fatalf("expected ErrNoTasksAvailable on second lease, got %v", err)
		}

		// Renew the existing lease — succeeds.
		if err := repo.RenewLease(ctx, leased.ID, *leased.LeaseID, 600); err != nil {
			t.Fatalf("RenewLease: %v", err)
		}

		// Release back to QUEUED.
		if err := repo.ReleaseLease(ctx, leased.ID, *leased.LeaseID,
			persistence.TaskStatusQueued, persistence.ReleaseOptions{}); err != nil {
			t.Fatalf("ReleaseLease: %v", err)
		}

		got, err := repo.Get(ctx, leased.ID)
		if err != nil {
			t.Fatalf("Get after Release: %v", err)
		}
		if got.Status != persistence.TaskStatusQueued {
			t.Errorf("expected QUEUED after Release, got %s", got.Status)
		}
		if got.LeaseID != nil {
			t.Errorf("LeaseID should be nil after Release, got %v", *got.LeaseID)
		}
	})

	t.Run("RenewLease_rejects_stale_lease_id", func(t *testing.T) {
		project := uniqueID("proj")
		_ = repo.Create(ctx, newQueuedTask(project))
		leased, err := repo.LeaseTask(ctx, persistence.LeaseOptions{
			ProjectID:            project,
			LeaseHolder:          "test",
			LeaseDurationSeconds: 300,
		})
		if err != nil {
			t.Fatalf("LeaseTask: %v", err)
		}
		// Wrong lease ID must surface ErrLeaseNotFound.
		err = repo.RenewLease(ctx, leased.ID, "lease-not-real", 600)
		if err == nil || err.Error() != persistence.ErrLeaseNotFound.Error() {
			t.Fatalf("expected ErrLeaseNotFound, got %v", err)
		}
	})

	t.Run("LeaseTask_respects_dependencies", func(t *testing.T) {
		project := uniqueID("proj")
		parent := newQueuedTask(project)
		_ = repo.Create(ctx, parent)

		child := newQueuedTask(project)
		child.Dependencies = []string{parent.ID}
		_ = repo.Create(ctx, child)

		// Parent leases first (it has no deps).
		first, err := repo.LeaseTask(ctx, persistence.LeaseOptions{
			ProjectID:            project,
			LeaseHolder:          "test",
			LeaseDurationSeconds: 300,
		})
		if err != nil {
			t.Fatalf("Lease parent: %v", err)
		}
		if first.ID != parent.ID {
			t.Errorf("expected parent leased first, got %s", first.ID)
		}
		// Child is gated — until parent COMPLETED, second lease
		// returns ErrNoTasksAvailable.
		_, err = repo.LeaseTask(ctx, persistence.LeaseOptions{
			ProjectID:            project,
			LeaseHolder:          "test",
			LeaseDurationSeconds: 300,
		})
		if err == nil || err.Error() != persistence.ErrNoTasksAvailable.Error() {
			t.Fatalf("expected ErrNoTasksAvailable while parent runs, got %v", err)
		}

		// Mark parent COMPLETED — child becomes leasable.
		if err := repo.UpdateStatus(ctx, parent.ID, persistence.TaskStatusCompleted); err != nil {
			t.Fatalf("UpdateStatus parent: %v", err)
		}
		second, err := repo.LeaseTask(ctx, persistence.LeaseOptions{
			ProjectID:            project,
			LeaseHolder:          "test",
			LeaseDurationSeconds: 300,
		})
		if err != nil {
			t.Fatalf("Lease child: %v", err)
		}
		if second.ID != child.ID {
			t.Errorf("expected child after parent completed, got %s", second.ID)
		}
	})

	t.Run("LeaseTask_skips_blocked_rows_without_starving_later_ready_task", func(t *testing.T) {
		project := uniqueID("proj")
		for i := 0; i < 70; i++ {
			blocked := newQueuedTask(project)
			blocked.Dependencies = []string{uniqueID("missing_dep")}
			if err := repo.Create(ctx, blocked); err != nil {
				t.Fatalf("Create blocked task %d: %v", i, err)
			}
		}
		ready := newQueuedTask(project)
		if err := repo.Create(ctx, ready); err != nil {
			t.Fatalf("Create ready task: %v", err)
		}

		leased, err := repo.LeaseTask(ctx, persistence.LeaseOptions{
			ProjectID:            project,
			LeaseHolder:          "test",
			LeaseDurationSeconds: 300,
		})
		if err != nil {
			t.Fatalf("LeaseTask should find ready task behind blocked queue head: %v", err)
		}
		if leased.ID != ready.ID {
			t.Fatalf("leased %s, want later ready task %s", leased.ID, ready.ID)
		}
	})

	t.Run("RequeueTerminalTask_resets_FAILED_to_QUEUED", func(t *testing.T) {
		project := uniqueID("proj")
		task := newQueuedTask(project)
		_ = repo.Create(ctx, task)
		_ = repo.UpdateStatus(ctx, task.ID, persistence.TaskStatusFailed)

		ok, err := repo.RequeueTerminalTask(ctx, task.ID, 2, 5)
		if err != nil {
			t.Fatalf("RequeueTerminalTask: %v", err)
		}
		if !ok {
			t.Fatal("RequeueTerminalTask returned false on a FAILED task")
		}
		got, _ := repo.Get(ctx, task.ID)
		if got.Status != persistence.TaskStatusQueued {
			t.Errorf("expected QUEUED, got %s", got.Status)
		}
		if got.Attempt != 2 || got.MaxAttempts != 5 {
			t.Errorf("expected attempt=2 max=5, got %d/%d", got.Attempt, got.MaxAttempts)
		}
	})

	t.Run("FindExpiredLeases_surfaces_only_expired", func(t *testing.T) {
		project := uniqueID("proj")
		fresh := newQueuedTask(project)
		_ = repo.Create(ctx, fresh)
		leasedFresh, _ := repo.LeaseTask(ctx, persistence.LeaseOptions{
			ProjectID: project, LeaseHolder: "h", LeaseDurationSeconds: 3600,
		})

		stale := newQueuedTask(project)
		_ = repo.Create(ctx, stale)
		leasedStale, _ := repo.LeaseTask(ctx, persistence.LeaseOptions{
			ProjectID: project, LeaseHolder: "h", LeaseDurationSeconds: 1,
		})
		// Force the stale lease into the past via Update.
		past := time.Now().UTC().Add(-1 * time.Minute)
		leasedStale.LeaseExpiresAt = &past
		if err := repo.Update(ctx, leasedStale); err != nil {
			t.Fatalf("Update stale lease: %v", err)
		}

		expired, err := repo.FindExpiredLeases(ctx, 10)
		if err != nil {
			t.Fatalf("FindExpiredLeases: %v", err)
		}
		ids := make(map[string]struct{}, len(expired))
		for _, e := range expired {
			ids[e.ID] = struct{}{}
		}
		if _, ok := ids[leasedStale.ID]; !ok {
			t.Errorf("expired list missing stale lease %s", leasedStale.ID)
		}
		if _, ok := ids[leasedFresh.ID]; ok {
			t.Errorf("expired list incorrectly included fresh lease %s", leasedFresh.ID)
		}
	})

	t.Run("CountByStatus_groups_correctly", func(t *testing.T) {
		project := uniqueID("proj")
		t1 := newQueuedTask(project)
		t2 := newQueuedTask(project)
		t3 := newQueuedTask(project)
		_ = repo.Create(ctx, t1)
		_ = repo.Create(ctx, t2)
		_ = repo.Create(ctx, t3)
		_ = repo.UpdateStatus(ctx, t2.ID, persistence.TaskStatusCompleted)
		_ = repo.UpdateStatus(ctx, t3.ID, persistence.TaskStatusFailed)

		counts, err := repo.CountByStatus(ctx, project)
		if err != nil {
			t.Fatalf("CountByStatus: %v", err)
		}
		if counts[persistence.TaskStatusQueued] != 1 {
			t.Errorf("QUEUED count = %d, want 1", counts[persistence.TaskStatusQueued])
		}
		if counts[persistence.TaskStatusCompleted] != 1 {
			t.Errorf("COMPLETED count = %d, want 1", counts[persistence.TaskStatusCompleted])
		}
		if counts[persistence.TaskStatusFailed] != 1 {
			t.Errorf("FAILED count = %d, want 1", counts[persistence.TaskStatusFailed])
		}
	})

	t.Run("GetDependents_matches_dependency_ids_exactly", func(t *testing.T) {
		project := uniqueID("proj")
		depID := "dep_task_a"
		nearMissID := "depXtaskXa"

		dep := newQueuedTask(project)
		dep.ID = depID
		if err := repo.Create(ctx, dep); err != nil {
			t.Fatalf("Create dep: %v", err)
		}
		nearMiss := newQueuedTask(project)
		nearMiss.ID = nearMissID
		if err := repo.Create(ctx, nearMiss); err != nil {
			t.Fatalf("Create near miss dep: %v", err)
		}

		trueDependent := newQueuedTask(project)
		trueDependent.Dependencies = []string{depID}
		if err := repo.Create(ctx, trueDependent); err != nil {
			t.Fatalf("Create true dependent: %v", err)
		}
		falseDependent := newQueuedTask(project)
		falseDependent.Dependencies = []string{nearMissID}
		if err := repo.Create(ctx, falseDependent); err != nil {
			t.Fatalf("Create false dependent: %v", err)
		}

		got, err := repo.GetDependents(ctx, depID)
		if err != nil {
			t.Fatalf("GetDependents: %v", err)
		}
		if len(got) != 1 || got[0].ID != trueDependent.ID {
			t.Fatalf("GetDependents(%q) = %+v, want only %s", depID, got, trueDependent.ID)
		}
	})

	t.Run("Concurrent_Lease_yields_unique_tasks", func(t *testing.T) {
		// Correctness-only concurrency check: 5 goroutines compete
		// for 5 tasks; each goroutine that gets a lease must get a
		// DIFFERENT task. Postgres parallelizes; SQLite serializes
		// — either way, IDs must be unique across the lease set.
		project := uniqueID("proj")
		const taskCount = 5
		for i := 0; i < taskCount; i++ {
			_ = repo.Create(ctx, newQueuedTask(project))
		}

		type leaseResult struct {
			id  string
			err error
		}
		ch := make(chan leaseResult, taskCount)
		for i := 0; i < taskCount; i++ {
			go func() {
				leased, err := repo.LeaseTask(ctx, persistence.LeaseOptions{
					ProjectID:            project,
					LeaseHolder:          "concurrency-test",
					LeaseDurationSeconds: 60,
				})
				if err != nil {
					ch <- leaseResult{err: err}
					return
				}
				ch <- leaseResult{id: leased.ID}
			}()
		}
		seen := make(map[string]struct{}, taskCount)
		var errs int
		for i := 0; i < taskCount; i++ {
			res := <-ch
			if res.err != nil {
				errs++
				continue
			}
			if _, dup := seen[res.id]; dup {
				t.Errorf("duplicate lease on task %s — two callers picked the same row", res.id)
			}
			seen[res.id] = struct{}{}
		}
		if errs > 0 {
			t.Errorf("%d/%d concurrent leases failed (unexpected — all tasks should be available)", errs, taskCount)
		}
		if len(seen) != taskCount {
			t.Errorf("got %d unique leases, want %d", len(seen), taskCount)
		}
	})
}

// newQueuedTask is a minimal in-memory persistence.Task constructor.
// Required fields only; everything else defaults at Create time
// inside the repo.
func newQueuedTask(projectID string) *persistence.Task {
	return &persistence.Task{
		ID:        uniqueID("task"),
		ProjectID: projectID,
		Priority:  50,
		Payload:   []byte(`{}`),
		Status:    persistence.TaskStatusQueued,
	}
}

// RunAPIKeyRepositorySuite exercises persistence.APIKeyRepository.
// The security-critical lookup paths (active-only filter, revoke
// semantics, hash uniqueness) make this the highest-leverage shared
// suite: any divergence between SQLite + Postgres on these surfaces
// the auth middleware would trust would be a security regression.
func RunAPIKeyRepositorySuite(t *testing.T, repo persistence.APIKeyRepository) {
	t.Helper()
	ctx := context.Background()

	t.Run("Create_then_LookupActiveByHash_resolves_key", func(t *testing.T) {
		hash := uniqueID("hash")
		k := &persistence.APIKey{
			ID:        uniqueID("akey"),
			ProjectID: uniqueID("proj"),
			Name:      "test-key",
			KeyHash:   hash,
			KeyPrefix: "sk-test",
			CreatedAt: time.Now().UTC(),
		}
		if err := repo.Create(ctx, k); err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, err := repo.LookupActiveByHash(ctx, hash)
		if err != nil {
			t.Fatalf("LookupActiveByHash: %v", err)
		}
		if got.ID != k.ID {
			t.Errorf("wrong key returned: %s", got.ID)
		}
	})

	t.Run("LookupActiveByHash_treats_revoked_as_missing", func(t *testing.T) {
		hash := uniqueID("hash")
		k := &persistence.APIKey{
			ID:        uniqueID("akey"),
			ProjectID: uniqueID("proj"),
			KeyHash:   hash,
			KeyPrefix: "sk-test",
			CreatedAt: time.Now().UTC(),
		}
		if err := repo.Create(ctx, k); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := repo.Revoke(ctx, k.ID); err != nil {
			t.Fatalf("Revoke: %v", err)
		}
		_, err := repo.LookupActiveByHash(ctx, hash)
		if err == nil {
			t.Fatal("expected ErrAPIKeyNotFound after Revoke")
		}
		if err != persistence.ErrAPIKeyNotFound {
			t.Errorf("expected ErrAPIKeyNotFound, got %v", err)
		}
	})

	t.Run("LookupActiveByHash_treats_expired_as_missing", func(t *testing.T) {
		past := time.Now().UTC().Add(-1 * time.Hour)
		hash := uniqueID("hash")
		k := &persistence.APIKey{
			ID:        uniqueID("akey"),
			ProjectID: uniqueID("proj"),
			KeyHash:   hash,
			KeyPrefix: "sk-test",
			CreatedAt: time.Now().UTC().Add(-24 * time.Hour),
			ExpiresAt: &past,
		}
		if err := repo.Create(ctx, k); err != nil {
			t.Fatalf("Create: %v", err)
		}
		_, err := repo.LookupActiveByHash(ctx, hash)
		if err == nil {
			t.Fatal("expected ErrAPIKeyNotFound for expired key")
		}
	})

	t.Run("ListByProject_returns_revoked_too", func(t *testing.T) {
		project := uniqueID("proj")
		active := &persistence.APIKey{
			ID: uniqueID("akey"), ProjectID: project, KeyHash: uniqueID("h"),
			KeyPrefix: "sk-test", CreatedAt: time.Now().UTC(),
		}
		revoked := &persistence.APIKey{
			ID: uniqueID("akey"), ProjectID: project, KeyHash: uniqueID("h"),
			KeyPrefix: "sk-test", CreatedAt: time.Now().UTC(),
		}
		_ = repo.Create(ctx, active)
		_ = repo.Create(ctx, revoked)
		_ = repo.Revoke(ctx, revoked.ID)
		rows, err := repo.ListByProject(ctx, project)
		if err != nil {
			t.Fatalf("ListByProject: %v", err)
		}
		if len(rows) != 2 {
			t.Fatalf("ListByProject returned %d, want 2 (including revoked)", len(rows))
		}
	})

	t.Run("TouchLastUsed_updates_timestamp", func(t *testing.T) {
		k := &persistence.APIKey{
			ID: uniqueID("akey"), ProjectID: uniqueID("proj"),
			KeyHash: uniqueID("h"), KeyPrefix: "sk-test",
			CreatedAt: time.Now().UTC(),
		}
		if err := repo.Create(ctx, k); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := repo.TouchLastUsed(ctx, k.ID); err != nil {
			t.Fatalf("TouchLastUsed: %v", err)
		}
		got, _ := repo.LookupActiveByHash(ctx, k.KeyHash)
		if got.LastUsedAt == nil {
			t.Error("LastUsedAt should be set after TouchLastUsed")
		}
	})

	t.Run("Revoke_is_idempotent", func(t *testing.T) {
		k := &persistence.APIKey{
			ID: uniqueID("akey"), ProjectID: uniqueID("proj"),
			KeyHash: uniqueID("h"), KeyPrefix: "sk-test",
			CreatedAt: time.Now().UTC(),
		}
		_ = repo.Create(ctx, k)
		if err := repo.Revoke(ctx, k.ID); err != nil {
			t.Fatalf("Revoke #1: %v", err)
		}
		if err := repo.Revoke(ctx, k.ID); err != nil {
			t.Fatalf("Revoke #2 (idempotent): %v — must succeed", err)
		}
	})

	t.Run("RevokeByName_then_LookupActiveByHash_misses", func(t *testing.T) {
		hash := uniqueID("hash")
		keyName := "agent:task_" + uniqueID("task")
		k := &persistence.APIKey{
			ID:        uniqueID("akey"),
			ProjectID: uniqueID("proj"),
			Name:      keyName,
			KeyHash:   hash,
			KeyPrefix: "sk-test",
			CreatedAt: time.Now().UTC(),
		}
		if err := repo.Create(ctx, k); err != nil {
			t.Fatalf("Create: %v", err)
		}
		// Key is active before revoke.
		if _, err := repo.LookupActiveByHash(ctx, hash); err != nil {
			t.Fatalf("LookupActiveByHash before revoke: %v", err)
		}
		// RevokeByName on the name must succeed.
		if err := repo.RevokeByName(ctx, keyName); err != nil {
			t.Fatalf("RevokeByName: %v", err)
		}
		// After revoke, active lookup must return ErrAPIKeyNotFound.
		_, err := repo.LookupActiveByHash(ctx, hash)
		if err == nil {
			t.Fatal("expected ErrAPIKeyNotFound after RevokeByName")
		}
		if err != persistence.ErrAPIKeyNotFound {
			t.Fatalf("expected ErrAPIKeyNotFound, got %v", err)
		}
	})

	t.Run("RevokeByName_nonexistent_name_is_noop", func(t *testing.T) {
		// Revoking a name that was never created must return nil (idempotent /
		// zero-rows-affected is not an error).
		if err := repo.RevokeByName(ctx, "agent:task_"+uniqueID("ghost")); err != nil {
			t.Fatalf("RevokeByName on nonexistent name must return nil, got: %v", err)
		}
	})
}

// RunTaskLLMUsageSuite exercises the financial cost-recording
// surface. Both backends must agree on what totals come back
// for any (project, time window) — operators rely on those
// numbers for budget enforcement and dashboards.
func RunTaskLLMUsageSuite(t *testing.T, repo persistence.TaskLLMUsageRepository) {
	t.Helper()
	ctx := context.Background()
	project := uniqueID("proj")
	taskA := uniqueID("task")

	rows := []persistence.TaskLLMUsage{
		{ID: uniqueID("u"), ProjectID: project, TaskID: &taskA, StepID: "s1", Role: "worker", Model: "m", PromptTokens: 100, CompletionTokens: 50, Iterations: 1, CostUSD: 0.01},
		{ID: uniqueID("u"), ProjectID: project, TaskID: &taskA, StepID: "s2", Role: "worker", Model: "m", PromptTokens: 200, CompletionTokens: 80, Iterations: 1, CostUSD: 0.02},
		{ID: uniqueID("u"), ProjectID: project, StepID: "s3", Role: "judge", Model: "m2", PromptTokens: 50, CompletionTokens: 20, Iterations: 1, CostUSD: 0.005, Source: persistence.TaskLLMUsageSourceJudge},
	}
	for i := range rows {
		if err := repo.Record(ctx, &rows[i]); err != nil {
			t.Fatalf("Record %s: %v", rows[i].ID, err)
		}
	}

	t.Run("SumCostByProject_returns_sum", func(t *testing.T) {
		total, err := repo.SumCostByProject(ctx, project, time.Time{}, time.Time{})
		if err != nil {
			t.Fatalf("SumCostByProject: %v", err)
		}
		want := 0.01 + 0.02 + 0.005
		// Compare with a tolerance — postgres SUM on float8 may
		// associate the addition in a different order than Go,
		// producing 0.034999999999999996 vs 0.035000000000000003
		// across runs. 1e-9 leaves us 7 decimals of headroom which
		// is far more than financial reporting cares about.
		if math.Abs(total-want) > 1e-9 {
			t.Errorf("SumCostByProject = %v, want %v (tolerance 1e-9)", total, want)
		}
	})

	t.Run("AggregateByRoleModel_groups_correctly", func(t *testing.T) {
		out, err := repo.AggregateByRoleModel(ctx, time.Time{}, time.Time{}, 10, project)
		if err != nil {
			t.Fatalf("AggregateByRoleModel: %v", err)
		}
		if len(out) != 2 {
			t.Fatalf("expected 2 (role, model) groups, got %d", len(out))
		}
	})

	t.Run("Upsert_replaces_by_id", func(t *testing.T) {
		row := &persistence.TaskLLMUsage{
			ID: uniqueID("u-stream"), ProjectID: project, StepID: "s",
			Role: "worker", Model: "m", CostUSD: 0.001,
		}
		if err := repo.Upsert(ctx, row); err != nil {
			t.Fatalf("Upsert #1: %v", err)
		}
		row.CostUSD = 99.99
		if err := repo.Upsert(ctx, row); err != nil {
			t.Fatalf("Upsert #2: %v", err)
		}
		project := row.ProjectID
		filter := persistence.TaskLLMUsageFilter{ProjectID: &project}
		list, _ := repo.List(ctx, filter)
		var found bool
		for _, r := range list {
			if r.ID == row.ID && r.CostUSD == 99.99 {
				found = true
			}
		}
		if !found {
			t.Errorf("Upsert did not replace cost for id %s", row.ID)
		}
	})
}

// RunAutonomyEvaluationSuite exercises the autonomy-tick audit
// surface — Record / List / CountByOutcome.
func RunAutonomyEvaluationSuite(t *testing.T, repo persistence.AutonomyEvaluationRepository) {
	t.Helper()
	ctx := context.Background()
	project := uniqueID("proj")
	for _, outcome := range []string{"CREATED", "REJECTED", "CREATED", "DEDUPED"} {
		if err := repo.Record(ctx, &persistence.AutonomyEvaluation{
			ID: uniqueID("eval"), ProjectID: project, Outcome: outcome, Reason: "test",
		}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	t.Run("CountByOutcome_groups_correctly", func(t *testing.T) {
		counts, err := repo.CountByOutcome(ctx, project, time.Time{}, time.Time{})
		if err != nil {
			t.Fatalf("CountByOutcome: %v", err)
		}
		if counts["CREATED"] != 2 || counts["REJECTED"] != 1 || counts["DEDUPED"] != 1 {
			t.Errorf("counts = %v", counts)
		}
	})

	t.Run("List_filter_by_outcome_isolates_rows", func(t *testing.T) {
		outcome := "CREATED"
		list, err := repo.List(ctx, persistence.AutonomyEvaluationFilter{
			ProjectID: &project, Outcome: &outcome,
		})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(list) != 2 {
			t.Errorf("List = %d, want 2 CREATED rows", len(list))
		}
	})
}

// RunWebhookEventSuite exercises persistence.WebhookEventRepository.
// Both backends must agree on the project-scoped list + the filter
// dimensions (source, status).
func RunWebhookEventSuite(t *testing.T, repo persistence.WebhookEventRepository) {
	t.Helper()
	ctx := context.Background()
	project := uniqueID("proj")

	t.Run("Record_then_List_returns_row", func(t *testing.T) {
		if err := repo.Record(ctx, &persistence.WebhookEvent{
			ID: uniqueID("wh"), ProjectID: project,
			Source: "github", Status: persistence.WebhookEventStatusAccepted,
		}); err != nil {
			t.Fatalf("Record: %v", err)
		}
		list, err := repo.List(ctx, persistence.WebhookEventFilter{ProjectID: &project})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(list) != 1 {
			t.Fatalf("List = %d, want 1", len(list))
		}
	})

	t.Run("List_filter_by_status_isolates_rows", func(t *testing.T) {
		filterProj := uniqueID("proj")
		_ = repo.Record(ctx, &persistence.WebhookEvent{
			ID: uniqueID("wh"), ProjectID: filterProj, Source: "github",
			Status: persistence.WebhookEventStatusAccepted,
		})
		_ = repo.Record(ctx, &persistence.WebhookEvent{
			ID: uniqueID("wh"), ProjectID: filterProj, Source: "github",
			Status: persistence.WebhookEventStatusRejected,
		})
		status := persistence.WebhookEventStatusAccepted
		list, _ := repo.List(ctx, persistence.WebhookEventFilter{
			ProjectID: &filterProj, Status: &status,
		})
		if len(list) != 1 || list[0].Status != persistence.WebhookEventStatusAccepted {
			t.Errorf("status filter returned %v", list)
		}
	})
}

// seedTaskRow inserts a tasks-table row so child rows with FK
// constraints (postgres-side: scratchpad, telegram_threads, judge
// verdicts, post-mortems) can land cleanly. SQLite has FKs off in
// phase 2 so this is harmless there too. Returns the seeded task ID.
func seedTaskRow(t *testing.T, ctx context.Context, taskRepo persistence.TaskRepository) string {
	t.Helper()
	task := newQueuedTask(uniqueID("proj"))
	if err := taskRepo.Create(ctx, task); err != nil {
		t.Fatalf("seedTaskRow: %v", err)
	}
	return task.ID
}

// seedArtifactRow inserts an artifacts-table row so child tables
// with source_artifact_id FKs (memory_quarantine, ingest_queue) can
// land cleanly on Postgres. Returns the seeded artifact ID.
func seedArtifactRow(t *testing.T, ctx context.Context, artifactRepo persistence.ArtifactRepository, projectID string) string {
	t.Helper()
	a := &persistence.Artifact{
		ID:            uniqueID("art"),
		ProjectID:     projectID,
		Name:          "seed.txt",
		ArtifactClass: persistence.ArtifactClassOutput,
		StoragePath:   "seed/" + uniqueID("p"),
		CreatedAt:     time.Now().UTC(),
	}
	if err := artifactRepo.Create(ctx, a); err != nil {
		t.Fatalf("seedArtifactRow: %v", err)
	}
	return a.ID
}

// RunTaskScratchpadSuite — single-row-per-task upsert contract.
// Takes taskRepo so the suite can seed a parent task row (Postgres
// has FK task_scratchpad.task_id → tasks(id)).
func RunTaskScratchpadSuite(t *testing.T, repo persistence.TaskScratchpadRepository, taskRepo persistence.TaskRepository) {
	t.Helper()
	ctx := context.Background()
	taskID := seedTaskRow(t, ctx, taskRepo)

	t.Run("Get_unknown_returns_nil_nil", func(t *testing.T) {
		got, err := repo.Get(ctx, uniqueID("missing"))
		if err != nil {
			t.Fatalf("Get unknown: %v", err)
		}
		if got != nil {
			t.Errorf("Get unknown should return nil scratchpad, got %+v", got)
		}
	})

	t.Run("Upsert_replaces_existing_row", func(t *testing.T) {
		sp := &persistence.TaskScratchpad{
			TaskID:        taskID,
			Summary:       "first",
			Facts:         []byte(`{"a":1}`),
			OpenQuestions: []byte(`["q1"]`),
		}
		if err := repo.Upsert(ctx, sp); err != nil {
			t.Fatalf("Upsert#1: %v", err)
		}
		got, err := repo.Get(ctx, taskID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Summary != "first" {
			t.Errorf("Summary = %q", got.Summary)
		}
		sp.Summary = "second"
		if err := repo.Upsert(ctx, sp); err != nil {
			t.Fatalf("Upsert#2: %v", err)
		}
		got, _ = repo.Get(ctx, taskID)
		if got.Summary != "second" {
			t.Errorf("Upsert did not replace: %q", got.Summary)
		}
	})
}

// RunTelegramThreadSuite — Insert / GetByTask / GetByThread /
// MarkClosed contract + UNIQUE (chat_id, thread_id) safeguard.
// Takes taskRepo to satisfy the FK on telegram_task_threads.task_id.
func RunTelegramThreadSuite(t *testing.T, repo persistence.TelegramThreadRepository, taskRepo persistence.TaskRepository) {
	t.Helper()
	ctx := context.Background()
	taskID := seedTaskRow(t, ctx, taskRepo)
	// +1 so a UnixNano that happens to be a clean multiple of the
	// modulus never produces a zero id — the repo validator rejects
	// zero chat_id/thread_id as missing and the test flakes ~1/1k.
	chatID := time.Now().UnixNano()%100000 + 1
	threadID := time.Now().UnixNano()%1000 + 1

	t.Run("Insert_then_GetByTask_and_GetByThread_resolve", func(t *testing.T) {
		thread := &persistence.TelegramTaskThread{
			TaskID: taskID, ChatID: chatID, ThreadID: threadID,
			TopicName: "Task " + taskID,
		}
		if err := repo.Insert(ctx, thread); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		byTask, err := repo.GetByTask(ctx, taskID)
		if err != nil {
			t.Fatalf("GetByTask: %v", err)
		}
		if byTask.ThreadID != threadID {
			t.Errorf("ThreadID = %d, want %d", byTask.ThreadID, threadID)
		}
		byThread, err := repo.GetByThread(ctx, chatID, threadID)
		if err != nil {
			t.Fatalf("GetByThread: %v", err)
		}
		if byThread.TaskID != taskID {
			t.Errorf("TaskID = %q", byThread.TaskID)
		}
	})

	t.Run("Insert_duplicate_pair_returns_ErrDuplicateKey", func(t *testing.T) {
		otherTask := seedTaskRow(t, ctx, taskRepo)
		dup := &persistence.TelegramTaskThread{
			TaskID: otherTask, ChatID: chatID, ThreadID: threadID,
			TopicName: "dup",
		}
		err := repo.Insert(ctx, dup)
		if err == nil {
			t.Fatal("expected ErrDuplicateKey on (chat_id, thread_id) reuse")
		}
		if !errors.Is(err, persistence.ErrDuplicateKey) {
			t.Fatalf("expected ErrDuplicateKey, got %v", err)
		}
	})

	t.Run("MarkClosed_stamps_closed_at_idempotent", func(t *testing.T) {
		taskID := seedTaskRow(t, ctx, taskRepo)
		_ = repo.Insert(ctx, &persistence.TelegramTaskThread{
			TaskID: taskID, ChatID: chatID + 1, ThreadID: threadID + 1,
			TopicName: "x",
		})
		if err := repo.MarkClosed(ctx, taskID); err != nil {
			t.Fatalf("MarkClosed: %v", err)
		}
		got, _ := repo.GetByTask(ctx, taskID)
		if got.ClosedAt == nil {
			t.Fatal("MarkClosed didn't stamp closed_at")
		}
		// Second call must be a no-op (not bump the timestamp).
		first := *got.ClosedAt
		if err := repo.MarkClosed(ctx, taskID); err != nil {
			t.Fatalf("MarkClosed #2: %v", err)
		}
		got, _ = repo.GetByTask(ctx, taskID)
		if !got.ClosedAt.Equal(first) {
			t.Errorf("idempotent MarkClosed shouldn't bump timestamp: %v vs %v",
				*got.ClosedAt, first)
		}
	})
}

// RunIntentVerdictSuite — two-tier verdict persistence contract.
func RunIntentVerdictSuite(t *testing.T, repo persistence.IntentVerdictRepository) {
	t.Helper()
	ctx := context.Background()
	project := uniqueID("proj")

	v := &persistence.IntentVerdict{
		ID:                      uniqueID("iv"),
		ProjectID:               project,
		ToolName:                "place_order",
		HeuristicRisk:           "medium",
		HeuristicConfidence:     0.7,
		HeuristicRecommendation: "approve",
		HeuristicReasoning:      "first heuristic",
		FinalRisk:               "medium",
		FinalRecommendation:     "approve",
	}

	t.Run("Insert_then_ListRecent_returns_row", func(t *testing.T) {
		if err := repo.Insert(ctx, v); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		got, err := repo.ListRecent(ctx, project, 10)
		if err != nil {
			t.Fatalf("ListRecent: %v", err)
		}
		if len(got) != 1 || got[0].ID != v.ID {
			t.Fatalf("ListRecent = %v", got)
		}
	})

	t.Run("UpdateLLMRefinement_fills_async_columns", func(t *testing.T) {
		llmRisk := "high"
		llmRec := "deny"
		v.LLMRisk = &llmRisk
		v.LLMRecommendation = &llmRec
		if err := repo.UpdateLLMRefinement(ctx, v); err != nil {
			t.Fatalf("UpdateLLMRefinement: %v", err)
		}
		got, _ := repo.ListRecent(ctx, project, 10)
		if got[0].LLMRisk == nil || *got[0].LLMRisk != "high" {
			t.Errorf("LLMRisk = %v", got[0].LLMRisk)
		}
		if got[0].RefinedAt == nil {
			t.Error("RefinedAt should be set after UpdateLLMRefinement")
		}
	})
}

// RunTaskJudgeVerdictSuite — one verdict per task + idempotency.
// Takes taskRepo so the FK on task_judge_verdicts.task_id resolves.
func RunTaskJudgeVerdictSuite(t *testing.T, repo persistence.TaskJudgeVerdictRepository, taskRepo persistence.TaskRepository) {
	t.Helper()
	ctx := context.Background()
	taskID := seedTaskRow(t, ctx, taskRepo)

	v := &persistence.TaskJudgeVerdict{
		ID: uniqueID("jv"), ProjectID: uniqueID("proj"), TaskID: taskID,
		Role: "judge", Model: "claude-opus", Verdict: "factual",
		Signals: []byte(`{"score":0.9}`),
	}
	project := v.ProjectID

	t.Run("Record_then_GetByTask_round_trips", func(t *testing.T) {
		if err := repo.Record(ctx, v); err != nil {
			t.Fatalf("Record: %v", err)
		}
		got, err := repo.GetByTask(ctx, taskID)
		if err != nil {
			t.Fatalf("GetByTask: %v", err)
		}
		if got.ID != v.ID {
			t.Errorf("ID mismatch: %s vs %s", got.ID, v.ID)
		}
		// JSON-parse both for comparison; Postgres normalizes
		// JSONB whitespace ('{"score":0.9}' → '{"score": 0.9}'),
		// so byte-for-byte comparison would diverge across backends.
		var wantSig, gotSig map[string]any
		_ = json.Unmarshal(v.Signals, &wantSig)
		_ = json.Unmarshal(got.Signals, &gotSig)
		if !reflect.DeepEqual(wantSig, gotSig) {
			t.Errorf("Signals round-trip lost data: %s vs %s", got.Signals, v.Signals)
		}
	})

	t.Run("Record_second_verdict_for_same_task_returns_ErrDuplicateKey", func(t *testing.T) {
		err := repo.Record(ctx, &persistence.TaskJudgeVerdict{
			ID: uniqueID("jv"), ProjectID: project, TaskID: taskID,
			Role: "judge", Model: "claude-opus", Verdict: "factual",
		})
		if err == nil {
			t.Fatal("expected ErrDuplicateKey on second Record for same task")
		}
		if err != persistence.ErrDuplicateKey {
			t.Errorf("expected ErrDuplicateKey, got %v", err)
		}
	})

	t.Run("ListRecent_with_empty_project_returns_global", func(t *testing.T) {
		got, err := repo.ListRecent(ctx, "", 100)
		if err != nil {
			t.Fatalf("ListRecent: %v", err)
		}
		var found bool
		for _, r := range got {
			if r.ID == v.ID {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("global ListRecent didn't include the recorded verdict")
		}
	})
}

// RunTaskPostMortemSuite — last-write-wins upsert contract.
// Takes taskRepo so the FK on task_post_mortems.task_id resolves.
func RunTaskPostMortemSuite(t *testing.T, repo persistence.TaskPostMortemRepository, taskRepo persistence.TaskRepository) {
	t.Helper()
	ctx := context.Background()
	taskID := seedTaskRow(t, ctx, taskRepo)

	pm := &persistence.TaskPostMortem{
		TaskID: taskID, ProjectID: uniqueID("proj"),
		Summary: "first", Model: "claude-opus",
	}

	t.Run("Record_then_Get_round_trips", func(t *testing.T) {
		if err := repo.Record(ctx, pm); err != nil {
			t.Fatalf("Record: %v", err)
		}
		got, err := repo.Get(ctx, taskID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Summary != "first" {
			t.Errorf("Summary = %q", got.Summary)
		}
	})

	t.Run("Get_unknown_returns_ErrNotFound", func(t *testing.T) {
		_, err := repo.Get(ctx, uniqueID("missing"))
		if err == nil {
			t.Fatal("expected ErrNotFound")
		}
		if err.Error() != persistence.ErrNotFound.Error() {
			t.Errorf("expected ErrNotFound, got %v", err)
		}
	})

	t.Run("Record_second_time_replaces_row", func(t *testing.T) {
		pm.Summary = "second"
		if err := repo.Record(ctx, pm); err != nil {
			t.Fatalf("Record#2: %v", err)
		}
		got, _ := repo.Get(ctx, taskID)
		if got.Summary != "second" {
			t.Errorf("upsert did not replace: %q", got.Summary)
		}
	})
}

// RunMemoryRetrievalAuditSuite — Record-only contract. The
// FeedbackStats + UnretrievedChunkIDs paths need a seeded
// project_memory_chunks table, which is cross-table state the
// shared suite doesn't manage. Per-backend tests cover those.
func RunMemoryRetrievalAuditSuite(t *testing.T, repo persistence.MemoryRetrievalAuditRepository) {
	t.Helper()
	ctx := context.Background()
	project := uniqueID("proj")

	t.Run("Record_persists_chunk_ids_array", func(t *testing.T) {
		audit := &persistence.MemoryRetrievalAudit{
			ProjectID: project,
			Query:     "what is x",
			ChunkIDs:  []string{"c1", "c2"},
		}
		if err := repo.Record(ctx, audit); err != nil {
			t.Fatalf("Record: %v", err)
		}
		if audit.ID == "" {
			t.Error("Record should populate ID when caller leaves empty")
		}
	})
}

// RunTradingOrderSuite exercises the broker→daemon audit channel's
// load-bearing identity-mismatch safeguard. A divergence here would
// re-open the NVDA bookkeeping corruption class.
func RunTradingOrderSuite(t *testing.T, repo persistence.TradingOrderRepository) {
	t.Helper()
	ctx := context.Background()
	limit := 178.50
	order := &persistence.TradingOrder{
		ID:             uniqueID("ord"),
		ProjectID:      uniqueID("proj"),
		IdempotencyKey: uniqueID("idem"),
		Mode:           "paper",
		Symbol:         "AAPL",
		Action:         "buy",
		OrderType:      "LMT",
		Qty:            6.0,
		LimitPrice:     &limit,
		TimeInForce:    "DAY",
		Status:         "submitted",
	}

	t.Run("Record_then_List_returns_row", func(t *testing.T) {
		if err := repo.Record(ctx, order); err != nil {
			t.Fatalf("Record: %v", err)
		}
		project := order.ProjectID
		list, err := repo.List(ctx, persistence.TradingOrderFilter{ProjectID: &project})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(list) != 1 || list[0].ID != order.ID {
			t.Fatalf("List returned %+v", list)
		}
	})

	t.Run("Record_same_identity_merges_status", func(t *testing.T) {
		order.Status = "filled"
		if err := repo.Record(ctx, order); err != nil {
			t.Fatalf("Record (same idem, same identity): %v", err)
		}
		project := order.ProjectID
		list, _ := repo.List(ctx, persistence.TradingOrderFilter{ProjectID: &project})
		if list[0].Status != "filled" {
			t.Errorf("status not merged: %s", list[0].Status)
		}
	})

	t.Run("Record_identity_mismatch_returns_ErrOrderIdentityMismatch", func(t *testing.T) {
		bad := &persistence.TradingOrder{
			ID:             uniqueID("ord"),
			ProjectID:      order.ProjectID,
			IdempotencyKey: order.IdempotencyKey,
			Mode:           "paper",
			Symbol:         "MSFT", // different from AAPL
			Action:         "buy",
			OrderType:      "LMT",
			Qty:            6.0,
			LimitPrice:     &limit,
			Status:         "submitted",
		}
		err := repo.Record(ctx, bad)
		if err == nil {
			t.Fatal("expected identity-mismatch error")
		}
		if !errors.Is(err, persistence.ErrOrderIdentityMismatch) {
			t.Fatalf("expected ErrOrderIdentityMismatch, got %v", err)
		}
	})
}

// RunTradingFillSuite — fill ingestion + SumVolume. INSERT OR
// IGNORE idempotency on (id) pinned with a retry-same-fill case.
// Takes orderRepo so the FK on trading_fills.order_id resolves.
func RunTradingFillSuite(t *testing.T, repo persistence.TradingFillRepository, orderRepo persistence.TradingOrderRepository) {
	t.Helper()
	ctx := context.Background()
	project := uniqueID("proj")

	// Seed the two parent orders so the FK on trading_fills.order_id
	// resolves on Postgres. SQLite has FKs off in phase 2; the seed
	// is harmless there.
	seedOrder := func(orderID, symbol string) {
		t.Helper()
		if err := orderRepo.Record(ctx, &persistence.TradingOrder{
			ID:             orderID,
			ProjectID:      project,
			IdempotencyKey: uniqueID("idem"),
			Mode:           "paper",
			Symbol:         symbol,
			Action:         "buy",
			OrderType:      "MKT",
			Qty:            10,
			TimeInForce:    "DAY",
			Status:         "submitted",
		}); err != nil {
			t.Fatalf("seed order %s: %v", orderID, err)
		}
	}
	order1 := uniqueID("ord")
	order2 := uniqueID("ord")
	seedOrder(order1, "AAPL")
	seedOrder(order2, "MSFT")

	commission := 0.02
	fills := []persistence.TradingFill{
		{ID: uniqueID("f"), OrderID: order1, ProjectID: project, Symbol: "AAPL", Qty: 6, Price: 178.50, CommissionUSD: &commission},
		{ID: uniqueID("f"), OrderID: order1, ProjectID: project, Symbol: "AAPL", Qty: 4, Price: 179.00, CommissionUSD: &commission},
		{ID: uniqueID("f"), OrderID: order2, ProjectID: project, Symbol: "MSFT", Qty: 10, Price: 400, CommissionUSD: nil},
	}

	t.Run("Record_then_List_returns_rows", func(t *testing.T) {
		for i := range fills {
			if err := repo.Record(ctx, &fills[i]); err != nil {
				t.Fatalf("Record %s: %v", fills[i].ID, err)
			}
		}
		got, err := repo.List(ctx, persistence.TradingFillFilter{ProjectID: &project})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("List = %d, want 3", len(got))
		}
	})

	t.Run("Record_retry_same_id_is_noop", func(t *testing.T) {
		if err := repo.Record(ctx, &fills[0]); err != nil {
			t.Fatalf("retry: %v", err)
		}
		got, _ := repo.List(ctx, persistence.TradingFillFilter{ProjectID: &project})
		if len(got) != 3 {
			t.Errorf("retry created a duplicate: %d rows", len(got))
		}
	})

	t.Run("SumVolume_returns_qty_times_price_sum", func(t *testing.T) {
		volume, err := repo.SumVolume(ctx, persistence.TradingFillFilter{ProjectID: &project})
		if err != nil {
			t.Fatalf("SumVolume: %v", err)
		}
		want := 6*178.50 + 4*179.00 + 10*400.0
		if volume != want {
			t.Errorf("SumVolume = %v, want %v", volume, want)
		}
	})
}

// RunTradingSafetyEventSuite — append-only safety event log,
// idempotent retries on (id).
func RunTradingSafetyEventSuite(t *testing.T, repo persistence.TradingSafetyEventRepository) {
	t.Helper()
	ctx := context.Background()
	project := uniqueID("proj")

	t.Run("Record_idempotent_and_List_returns_row", func(t *testing.T) {
		event := &persistence.TradingSafetyEvent{
			ID: uniqueID("evt"), ProjectID: project,
			Kind: "breaker_trip", Severity: "alert",
			Detail: []byte(`{"window_pct":-0.05}`),
		}
		if err := repo.Record(ctx, event); err != nil {
			t.Fatalf("Record: %v", err)
		}
		// Idempotent retry — INSERT OR IGNORE.
		if err := repo.Record(ctx, event); err != nil {
			t.Fatalf("Record retry: %v", err)
		}
		rows, err := repo.List(ctx, persistence.TradingSafetyEventFilter{ProjectID: &project})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("List = %d, want 1 (retry collapsed)", len(rows))
		}
		// JSON detail round-trips with possible whitespace
		// normalization on Postgres.
		var wantD, gotD map[string]any
		_ = json.Unmarshal(event.Detail, &wantD)
		_ = json.Unmarshal(rows[0].Detail, &gotD)
		if !reflect.DeepEqual(wantD, gotD) {
			t.Errorf("Detail round-trip mismatch: %s", rows[0].Detail)
		}
	})

	t.Run("Count_matches_list_length", func(t *testing.T) {
		n, err := repo.Count(ctx, persistence.TradingSafetyEventFilter{ProjectID: &project})
		if err != nil {
			t.Fatalf("Count: %v", err)
		}
		if n != 1 {
			t.Errorf("Count = %d, want 1", n)
		}
	})

	t.Run("List_filter_by_kind", func(t *testing.T) {
		kind := "breaker_trip"
		rows, _ := repo.List(ctx, persistence.TradingSafetyEventFilter{
			ProjectID: &project, Kind: &kind,
		})
		if len(rows) != 1 {
			t.Errorf("kind filter returned %d rows, want 1", len(rows))
		}
	})
}

// RunExecutionStepOutcomeSuite — Record/Finalize lifecycle for
// the outcome row that drives the spend-quality dashboards. Both
// backends must agree on FinalizePending's "find newest pending"
// pickup + SweepPending's batch finalize.
func RunExecutionStepOutcomeSuite(t *testing.T, repo persistence.ExecutionStepOutcomeRepository) {
	t.Helper()
	ctx := context.Background()
	project := uniqueID("proj")
	exec := uniqueID("exec")

	t.Run("Record_Finalize_Sweep_lifecycle", func(t *testing.T) {
		// Two pending rows under one execution.
		for i, step := range []string{"s1", "s2"} {
			if err := repo.Record(ctx, &persistence.ExecutionStepOutcome{
				ID:          uniqueID("oc"),
				ProjectID:   project,
				TaskID:      uniqueID("t"),
				ExecutionID: exec,
				StepID:      step,
				Role:        "worker",
				Model:       "m",
				Outcome:     "pending_validation",
				RecordedAt:  time.Now().UTC().Add(time.Duration(i) * time.Millisecond),
			}); err != nil {
				t.Fatalf("Record %s: %v", step, err)
			}
		}
		role, model, err := repo.FinalizePending(ctx, exec, "s1", "ok", "", "", nil)
		if err != nil {
			t.Fatalf("FinalizePending: %v", err)
		}
		if role != "worker" || model != "m" {
			t.Errorf("FinalizePending returned (%q, %q), want (worker, m)", role, model)
		}
		swept, err := repo.SweepPending(ctx, exec, "ok")
		if err != nil {
			t.Fatalf("SweepPending: %v", err)
		}
		if len(swept) != 1 || swept[0].StepID != "s2" {
			t.Fatalf("SweepPending returned %v", swept)
		}
	})

	t.Run("CountByRoleModelOutcome_groups_ok_rows", func(t *testing.T) {
		counts, err := repo.CountByRoleModelOutcome(ctx, "ok", time.Time{}, time.Time{}, project)
		if err != nil {
			t.Fatalf("CountByRoleModelOutcome: %v", err)
		}
		if len(counts) != 1 || counts[0].Count != 2 {
			t.Errorf("expected one (worker, m) row with count 2, got %+v", counts)
		}
	})

	// FinalizePending_is_CAS is the hardening regression (2026-06-15,
	// memory LLD review batch 4): once a pending row is finalized, a
	// second finalize of the same (execution, step) must be a no-op
	// (ErrNotFound), not a last-write-wins overwrite of the terminal
	// outcome. Two siblings racing to finalize a shared parent pending
	// row hit exactly this.
	t.Run("FinalizePending_is_CAS", func(t *testing.T) {
		exec2 := uniqueID("exec")
		if err := repo.Record(ctx, &persistence.ExecutionStepOutcome{
			ID:          uniqueID("oc"),
			ProjectID:   project,
			TaskID:      uniqueID("t"),
			ExecutionID: exec2,
			StepID:      "s1",
			Role:        "worker",
			Model:       "m",
			Outcome:     "pending_validation",
			RecordedAt:  time.Now().UTC(),
		}); err != nil {
			t.Fatalf("Record: %v", err)
		}
		if _, _, err := repo.FinalizePending(ctx, exec2, "s1", "ok", "", "", nil); err != nil {
			t.Fatalf("first FinalizePending: %v", err)
		}
		// Second finalize (the losing sibling) must not overwrite.
		_, _, err := repo.FinalizePending(ctx, exec2, "s1", "error", "boom", "d", nil)
		if !errors.Is(err, persistence.ErrNotFound) {
			t.Fatalf("second FinalizePending err = %v, want ErrNotFound (CAS rejects overwrite)", err)
		}
	})

	// Migration-106 budget-stamp round-trip (instinct ↔ tool-budget seam).
	// Two sub-cases: (a) all three columns set on an agent-step row, and
	// (b) all three left NULL (non-agent step / pre-migration row).
	t.Run("BudgetStamp_roundtrip_set", func(t *testing.T) {
		eff := 120
		used := 43
		id := uniqueID("oc")
		tier := "complex"
		if err := repo.Record(ctx, &persistence.ExecutionStepOutcome{
			ID:                  id,
			ProjectID:           project,
			TaskID:              uniqueID("t"),
			ExecutionID:         uniqueID("exec"),
			StepID:              "agent-s1",
			Role:                "coder",
			Model:               "m",
			Outcome:             "ok",
			RecordedAt:          time.Now().UTC(),
			ComplexityTier:      tier,
			EffectiveToolBudget: &eff,
			ToolCallsUsed:       &used,
		}); err != nil {
			t.Fatalf("Record (budget set): %v", err)
		}
		rows, err := repo.List(ctx, persistence.ExecutionStepOutcomeFilter{
			StepID: &[]string{"agent-s1"}[0],
		})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		var got *persistence.ExecutionStepOutcome
		for _, r := range rows {
			if r.ID == id {
				got = r
				break
			}
		}
		if got == nil {
			t.Fatalf("row %s not found in List result", id)
		}
		if got.ComplexityTier != tier {
			t.Errorf("ComplexityTier: got %q, want %q", got.ComplexityTier, tier)
		}
		if got.EffectiveToolBudget == nil || *got.EffectiveToolBudget != eff {
			t.Errorf("EffectiveToolBudget: got %v, want %d", got.EffectiveToolBudget, eff)
		}
		if got.ToolCallsUsed == nil || *got.ToolCallsUsed != used {
			t.Errorf("ToolCallsUsed: got %v, want %d", got.ToolCallsUsed, used)
		}
	})

	t.Run("BudgetStamp_roundtrip_null", func(t *testing.T) {
		id := uniqueID("oc")
		stepID := "gate-s2-" + id
		if err := repo.Record(ctx, &persistence.ExecutionStepOutcome{
			ID:          id,
			ProjectID:   project,
			TaskID:      uniqueID("t"),
			ExecutionID: uniqueID("exec"),
			StepID:      stepID,
			Role:        "gate",
			Model:       "",
			Outcome:     "ok",
			RecordedAt:  time.Now().UTC(),
			// ComplexityTier, EffectiveToolBudget, ToolCallsUsed intentionally zero/nil
		}); err != nil {
			t.Fatalf("Record (budget null): %v", err)
		}
		rows, err := repo.List(ctx, persistence.ExecutionStepOutcomeFilter{
			StepID: &stepID,
		})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		var got *persistence.ExecutionStepOutcome
		for _, r := range rows {
			if r.ID == id {
				got = r
				break
			}
		}
		if got == nil {
			t.Fatalf("row %s not found in List result", id)
		}
		if got.ComplexityTier != "" {
			t.Errorf("ComplexityTier: got %q, want empty (NULL)", got.ComplexityTier)
		}
		if got.EffectiveToolBudget != nil {
			t.Errorf("EffectiveToolBudget: got %v, want nil (NULL)", got.EffectiveToolBudget)
		}
		if got.ToolCallsUsed != nil {
			t.Errorf("ToolCallsUsed: got %v, want nil (NULL)", got.ToolCallsUsed)
		}
	})
}

// RunKnowledgeEntitySuite — CRUD + AddAlias + UpdateLifecycle.
// SimilarByEmbedding is unimplemented on SQLite (no pgvector),
// so it stays per-backend and is not part of the shared contract.
func RunKnowledgeEntitySuite(t *testing.T, repo persistence.KnowledgeEntityRepository) {
	t.Helper()
	ctx := context.Background()
	project := uniqueID("proj")
	name := "Vadim-" + uniqueID("")
	ent := &persistence.KnowledgeEntity{
		ProjectID:     project,
		Type:          "person",
		CanonicalName: name,
		Aliases:       []byte(`["Vad"]`),
	}

	t.Run("Insert_then_GetByCanonical_round_trips", func(t *testing.T) {
		if err := repo.Insert(ctx, ent); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		// Insert with a blank ID assigns the deterministic identity-triple
		// hash (idempotent extraction), not a random ID.
		if want := persistence.DeterministicEntityID(project, "person", name); ent.ID != want {
			t.Errorf("Insert assigned ID %q, want deterministic %q", ent.ID, want)
		}
		got, err := repo.GetByCanonical(ctx, project, "person", name)
		if err != nil {
			t.Fatalf("GetByCanonical: %v", err)
		}
		if got.ID != ent.ID {
			t.Errorf("ID mismatch: %s vs %s", got.ID, ent.ID)
		}
	})

	t.Run("Insert_duplicate_canonical_returns_ErrDuplicateKey", func(t *testing.T) {
		dup := &persistence.KnowledgeEntity{
			ProjectID: project, Type: "person", CanonicalName: name,
		}
		err := repo.Insert(ctx, dup)
		if err == nil {
			t.Fatal("expected ErrDuplicateKey")
		}
		if !errors.Is(err, persistence.ErrDuplicateKey) {
			t.Errorf("expected ErrDuplicateKey, got %v", err)
		}
	})

	t.Run("AddAlias_appends_without_duplicates", func(t *testing.T) {
		if err := repo.AddAlias(ctx, ent.ID, "Vad"); err != nil {
			t.Fatalf("AddAlias existing: %v", err)
		}
		if err := repo.AddAlias(ctx, ent.ID, "VG"); err != nil {
			t.Fatalf("AddAlias new: %v", err)
		}
		got, _ := repo.Get(ctx, ent.ID)
		var aliases []string
		_ = json.Unmarshal(got.Aliases, &aliases)
		if len(aliases) != 2 {
			t.Errorf("expected 2 aliases (Vad + VG), got %v", aliases)
		}
	})

	t.Run("UpdateLifecycle_flips_state", func(t *testing.T) {
		if err := repo.UpdateLifecycle(ctx, ent.ID, "quarantined"); err != nil {
			t.Fatalf("UpdateLifecycle: %v", err)
		}
		got, _ := repo.Get(ctx, ent.ID)
		if got.LifecycleState != "quarantined" {
			t.Errorf("lifecycle = %s, want quarantined", got.LifecycleState)
		}
	})
}

// RunKnowledgeEdgeSuite — Upsert merge semantic + DropChunkFromSources.
// Takes entityRepo to seed the two entities the edge references.
func RunKnowledgeEdgeSuite(t *testing.T, repo persistence.KnowledgeEdgeRepository, entityRepo persistence.KnowledgeEntityRepository) {
	t.Helper()
	ctx := context.Background()
	project := uniqueID("proj")

	// Seed the two entities the edge will reference.
	entA := &persistence.KnowledgeEntity{
		ProjectID: project, Type: "person", CanonicalName: "Alice-" + uniqueID(""),
	}
	entB := &persistence.KnowledgeEntity{
		ProjectID: project, Type: "person", CanonicalName: "Bob-" + uniqueID(""),
	}
	if err := entityRepo.Insert(ctx, entA); err != nil {
		t.Fatalf("seed entity A: %v", err)
	}
	if err := entityRepo.Insert(ctx, entB); err != nil {
		t.Fatalf("seed entity B: %v", err)
	}

	t.Run("UpsertEdge_merges_source_chunks_and_max_confidence", func(t *testing.T) {
		e1 := &persistence.KnowledgeEdge{
			ProjectID: project, FromEntity: entA.ID, ToEntity: entB.ID,
			Predicate: "works_with", SourceChunks: []string{"c1"}, Confidence: 0.5,
		}
		if err := repo.UpsertEdge(ctx, e1); err != nil {
			t.Fatalf("UpsertEdge#1: %v", err)
		}
		e2 := &persistence.KnowledgeEdge{
			ProjectID: project, FromEntity: entA.ID, ToEntity: entB.ID,
			Predicate: "works_with", SourceChunks: []string{"c2"}, Confidence: 0.9,
		}
		if err := repo.UpsertEdge(ctx, e2); err != nil {
			t.Fatalf("UpsertEdge#2: %v", err)
		}
		got, err := repo.List(ctx, persistence.KnowledgeEdgeFilter{ProjectID: project})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("Upsert did not merge: %d rows", len(got))
		}
		if len(got[0].SourceChunks) != 2 {
			t.Errorf("merged source_chunks = %v, want 2", got[0].SourceChunks)
		}
		if got[0].Confidence != 0.9 {
			t.Errorf("Confidence not max-merged: %v", got[0].Confidence)
		}
	})

	// Hardening 2026-06-15 (memory LLD review batch 4): edge integrity is
	// enforced at write time.
	t.Run("UpsertEdge_rejects_self_loop", func(t *testing.T) {
		err := repo.UpsertEdge(ctx, &persistence.KnowledgeEdge{
			ProjectID: project, FromEntity: entA.ID, ToEntity: entA.ID,
			Predicate: "self", SourceChunks: []string{"c1"},
		})
		if err == nil {
			t.Fatal("self-loop edge was accepted, want error")
		}
	})

	t.Run("UpsertEdge_rejects_cross_project_entity", func(t *testing.T) {
		// An entity in a DIFFERENT project.
		other := uniqueID("proj")
		entC := &persistence.KnowledgeEntity{
			ProjectID: other, Type: "person", CanonicalName: "Carol-" + uniqueID(""),
		}
		if err := entityRepo.Insert(ctx, entC); err != nil {
			t.Fatalf("seed cross-project entity: %v", err)
		}
		err := repo.UpsertEdge(ctx, &persistence.KnowledgeEdge{
			ProjectID: project, FromEntity: entA.ID, ToEntity: entC.ID,
			Predicate: "works_with", SourceChunks: []string{"c1"},
		})
		if err == nil {
			t.Fatal("edge to a cross-project entity was accepted, want error")
		}
	})

	// DropChunkFromSources is implementation-specific (postgres uses a
	// single-statement CTE; SQLite walks rows in Go). The two
	// backends behave subtly differently on whether the outer
	// lifecycle-flip is visible to a subsequent List; per-backend
	// tests cover that.
}

// RunMemoryQuarantineSuite — Insert + ListPending + MarkReleased +
// CountByGate lifecycle. Postgres enforces source_artifact_id FK →
// artifacts(id); takes artifactRepo for seeding.
func RunMemoryQuarantineSuite(t *testing.T, repo persistence.MemoryQuarantineRepository, artifactRepo persistence.ArtifactRepository) {
	t.Helper()
	ctx := context.Background()
	project := uniqueID("proj")
	artifactID := seedArtifactRow(t, ctx, artifactRepo, project)

	t.Run("Insert_then_ListPending_returns_row", func(t *testing.T) {
		item := &persistence.MemoryQuarantineItem{
			ProjectID:        project,
			SourceArtifactID: artifactID,
			Content:          "leaked secret",
			ContentHash:      "h",
			FailedGate:       "secret_leak",
		}
		if err := repo.Insert(ctx, item); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		pending, err := repo.ListPending(ctx, project, 10)
		if err != nil {
			t.Fatalf("ListPending: %v", err)
		}
		if len(pending) != 1 {
			t.Fatalf("ListPending = %d, want 1", len(pending))
		}
	})

	t.Run("CountByGate_groups_by_failed_gate", func(t *testing.T) {
		counts, err := repo.CountByGate(ctx, project)
		if err != nil {
			t.Fatalf("CountByGate: %v", err)
		}
		if counts["secret_leak"] != 1 {
			t.Errorf("CountByGate = %v, want secret_leak=1", counts)
		}
	})

	t.Run("MarkReleased_drops_from_pending_list", func(t *testing.T) {
		pending, _ := repo.ListPending(ctx, project, 10)
		if len(pending) == 0 {
			t.Fatal("nothing pending to release")
		}
		// Empty chunk-id is stored as NULL (FK on released_chunk_id
		// otherwise rejects unknown IDs on Postgres). Operators
		// sometimes release before the chunk lands.
		if err := repo.MarkReleased(ctx, pending[0].ID, ""); err != nil {
			t.Fatalf("MarkReleased: %v", err)
		}
		after, _ := repo.ListPending(ctx, project, 10)
		if len(after) != 0 {
			t.Errorf("ListPending after release = %d, want 0", len(after))
		}
	})
}

// RunCorpusEpochSuite — Create/Activate/Deactivate/ListActive
// lifecycle. RollbackTo is implementation-specific (postgres uses
// SQL CTE; SQLite uses BEGIN IMMEDIATE) so it's covered by
// per-backend tests, not the shared contract.
func RunCorpusEpochSuite(t *testing.T, repo persistence.CorpusEpochRepository) {
	t.Helper()
	ctx := context.Background()
	project := uniqueID("proj")
	id1 := "epoch-" + uniqueID("")
	id2 := "epoch-" + uniqueID("")

	t.Run("CreateEpoch_Activate_ListActive_round_trip", func(t *testing.T) {
		for _, id := range []string{id1, id2} {
			if err := repo.CreateEpoch(ctx, &persistence.CorpusEpoch{ID: id, ProjectID: project}); err != nil {
				t.Fatalf("CreateEpoch %s: %v", id, err)
			}
			if err := repo.Activate(ctx, project, id, "test", "smoke"); err != nil {
				t.Fatalf("Activate %s: %v", id, err)
			}
		}
		active, err := repo.ListActive(ctx, project)
		if err != nil {
			t.Fatalf("ListActive: %v", err)
		}
		if len(active) != 2 {
			t.Fatalf("ListActive = %d, want 2", len(active))
		}
	})

	t.Run("Activate_is_idempotent", func(t *testing.T) {
		if err := repo.Activate(ctx, project, id1, "test", "again"); err != nil {
			t.Fatalf("re-Activate: %v", err)
		}
		active, _ := repo.ListActive(ctx, project)
		if len(active) != 2 {
			t.Errorf("re-Activate created a dup: %d active", len(active))
		}
	})

	t.Run("Deactivate_removes_from_active_set", func(t *testing.T) {
		if err := repo.Deactivate(ctx, project, id1, "test"); err != nil {
			t.Fatalf("Deactivate: %v", err)
		}
		active, _ := repo.ListActive(ctx, project)
		if len(active) != 1 || active[0] != id2 {
			t.Errorf("after Deactivate: %v", active)
		}
	})
}

// RunIngestQueueSuite — Enqueue → ClaimBatch → MarkDone / MarkFailed
// lifecycle. Postgres enforces source_artifact_id FK → artifacts(id);
// takes artifactRepo for seeding.
func RunIngestQueueSuite(t *testing.T, repo persistence.IngestQueueRepository, artifactRepo persistence.ArtifactRepository) {
	t.Helper()
	ctx := context.Background()
	project := uniqueID("proj")

	t.Run("Enqueue_then_ClaimBatch_returns_processing_rows", func(t *testing.T) {
		// Distinct artifacts per row: the active-idempotency index allows
		// at most one active row per (project, artifact), so multiple
		// queued rows means multiple artifacts.
		for i := 0; i < 3; i++ {
			art := seedArtifactRow(t, ctx, artifactRepo, project)
			if err := repo.Enqueue(ctx, &persistence.IngestQueueItem{
				ProjectID:        project,
				SourceArtifactID: art,
				ProducerRole:     "worker",
				Priority:         int16(i),
			}); err != nil {
				t.Fatalf("Enqueue %d: %v", i, err)
			}
		}
		depth, err := repo.QueueDepth(ctx, project)
		if err != nil {
			t.Fatalf("QueueDepth: %v", err)
		}
		if depth != 3 {
			t.Errorf("QueueDepth = %d, want 3", depth)
		}
		claimed, err := repo.ClaimBatch(ctx, project, 10)
		if err != nil {
			t.Fatalf("ClaimBatch: %v", err)
		}
		if len(claimed) != 3 {
			t.Fatalf("ClaimBatch = %d, want 3", len(claimed))
		}
		for _, item := range claimed {
			if item.State != "processing" {
				t.Errorf("claimed item state = %s, want processing", item.State)
			}
			if item.Attempts != 1 {
				t.Errorf("claimed item attempts = %d, want 1", item.Attempts)
			}
		}
	})

	// Hardening 2026-06-15 (memory LLD review batch 2): a second enqueue
	// of the same (project, artifact) while a row is still active must be
	// a no-op, not a duplicate that re-ingests identical content. Once the
	// prior row reaches a terminal state the artifact can be re-enqueued.
	t.Run("Enqueue_idempotent_while_active", func(t *testing.T) {
		art := seedArtifactRow(t, ctx, artifactRepo, project)
		for i := 0; i < 3; i++ {
			if err := repo.Enqueue(ctx, &persistence.IngestQueueItem{
				ProjectID: project, SourceArtifactID: art, ProducerRole: "worker",
			}); err != nil {
				t.Fatalf("Enqueue %d: %v", i, err)
			}
		}
		claimed, err := repo.ClaimBatch(ctx, project, 10)
		if err != nil {
			t.Fatalf("ClaimBatch: %v", err)
		}
		active := 0
		for _, it := range claimed {
			if it.SourceArtifactID == art {
				active++
			}
		}
		if active != 1 {
			t.Fatalf("duplicate enqueue created %d active rows for one artifact, want 1", active)
		}
		// After the row goes terminal, the artifact can be re-enqueued.
		if _, err := repo.MarkFailed(ctx, claimed[0].ID, 1, "done-ish"); err != nil {
			t.Fatalf("MarkFailed: %v", err)
		}
		if err := repo.Enqueue(ctx, &persistence.IngestQueueItem{
			ProjectID: project, SourceArtifactID: art, ProducerRole: "worker",
		}); err != nil {
			t.Fatalf("re-enqueue after terminal: %v", err)
		}
		reclaimed, err := repo.ClaimBatch(ctx, project, 10)
		if err != nil {
			t.Fatalf("ClaimBatch after re-enqueue: %v", err)
		}
		if len(reclaimed) != 1 || reclaimed[0].SourceArtifactID != art {
			t.Fatalf("re-enqueue after terminal did not produce a fresh active row: %+v", reclaimed)
		}
	})

	t.Run("MarkFailed_terminal_when_attempts_exhausted", func(t *testing.T) {
		art := seedArtifactRow(t, ctx, artifactRepo, project)
		_ = repo.Enqueue(ctx, &persistence.IngestQueueItem{
			ProjectID: project, SourceArtifactID: art,
			ProducerRole: "worker",
		})
		claimed, _ := repo.ClaimBatch(ctx, project, 1)
		if len(claimed) == 0 {
			t.Fatal("nothing to claim")
		}
		terminal, err := repo.MarkFailed(ctx, claimed[0].ID, 1, "boom")
		if err != nil {
			t.Fatalf("MarkFailed: %v", err)
		}
		if !terminal {
			t.Error("MarkFailed should return terminal=true at attempts >= max")
		}
	})

	t.Run("MarkFailed_re-queues_when_attempts_remain", func(t *testing.T) {
		art := seedArtifactRow(t, ctx, artifactRepo, project)
		_ = repo.Enqueue(ctx, &persistence.IngestQueueItem{
			ProjectID: project, SourceArtifactID: art,
			ProducerRole: "worker",
		})
		claimed, _ := repo.ClaimBatch(ctx, project, 1)
		if len(claimed) == 0 {
			t.Fatal("nothing to claim")
		}
		terminal, err := repo.MarkFailed(ctx, claimed[0].ID, 10, "transient")
		if err != nil {
			t.Fatalf("MarkFailed retry: %v", err)
		}
		if terminal {
			t.Error("MarkFailed should NOT be terminal when attempts < max")
		}
	})
}

// RunTradingSnapshotSuite — equity/cash time series.
func RunTradingSnapshotSuite(t *testing.T, repo persistence.TradingPositionsSnapshotRepository) {
	t.Helper()
	ctx := context.Background()
	project := uniqueID("proj")
	base := time.Now().UTC().Add(-1 * time.Hour)

	t.Run("Record_then_ListSince_returns_oldest_first", func(t *testing.T) {
		for i := 0; i < 3; i++ {
			if err := repo.Record(ctx, &persistence.TradingPositionsSnapshot{
				ID:              uniqueID("snap"),
				ProjectID:       project,
				RecordedAt:      base.Add(time.Duration(i) * time.Minute),
				CashUSD:         10000 + float64(i),
				EquityUSD:       11000 + float64(i),
				UnrealisedPLUSD: 100 * float64(i),
				PositionsJSON:   []byte(`{"AAPL":10}`),
			}); err != nil {
				t.Fatalf("Record %d: %v", i, err)
			}
		}
		since := base.Add(30 * time.Second)
		got, err := repo.ListSince(ctx, project, since, 0)
		if err != nil {
			t.Fatalf("ListSince: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("ListSince = %d, want 2 (last 2 of 3)", len(got))
		}
		// Oldest-first: first row's UnrealisedPL = 100 (i=1).
		if got[0].UnrealisedPLUSD != 100 {
			t.Errorf("oldest-first violated: first = %v", got[0].UnrealisedPLUSD)
		}
	})
}

// RunExecutionRepositorySuite exercises persistence.ExecutionRepository.
// Covers the contract surface 19 consumer-packages depend on: CRUD +
// terminal-state markers (RecordCompletion/Failure) + the
// SupersedeNonTerminalForTask sweep that the orphan-PAUSED follow-on
// (2026-05-22 incident) added + the GetByTaskIDs batch shape (autonomy
// state builder) + CountByStatus aggregation.
//
// Takes a TaskRepository alongside ExecutionRepository because
// postgres enforces the executions.task_id FK to tasks(id); the
// suite seeds task rows before creating their executions. SQLite
// builds with FKs off so this would be optional there, but the
// shared suite enforces the production-shape contract.
func RunExecutionRepositorySuite(t *testing.T, repo persistence.ExecutionRepository, taskRepo persistence.TaskRepository) {
	t.Helper()
	ctx := context.Background()

	// seedTask creates an empty Task row with the given ID. Used by
	// every sub-test that needs an Execution whose task_id FK must
	// resolve.
	seedTask := func(t *testing.T, taskID, projectID string) {
		t.Helper()
		task := newQueuedTask(projectID)
		task.ID = taskID
		if err := taskRepo.Create(ctx, task); err != nil {
			t.Fatalf("seedTask Create: %v", err)
		}
	}

	newExec := func(t *testing.T, taskID, projectID string, status persistence.ExecutionStatus) *persistence.Execution {
		t.Helper()
		seedTask(t, taskID, projectID)
		return &persistence.Execution{
			ID:         uniqueID("exec"),
			TaskID:     taskID,
			ProjectID:  projectID,
			WorkflowID: "wf-x",
			Status:     status,
			CreatedAt:  time.Now().UTC().Truncate(time.Millisecond),
			UpdatedAt:  time.Now().UTC().Truncate(time.Millisecond),
		}
	}
	// newExecForTask is the variant that reuses an already-seeded
	// task — for cases (GetByTaskID, SupersedeNonTerminalForTask)
	// where multiple executions share one task.
	newExecForTask := func(taskID, projectID string, status persistence.ExecutionStatus) *persistence.Execution {
		return &persistence.Execution{
			ID:         uniqueID("exec"),
			TaskID:     taskID,
			ProjectID:  projectID,
			WorkflowID: "wf-x",
			Status:     status,
			CreatedAt:  time.Now().UTC().Truncate(time.Millisecond),
			UpdatedAt:  time.Now().UTC().Truncate(time.Millisecond),
		}
	}

	t.Run("Create_then_Get_round_trips", func(t *testing.T) {
		e := newExec(t, uniqueID("task"), "proj-1", persistence.ExecutionStatusRunning)
		if err := repo.Create(ctx, e); err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, err := repo.Get(ctx, e.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.ID != e.ID || got.TaskID != e.TaskID || got.WorkflowID != e.WorkflowID {
			t.Errorf("round-trip mismatch: got %+v want %+v", got, e)
		}
	})

	t.Run("GetByTaskID_returns_most_recent", func(t *testing.T) {
		taskID := uniqueID("task")
		seedTask(t, taskID, "p1")
		first := newExecForTask(taskID, "p1", persistence.ExecutionStatusCompleted)
		if err := repo.Create(ctx, first); err != nil {
			t.Fatalf("Create first: %v", err)
		}
		time.Sleep(2 * time.Millisecond)
		second := newExecForTask(taskID, "p1", persistence.ExecutionStatusRunning)
		if err := repo.Create(ctx, second); err != nil {
			t.Fatalf("Create second: %v", err)
		}
		got, err := repo.GetByTaskID(ctx, taskID)
		if err != nil {
			t.Fatalf("GetByTaskID: %v", err)
		}
		// "Current or most recent" — implementations may prefer the
		// non-terminal row when present (the running one here) OR the
		// newest-created. Either is acceptable per the docstring; pin
		// the weaker invariant: SOME execution for this task is returned.
		if got == nil {
			t.Fatal("GetByTaskID returned nil for a task with executions")
		}
		if got.TaskID != taskID {
			t.Errorf("wrong task: got %s", got.TaskID)
		}
	})

	t.Run("GetByTaskIDs_batch_keys_absent_for_no_execution", func(t *testing.T) {
		t1 := uniqueID("task")
		t2 := uniqueID("task")
		tGhost := uniqueID("task") // no execution; tGhost intentionally
		// has no seeded task row either since the contract is
		// "absent from map when no execution row exists" — the
		// FK only matters at execution insert, not at lookup.
		_ = repo.Create(ctx, newExec(t, t1, "p", persistence.ExecutionStatusRunning))
		_ = repo.Create(ctx, newExec(t, t2, "p", persistence.ExecutionStatusCompleted))
		got, err := repo.GetByTaskIDs(ctx, []string{t1, t2, tGhost})
		if err != nil {
			t.Fatalf("GetByTaskIDs: %v", err)
		}
		// Contract: keys for tasks without an execution are ABSENT
		// (not zero-valued). The autonomy state builder relies on
		// `if _, ok := m[taskID]; ok` to distinguish "no row" from
		// "missing field".
		if _, ok := got[tGhost]; ok {
			t.Errorf("task with no execution must be absent, got: %+v", got[tGhost])
		}
		if _, ok := got[t1]; !ok {
			t.Errorf("task with execution must be present, missing: %s", t1)
		}
	})

	t.Run("UpdateStatus_persists_transition", func(t *testing.T) {
		e := newExec(t, uniqueID("task"), "p", persistence.ExecutionStatusRunning)
		if err := repo.Create(ctx, e); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := repo.UpdateStatus(ctx, e.ID, persistence.ExecutionStatusPaused); err != nil {
			t.Fatalf("UpdateStatus: %v", err)
		}
		got, _ := repo.Get(ctx, e.ID)
		if got.Status != persistence.ExecutionStatusPaused {
			t.Errorf("status = %s, want paused", got.Status)
		}
	})

	// Regression (2026-06-11): /ui/executions showed blank Started +
	// Duration for every row because started_at was NULL in the DB even
	// for completed runs. Root cause: the executor stamps started_at via
	// UpdateStatus(RUNNING) — DB-side only — then calls the full-row
	// Update(ctx, execution) to persist the resolved workflow_id while the
	// in-memory struct still has StartedAt == nil. The naive Update wrote
	// started_at = $param, clobbering the stamp back to NULL. Update must
	// preserve an already-set started_at when handed a nil (COALESCE),
	// since no caller ever intentionally resets started_at to NULL.
	t.Run("Update_preserves_started_at_stamped_by_UpdateStatus", func(t *testing.T) {
		e := newExec(t, uniqueID("task"), "p", persistence.ExecutionStatusPending)
		e.WorkflowID = "default-workflow"
		if err := repo.Create(ctx, e); err != nil {
			t.Fatalf("Create: %v", err)
		}
		// Executor step 1: RUNNING transition stamps started_at in the DB.
		if err := repo.UpdateStatus(ctx, e.ID, persistence.ExecutionStatusRunning); err != nil {
			t.Fatalf("UpdateStatus: %v", err)
		}
		stamped, _ := repo.Get(ctx, e.ID)
		if stamped.StartedAt == nil {
			t.Fatal("precondition: UpdateStatus(RUNNING) must stamp started_at")
		}
		// Executor step 2: persist the resolved workflow_id via a full-row
		// Update. The in-memory struct's StartedAt is still nil here.
		e.Status = persistence.ExecutionStatusRunning
		e.WorkflowID = "research"
		e.StartedAt = nil
		if err := repo.Update(ctx, e); err != nil {
			t.Fatalf("Update: %v", err)
		}
		got, _ := repo.Get(ctx, e.ID)
		if got.WorkflowID != "research" {
			t.Errorf("workflow_id = %q, want research — Update must still persist it", got.WorkflowID)
		}
		if got.StartedAt == nil {
			t.Error("started_at clobbered to NULL by full-row Update — /ui/executions Started+Duration go blank")
		}
	})

	t.Run("SaveStateSnapshot_then_Get_round_trips_snapshot_bytes", func(t *testing.T) {
		e := newExec(t, uniqueID("task"), "p", persistence.ExecutionStatusRunning)
		if err := repo.Create(ctx, e); err != nil {
			t.Fatalf("Create: %v", err)
		}
		snap := []byte(`{"step":"x","visit_counts":{"x":1}}`)
		if err := repo.SaveStateSnapshot(ctx, e.ID, snap, "x", []string{"a", "b"}); err != nil {
			t.Fatalf("SaveStateSnapshot: %v", err)
		}
		got, _ := repo.Get(ctx, e.ID)
		// Compare semantically — postgres JSONB normalizes
		// whitespace + key order, sqlite preserves the byte
		// representation. The contract is "the same JSON
		// document round-trips", not byte-equality.
		if !jsonEqual(t, got.StateSnapshot, snap) {
			t.Errorf("snapshot content lost: got %q want %q", got.StateSnapshot, snap)
		}
		if got.CurrentStepID == nil || *got.CurrentStepID != "x" {
			t.Errorf("current_step_id = %v, want x", got.CurrentStepID)
		}
	})

	t.Run("SetWorkflowSnapshot_GetWorkflowSnapshot_round_trip", func(t *testing.T) {
		e := newExec(t, uniqueID("task"), "p", persistence.ExecutionStatusRunning)
		if err := repo.Create(ctx, e); err != nil {
			t.Fatalf("Create: %v", err)
		}
		wf := []byte(`{"workflowId":"x","steps":{"a":{"type":"agent"}}}`)
		if err := repo.SetWorkflowSnapshot(ctx, e.ID, wf); err != nil {
			t.Fatalf("SetWorkflowSnapshot: %v", err)
		}
		got, err := repo.GetWorkflowSnapshot(ctx, e.ID)
		if err != nil {
			t.Fatalf("GetWorkflowSnapshot: %v", err)
		}
		if !jsonEqual(t, got, wf) {
			t.Errorf("workflow snapshot lost: got %q want %q", got, wf)
		}
	})

	t.Run("SetWorkflowSnapshot_empty_payload_is_no_op", func(t *testing.T) {
		e := newExec(t, uniqueID("task"), "p", persistence.ExecutionStatusRunning)
		if err := repo.Create(ctx, e); err != nil {
			t.Fatalf("Create: %v", err)
		}
		// Pre-populate via a real write so the no-op test has
		// something to NOT clobber.
		wf := []byte(`{"a":1}`)
		_ = repo.SetWorkflowSnapshot(ctx, e.ID, wf)
		// Empty/zero-byte payload must NOT overwrite the existing
		// snapshot — docstring contract.
		if err := repo.SetWorkflowSnapshot(ctx, e.ID, nil); err != nil {
			t.Fatalf("SetWorkflowSnapshot nil: %v", err)
		}
		got, _ := repo.GetWorkflowSnapshot(ctx, e.ID)
		if !jsonEqual(t, got, wf) {
			t.Errorf("empty-payload SetWorkflowSnapshot clobbered: got %q want %q", got, wf)
		}
	})

	t.Run("RecordCompletion_marks_terminal_with_result", func(t *testing.T) {
		e := newExec(t, uniqueID("task"), "p", persistence.ExecutionStatusRunning)
		if err := repo.Create(ctx, e); err != nil {
			t.Fatalf("Create: %v", err)
		}
		result := []byte(`{"ok":true}`)
		if err := repo.RecordCompletion(ctx, e.ID, result); err != nil {
			t.Fatalf("RecordCompletion: %v", err)
		}
		got, _ := repo.Get(ctx, e.ID)
		if got.Status != persistence.ExecutionStatusCompleted {
			t.Errorf("status after completion = %s", got.Status)
		}
		if !jsonEqual(t, got.Result, result) {
			t.Errorf("result content lost: got %q want %q", got.Result, result)
		}
		if got.CompletedAt == nil {
			t.Error("completed_at must be set on RecordCompletion")
		}
	})

	t.Run("RecordFailure_marks_terminal_with_error", func(t *testing.T) {
		e := newExec(t, uniqueID("task"), "p", persistence.ExecutionStatusRunning)
		if err := repo.Create(ctx, e); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := repo.RecordFailure(ctx, e.ID, "kaboom", "transient_5xx"); err != nil {
			t.Fatalf("RecordFailure: %v", err)
		}
		got, _ := repo.Get(ctx, e.ID)
		if got.Status != persistence.ExecutionStatusFailed {
			t.Errorf("status after failure = %s", got.Status)
		}
		if got.ErrorMessage == nil || *got.ErrorMessage != "kaboom" {
			t.Errorf("error_message lost: %v", got.ErrorMessage)
		}
		if got.ErrorCode == nil || *got.ErrorCode != "transient_5xx" {
			t.Errorf("error_code lost: %v", got.ErrorCode)
		}
	})

	t.Run("SupersedeNonTerminalForTask_sweeps_orphans", func(t *testing.T) {
		// Reproduce the 2026-05-22 orphan-PAUSED incident: a task has
		// one PAUSED exec (awaiting_children) + one RUNNING. Both
		// must flip to CANCELLED with error_code=superseded_*.
		taskID := uniqueID("task")
		seedTask(t, taskID, "p")
		paused := newExecForTask(taskID, "p", persistence.ExecutionStatusPaused)
		_ = repo.Create(ctx, paused)
		running := newExecForTask(taskID, "p", persistence.ExecutionStatusRunning)
		_ = repo.Create(ctx, running)

		n, err := repo.SupersedeNonTerminalForTask(ctx, taskID)
		if err != nil {
			t.Fatalf("SupersedeNonTerminalForTask: %v", err)
		}
		if n != 2 {
			t.Errorf("expected 2 rows superseded, got %d", n)
		}
		gotPaused, _ := repo.Get(ctx, paused.ID)
		gotRunning, _ := repo.Get(ctx, running.ID)
		if gotPaused.Status != persistence.ExecutionStatusCancelled {
			t.Errorf("paused exec status = %s, want cancelled", gotPaused.Status)
		}
		if gotRunning.Status != persistence.ExecutionStatusCancelled {
			t.Errorf("running exec status = %s, want cancelled", gotRunning.Status)
		}
		// Idempotent — second call sweeps nothing.
		n2, _ := repo.SupersedeNonTerminalForTask(ctx, taskID)
		if n2 != 0 {
			t.Errorf("second sweep should be idempotent, swept %d", n2)
		}
	})

	t.Run("SupersedeOrphanPausedExecutions_global_backstop", func(t *testing.T) {
		// Orphan: PAUSED exec whose parent task is already terminal — must
		// be finalized regardless of which path stranded it.
		orphanTask := uniqueID("task")
		seedTask(t, orphanTask, "p")
		if err := taskRepo.UpdateStatus(ctx, orphanTask, persistence.TaskStatusCompleted); err != nil {
			t.Fatalf("mark orphan task terminal: %v", err)
		}
		orphan := newExecForTask(orphanTask, "p", persistence.ExecutionStatusPaused)
		_ = repo.Create(ctx, orphan)

		// Control: PAUSED exec whose task is still NON-terminal (a
		// legitimate pause) — must be left alone.
		liveTask := uniqueID("task")
		seedTask(t, liveTask, "p") // stays QUEUED
		live := newExecForTask(liveTask, "p", persistence.ExecutionStatusPaused)
		_ = repo.Create(ctx, live)

		n, err := repo.SupersedeOrphanPausedExecutions(ctx)
		if err != nil {
			t.Fatalf("SupersedeOrphanPausedExecutions: %v", err)
		}
		if n < 1 {
			t.Errorf("expected >=1 orphan swept, got %d", n)
		}
		gotOrphan, _ := repo.Get(ctx, orphan.ID)
		if gotOrphan.Status != persistence.ExecutionStatusCancelled {
			t.Errorf("orphan status = %s, want CANCELLED", gotOrphan.Status)
		}
		if gotOrphan.ErrorCode == nil || *gotOrphan.ErrorCode != "superseded_orphan_paused" {
			t.Errorf("orphan error_code = %v, want superseded_orphan_paused", gotOrphan.ErrorCode)
		}
		if gotOrphan.CompletedAt == nil {
			t.Error("orphan completed_at must be set")
		}
		gotLive, _ := repo.Get(ctx, live.ID)
		if gotLive.Status != persistence.ExecutionStatusPaused {
			t.Errorf("legitimate PAUSED exec (non-terminal task) = %s, want still PAUSED", gotLive.Status)
		}

		// Idempotent: the now-CANCELLED orphan is no longer PAUSED, the
		// live one is still protected by its non-terminal task.
		gotLive2, _ := repo.Get(ctx, live.ID)
		if gotLive2.Status != persistence.ExecutionStatusPaused {
			t.Errorf("control exec changed on idempotency re-check: %s", gotLive2.Status)
		}
	})

	t.Run("CountByStatus_aggregates_per_project", func(t *testing.T) {
		project := uniqueID("proj")
		other := uniqueID("proj-other")
		_ = repo.Create(ctx, newExec(t, uniqueID("t"), project, persistence.ExecutionStatusRunning))
		_ = repo.Create(ctx, newExec(t, uniqueID("t"), project, persistence.ExecutionStatusRunning))
		_ = repo.Create(ctx, newExec(t, uniqueID("t"), project, persistence.ExecutionStatusCompleted))
		_ = repo.Create(ctx, newExec(t, uniqueID("t"), other, persistence.ExecutionStatusRunning))

		counts, err := repo.CountByStatus(ctx, project)
		if err != nil {
			t.Fatalf("CountByStatus: %v", err)
		}
		if counts[persistence.ExecutionStatusRunning] != 2 {
			t.Errorf("running count = %d, want 2 (other project's 1 must be excluded)", counts[persistence.ExecutionStatusRunning])
		}
		if counts[persistence.ExecutionStatusCompleted] != 1 {
			t.Errorf("completed count = %d, want 1", counts[persistence.ExecutionStatusCompleted])
		}
	})

	t.Run("List_filter_by_task_isolates_rows", func(t *testing.T) {
		taskID := uniqueID("task")
		seedTask(t, taskID, "p")
		for i := 0; i < 2; i++ {
			_ = repo.Create(ctx, newExecForTask(taskID, "p", persistence.ExecutionStatusCompleted))
		}
		_ = repo.Create(ctx, newExec(t, uniqueID("other-task"), "p", persistence.ExecutionStatusCompleted))
		got, err := repo.List(ctx, persistence.ExecutionFilter{TaskID: &taskID, PageSize: 50})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("task filter returned %d rows, want 2", len(got))
		}
	})
}

// RunExtractedDocumentSuite exercises persistence.ExtractedDocumentRepository.
// Covers the contract the document-ingest workflow (B-7) depends on:
// idempotent Upsert keyed on (source_artifact, extractor name, version),
// GetByArtifact's "most recent" promise, and the per-project listing
// that powers the /ui/projects/{id}/documents page.
func RunExtractedDocumentSuite(t *testing.T, repo persistence.ExtractedDocumentRepository) {
	t.Helper()
	ctx := context.Background()

	newDoc := func(projectID, artifactID, extName, extVersion string) *persistence.ExtractedDocument {
		return &persistence.ExtractedDocument{
			ID:               uniqueID("extdoc"),
			ProjectID:        projectID,
			SourceArtifactID: artifactID,
			ExtractorName:    extName,
			ExtractorVersion: extVersion,
			MimeType:         "text/markdown",
			StoragePath:      "/tmp/" + uniqueID("p"),
			MetadataBlob:     []byte(`{"title":"t"}`),
			OutlineBlob:      []byte(`[]`),
			SectionCount:     1,
			TotalTextBytes:   100,
			Status:           persistence.ExtractedDocumentStatusOK,
			ExtractedAt:      time.Now().UTC().Truncate(time.Millisecond),
		}
	}

	t.Run("Upsert_then_Get_round_trips", func(t *testing.T) {
		d := newDoc("p1", uniqueID("art"), "vornik-extract-text", "0.1.0")
		if err := repo.Upsert(ctx, d); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		got, err := repo.Get(ctx, d.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.ID != d.ID || got.SourceArtifactID != d.SourceArtifactID {
			t.Errorf("round-trip mismatch: %+v vs %+v", got, d)
		}
		if got.SectionCount != 1 {
			t.Errorf("section_count lost: %d", got.SectionCount)
		}
	})

	t.Run("Upsert_idempotent_on_same_source_extractor_version_triple", func(t *testing.T) {
		artifactID := uniqueID("art")
		// First upsert: creates a row.
		d1 := newDoc("p1", artifactID, "vornik-extract-text", "0.1.0")
		if err := repo.Upsert(ctx, d1); err != nil {
			t.Fatalf("Upsert d1: %v", err)
		}
		// Second upsert with same (artifact, extractor, version) and
		// different secondary fields: must UPDATE in place,
		// preserving the ID per the docstring "row's ID is
		// preserved on update so existing memory-chunk provenance
		// pointers remain valid".
		d2 := newDoc("p1", artifactID, "vornik-extract-text", "0.1.0")
		d2.SectionCount = 5
		if err := repo.Upsert(ctx, d2); err != nil {
			t.Fatalf("Upsert d2: %v", err)
		}
		// d2's row should match d1's ID (preserved); the original
		// row's section_count is updated.
		got, err := repo.GetByArtifact(ctx, artifactID)
		if err != nil {
			t.Fatalf("GetByArtifact: %v", err)
		}
		if got.ID != d1.ID {
			t.Errorf("ID NOT preserved across upsert: d1.ID=%s, got.ID=%s", d1.ID, got.ID)
		}
	})

	t.Run("Upsert_different_version_creates_new_row", func(t *testing.T) {
		artifactID := uniqueID("art")
		d1 := newDoc("p1", artifactID, "vornik-extract-pdf", "1.0.0")
		d2 := newDoc("p1", artifactID, "vornik-extract-pdf", "1.1.0") // bumped
		_ = repo.Upsert(ctx, d1)
		_ = repo.Upsert(ctx, d2)
		if d1.ID == d2.ID {
			t.Errorf("different versions must produce different rows; both IDs = %s", d1.ID)
		}
	})

	t.Run("GetByArtifact_nil_for_unknown_artifact", func(t *testing.T) {
		got, err := repo.GetByArtifact(ctx, uniqueID("ghost"))
		if err != nil {
			t.Fatalf("GetByArtifact on unknown artifact must NOT error: %v", err)
		}
		if got != nil {
			t.Errorf("expected nil for missing artifact, got %+v", got)
		}
	})

	t.Run("ListByProject_orders_extracted_at_DESC", func(t *testing.T) {
		project := uniqueID("proj")
		for i := 0; i < 3; i++ {
			d := newDoc(project, uniqueID("art"), "vornik-extract-text", "0.1.0")
			d.ExtractedAt = time.Now().UTC().Add(time.Duration(i) * time.Second)
			_ = repo.Upsert(ctx, d)
		}
		got, err := repo.ListByProject(ctx, project, 10)
		if err != nil {
			t.Fatalf("ListByProject: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("ListByProject returned %d, want 3", len(got))
		}
		// Newest-first per docstring.
		for i := 0; i < len(got)-1; i++ {
			if got[i].ExtractedAt.Before(got[i+1].ExtractedAt) {
				t.Errorf("ListByProject not DESC by extracted_at at index %d", i)
			}
		}
	})

	t.Run("Delete_removes_row", func(t *testing.T) {
		d := newDoc("p", uniqueID("art"), "vornik-extract-text", "0.1.0")
		_ = repo.Upsert(ctx, d)
		if err := repo.Delete(ctx, d.ID); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		// Contract: Get returns (nil, nil) on missing rows
		// (unlike most other repos which return ErrNotFound) —
		// pinned at both backends per scanExtractedDocument /
		// scanExtractedDocumentSQLite. Operator-facing tools
		// distinguish "no extraction yet" from "extraction failed"
		// via this nil-check.
		got, err := repo.Get(ctx, d.ID)
		if err != nil {
			t.Fatalf("Get on deleted row should return (nil, nil), got err: %v", err)
		}
		if got != nil {
			t.Fatalf("Get on deleted row should return nil, got: %+v", got)
		}
	})
}

// RunMemoryIngestAuditSuite exercises persistence.MemoryIngestAuditRepository.
// Covers the per-call ingest audit trail introduced by LLD-22 (migration
// 74) and the B-16 List(filter) extension. Both paths matter for the
// /ui/admin/memory-audit ingest panel + the SaaS-tier "show every
// deposit by API key X this week" compliance surface.
func RunMemoryIngestAuditSuite(t *testing.T, repo persistence.MemoryIngestAuditRepository) {
	t.Helper()
	ctx := context.Background()

	newAudit := func(project, source string, decision string) *persistence.MemoryIngestAudit {
		actorKind := "companion:claude-code"
		actorID := uniqueID("akey")
		repoScope := "github.com/owner/repo"
		return &persistence.MemoryIngestAudit{
			ID:             uniqueID("ming"),
			ProjectID:      project,
			ActorKind:      &actorKind,
			ActorID:        &actorID,
			SourceName:     source,
			ContentHash:    uniqueID("hash"),
			ContentBytes:   1234,
			Decision:       decision,
			ChunksAdmitted: 1,
			IngestedAt:     time.Now().UTC().Truncate(time.Millisecond),
			RepoScope:      &repoScope,
		}
	}

	t.Run("Record_then_ListByProject_round_trips", func(t *testing.T) {
		project := uniqueID("proj")
		a := newAudit(project, "decision-foo.md", persistence.MemoryIngestAuditAdmitted)
		if err := repo.Record(ctx, a); err != nil {
			t.Fatalf("Record: %v", err)
		}
		got, err := repo.ListByProject(ctx, project, 10)
		if err != nil {
			t.Fatalf("ListByProject: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("ListByProject returned %d, want 1", len(got))
		}
		if got[0].SourceName != "decision-foo.md" {
			t.Errorf("source_name lost: %s", got[0].SourceName)
		}
		if got[0].ActorKind == nil || *got[0].ActorKind != "companion:claude-code" {
			t.Errorf("actor_kind lost: %v", got[0].ActorKind)
		}
		if got[0].RepoScope == nil || *got[0].RepoScope != "github.com/owner/repo" {
			t.Errorf("repo_scope lost: %v", got[0].RepoScope)
		}
	})

	t.Run("ListByProject_orders_newest_first", func(t *testing.T) {
		project := uniqueID("proj")
		for i := 0; i < 3; i++ {
			a := newAudit(project, fmt.Sprintf("doc-%d", i), persistence.MemoryIngestAuditAdmitted)
			a.IngestedAt = time.Now().UTC().Add(time.Duration(i) * time.Second)
			_ = repo.Record(ctx, a)
		}
		got, _ := repo.ListByProject(ctx, project, 10)
		if len(got) != 3 {
			t.Fatalf("got %d rows", len(got))
		}
		for i := 0; i < len(got)-1; i++ {
			if got[i].IngestedAt.Before(got[i+1].IngestedAt) {
				t.Errorf("ListByProject not DESC at index %d", i)
			}
		}
	})

	t.Run("ListByProject_limit_caps", func(t *testing.T) {
		project := uniqueID("proj")
		for i := 0; i < 5; i++ {
			_ = repo.Record(ctx, newAudit(project, "x", persistence.MemoryIngestAuditAdmitted))
		}
		got, _ := repo.ListByProject(ctx, project, 2)
		if len(got) != 2 {
			t.Errorf("limit=2 returned %d", len(got))
		}
	})

	t.Run("List_rejects_unbounded_pagesize", func(t *testing.T) {
		_, err := repo.List(ctx, persistence.MemoryIngestAuditFilter{})
		if err == nil {
			t.Fatal("List with PageSize=0 must error (would otherwise unbound the admin page)")
		}
	})

	t.Run("List_filter_by_decision_isolates_rows", func(t *testing.T) {
		project := uniqueID("proj")
		_ = repo.Record(ctx, newAudit(project, "admitted-doc", persistence.MemoryIngestAuditAdmitted))
		_ = repo.Record(ctx, newAudit(project, "quarantined-doc", persistence.MemoryIngestAuditQuarantined))
		_ = repo.Record(ctx, newAudit(project, "rejected-doc", persistence.MemoryIngestAuditRejected))

		got, err := repo.List(ctx, persistence.MemoryIngestAuditFilter{
			ProjectID: project,
			Decision:  persistence.MemoryIngestAuditQuarantined,
			PageSize:  10,
		})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("decision=quarantined returned %d rows, want 1", len(got))
		}
		if got[0].Decision != persistence.MemoryIngestAuditQuarantined {
			t.Errorf("wrong decision returned: %s", got[0].Decision)
		}
	})

	t.Run("List_filter_by_actor_kind_isolates_rows", func(t *testing.T) {
		project := uniqueID("proj")
		a1 := newAudit(project, "companion-doc", persistence.MemoryIngestAuditAdmitted)
		a2 := newAudit(project, "agent-doc", persistence.MemoryIngestAuditAdmitted)
		agentKind := "agent"
		a2.ActorKind = &agentKind
		_ = repo.Record(ctx, a1)
		_ = repo.Record(ctx, a2)

		got, _ := repo.List(ctx, persistence.MemoryIngestAuditFilter{
			ProjectID: project,
			ActorKind: "companion:claude-code",
			PageSize:  10,
		})
		if len(got) != 1 {
			t.Errorf("actor_kind filter returned %d, want 1", len(got))
		}
	})

	t.Run("List_filter_by_repo_scope_isolates_rows", func(t *testing.T) {
		project := uniqueID("proj")
		a1 := newAudit(project, "scoped-x", persistence.MemoryIngestAuditAdmitted)
		a2 := newAudit(project, "scoped-y", persistence.MemoryIngestAuditAdmitted)
		other := "github.com/other/repo"
		a2.RepoScope = &other
		_ = repo.Record(ctx, a1)
		_ = repo.Record(ctx, a2)

		got, _ := repo.List(ctx, persistence.MemoryIngestAuditFilter{
			ProjectID: project,
			RepoScope: "github.com/owner/repo",
			PageSize:  10,
		})
		if len(got) != 1 {
			t.Errorf("repo_scope filter returned %d, want 1", len(got))
		}
	})
}

// seedHealingTrigger inserts a workflow_healing_triggers row so the
// candidate suite can satisfy the FK trigger_id →
// workflow_healing_triggers(id). Returns the seeded trigger ID.
func seedHealingTrigger(t *testing.T, ctx context.Context, repo persistence.WorkflowHealingTriggerRepository, projectID, workflowID string) string {
	t.Helper()
	now := time.Now().UTC()
	tr := &persistence.HealingTrigger{
		ProjectID:            projectID,
		WorkflowID:           workflowID,
		TriggerClass:         persistence.HealingTriggerFailureRateSpike,
		BaselineStart:        now.Add(-48 * time.Hour),
		BaselineEnd:          now.Add(-24 * time.Hour),
		ComparisonStart:      now.Add(-24 * time.Hour),
		ComparisonEnd:        now,
		MetricName:           "failure_rate",
		BaselineValue:        0.1,
		ComparisonValue:      0.4,
		ThresholdValue:       0.25,
		EvidenceExecutionIDs: []string{uniqueID("exec"), uniqueID("exec")},
	}
	if err := repo.Insert(ctx, tr); err != nil {
		t.Fatalf("seedHealingTrigger: %v", err)
	}
	return tr.ID
}

// RunHealingCandidateSuite exercises the
// WorkflowHealingCandidateRepository contract: insert/get round-trip,
// list filtering, status transitions, and the promote/reject
// terminal guards. Takes triggerRepo to seed the FK parent
// (workflow_healing_candidates.trigger_id →
// workflow_healing_triggers(id) on Postgres).
func RunHealingCandidateSuite(t *testing.T, repo persistence.WorkflowHealingCandidateRepository, triggerRepo persistence.WorkflowHealingTriggerRepository) {
	t.Helper()
	ctx := context.Background()
	project := uniqueID("proj")
	workflow := "wf-" + uniqueID("x")
	triggerID := seedHealingTrigger(t, ctx, triggerRepo, project, workflow)

	newCandidate := func() *persistence.HealingCandidate {
		return &persistence.HealingCandidate{
			TriggerID:           triggerID,
			ProjectID:           project,
			WorkflowID:          workflow,
			ProposalID:          uniqueID("wpr"),
			BaselineGenomeHash:  "abc123",
			CandidateGenomeHash: "def456",
			CandidateClass:      persistence.HealingCandidateRetryBudget,
			ProposalDiff:        "--- a\n+++ b",
			Motivation:          "retry loop on step build",
			ExpectedEffect:      "fewer retries",
			RiskLevel:           persistence.HealingRiskLow,
		}
	}

	t.Run("Get_unknown_returns_ErrNotFound", func(t *testing.T) {
		_, err := repo.Get(ctx, uniqueID("missing"))
		if !errors.Is(err, persistence.ErrNotFound) {
			t.Fatalf("Get unknown: want ErrNotFound, got %v", err)
		}
	})

	t.Run("Insert_then_Get_roundtrips_and_defaults", func(t *testing.T) {
		c := newCandidate()
		if err := repo.Insert(ctx, c); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		if c.ID == "" {
			t.Fatal("Insert did not stamp ID")
		}
		got, err := repo.Get(ctx, c.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Status != persistence.HealingCandidateDraft {
			t.Errorf("default status = %q, want draft", got.Status)
		}
		if got.ProposalID != c.ProposalID || got.WorkflowID != workflow {
			t.Errorf("roundtrip mismatch: %+v", got)
		}
		if got.CandidateClass != persistence.HealingCandidateRetryBudget {
			t.Errorf("class = %q", got.CandidateClass)
		}
		if got.RiskLevel != persistence.HealingRiskLow {
			t.Errorf("risk = %q", got.RiskLevel)
		}
		if got.PromotedAt != nil {
			t.Errorf("PromotedAt should be nil before promotion")
		}
	})

	t.Run("Insert_rejects_missing_required_fields", func(t *testing.T) {
		if err := repo.Insert(ctx, &persistence.HealingCandidate{ProjectID: project}); err == nil {
			t.Fatal("Insert with missing trigger_id/proposal_id should error")
		}
	})

	t.Run("List_filters_by_trigger_and_status", func(t *testing.T) {
		c := newCandidate()
		if err := repo.Insert(ctx, c); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		got, err := repo.List(ctx, persistence.HealingCandidateListFilter{TriggerID: triggerID})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) < 1 {
			t.Fatalf("List by trigger returned %d rows", len(got))
		}
		for _, row := range got {
			if row.TriggerID != triggerID {
				t.Errorf("List leaked row from trigger %q", row.TriggerID)
			}
		}
		// Status filter: nothing is promoted yet.
		promoted, err := repo.List(ctx, persistence.HealingCandidateListFilter{
			TriggerID: triggerID,
			Status:    persistence.HealingCandidatePromoted,
		})
		if err != nil {
			t.Fatalf("List(promoted): %v", err)
		}
		if len(promoted) != 0 {
			t.Errorf("expected no promoted rows, got %d", len(promoted))
		}
	})

	t.Run("SetStatus_moves_through_trial_states", func(t *testing.T) {
		c := newCandidate()
		_ = repo.Insert(ctx, c)
		if err := repo.SetStatus(ctx, c.ID, persistence.HealingCandidateTrialRunning); err != nil {
			t.Fatalf("SetStatus(running): %v", err)
		}
		got, _ := repo.Get(ctx, c.ID)
		if got.Status != persistence.HealingCandidateTrialRunning {
			t.Errorf("status = %q, want trial_running", got.Status)
		}
		if err := repo.SetStatus(ctx, c.ID, persistence.HealingCandidateTrialPassed); err != nil {
			t.Fatalf("SetStatus(passed): %v", err)
		}
	})

	t.Run("SetStatus_rejects_terminal_target", func(t *testing.T) {
		c := newCandidate()
		_ = repo.Insert(ctx, c)
		if err := repo.SetStatus(ctx, c.ID, persistence.HealingCandidatePromoted); err == nil {
			t.Fatal("SetStatus to a terminal status should error")
		}
	})

	t.Run("SetStatus_unknown_returns_ErrNotFound", func(t *testing.T) {
		err := repo.SetStatus(ctx, uniqueID("missing"), persistence.HealingCandidateTrialRunning)
		if !errors.Is(err, persistence.ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})

	t.Run("Promote_stamps_and_is_terminal", func(t *testing.T) {
		c := newCandidate()
		_ = repo.Insert(ctx, c)
		if err := repo.Promote(ctx, c.ID, "operator@example.com"); err != nil {
			t.Fatalf("Promote: %v", err)
		}
		got, _ := repo.Get(ctx, c.ID)
		if got.Status != persistence.HealingCandidatePromoted {
			t.Errorf("status = %q, want promoted", got.Status)
		}
		if got.PromotedBy != "operator@example.com" {
			t.Errorf("promoted_by = %q", got.PromotedBy)
		}
		if got.PromotedAt == nil {
			t.Errorf("promoted_at not stamped")
		}
		// Second promote on a terminal row is a no-op → ErrNotFound.
		if err := repo.Promote(ctx, c.ID, "other@example.com"); !errors.Is(err, persistence.ErrNotFound) {
			t.Errorf("re-promote: want ErrNotFound, got %v", err)
		}
		// Reject after promote also refuses.
		if err := repo.Reject(ctx, c.ID); !errors.Is(err, persistence.ErrNotFound) {
			t.Errorf("reject-after-promote: want ErrNotFound, got %v", err)
		}
	})

	t.Run("Reject_is_terminal", func(t *testing.T) {
		c := newCandidate()
		_ = repo.Insert(ctx, c)
		if err := repo.Reject(ctx, c.ID); err != nil {
			t.Fatalf("Reject: %v", err)
		}
		got, _ := repo.Get(ctx, c.ID)
		if got.Status != persistence.HealingCandidateRejected {
			t.Errorf("status = %q, want rejected", got.Status)
		}
		if err := repo.SetStatus(ctx, c.ID, persistence.HealingCandidateTrialRunning); !errors.Is(err, persistence.ErrNotFound) {
			t.Errorf("set-status-after-reject: want ErrNotFound, got %v", err)
		}
	})
}

// RunHealingTrialSuite exercises the WorkflowHealingTrialRepository
// contract: insert/get round-trip with JSONB blobs, list-by-candidate
// ordering, and the Finish transition. Takes triggerRepo +
// candidateRepo to seed the FK chain
// (trial.candidate_id → candidate.id → trigger.id).
func RunHealingTrialSuite(t *testing.T, repo persistence.WorkflowHealingTrialRepository, candidateRepo persistence.WorkflowHealingCandidateRepository, triggerRepo persistence.WorkflowHealingTriggerRepository) {
	t.Helper()
	ctx := context.Background()
	project := uniqueID("proj")
	workflow := "wf-" + uniqueID("x")
	triggerID := seedHealingTrigger(t, ctx, triggerRepo, project, workflow)

	cand := &persistence.HealingCandidate{
		TriggerID:      triggerID,
		ProjectID:      project,
		WorkflowID:     workflow,
		ProposalID:     uniqueID("wpr"),
		CandidateClass: persistence.HealingCandidateVerifierInsertion,
	}
	if err := candidateRepo.Insert(ctx, cand); err != nil {
		t.Fatalf("seed candidate: %v", err)
	}

	t.Run("Get_unknown_returns_ErrNotFound", func(t *testing.T) {
		_, err := repo.Get(ctx, uniqueID("missing"))
		if !errors.Is(err, persistence.ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})

	t.Run("Insert_then_Get_roundtrips_jsonb_and_defaults", func(t *testing.T) {
		tr := &persistence.HealingTrial{
			CandidateID:          cand.ID,
			Mode:                 persistence.HealingTrialModeStatic,
			EvidenceExecutionIDs: []string{"exec-1", "exec-2"},
		}
		if err := repo.Insert(ctx, tr); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		if tr.ID == "" {
			t.Fatal("Insert did not stamp ID")
		}
		got, err := repo.Get(ctx, tr.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Verdict != persistence.HealingTrialPending {
			t.Errorf("default verdict = %q, want pending", got.Verdict)
		}
		if got.Mode != persistence.HealingTrialModeStatic {
			t.Errorf("mode = %q", got.Mode)
		}
		if len(got.EvidenceExecutionIDs) != 2 || got.EvidenceExecutionIDs[0] != "exec-1" {
			t.Errorf("evidence roundtrip = %v", got.EvidenceExecutionIDs)
		}
		if got.BaselineSummary != "{}" || got.Scorecard != "{}" {
			t.Errorf("empty blobs should default to '{}', got baseline=%q scorecard=%q", got.BaselineSummary, got.Scorecard)
		}
		if got.FinishedAt != nil {
			t.Errorf("FinishedAt should be nil before Finish")
		}
	})

	t.Run("Insert_rejects_missing_required_fields", func(t *testing.T) {
		if err := repo.Insert(ctx, &persistence.HealingTrial{CandidateID: cand.ID}); err == nil {
			t.Fatal("Insert without mode should error")
		}
		if err := repo.Insert(ctx, &persistence.HealingTrial{Mode: persistence.HealingTrialModeStatic}); err == nil {
			t.Fatal("Insert without candidate_id should error")
		}
	})

	t.Run("Finish_stamps_verdict_and_blobs", func(t *testing.T) {
		tr := &persistence.HealingTrial{
			CandidateID: cand.ID,
			Mode:        persistence.HealingTrialModeReplay,
		}
		_ = repo.Insert(ctx, tr)
		bsum := `{"runs":5,"successes":4}`
		csum := `{"runs":5,"successes":5}`
		score := `{"success_delta":0.2,"verdict":"passed"}`
		if err := repo.Finish(ctx, tr.ID, persistence.HealingTrialPassed, bsum, csum, score); err != nil {
			t.Fatalf("Finish: %v", err)
		}
		got, _ := repo.Get(ctx, tr.ID)
		if got.Verdict != persistence.HealingTrialPassed {
			t.Errorf("verdict = %q, want passed", got.Verdict)
		}
		if got.FinishedAt == nil {
			t.Errorf("finished_at not stamped")
		}
		if !jsonEqual(t, []byte(got.CandidateSummary), []byte(csum)) {
			t.Errorf("candidate_summary = %q, want %q", got.CandidateSummary, csum)
		}
		if !jsonEqual(t, []byte(got.Scorecard), []byte(score)) {
			t.Errorf("scorecard = %q, want %q", got.Scorecard, score)
		}
	})

	t.Run("Finish_unknown_returns_ErrNotFound", func(t *testing.T) {
		err := repo.Finish(ctx, uniqueID("missing"), persistence.HealingTrialFailed, "{}", "{}", "{}")
		if !errors.Is(err, persistence.ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})

	t.Run("ListByCandidate_newest_first", func(t *testing.T) {
		c2 := &persistence.HealingCandidate{
			TriggerID:      triggerID,
			ProjectID:      project,
			WorkflowID:     workflow,
			ProposalID:     uniqueID("wpr"),
			CandidateClass: persistence.HealingCandidateArchitect,
		}
		_ = candidateRepo.Insert(ctx, c2)
		first := &persistence.HealingTrial{CandidateID: c2.ID, Mode: persistence.HealingTrialModeStatic}
		_ = repo.Insert(ctx, first)
		second := &persistence.HealingTrial{CandidateID: c2.ID, Mode: persistence.HealingTrialModeReplay}
		second.StartedAt = time.Now().UTC().Add(1 * time.Second)
		_ = repo.Insert(ctx, second)

		got, err := repo.ListByCandidate(ctx, c2.ID)
		if err != nil {
			t.Fatalf("ListByCandidate: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("want 2 trials, got %d", len(got))
		}
		if got[0].ID != second.ID {
			t.Errorf("newest-first ordering broken: got[0]=%s want %s", got[0].ID, second.ID)
		}
		for _, row := range got {
			if row.CandidateID != c2.ID {
				t.Errorf("ListByCandidate leaked row from %q", row.CandidateID)
			}
		}
	})
}

// RunIdentityRepositorySuite exercises the persistence.IdentityRepository
// contract: user/group/membership/project writes plus the resolver's
// single-round-trip join (ResolvePrincipalRows) consumed by
// internal/authz. The matrix codifies the Task-4 slice-review pins:
// the revoked_at filter is real (revoked binding → zero rows), the
// admin/user mixed shape (admin row carries a genuinely-nil ProjectID),
// literal '*' project passthrough, wholesale project replacement, and
// the best-effort touch. Fixtures use uniqueID-stamped IDs (prefixes
// user/grp/grpname/uident) so concurrent runs never collide; see the
// repotest_integration_test.go cleanup hook for the matching purge.
func RunIdentityRepositorySuite(t *testing.T, repo persistence.IdentityRepository) {
	t.Helper()
	ctx := context.Background()

	mkUser := func(t *testing.T) *persistence.User {
		t.Helper()
		u := &persistence.User{
			ID:          uniqueID("user"),
			DisplayName: "test user",
			CreatedAt:   time.Now().UTC(),
		}
		if err := repo.CreateUser(ctx, u); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		return u
	}
	mkGroup := func(t *testing.T, role string, projects ...string) *persistence.Group {
		t.Helper()
		g := &persistence.Group{
			ID:        uniqueID("grp"),
			Name:      uniqueID("grpname"),
			Role:      role,
			CreatedAt: time.Now().UTC(),
		}
		if err := repo.CreateGroup(ctx, g); err != nil {
			t.Fatalf("CreateGroup: %v", err)
		}
		if len(projects) > 0 {
			if err := repo.SetGroupProjects(ctx, g.ID, projects); err != nil {
				t.Fatalf("SetGroupProjects: %v", err)
			}
		}
		return g
	}
	bind := func(t *testing.T, userID, channel, externalID string) {
		t.Helper()
		if err := repo.BindIdentity(ctx, &persistence.UserIdentity{
			ID:         uniqueID("uident"),
			UserID:     userID,
			Channel:    channel,
			ExternalID: externalID,
			Display:    "bound",
			CreatedAt:  time.Now().UTC(),
		}); err != nil {
			t.Fatalf("BindIdentity: %v", err)
		}
	}
	// projectSet collects the non-nil ProjectIDs from a resolver row set.
	projectSet := func(rows []persistence.PrincipalRow) map[string]bool {
		out := map[string]bool{}
		for _, r := range rows {
			if r.ProjectID != nil {
				out[*r.ProjectID] = true
			}
		}
		return out
	}

	t.Run("resolve_multi_group_union", func(t *testing.T) {
		u := mkUser(t)
		projA := uniqueID("proj")
		projB := uniqueID("proj")
		g1 := mkGroup(t, "user", projA)
		g2 := mkGroup(t, "user", projB, projA)
		if err := repo.AddGroupMember(ctx, g1.ID, u.ID); err != nil {
			t.Fatalf("AddGroupMember g1: %v", err)
		}
		if err := repo.AddGroupMember(ctx, g2.ID, u.ID); err != nil {
			t.Fatalf("AddGroupMember g2: %v", err)
		}
		ch, ext := "google", uniqueID("ext")
		bind(t, u.ID, ch, ext)

		rows, err := repo.ResolvePrincipalRows(ctx, ch, ext)
		if err != nil {
			t.Fatalf("ResolvePrincipalRows: %v", err)
		}
		got := projectSet(rows)
		if !got[projA] || !got[projB] {
			t.Fatalf("union project set %v missing %s/%s", got, projA, projB)
		}
		for _, r := range rows {
			if r.UserID != u.ID {
				t.Errorf("row leaked from user %q", r.UserID)
			}
			if r.Role == nil || *r.Role != "user" {
				t.Errorf("expected role=user, got %v", r.Role)
			}
		}
	})

	t.Run("resolve_unknown_identity_zero_rows", func(t *testing.T) {
		rows, err := repo.ResolvePrincipalRows(ctx, "google", uniqueID("ghost"))
		if err != nil {
			t.Fatalf("ResolvePrincipalRows: %v", err)
		}
		if len(rows) != 0 {
			t.Fatalf("unknown identity yielded %d rows, want 0", len(rows))
		}
	})

	t.Run("revoked_identity_not_resolved", func(t *testing.T) {
		// Carry-forward (a) [SECURITY PIN]: a revoked binding must
		// yield zero resolver rows, full stop.
		u := mkUser(t)
		g := mkGroup(t, "user", uniqueID("proj"))
		if err := repo.AddGroupMember(ctx, g.ID, u.ID); err != nil {
			t.Fatalf("AddGroupMember: %v", err)
		}
		ch, ext := "telegram", uniqueID("ext")
		bind(t, u.ID, ch, ext)
		// Sanity: active binding resolves before revoke.
		if rows, err := repo.ResolvePrincipalRows(ctx, ch, ext); err != nil || len(rows) == 0 {
			t.Fatalf("pre-revoke resolve: rows=%d err=%v", len(rows), err)
		}
		if err := repo.RevokeIdentity(ctx, ch, ext); err != nil {
			t.Fatalf("RevokeIdentity: %v", err)
		}
		rows, err := repo.ResolvePrincipalRows(ctx, ch, ext)
		if err != nil {
			t.Fatalf("post-revoke resolve: %v", err)
		}
		if len(rows) != 0 {
			t.Fatalf("revoked binding yielded %d rows, want 0", len(rows))
		}
	})

	t.Run("rebind_after_revoke_repoints_row", func(t *testing.T) {
		// Carry-forward (e): revoke, rebind to a DIFFERENT user, the
		// resolver must see the new user.
		u1 := mkUser(t)
		u2 := mkUser(t)
		ch, ext := "github", uniqueID("ext")
		bind(t, u1.ID, ch, ext)
		if err := repo.RevokeIdentity(ctx, ch, ext); err != nil {
			t.Fatalf("RevokeIdentity: %v", err)
		}
		// Rebind the same (channel, external_id) to u2 — BindIdentity
		// upserts on the UNIQUE constraint, clearing revoked_at.
		bind(t, u2.ID, ch, ext)
		rows, err := repo.ResolvePrincipalRows(ctx, ch, ext)
		if err != nil {
			t.Fatalf("ResolvePrincipalRows: %v", err)
		}
		if len(rows) == 0 {
			t.Fatal("rebound identity yielded zero rows")
		}
		if rows[0].UserID != u2.ID {
			t.Fatalf("rebind repoint failed: rows[0].UserID=%q want %q", rows[0].UserID, u2.ID)
		}
	})

	t.Run("disabled_user_flag_surfaces", func(t *testing.T) {
		// Carry-forward (d): the repo layer does NOT error on a
		// disabled user — it returns non-empty rows with Disabled=true.
		// authz is the layer that denies; the repo just reports.
		u := mkUser(t)
		g := mkGroup(t, "user", uniqueID("proj"))
		if err := repo.AddGroupMember(ctx, g.ID, u.ID); err != nil {
			t.Fatalf("AddGroupMember: %v", err)
		}
		if err := repo.SetUserDisabled(ctx, u.ID, true); err != nil {
			t.Fatalf("SetUserDisabled: %v", err)
		}
		ch, ext := "slack", uniqueID("ext")
		bind(t, u.ID, ch, ext)
		rows, err := repo.ResolvePrincipalRows(ctx, ch, ext)
		if err != nil {
			t.Fatalf("ResolvePrincipalRows: %v", err)
		}
		if len(rows) == 0 {
			t.Fatal("disabled user yielded zero rows; repo must not filter")
		}
		for _, r := range rows {
			if !r.Disabled {
				t.Errorf("expected Disabled=true, got false for user %q", r.UserID)
			}
		}
	})

	t.Run("group_role_check_constraint", func(t *testing.T) {
		// CHECK (role IN ('admin','user')) — "superuser" must be
		// rejected by the DB.
		g := &persistence.Group{
			ID:        uniqueID("grp"),
			Name:      uniqueID("grpname"),
			Role:      "superuser",
			CreatedAt: time.Now().UTC(),
		}
		if err := repo.CreateGroup(ctx, g); err == nil {
			t.Fatal("expected CHECK-constraint error for role=superuser, got nil")
		}
	})

	t.Run("admin_plus_user_groups_row_shape", func(t *testing.T) {
		// Carry-forward (b): a user in an admin-role group AND a
		// user-role group with two projects yields exactly 3 rows —
		// one admin row with a genuinely-nil ProjectID, and two user
		// rows each with a non-nil ProjectID.
		u := mkUser(t)
		admin := mkGroup(t, "admin") // admin groups ignore group_projects
		projA := uniqueID("proj")
		projB := uniqueID("proj")
		user := mkGroup(t, "user", projA, projB)
		if err := repo.AddGroupMember(ctx, admin.ID, u.ID); err != nil {
			t.Fatalf("AddGroupMember admin: %v", err)
		}
		if err := repo.AddGroupMember(ctx, user.ID, u.ID); err != nil {
			t.Fatalf("AddGroupMember user: %v", err)
		}
		ch, ext := "google", uniqueID("ext")
		bind(t, u.ID, ch, ext)
		rows, err := repo.ResolvePrincipalRows(ctx, ch, ext)
		if err != nil {
			t.Fatalf("ResolvePrincipalRows: %v", err)
		}
		if len(rows) != 3 {
			t.Fatalf("admin+user shape: got %d rows, want 3 (1 admin×NULL + 2 user×project)", len(rows))
		}
		var adminRows, userRows int
		gotProjects := map[string]bool{}
		for _, r := range rows {
			if r.Role == nil {
				t.Fatalf("unexpected nil role in mixed-group shape: %+v", r)
			}
			switch *r.Role {
			case "admin":
				adminRows++
				if r.ProjectID != nil {
					t.Errorf("admin row ProjectID must be genuinely nil, got %q", *r.ProjectID)
				}
			case "user":
				userRows++
				if r.ProjectID == nil {
					t.Errorf("user row ProjectID must be non-nil")
				} else {
					gotProjects[*r.ProjectID] = true
				}
			default:
				t.Errorf("unexpected role %q", *r.Role)
			}
		}
		if adminRows != 1 {
			t.Errorf("admin rows = %d, want 1", adminRows)
		}
		if userRows != 2 {
			t.Errorf("user rows = %d, want 2", userRows)
		}
		if !gotProjects[projA] || !gotProjects[projB] {
			t.Errorf("user project set %v missing %s/%s", gotProjects, projA, projB)
		}
	})

	t.Run("star_project_literal_passthrough", func(t *testing.T) {
		// Carry-forward (c): project_id '*' is a literal string, not
		// an expansion — the resolver row carries it verbatim.
		u := mkUser(t)
		g := mkGroup(t, "user", "*")
		if err := repo.AddGroupMember(ctx, g.ID, u.ID); err != nil {
			t.Fatalf("AddGroupMember: %v", err)
		}
		ch, ext := "google", uniqueID("ext")
		bind(t, u.ID, ch, ext)
		rows, err := repo.ResolvePrincipalRows(ctx, ch, ext)
		if err != nil {
			t.Fatalf("ResolvePrincipalRows: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("star group: got %d rows, want 1", len(rows))
		}
		if rows[0].ProjectID == nil || *rows[0].ProjectID != "*" {
			t.Fatalf("expected literal '*' ProjectID, got %v", rows[0].ProjectID)
		}
	})

	t.Run("set_group_projects_wholesale_replace", func(t *testing.T) {
		// SetGroupProjects is wholesale, not additive: setting {c}
		// after {a,b} must leave only c visible to the resolver.
		u := mkUser(t)
		projA := uniqueID("proj")
		projB := uniqueID("proj")
		projC := uniqueID("proj")
		g := mkGroup(t, "user", projA, projB)
		if err := repo.AddGroupMember(ctx, g.ID, u.ID); err != nil {
			t.Fatalf("AddGroupMember: %v", err)
		}
		if err := repo.SetGroupProjects(ctx, g.ID, []string{projC}); err != nil {
			t.Fatalf("SetGroupProjects replace: %v", err)
		}
		ch, ext := "google", uniqueID("ext")
		bind(t, u.ID, ch, ext)
		rows, err := repo.ResolvePrincipalRows(ctx, ch, ext)
		if err != nil {
			t.Fatalf("ResolvePrincipalRows: %v", err)
		}
		got := projectSet(rows)
		if len(got) != 1 || !got[projC] {
			t.Fatalf("wholesale replace: project set %v, want only %s", got, projC)
		}
	})

	t.Run("resolve_user_by_id", func(t *testing.T) {
		// Session-path resolver: ResolveUserPrincipalRows keys on
		// users.id directly (no binding join — login already resolved
		// it). Same joined-row contract; assert non-empty + project
		// surfaces.
		u := mkUser(t)
		proj := uniqueID("proj")
		g := mkGroup(t, "user", proj)
		if err := repo.AddGroupMember(ctx, g.ID, u.ID); err != nil {
			t.Fatalf("AddGroupMember: %v", err)
		}
		rows, err := repo.ResolveUserPrincipalRows(ctx, u.ID)
		if err != nil {
			t.Fatalf("ResolveUserPrincipalRows: %v", err)
		}
		if len(rows) == 0 {
			t.Fatal("ResolveUserPrincipalRows yielded zero rows for an existing user")
		}
		for _, r := range rows {
			if r.UserID != u.ID {
				t.Errorf("row leaked from user %q", r.UserID)
			}
		}
		if got := projectSet(rows); !got[proj] {
			t.Fatalf("project set %v missing %s", got, proj)
		}
	})

	t.Run("resolve_user_by_id_unknown_zero_rows", func(t *testing.T) {
		rows, err := repo.ResolveUserPrincipalRows(ctx, uniqueID("ghostuser"))
		if err != nil {
			t.Fatalf("ResolveUserPrincipalRows: %v", err)
		}
		if len(rows) != 0 {
			t.Fatalf("unknown user yielded %d rows, want 0", len(rows))
		}
	})

	t.Run("touch_last_used_best_effort", func(t *testing.T) {
		// TouchIdentityLastUsed on a revoked binding affects zero rows
		// but must return nil — it is fired async and stale is harmless.
		u := mkUser(t)
		ch, ext := "telegram", uniqueID("ext")
		bind(t, u.ID, ch, ext)
		if err := repo.RevokeIdentity(ctx, ch, ext); err != nil {
			t.Fatalf("RevokeIdentity: %v", err)
		}
		if err := repo.TouchIdentityLastUsed(ctx, ch, ext); err != nil {
			t.Fatalf("TouchIdentityLastUsed on revoked binding must be nil, got %v", err)
		}
	})
}
