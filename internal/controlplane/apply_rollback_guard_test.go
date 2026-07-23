package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// seedAppliedProposal directly inserts an APPLIED proposal row targeting
// config.yaml (bypassing the draft/approve/apply lifecycle) so tests can
// control AppliedAt precisely — per the brief, timestamps are constructed
// from time.Unix, never time.Now.
func seedAppliedProposal(t *testing.T, repo persistence.ProposalRepository, applyContent, preApplySnapshot string, appliedAt time.Time) *persistence.ControlPlaneProposal {
	t.Helper()
	p := &persistence.ControlPlaneProposal{
		ID: persistence.GenerateID("cpp"), ProjectID: "janka",
		Kind: persistence.ProposalKindConfig, BlastRadius: persistence.ProposalScopeProject,
		Title: "guard test", ApplyTarget: "config.yaml", ApplyContent: applyContent,
		Status: persistence.ProposalStatusApplied, ProposedBy: "tester", Approver: "vadim",
		PreApplySnapshot: preApplySnapshot, AppliedBy: "vadim", AppliedAt: &appliedAt,
	}
	if err := repo.Create(context.Background(), p); err != nil {
		t.Fatalf("create: %v", err)
	}
	return p
}

// TestRollbackGuard_OverwriteRefused: P1 applied, then P2 applied on top of
// the same target (later AppliedAt); disk holds P2's content. Rolling back P1
// would clobber P2's live change — must be refused, nothing written.
func TestRollbackGuard_OverwriteRefused(t *testing.T) {
	repo := newApplyRepo(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "config.yaml")
	base := time.Unix(1700000000, 0)
	t1 := base
	t2 := base.Add(time.Minute)

	p1 := seedAppliedProposal(t, repo, "p1-content\n", "pre-content\n", t1)
	p2 := seedAppliedProposal(t, repo, "p2-content\n", "p1-content\n", t2)
	if err := os.WriteFile(file, []byte(p2.ApplyContent), 0o600); err != nil {
		t.Fatalf("seed disk: %v", err)
	}

	e := &ApplyEngine{Proposals: repo, ConfigDir: dir, Logger: zerolog.Nop()}
	err := e.Rollback(context.Background(), p1.ID)
	if !errors.Is(err, ErrRollbackTargetDrifted) {
		t.Fatalf("Rollback(P1) = %v, want ErrRollbackTargetDrifted", err)
	}
	if got := readFile(t, file); got != p2.ApplyContent {
		t.Errorf("file must be unchanged, got %q", got)
	}
}

// TestRollbackGuard_IdenticalContentRefused: P2's applied content happens to
// equal P1's, so a content-only drift check would miss it — the ordering
// check must still refuse (P1 is not the live top-of-stack).
func TestRollbackGuard_IdenticalContentRefused(t *testing.T) {
	repo := newApplyRepo(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "config.yaml")
	base := time.Unix(1700000000, 0)
	t1 := base
	t2 := base.Add(time.Minute)

	const same = "same-content\n"
	p1 := seedAppliedProposal(t, repo, same, "pre-content\n", t1)
	_ = seedAppliedProposal(t, repo, same, same, t2)
	if err := os.WriteFile(file, []byte(same), 0o600); err != nil {
		t.Fatalf("seed disk: %v", err)
	}

	e := &ApplyEngine{Proposals: repo, ConfigDir: dir, Logger: zerolog.Nop()}
	err := e.Rollback(context.Background(), p1.ID)
	if !errors.Is(err, ErrRollbackTargetDrifted) {
		t.Fatalf("Rollback(P1) = %v, want ErrRollbackTargetDrifted (ordering, not content)", err)
	}
	if got := readFile(t, file); got != same {
		t.Errorf("file must be unchanged, got %q", got)
	}
}

