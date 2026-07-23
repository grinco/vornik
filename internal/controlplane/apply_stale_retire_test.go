package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// TestApply_StaleBase_AutoRetires: a config-drift ErrStaleBase must not leave
// the proposal lingering APPROVED — Apply reactively retires it to REJECTED
// (approver AutoRetireStaleActor) before returning the ErrStaleBase-wrapped
// error (design 2026-07-23 §B).
func TestApply_StaleBase_AutoRetires(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Connect(ctx, sqlite.DefaultConfig())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	repo := sqlite.NewProposalRepository(db.DB)

	dir := t.TempDir()
	target := "workflows/research.md"
	if err := os.MkdirAll(filepath.Join(dir, "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, target), []byte("current on-disk content\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ev, _ := json.Marshal(map[string]string{"base_hash": "deadbeef_not_the_current_hash"})
	p := &persistence.ControlPlaneProposal{
		ID: "cpp_stale_1", Kind: "config", BlastRadius: persistence.ProposalScopeProject,
		Title: "reclaim timeout", Status: persistence.ProposalStatusApproved,
		ProposedBy: "tune-detector", Approver: "operator",
		ApplyTarget: target, ApplyContent: "new content\n", Evidence: string(ev),
	}
	if err := repo.Create(ctx, p); err != nil {
		t.Fatalf("create: %v", err)
	}

	eng := &ApplyEngine{Proposals: repo, ConfigDir: dir, Logger: zerolog.Nop()}
	err = eng.Apply(ctx, p.ID, "operator", false)
	if !errors.Is(err, ErrStaleBase) {
		t.Fatalf("want ErrStaleBase, got %v", err)
	}
	got, gerr := repo.GetByID(ctx, p.ID)
	if gerr != nil {
		t.Fatalf("get: %v", gerr)
	}
	if got.Status != persistence.ProposalStatusRejected {
		t.Fatalf("stale proposal must be auto-retired to REJECTED, got %s", got.Status)
	}
	if got.Approver != AutoRetireStaleActor {
		t.Fatalf("retire approver = %q, want %q", got.Approver, AutoRetireStaleActor)
	}
}
