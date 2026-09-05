package repotest

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// RunProjectFirstSeenSuite pins the contract behind project_created telemetry:
// MarkSeen reports first-time EXACTLY once per project id, whatever the backend.
//
// It runs on both lanes because the counter it gates is wrong in opposite
// directions if either half drifts. A backend that always says "first" turns a
// lifecycle counter into a restart counter — the failure the persisted marker
// exists to prevent. One that never says "first" silently restores the original
// gap, where the file-drop path (the way every project in this installation was
// created) emitted nothing at all.
//
// Design: https://docs.vornik.io
func RunProjectFirstSeenSuite(t *testing.T, repo persistence.ProjectFirstSeenRepository) {
	t.Helper()
	ctx := context.Background()

	t.Run("FirstCallIsFirst_SecondIsNot", func(t *testing.T) {
		id := uniqueID("proj-fs")
		first, err := repo.MarkSeen(ctx, id, "config_file")
		if err != nil {
			t.Fatalf("MarkSeen: %v", err)
		}
		if !first {
			t.Fatal("the FIRST observation of a project did not report first — the file-drop " +
				"path emits nothing again")
		}
		again, err := repo.MarkSeen(ctx, id, "config_file")
		if err != nil {
			t.Fatalf("MarkSeen (second): %v", err)
		}
		if again {
			t.Fatal("a second observation reported first — every daemon restart re-emits and " +
				"the counter becomes a restart counter")
		}
	})

	t.Run("RepeatedObservationsStayNotFirst", func(t *testing.T) {
		id := uniqueID("proj-fs")
		if _, err := repo.MarkSeen(ctx, id, "config_file"); err != nil {
			t.Fatalf("MarkSeen: %v", err)
		}
		// Ten reloads, as a busy day would produce.
		for i := 0; i < 10; i++ {
			first, err := repo.MarkSeen(ctx, id, "config_file")
			if err != nil {
				t.Fatalf("MarkSeen (reload %d): %v", i, err)
			}
			if first {
				t.Fatalf("reload %d reported first", i)
			}
		}
	})

	t.Run("DistinctProjectsEachReportFirstOnce", func(t *testing.T) {
		a, b := uniqueID("proj-fs"), uniqueID("proj-fs")
		for _, id := range []string{a, b} {
			first, err := repo.MarkSeen(ctx, id, "config_file")
			if err != nil {
				t.Fatalf("MarkSeen(%s): %v", id, err)
			}
			if !first {
				t.Fatalf("%s did not report first; the marker is not per-project", id)
			}
		}
	})

	t.Run("EmptyProjectIDIsRefused", func(t *testing.T) {
		// An empty id would claim ONE shared marker for every unnamed project:
		// the first emits and the rest are silently swallowed, which is a worse
		// under-count than the one being fixed.
		if _, err := repo.MarkSeen(ctx, "", "config_file"); err == nil {
			t.Fatal("an empty project id was accepted")
		}
	})
}
