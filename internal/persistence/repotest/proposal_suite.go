package repotest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// RunProposalSuite is the backend-agnostic contract for
// persistence.ProposalRepository (control-plane proposal ledger, Phase 1).
// Both Postgres and SQLite run it.
func RunProposalSuite(t *testing.T, repo persistence.ProposalRepository) {
	t.Helper()
	t.Run("Create_then_GetByID_round_trips", func(t *testing.T) { proposalRoundTrip(t, repo) })
	t.Run("daemon_scope_project_is_NULL", func(t *testing.T) { proposalDaemonScope(t, repo) })
	t.Run("Create_rejects_oversized_field", func(t *testing.T) { proposalOversizeField(t, repo) })
	t.Run("GetByID_unknown_is_ErrNotFound", func(t *testing.T) { proposalGetUnknown(t, repo) })
	t.Run("List_filters_project_and_status", func(t *testing.T) { proposalListFilters(t, repo) })
	t.Run("SetStatus_approve_records_approver", func(t *testing.T) { proposalApprove(t, repo) })
	t.Run("SetStatus_rejects_self_approve", func(t *testing.T) { proposalSelfApprove(t, repo) })
	t.Run("SetStatus_rejects_non_draft", func(t *testing.T) { proposalNonDraft(t, repo) })
	t.Run("SetStatus_reject_transition", func(t *testing.T) { proposalReject(t, repo) })
}

func newTestProposal(id, project string) *persistence.ControlPlaneProposal {
	return &persistence.ControlPlaneProposal{
		ID: id, ProjectID: project,
		Kind: persistence.ProposalKindConfig, BlastRadius: persistence.ProposalScopeProject,
		Title: "bump scraper timeout", Diff: "-timeout: 30\n+timeout: 90",
		Rationale: "web_fetch timing out", Evidence: `{"metric":"tool_error_rate","v":0.4}`,
		ProposedBy: "agent-exec-1",
	}
}

func mustCreateProposal(t *testing.T, repo persistence.ProposalRepository, p *persistence.ControlPlaneProposal) {
	t.Helper()
	if err := repo.Create(context.Background(), p); err != nil {
		t.Fatalf("Create %s: %v", p.ID, err)
	}
}

func proposalRoundTrip(t *testing.T, repo persistence.ProposalRepository) {
	ctx := context.Background()
	mustCreateProposal(t, repo, newTestProposal("pr-1", "p1"))
	got, err := repo.GetByID(ctx, "pr-1")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.ProjectID != "p1" || got.Kind != persistence.ProposalKindConfig ||
		got.BlastRadius != persistence.ProposalScopeProject || got.Title != "bump scraper timeout" {
		t.Errorf("core fields mismatch: %+v", got)
	}
	if got.Status != persistence.ProposalStatusDraft {
		t.Errorf("new proposal must default to DRAFT, got %s", got.Status)
	}
	if got.ProposedBy != "agent-exec-1" || got.Diff == "" || got.Evidence == "" {
		t.Errorf("proposer/diff/evidence not persisted: %+v", got)
	}
	if got.CreatedAt.IsZero() {
		t.Error("created_at must be stamped")
	}
	if got.DecidedAt != nil || got.AppliedAt != nil {
		t.Error("undecided proposal must have nil decided_at/applied_at")
	}
}

func proposalDaemonScope(t *testing.T, repo persistence.ProposalRepository) {
	ctx := context.Background()
	d := newTestProposal("pr-daemon", "")
	d.BlastRadius = persistence.ProposalScopeDaemon
	mustCreateProposal(t, repo, d)
	got, err := repo.GetByID(ctx, "pr-daemon")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.ProjectID != "" {
		t.Errorf("daemon-scope proposal must round-trip empty project (NULL), got %q", got.ProjectID)
	}
}

func proposalOversizeField(t *testing.T, repo persistence.ProposalRepository) {
	p := newTestProposal("pr-big", "p1")
	p.Diff = strings.Repeat("x", persistence.ProposalMaxFieldBytes+1)
	err := repo.Create(context.Background(), p)
	if !errors.Is(err, persistence.ErrProposalFieldTooLarge) {
		t.Fatalf("expected ErrProposalFieldTooLarge, got %v", err)
	}
}

