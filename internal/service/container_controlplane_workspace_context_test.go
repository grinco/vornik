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

// UPDATED 2026-09-19 (config-apply-journal design §9.2b/c). This asserted that
// the workspace_context KIND APPLIER was registered. It no longer exists: the
// write was on that seam only because the apply engine could resolve ops under
// exactly one root, so it bypassed the journal and took durable recovery with
// it. The engine now understands named roots, so the assertion moves to the
// wiring that replaced it — and the test is rewritten deliberately rather than
// deleted, because "the workspace write has somewhere to land" is still exactly
// what needs pinning.
func TestNewProposalApplierWiresTheWorkspaceRoot(t *testing.T) {
	c := newProposalApplierContainer(t)
	ws := t.TempDir()
	c.Config = &config.Config{}
	c.Config.Runtime.ProjectWorkspacePath = ws

	engine := c.newProposalApplier()
	if engine == nil {
		t.Fatal("newProposalApplier returned nil")
	}
	if got := engine.Roots[controlplane.WorkspaceRootName]; got != ws {
		t.Fatalf("apply engine workspace root = %q, want %q", got, ws)
	}
	// The container must carry the SAME mapping: verifyConfigGeneration
	// re-reads through it after the reload, and a second spelling is how a
	// write succeeds while its verification looks in the wrong tree.
	if got := c.applyRoots[controlplane.WorkspaceRootName]; got != ws {
		t.Fatalf("container apply root = %q, want %q", got, ws)
	}
	// And the kind must NOT be on the applier seam any more, or both paths
	// would claim the write.
	if _, ok := engine.KindAppliers[persistence.ProposalKindWorkspaceContext]; ok {
		t.Fatal("workspace_context is still registered as a KindApplier; two paths now claim one write")
	}
	if persistence.KindApplierManaged(persistence.ProposalKindWorkspaceContext) {
		t.Fatal("workspace_context is still reported as kind-applier managed")
	}
}
