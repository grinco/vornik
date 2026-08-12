package membench

import (
	"math"
	"strings"
	"testing"
)

// Aggregation across repeated runs (design §13.6, §13.9). The variance of a
// metric decides whether it can be gated at all, so computing it is part of the
// harness rather than a script an operator rewrites each time.

// goldSetSize matches the native gold set, so the fixtures below score the same
// number of questions a real run does.
const goldSetSize = 30

func resultWith(key string, recall, precision, mrr float64, correct int) Result {
	return Result{
		Counts: map[string]OutcomeCounts{
			"c": {Correct: correct, Incorrect: goldSetSize - correct},
		},
		Metrics: map[string]Metrics{
			"c": {ContextRecall: recall, ContextPrecision: precision, MRR: mrr, Scored: goldSetSize},
		},
		Trust:  Trust{Trustworthy: true},
		Fields: ComparabilityFields{HarnessVersion: key, DatasetName: "native"},
	}
}

// TestAggregate_ReportsMeanAndSpread — the numbers §13.9 needed.
func TestAggregate_ReportsMeanAndSpread(t *testing.T) {
	runs := []Result{
		resultWith("3", 1.0, 0.30, 0.80, 20),
		resultWith("3", 0.9, 0.20, 0.60, 22),
	}
	agg, err := Aggregate(runs)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if agg.Runs != 2 {
		t.Errorf("Runs = %d, want 2", agg.Runs)
	}
	if got := agg.ContextRecall.Mean; math.Abs(got-0.95) > 1e-9 {
		t.Errorf("recall mean = %v, want 0.95", got)
	}
	if got := agg.ContextRecall.Spread; math.Abs(got-0.1) > 1e-9 {
		t.Errorf("recall spread = %v, want 0.1", got)
	}
	// Accuracy is pooled from the counts, not averaged from per-run rates.
	if got := agg.Accuracy.Mean; math.Abs(got-0.7) > 1e-9 {
		t.Errorf("accuracy mean = %v, want 0.7", got)
	}
}

// TestAggregate_RefusesIncomparableRuns — the whole point of the comparability
// key is that incomparable runs never land on one axis. Averaging them would be
// a worse violation than plotting them, because the result LOOKS like one number.
func TestAggregate_RefusesIncomparableRuns(t *testing.T) {
	runs := []Result{
		resultWith("2", 1.0, 0.30, 0.80, 20),
		resultWith("3", 1.0, 0.30, 0.80, 20),
	}
	if _, err := Aggregate(runs); err == nil {
		t.Fatal("Aggregate accepted runs with different comparability keys")
	} else if !strings.Contains(err.Error(), "harness_version") {
		t.Errorf("error should name the differing field, got %v", err)
	}
}

// TestAggregate_FlagsDeterminism — the distinction §13.9 turns on. A metric with
// zero spread can carry an exact-equality gate; one without needs a tolerance.
func TestAggregate_FlagsDeterminism(t *testing.T) {
	runs := []Result{
		resultWith("3", 0.95, 0.30, 0.80, 20),
		resultWith("3", 0.95, 0.31, 0.80, 20),
	}
	agg, err := Aggregate(runs)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if !agg.ContextRecall.Deterministic {
		t.Error("recall was identical across runs but not flagged deterministic")
	}
	if agg.ContextPrecision.Deterministic {
		t.Error("precision varied but was flagged deterministic")
	}
}

// TestAggregate_GateToleranceCoversNoise — a threshold below the observed noise
// would fail on noise alone, which is the failure mode that makes a gate get
// switched off and stay off.
func TestAggregate_GateToleranceCoversNoise(t *testing.T) {
	runs := []Result{
		resultWith("3", 0.95, 0.30, 0.80, 20),
		resultWith("3", 0.93, 0.30, 0.80, 20),
		resultWith("3", 0.97, 0.30, 0.80, 20),
	}
	agg, err := Aggregate(runs)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if agg.ContextRecall.GateTolerance < agg.ContextRecall.Spread {
		t.Errorf("gate tolerance %v is narrower than the observed spread %v",
			agg.ContextRecall.GateTolerance, agg.ContextRecall.Spread)
	}
	if agg.ContextPrecision.GateTolerance != 0 {
		t.Errorf("a deterministic metric needs no tolerance, got %v",
			agg.ContextPrecision.GateTolerance)
	}
}

// TestAggregate_RefusesUntrustworthyRun — a run the harness already refused to
// quote must not be laundered into a mean.
func TestAggregate_RefusesUntrustworthyRun(t *testing.T) {
	bad := resultWith("3", 1.0, 0.3, 0.8, 20)
	bad.Trust = Trust{Trustworthy: false, Reason: "degraded rate 100%"}
	runs := []Result{resultWith("3", 1.0, 0.3, 0.8, 20), bad}
	if _, err := Aggregate(runs); err == nil {
		t.Fatal("Aggregate accepted an untrustworthy run")
	}
}

// TestAggregate_RefusesFewerThanTwo — a "variance" over one run is not a
// measurement, and returning zero would read as determinism.
func TestAggregate_RefusesFewerThanTwo(t *testing.T) {
	if _, err := Aggregate([]Result{resultWith("3", 1, 0.3, 0.8, 20)}); err == nil {
		t.Fatal("Aggregate accepted a single run")
	}
}

