package membench

import (
	"fmt"
	"math"
)

// Outcome taxonomy and run trustworthiness (design §5.9).
//
// Four outcomes, kept distinct because collapsing them is how a benchmark lies.
// The two that matter most are the ones a naive harness would fold into
// "incorrect": a judge whose output could not be parsed, and an adapter that
// errored. Scoring either as a wrong answer blames retrieval for something that
// is not retrieval's fault, and does so invisibly.

// Outcome is the result of one question.
type Outcome string

const (
	// OutcomeCorrect — the judge affirmed the answer.
	OutcomeCorrect Outcome = "correct"
	// OutcomeIncorrect — the judge rejected it.
	OutcomeIncorrect Outcome = "incorrect"
	// OutcomeInvalid — the judge's own output could not be parsed after a
	// retry. NEVER scored as incorrect: we do not know what the verdict was.
	OutcomeInvalid Outcome = "invalid"
	// OutcomeError — an HTTP fault, timeout or 4xx from a system under test.
	// An infrastructure problem, not a wrong answer.
	OutcomeError Outcome = "error"
	// OutcomeUnjudged — retrieval was scored and NOTHING WAS JUDGED, because the
	// run was tier-2-only. Not a verdict and not a fault: the item ran, its
	// retrieval produced context recall / precision / MRR, and no answer was ever
	// generated to grade.
	//
	// It is a distinct outcome rather than a reuse of OutcomeInvalid because the
	// two mean opposite things to an operator. Invalid means the judge spoke and we
	// could not read it — a degraded run. Unjudged means we deliberately did not
	// ask, and the run is fully trustworthy for what it measured.
	OutcomeUnjudged Outcome = "unjudged"
)

// MaxDegradedThreshold is the hard ceiling on the tolerated (invalid + error)
// rate, per category. NOT raisable by configuration.
//
// Round-1 review found the loophole this closes: a purely configurable
// threshold lets an operator set it to 50% and quote a half-broken run as a
// result. A guard the subject of the measurement can widen arbitrarily is not a
// guard. Configuration may only tighten it.
const MaxDegradedThreshold = 0.20

// MaxHaystackLoss is the fraction of an item's haystack that may be rejected at
// ingest before the run is stamped untrustworthy.
//
// Past this, the score is being computed against a materially easier task than
// the dataset poses — fewer distractors to discriminate against — so reporting
// it as the dataset's score overstates the system.
const MaxHaystackLoss = 0.50

// OutcomeCounts tallies one category or one whole run.
type OutcomeCounts struct {
	Correct   int `json:"correct"`
	Incorrect int `json:"incorrect"`
	Invalid   int `json:"invalid"`
	Error     int `json:"error"`
	// Unjudged is items whose retrieval was scored with no verdict sought
	// (tier-2-only). Counted in Total because they ran, excluded from Judged so
	// they cannot move accuracy, and excluded from DegradedRate because nothing
	// went wrong.
	Unjudged int `json:"unjudged,omitempty"`
}

// Add increments the tally for one outcome.
func (c *OutcomeCounts) Add(o Outcome) {
	switch o {
	case OutcomeCorrect:
		c.Correct++
	case OutcomeIncorrect:
		c.Incorrect++
	case OutcomeInvalid:
		c.Invalid++
	case OutcomeError:
		c.Error++
	case OutcomeUnjudged:
		c.Unjudged++
	}
}

// Total is every question attempted.
func (c OutcomeCounts) Total() int {
	return c.Correct + c.Incorrect + c.Invalid + c.Error + c.Unjudged
}

// Judged is the population an accuracy figure can legitimately be computed over.
func (c OutcomeCounts) Judged() int { return c.Correct + c.Incorrect }

// Accuracy is correct over JUDGED, not over total. NaN when nothing was
// gradeable — which is not the same as having got everything wrong.
func (c OutcomeCounts) Accuracy() float64 {
	j := c.Judged()
	if j == 0 {
		return math.NaN()
	}
	return float64(c.Correct) / float64(j)
}

// DegradedRate is (invalid + error) over total: the share of the run that told
// us nothing about retrieval quality.
func (c OutcomeCounts) DegradedRate() float64 {
	t := c.Total()
	if t == 0 {
		return 0
	}
	return float64(c.Invalid+c.Error) / float64(t)
}

// ResolveDegradedThreshold clamps a requested threshold to the hard ceiling.
// Zero or negative means "use the ceiling" rather than "tolerate nothing" — a
// single flaky HTTP call should not invalidate an otherwise clean run by
// default.
func ResolveDegradedThreshold(requested float64) float64 {
	if requested <= 0 || requested > MaxDegradedThreshold {
		return MaxDegradedThreshold
	}
	return requested
}

// Trust is the verdict on whether a run's numbers may be quoted.
type Trust struct {
	Trustworthy bool   `json:"trustworthy"`
	Reason      string `json:"reason,omitempty"`
}

// AssessTrust decides whether a run is quotable.
//
// worstHaystackLoss is the largest fraction of any single item's haystack that
// was rejected at ingest; requestedThreshold may tighten the degraded-rate bar
// but never loosen it past MaxDegradedThreshold.
func AssessTrust(c OutcomeCounts, worstHaystackLoss, requestedThreshold float64) Trust {
	limit := ResolveDegradedThreshold(requestedThreshold)
	if rate := c.DegradedRate(); rate > limit {
		return Trust{
			Reason: fmt.Sprintf(
				"degraded rate %.1f%% exceeds %.1f%% (invalid=%d error=%d of %d): "+
					"the run is too damaged to quote as a score",
				rate*100, limit*100, c.Invalid, c.Error, c.Total()),
		}
	}
	if worstHaystackLoss > MaxHaystackLoss {
		return Trust{
			Reason: fmt.Sprintf(
				"an item lost %.1f%% of its haystack at ingest (limit %.1f%%): "+
					"the score measures an easier task than the dataset poses",
				worstHaystackLoss*100, MaxHaystackLoss*100),
		}
	}
	return Trust{Trustworthy: true}
}

// isNaN is a test-visible helper so the outcome tests don't each import math.
func isNaN(f float64) bool { return math.IsNaN(f) }