func proposalGetUnknown(t *testing.T, repo persistence.ProposalRepository) {
	_, err := repo.GetByID(context.Background(), "pr-nope")
	if !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func proposalListFilters(t *testing.T, repo persistence.ProposalRepository) {
	ctx := context.Background()
	mustCreateProposal(t, repo, newTestProposal("lf-a", "projX"))
	b := newTestProposal("lf-b", "projX")
	mustCreateProposal(t, repo, b)
	_ = repo.SetStatus(ctx, "lf-b", persistence.ProposalStatusApproved, "human-op")
	mustCreateProposal(t, repo, newTestProposal("lf-c", "projY"))

	// Filter by project.
	got, err := repo.List(ctx, persistence.ProposalListFilter{ProjectID: "projX"})
	if err != nil {
		t.Fatalf("List projX: %v", err)
	}
	ids := proposalIDs(got)
	if !ids["lf-a"] || !ids["lf-b"] || ids["lf-c"] {
		t.Errorf("project filter wrong: %v", keys(ids))
	}
	// Filter by status.
	got, _ = repo.List(ctx, persistence.ProposalListFilter{Statuses: []string{persistence.ProposalStatusDraft}})
	ids = proposalIDs(got)
	if !ids["lf-a"] || ids["lf-b"] {
		t.Errorf("status filter must include DRAFT lf-a, exclude APPROVED lf-b: %v", keys(ids))
	}
}

func proposalApprove(t *testing.T, repo persistence.ProposalRepository) {
	ctx := context.Background()
	mustCreateProposal(t, repo, newTestProposal("ap-1", "p1"))
	if err := repo.SetStatus(ctx, "ap-1", persistence.ProposalStatusApproved, "human-op"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	got, _ := repo.GetByID(ctx, "ap-1")
	if got.Status != persistence.ProposalStatusApproved || got.Approver != "human-op" {
		t.Errorf("approve did not record status/approver: %+v", got)
	}
	if got.DecidedAt == nil {
		t.Error("approve must stamp decided_at")
	}
}

func proposalSelfApprove(t *testing.T, repo persistence.ProposalRepository) {
	ctx := context.Background()
	p := newTestProposal("sa-1", "p1")
	p.ProposedBy = "agent-x"
	mustCreateProposal(t, repo, p)
	err := repo.SetStatus(ctx, "sa-1", persistence.ProposalStatusApproved, "agent-x")
	if !errors.Is(err, persistence.ErrProposalSelfApprove) {
		t.Fatalf("self-approval must be rejected, got %v", err)
	}
	got, _ := repo.GetByID(ctx, "sa-1")
	if got.Status != persistence.ProposalStatusDraft {
		t.Errorf("rejected self-approval must leave it DRAFT, got %s", got.Status)
	}
}

func proposalNonDraft(t *testing.T, repo persistence.ProposalRepository) {
	ctx := context.Background()
	mustCreateProposal(t, repo, newTestProposal("nd-1", "p1"))
	if err := repo.SetStatus(ctx, "nd-1", persistence.ProposalStatusApproved, "human-op"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	// A decided proposal can't be re-decided.
	err := repo.SetStatus(ctx, "nd-1", persistence.ProposalStatusRejected, "human-op")
	if !errors.Is(err, persistence.ErrProposalNotDraft) {
		t.Fatalf("re-deciding a non-DRAFT proposal must fail, got %v", err)
	}
}

func proposalReject(t *testing.T, repo persistence.ProposalRepository) {
	ctx := context.Background()
	p := newTestProposal("rj-1", "p1")
	p.ProposedBy = "agent-y"
	mustCreateProposal(t, repo, p)
	// Self-reject is allowed (the proposer withdrawing); only APPROVE is
	// self-guarded.
	if err := repo.SetStatus(ctx, "rj-1", persistence.ProposalStatusRejected, "agent-y"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	got, _ := repo.GetByID(ctx, "rj-1")
	if got.Status != persistence.ProposalStatusRejected {
		t.Errorf("expected REJECTED, got %s", got.Status)
	}
}

func proposalIDs(ps []*persistence.ControlPlaneProposal) map[string]bool {
	m := make(map[string]bool, len(ps))
	for _, p := range ps {
		m[p.ID] = true
	}
	return m
}
