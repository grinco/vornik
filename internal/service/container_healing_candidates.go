package service

// Service-layer adapters for the Self-Healing Workflow Genome v1 admin
// API (Unit 5). The api package defines narrow seams (api.HealingTrialRunner,
// api.HealingCandidatePromoter) so it stays free of a workflowhealing
// import; these adapters bridge the concrete *workflowhealing.TrialRunner /
// *workflowhealing.Promoter to those seams and translate the
// workflowhealing sentinel errors into the api sentinels the handlers map
// to HTTP statuses.

import (
	"context"
	"encoding/json"
	"errors"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/ui"
	"vornik.io/vornik/internal/workflowhealing"
)

// healingTrialRunnerAdapter bridges *workflowhealing.TrialRunner to
// api.HealingTrialRunner. It flattens the runner's TrialResult to the
// api projection and re-wraps the workflowhealing sentinels as api ones.
type healingTrialRunnerAdapter struct {
	runner *workflowhealing.TrialRunner
}

func newHealingTrialRunnerAdapter(r *workflowhealing.TrialRunner) api.HealingTrialRunner {
	if r == nil {
		return nil
	}
	return &healingTrialRunnerAdapter{runner: r}
}

func (a *healingTrialRunnerAdapter) RunTrial(ctx context.Context, candidateID, mode string, evidenceIDs []string) (*api.HealingTrialOutcome, error) {
	res, err := a.runner.RunTrial(ctx, candidateID, persistence.HealingTrialMode(mode), evidenceIDs)
	if err != nil {
		return nil, mapHealingError(err)
	}
	out := &api.HealingTrialOutcome{
		Mode:    string(res.Mode),
		Verdict: string(res.Verdict),
	}
	if b, mErr := json.Marshal(res.Scorecard); mErr == nil {
		out.ScorecardJSON = string(b)
	}
	return out, nil
}

func (a *healingTrialRunnerAdapter) RunTrialAsync(ctx context.Context, candidateID, mode string, evidenceIDs []string) (string, error) {
	trial, err := a.runner.RunTrialAsync(ctx, candidateID, persistence.HealingTrialMode(mode), evidenceIDs)
	if err != nil {
		return "", mapHealingError(err)
	}
	return trial.ID, nil
}

// healingPromoterAdapter bridges *workflowhealing.Promoter to
// api.HealingCandidatePromoter, re-wrapping the workflowhealing sentinels.
type healingPromoterAdapter struct {
	promoter *workflowhealing.Promoter
}

func newHealingPromoterAdapter(p *workflowhealing.Promoter) api.HealingCandidatePromoter {
	if p == nil {
		return nil
	}
	return &healingPromoterAdapter{promoter: p}
}

func (a *healingPromoterAdapter) Promote(ctx context.Context, candidateID, promotedBy string) (*persistence.HealingCandidate, error) {
	cand, err := a.promoter.Promote(ctx, candidateID, promotedBy)
	if err != nil {
		return nil, mapHealingError(err)
	}
	return cand, nil
}

func (a *healingPromoterAdapter) Reject(ctx context.Context, candidateID string) (*persistence.HealingCandidate, error) {
	cand, err := a.promoter.Reject(ctx, candidateID)
	if err != nil {
		return nil, mapHealingError(err)
	}
	return cand, nil
}

// healingTrialRunnerUIAdapter bridges *workflowhealing.TrialRunner to the
// ui.HealingTrialRunnerUI seam (Unit 6). The UI handler only needs the
// error (it re-reads the candidate + trials for the redirect target), so
// this drops the TrialResult and re-wraps the workflowhealing sentinels
// as ui sentinels.
type healingTrialRunnerUIAdapter struct {
	runner *workflowhealing.TrialRunner
}

func newHealingTrialRunnerUIAdapter(r *workflowhealing.TrialRunner) ui.HealingTrialRunnerUI {
	if r == nil {
		return nil
	}
	return &healingTrialRunnerUIAdapter{runner: r}
}

func (a *healingTrialRunnerUIAdapter) RunTrial(ctx context.Context, candidateID, mode string, evidenceIDs []string) error {
	if _, err := a.runner.RunTrial(ctx, candidateID, persistence.HealingTrialMode(mode), evidenceIDs); err != nil {
		return mapHealingErrorUI(err)
	}
	return nil
}