// TestRollbackGuard_TopRollsBack: P2 is the live top (newest, disk matches
// its applied content, nothing overlapping applied after it) — rollback must
// succeed and restore P2's own pre-apply snapshot.
func TestRollbackGuard_TopRollsBack(t *testing.T) {
	repo := newApplyRepo(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "config.yaml")
	base := time.Unix(1700000000, 0)
	t1 := base
	t2 := base.Add(time.Minute)

	_ = seedAppliedProposal(t, repo, "p1-content\n", "pre-content\n", t1)
	p2 := seedAppliedProposal(t, repo, "p2-content\n", "p1-content\n", t2)
	if err := os.WriteFile(file, []byte(p2.ApplyContent), 0o600); err != nil {
		t.Fatalf("seed disk: %v", err)
	}

	e := &ApplyEngine{Proposals: repo, ConfigDir: dir, Logger: zerolog.Nop()}
	if err := e.Rollback(context.Background(), p2.ID); err != nil {
		t.Fatalf("Rollback(P2) = %v, want success", err)
	}
	if got := readFile(t, file); got != p2.PreApplySnapshot {
		t.Errorf("file = %q, want P2's pre-apply snapshot %q", got, p2.PreApplySnapshot)
	}
}

// TestRollbackGuard_HandEditDriftRefused: single APPLIED proposal, no later
// overlap, but disk was hand-edited away from what P1 applied — refuse.
func TestRollbackGuard_HandEditDriftRefused(t *testing.T) {
	repo := newApplyRepo(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "config.yaml")
	t1 := time.Unix(1700000000, 0)

	p1 := seedAppliedProposal(t, repo, "p1-content\n", "pre-content\n", t1)
	if err := os.WriteFile(file, []byte("hand-edited-content\n"), 0o600); err != nil {
		t.Fatalf("seed disk: %v", err)
	}

	e := &ApplyEngine{Proposals: repo, ConfigDir: dir, Logger: zerolog.Nop()}
	err := e.Rollback(context.Background(), p1.ID)
	if !errors.Is(err, ErrRollbackTargetDrifted) {
		t.Fatalf("Rollback(P1) = %v, want ErrRollbackTargetDrifted", err)
	}
	if got := readFile(t, file); got != "hand-edited-content\n" {
		t.Errorf("file must be unchanged, got %q", got)
	}
}

// TestRollbackGuard_LegitimateNotBlocked: single APPLIED proposal, disk still
// matches what it applied, no other overlapping applied proposal — the guard
// must not block this (the ordinary single-proposal rollback case).
func TestRollbackGuard_LegitimateNotBlocked(t *testing.T) {
	repo := newApplyRepo(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "config.yaml")
	t1 := time.Unix(1700000000, 0)

	p1 := seedAppliedProposal(t, repo, "p1-content\n", "pre-content\n", t1)
	if err := os.WriteFile(file, []byte(p1.ApplyContent), 0o600); err != nil {
		t.Fatalf("seed disk: %v", err)
	}

	e := &ApplyEngine{Proposals: repo, ConfigDir: dir, Logger: zerolog.Nop()}
	if err := e.Rollback(context.Background(), p1.ID); err != nil {
		t.Fatalf("Rollback(P1) = %v, want success", err)
	}
	if got := readFile(t, file); got != p1.PreApplySnapshot {
		t.Errorf("file = %q, want P1's pre-apply snapshot %q", got, p1.PreApplySnapshot)
	}
	p, _ := repo.GetByID(context.Background(), p1.ID)
	if p.Status != persistence.ProposalStatusRolledBack {
		t.Errorf("status = %s, want ROLLED_BACK", p.Status)
	}
}

// TestRollbackGuard_TargetsOverlap covers targetsOverlap's overlapping vs
// disjoint cases directly.
func TestRollbackGuard_TargetsOverlap(t *testing.T) {
	if !targetsOverlap([]string{"a.yaml", "b.yaml"}, []string{"b.yaml", "c.yaml"}) {
		t.Error("expected overlap on b.yaml")
	}
	if targetsOverlap([]string{"a.yaml"}, []string{"b.yaml"}) {
		t.Error("expected no overlap")
	}
	if targetsOverlap(nil, []string{"a.yaml"}) {
		t.Error("expected no overlap against nil")
	}
}

