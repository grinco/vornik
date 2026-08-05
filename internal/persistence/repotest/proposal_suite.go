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
	t.Run("Whole_file_content_gets_its_own_cap", func(t *testing.T) { proposalWholeFileContent(t, repo) })
	t.Run("GetByID_unknown_is_ErrNotFound", func(t *testing.T) { proposalGetUnknown(t, repo) })
	t.Run("List_filters_project_and_status", func(t *testing.T) { proposalListFilters(t, repo) })
	t.Run("SetStatus_approve_records_approver", func(t *testing.T) { proposalApprove(t, repo) })
	t.Run("SetStatus_rejects_self_approve", func(t *testing.T) { proposalSelfApprove(t, repo) })
	t.Run("SetStatus_rejects_non_draft", func(t *testing.T) { proposalNonDraft(t, repo) })
	t.Run("SetStatus_reject_transition", func(t *testing.T) { proposalReject(t, repo) })
	t.Run("apply_fields_round_trip", func(t *testing.T) { proposalApplyFields(t, repo) })
	t.Run("MarkApplied_only_from_approved", func(t *testing.T) { proposalMarkApplied(t, repo) })
	t.Run("MarkRolledBack_only_from_applied", func(t *testing.T) { proposalMarkRolledBack(t, repo) })
	t.Run("MarkRegressed_from_applied", func(t *testing.T) { proposalMarkRegressedFromApplied(t, repo) })
	t.Run("MarkRegressed_from_rolled_back", func(t *testing.T) { proposalMarkRegressedFromRolledBack(t, repo) })
	t.Run("MarkRegressed_rejects_draft", func(t *testing.T) { proposalMarkRegressedRejectsDraft(t, repo) })
}

// proposalMarkRegressedFromApplied pins APPLIED → REGRESSED (design §4.4).
func proposalMarkRegressedFromApplied(t *testing.T, repo persistence.ProposalRepository) {
	ctx := context.Background()
	mustCreateProposal(t, repo, newTestProposal("mreg-ap", "p1"))
	_ = repo.SetStatus(ctx, "mreg-ap", persistence.ProposalStatusApproved, "human")
	_ = repo.MarkApplied(ctx, "mreg-ap", "vadim", "snap")
	if err := repo.MarkRegressed(ctx, "mreg-ap", "A1 quality regressed"); err != nil {
		t.Fatalf("MarkRegressed(APPLIED): %v", err)
	}
	got, _ := repo.GetByID(ctx, "mreg-ap")
	if got.Status != persistence.ProposalStatusRegressed {
		t.Fatalf("expected REGRESSED, got %s", got.Status)
	}
}

// proposalMarkRegressedFromRolledBack pins ROLLED_BACK → REGRESSED — the
// transition the canary guard's trip path actually executes (Rollback first,
// then the best-effort badge). Design §4.4/§4.5 C2.
func proposalMarkRegressedFromRolledBack(t *testing.T, repo persistence.ProposalRepository) {
	ctx := context.Background()
	mustCreateProposal(t, repo, newTestProposal("mreg-rb", "p1"))
	_ = repo.SetStatus(ctx, "mreg-rb", persistence.ProposalStatusApproved, "human")
	_ = repo.MarkApplied(ctx, "mreg-rb", "vadim", "snap")
	if err := repo.MarkRolledBack(ctx, "mreg-rb"); err != nil {
		t.Fatalf("MarkRolledBack: %v", err)
	}
	if err := repo.MarkRegressed(ctx, "mreg-rb", "canary trip"); err != nil {
		t.Fatalf("MarkRegressed(ROLLED_BACK): %v", err)
	}
	got, _ := repo.GetByID(ctx, "mreg-rb")
	if got.Status != persistence.ProposalStatusRegressed {
		t.Fatalf("expected REGRESSED, got %s", got.Status)
	}
}

// proposalMarkRegressedRejectsDraft pins the guard: neither APPLIED nor
// ROLLED_BACK → ErrProposalNotRegressable.
func proposalMarkRegressedRejectsDraft(t *testing.T, repo persistence.ProposalRepository) {
	ctx := context.Background()
	mustCreateProposal(t, repo, newTestProposal("mreg-dr", "p1"))
	if err := repo.MarkRegressed(ctx, "mreg-dr", "nope"); !errors.Is(err, persistence.ErrProposalNotRegressable) {
		t.Fatalf("MarkRegressed on DRAFT must fail ErrProposalNotRegressable, got %v", err)
	}
}

func proposalApplyFields(t *testing.T, repo persistence.ProposalRepository) {
	ctx := context.Background()
	p := newTestProposal("af-1", "p1")
	p.ApplyTarget = "config.yaml"
	p.ApplyContent = "server:\n  address: :8080\n"
	p.ApplyOps = `[{"op":"create","path":"projects/x.yaml","content":"id: x\n"}]`
	mustCreateProposal(t, repo, p)
	got, err := repo.GetByID(ctx, "af-1")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.ApplyTarget != "config.yaml" || got.ApplyContent != "server:\n  address: :8080\n" {
		t.Errorf("apply fields did not round-trip: %+v", got)
	}
	if got.ApplyOps != p.ApplyOps {
		t.Errorf("apply_ops did not round-trip: got %q want %q", got.ApplyOps, p.ApplyOps)
	}
	if got.AppliedBy != "" || got.AppliedAt != nil {
		t.Errorf("un-applied proposal must have empty applied_by/at: %+v", got)
	}
}

