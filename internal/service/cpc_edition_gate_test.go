package service

import (
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/storage"
)

// stubCPCRepo satisfies persistence.CrossProjectCallRepository via an embedded
// (nil) interface — enough to be a non-nil value for the wiring-gate test; its
// methods are never called here.
type stubCPCRepo struct {
	persistence.CrossProjectCallRepository
}

// TestCrossProjectCallRepo_EditionGated — inter-project orchestration is
// Enterprise-only (editions matrix, CPC row). The container wires the CPC
// ledger only when providers.CrossProject is set; Community gets nil, which
// makes the executor's call_project step fail closed with
// errCrossProjectDisabled and leaves the CPC admin/UI surfaces without a ledger.
func TestCrossProjectCallRepo_EditionGated(t *testing.T) {
	repo := &stubCPCRepo{}

	ee := &Container{providers: ProviderSet{CrossProject: true}, repos: &storage.Repositories{CrossProjectCalls: repo}}
	if ee.crossProjectCallRepo() == nil {
		t.Fatal("Enterprise (CrossProject=true) must wire the CPC ledger")
	}

	ce := &Container{providers: ProviderSet{CrossProject: false}, repos: &storage.Repositories{CrossProjectCalls: repo}}
	if ce.crossProjectCallRepo() != nil {
		t.Fatal("Community (CrossProject=false) must NOT wire the CPC ledger — cross-project orchestration is Enterprise-only")
	}
}
