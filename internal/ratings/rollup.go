// Package ratings turns the human verdicts phase 1 collects into an answer to
// "did the change make it worse".
//
// Design: https://docs.vornik.io
// The method — treatment versus a matched concurrent complement over the same
// window — is borrowed unchanged from
// https://docs.vornik.io What is
// NOT borrowed is that design's central assumption, that every case has an
// outcome: instinct lift reads results the system records for every execution,
// while a rating exists only where a human chose to write one, and that choice
// is not random. Everything about coverage below follows from that one
// difference.
//
// Advisory only. Nothing here gates a run.
package ratings

// Verdicts. The first four are instinct lift's vocabulary, unchanged so an
// operator reading both surfaces is not learning two. VerdictNotComparable is
// new here, and exists because of the sparse-population problem above.
const (
	// VerdictNotMeasurable is set by the caller when the subject has no
	// attribution surface to join on. Evaluate is never called for it.
	VerdictNotMeasurable = "not_measurable"
	// VerdictUnknown — too few ratings, or too few as a share of what was
	// eligible, to say anything.
	VerdictUnknown = "unknown"
	// VerdictNotComparable — the arms were observed at materially different
	// rates, so a lift figure across them measures who was watching.
	VerdictNotComparable = "not_comparable"
	// VerdictLowLift — up-rate materially lower under treatment.
	VerdictLowLift = "low_lift"
	// VerdictHelping — nothing wrong detected. Note this is a WEAKER claim
	// than instinct lift's identically-named verdict: there it can reflect a
	// measured improvement, here the ceiling is silence, so it can only ever
	// mean no evidence of harm. Same label for vocabulary parity; the rendered
	// text differs.
	VerdictHelping = "helping"
)

// Arm is one side of the comparison.
type Arm struct {
	// EligibleN is how many executions were in this arm's context in the
	// window — rated or not. It is the denominator coverage needs, and the
	// number instinct lift never had to collect.
	EligibleN int
	// RatedN is how many of those carry a rating this rollup counted. An
	// execution rated by several operators who agree counts ONCE.
	RatedN int
	// UpN is how many of RatedN were rated up. The success predicate.
	UpN int
	// ContestedN is executions whose raters disagreed. Excluded from RatedN
	// and from UpN — a disagreement between humans about one output is
	// evidence about the raters, not about the subject — and carried here so
	// the exclusion is visible rather than silent (design §4.1).
	ContestedN int
}

// upRate is the success rate, 0 when nothing was rated.
func (a Arm) upRate() float64 {
	if a.RatedN == 0 {
		return 0
	}
	return float64(a.UpN) / float64(a.RatedN)
}

// coverage is rated over eligible, 0 when nothing was eligible.
//
// Zero rather than 1.0 for an empty arm: having rated none of nothing is not
// complete observation, and reporting it as such would make the emptiest arm
// look like the best-observed one.
func (a Arm) coverage() float64 {
	if a.EligibleN == 0 {
		return 0
	}
	return float64(a.RatedN) / float64(a.EligibleN)
}

// Config tunes the gates.
type Config struct {
	// MinRated is the per-arm floor below which no verdict is offered.
	MinRated int
	// Margin is how far below zero lift must fall before it is called out.
	Margin float64
	// CoverageSkewMax is the largest ratio between the arms' coverages that
	// still counts as comparable.
	CoverageSkewMax float64
	// CoverageMinAbsolute is the coverage each arm must reach before the skew
	// ratio means anything at all.
	CoverageMinAbsolute float64
}

// DefaultConfig matches instinct lift where the methodology is unaffected by
// the sparse-population problem, and only diverges where it is.
//
// MinRated and Margin are instinct lift's own 8 and 0.05, deliberately NOT
// relaxed: ratings being scarce argues for a higher bar, not a lower one,
// because a rating is a MORE biased observation than an automatic outcome, not
// a more informative one. Lowering the floor to get verdicts out of sparse data
// is how a noisy signal starts driving retire proposals.
func DefaultConfig() Config {
	return Config{
		MinRated:            8,
		Margin:              0.05,
		CoverageSkewMax:     2.0,
		CoverageMinAbsolute: 0.05,
	}
}

// Result is one rollup row.
type Result struct {
	Verdict string
	// Cause explains a verdict that carries no number. Meaningful only for
	// VerdictNotMeasurable, where four different facts share one verdict and
	// imply four different operator actions (see render.go). Evaluate never
	// sets it: Evaluate is only reached once there IS an attribution surface
	// to measure, so every cause is a property of the caller's subject rather
	// than of the arms.
	Cause Cause
	// Lift is upRate(treatment) − upRate(baseline). NEGATIVE means worse under
	// treatment — instinct lift's sign convention, kept so the two surfaces
	// compute the same thing the same way.
	Lift               float64
	TreatmentCoverage  float64
	BaselineCoverage   float64
	TreatmentN         int
	TreatmentUp        int
	BaselineN          int
	BaselineUp         int
	TreatmentContested int
	BaselineContested  int
}

// Evaluate compares one arm against its concurrent complement.
//
// The order of the gates is the design's and is load-bearing. Sample size
// first: "too few ratings" and "the arms are not comparable" are different
// facts and an operator acts differently on each, so a thin arm must not be
// reported as an incomparable one. Coverage skew second, before any lift is
// judged, because it decides whether the lift figure means anything.
func Evaluate(treatment, baseline Arm, cfg Config) Result {
	res := Result{
		Lift:               treatment.upRate() - baseline.upRate(),
		TreatmentCoverage:  treatment.coverage(),
		BaselineCoverage:   baseline.coverage(),
		TreatmentN:         treatment.RatedN,
		TreatmentUp:        treatment.UpN,
		BaselineN:          baseline.RatedN,
		BaselineUp:         baseline.UpN,
		TreatmentContested: treatment.ContestedN,
		BaselineContested:  baseline.ContestedN,
	}

	if treatment.RatedN < cfg.MinRated || baseline.RatedN < cfg.MinRated {
		res.Verdict = VerdictUnknown
		return res
	}
	// The absolute floor comes before the ratio. Two arms at 2% and 0% give an
	// infinite ratio and a confident refusal about two populations that are
	// both essentially unobserved — where the interesting fact is that nobody
	// rated anything, which is what unknown means.
	if res.TreatmentCoverage < cfg.CoverageMinAbsolute ||
		res.BaselineCoverage < cfg.CoverageMinAbsolute {
		res.Verdict = VerdictUnknown
		return res
	}
	if skew(res.TreatmentCoverage, res.BaselineCoverage) > cfg.CoverageSkewMax {
		res.Verdict = VerdictNotComparable
		return res
	}

	// One-sided. A treatment arm that looks BETTER is not evidence of harm, and
	// with this signal it is not evidence of benefit either — it is helping in
	// the weak sense the constant documents.
	if res.Lift <= -cfg.Margin {
		res.Verdict = VerdictLowLift
		return res
	}
	res.Verdict = VerdictHelping
	return res
}

// skew is the ratio of the larger coverage to the smaller, orientation-free so
// it does not matter which arm was watched harder.
func skew(a, b float64) float64 {
	if a <= 0 || b <= 0 {
		// Reached only above the absolute floor, so this is defensive: a zero
		// here would be a division by zero, and treating it as maximally skewed
		// is the fail-toward-refusal direction.
		return 1e9
	}
	if a > b {
		return a / b
	}
	return b / a
}