// TestAggregate_WeightsCategoriesByQuestionCount — a run-level tier-2 value must
// weight each category by how many questions it scored.
//
// Without this, a 2-question category moves the run-level number as much as a
// 20-question one, and the native gold set's categories differ by 2x.
func TestAggregate_WeightsCategoriesByQuestionCount(t *testing.T) {
	// One category scores 2 questions at recall 0.0, another 8 at recall 1.0.
	// Weighted: 0.8. An unweighted mean over categories would say 0.5.
	lopsided := Result{
		Counts: map[string]OutcomeCounts{"small": {Correct: 1, Incorrect: 1}, "big": {Correct: 8}},
		Metrics: map[string]Metrics{
			"small": {ContextRecall: 0.0, ContextPrecision: 0.1, MRR: 0.0, Scored: 2},
			"big":   {ContextRecall: 1.0, ContextPrecision: 0.5, MRR: 1.0, Scored: 8},
		},
		Trust:  Trust{Trustworthy: true},
		Fields: ComparabilityFields{HarnessVersion: "3", DatasetName: "native"},
	}
	agg, err := Aggregate([]Result{lopsided, lopsided})
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if got := agg.ContextRecall.Mean; math.Abs(got-0.8) > 1e-9 {
		t.Errorf("weighted recall = %v, want 0.8 (unweighted would be 0.5)", got)
	}
	// Pooled accuracy counts DECIDED outcomes: 9 correct of 10 graded.
	if got := agg.Accuracy.Mean; math.Abs(got-0.9) > 1e-9 {
		t.Errorf("pooled accuracy = %v, want 0.9", got)
	}
}

// TestAggregate_SkipsUnscoredCategories — a category that scored nothing must not
// drag a run-level metric toward zero; it has no measurement to contribute.
func TestAggregate_SkipsUnscoredCategories(t *testing.T) {
	r := Result{
		Counts: map[string]OutcomeCounts{"real": {Correct: 3}},
		Metrics: map[string]Metrics{
			"real":  {ContextRecall: 1.0, ContextPrecision: 0.4, MRR: 1.0, Scored: 3},
			"empty": {ContextRecall: 0, ContextPrecision: 0, MRR: 0, Scored: 0},
		},
		Trust:  Trust{Trustworthy: true},
		Fields: ComparabilityFields{HarnessVersion: "3", DatasetName: "native"},
	}
	agg, err := Aggregate([]Result{r, r})
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if got := agg.ContextRecall.Mean; math.Abs(got-1.0) > 1e-9 {
		t.Errorf("recall = %v, want 1.0 — an unscored category was averaged in", got)
	}
}

// Corpus digest in the comparability key (2026-08-11).
//
// The native dataset's haystack is an external directory the key never covered:
// only the GOLD SET's sha256 was recorded. Editing the corpus therefore left two
// runs sharing a byte-identical key while retrieving from different documents —
// found the hard way, by editing the corpus (the design-doc tree IS the haystack)
// during a benchmark session and watching the chunk count move mid-run.

// TestCorpusDigest_DiffersWithContent — the property the key needs.
func TestCorpusDigest_DiffersWithContent(t *testing.T) {
	a := CorpusDigest([]Item{{DocumentID: "d1", Content: "alpha"}})
	b := CorpusDigest([]Item{{DocumentID: "d1", Content: "alpha revised"}})
	if a == b {
		t.Fatal("editing a document did not change the corpus digest")
	}
}

// TestCorpusDigest_StableAcrossOrder — the ingest order of a directory walk is not
// a property of the corpus, so it must not change the digest and split otherwise
// identical runs.
func TestCorpusDigest_StableAcrossOrder(t *testing.T) {
	one := []Item{{DocumentID: "a", Content: "x"}, {DocumentID: "b", Content: "y"}}
	two := []Item{{DocumentID: "b", Content: "y"}, {DocumentID: "a", Content: "x"}}
	if CorpusDigest(one) != CorpusDigest(two) {
		t.Error("digest depends on ingest order")
	}
}

// TestCorpusDigest_DiffersWhenADocumentIsAdded — a corpus that gained a distractor
// is a different retrieval problem, even though every previous document is intact.
func TestCorpusDigest_DiffersWhenADocumentIsAdded(t *testing.T) {
	base := []Item{{DocumentID: "a", Content: "x"}}
	plus := []Item{{DocumentID: "a", Content: "x"}, {DocumentID: "b", Content: "y"}}
	if CorpusDigest(base) == CorpusDigest(plus) {
		t.Error("adding a document did not change the digest")
	}
}

// TestCorpusDigest_DistinguishesIDFromContent — concatenating id and content
// without a separator would let a rename compensate for an edit.
func TestCorpusDigest_DistinguishesIDFromContent(t *testing.T) {
	a := CorpusDigest([]Item{{DocumentID: "ab", Content: "c"}})
	b := CorpusDigest([]Item{{DocumentID: "a", Content: "bc"}})
	if a == b {
		t.Error("id and content are not separated in the digest")
	}
}

// TestCorpusDigest_EmptyIsEmpty — a dataset carrying its own haystack has no
// external corpus, and an empty string is what marks that. A digest of nothing
// must not be a real-looking hash.
func TestCorpusDigest_EmptyIsEmpty(t *testing.T) {
	if got := CorpusDigest(nil); got != "" {
		t.Errorf("CorpusDigest(nil) = %q, want empty", got)
	}
}
