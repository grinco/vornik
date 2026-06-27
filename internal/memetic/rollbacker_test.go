package memetic

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

type stubGitReverter struct {
	gotSHA, gotMsg, gotName, gotEmail string
	revertSHA                         string
	err                               error
}

func (g *stubGitReverter) Revert(_ context.Context, sha, msg, name, email string) (string, error) {
	g.gotSHA, g.gotMsg, g.gotName, g.gotEmail = sha, msg, name, email
	if g.err != nil {
		return "", g.err
	}
	if g.revertSHA == "" {
		return "revert-sha", nil
	}
	return g.revertSHA, nil
}

func appliedFixture(id, workflowID, appliedSHA string) *persistence.WorkflowProposal {
	return &persistence.WorkflowProposal{
		ID: id, WorkflowID: workflowID,
		Status:         persistence.WorkflowProposalStatusApplied,
		ProposalYAML:   "yaml",
		Motivation:     "m",
		EvidenceRunIDs: []string{"r-1", "r-2", "r-3"},
		Confidence:     0.8,
		ArchitectModel: "m",
		AppliedCommit:  appliedSHA,
		CreatedAt:      time.Now().UTC(),
	}
}

func TestRollbacker_HappyPath(t *testing.T) {
	repo := newStubProposalRepo()
	_ = repo.Insert(context.Background(), appliedFixture("wpr-1", "research", "abc1234"))
	git := &stubGitReverter{revertSHA: "def5678"}
	reloader := &stubReloader{}
	r := NewRollbacker(repo, git, reloader, RollbackerConfig{AuthorName: "vornik"})

	got, err := r.Rollback(context.Background(), "wpr-1", "operator-y")
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if got.Status != persistence.WorkflowProposalStatusRolledBack {
		t.Errorf("status: %q", got.Status)
	}
	if got.RollbackCommit != "def5678" {
		t.Errorf("rollback_commit: %q", got.RollbackCommit)
	}
	if git.gotSHA != "abc1234" {
		t.Errorf("revert should target applied_commit, got %q", git.gotSHA)
	}
	if !strings.Contains(git.gotMsg, "workflow(research)") {
		t.Errorf("revert message: %q", git.gotMsg)
	}
	if !strings.Contains(git.gotMsg, "operator-y") {
		t.Errorf("revert message should include operator: %q", git.gotMsg)
	}
	if reloader.called != 1 {
		t.Errorf("reloader should fire once, got %d", reloader.called)
	}
}

func TestRollbacker_NotApplied(t *testing.T) {
	repo := newStubProposalRepo()
	approved := appliedFixture("wpr-1", "research", "abc1234")
	approved.Status = persistence.WorkflowProposalStatusApproved
	_ = repo.Insert(context.Background(), approved)
	r := NewRollbacker(repo, &stubGitReverter{}, &stubReloader{}, RollbackerConfig{})

	_, err := r.Rollback(context.Background(), "wpr-1", "operator-x")
	if !errors.Is(err, ErrProposalNotApplied) {
		t.Fatalf("want ErrProposalNotApplied, got %v", err)
	}
}

// TestRollbacker_NoGitHistory — applied_commit is the "no-git"
// sentinel (Slice 4 deployed-only deployment). Rollback fails
// early with a clear message rather than asking git to revert a
// non-existent SHA.
func TestRollbacker_NoGitHistory(t *testing.T) {
	repo := newStubProposalRepo()
	_ = repo.Insert(context.Background(), appliedFixture("wpr-1", "research", "no-git"))
	r := NewRollbacker(repo, &stubGitReverter{}, &stubReloader{}, RollbackerConfig{})

	_, err := r.Rollback(context.Background(), "wpr-1", "operator-x")
	if err == nil || !strings.Contains(err.Error(), "no git commit") {
		t.Fatalf("want no-git-history error, got %v", err)
	}
}

// TestRollbacker_NoGitWired — applier wrote a real commit but
// no GitReverter is wired. Hard fail; we don't silently skip the
// revert step because that would leave the filesystem out of
// sync with the row state.
func TestRollbacker_NoGitWired(t *testing.T) {
	repo := newStubProposalRepo()
	_ = repo.Insert(context.Background(), appliedFixture("wpr-1", "research", "abc1234"))
	r := NewRollbacker(repo, nil, &stubReloader{}, RollbackerConfig{})

	_, err := r.Rollback(context.Background(), "wpr-1", "operator-x")
	if err == nil || !strings.Contains(err.Error(), "git reverter not wired") {
		t.Fatalf("want no-git-wired error, got %v", err)
	}
}

func TestRollbacker_NotFound(t *testing.T) {
	repo := newStubProposalRepo()
	r := NewRollbacker(repo, &stubGitReverter{}, &stubReloader{}, RollbackerConfig{})
	_, err := r.Rollback(context.Background(), "missing", "operator-x")
	if !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// TestRollbacker_GitRevertError — git revert fails (e.g. merge
// conflict against current HEAD). Error propagates; row stays
// in applied so the operator can retry.
func TestRollbacker_GitRevertError(t *testing.T) {
	repo := newStubProposalRepo()
	_ = repo.Insert(context.Background(), appliedFixture("wpr-1", "research", "abc1234"))
	git := &stubGitReverter{err: fmt.Errorf("conflict")}
	r := NewRollbacker(repo, git, &stubReloader{}, RollbackerConfig{})

	_, err := r.Rollback(context.Background(), "wpr-1", "operator-x")
	if err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("want git error propagated, got %v", err)
	}
}

func TestRollbacker_EmptyProposalID(t *testing.T) {
	r := NewRollbacker(newStubProposalRepo(), &stubGitReverter{}, &stubReloader{}, RollbackerConfig{})
	if _, err := r.Rollback(context.Background(), "", "operator-x"); err == nil {
		t.Error("empty proposalID should error")
	}
}

func TestRollbacker_NoProposalsRepo(t *testing.T) {
	r := NewRollbacker(nil, &stubGitReverter{}, &stubReloader{}, RollbackerConfig{})
	_, err := r.Rollback(context.Background(), "wpr-1", "operator-x")
	if err == nil || !strings.Contains(err.Error(), "proposals repo") {
		t.Errorf("want missing-repo error, got %v", err)
	}
}

// ARCHITECT PAUSE ----------------------------------------------

func TestArchitect_Propose_Paused(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Paused = true
	sink := &stubProposalSink{}
	a := New(&stubProvider{}, &stubTelemetry{}, &stubWorkflowSource{}, nil, sink, cfg)
	_, err := a.Propose(context.Background(), "wf-x")
	if !errors.Is(err, ErrArchitectPaused) {
		t.Fatalf("want ErrArchitectPaused, got %v", err)
	}
	if sink.inserted != nil {
		t.Error("paused architect must not insert a proposal")
	}
}

// EVIDENCE-REQUIRED PINNING ------------------------------------

// TestArchitect_EvidenceMinimum_DesignPinned — the design doc
// pins evidence ≥ 3. This test guards the floor so a future
// "let's lower it to 1 for fast iteration" change has to update
// both the doc AND this test.
func TestArchitect_EvidenceMinimum_DesignPinned(t *testing.T) {
	if DefaultConfig().MinEvidenceRunIDs != 3 {
		t.Errorf("design pins evidence floor at 3, got %d",
			DefaultConfig().MinEvidenceRunIDs)
	}
}
