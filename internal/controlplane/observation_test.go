package controlplane

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// Observations — detector output that carries no applyable change.
//
// Validated on the live ledger 2026-08-10: 19 proposals sat in APPROVED with
// apply_target, apply_ops AND apply_content all empty. There was no Apply
// button and never had been, so approving them accomplished nothing. Worse,
// approving one moved it out of DRAFT, which is the only status the title dedup
// consulted — so the detector re-filed the identical title days later. 10 of
// the 19 were re-files of a title already sitting in APPROVED: the act of
// clearing the inbox was what refilled it.

// An observation must never become a decidable proposal. This is the invariant
// that removes the inert-APPROVED class entirely.
func TestPropose_InformationalFilesObservationNotDecidableProposal(t *testing.T) {
	repo := newTuneTestRepo(t)
	w := newReclaimWorker(t, repo, nil)

	w.propose(context.Background(), "p1", "Tune: high p95 latency on p1", "rationale", `{"signal":"x"}`, "tune-detector")

	ps, err := repo.List(context.Background(), persistence.ProposalListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(ps) != 1 {
		t.Fatalf("want 1 row, got %d", len(ps))
	}
	if ps[0].Kind != persistence.ProposalKindObservation {
		t.Fatalf("kind = %q, want %q — an informational row must not be a decidable proposal",
			ps[0].Kind, persistence.ProposalKindObservation)
	}
}

// The accumulation fix: a repeat observation updates the existing row rather
// than inserting a second one, REGARDLESS of that row's status. Keying on DRAFT
// alone is the exact bug that produced six copies of the ibkr-trader title.
func TestPropose_RepeatObservationUpdatesInPlaceAcrossStatuses(t *testing.T) {
	// APPROVED is deliberately absent: SetStatus now refuses to approve an
	// observation (ErrProposalNotDecidable), so that state is unreachable by
	// construction. DRAFT and REJECTED are the whole reachable space — and
	// REJECTED is the one that matters, because "operator dismissed it, then it
	// happened again" is exactly the case the old DRAFT-only dedup mishandled.
	for _, status := range []string{
		persistence.ProposalStatusDraft,
		persistence.ProposalStatusRejected,
	} {
		t.Run(status, func(t *testing.T) {
			repo := newTuneTestRepo(t)
			w := newReclaimWorker(t, repo, nil)
			ctx := context.Background()
			title := "Tune: high p95 latency on p1"

			w.propose(ctx, "p1", title, "first sighting", `{"signal":"x"}`, "tune-detector")
			ps, _ := repo.List(ctx, persistence.ProposalListFilter{})
			if len(ps) != 1 {
				t.Fatalf("setup: want 1 row, got %d", len(ps))
			}
			if status != persistence.ProposalStatusDraft {
				if err := repo.SetStatus(ctx, ps[0].ID, status, "session:admin"); err != nil {
					t.Fatalf("set %s: %v", status, err)
				}
			}

			w.propose(ctx, "p1", title, "second sighting", `{"signal":"x"}`, "tune-detector")

			ps, err := repo.List(ctx, persistence.ProposalListFilter{})
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(ps) != 1 {
				t.Fatalf("observation re-filed as a new row while the prior one was %s: %d rows", status, len(ps))
			}
		})
	}
}

// The recurrence must be visible, or updating in place would hide that a
// problem is persistent — the operator needs to see "still happening", which is
// the signal a transient blip does not carry.
func TestPropose_RepeatObservationRecordsRecurrence(t *testing.T) {
	repo := newTuneTestRepo(t)
	w := newReclaimWorker(t, repo, nil)
	ctx := context.Background()
	title := "Tune: high p95 latency on p1"

	w.propose(ctx, "p1", title, "first", `{"signal":"x"}`, "tune-detector")
	w.propose(ctx, "p1", title, "second", `{"signal":"x"}`, "tune-detector")
	w.propose(ctx, "p1", title, "third", `{"signal":"x"}`, "tune-detector")

	ps, _ := repo.List(ctx, persistence.ProposalListFilter{})
	if len(ps) != 1 {
		t.Fatalf("want 1 row, got %d", len(ps))
	}
	if !strings.Contains(ps[0].Evidence, `"occurrences":3`) {
		t.Fatalf("evidence must carry the occurrence count, got: %s", ps[0].Evidence)
	}
	// The newest rationale wins: the operator should read the current numbers,
	// not the first sighting's.
	if ps[0].Rationale != "third" {
		t.Fatalf("rationale = %q, want the latest sighting", ps[0].Rationale)
	}
}

// An APPLYABLE proposal is unaffected — it stays a decidable config proposal.
func TestFileRendered_ApplyableStaysAConfigProposal(t *testing.T) {
	repo := newTuneTestRepo(t)
	w := newReclaimWorker(t, repo, nil)

	w.fileRendered(context.Background(), "p1", "Tune: raise binding timeout", "r", "{}", "tune-detector",
		&RenderedChange{ApplyTarget: "configs/workflows/dev-pipeline.md", ApplyContent: "x", Summary: "s"})

	ps, _ := repo.List(context.Background(), persistence.ProposalListFilter{})
	if len(ps) != 1 {
		t.Fatalf("want 1 row, got %d", len(ps))
	}
	if ps[0].Kind != persistence.ProposalKindConfig {
		t.Fatalf("kind = %q, want config — an applyable change must stay decidable", ps[0].Kind)
	}
}