func proposalMarkApplied(t *testing.T, repo persistence.ProposalRepository) {
	ctx := context.Background()
	// Not-approved (DRAFT) → refused.
	mustCreateProposal(t, repo, newTestProposal("ma-draft", "p1"))
	if err := repo.MarkApplied(ctx, "ma-draft", "vadim", "snap"); !errors.Is(err, persistence.ErrProposalNotApproved) {
		t.Fatalf("MarkApplied on DRAFT must fail ErrProposalNotApproved, got %v", err)
	}
	// Approve, then apply.
	mustCreateProposal(t, repo, newTestProposal("ma-1", "p1"))
	if err := repo.SetStatus(ctx, "ma-1", persistence.ProposalStatusApproved, "human"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := repo.MarkApplied(ctx, "ma-1", "vadim", "OLD_FILE_BYTES"); err != nil {
		t.Fatalf("MarkApplied: %v", err)
	}
	got, _ := repo.GetByID(ctx, "ma-1")
	if got.Status != persistence.ProposalStatusApplied || got.AppliedBy != "vadim" ||
		got.PreApplySnapshot != "OLD_FILE_BYTES" || got.AppliedAt == nil {
		t.Fatalf("MarkApplied did not record status/appliedBy/snapshot/at: %+v", got)
	}
	// Second apply (now APPLIED) → refused (idempotent single-apply).
	if err := repo.MarkApplied(ctx, "ma-1", "vadim", "snap2"); !errors.Is(err, persistence.ErrProposalNotApproved) {
		t.Fatalf("re-apply must fail ErrProposalNotApproved, got %v", err)
	}
}

func proposalMarkRolledBack(t *testing.T, repo persistence.ProposalRepository) {
	ctx := context.Background()
	mustCreateProposal(t, repo, newTestProposal("mr-1", "p1"))
	// Not applied yet → refused.
	if err := repo.MarkRolledBack(ctx, "mr-1"); !errors.Is(err, persistence.ErrProposalNotApplied) {
		t.Fatalf("rollback of non-APPLIED must fail ErrProposalNotApplied, got %v", err)
	}
	// Approve → apply → rollback.
	_ = repo.SetStatus(ctx, "mr-1", persistence.ProposalStatusApproved, "human")
	_ = repo.MarkApplied(ctx, "mr-1", "vadim", "snap")
	if err := repo.MarkRolledBack(ctx, "mr-1"); err != nil {
		t.Fatalf("MarkRolledBack: %v", err)
	}
	got, _ := repo.GetByID(ctx, "mr-1")
	if got.Status != persistence.ProposalStatusRolledBack {
		t.Errorf("expected ROLLED_BACK, got %s", got.Status)
	}
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

// proposalWholeFileContent pins the 2026-08-05 defect: a whole-FILE field
// (apply_content, and the pre-apply snapshot taken from it) is not free-text
// UI prose and must not share the 64 KiB free-text cap. The live config.yaml
// is 81 KB, so under the old shared cap the hub created an MCP-add proposal
// that Apply then refused with ErrContentTooLarge before any write — an
// un-appliable proposal, reported as the generic "apply failed".
func proposalWholeFileContent(t *testing.T, repo persistence.ProposalRepository) {
	ctx := context.Background()
	// A realistic config.yaml — over the free-text cap, under the content cap.
	big := strings.Repeat("y", persistence.ProposalMaxFieldBytes+20_000)

	p := newTestProposal("pr-wholefile", "p1")
	p.ApplyTarget = "config.yaml"
	p.ApplyContent = big
	if err := repo.Create(ctx, p); err != nil {
		t.Fatalf("a config-sized apply_content must be storable, got %v", err)
	}
	got, err := repo.GetByID(ctx, "pr-wholefile")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if len(got.ApplyContent) != len(big) {
		t.Fatalf("apply_content must round-trip whole, got %d bytes want %d", len(got.ApplyContent), len(big))
	}

	// The snapshot is the pre-image of that same file, so it needs the same
	// headroom — otherwise MarkApplied fails AFTER the write and the engine
	// reverses a good apply.
	if err := repo.SetStatus(ctx, "pr-wholefile", persistence.ProposalStatusApproved, "approver"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := repo.MarkApplied(ctx, "pr-wholefile", "op", big); err != nil {
		t.Fatalf("a config-sized snapshot must be storable, got %v", err)
	}

	// Still bounded: past the content cap it is refused, not truncated.
	over := newTestProposal("pr-overcontent", "p1")
	over.ApplyContent = strings.Repeat("z", persistence.ProposalMaxContentBytes+1)
	if err := repo.Create(ctx, over); !errors.Is(err, persistence.ErrProposalFieldTooLarge) {
		t.Fatalf("apply_content over the content cap must be rejected, got %v", err)
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
	// An APPROVED proposal can't be re-APPROVED.
	if err := repo.SetStatus(ctx, "nd-1", persistence.ProposalStatusApproved, "human-op2"); !errors.Is(err, persistence.ErrProposalNotDraft) {
		t.Fatalf("re-approving a non-DRAFT proposal must fail, got %v", err)
	}
	// But an APPROVED proposal CAN be withdrawn (rejected) — e.g. an approved
	// change that turned out un-appliable and was superseded by a re-draft.
	if err := repo.SetStatus(ctx, "nd-1", persistence.ProposalStatusRejected, "human-op"); err != nil {
		t.Fatalf("withdrawing an APPROVED proposal must succeed, got %v", err)
	}
	got, _ := repo.GetByID(ctx, "nd-1")
	if got.Status != persistence.ProposalStatusRejected {
		t.Fatalf("withdrawn proposal must be REJECTED, got %s", got.Status)
	}
	// A terminal (REJECTED) proposal can't be re-decided.
	if err := repo.SetStatus(ctx, "nd-1", persistence.ProposalStatusRejected, "human-op"); !errors.Is(err, persistence.ErrProposalNotPending) {
		t.Fatalf("re-deciding a terminal proposal must fail ErrProposalNotPending, got %v", err)
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
