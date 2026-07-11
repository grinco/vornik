package projectwizard

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ConfirmSchedule records the operator's explicit confirmation of the
// tier-3 bundle's autonomy cadence (design §5.4's schedule-
// confirmation gate) on the session: session.ScheduleConfirmedAt /
// ScheduleConfirmedCron (persistence columns added ahead of this task
// — see internal/persistence/migrations.go). applyBundle
// (composer_engine.go) reads these back on the NEXT turn via
// scheduleConfirmed/schedulesEquivalent to decide whether
// ready_to_commit may stay true for an autonomy-enabled bundle.
//
// cron is the exact registry.ProjectAutonomy.PollInterval value the
// operator was shown (the UI reads it off the latest envelope's
// bundle.project.autonomy.pollInterval) — recording the precise
// value, not just "confirmed=true", is what lets a LATER turn that
// changes the cadence force a re-confirmation instead of silently
// reusing a stale approval (schedulesEquivalent is the strict
// comparison; this is the write side).
//
// Mirrors Cancel's ownership/state checks: unknown/foreign session id
// -> persistence.ErrNotFound (never leaks another operator's session
// by existing); already committed -> ErrSessionCommitted; already
// cancelled -> ErrSessionCancelled.
func (w *Wizard) ConfirmSchedule(ctx context.Context, sessionID, operatorID, cron string) error {
	if w == nil || w.Sessions == nil {
		return errors.New("projectwizard: not fully wired")
	}
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("projectwizard: session id required")
	}
	if strings.TrimSpace(operatorID) == "" {
		return errors.New("projectwizard: operator id required")
	}
	cron = strings.TrimSpace(cron)
	if cron == "" {
		return errors.New("projectwizard: cron is required")
	}

	session, err := w.Sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("projectwizard: load session: %w", err)
	}
	if session == nil {
		return persistence.ErrNotFound
	}
	if session.OperatorID != operatorID {
		// Cross-operator confirm attempt — treat as not-found so the
		// response shape doesn't leak another operator's session.
		return persistence.ErrNotFound
	}
	if session.CommittedProjectID != nil {
		return ErrSessionCommitted
	}
	if session.CancelledAt != nil {
		return ErrSessionCancelled
	}

	now := time.Now().UTC()
	session.ScheduleConfirmedAt = &now
	session.ScheduleConfirmedCron = cron
	if err := w.Sessions.Update(ctx, session); err != nil {
		return fmt.Errorf("projectwizard: update session: %w", err)
	}
	return nil
}
