package controlplane

import (
	"context"
	"errors"
	"fmt"

	"vornik.io/vornik/internal/persistence"
)

// Staged-kind recovery — re-audit 2026-09-15, CA-06.
//
// A KindApplier "bypasses the file-based apply path entirely" by design, and
// that bypass took the journal with it. For workspace_context the ordering is
// now correct — the pre-image is committed before the write — but a pre-image
// nothing READS is not recovery. When the ledger write failed after the
// rename, the deployment was left with:
//
//	the file changed · the proposal still APPROVED · a staged snapshot · NO journal row
//
// and every route out was closed: `Reconcile` scans `config_apply_journal`
// only, retry fails the read-set check because the target is no longer
// absent, and `Rollback` refuses a proposal that is not APPLIED. The operator
// could neither finish the change nor undo it.
//
// This closes that hole for the kinds that stage a snapshot, using the
// snapshot as the recovery record. It is NOT the full fix: the deeper one is
// to journal these writes like every other, which needs the journal to
// understand a second deployment root (the workspace tree). That is recorded
// in the design and the backlog rather than implied by this narrower pass.

// StagedRecoverable is the capability a KindApplier must expose to take part
// in staged recovery: given a proposal and its staged snapshot, say what the
// world currently looks like.
type StagedRecoverable interface {
	// StagedState reports whether the mutation this proposal describes has
	// ALREADY landed on disk ("applied"), is still at its pre-image
	// ("pending"), or matches neither ("drift").
	StagedState(ctx context.Context, p *persistence.ControlPlaneProposal) (string, error)
}

// Staged states.
const (
	StagedStateApplied = "applied"
	StagedStatePending = "pending"
	StagedStateDrift   = "drift"
)

// RecoverStagedKindApplies reconciles APPROVED proposals whose kind-applier
// staged a pre-image but whose ledger transition never landed.
//
// Only "the write provably landed" is auto-completed. A target still at its
// pre-image needs nothing (the proposal is simply still APPROVED and
// retryable), and a target matching neither image is EXTERNAL DRIFT, which is
// an operator's call — the same rule §1.4 applies to the journal, for the
// same reason: recovery must never overwrite a hand edit.
func (e *ApplyEngine) RecoverStagedKindApplies(ctx context.Context) error {
	if e == nil || e.Proposals == nil || len(e.KindAppliers) == 0 {
		return nil
	}
	if !configWriteMu.TryLock() {
		return ErrApplyInProgress
	}
	defer configWriteMu.Unlock()

	rows, err := e.Proposals.List(ctx, persistence.ProposalListFilter{
		Statuses: []string{persistence.ProposalStatusApproved},
	})
	if err != nil {
		return fmt.Errorf("staged recovery: list proposals: %w", err)
	}
	var errs []error
	for _, p := range rows {
		if p.PreApplySnapshot == "" {
			continue // nothing was staged: nothing to recover
		}
		ka := e.kindApplier(p.Kind)
		if ka == nil {
			continue
		}
		sr, ok := ka.(StagedRecoverable)
		if !ok {
			continue
		}
		state, serr := sr.StagedState(ctx, p)
		if serr != nil {
			errs = append(errs, fmt.Errorf("staged recovery %s: %w", p.ID, serr))
			continue
		}
		switch state {
		case StagedStateApplied:
			// The write landed and the ledger did not. Complete it: the
			// snapshot it stages is the one already on the row.
			if merr := e.Proposals.MarkApplied(ctx, p.ID, JournalReconcileActor, p.PreApplySnapshot); merr != nil {
				errs = append(errs, fmt.Errorf("staged recovery %s: complete ledger: %w", p.ID, merr))
				continue
			}
			e.Logger.Warn().Str("proposal_id", p.ID).Str("kind", p.Kind).
				Msg("control-plane: recovered a staged apply whose write landed but whose ledger transition did not")
		case StagedStateDrift:
			errs = append(errs, fmt.Errorf("staged recovery %s: %w (target matches neither its pre-image nor the intended content; an operator must resolve it)", p.ID, ErrJournalDrift))
		default:
			// Pending: nothing was written. The proposal stays APPROVED and
			// an ordinary retry applies it.
		}
	}
	return errors.Join(errs...)
}