// TestRollbackGuard_EqualTimestampTieBreak: two overlapping APPLIED proposals
// applied at the SAME instant (AppliedAt equal) — the inclusive-`>=` ordering
// check would flag each as superseding the other (both refused, deadlock).
// The strict-after + ID tie-break must instead treat exactly one as the live
// top: the lower-ID one refused, the higher-ID one permitted.
func TestRollbackGuard_EqualTimestampTieBreak(t *testing.T) {
	repo := newApplyRepo(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "config.yaml")
	at := time.Unix(1700000000, 0)
	const content = "same-content\n"

	pLow := &persistence.ControlPlaneProposal{
		ID: "cpp_aaa", ProjectID: "janka", Kind: persistence.ProposalKindConfig,
		BlastRadius: persistence.ProposalScopeProject, Title: "tie low",
		ApplyTarget: "config.yaml", ApplyContent: content,
		Status: persistence.ProposalStatusApplied, ProposedBy: "tester", Approver: "vadim",
		PreApplySnapshot: "pre-low\n", AppliedBy: "vadim", AppliedAt: &at,
	}
	if err := repo.Create(context.Background(), pLow); err != nil {
		t.Fatalf("create low: %v", err)
	}
	pHigh := &persistence.ControlPlaneProposal{
		ID: "cpp_bbb", ProjectID: "janka", Kind: persistence.ProposalKindConfig,
		BlastRadius: persistence.ProposalScopeProject, Title: "tie high",
		ApplyTarget: "config.yaml", ApplyContent: content,
		Status: persistence.ProposalStatusApplied, ProposedBy: "tester", Approver: "vadim",
		PreApplySnapshot: "pre-high\n", AppliedBy: "vadim", AppliedAt: &at,
	}
	if err := repo.Create(context.Background(), pHigh); err != nil {
		t.Fatalf("create high: %v", err)
	}
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatalf("seed disk: %v", err)
	}

	e := &ApplyEngine{Proposals: repo, ConfigDir: dir, Logger: zerolog.Nop()}
	if err := e.Rollback(context.Background(), pLow.ID); !errors.Is(err, ErrRollbackTargetDrifted) {
		t.Fatalf("Rollback(low ID) = %v, want ErrRollbackTargetDrifted", err)
	}
	if err := e.Rollback(context.Background(), pHigh.ID); err != nil {
		t.Fatalf("Rollback(high ID) = %v, want success (tie-break top)", err)
	}
	if got := readFile(t, file); got != pHigh.PreApplySnapshot {
		t.Errorf("file = %q, want high ID's pre-apply snapshot %q", got, pHigh.PreApplySnapshot)
	}
}

// TestRollbackGuard_TwoStepRecovery locks in the core invariant: rolling back
// the live top of an overlapping stack frees the one beneath it, with no
// persisted "who's on top" state — the ordering check is recomputed from the
// ledger + disk every time.
func TestRollbackGuard_TwoStepRecovery(t *testing.T) {
	repo := newApplyRepo(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "config.yaml")
	base := time.Unix(1700000000, 0)
	t1 := base
	t2 := base.Add(time.Minute)

	p1 := seedAppliedProposal(t, repo, "p1-content\n", "pre-p1-content\n", t1)
	p2 := seedAppliedProposal(t, repo, "p2-content\n", p1.ApplyContent, t2)
	if err := os.WriteFile(file, []byte(p2.ApplyContent), 0o600); err != nil {
		t.Fatalf("seed disk: %v", err)
	}

	e := &ApplyEngine{Proposals: repo, ConfigDir: dir, Logger: zerolog.Nop()}

	// (1) P1 is not the live top (P2 overlaps + applied later) — refused.
	if err := e.Rollback(context.Background(), p1.ID); !errors.Is(err, ErrRollbackTargetDrifted) {
		t.Fatalf("Rollback(P1) step1 = %v, want ErrRollbackTargetDrifted", err)
	}

	// (2) P2 is the live top — rolls back, restoring its pre-image (P1's
	// applied content).
	if err := e.Rollback(context.Background(), p2.ID); err != nil {
		t.Fatalf("Rollback(P2) step2 = %v, want success", err)
	}
	if got := readFile(t, file); got != p1.ApplyContent {
		t.Errorf("after Rollback(P2), file = %q, want P1's applied content %q", got, p1.ApplyContent)
	}

	// Rollback(P2) already flipped it to ROLLED_BACK — confirm it's no longer
	// APPLIED before the next step (it drops out of the ordering check).
	p2Row, err := repo.GetByID(context.Background(), p2.ID)
	if err != nil {
		t.Fatalf("get p2: %v", err)
	}
	if p2Row.Status != persistence.ProposalStatusRolledBack {
		t.Fatalf("p2 status = %s, want ROLLED_BACK", p2Row.Status)
	}

	// (3) P1 is now the (only) live top for its target — rolls back to its
	// own pre-apply snapshot.
	if err := e.Rollback(context.Background(), p1.ID); err != nil {
		t.Fatalf("Rollback(P1) step3 = %v, want success", err)
	}
	if got := readFile(t, file); got != p1.PreApplySnapshot {
		t.Errorf("after Rollback(P1), file = %q, want P1's pre-apply snapshot %q", got, p1.PreApplySnapshot)
	}
}

