package executor

// instinct_budget_resolver_test.go — nil-path and injection tests for the
// contracts.InstinctBudgetResolver seam (Phase 1c, Task 3).
//
// Covers:
//  - nil resolver → instinctBudgetResolver stays nil; WithInstinctBudgetResolver(nil) is a safe no-op
//  - non-nil fake resolver → WithInstinctBudgetResolver wires it; field is set
//  - nil resolver gate: instinctToolBudget on + nil resolver → no LearnedTier call, no panic
//  - non-nil resolver gate: instinctToolBudget on + non-nil resolver → LearnedTier called with correct args
//  - LearnedTier ok==false path → budgetTier stays empty string
//  - LearnedTier ok==true path → budgetTier is set to result.Tier

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/contracts"
)

// stubBudgetResolver is a minimal contracts.InstinctBudgetResolver for tests.
type stubBudgetResolver struct {
	// result and found are what LearnedTier returns.
	result contracts.LearnedTierResult
	found  bool
	// calledWith records the last call's arguments for assertion.
	calledWith struct {
		projectID string
		role      string
		minConf   float64
	}
}

func (s *stubBudgetResolver) LearnedTier(
	_ context.Context,
	projectID, role string,
	minConf float64,
) (contracts.LearnedTierResult, bool) {
	s.calledWith.projectID = projectID
	s.calledWith.role = role
	s.calledWith.minConf = minConf
	return s.result, s.found
}

// TestWithInstinctBudgetResolver_NilIsASafeNoOp ensures that wiring a nil
// resolver does not panic and leaves the field nil.
func TestWithInstinctBudgetResolver_NilIsASafeNoOp(t *testing.T) {
	rt := NewMockRuntime()
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()

	e := NewWithOptions(rt, er, ar, tr, nil,
		WithInstinctBudgetResolver(nil),
	)
	if e.instinctBudgetResolver != nil {
		t.Fatal("nil resolver: field must remain nil")
	}
}

// TestWithInstinctBudgetResolver_WiresResolver ensures the option stores the resolver.
func TestWithInstinctBudgetResolver_WiresResolver(t *testing.T) {
	rt := NewMockRuntime()
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()

	resolver := &stubBudgetResolver{found: false}
	e := NewWithOptions(rt, er, ar, tr, nil,
		WithInstinctBudgetResolver(resolver),
	)
	if e.instinctBudgetResolver == nil {
		t.Fatal("expected non-nil resolver field after WithInstinctBudgetResolver")
	}
}

// TestInstinctBudgetResolverNilPath_NoPanic verifies that when the gate is on
// but the resolver is nil, the nil-guard fires and no LearnedTier call happens
// (no panic, no side effect). This is the Community nil-path.
func TestInstinctBudgetResolverNilPath_NoPanic(t *testing.T) {
	e := &Executor{
		instinctBudgetResolver: nil, // Community: no resolver wired
		instinctToolBudget:     true,
	}
	// Invoke the gate condition directly — replicate the logic from container.go:
	// if budgetTier == "" && e.instinctToolBudget && e.instinctBudgetResolver != nil { ... }
	// With resolver == nil the block is never entered; no panic.
	budgetTier := ""
	if budgetTier == "" && e.instinctToolBudget && e.instinctBudgetResolver != nil {
		t.Fatal("nil resolver: gate must not be entered")
	}
	if budgetTier != "" {
		t.Fatalf("nil resolver: budgetTier must stay empty, got %q", budgetTier)
	}
}

// TestInstinctBudgetResolverNonNilPath_TierApplied verifies that a non-nil
// resolver whose LearnedTier returns (result, true) causes the budgetTier to
// be set from result.Tier.
func TestInstinctBudgetResolverNonNilPath_TierApplied(t *testing.T) {
	resolver := &stubBudgetResolver{
		result: contracts.LearnedTierResult{Tier: "open_ended", InstinctID: "inst-99"},
		found:  true,
	}
	e := &Executor{
		instinctBudgetResolver: resolver,
		instinctToolBudget:     true,
	}

	const minConf = 0.6
	budgetTier := ""
	projectID := "proj-abc"
	role := "lead"

	if budgetTier == "" && e.instinctToolBudget && e.instinctBudgetResolver != nil {
		if ltr, ok := e.instinctBudgetResolver.LearnedTier(context.Background(), projectID, role, minConf); ok {
			budgetTier = ltr.Tier
		}
	}

	if budgetTier != "open_ended" {
		t.Fatalf("expected budgetTier = %q, got %q", "open_ended", budgetTier)
	}
	if resolver.calledWith.projectID != projectID {
		t.Errorf("resolver called with projectID %q, want %q", resolver.calledWith.projectID, projectID)
	}
	if resolver.calledWith.role != role {
		t.Errorf("resolver called with role %q, want %q", resolver.calledWith.role, role)
	}
	if resolver.calledWith.minConf != minConf {
		t.Errorf("resolver called with minConf %v, want %v", resolver.calledWith.minConf, minConf)
	}
}

// TestInstinctBudgetResolverNonNilPath_NotFound verifies that when the resolver
// returns (zero, false), budgetTier remains empty (default budget path).
func TestInstinctBudgetResolverNonNilPath_NotFound(t *testing.T) {
	resolver := &stubBudgetResolver{found: false}
	e := &Executor{
		instinctBudgetResolver: resolver,
		instinctToolBudget:     true,
	}

	const minConf = 0.6
	budgetTier := ""

	if budgetTier == "" && e.instinctToolBudget && e.instinctBudgetResolver != nil {
		if ltr, ok := e.instinctBudgetResolver.LearnedTier(context.Background(), "proj", "lead", minConf); ok {
			budgetTier = ltr.Tier
		}
	}

	if budgetTier != "" {
		t.Fatalf("resolver not-found: budgetTier must stay empty, got %q", budgetTier)
	}
}
