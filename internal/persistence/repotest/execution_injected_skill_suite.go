package repotest

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// RunExecutionInjectedSkillSuite is the backend-agnostic contract for
// persistence.ExecutionInjectedSkillRepository — the (execution_id,
// skill_id) association the maturity engine uses to credit a "worked"
// signal to the skills injected into a successful execution
// (LLD 2026-07-07-knowledge-skill-learning-loop-design). Fixtures use
// the "exec-" hyphen prefix so the purge sweep can isolate them from
// production "exec_" (underscore) IDs.
func RunExecutionInjectedSkillSuite(t *testing.T, repo persistence.ExecutionInjectedSkillRepository) {
	t.Helper()
	ctx := context.Background()

	t.Run("Record_then_ListByExecution_round_trips", func(t *testing.T) {
		if err := repo.Record(ctx, "exec-1", "skill-a"); err != nil {
			t.Fatalf("Record a: %v", err)
		}
		if err := repo.Record(ctx, "exec-1", "skill-b"); err != nil {
			t.Fatalf("Record b: %v", err)
		}
		got, err := repo.ListByExecution(ctx, "exec-1")
		if err != nil {
			t.Fatalf("ListByExecution: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("expected 2 skills, got %v", got)
		}
		seen := map[string]bool{got[0]: true}
		if len(got) > 1 {
			seen[got[1]] = true
		}
		if !seen["skill-a"] || !seen["skill-b"] {
			t.Fatalf("round-trip mismatch: %v", got)
		}
	})

	t.Run("Record_is_idempotent_on_duplicate_pair", func(t *testing.T) {
		if err := repo.Record(ctx, "exec-2", "skill-x"); err != nil {
			t.Fatalf("Record 1: %v", err)
		}
		if err := repo.Record(ctx, "exec-2", "skill-x"); err != nil {
			t.Fatalf("Record 2 (idempotent): %v", err)
		}
		got, _ := repo.ListByExecution(ctx, "exec-2")
		if len(got) != 1 {
			t.Fatalf("duplicate pair must not double-insert: %v", got)
		}
	})

	t.Run("ListByExecution_unknown_is_empty", func(t *testing.T) {
		got, err := repo.ListByExecution(ctx, "exec-no-such")
		if err != nil {
			t.Fatalf("ListByExecution: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("expected empty, got %v", got)
		}
	})

	t.Run("distinct_executions_are_isolated", func(t *testing.T) {
		if err := repo.Record(ctx, "exec-3a", "skill-only-3a"); err != nil {
			t.Fatalf("Record: %v", err)
		}
		if err := repo.Record(ctx, "exec-3b", "skill-only-3b"); err != nil {
			t.Fatalf("Record: %v", err)
		}
		got, _ := repo.ListByExecution(ctx, "exec-3a")
		if len(got) != 1 || got[0] != "skill-only-3a" {
			t.Fatalf("execution isolation broken: %v", got)
		}
	})
}
