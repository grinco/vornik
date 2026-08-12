package membench

import (
	"strings"
	"testing"
)

// The run-twice determinism assertion, which is the cheapest gate in the design
// and the only one that can never go stale: it needs no committed baseline, so
// there is nothing to re-bless when retrieval legitimately improves.
//
// It exists because of a specific escape. On 2026-08-11 RRF ties broke
// arbitrarily, so two identical runs ranked differently — and every number the
// harness reported said the runs were identical, because tier-2 metrics collapse
// chunks to documents and the document SET was the same. The run looked perfectly
// reproducible by every available signal.
//
// So this comparison must look at what the metrics are blind to: the chunk-level
// rank order.

func detail(id string, chunks ...string) RetrievalDetail {
	return RetrievalDetail{
		ItemID: id, Category: "c", Question: "q-" + id,
		RetrievedChunks:    chunks,
		RetrievedDocuments: distinctInOrder(chunks),
	}
}

func TestCompareRetrieval_IdenticalRunsAgree(t *testing.T) {
	a := []RetrievalDetail{detail("i1", "d1#1", "d2#1"), detail("i2", "d3#1")}
	b := []RetrievalDetail{detail("i1", "d1#1", "d2#1"), detail("i2", "d3#1")}
	if err := CompareRetrieval(a, b); err != nil {
		t.Errorf("identical runs reported a divergence: %v", err)
	}
}

// TestCompareRetrieval_CatchesReorderWithinTheSameDocumentSet is the 2026-08-11
// bug, reduced. Both runs retrieved the same two documents; only the chunk order
// differs. Tier-2 metrics score these identically, which is exactly why the
// comparison cannot be built on them.
func TestCompareRetrieval_CatchesReorderWithinTheSameDocumentSet(t *testing.T) {
	a := []RetrievalDetail{detail("i1", "d1#1", "d2#1")}
	b := []RetrievalDetail{detail("i1", "d2#1", "d1#1")}

	err := CompareRetrieval(a, b)
	if err == nil {
		t.Fatal("a chunk-order difference went undetected — this is the RRF-tie defect, " +
			"and metrics-based comparison is blind to it by construction")
	}
	if !strings.Contains(err.Error(), "i1") {
		t.Errorf("error %q does not name the diverging item", err)
	}
	// The message has to show BOTH orders, or a reader cannot tell which run moved.
	if !strings.Contains(err.Error(), "d1#1") || !strings.Contains(err.Error(), "d2#1") {
		t.Errorf("error %q does not show the two orderings", err)
	}
}

func TestCompareRetrieval_CatchesDifferentChunkCount(t *testing.T) {
	a := []RetrievalDetail{detail("i1", "d1#1", "d2#1")}
	b := []RetrievalDetail{detail("i1", "d1#1")}
	if err := CompareRetrieval(a, b); err == nil {
		t.Error("a run that retrieved fewer chunks compared equal")
	}
}

// TestCompareRetrieval_CatchesDifferentPopulation: two runs over different item
// sets are not a determinism result at all — they are a configuration mistake, and
// silently comparing their intersection would report determinism the runs never
// demonstrated.
func TestCompareRetrieval_CatchesDifferentPopulation(t *testing.T) {
	a := []RetrievalDetail{detail("i1", "d1#1"), detail("i2", "d2#1")}
	b := []RetrievalDetail{detail("i1", "d1#1")}

	err := CompareRetrieval(a, b)
	if err == nil {
		t.Fatal("runs over different item populations compared equal")
	}
	if !strings.Contains(err.Error(), "i2") {
		t.Errorf("error %q should name the item missing from the second run", err)
	}
}

// TestCompareRetrieval_OrderOfItemsDoesNotMatter: items are matched by id, so a
// difference in the ORDER questions were asked is not a retrieval divergence.
// Otherwise the gate would fail on an irrelevant scheduling change.
func TestCompareRetrieval_OrderOfItemsDoesNotMatter(t *testing.T) {
	a := []RetrievalDetail{detail("i1", "d1#1"), detail("i2", "d2#1")}
	b := []RetrievalDetail{detail("i2", "d2#1"), detail("i1", "d1#1")}
	if err := CompareRetrieval(a, b); err != nil {
		t.Errorf("item ordering was treated as a divergence: %v", err)
	}
}

// TestCompareRetrieval_CatchesAnErrorAppearingInOneRun: an item that faulted in
// one run and not the other is non-deterministic even if both retrieved nothing —
// "retrieved nothing" and "failed" are different states, which RetrievalDetail
// already keeps apart.
func TestCompareRetrieval_CatchesAnErrorAppearingInOneRun(t *testing.T) {
	a := []RetrievalDetail{detail("i1")}
	failed := detail("i1")
	failed.Error = "recall: timeout"
	b := []RetrievalDetail{failed}

	if err := CompareRetrieval(a, b); err == nil {
		t.Error("an item that faulted in only one run compared equal; a flaky recall is " +
			"precisely the thing a determinism gate should catch")
	}
}

// TestCompareRetrieval_EmptyRunsAreNotAPass: comparing two runs that scored
// nothing must not report success. A gate that passes on an empty result set is a
// gate that passes when the harness silently did no work.
func TestCompareRetrieval_EmptyRunsAreNotAPass(t *testing.T) {
	if err := CompareRetrieval(nil, nil); err == nil {
		t.Error("two empty runs compared equal — a gate must not go green on a run that " +
			"retrieved nothing at all")
	}
}
