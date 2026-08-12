package membench

import (
	"fmt"
	"sort"
	"strings"
)

// CompareRetrieval reports whether two runs retrieved byte-identically.
//
// This is the run-twice determinism assertion: run the same fixture twice and
// require the same retrieval. It is the cheapest gate in the retrieval-CI design
// and the only one that cannot go stale — it needs no committed baseline, so
// there is nothing to re-bless when retrieval legitimately improves.
//
// WHY IT COMPARES CHUNKS AND NOT METRICS. On 2026-08-11 RRF ties broke
// arbitrarily, so two identical runs ranked differently. Every number the harness
// reported said they were identical, because tier-2 metrics collapse chunks to
// documents and the document SET was unchanged. The run looked perfectly
// reproducible by every available signal. A comparison built on the metrics would
// have been blind to the defect it most needs to catch, so this one reads the
// chunk-level rank order the metrics discard.
//
// Returns nil when the two runs agree. Otherwise the error names the first
// divergence concretely enough to act on: which item, and both orderings.
func CompareRetrieval(a, b []RetrievalDetail) error {
	// Two empty runs are not a pass. A gate that goes green when the harness
	// silently did no work is worse than no gate, because it reports a property it
	// never tested.
	if len(a) == 0 && len(b) == 0 {
		return fmt.Errorf("both runs recorded no retrieval at all: nothing was compared, so " +
			"determinism was not demonstrated")
	}

	byID := func(rs []RetrievalDetail) map[string]RetrievalDetail {
		m := make(map[string]RetrievalDetail, len(rs))
		for _, r := range rs {
			m[r.ItemID] = r
		}
		return m
	}
	first, second := byID(a), byID(b)

	// Population first. Two runs over different item sets are a configuration
	// mistake, not a determinism result, and comparing only their intersection
	// would report determinism the runs never demonstrated.
	var missing []string
	for id := range first {
		if _, ok := second[id]; !ok {
			missing = append(missing, id+" (absent from run B)")
		}
	}
	for id := range second {
		if _, ok := first[id]; !ok {
			missing = append(missing, id+" (absent from run A)")
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("the two runs scored different item populations, so they cannot be "+
			"compared for determinism: %s", strings.Join(missing, ", "))
	}

	// Deterministic report order, so a CI log is stable across reruns.
	ids := make([]string, 0, len(first))
	for id := range first {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		x, y := first[id], second[id]

		// A fault in one run only is non-determinism even when both retrieved
		// nothing: "retrieved nothing" and "failed" are different states, and
		// RetrievalDetail already keeps them apart precisely so this stays visible.
		if x.Error != y.Error {
			return fmt.Errorf("item %s faulted differently across runs:\n  run A: %s\n  run B: %s",
				id, errOrNone(x.Error), errOrNone(y.Error))
		}

		if !equalStrings(x.RetrievedChunks, y.RetrievedChunks) {
			return fmt.Errorf("item %s retrieved a different chunk RANKING across two runs of "+
				"the same fixture:\n  run A: %s\n  run B: %s\n"+
				"document-level metrics may be identical and still hide this, which is the "+
				"shape of the 2026-08-11 RRF tie-breaking defect",
				id, joinOrEmpty(x.RetrievedChunks), joinOrEmpty(y.RetrievedChunks))
		}
	}
	return nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func errOrNone(s string) string {
	if s == "" {
		return "(no error)"
	}
	return s
}

func joinOrEmpty(s []string) string {
	if len(s) == 0 {
		return "(nothing retrieved)"
	}
	return strings.Join(s, ", ")
}
