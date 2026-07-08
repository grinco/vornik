package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// fakeProposalRepo is a minimal ProposalRepository capturing Create calls.
type fakeProposalRepo struct {
	created []*persistence.ControlPlaneProposal
	err     error
}

func (f *fakeProposalRepo) Create(_ context.Context, p *persistence.ControlPlaneProposal) error {
	if f.err != nil {
		return f.err
	}
	f.created = append(f.created, p)
	return nil
}
func (f *fakeProposalRepo) GetByID(context.Context, string) (*persistence.ControlPlaneProposal, error) {
	return nil, persistence.ErrNotFound
}
func (f *fakeProposalRepo) List(context.Context, persistence.ProposalListFilter) ([]*persistence.ControlPlaneProposal, error) {
	return nil, nil
}
func (f *fakeProposalRepo) SetStatus(context.Context, string, string, string) error { return nil }
func (f *fakeProposalRepo) MarkApplied(context.Context, string, string, string) error {
	return nil
}
func (f *fakeProposalRepo) MarkRolledBack(context.Context, string) error { return nil }

// TestScaffoldProposer_FilesCreateOpsBundle is the core service-layer test:
// ProposeScaffold turns the wizard's configs-relative file set into a DRAFT
// scaffold proposal whose apply_ops are create-ops with paths resolved
// relative to the apply engine's config dir (the `configs/` offset prepended),
// stamped operator-ui / scaffold / project scope.
func TestScaffoldProposer_FilesCreateOpsBundle(t *testing.T) {
	// Production layout: config.yaml at <root>/config.yaml, registry under
	// <root>/configs — so the apply engine's ConfigDir is <root> and the
	// wizard's configsDir is <root>/configs. Offset = "configs".
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	configsDir := filepath.Join(root, "configs")
	if err := os.MkdirAll(configsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	repo := &fakeProposalRepo{}
	p := newScaffoldProposer(repo, configPath, configsDir)
	if p == nil {
		t.Fatal("newScaffoldProposer returned nil")
	}

	files := map[string]string{
		"projects/news.yaml":   "projectId: news\n",
		"swarms/news-swarm.md": "---\nid: news-swarm\n---\n# body\n",
	}
	proposalID, url, err := p.ProposeScaffold(context.Background(), "news", files)
	if err != nil {
		t.Fatalf("ProposeScaffold: %v", err)
	}
	if proposalID == "" {
		t.Error("expected a non-empty proposal id")
	}
	if url == "" {
		t.Error("expected a review URL")
	}
	if len(repo.created) != 1 {
		t.Fatalf("Create called %d times, want 1", len(repo.created))
	}
	pr := repo.created[0]
	if pr.Kind != persistence.ProposalKindScaffold {
		t.Errorf("Kind = %q, want scaffold", pr.Kind)
	}
	if pr.BlastRadius != persistence.ProposalScopeProject {
		t.Errorf("BlastRadius = %q, want project", pr.BlastRadius)
	}
	if pr.ProposedBy != "operator-ui" {
		t.Errorf("ProposedBy = %q, want operator-ui (reserved, server-stamped)", pr.ProposedBy)
	}
	if pr.ProjectID != "news" {
		t.Errorf("ProjectID = %q, want news", pr.ProjectID)
	}
	if pr.Status != persistence.ProposalStatusDraft {
		t.Errorf("Status = %q, want DRAFT", pr.Status)
	}

	var ops []scaffoldOp
	if err := json.Unmarshal([]byte(pr.ApplyOps), &ops); err != nil {
		t.Fatalf("apply_ops not valid JSON: %v", err)
	}
	if len(ops) != 2 {
		t.Fatalf("apply_ops has %d ops, want 2", len(ops))
	}
	gotPaths := map[string]string{}
	for _, o := range ops {
		if o.Op != "create" {
			t.Errorf("op for %s = %q, want create", o.Path, o.Op)
		}
		gotPaths[o.Path] = o.Content
	}
	// Paths must be prefixed with the configs/ offset so they resolve under
	// the apply engine's ConfigDir (the root, not the configs dir).
	if _, ok := gotPaths["configs/projects/news.yaml"]; !ok {
		t.Errorf("apply_ops missing configs/projects/news.yaml; got %v", gotPaths)
	}
	if _, ok := gotPaths["configs/swarms/news-swarm.md"]; !ok {
		t.Errorf("apply_ops missing configs/swarms/news-swarm.md; got %v", gotPaths)
	}
}

// TestScaffoldProposer_CoincidentDirsNoPrefix covers the layout where the
// registry root IS the config dir (configsDir == configDir, e.g. some
// dev/test setups): filepath.Rel returns ".", so op paths must stay
// configs-root-relative with NO "configs/" prefix (review suggestion 2).
func TestScaffoldProposer_CoincidentDirsNoPrefix(t *testing.T) {
	root := t.TempDir()
	// config.yaml and the registry both live directly at root → configDir
	// (Dir(configPath)) == configsDir == root.
	configPath := filepath.Join(root, "config.yaml")
	repo := &fakeProposalRepo{}
	p := newScaffoldProposer(repo, configPath, root)
	if p == nil {
		t.Fatal("newScaffoldProposer returned nil")
	}
	if _, _, err := p.ProposeScaffold(context.Background(), "news", map[string]string{
		"projects/news.yaml": "projectId: news\n",
	}); err != nil {
		t.Fatalf("ProposeScaffold: %v", err)
	}
	var ops []scaffoldOp
	if err := json.Unmarshal([]byte(repo.created[0].ApplyOps), &ops); err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 || ops[0].Path != "projects/news.yaml" {
		t.Errorf("op path = %v, want [projects/news.yaml] (no configs/ prefix when dirs coincide)", ops)
	}
}

// TestScaffoldProposer_RejectsTraversalKey pins the defense-in-depth guard
// (review finding #7): a file key that escapes the config dir is rejected at
// propose time and no proposal is filed.
func TestScaffoldProposer_RejectsTraversalKey(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	configsDir := filepath.Join(root, "configs")
	repo := &fakeProposalRepo{}
	p := newScaffoldProposer(repo, configPath, configsDir)

	_, _, err := p.ProposeScaffold(context.Background(), "evil", map[string]string{
		"../../etc/whatever.yaml": "x: 1\n",
	})
	if err == nil {
		t.Fatal("expected a path-escape error, got nil")
	}
	if !strings.Contains(err.Error(), "escapes the config dir") {
		t.Errorf("error %q should report the path escape", err.Error())
	}
	if len(repo.created) != 0 {
		t.Errorf("no proposal should be filed for a traversal key; got %d", len(repo.created))
	}
}

// TestScaffoldProposer_RejectsCollision — a project id whose file already
// exists fails at propose time with an "already exists" error (mapped to
// PROJECT_EXISTS by the commit handler), rather than filing a doomed proposal.
func TestScaffoldProposer_RejectsCollision(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	configsDir := filepath.Join(root, "configs")
	if err := os.MkdirAll(filepath.Join(configsDir, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Pre-existing project file → collision.
	if err := os.WriteFile(filepath.Join(configsDir, "projects", "dupe.yaml"), []byte("x: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := &fakeProposalRepo{}
	p := newScaffoldProposer(repo, configPath, configsDir)

	_, _, err := p.ProposeScaffold(context.Background(), "dupe", map[string]string{
		"projects/dupe.yaml": "projectId: dupe\n",
	})
	if err == nil {
		t.Fatal("expected a collision error, got nil")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error %q should contain 'already exists' (handler maps to PROJECT_EXISTS)", err.Error())
	}
	if len(repo.created) != 0 {
		t.Errorf("no proposal should be filed on collision; got %d", len(repo.created))
	}
}
