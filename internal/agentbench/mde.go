package agentbench

import (
	"fmt"
	"math"
)

// Minimum detectable effect (§5.5).
//
// WHY THE HARNESS COMPUTES n RATHER THAN ACCEPTING IT. An implementer who picks
// a comfortable n and derives an MDE afterwards has produced a post-hoc
// justification, not a measurement. So the run path computes the floor from the
// MEASURED σ_d and refuses below it — and where the task set cannot grow, it
// reports the δ that the available pairs can actually resolve rather than
// pretending the run answered the question it was commissioned to answer.
//
// PAIRED, because arms run the same task set. The statistic is the per-task
// DIFFERENCE between arms, which cancels between-task variance — some tasks need
// three tools and some need twelve, and that spread has nothing to do with the
// policy under test. Pairing buys far more power here than enlarging the set
// would.

// zAlphaBetaSquared is (z₀.₉₇₅ + z₀.₈)² for a two-sided test at α=0.05 with
// power 0.8 — the conventional pair, stated as a constant so a future reader can
// see which convention was chosen rather than reverse-engineering 7.849 from the
// arithmetic.
const zAlphaBetaSquared = 7.8489

// RequiredPairs is the smallest number of paired tasks that can resolve an
// effect of size delta given a measured per-task-difference sigma.
//
// Returns an error rather than a number when the inputs cannot support one:
// a non-positive delta asks for infinite power, and a non-positive sigma means
// the noise floor has not been measured yet — and §5.4 forbids a gate before it
// has been.
func RequiredPairs(sigmaD, delta float64) (int, error) {
	if math.IsNaN(sigmaD) || math.IsInf(sigmaD, 0) || sigmaD <= 0 {
		return 0, fmt.Errorf("sigma_d must be positive: an unmeasured noise floor cannot " +
			"size a run, and §5.4 forbids gating before the noise-floor pass reports one")
	}
	if math.IsNaN(delta) || math.IsInf(delta, 0) || delta <= 0 {
		return 0, fmt.Errorf("delta must be positive: resolving an effect of zero needs " +
			"infinitely many pairs")
	}
	ratio := sigmaD / delta
	required := math.Ceil(zAlphaBetaSquared * ratio * ratio)
	maxInt := float64(int(^uint(0) >> 1))
	if math.IsNaN(required) || math.IsInf(required, 0) || required > maxInt {
		return 0, fmt.Errorf("required pair count overflows int for sigma_d=%g delta=%g", sigmaD, delta)
	}
	return int(required), nil
}

// ResolvableDelta inverts RequiredPairs: the smallest effect that n pairs can
// resolve at the same α and power.
//
// This is what a fixed-size task set reports instead of a verdict. Publishing
// "no change" from a run that could never have seen the change is the specific
// dishonesty §5.5 exists to prevent.
func ResolvableDelta(sigmaD float64, pairs int) (float64, error) {
	if math.IsNaN(sigmaD) || math.IsInf(sigmaD, 0) || sigmaD <= 0 {
		return 0, fmt.Errorf("sigma_d must be positive")
	}
	if pairs <= 0 {
		return 0, fmt.Errorf("pairs must be positive")
	}
	result := sigmaD * math.Sqrt(zAlphaBetaSquared/float64(pairs))
	if math.IsNaN(result) || math.IsInf(result, 0) {
		return 0, fmt.Errorf("resolvable delta is non-finite")
	}
	return result, nil
}

// PowerCheck is a run's sizing verdict.
type PowerCheck struct {
	SigmaD         float64 `json:"sigmaD"`
	SigmaN         int     `json:"sigmaN"`
	TargetDelta    float64 `json:"targetDelta"`
	RequiredPairs  int     `json:"requiredPairs"`
	AvailablePairs int     `json:"availablePairs"`
	// ResolvableDelta is what the available pairs CAN resolve. Reported whether
	// or not the run is adequately powered, because it is the number that makes
	// an inconclusive result readable.
	ResolvableDelta float64 `json:"resolvableDelta"`
	Adequate        bool    `json:"adequate"`
}

// CheckPower sizes a run and reports whether it can answer its question.
//
// sigmaN is carried because an σ from n=4 is not an σ: the 2026-08-11
// measurement moved context precision from 0.0082 at n=4 to 0.0195 at n=10, a
// factor of 2.4, turning an apparent 3.4σ effect into 1.6σ. Underestimating
// spread manufactures significance because it is the denominator, so the n the
// σ came from travels with it into every report.
func CheckPower(sigmaD float64, sigmaN int, targetDelta float64, availablePairs int) (PowerCheck, error) {
	if sigmaN <= 0 {
		return PowerCheck{}, fmt.Errorf("sigma_n must be positive")
	}
	required, err := RequiredPairs(sigmaD, targetDelta)
	if err != nil {
		return PowerCheck{}, err
	}
	resolvable, err := ResolvableDelta(sigmaD, availablePairs)
	if err != nil {
		return PowerCheck{}, err
	}
	return PowerCheck{
		SigmaD:          sigmaD,
		SigmaN:          sigmaN,
		TargetDelta:     targetDelta,
		RequiredPairs:   required,
		AvailablePairs:  availablePairs,
		ResolvableDelta: resolvable,
		Adequate:        availablePairs >= required,
	}, nil
}

// MinimumSigmaRuns is the smallest n a σ may be measured from before it may size
// anything. §5.4: treat n<10 as a direction, not a verdict.
const MinimumSigmaRuns = 10

// Refuse returns an error when the run cannot resolve the effect it was
// commissioned to find, naming the δ it CAN resolve.
//
// The message carries the resolvable δ rather than only the shortfall in pairs,
// because "you need 47 pairs and have 20" leaves an operator to work out what
// their 20 are good for, and the usual answer to that is to run it anyway.
func (p PowerCheck) Refuse() error {
	if p.SigmaN <= 0 {
		return fmt.Errorf("refusing to gate: sigma_n must be positive")
	}
	required, err := RequiredPairs(p.SigmaD, p.TargetDelta)
	if err != nil {
		return fmt.Errorf("refusing to gate: invalid power inputs: %w", err)
	}
	resolvable, err := ResolvableDelta(p.SigmaD, p.AvailablePairs)
	if err != nil {
		return fmt.Errorf("refusing to gate: invalid available pairs: %w", err)
	}
	derivedAdequate := p.AvailablePairs >= required
	tolerance := math.Max(1e-3, resolvable*0.01)
	resolvableMismatch := p.ResolvableDelta > 0 && math.Abs(p.ResolvableDelta-resolvable) > tolerance
	if p.RequiredPairs != required || resolvableMismatch || p.Adequate != derivedAdequate {
		return fmt.Errorf("refusing to gate: serialized power verdict is inconsistent with its inputs")
	}
	if p.SigmaN < MinimumSigmaRuns {
		return fmt.Errorf("refusing to gate: sigma_d=%.4f was measured from only n=%d runs. "+
			"An sigma from a handful of runs is a direction, not a verdict — underestimating "+
			"spread manufactures significance because it is the denominator. Collect at least "+
			"%d", p.SigmaD, p.SigmaN, MinimumSigmaRuns)
	}
	if derivedAdequate {
		return nil
	}
	return fmt.Errorf("refusing to gate: resolving delta=%.4f at sigma_d=%.4f needs %d paired "+
		"tasks and this run has %d. Those %d can resolve delta=%.4f — report a smaller effect "+
		"as INCONCLUSIVE with that floor, never as 'no change'",
		p.TargetDelta, p.SigmaD, p.RequiredPairs, p.AvailablePairs, p.AvailablePairs,
		p.ResolvableDelta)
}
