package sqlite

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// The SQLite stub never reaches the DB — its semantics are encoded
// in the methods themselves. Tests pin the contract so a future
// "let's just wire it up on SQLite too" change has to update them.

func TestWorkflowProposalStub_Insert_ReturnsNotFound(t *testing.T) {
	r := NewWorkflowProposalRepository(nil)
	err := r.Insert(context.Background(), &persistence.WorkflowProposal{ID: "x", WorkflowID: "y"})
	if !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestWorkflowProposalStub_Get_ReturnsNotFound(t *testing.T) {
	r := NewWorkflowProposalRepository(nil)
	if _, err := r.Get(context.Background(), "wpr-1"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestWorkflowProposalStub_List_ReturnsEmpty(t *testing.T) {
	r := NewWorkflowProposalRepository(nil)
	got, err := r.List(context.Background(), persistence.WorkflowProposalFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("List should return [] not nil so JSON encodes []")
	}
	if len(got) != 0 {
		t.Fatalf("want empty, got %d", len(got))
	}
}

func TestWorkflowProposalStub_WritesAreNoOps(t *testing.T) {
	r := NewWorkflowProposalRepository(nil)
	ctx := context.Background()
	if err := r.Decide(ctx, "x", persistence.WorkflowProposalStatusApproved, "v", ""); err != nil {
		t.Errorf("Decide should no-op, got %v", err)
	}
	if err := r.MarkApplied(ctx, "x", "abc"); err != nil {
		t.Errorf("MarkApplied should no-op, got %v", err)
	}
	if err := r.MarkRolledBack(ctx, "x", "abc"); err != nil {
		t.Errorf("MarkRolledBack should no-op, got %v", err)
	}
}
