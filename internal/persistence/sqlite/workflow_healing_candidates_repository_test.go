package sqlite

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// The SQLite candidate/trial repos are Postgres-only stubs (same
// Phase B discipline as triggers + overrides). These tests pin the
// contract: every method returns the unsupported sentinel so the
// interface is satisfied in the test build and a wiring mistake that
// silently routes the genome layer at SQLite fails loudly.

func TestSQLiteHealingCandidateRepository_AllUnsupported(t *testing.T) {
	r := NewWorkflowHealingCandidateRepository(nil)
	ctx := context.Background()

	if err := r.Insert(ctx, &persistence.HealingCandidate{}); !errors.Is(err, ErrSQLiteHealingCandidatesUnsupported) {
		t.Errorf("Insert: %v", err)
	}
	if _, err := r.Get(ctx, "id"); !errors.Is(err, ErrSQLiteHealingCandidatesUnsupported) {
		t.Errorf("Get: %v", err)
	}
	if _, err := r.List(ctx, persistence.HealingCandidateListFilter{}); !errors.Is(err, ErrSQLiteHealingCandidatesUnsupported) {
		t.Errorf("List: %v", err)
	}
	if err := r.SetStatus(ctx, "id", persistence.HealingCandidateTrialRunning); !errors.Is(err, ErrSQLiteHealingCandidatesUnsupported) {
		t.Errorf("SetStatus: %v", err)
	}
	if err := r.Promote(ctx, "id", "op"); !errors.Is(err, ErrSQLiteHealingCandidatesUnsupported) {
		t.Errorf("Promote: %v", err)
	}
	if err := r.Reject(ctx, "id"); !errors.Is(err, ErrSQLiteHealingCandidatesUnsupported) {
		t.Errorf("Reject: %v", err)
	}
}

func TestSQLiteHealingTrialRepository_AllUnsupported(t *testing.T) {
	r := NewWorkflowHealingTrialRepository(nil)
	ctx := context.Background()

	if err := r.Insert(ctx, &persistence.HealingTrial{}); !errors.Is(err, ErrSQLiteHealingTrialsUnsupported) {
		t.Errorf("Insert: %v", err)
	}
	if _, err := r.Get(ctx, "id"); !errors.Is(err, ErrSQLiteHealingTrialsUnsupported) {
		t.Errorf("Get: %v", err)
	}
	if _, err := r.ListByCandidate(ctx, "cand"); !errors.Is(err, ErrSQLiteHealingTrialsUnsupported) {
		t.Errorf("ListByCandidate: %v", err)
	}
	if err := r.Finish(ctx, "id", persistence.HealingTrialPassed, "{}", "{}", "{}"); !errors.Is(err, ErrSQLiteHealingTrialsUnsupported) {
		t.Errorf("Finish: %v", err)
	}
}

// Compile-time assertions that the stubs satisfy the repository
// interfaces — the real value of keeping the stubs in the tree.
var (
	_ persistence.WorkflowHealingCandidateRepository = (*WorkflowHealingCandidateRepository)(nil)
	_ persistence.WorkflowHealingTrialRepository     = (*WorkflowHealingTrialRepository)(nil)
)
