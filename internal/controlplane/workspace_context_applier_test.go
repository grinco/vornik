package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

func approvedWorkspaceContextProposal(t *testing.T, repo persistence.ProposalRepository, projectID, opPath, op, content, base string) string {
	t.Helper()
	ops, _ := json.Marshal([]applyFileOp{{Op: op, Path: opPath, Content: content}})
	ev, _ := json.Marshal(struct {
		ReadSet map[string]string `json:"read_set"`
	}{ReadSet: map[string]string{opPath: base}})
	p := &persistence.ControlPlaneProposal{
		ID:          persistence.GenerateID("cpp"),
		ProjectID:   projectID,
		Kind:        persistence.ProposalKindWorkspaceContext,
		BlastRadius: persistence.ProposalScopeProject,
		Title:       "update context",
		ApplyOps:    string(ops),
		Evidence:    string(ev),
		Status:      persistence.ProposalStatusDraft,
		ProposedBy:  "config-assistant",
	}
	if err := repo.Create(context.Background(), p); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.SetStatus(context.Background(), p.ID, persistence.ProposalStatusApproved, "vadim"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	return p.ID
}

func TestWorkspaceContextApplier_ApplyAndRollback(t *testing.T) {
	repo := newApplyRepo(t)
	ws := t.TempDir()
	target := filepath.Join(ws, "assistant", ".autonomy", "PROJECT_CONTEXT.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	old := "# Context\nold source\n"
	next := "# Context\nnew source\n"
	if err := os.WriteFile(target, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	path := "workspace/assistant/.autonomy/PROJECT_CONTEXT.md"
	id := approvedWorkspaceContextProposal(t, repo, "assistant", path, applyOpReplace, next, workspaceContextHashBytes([]byte(old)))
	e := &ApplyEngine{
		Proposals: repo,
		ConfigDir: t.TempDir(),
		Logger:    zerolog.Nop(),
		Reload:    func() error { t.Fatal("workspace context apply must not reload config"); return nil },
		KindAppliers: map[string]KindApplier{
			persistence.ProposalKindWorkspaceContext: &WorkspaceContextApplier{WorkspaceRoot: ws},
		},
	}

	if err := e.Apply(context.Background(), id, "vadim", false); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := readFile(t, target); got != next {
		t.Fatalf("workspace context = %q", got)
	}
	p, err := repo.GetByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != persistence.ProposalStatusApplied || !strings.Contains(p.PreApplySnapshot, "old source") {
		t.Fatalf("ledger after apply: %+v", p)
	}
	if err := e.Rollback(context.Background(), id); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if got := readFile(t, target); got != old {
		t.Fatalf("workspace context after rollback = %q", got)
	}
}

func TestWorkspaceContextApplier_RefusesStaleBase(t *testing.T) {
	repo := newApplyRepo(t)
	ws := t.TempDir()
	target := filepath.Join(ws, "assistant", ".autonomy", "PROJECT_CONTEXT.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("current\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := "workspace/assistant/.autonomy/PROJECT_CONTEXT.md"
	id := approvedWorkspaceContextProposal(t, repo, "assistant", path, applyOpReplace, "new\n", workspaceContextHashBytes([]byte("old\n")))
	e := &ApplyEngine{Proposals: repo, ConfigDir: t.TempDir(), Logger: zerolog.Nop(), KindAppliers: map[string]KindApplier{
		persistence.ProposalKindWorkspaceContext: &WorkspaceContextApplier{WorkspaceRoot: ws},
	}}

	if err := e.Apply(context.Background(), id, "vadim", false); !errors.Is(err, ErrStaleBase) {
		t.Fatalf("apply = %v, want ErrStaleBase", err)
	}
	if got := readFile(t, target); got != "current\n" {
		t.Fatalf("stale refusal touched disk: %q", got)
	}
}

func TestWorkspaceContextApplier_RefusesWrongProjectPath(t *testing.T) {
	repo := newApplyRepo(t)
	ws := t.TempDir()
	path := "workspace/other/.autonomy/PROJECT_CONTEXT.md"
	id := approvedWorkspaceContextProposal(t, repo, "assistant", path, applyOpCreate, "new\n", "ABSENT")
	e := &ApplyEngine{Proposals: repo, ConfigDir: t.TempDir(), Logger: zerolog.Nop(), KindAppliers: map[string]KindApplier{
		persistence.ProposalKindWorkspaceContext: &WorkspaceContextApplier{WorkspaceRoot: ws},
	}}

	if err := e.Apply(context.Background(), id, "vadim", false); err == nil || !strings.Contains(err.Error(), "canonical context path") {
		t.Fatalf("apply = %v, want canonical path refusal", err)
	}
	if _, err := os.Stat(filepath.Join(ws, "other")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("wrong-project path was touched: %v", err)
	}
}
