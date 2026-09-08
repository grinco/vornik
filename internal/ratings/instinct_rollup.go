package ratings

import (
	"context"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// The reported half of instinct lift.
//
// Design: https://docs.vornik.io §3.
//
// instinct_lift asks whether surfacing an instinct helped, by the system's own
// success predicate. This asks whether the people who saw the output thought
// so. They sit side by side on a retire proposal, and the case worth naming is
// when they disagree.

// Domain names, matching persistence.InstinctDomain* — repeated here rather
// than imported so this package keeps depending only on persistence's types.
const (
	domainRecovery = "recovery"
)

// InstinctArmReader is the persistence surface this needs. Narrower than the
// repository interface so a caller can be tested without one.
type InstinctArmReader interface {
	InstinctRecoveryRatingArms(ctx context.Context, instinctID, projectID, role, errorClass string,
		since time.Time) (persistence.InstinctRatingArms, error)
}

// InstinctRollupResult is one instinct's reported verdict over one window.
//
// One result rather than a slice, unlike the skill rollup: a skill is measured
// across every (project, workflow) it appeared in, while a recovery instinct
// has exactly ONE context and it comes from the instinct record itself. There
// is no summary step here to get wrong.
type InstinctRollupResult struct {
	InstinctID string
	Window     time.Duration
	Result     Result
}

// InstinctRollup measures one instinct over one window.
//
// The window is PASSED IN, never resolved here: it must be the same span the
// measured pass used on the same tick, or the two numbers on the proposal
// describe different periods and their agreement means nothing.
//
// domain decides whether there is anything to measure at all. Only recovery
// records an execution_id on its applications, so budget and architect return
// not_measurable by CONSTRUCTION — a permanent property of the schema, and a
// different fact from "nobody has rated this yet".
func InstinctRollup(ctx context.Context, repo InstinctArmReader,
	instinctID, domain, projectID, role, errorClass string,
	window time.Duration, cfg Config) (InstinctRollupResult, error) {
	out := InstinctRollupResult{
		InstinctID: instinctID,
		Window:     window,
		Result:     Result{Verdict: VerdictNotMeasurable},
	}

	if domain != domainRecovery {
		// No query. There is no attribution surface to join on and there never
		// will be for this domain, so asking the database would be asking a
		// question whose answer is fixed by the schema.
		out.Result.Cause = CauseConstruction
		return out, nil
	}

	arms, err := repo.InstinctRecoveryRatingArms(ctx, instinctID, projectID, role, errorClass,
		time.Now().UTC().Add(-window))
	if err != nil {
		return out, fmt.Errorf("read reported arms for instinct %s: %w", instinctID, err)
	}

	// Nothing in this context at all: not a measurement failure, and not the
	// domain's fault either — nobody has been in a position to rate it.
	if arms.Treatment.EligibleN == 0 && arms.Baseline.EligibleN == 0 {
		out.Result.Cause = CauseAbsence
		return out, nil
	}

	out.Result = Evaluate(toArm(arms.Treatment), toArm(arms.Baseline), cfg)
	return out, nil
}

// Disagrees reports whether the measured and reported verdicts are in genuine
// conflict — the finding a retire proposal exists to surface.
//
// Both sides must stand behind a number. `not_comparable` is excluded even
// though it HAS arms: it means the two sides were watched at materially
// different rates, so there is no disagreement, only two measurements that
// cannot be set against each other. Calling that a disagreement would tell an
// operator the humans dissented when the truth is they were looking at a
// different set (design §3.3, round-2 review suggestion 1).
func Disagrees(measuredVerdict, reportedVerdict string) bool {
	if !standsBehindANumber(measuredVerdict) || !standsBehindANumber(reportedVerdict) {
		return false
	}
	return measuredVerdict != reportedVerdict
}

// standsBehindANumber is ShowsDifference under the name that says why it is
// being asked here. The two verdict vocabularies share these values by design,
// so one predicate serves both halves.
func standsBehindANumber(verdict string) bool {
	return ShowsDifference(verdict)
}
