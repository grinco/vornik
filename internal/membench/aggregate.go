package membench

import (
	"fmt"
	"math"
)

// Aggregation across repeated runs.
//
// This exists because the variance of a metric, not its value, is what decides
// whether the metric can be gated. §13.6 could gate tier 2 on exact equality only
// because ten RRF runs had a standard deviation of exactly zero; §13.9 found that
// putting an LLM in the retrieval path ends that, so the same question has to be
// re-asked for every path. Computing it by hand each time invited two mistakes
// that both happened: quoting a single stochastic run as a result, and mixing runs
// that the comparability key would have refused to compare.

// GateSigma is how many standard deviations a CI threshold must clear.
//
// Three, because a gate that trips on noise gets switched off and stays off — and
// a disabled gate is strictly worse than a loose one, since it reads as passing.
const GateSigma = 3.0

// Stat summarises one metric across runs.
type Stat struct {
	Mean   float64 `json:"mean"`
	SD     float64 `json:"sd"`
	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
	Spread float64 `json:"spread"`

	// Deterministic reports that every run produced the identical value. Only
	// such a metric can carry an exact-equality gate.
	Deterministic bool `json:"deterministic"`

	// GateTolerance is the narrowest defensible CI threshold: GateSigma * SD,
	// and zero for a deterministic metric, which needs none.
	GateTolerance float64 `json:"gate_tolerance"`
}

// Aggregation is the summary of a set of repeated runs.
type Aggregation struct {
	Runs int `json:"runs"`

	// Accuracy is POOLED from the outcome counts rather than averaged over
	// per-run rates. Averaging rates would weight a run that scored few items
	// equally with one that scored many.
	Accuracy         Stat `json:"accuracy"`
	ContextRecall    Stat `json:"context_recall"`
	ContextPrecision Stat `json:"context_precision"`
	MRR              Stat `json:"mrr"`

	// Fields is the shared comparability key's fields — shared because
	// Aggregate refuses anything else.
	Fields ComparabilityFields `json:"comparability_fields"`
}

// Aggregate summarises repeated runs of the SAME experiment.
//
// It refuses three things outright rather than producing a misleading number:
// fewer than two runs (a variance over one run is not a measurement, and zero
// would read as determinism), runs whose comparability keys differ (averaging
// incomparable runs is worse than plotting them, because the result looks like one
// number), and any run the harness already marked untrustworthy (a run too damaged
// to quote must not be laundered into a mean).
func Aggregate(runs []Result) (Aggregation, error) {
	if len(runs) < 2 {
		return Aggregation{}, fmt.Errorf("aggregate needs at least 2 runs, got %d: "+
			"a variance over one run is not a measurement", len(runs))
	}
	for i, r := range runs {
		if !r.Trust.Trustworthy {
			return Aggregation{}, fmt.Errorf("run %d is untrustworthy (%s): "+
				"a run the harness refused to quote cannot be averaged into one",
				i+1, r.Trust.Reason)
		}
		if i > 0 {
			if err := CheckComparable(runs[0].Fields, r.Fields); err != nil {
				return Aggregation{}, fmt.Errorf("run %d is not comparable to run 1: %w", i+1, err)
			}
		}
	}

	accs := make([]float64, 0, len(runs))
	recalls := make([]float64, 0, len(runs))
	precisions := make([]float64, 0, len(runs))
	mrrs := make([]float64, 0, len(runs))

	for _, r := range runs {
		if a, ok := pooledAccuracy(r); ok {
			accs = append(accs, a)
		}
		rec, pre, mrr := weightedMetrics(r)
		if !math.IsNaN(rec) {
			recalls = append(recalls, rec)
		}
		if !math.IsNaN(pre) {
			precisions = append(precisions, pre)
		}
		if !math.IsNaN(mrr) {
			mrrs = append(mrrs, mrr)
		}
	}

	return Aggregation{
		Runs:             len(runs),
		Accuracy:         summariseSpread(accs),
		ContextRecall:    summariseSpread(recalls),
		ContextPrecision: summariseSpread(precisions),
		MRR:              summariseSpread(mrrs),
		Fields:           runs[0].Fields,
	}, nil
}

// pooledAccuracy is correct answers over every DECIDED outcome in the run.
//
// Errors and invalids are excluded from the denominator deliberately: accuracy is
// a claim about answers the judge actually graded, and the degraded rate is what
// reports the rest (§5.9). Their absence from accuracy is why trust is checked
// separately and never traded against it.
func pooledAccuracy(r Result) (float64, bool) {
	var correct, decided int
	for _, c := range r.Counts {
		correct += c.Correct
		decided += c.Correct + c.Incorrect
	}
	if decided == 0 {
		return 0, false
	}
	return float64(correct) / float64(decided), true
}

// weightedMetrics collapses per-category tier-2 metrics into one run-level value,
// weighting each category by how many questions it scored.
//
// An unweighted mean over categories would let a 4-question category move the
// run-level number as much as a 12-question one.
func weightedMetrics(r Result) (recall, precision, mrr float64) {
	var wr, wp, wm float64
	var n int
	for _, m := range r.Metrics {
		if m.Scored <= 0 {
			continue
		}
		w := float64(m.Scored)
		wr += m.ContextRecall * w
		wp += m.ContextPrecision * w
		wm += m.MRR * w
		n += m.Scored
	}
	if n == 0 {
		return math.NaN(), math.NaN(), math.NaN()
	}
	d := float64(n)
	return wr / d, wp / d, wm / d
}

// summariseSpread computes the spread statistics for one metric.
func summariseSpread(vals []float64) Stat {
	if len(vals) == 0 {
		return Stat{Mean: math.NaN(), SD: math.NaN(), Min: math.NaN(), Max: math.NaN(), Spread: math.NaN()}
	}
	minV, maxV, sum := vals[0], vals[0], 0.0
	for _, v := range vals {
		if v < minV {
			minV = v
		}
		if v > maxV {
			maxV = v
		}
		sum += v
	}
	mean := sum / float64(len(vals))

	// Sample standard deviation (n-1): these runs are a sample of the run
	// distribution, not the population of every run that could be made.
	sd := 0.0
	if len(vals) > 1 {
		var ss float64
		for _, v := range vals {
			d := v - mean
			ss += d * d
		}
		sd = math.Sqrt(ss / float64(len(vals)-1))
	}

	det := maxV == minV
	tol := GateSigma * sd
	if det {
		tol = 0
	}
	return Stat{
		Mean: mean, SD: sd, Min: minV, Max: maxV, Spread: maxV - minV,
		Deterministic: det, GateTolerance: tol,
	}
}