func (a *healingTrialRunnerUIAdapter) RunTrialAsync(ctx context.Context, candidateID, mode string, evidenceIDs []string) error {
	if _, err := a.runner.RunTrialAsync(ctx, candidateID, persistence.HealingTrialMode(mode), evidenceIDs); err != nil {
		return mapHealingErrorUI(err)
	}
	return nil
}

// healingPromoterUIAdapter bridges *workflowhealing.Promoter to the
// ui.HealingCandidatePromoterUI seam, re-wrapping the workflowhealing
// sentinels as ui sentinels.
type healingPromoterUIAdapter struct {
	promoter *workflowhealing.Promoter
}

func newHealingPromoterUIAdapter(p *workflowhealing.Promoter) ui.HealingCandidatePromoterUI {
	if p == nil {
		return nil
	}
	return &healingPromoterUIAdapter{promoter: p}
}

func (a *healingPromoterUIAdapter) Promote(ctx context.Context, candidateID, promotedBy string) (*persistence.HealingCandidate, error) {
	cand, err := a.promoter.Promote(ctx, candidateID, promotedBy)
	if err != nil {
		return nil, mapHealingErrorUI(err)
	}
	return cand, nil
}

func (a *healingPromoterUIAdapter) Reject(ctx context.Context, candidateID string) (*persistence.HealingCandidate, error) {
	cand, err := a.promoter.Reject(ctx, candidateID)
	if err != nil {
		return nil, mapHealingErrorUI(err)
	}
	return cand, nil
}

// mapHealingErrorUI mirrors mapHealingError for the ui sentinel set.
func mapHealingErrorUI(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, workflowhealing.ErrCandidateNotFound):
		return wrapHealingSentinel(err, ui.ErrUICandidateNotFound)
	case errors.Is(err, workflowhealing.ErrCandidateTerminal):
		return wrapHealingSentinel(err, ui.ErrUICandidateTerminal)
	case errors.Is(err, workflowhealing.ErrCandidateNotPromotable):
		return wrapHealingSentinel(err, ui.ErrUICandidateNotPromotable)
	case errors.Is(err, workflowhealing.ErrUnsupportedMode):
		return wrapHealingSentinel(err, ui.ErrUITrialMode)
	case errors.Is(err, workflowhealing.ErrTrialAlreadyRunning):
		return wrapHealingSentinel(err, ui.ErrUITrialRunning)
	default:
		return err
	}
}

// mapHealingError translates the workflowhealing sentinel errors into the
// api sentinels so the handlers can map them to HTTP statuses without
// importing workflowhealing. Unknown errors pass through verbatim (the
// handler maps them to 500).
func mapHealingError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, workflowhealing.ErrCandidateNotFound):
		return wrapHealingSentinel(err, api.ErrHealingCandidateNotFound)
	case errors.Is(err, workflowhealing.ErrCandidateTerminal):
		return wrapHealingSentinel(err, api.ErrHealingCandidateTerminal)
	case errors.Is(err, workflowhealing.ErrCandidateNotPromotable):
		return wrapHealingSentinel(err, api.ErrHealingCandidateNotPromotable)
	case errors.Is(err, workflowhealing.ErrUnsupportedMode):
		return wrapHealingSentinel(err, api.ErrHealingTrialMode)
	case errors.Is(err, workflowhealing.ErrTrialAlreadyRunning):
		return wrapHealingSentinel(err, api.ErrHealingTrialRunning)
	default:
		return err
	}
}

// wrapHealingSentinel returns an error that errors.Is-matches the api
// sentinel while preserving the original message for the operator.
func wrapHealingSentinel(orig, sentinel error) error {
	return &healingWrappedError{msg: orig.Error(), sentinel: sentinel}
}

type healingWrappedError struct {
	msg      string
	sentinel error
}

func (e *healingWrappedError) Error() string { return e.msg }
func (e *healingWrappedError) Is(target error) bool {
	return target == e.sentinel
}
func (e *healingWrappedError) Unwrap() error { return e.sentinel }
