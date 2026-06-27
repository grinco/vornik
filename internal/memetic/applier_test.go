package memetic

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// stubProposalRepo is an in-memory implementation of
// persistence.WorkflowProposalRepository. Only the methods Apply
// exercises are real; the rest return zero values.
type stubProposalRepo struct {
	mu         sync.Mutex
	rows       map[string]*persistence.WorkflowProposal
	getErr     error
	markErr    error
	markCalled string
	markSHA    string
}

func newStubProposalRepo() *stubProposalRepo {
	return &stubProposalRepo{rows: map[string]*persistence.WorkflowProposal{}}
}

func (s *stubProposalRepo) Insert(_ context.Context, p *persistence.WorkflowProposal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *p
	s.rows[p.ID] = &cp
	return nil
}
func (s *stubProposalRepo) Get(_ context.Context, id string) (*persistence.WorkflowProposal, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.rows[id]; ok {
		cp := *p
		return &cp, nil
	}
	return nil, persistence.ErrNotFound
}
func (s *stubProposalRepo) List(_ context.Context, _ persistence.WorkflowProposalFilter) ([]*persistence.WorkflowProposal, error) {
	return nil, nil
}
func (s *stubProposalRepo) Decide(_ context.Context, _ string, _ persistence.WorkflowProposalStatus, _, _ string) error {
	return nil
}
func (s *stubProposalRepo) MarkApplied(_ context.Context, id, sha string) error {
	if s.markErr != nil {
		return s.markErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markCalled = id
	s.markSHA = sha
	if p, ok := s.rows[id]; ok {
		p.Status = persistence.WorkflowProposalStatusApplied
		now := time.Now().UTC()
		p.AppliedAt = &now
		p.AppliedCommit = sha
	}
	return nil
}
func (s *stubProposalRepo) MarkRolledBack(_ context.Context, id, sha string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.rows[id]; ok {
		p.Status = persistence.WorkflowProposalStatusRolledBack
		p.RollbackCommit = sha
	}
	return nil
}
func (s *stubProposalRepo) UpdateProposalYAML(_ context.Context, id, yaml, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.rows[id]; ok {
		p.ProposalYAML = yaml
	}
	return nil
}

type stubWriter struct {
	gotWorkflowID   string
	gotBody         []byte
	sourcePath      string
	emptySourcePath bool
	err             error
}

func (w *stubWriter) Write(_ context.Context, workflowID string, body []byte) (string, error) {
	w.gotWorkflowID = workflowID
	w.gotBody = body
	if w.err != nil {
		return "", w.err
	}
	if w.emptySourcePath {
		return "", nil
	}
	if w.sourcePath == "" {
		return "/tmp/configs/workflows/" + workflowID + ".md", nil
	}
	return w.sourcePath, nil
}

type stubGit struct {
	gotPath, gotMsg, gotName, gotEmail string
	sha                                string
	err                                error
}

func (g *stubGit) Commit(_ context.Context, path, msg, name, email string) (string, error) {
	g.gotPath, g.gotMsg, g.gotName, g.gotEmail = path, msg, name, email
	if g.err != nil {
		return "", g.err
	}
	if g.sha == "" {
		return "deadbeef", nil
	}
	return g.sha, nil
}

type stubReloader struct {
	called int
	err    error
}

func (r *stubReloader) Reload() error {
	r.called++
	return r.err
}

func approvedFixture(id, workflowID string) *persistence.WorkflowProposal {
	return &persistence.WorkflowProposal{
		ID: id, WorkflowID: workflowID,
		Status:         persistence.WorkflowProposalStatusApproved,
		ProposalYAML:   "---\nworkflowId: " + workflowID + "\n---\nbody\n",
		Motivation:     "tighten the gate on step3 — saw 32% failure",
		EvidenceRunIDs: []string{"r-1", "r-2", "r-3"},
		Confidence:     0.81,
		ArchitectModel: "test",
		CreatedAt:      time.Now().UTC(),
	}
}

// TestApplier_HappyPath — approved row + all deps wired → row
// transitions to applied + commit SHA is stored.
func TestApplier_HappyPath(t *testing.T) {
	repo := newStubProposalRepo()
	_ = repo.Insert(context.Background(), approvedFixture("wpr-1", "research"))
	writer := &stubWriter{}
	git := &stubGit{sha: "abc1234"}
	reloader := &stubReloader{}
	a := NewApplier(repo, writer, git, reloader,
		ApplierConfig{AuthorName: "vornik", AuthorEmail: "vornik@example.com"})

	got, err := a.Apply(context.Background(), "wpr-1", "operator-x")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got == nil || got.Status != persistence.WorkflowProposalStatusApplied {
		t.Fatalf("status not applied: %+v", got)
	}
	if got.AppliedCommit != "abc1234" {
		t.Errorf("applied_commit: %q", got.AppliedCommit)
	}
	if writer.gotWorkflowID != "research" {
		t.Errorf("writer not called with workflow ID: %q", writer.gotWorkflowID)
	}
	if !strings.Contains(string(writer.gotBody), "workflowId: research") {
		t.Errorf("writer body: %q", writer.gotBody)
	}
	if git.gotName != "vornik" {
		t.Errorf("git author name: %q", git.gotName)
	}
	if !strings.Contains(git.gotMsg, "workflow(research):") {
		t.Errorf("commit subject: %q", git.gotMsg)
	}
	if !strings.Contains(git.gotMsg, "proposal_id=wpr-1") {
		t.Errorf("commit body should reference proposal_id: %q", git.gotMsg)
	}
	if !strings.Contains(git.gotMsg, "operator-x") {
		t.Errorf("commit body should reference operator: %q", git.gotMsg)
	}
	if !strings.Contains(git.gotMsg, "Evidence (3 runs)") {
		t.Errorf("commit body should list evidence: %q", git.gotMsg)
	}
	if reloader.called != 1 {
		t.Errorf("reloader should fire exactly once, got %d", reloader.called)
	}
	if repo.markSHA != "abc1234" {
		t.Errorf("markApplied SHA: %q", repo.markSHA)
	}
}

// TestApplier_NotApproved — pending row can't be applied. The
// operator must approve via Slice 3 first.
func TestApplier_NotApproved(t *testing.T) {
	repo := newStubProposalRepo()
	pending := approvedFixture("wpr-1", "research")
	pending.Status = persistence.WorkflowProposalStatusPending
	_ = repo.Insert(context.Background(), pending)
	a := NewApplier(repo, &stubWriter{}, &stubGit{}, &stubReloader{}, ApplierConfig{})

	_, err := a.Apply(context.Background(), "wpr-1", "operator-x")
	if !errors.Is(err, ErrProposalNotApproved) {
		t.Fatalf("want ErrProposalNotApproved, got %v", err)
	}
}

// TestApplier_AlreadyApplied — a row already in status=applied
// can't be applied twice. Slice 5's rollback path is the only
// way to move out of applied.
func TestApplier_AlreadyApplied(t *testing.T) {
	repo := newStubProposalRepo()
	applied := approvedFixture("wpr-1", "research")
	applied.Status = persistence.WorkflowProposalStatusApplied
	_ = repo.Insert(context.Background(), applied)
	a := NewApplier(repo, &stubWriter{}, &stubGit{}, &stubReloader{}, ApplierConfig{})

	_, err := a.Apply(context.Background(), "wpr-1", "operator-x")
	if !errors.Is(err, ErrProposalNotApproved) {
		t.Fatalf("applying twice should error, got %v", err)
	}
}

// TestApplier_NotFound — missing proposal surfaces ErrNotFound
// verbatim so the API can return 404.
func TestApplier_NotFound(t *testing.T) {
	repo := newStubProposalRepo()
	a := NewApplier(repo, &stubWriter{}, &stubGit{}, &stubReloader{}, ApplierConfig{})
	_, err := a.Apply(context.Background(), "missing", "operator-x")
	if !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// TestApplier_WriterError — writer failure stops the apply BEFORE
// touching git or the DB. The proposal row stays in approved.
func TestApplier_WriterError(t *testing.T) {
	repo := newStubProposalRepo()
	_ = repo.Insert(context.Background(), approvedFixture("wpr-1", "research"))
	writer := &stubWriter{err: fmt.Errorf("disk full")}
	git := &stubGit{}
	a := NewApplier(repo, writer, git, &stubReloader{}, ApplierConfig{})

	_, err := a.Apply(context.Background(), "wpr-1", "operator-x")
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("want writer error propagated, got %v", err)
	}
	if git.gotPath != "" {
		t.Error("git should not run after writer failure")
	}
	if repo.markCalled != "" {
		t.Error("MarkApplied should not run after writer failure")
	}
}

// TestApplier_GitError — git commit failure stops the apply
// BEFORE the row is marked applied. Filesystem write happened
// but the row remains approved so the operator can retry.
func TestApplier_GitError(t *testing.T) {
	repo := newStubProposalRepo()
	_ = repo.Insert(context.Background(), approvedFixture("wpr-1", "research"))
	git := &stubGit{err: fmt.Errorf("nothing to commit")}
	a := NewApplier(repo, &stubWriter{}, git, &stubReloader{}, ApplierConfig{})

	_, err := a.Apply(context.Background(), "wpr-1", "operator-x")
	if err == nil || !strings.Contains(err.Error(), "nothing to commit") {
		t.Fatalf("want git error propagated, got %v", err)
	}
	if repo.markCalled != "" {
		t.Error("MarkApplied should not run after git failure")
	}
}

// TestApplier_NoGit_DeployedOnlyDeployment — writer returns empty
// sourcePath (no operator checkout). Apply still succeeds but
// records "no-git" as the applied_commit so operators know they
// can't roll back via revert.
func TestApplier_NoGit_DeployedOnlyDeployment(t *testing.T) {
	repo := newStubProposalRepo()
	_ = repo.Insert(context.Background(), approvedFixture("wpr-1", "research"))
	writer := &stubWriter{emptySourcePath: true}
	// Even with a git committer wired, an empty source path
	// means "no git available" — skip the commit step.
	a := NewApplier(repo, writer, &stubGit{}, &stubReloader{}, ApplierConfig{})

	got, err := a.Apply(context.Background(), "wpr-1", "operator-x")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got.AppliedCommit != "no-git" {
		t.Errorf("applied_commit should be no-git sentinel, got %q", got.AppliedCommit)
	}
}

// TestApplier_ReloaderError_DoesNotFailApply — config reload
// failure is best-effort. The filesystem write is on disk; the
// file-watcher will catch up.
func TestApplier_ReloaderError_DoesNotFailApply(t *testing.T) {
	repo := newStubProposalRepo()
	_ = repo.Insert(context.Background(), approvedFixture("wpr-1", "research"))
	a := NewApplier(repo, &stubWriter{}, &stubGit{},
		&stubReloader{err: fmt.Errorf("registry locked")}, ApplierConfig{})
	got, err := a.Apply(context.Background(), "wpr-1", "operator-x")
	if err != nil {
		t.Fatalf("reload error should not fail apply, got %v", err)
	}
	if got.Status != persistence.WorkflowProposalStatusApplied {
		t.Errorf("status should still be applied: %q", got.Status)
	}
}

// TestApplier_MarkAppliedError — MarkApplied failure propagates;
// the filesystem write has happened but the row didn't advance.
// The operator can fix the DB issue and re-apply (idempotent
// at the filesystem level — writing the same YAML is a no-op).
func TestApplier_MarkAppliedError(t *testing.T) {
	repo := newStubProposalRepo()
	_ = repo.Insert(context.Background(), approvedFixture("wpr-1", "research"))
	repo.markErr = fmt.Errorf("db down")
	a := NewApplier(repo, &stubWriter{}, &stubGit{}, &stubReloader{}, ApplierConfig{})
	_, err := a.Apply(context.Background(), "wpr-1", "operator-x")
	if err == nil || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("want MarkApplied error propagated, got %v", err)
	}
}

