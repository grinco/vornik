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
)

// wsProposal builds a workspace_context proposal for project p targeting the
// canonical context path, with the given read-set hash for that path.
// The project is fixed: these cases are about the applier, not about
// project routing, which targetPath covers separately.
const (
	wsTestProject = "proj"
	wsTestContent = "# after\n"
)

func wsProposal(t *testing.T, readHash string, extraReadSet map[string]string) *persistence.ControlPlaneProposal {
	project, content := wsTestProject, wsTestContent
	t.Helper()
	path := "workspace/" + project + "/.autonomy/PROJECT_CONTEXT.md"
	ops, err := json.Marshal([]applyFileOp{{Op: applyOpReplace, Path: path, Content: content}})
	if err != nil {
		t.Fatal(err)
	}
	readSet := map[string]string{path: readHash}
	for k, v := range extraReadSet {
		readSet[k] = v
	}
	ev, err := json.Marshal(map[string]any{"read_set": readSet})
	if err != nil {
		t.Fatal(err)
	}
	return &persistence.ControlPlaneProposal{
		ID: "cpp_ws", ProjectID: project, Kind: persistence.ProposalKindWorkspaceContext,
		ApplyOps: string(ops), Evidence: string(ev), Status: persistence.ProposalStatusApproved,
	}
}

