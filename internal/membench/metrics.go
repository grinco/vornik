// Package membench is the memory-benchmark harness described in
// https://docs.vornik.io
//
// It scores a memory system's retrieval quality on labelled datasets, and can
// drive an external system over the same interface for a head-to-head
// comparison. The design's central discipline is that the three metric tiers
// are never merged: judge-free retrieval metrics (tier 2) are what a CI gate
// can afford to run per-change, judged answer accuracy (tier 1) is what a
// scorecard reports, and cost/latency (tier 3) is neither.
package membench

import "math"

// Tier-2 metrics (design §5.7). Deterministic, LLM-free, and therefore cheap
// enough to gate on. Naming follows the RAGAS vocabulary already established in
// rag-ingest-pipeline-design.md §2.6.
//
// All three return NaN when the quantity is undefined rather than substituting
// zero. A zero would be indistinguishable from a genuine score of zero, which
// is the difference between "this item had no labels" and "retrieval failed
// completely" — and averaging the latter in silently understates the system.

// ContextRecall is the fraction of gold documents that appear in the retrieved
// set. NaN when the item has no gold documents.
//
// Set semantics on both sides: retrieving the same gold document twice is one
// document found, not two, and a duplicated gold entry does not make the target
// harder.
func ContextRecall(retrieved, gold []string) float64 {
	goldSet := toSet(gold)
	if len(goldSet) == 0 {
		return math.NaN()
	}
	retrievedSet := toSet(retrieved)
	found := 0
	for id := range goldSet {
		if retrievedSet[id] {
			found++
		}
	}
	return float64(found) / float64(len(goldSet))
}

// ContextPrecision is the fraction of retrieved DOCUMENTS that are gold. NaN when
// nothing was retrieved.
//
// Document-level on both sides, because that is the granularity the gold labels
// use. Retrieval returns chunks, and several chunks routinely come from one
// document; collapsing them to their documents is what makes the ratio comparable
// to the label.
//
// An earlier version divided by the retrieved CHUNK count, on the reasoning that a
// duplicated gold hit wastes a budget slot. The first live baseline exposed the
// flaw: with one gold document and an 8-chunk budget, precision was pinned at
// 1/8 = 0.125 across every category regardless of retrieval quality, because the
// ceiling is gold_docs/budget. It could not tell "the gold document plus seven
// junk chunks" from "all eight chunks drawn from the gold document", so a CI gate
// on it would have sat at 0.125 forever and detected nothing.
//
// Wasted budget is a real concern, but it is budget efficiency — a tier-3
// question — and folding it into precision cost precision its only job.
//
// NaN rather than 1.0 for an empty result keeps the degenerate strategy closed:
// retrieve nothing, claim perfection.
func ContextPrecision(retrieved, gold []string) float64 {
	if len(retrieved) == 0 {
		return math.NaN()
	}
	goldSet := toSet(gold)
	retrievedSet := toSet(retrieved)
	relevant := 0
	for id := range retrievedSet {
		if goldSet[id] {
			relevant++
		}
	}
	return float64(relevant) / float64(len(retrievedSet))
}

// MRR is the reciprocal rank of the FIRST gold document in the retrieved order,
// or 0 when none appears. NaN when the item has no gold documents.
//
// This is the metric that notices a change which retrieves the right thing but
// buries it — recall and precision are both blind to ordering.
func MRR(retrieved, gold []string) float64 {
	goldSet := toSet(gold)
	if len(goldSet) == 0 {
		return math.NaN()
	}
	for i, id := range retrieved {
		if goldSet[id] {
			return 1.0 / float64(i+1)
		}
	}
	return 0
}

// MeanIgnoringNaN averages the defined values, returning the mean and how many
// counted. NaN with a count of zero when nothing was measurable.
//
// Skipping rather than propagating matters: one unlabelled item would otherwise
// turn a whole category's score into NaN, which reads as total failure instead
// of as a gap in the labels.
func MeanIgnoringNaN(values []float64) (mean float64, counted int) {
	sum := 0.0
	for _, v := range values {
		if math.IsNaN(v) {
			continue
		}
		sum += v
		counted++
	}
	if counted == 0 {
		return math.NaN(), 0
	}
	return sum / float64(counted), counted
}

func toSet(ids []string) map[string]bool {
	if len(ids) == 0 {
		return nil
	}
	s := make(map[string]bool, len(ids))
	for _, id := range ids {
		s[id] = true
	}
	return s
}