// TestRollbackGuard_CorruptApplyOpsFailsClosed: an APPLIED proposal whose
// ApplyOps is non-empty but not valid JSON must refuse rollback rather than
// silently treating it as having no targets (which would fall through the
// ordering/drift checks entirely and report "safe").
func TestRollbackGuard_CorruptApplyOpsFailsClosed(t *testing.T) {
	repo := newApplyRepo(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "config.yaml")
	at := time.Unix(1700000000, 0)

	p := &persistence.ControlPlaneProposal{
		ID: persistence.GenerateID("cpp"), ProjectID: "janka", Kind: persistence.ProposalKindConfig,
		BlastRadius: persistence.ProposalScopeProject, Title: "corrupt ops",
		ApplyTarget: "config.yaml", ApplyContent: "content\n", ApplyOps: "{not valid json",
		Status: persistence.ProposalStatusApplied, ProposedBy: "tester", Approver: "vadim",
		PreApplySnapshot: "pre-content\n", AppliedBy: "vadim", AppliedAt: &at,
	}
	if err := repo.Create(context.Background(), p); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := os.WriteFile(file, []byte("content\n"), 0o600); err != nil {
		t.Fatalf("seed disk: %v", err)
	}

	e := &ApplyEngine{Proposals: repo, ConfigDir: dir, Logger: zerolog.Nop()}
	err := e.Rollback(context.Background(), p.ID)
	if !errors.Is(err, ErrRollbackTargetDrifted) {
		t.Fatalf("Rollback = %v, want ErrRollbackTargetDrifted (fail closed on corrupt ApplyOps)", err)
	}
	if got := readFile(t, file); got != "content\n" {
		t.Errorf("file must be unchanged, got %q", got)
	}
}

// TestRollbackGuard_ProposalTargets covers proposalTargets for a single-op
// (ApplyTarget) proposal vs a multi-op (ApplyOps JSON) proposal.
func TestRollbackGuard_ProposalTargets(t *testing.T) {
	single := &persistence.ControlPlaneProposal{ApplyTarget: "config.yaml"}
	got := proposalTargets(single)
	if len(got) != 1 || got[0] != "config.yaml" {
		t.Errorf("single-op targets = %v, want [config.yaml]", got)
	}

	ops := []applyFileOp{
		{Op: applyOpReplace, Path: "config.yaml", Content: "x"},
		{Op: applyOpCreate, Path: "projects/digest.yaml", Content: "y"},
	}
	raw, _ := json.Marshal(ops)
	multi := &persistence.ControlPlaneProposal{ApplyOps: string(raw)}
	got = proposalTargets(multi)
	if len(got) != 2 || got[0] != "config.yaml" || got[1] != "projects/digest.yaml" {
		t.Errorf("multi-op targets = %v, want [config.yaml projects/digest.yaml]", got)
	}

	reviewOnly := &persistence.ControlPlaneProposal{}
	if got := proposalTargets(reviewOnly); got != nil {
		t.Errorf("review-only targets = %v, want nil", got)
	}
}
