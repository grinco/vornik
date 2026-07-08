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

// multiOpEnv seeds a temp config dir with an existing config.yaml and returns
// an engine + repo + dir. The scaffold under test creates a new project file
// and replaces config.yaml.
func multiOpEnv(t *testing.T) (*ApplyEngine, persistence.ProposalRepository, string) {
	t.Helper()
	repo := newApplyRepo(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(oldContent), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "projects"), 0o700); err != nil {
		t.Fatalf("mkdir projects: %v", err)
	}
	e := &ApplyEngine{Proposals: repo, ConfigDir: dir, Reload: func() error { return nil }, Logger: zerolog.Nop()}
	return e, repo, dir
}

// seedScaffold creates + approves a multi-op proposal with the given ops.
func seedScaffold(t *testing.T, repo persistence.ProposalRepository, ops []applyFileOp) string {
	t.Helper()
	raw, _ := json.Marshal(ops)
	p := &persistence.ControlPlaneProposal{
		ID: persistence.GenerateID("cpp"), ProjectID: "digest",
		Kind: persistence.ProposalKindScaffold, BlastRadius: persistence.ProposalScopeProject,
		Title: "scaffold digest", ApplyOps: string(raw),
		Status: persistence.ProposalStatusDraft, ProposedBy: "scaffold",
	}
	if err := repo.Create(context.Background(), p); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.SetStatus(context.Background(), p.ID, persistence.ProposalStatusApproved, "vadim"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	return p.ID
}

func TestApplyMultiOp_CreateAndReplace(t *testing.T) {
	e, repo, dir := multiOpEnv(t)
	id := seedScaffold(t, repo, []applyFileOp{
		{Op: applyOpReplace, Path: "config.yaml", Content: newContent},
		{Op: applyOpCreate, Path: "projects/digest.yaml", Content: "id: digest\n"},
	})
	if err := e.Apply(context.Background(), id, "vadim", false); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := readFile(t, filepath.Join(dir, "config.yaml")); got != newContent {
		t.Errorf("config.yaml not replaced: %q", got)
	}
	if got := readFile(t, filepath.Join(dir, "projects/digest.yaml")); got != "id: digest\n" {
		t.Errorf("project file not created: %q", got)
	}
	p, _ := repo.GetByID(context.Background(), id)
	if p.Status != persistence.ProposalStatusApplied {
		t.Errorf("status = %s", p.Status)
	}
	// Snapshot must be the versioned envelope for a multi-op apply.
	var env snapshotEnvelope
	if err := json.Unmarshal([]byte(p.PreApplySnapshot), &env); err != nil || env.Version != snapshotEnvelopeVersion {
		t.Fatalf("expected versioned snapshot envelope, got %q (%v)", p.PreApplySnapshot, err)
	}
	if !env.Entries["config.yaml"].Existed || env.Entries["projects/digest.yaml"].Existed {
		t.Errorf("snapshot entries wrong: %+v", env.Entries)
	}
}

func TestApplyMultiOp_RollbackDeletesCreatedRestoresReplaced(t *testing.T) {
	e, repo, dir := multiOpEnv(t)
	id := seedScaffold(t, repo, []applyFileOp{
		{Op: applyOpReplace, Path: "config.yaml", Content: newContent},
		{Op: applyOpCreate, Path: "projects/digest.yaml", Content: "id: digest\n"},
	})
	if err := e.Apply(context.Background(), id, "vadim", false); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := e.Rollback(context.Background(), id); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if got := readFile(t, filepath.Join(dir, "config.yaml")); got != oldContent {
		t.Errorf("config.yaml not restored: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "projects/digest.yaml")); !os.IsNotExist(err) {
		t.Errorf("created file should be deleted on rollback, stat err=%v", err)
	}
}

func TestApplyMultiOp_CreateConflictRefusedPreWrite(t *testing.T) {
	e, repo, dir := multiOpEnv(t)
	// Pre-existing orphan at the create target.
	if err := os.WriteFile(filepath.Join(dir, "projects/digest.yaml"), []byte("orphan\n"), 0o600); err != nil {
		t.Fatalf("seed orphan: %v", err)
	}
	id := seedScaffold(t, repo, []applyFileOp{
		{Op: applyOpReplace, Path: "config.yaml", Content: newContent},
		{Op: applyOpCreate, Path: "projects/digest.yaml", Content: "id: digest\n"},
	})
	if err := e.Apply(context.Background(), id, "vadim", false); !errors.Is(err, ErrScaffoldConflict) {
		t.Fatalf("create-over-existing must be ErrScaffoldConflict, got %v", err)
	}
	// config.yaml must be untouched (pre-write refusal).
	if got := readFile(t, filepath.Join(dir, "config.yaml")); got != oldContent {
		t.Errorf("conflict must not touch config.yaml, got %q", got)
	}
}

func TestApplyMultiOp_ReplaceMissingRefused(t *testing.T) {
	e, repo, _ := multiOpEnv(t)
	id := seedScaffold(t, repo, []applyFileOp{
		{Op: applyOpReplace, Path: "projects/ghost.yaml", Content: "x\n"},
	})
	if err := e.Apply(context.Background(), id, "vadim", false); !errors.Is(err, ErrScaffoldConflict) {
		t.Fatalf("replace-of-missing must be ErrScaffoldConflict, got %v", err)
	}
}

func TestApplyMultiOp_ReloadFailureReversesAll(t *testing.T) {
	e, repo, dir := multiOpEnv(t)
	calls := 0
	e.Reload = func() error {
		calls++
		if calls == 1 {
			return errors.New("config rejected")
		}
		return nil
	}
	id := seedScaffold(t, repo, []applyFileOp{
		{Op: applyOpReplace, Path: "config.yaml", Content: newContent},
		{Op: applyOpCreate, Path: "projects/digest.yaml", Content: "id: digest\n"},
	})
	if err := e.Apply(context.Background(), id, "vadim", false); err == nil {
		t.Fatal("expected reload failure")
	}
	if got := readFile(t, filepath.Join(dir, "config.yaml")); got != oldContent {
		t.Errorf("config.yaml must be restored after reload failure, got %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "projects/digest.yaml")); !os.IsNotExist(err) {
		t.Errorf("created file must be removed after reload failure, stat err=%v", err)
	}
	p, _ := repo.GetByID(context.Background(), id)
	if p.Status != persistence.ProposalStatusApproved {
		t.Errorf("must stay APPROVED after failed apply, got %s", p.Status)
	}
}

func TestApplyMultiOp_TooManyOpsRefused(t *testing.T) {
	e, repo, _ := multiOpEnv(t)
	ops := make([]applyFileOp, scaffoldMaxOps+1)
	for i := range ops {
		ops[i] = applyFileOp{Op: applyOpCreate, Path: filepath.Join("projects", "p"+string(rune('a'+i))+".yaml"), Content: "x\n"}
	}
	id := seedScaffold(t, repo, ops)
	if err := e.Apply(context.Background(), id, "vadim", false); !errors.Is(err, ErrTooManyOps) {
		t.Fatalf("over-cap ops must be ErrTooManyOps, got %v", err)
	}
}

func TestApply_BaseHashStaleRefused(t *testing.T) {
	e, repo, id, file := approvedProposal(t, persistence.ProposalScopeProject)
	// Record a base hash that does NOT match the on-disk file → stale.
	p, _ := repo.GetByID(context.Background(), id)
	p.Evidence = `{"base_hash":"deadbeef"}`
	// Re-file as a fresh approved proposal carrying the stale base hash.
	np := &persistence.ControlPlaneProposal{
		ID: persistence.GenerateID("cpp"), ProjectID: "janka",
		Kind: persistence.ProposalKindConfig, BlastRadius: persistence.ProposalScopeProject,
		Title: "edit", ApplyTarget: "config.yaml", ApplyContent: newContent,
		Evidence: `{"base_hash":"deadbeef"}`,
		Status:   persistence.ProposalStatusDraft, ProposedBy: "operator-ui",
	}
	_ = repo.Create(context.Background(), np)
	_ = repo.SetStatus(context.Background(), np.ID, persistence.ProposalStatusApproved, "vadim")
	if err := e.Apply(context.Background(), np.ID, "vadim", false); !errors.Is(err, ErrStaleBase) {
		t.Fatalf("stale base hash must be refused with ErrStaleBase, got %v", err)
	}
	if readFile(t, file) != oldContent {
		t.Error("stale-base refusal must not touch the file")
	}
}

func TestApply_BaseHashMatchApplies(t *testing.T) {
	e, repo, _, file := approvedProposal(t, persistence.ProposalScopeProject)
	// Correct base hash of the seeded oldContent.
	sum := hashBytes([]byte(oldContent))
	np := &persistence.ControlPlaneProposal{
		ID: persistence.GenerateID("cpp"), ProjectID: "janka",
		Kind: persistence.ProposalKindConfig, BlastRadius: persistence.ProposalScopeProject,
		Title: "edit", ApplyTarget: "config.yaml", ApplyContent: newContent,
		Evidence: `{"base_hash":"` + sum + `"}`,
		Status:   persistence.ProposalStatusDraft, ProposedBy: "operator-ui",
	}
	_ = repo.Create(context.Background(), np)
	_ = repo.SetStatus(context.Background(), np.ID, persistence.ProposalStatusApproved, "vadim")
	if err := e.Apply(context.Background(), np.ID, "vadim", false); err != nil {
		t.Fatalf("matching base hash must apply, got %v", err)
	}
	if readFile(t, file) != newContent {
		t.Error("expected the edit applied")
	}
}

func TestApplyMultiOp_OrderingCreatesBeforeConfigReplace(t *testing.T) {
	// config.yaml replace must be ordered LAST so referenced files exist first.
	ops := []applyFileOp{
		{Op: applyOpReplace, Path: "config.yaml", Content: "c"},
		{Op: applyOpCreate, Path: "projects/a.yaml", Content: "a"},
		{Op: applyOpReplace, Path: "swarms/s.md", Content: "s"},
	}
	orderOps(ops)
	if ops[0].Op != applyOpCreate {
		t.Errorf("create must come first, got %+v", ops[0])
	}
	if filepath.Base(ops[len(ops)-1].Path) != "config.yaml" {
		t.Errorf("config.yaml must be last, got %+v", ops[len(ops)-1])
	}
}
