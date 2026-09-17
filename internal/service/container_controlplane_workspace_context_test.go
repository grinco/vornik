package service

import (
	"context"
	"path/filepath"
	"testing"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/controlplane"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/storage"
)

// newProposalApplierContainer builds the minimal Container the applier
// constructor needs, leaving Config for the caller to set (or not).
func newProposalApplierContainer(t *testing.T) *Container {
	t.Helper()
	db, err := sqlite.Connect(context.Background(), sqlite.DefaultConfig())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return &Container{
		ConfigPath: filepath.Join(t.TempDir(), "config.yaml"),
		repos: &storage.Repositories{
			Proposals: sqlite.NewProposalRepository(db.DB),
			// The journal and the leader locks are what production
			// constructs; a fixture without them cannot see whether the
			// applier attaches them (audit 2026-09-15 CA-19).
			ApplyJournal: sqlite.NewApplyJournalRepository(db.DB),
			LeaderLocks:  sqlite.NewLeaderLockRepository(db.DB),
		},
	}
}

// TestNewProposalApplierSurvivesNilConfig pins the nil-Config guard on the
// workspace_context KindApplier wiring. Regression: the first cut of the
// 2026-09-13 config-assistant workspace PROJECT_CONTEXT.md bridge (design
// §6.4a) read c.Config.Runtime.ProjectWorkspacePath unguarded, and every
// caller that builds a Container without a Config — the existing
// TestProposalApplierValidateChangeRefusesTrading among them — panicked with
// a nil pointer dereference instead of building an engine.
func TestNewProposalApplierSurvivesNilConfig(t *testing.T) {
	c := newProposalApplierContainer(t)
	if c.Config != nil {
		t.Fatal("fixture must leave Config nil")
	}
	engine := c.newProposalApplier()
	if engine == nil {
		t.Fatal("newProposalApplier must build an engine with a nil Config")
	}
	if _, ok := engine.KindAppliers[persistence.ProposalKindWorkspaceContext]; ok {
		t.Error("workspace_context applier must not be registered without a configured workspace root")
	}
}

// TestNewProposalApplierWiresWorkspaceContext asserts the other half: with a
// runtime workspace path configured, the workspace_context kind IS registered
// and carries that root, so an approved virtual PROJECT_CONTEXT.md proposal
// has somewhere to land.
func TestNewProposalApplierWiresWorkspaceContext(t *testing.T) {
	c := newProposalApplierContainer(t)
	ws := t.TempDir()
	c.Config = &config.Config{}
	c.Config.Runtime.ProjectWorkspacePath = ws

	engine := c.newProposalApplier()
	if engine == nil {
		t.Fatal("newProposalApplier returned nil")
	}
	ka, ok := engine.KindAppliers[persistence.ProposalKindWorkspaceContext]
	if !ok {
		t.Fatal("workspace_context KindApplier must be registered when runtime.project_workspace_path is set")
	}
	wca, ok := ka.(*controlplane.WorkspaceContextApplier)
	if !ok {
		t.Fatalf("workspace_context applier has type %T", ka)
	}
	if wca.WorkspaceRoot != ws {
		t.Errorf("workspace root = %q, want %q", wca.WorkspaceRoot, ws)
	}
}