// Regression: audit 2026-09-15 CA-06 — "Workspace Apply Has No Durable
// Preimage Before Mutation". The applier called os.WriteFile and only THEN
// returned the prior content for the engine to persist in MarkApplied. A
// crash or a ledger failure in between left the live context changed, the
// proposal still APPROVED, and no durable rollback snapshot — and retry then
// failed the stale-base check, so the operator could neither re-apply nor
// roll back.
//
// The fix is ordering: the pre-image must be durable BEFORE the file is
// touched. This test injects a PreImage sink that fails, and asserts the
// file was never modified.
func TestWorkspaceContextApplier_NoMutationWithoutADurablePreImage(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "proj", ".autonomy", "PROJECT_CONTEXT.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	original := "# before\n"
	if err := os.WriteFile(target, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &WorkspaceContextApplier{
		WorkspaceRoot: root,
		PreImage: func(_ context.Context, _ string, _ string) error {
			return os.ErrPermission // the durable record could not be written
		},
	}
	p := wsProposal(t, hashString(original), nil)
	if _, err := a.Apply(context.Background(), p); err == nil {
		t.Fatal("Apply must fail when the pre-image cannot be made durable")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Fatalf("the live context was mutated with no durable pre-image to roll back to: %q", got)
	}
}

// The pre-image must be recorded BEFORE the write, not after it — the
// ordering is the whole point, so assert the sequence, not just presence.
func TestWorkspaceContextApplier_PreImagePrecedesTheWrite(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "proj", ".autonomy", "PROJECT_CONTEXT.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	original := "# before\n"
	if err := os.WriteFile(target, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	var contentAtPreImage string
	a := &WorkspaceContextApplier{
		WorkspaceRoot: root,
		PreImage: func(_ context.Context, _ string, _ string) error {
			b, _ := os.ReadFile(target)
			contentAtPreImage = string(b)
			return nil
		},
	}
	p := wsProposal(t, hashString(original), nil)
	if _, err := a.Apply(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if contentAtPreImage != original {
		t.Fatalf("the pre-image was taken after the write (saw %q)", contentAtPreImage)
	}
	after, _ := os.ReadFile(target)
	if string(after) != "# after\n" {
		t.Fatalf("content = %q", after)
	}
}

// Regression: audit 2026-09-15 CA-06 — "The workspace applier checks only the
// target hash, ignoring other dependencies present in proposal evidence."
// A proposal grounded on a config file that has since changed must be
// refused as stale, exactly as the file path refuses it.
func TestWorkspaceContextApplier_ChecksEveryReadSetEntry(t *testing.T) {
	root := t.TempDir()
	cfgDir := t.TempDir()
	target := filepath.Join(root, "proj", ".autonomy", "PROJECT_CONTEXT.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	original := "# before\n"
	if err := os.WriteFile(target, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	dep := filepath.Join(cfgDir, "configs", "projects", "proj.yaml")
	if err := os.MkdirAll(filepath.Dir(dep), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dep, []byte("projectId: proj\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &WorkspaceContextApplier{WorkspaceRoot: root, ConfigDir: cfgDir}
	// The proposal was grounded on the ORIGINAL dependency content...
	p := wsProposal(t, hashString(original), map[string]string{
		"configs/projects/proj.yaml": hashString("projectId: proj\n"),
	})
	// ...which an operator then changed.
	if err := os.WriteFile(dep, []byte("projectId: proj\nautonomy:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Apply(context.Background(), p); err == nil {
		t.Fatal("a changed read-set dependency must make the proposal stale, not applyable")
	} else if !errors.Is(err, ErrStaleBase) {
		t.Fatalf("want a stale-base refusal, got %v", err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != original {
		t.Fatal("a stale proposal must not have mutated the context file")
	}
}

// failingMarkApplied wraps a store so the ledger write fails exactly once,
// reproducing the window between the workspace file landing and the ledger
// recording it.
type failingMarkApplied struct {
	persistence.ProposalRepository
	fail bool
	rows map[string]*persistence.ControlPlaneProposal
}

func (f *failingMarkApplied) GetByID(_ context.Context, id string) (*persistence.ControlPlaneProposal, error) {
	p, ok := f.rows[id]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	cp := *p
	return &cp, nil
}

func (f *failingMarkApplied) List(_ context.Context, filter persistence.ProposalListFilter) ([]*persistence.ControlPlaneProposal, error) {
	var out []*persistence.ControlPlaneProposal
	for _, p := range f.rows {
		if len(filter.Statuses) > 0 {
			match := false
			for _, st := range filter.Statuses {
				if p.Status == st {
					match = true
				}
			}
			if !match {
				continue
			}
		}
		cp := *p
		out = append(out, &cp)
	}
	return out, nil
}

func (f *failingMarkApplied) StagePreApplySnapshot(_ context.Context, id, snapshot string) error {
	if p, ok := f.rows[id]; ok {
		p.PreApplySnapshot = snapshot
	}
	return nil
}

func (f *failingMarkApplied) MarkApplied(_ context.Context, id, actor, snapshot string) error {
	if f.fail {
		return errors.New("ledger unavailable")
	}
	if p, ok := f.rows[id]; ok {
		p.Status, p.AppliedBy, p.PreApplySnapshot = persistence.ProposalStatusApplied, actor, snapshot
	}
	return nil
}

// Regression: re-audit 2026-09-15 CA-06 (REOPENED, first probe) — "staged
// workspace pre-images have no recovery consumer".
//
// The pre-image is now committed before the write, which is what the previous
// round fixed. But workspace_context is a KindApplier and bypasses the
// journal, so when MarkApplied fails (or the process dies) after the rename,
// the result is: file changed, proposal still APPROVED, snapshot staged — and
// ZERO journal rows. Reconcile scans config_apply_journal only, so restart
// recovery does not see it; retry fails the read-set check because the target
// is no longer absent, and rollback refuses a proposal that is not APPLIED.
// The operator is left with a changed file they can neither finish nor undo.
//
// THE SEAM: staging a pre-image is not recovery. Something has to READ it.
func TestWorkspaceContext_RecoversAStagedProposalAfterLedgerFailure(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "proj", ".autonomy", "PROJECT_CONTEXT.md")
	store := &failingMarkApplied{fail: true, rows: map[string]*persistence.ControlPlaneProposal{}}
	p := wsProposal(t, contentHashState(nil, false), nil)
	// The target is absent, so this is a CREATE.
	ops, err := json.Marshal([]applyFileOp{{Op: applyOpCreate, Path: "workspace/proj/.autonomy/PROJECT_CONTEXT.md", Content: "# after\n"}})
	if err != nil {
		t.Fatal(err)
	}
	p.ApplyOps = string(ops)
	p.Status = persistence.ProposalStatusApproved
	store.rows[p.ID] = p

	ws := &WorkspaceContextApplier{
		WorkspaceRoot: root,
		PreImage: func(ctx context.Context, id, snap string) error {
			return store.StagePreApplySnapshot(ctx, id, snap)
		},
	}
	e := &ApplyEngine{
		Proposals:    store,
		ConfigDir:    t.TempDir(),
		Logger:       zerolog.Nop(),
		KindAppliers: map[string]KindApplier{persistence.ProposalKindWorkspaceContext: ws},
	}

	// The apply writes the file and then the ledger write fails.
	if err := e.Apply(context.Background(), p.ID, "operator", false); err == nil {
		t.Fatal("the ledger failure should have surfaced")
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("fixture: the file should have been written: %v", err)
	}
	if store.rows[p.ID].Status != persistence.ProposalStatusApproved {
		t.Fatalf("fixture: proposal should still be APPROVED, got %s", store.rows[p.ID].Status)
	}

	// Restart: the ledger is healthy again and recovery runs.
	store.fail = false
	if err := e.RecoverStagedKindApplies(context.Background()); err != nil {
		t.Fatalf("staged-kind recovery failed: %v", err)
	}
	if got := store.rows[p.ID].Status; got != persistence.ProposalStatusApplied {
		t.Fatalf("a write that landed must be reconciled to APPLIED, got %s — the operator can neither finish nor undo it", got)
	}
}

// Regression: companion review-20260915-10cd F5 — checkReadSetDependencies
// joined EVERY non-target read-set entry under ConfigDir, including
// workspace-rooted ones. A proposal whose read set names another workspace
// path resolved to the wrong file in the wrong tree, hashed the wrong bytes
// and was refused as stale. Fail-closed, but wrong — and a legitimate apply
// an operator cannot make is still an outage for them.
func TestWorkspaceContextApplier_ResolvesReadSetEntriesByRoot(t *testing.T) {
	root := t.TempDir()
	cfgDir := t.TempDir()
	target := filepath.Join(root, "proj", ".autonomy", "PROJECT_CONTEXT.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("# before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A SECOND workspace-rooted dependency, unchanged since the proposal.
	sibling := filepath.Join(root, "proj", ".autonomy", "NOTES.md")
	if err := os.WriteFile(sibling, []byte("notes\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	a := &WorkspaceContextApplier{WorkspaceRoot: root, ConfigDir: cfgDir}
	p := wsProposal(t, hashString("# before\n"), map[string]string{
		"workspace/proj/.autonomy/NOTES.md": hashString("notes\n"),
	})
	if _, err := a.Apply(context.Background(), p); err != nil {
		t.Fatalf("an unchanged workspace-rooted dependency must not read as stale: %v", err)
	}

	// And it must still DETECT a real change to that dependency.
	if err := os.WriteFile(target, []byte("# before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sibling, []byte("notes changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p2 := wsProposal(t, hashString("# before\n"), map[string]string{
		"workspace/proj/.autonomy/NOTES.md": hashString("notes\n"),
	})
	if _, err := a.Apply(context.Background(), p2); !errors.Is(err, ErrStaleBase) {
		t.Fatalf("a changed workspace-rooted dependency must be stale, got %v", err)
	}
}
