package controlplane

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// Durable operator rejection (LLD 2026-08-10 §7).
//
// Incident: the identical "reclaim over-provisioned timeout for ingest on
// easeit-companion" proposal was filed four times and REJECTED three (07-30,
// 07-31, 08-05) before one was approved and took the step down.
// fileRenderedProposal deduped only against open DRAFTs, so a rejection
// suppressed nothing and the detector re-filed on every fresh streak.

// seedRejected files a proposal under title and moves it to REJECTED, which is
// what an operator clicking Reject does.
func seedRejected(t *testing.T, repo persistence.ProposalRepository, project, title string) {
	t.Helper()
	ctx := context.Background()
	p := &persistence.ControlPlaneProposal{
		ID: "cpp_seed_" + title, ProjectID: project, Kind: persistence.ProposalKindConfig,
		BlastRadius: persistence.ProposalScopeProject, Title: title,
		Status: persistence.ProposalStatusDraft, ProposedBy: "tune-detector",
	}
	if err := repo.Create(ctx, p); err != nil {
		t.Fatalf("seed create: %v", err)
	}
	if err := repo.SetStatus(ctx, p.ID, persistence.ProposalStatusRejected, "session:admin"); err != nil {
		t.Fatalf("seed reject: %v", err)
	}
}

func TestFileRendered_SuppressedWhileRejectionIsInCooldown(t *testing.T) {
	repo := newTuneTestRepo(t)
	w := newReclaimWorker(t, repo, nil)
	w.RejectCooldown = 168 * time.Hour

	title := tuneTimeoutReclaimTitle("p1", "implement")
	seedRejected(t, repo, "p1", title)
	before := draftCount(t, repo)

	w.fileRendered(context.Background(), "p1", title, "rationale", "{}", "tune-detector",
		&RenderedChange{ApplyTarget: "configs/workflows/dev-pipeline.md", ApplyContent: "x", Summary: "s"})

	if got := draftCount(t, repo); got != before {
		t.Fatalf("filed a proposal whose title was rejected 0h ago under a 168h cooldown (%d -> %d)", before, got)
	}
}

func TestFileRendered_FilesOnceTheRejectionCooldownLapses(t *testing.T) {
	repo := newTuneTestRepo(t)
	w := newReclaimWorker(t, repo, nil)
	w.RejectCooldown = time.Nanosecond // already lapsed

	title := tuneTimeoutReclaimTitle("p1", "implement")
	seedRejected(t, repo, "p1", title)
	before := draftCount(t, repo)

	w.fileRendered(context.Background(), "p1", title, "rationale", "{}", "tune-detector",
		&RenderedChange{ApplyTarget: "configs/workflows/dev-pipeline.md", ApplyContent: "x", Summary: "s"})

	if got := draftCount(t, repo); got != before+1 {
		t.Fatalf("cooldown had lapsed but nothing was filed (%d -> %d)", before, got)
	}
}

// A rejection of a DIFFERENT title must not suppress this one — the cooldown is
// per-title, not global, or one rejection would mute every detector.
func TestFileRendered_RejectionOfAnotherTitleDoesNotSuppress(t *testing.T) {
	repo := newTuneTestRepo(t)
	w := newReclaimWorker(t, repo, nil)
	w.RejectCooldown = 168 * time.Hour

	seedRejected(t, repo, "p1", tuneTimeoutReclaimTitle("p1", "other-step"))
	before := draftCount(t, repo)

	w.fileRendered(context.Background(), "p1", tuneTimeoutReclaimTitle("p1", "implement"),
		"rationale", "{}", "tune-detector",
		&RenderedChange{ApplyTarget: "configs/workflows/dev-pipeline.md", ApplyContent: "x", Summary: "s"})

	if got := draftCount(t, repo); got != before+1 {
		t.Fatalf("a rejection of a different title suppressed this one (%d -> %d)", before, got)
	}
}

// Zero means the shipped default, not "no cooldown" — a zero-value worker must
// not silently revert to the pre-2026-08-10 re-file-forever behaviour.
func TestRejectCooldown_ZeroUsesDefault(t *testing.T) {
	w := &TuneWorker{}
	if got := w.rejectCooldown(); got != 168*time.Hour {
		t.Fatalf("rejectCooldown() = %v, want 168h default", got)
	}
}

// One open applyable proposal per target FILE (design §6, implementation
// finding). Two detectors can legitimately want to edit the same workflow file
// in the same tick — scanLatency's raise path and scanTimeoutBinding both size
// a step timeout. Filing both is not merely noisy: single-op applies carry a
// base_hash, so once the first applies the second is permanently stale and can
// never apply. The second must not be filed.
func TestFileRendered_SkipsSecondApplyableForSameTarget(t *testing.T) {
	repo := newTuneTestRepo(t)
	w := newReclaimWorker(t, repo, nil)
	ctx := context.Background()
	rc := func() *RenderedChange {
		return &RenderedChange{ApplyTarget: "configs/workflows/dev-pipeline.md", ApplyContent: "x", Summary: "s"}
	}

	w.fileRendered(ctx, "p1", "Tune: first proposal", "r", "{}", "tune-detector", rc())
	w.fileRendered(ctx, "p1", "Tune: second proposal", "r", "{}", "tune-detector", rc())

	if n := draftCount(t, repo); n != 1 {
		t.Fatalf("filed %d applyable proposals against one file; want 1", n)
	}
}

// An INFORMATIONAL proposal (no ApplyTarget) must not be blocked by, or block,
// an applyable one — the supersede-informational-with-applyable upgrade path
// depends on both being able to exist.
func TestFileRendered_InformationalNotBlockedByApplyable(t *testing.T) {
	repo := newTuneTestRepo(t)
	w := newReclaimWorker(t, repo, nil)
	ctx := context.Background()

	w.fileRendered(ctx, "p1", "Tune: applyable", "r", "{}", "tune-detector",
		&RenderedChange{ApplyTarget: "configs/workflows/dev-pipeline.md", ApplyContent: "x", Summary: "s"})
	w.fileRendered(ctx, "p1", "Tune: informational", "r", "{}", "tune-detector", nil)

	if n := draftCount(t, repo); n != 2 {
		t.Fatalf("got %d proposals; want 2 (informational must not be blocked)", n)
	}
}
