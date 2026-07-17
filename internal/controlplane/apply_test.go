package controlplane

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

const (
	oldContent = "server:\n  address: :8080\n"
	newContent = "server:\n  address: :9090\n"
)

func newApplyRepo(t *testing.T) persistence.ProposalRepository {
	t.Helper()
	db, err := sqlite.Connect(context.Background(), sqlite.DefaultConfig())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return sqlite.NewProposalRepository(db.DB)
}

// approvedProposal seeds an APPROVED, applyable proposal + writes the target
// file with oldContent. Returns the engine, repo, proposal id, and file path.
func approvedProposal(t *testing.T, scope string) (*ApplyEngine, persistence.ProposalRepository, string, string) {
	t.Helper()
	repo := newApplyRepo(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(file, []byte(oldContent), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	ctx := context.Background()
	p := &persistence.ControlPlaneProposal{
		ID: persistence.GenerateID("cpp"), ProjectID: "janka",
		Kind: persistence.ProposalKindConfig, BlastRadius: scope,
		Title: "bump", ApplyTarget: "config.yaml", ApplyContent: newContent,
		Status: persistence.ProposalStatusDraft, ProposedBy: "tune-detector",
	}
	if err := repo.Create(ctx, p); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.SetStatus(ctx, p.ID, persistence.ProposalStatusApproved, "vadim"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	e := &ApplyEngine{Proposals: repo, ConfigDir: dir, Reload: func() error { return nil }, Logger: zerolog.Nop()}
	return e, repo, p.ID, file
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(b)
}

func TestApply_Success(t *testing.T) {
	e, repo, id, file := approvedProposal(t, persistence.ProposalScopeProject)
	if err := e.Apply(context.Background(), id, "vadim", false); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := readFile(t, file); got != newContent {
		t.Errorf("file not updated: %q", got)
	}
	p, _ := repo.GetByID(context.Background(), id)
	if p.Status != persistence.ProposalStatusApplied || p.AppliedBy != "vadim" || p.PreApplySnapshot != oldContent {
		t.Errorf("ledger not recorded: %+v", p)
	}
}

func TestApply_ReloadFailsAutoRollback(t *testing.T) {
	e, repo, id, file := approvedProposal(t, persistence.ProposalScopeProject)
	calls := 0
	e.Reload = func() error {
		calls++
		if calls == 1 {
			return errors.New("config rejected")
		}
		return nil // rollback reload succeeds
	}
	err := e.Apply(context.Background(), id, "vadim", false)
	if err == nil {
		t.Fatal("expected apply to fail on reload rejection")
	}
	if got := readFile(t, file); got != oldContent {
		t.Errorf("file must be auto-rolled-back to old content, got %q", got)
	}
	p, _ := repo.GetByID(context.Background(), id)
	if p.Status != persistence.ProposalStatusApproved {
		t.Errorf("status must stay APPROVED after failed apply, got %s", p.Status)
	}
}

func TestApply_ValidationFailsNoWrite(t *testing.T) {
	e, _, id, file := approvedProposal(t, persistence.ProposalScopeProject)
	e.Validate = func(_, _ string) error { return errors.New("bad yaml") }
	if err := e.Apply(context.Background(), id, "vadim", false); err == nil {
		t.Fatal("expected validation failure")
	}
	if got := readFile(t, file); got != oldContent {
		t.Errorf("file must be untouched on validation failure, got %q", got)
	}
	// No temp files left behind.
	entries, _ := os.ReadDir(filepath.Dir(file))
	for _, en := range entries {
		if filepath.Ext(en.Name()) == ".tmp" || len(en.Name()) > 8 && en.Name()[:9] == ".cp-apply" {
			t.Errorf("stray temp file left: %s", en.Name())
		}
	}
}

func TestApply_ReviewOnlyRefused(t *testing.T) {
	e, repo, id, _ := approvedProposal(t, persistence.ProposalScopeProject)
	// Blank the apply target → review-only.
	p, _ := repo.GetByID(context.Background(), id)
	p.ApplyTarget = ""
	// Re-create a review-only approved proposal instead (target is immutable
	// via repo); simplest: new proposal with no target.
	np := &persistence.ControlPlaneProposal{
		ID: persistence.GenerateID("cpp"), ProjectID: "janka",
		Kind: persistence.ProposalKindConfig, BlastRadius: persistence.ProposalScopeProject,
		Title: "review only", Status: persistence.ProposalStatusDraft, ProposedBy: "tune-detector",
	}
	_ = repo.Create(context.Background(), np)
	_ = repo.SetStatus(context.Background(), np.ID, persistence.ProposalStatusApproved, "vadim")
	if err := e.Apply(context.Background(), np.ID, "vadim", false); !errors.Is(err, ErrReviewOnly) {
		t.Fatalf("review-only apply must fail ErrReviewOnly, got %v", err)
	}
}

func TestApply_DaemonScopeNeedsAck(t *testing.T) {
	e, _, id, _ := approvedProposal(t, persistence.ProposalScopeDaemon)
	if err := e.Apply(context.Background(), id, "vadim", false); !errors.Is(err, ErrDaemonAckRequired) {
		t.Fatalf("daemon-scope without ack must fail, got %v", err)
	}
	if err := e.Apply(context.Background(), id, "vadim", true); err != nil {
		t.Fatalf("daemon-scope with ack must succeed, got %v", err)
	}
}

func TestApply_BusyRefused(t *testing.T) {
	e, _, id, file := approvedProposal(t, persistence.ProposalScopeProject)
	e.HasActiveTasks = func(_ context.Context, _ string) (bool, error) { return true, nil }
	if err := e.Apply(context.Background(), id, "vadim", false); !errors.Is(err, ErrBusy) {
		t.Fatalf("busy scope must refuse, got %v", err)
	}
	if got := readFile(t, file); got != oldContent {
		t.Errorf("busy refusal must not touch the file")
	}
}

func TestApply_NotApprovedRefused(t *testing.T) {
	e, repo, id, _ := approvedProposal(t, persistence.ProposalScopeProject)
	_ = e.Apply(context.Background(), id, "vadim", false) // → APPLIED
	// Second apply refused.
	if err := e.Apply(context.Background(), id, "vadim", false); !errors.Is(err, persistence.ErrProposalNotApproved) {
		t.Fatalf("re-apply must fail ErrProposalNotApproved, got %v", err)
	}
	_ = repo
}

func TestApply_PathTraversalRefused(t *testing.T) {
	e, repo, _, _ := approvedProposal(t, persistence.ProposalScopeProject)
	np := &persistence.ControlPlaneProposal{
		ID: persistence.GenerateID("cpp"), ProjectID: "janka",
		Kind: persistence.ProposalKindConfig, BlastRadius: persistence.ProposalScopeProject,
		Title: "evil", ApplyTarget: "../../etc/passwd", ApplyContent: "x",
		Status: persistence.ProposalStatusDraft, ProposedBy: "x",
	}
	_ = repo.Create(context.Background(), np)
	_ = repo.SetStatus(context.Background(), np.ID, persistence.ProposalStatusApproved, "vadim")
	if err := e.Apply(context.Background(), np.ID, "vadim", false); !errors.Is(err, ErrPathTraversal) {
		t.Fatalf("path traversal must be refused, got %v", err)
	}
}

func TestApply_AbsolutePathRefused(t *testing.T) {
	e, repo, _, _ := approvedProposal(t, persistence.ProposalScopeProject)
	np := &persistence.ControlPlaneProposal{
		ID: persistence.GenerateID("cpp"), ProjectID: "janka",
		Kind: persistence.ProposalKindConfig, BlastRadius: persistence.ProposalScopeProject,
		Title: "absolute", ApplyTarget: "/tmp/evil.yaml", ApplyContent: "x",
		Status: persistence.ProposalStatusDraft, ProposedBy: "x",
	}
	if err := repo.Create(context.Background(), np); err != nil {
		t.Fatalf("create proposal: %v", err)
	}
	if err := repo.SetStatus(context.Background(), np.ID, persistence.ProposalStatusApproved, "vadim"); err != nil {
		t.Fatalf("approve proposal: %v", err)
	}
	if err := e.Apply(context.Background(), np.ID, "vadim", false); !errors.Is(err, ErrPathTraversal) {
		t.Fatalf("absolute apply target must be refused, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.ConfigDir, "tmp", "evil.yaml")); !os.IsNotExist(err) {
		t.Fatalf("absolute target must not be silently retargeted under config dir, stat err=%v", err)
	}
}

func TestRollback_RestoresSnapshot(t *testing.T) {
	e, repo, id, file := approvedProposal(t, persistence.ProposalScopeProject)
	if err := e.Apply(context.Background(), id, "vadim", false); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if readFile(t, file) != newContent {
		t.Fatal("precondition: file should be new")
	}
	if err := e.Rollback(context.Background(), id); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if got := readFile(t, file); got != oldContent {
		t.Errorf("rollback must restore old content, got %q", got)
	}
	p, _ := repo.GetByID(context.Background(), id)
	if p.Status != persistence.ProposalStatusRolledBack {
		t.Errorf("expected ROLLED_BACK, got %s", p.Status)
	}
}

func TestSyncDir_MissingDirReturnsError(t *testing.T) {
	if err := syncDir(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("syncDir must surface open/sync failures")
	}
}
