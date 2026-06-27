package memetic

// Slice 5 — Rollback path for applied proposals. Mirror of the
// Slice 4 applier: validate state, run `git revert` against the
// applied_commit in the source tree, reload config, stamp the
// proposal row as rolled_back.
//
// State machine: only applied → rolled_back is valid. The
// repository layer's MarkRolledBack enforces this at SQL; the
// rollbacker short-circuits earlier so the error message is
// clearer.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ErrProposalNotApplied fires when the operator tries to roll
// back a proposal that isn't in status=applied. Maps to 409 at
// the API. Distinct from ErrInvalidProposalTransition so the API
// can return a clearer message ("not applied" vs the generic
// state-machine guard).
var ErrProposalNotApplied = errors.New("memetic: proposal must be applied before rollback")

// GitReverter is the narrow operation the rollbacker needs from
// the source tree's git repo. Takes the SHA to revert and an
// operator-supplied message, returns the new revert commit's SHA.
//
// Implementations should use `git revert --no-edit <sha>` and
// expose --no-edit so the operator's revert lands without an
// interactive editor; the message arg is appended via -m.
type GitReverter interface {
	Revert(ctx context.Context, sha, message, authorName, authorEmail string) (revertSHA string, err error)
}

// RollbackerConfig tunes commit identity, mirrors ApplierConfig.
type RollbackerConfig struct {
	AuthorName  string
	AuthorEmail string
}

// Rollbacker owns the rollback-path workflow.
type Rollbacker struct {
	proposals persistence.WorkflowProposalRepository
	git       GitReverter
	reloader  ConfigReloadTrigger
	cfg       RollbackerConfig
}

// NewRollbacker wires the rollbacker. proposals is mandatory; git
// + reloader are nil-safe. When git is nil the rollback fails
// hard (without git revert there's nothing to undo); when reloader
// is nil the rollback succeeds but the operator is responsible
// for triggering the reload manually.
func NewRollbacker(
	proposals persistence.WorkflowProposalRepository,
	git GitReverter,
	reloader ConfigReloadTrigger,
	cfg RollbackerConfig,
) *Rollbacker {
	return &Rollbacker{
		proposals: proposals,
		git:       git,
		reloader:  reloader,
		cfg:       cfg,
	}
}

// Rollback runs one rollback turn for `proposalID`. Returns the
// updated proposal row on success. Errors:
//
//   - persistence.ErrNotFound          → 404
//   - ErrProposalNotApplied            → 409 (row isn't applied)
//   - persistence.ErrInvalidProposalTransition → 409 (race)
//
// The applied_commit must be a real git SHA (not the "no-git"
// sentinel from Slice 4's apply path when source tree was
// absent); if it's the sentinel the rollback fails early so the
// operator gets a clear "no git history available" message.
func (r *Rollbacker) Rollback(ctx context.Context, proposalID, revertedBy string) (*persistence.WorkflowProposal, error) {
	if proposalID == "" {
		return nil, fmt.Errorf("memetic.Rollback: proposalID is required")
	}
	if r.proposals == nil {
		return nil, fmt.Errorf("memetic.Rollback: proposals repo not wired")
	}

	got, err := r.proposals.Get(ctx, proposalID)
	if err != nil {
		return nil, err
	}
	if got.Status != persistence.WorkflowProposalStatusApplied {
		return nil, fmt.Errorf("%w: current status=%s",
			ErrProposalNotApplied, got.Status)
	}
	if got.AppliedCommit == "" || got.AppliedCommit == "no-git" {
		return nil, fmt.Errorf("memetic.Rollback: proposal has no git commit to revert (applied without git history)")
	}
	if r.git == nil {
		return nil, fmt.Errorf("memetic.Rollback: git reverter not wired on this deployment")
	}

	message := fmt.Sprintf("Revert workflow(%s) [proposal_id=%s, reverted_by=%s]",
		got.WorkflowID, got.ID, revertedBy)
	revertSHA, err := r.git.Revert(ctx, got.AppliedCommit, message,
		r.cfg.AuthorName, r.cfg.AuthorEmail)
	if err != nil {
		return nil, fmt.Errorf("memetic.Rollback: git revert: %w", err)
	}

	// Best-effort reload — same pattern as Apply.
	if r.reloader != nil {
		_ = r.reloader.Reload()
	}

	if err := r.proposals.MarkRolledBack(ctx, proposalID, revertSHA); err != nil {
		return nil, fmt.Errorf("memetic.Rollback: mark rolled_back: %w", err)
	}

	updated, err := r.proposals.Get(ctx, proposalID)
	if err != nil {
		now := time.Now().UTC()
		return &persistence.WorkflowProposal{
			ID:             proposalID,
			WorkflowID:     got.WorkflowID,
			Status:         persistence.WorkflowProposalStatusRolledBack,
			AppliedCommit:  got.AppliedCommit,
			RollbackCommit: revertSHA,
			AppliedAt:      got.AppliedAt,
			DecidedAt:      &now,
		}, nil
	}
	return updated, nil
}