// TestApplier_NoProposalsRepo — caller forgot to wire the repo.
// Hard error, not a silent no-op.
func TestApplier_NoProposalsRepo(t *testing.T) {
	a := NewApplier(nil, &stubWriter{}, &stubGit{}, &stubReloader{}, ApplierConfig{})
	_, err := a.Apply(context.Background(), "wpr-1", "operator-x")
	if err == nil || !strings.Contains(err.Error(), "proposals repo") {
		t.Errorf("want missing-repo error, got %v", err)
	}
}

// TestApplier_NoWriter — same check, different dep.
func TestApplier_NoWriter(t *testing.T) {
	repo := newStubProposalRepo()
	a := NewApplier(repo, nil, &stubGit{}, &stubReloader{}, ApplierConfig{})
	_, err := a.Apply(context.Background(), "wpr-1", "operator-x")
	if err == nil || !strings.Contains(err.Error(), "writer not wired") {
		t.Errorf("want missing-writer error, got %v", err)
	}
}

// TestApplier_EmptyProposalID — guard catches the obvious input.
func TestApplier_EmptyProposalID(t *testing.T) {
	a := NewApplier(newStubProposalRepo(), &stubWriter{}, &stubGit{}, &stubReloader{}, ApplierConfig{})
	if _, err := a.Apply(context.Background(), "", "operator-x"); err == nil {
		t.Error("empty proposalID should error")
	}
}

// TestFormatCommitMessage_Subject_Truncation pins the one-line
// rule: subject line stays under typical git formatting limits
// and ellipsises long motivations.
func TestFormatCommitMessage_Subject_Truncation(t *testing.T) {
	a := &Applier{}
	p := &persistence.WorkflowProposal{
		WorkflowID: "research",
		Motivation: strings.Repeat("x", 200),
	}
	msg := a.formatCommitMessage(p, "")
	subject := strings.SplitN(msg, "\n", 2)[0]
	if len(subject) > 100 {
		t.Errorf("subject too long: %d chars: %q", len(subject), subject)
	}
	if !strings.HasSuffix(subject, "…") {
		t.Errorf("truncated subject should end with ellipsis: %q", subject)
	}
}
