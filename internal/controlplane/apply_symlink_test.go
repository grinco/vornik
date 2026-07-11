package controlplane

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// TestApply_RejectsSymlinkEscape — audit 2026-07-09 LOW-4: resolveTarget now
// routes through safepath.JoinUnder, so an apply whose target resolves through
// a pre-existing symlink pointing outside ConfigDir is rejected (the old
// lexical Clean+HasPrefix guard would have written through it).
func TestApply_RejectsSymlinkEscape(t *testing.T) {
	repo := newApplyRepo(t)
	dir := t.TempDir()
	outside := t.TempDir()
	// dir/escape → outside; the proposal targets escape/pwned (lexically under
	// dir, resolves outside).
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	// Seed the escape target file so replace has something to hit.
	if err := os.WriteFile(filepath.Join(outside, "pwned"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p := &persistence.ControlPlaneProposal{
		ID: persistence.GenerateID("cpp"), ProjectID: "janka",
		Kind: persistence.ProposalKindConfig, BlastRadius: persistence.ProposalScopeProject,
		Title: "escape", ApplyTarget: "escape/pwned", ApplyContent: "new",
		Status: persistence.ProposalStatusDraft, ProposedBy: "tune-detector",
	}
	if err := repo.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetStatus(ctx, p.ID, persistence.ProposalStatusApproved, "vadim"); err != nil {
		t.Fatal(err)
	}
	e := &ApplyEngine{Proposals: repo, ConfigDir: dir, Reload: func() error { return nil }, Logger: zerolog.Nop()}
	if err := e.Apply(ctx, p.ID, "vadim", false); !errors.Is(err, ErrPathTraversal) {
		t.Fatalf("symlink-escaping apply must be ErrPathTraversal, got %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "pwned")); string(b) != "old" {
		t.Fatal("the outside file must not have been written through the symlink")
	}
}
