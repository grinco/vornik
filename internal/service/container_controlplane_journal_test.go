package service

import (
	"context"
	"testing"
)

// Regression: audit 2026-09-15 CA-19 — "The Durable Config Apply Engine Is Not
// Wired Into Production".
//
// Both storage backends construct Repositories.ApplyJournal, and
// internal/controlplane carries a complete journal implementation with unit
// tests that inject it by hand. newProposalApplier never assigned it, so
// every apply the daemon actually performed took the legacy in-memory
// reverse path: no PREPARED row before the first write, no multi-file read
// set check, no writer fence, and nothing for a restart to recover from —
// while feature verification reported a durable journal confirmed.
//
// The unit tests that pass a Journal in explicitly cannot catch this: they
// exercise a path the daemon never reached. The assertion has to be made
// against the PRODUCTION constructor.
func TestNewProposalApplier_WiresTheDurableJournal(t *testing.T) {
	c := newProposalApplierContainer(t)
	if c.repos.ApplyJournal == nil {
		t.Fatal("fixture: the storage layer must provide an ApplyJournal repository")
	}
	engine := c.newProposalApplier()
	if engine == nil {
		t.Fatal("newProposalApplier returned nil")
	}
	if engine.Journal == nil {
		t.Fatal("ApplyEngine.Journal is nil: the daemon would take the legacy in-memory reverse path, with no durable pre-images and nothing to recover after a crash")
	}
	if engine.WriterEpoch == nil {
		t.Fatal("ApplyEngine.WriterEpoch is nil: the journal row would record epoch 0 and the fence could not detect a superseded writer")
	}
	if engine.LeaderGate == nil {
		t.Fatal("ApplyEngine.LeaderGate is nil: a process-local mutex is not a writer fence across replicas (journal LLD §1.2)")
	}
	if engine.VerifyGeneration == nil {
		t.Fatal("ApplyEngine.VerifyGeneration is nil: a reload that returns nil but leaves the old generation active would be recorded as APPLIED (journal LLD §1.3)")
	}
}

// Startup recovery must actually run: an unreconciled journal leaves a
// half-applied bundle in place with a ledger that says APPROVED.
func TestContainer_ReconcilesTheApplyJournalAtStartup(t *testing.T) {
	c := newProposalApplierContainer(t)
	if err := c.ReconcileConfigApplyJournal(context.Background()); err != nil {
		t.Fatalf("startup reconcile failed: %v", err)
	}
}
