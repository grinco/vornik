package controlplane

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// fakeInstinctRetireStore is a local minimal fake for the narrow
// instinctRetireStore interface — deliberately NOT one of the big
// InstinctRepository fakes living in internal/api, internal/ui, etc.
type fakeInstinctRetireStore struct {
	instincts map[string]*persistence.Instinct

	getCalls    int
	retireCalls int
	unretireArg struct{ id, priorStatus string }

	unretireErr error
}

func newFakeInstinctRetireStore() *fakeInstinctRetireStore {
	return &fakeInstinctRetireStore{instincts: map[string]*persistence.Instinct{}}
}

func (f *fakeInstinctRetireStore) Get(_ context.Context, id string) (*persistence.Instinct, error) {
	f.getCalls++
	inst, ok := f.instincts[id]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	return inst, nil
}

func (f *fakeInstinctRetireStore) Retire(_ context.Context, id string) error {
	f.retireCalls++
	inst, ok := f.instincts[id]
	if !ok {
		return persistence.ErrNotFound
	}
	inst.Status = persistence.InstinctStatusRetired
	return nil
}

func (f *fakeInstinctRetireStore) UnretireTo(_ context.Context, id, priorStatus string) error {
	f.unretireArg = struct{ id, priorStatus string }{id, priorStatus}
	if f.unretireErr != nil {
		return f.unretireErr
	}
	inst, ok := f.instincts[id]
	if !ok {
		return persistence.ErrNotFound
	}
	if inst.Status != persistence.InstinctStatusRetired {
		return persistence.ErrNotFound
	}
	inst.Status = priorStatus
	return nil
}

func retireProposal(evidence, preApplySnapshot string) *persistence.ControlPlaneProposal {
	return &persistence.ControlPlaneProposal{
		ID: "cpp-retire-1", Kind: persistence.ProposalKindInstinctRetire,
		BlastRadius: persistence.ProposalScopeProject, Status: persistence.ProposalStatusApproved,
		Evidence: evidence, PreApplySnapshot: preApplySnapshot,
	}
}

func TestInstinctRetireApplier_ApplyRetires(t *testing.T) {
	store := newFakeInstinctRetireStore()
	store.instincts["i1"] = &persistence.Instinct{ID: "i1", Status: persistence.InstinctStatusActive}
	a := &InstinctRetireApplier{Instincts: store}
	p := retireProposal(`{"instinct_id":"i1"}`, "")

	snap, err := a.Apply(context.Background(), p)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if store.instincts["i1"].Status != persistence.InstinctStatusRetired {
		t.Errorf("instinct not retired: %+v", store.instincts["i1"])
	}
	want := `{"instinct_id":"i1","prior_status":"active"}`
	if snap != want {
		t.Errorf("snapshot = %q, want %q", snap, want)
	}
}

func TestInstinctRetireApplier_ApplyMissingEvidence(t *testing.T) {
	store := newFakeInstinctRetireStore()
	store.instincts["i1"] = &persistence.Instinct{ID: "i1", Status: persistence.InstinctStatusActive}
	a := &InstinctRetireApplier{Instincts: store}
	p := retireProposal(`{}`, "")

	if _, err := a.Apply(context.Background(), p); err == nil {
		t.Fatal("expected error for missing instinct_id in evidence")
	}
	if store.getCalls != 0 {
		t.Errorf("store must not be called when evidence is missing instinct_id, got %d Get calls", store.getCalls)
	}
}

func TestInstinctRetireApplier_ApplyAlreadyRetired(t *testing.T) {
	store := newFakeInstinctRetireStore()
	store.instincts["i1"] = &persistence.Instinct{ID: "i1", Status: persistence.InstinctStatusRetired}
	a := &InstinctRetireApplier{Instincts: store}
	p := retireProposal(`{"instinct_id":"i1"}`, "")

	if _, err := a.Apply(context.Background(), p); err == nil {
		t.Fatal("expected error for already-retired instinct")
	}
	if store.retireCalls != 0 {
		t.Errorf("Retire must not be called on an already-retired instinct, got %d calls", store.retireCalls)
	}
}

func TestInstinctRetireApplier_RollbackRestores(t *testing.T) {
	store := newFakeInstinctRetireStore()
	store.instincts["i1"] = &persistence.Instinct{ID: "i1", Status: persistence.InstinctStatusRetired}
	a := &InstinctRetireApplier{Instincts: store}
	p := retireProposal("", `{"instinct_id":"i1","prior_status":"active"}`)

	if err := a.Rollback(context.Background(), p); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if store.unretireArg.id != "i1" || store.unretireArg.priorStatus != "active" {
		t.Errorf("UnretireTo called with %+v, want i1/active", store.unretireArg)
	}
	if store.instincts["i1"].Status != persistence.InstinctStatusActive {
		t.Errorf("instinct not restored: %+v", store.instincts["i1"])
	}
}

func TestInstinctRetireApplier_RollbackFailSafe(t *testing.T) {
	store := newFakeInstinctRetireStore()
	store.unretireErr = persistence.ErrNotFound
	a := &InstinctRetireApplier{Instincts: store}
	p := retireProposal("", `{"instinct_id":"i1","prior_status":"active"}`)

	err := a.Rollback(context.Background(), p)
	if err == nil {
		t.Fatal("expected fail-safe rollback error")
	}
	if !errors.Is(err, persistence.ErrNotFound) {
		t.Errorf("rollback error must wrap ErrNotFound, got %v", err)
	}
}
