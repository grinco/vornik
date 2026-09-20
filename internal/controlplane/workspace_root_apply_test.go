package controlplane

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// The workspace-context write on the JOURNALED file path (config-apply-journal
// design §9.2b/c).
//
// It used to be a KindApplier, and these cases are the ones its own tests
// covered that are still meaningful after the move — ported rather than
// deleted, because what they pin is the BEHAVIOUR, not the mechanism: the file
// lands where the virtual path says, a stale base refuses, another project's
// context is refused, and a rollback restores. The durability properties those
// tests also covered (a pre-image committed before the mutation, a fenced
// rollback, recovery of a staged apply) are now the journal's and are covered
// by the journal's own suite — which is the point of the move.

func workspaceEnv(t *testing.T) (*journalEnv, string) {
	t.Helper()
	env := newJournalEnv(t)
	ws := t.TempDir()
	env.e.Roots = map[string]string{WorkspaceRootName: ws}
	return env, ws
}

func seedWorkspaceProposal(t *testing.T, env *journalEnv, projectID, content, readHash string) string {
	t.Helper()
	ops := []applyFileOp{{Op: applyOpCreate, Path: WorkspaceContextPath(projectID), Content: content}}
	raw, _ := json.Marshal(ops)
	evidence := ""
	if readHash != "" {
		evidence = `{"read_set":{"` + WorkspaceContextPath(projectID) + `":"` + readHash + `"}}`
	}
	p := &persistence.ControlPlaneProposal{
		ID: persistence.GenerateID("cpp"), ProjectID: projectID,
		Kind: persistence.ProposalKindWorkspaceContext, BlastRadius: persistence.ProposalScopeProject,
		Title: "context for " + projectID, ApplyOps: string(raw), Evidence: evidence,
		Status: persistence.ProposalStatusDraft, ProposedBy: "assistant",
	}
	ctx := context.Background()
	if err := env.repo.Create(ctx, p); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := env.repo.SetStatus(ctx, p.ID, persistence.ProposalStatusApproved, "vadim"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	return p.ID
}

// The write lands in the WORKSPACE tree, not under the config dir — which is
// the whole reason the root exists.
func TestWorkspaceRootApply_WritesIntoTheWorkspaceTree(t *testing.T) {
	env, ws := workspaceEnv(t)
	id := seedWorkspaceProposal(t, env, "proj-1", "the context", "")

	if err := env.e.Apply(context.Background(), id, "vadim", false); err != nil {
		t.Fatalf("apply: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(ws, "proj-1/.autonomy/PROJECT_CONTEXT.md"))
	if err != nil {
		t.Fatalf("the context file is not in the workspace tree: %v", err)
	}
	if string(got) != "the context" {
		t.Fatalf("content = %q", got)
	}
	// And nothing was written into the config tree under the virtual path.
	if env.exists("workspace/proj-1/.autonomy/PROJECT_CONTEXT.md") {
		t.Fatal("the op also resolved under the config dir; the root is not routing")
	}
}

// It is journalled like any other apply — which is the property the move
// exists to gain, and the one the KindApplier seam could not provide.
func TestWorkspaceRootApply_IsJournalled(t *testing.T) {
	env, _ := workspaceEnv(t)
	id := seedWorkspaceProposal(t, env, "proj-1", "the context", "")

	if err := env.e.Apply(context.Background(), id, "vadim", false); err != nil {
		t.Fatalf("apply: %v", err)
	}

	row := env.latestJournal(t, id)
	if row == nil {
		t.Fatal("a workspace write produced no journal row; it is still bypassing the journal")
	}
	if row.State != persistence.JournalStateApplied {
		t.Fatalf("journal state = %q, want applied", row.State)
	}
}

// A stale base refuses, exactly as the applier did: the proposal was drafted
// against content that is no longer there.
func TestWorkspaceRootApply_RefusesAStaleBase(t *testing.T) {
	env, ws := workspaceEnv(t)
	if err := os.MkdirAll(filepath.Join(ws, "proj-1/.autonomy"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "proj-1/.autonomy/PROJECT_CONTEXT.md"), []byte("actual"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := seedWorkspaceProposal(t, env, "proj-1", "new", "sha256:0000000000000000000000000000000000000000000000000000000000000000")

	if err := env.e.Apply(context.Background(), id, "vadim", false); err == nil {
		t.Fatal("an apply against a stale base was allowed")
	}
	got, _ := os.ReadFile(filepath.Join(ws, "proj-1/.autonomy/PROJECT_CONTEXT.md"))
	if string(got) != "actual" {
		t.Fatalf("the refused apply still mutated the file: %q", got)
	}
}
