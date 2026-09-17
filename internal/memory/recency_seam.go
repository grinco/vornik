package memory

import "time"

// Test seam for the cross-driver recency contract suite
// (internal/persistence/repotest/retrieval_recency_suite.go).
//
// Why this exists rather than the suite re-implementing the re-rank: the
// incident this whole slice exists for was NOT a unit-level defect. The curve
// was right, the store was right, and the stale digest still won — the failure
// only appears when a flag DERIVED BY THE DATABASE meets the scorer. So the
// contract suite has to feed the real predicate's output into the real scorer;
// a second copy of the scoring arithmetic in repotest would be a safety check
// with two implementations, one of which is wrong and nobody knows which.
//
// The suite lives in repotest because it must run on BOTH drivers, and
// repotest is where this repository keeps a suite the sqlite and pgvector
// lanes both point at. That puts it outside package memory, hence this
// two-line export. It is deliberately the only one: everything else the suite
// needs (SearchResult, RecencyConfig, DefaultRecencyConfig, SetEnabled) is
// already exported for production callers.
//
// Design: https://docs.vornik.io §5.5, §7.

// ApplyRecencyForTest exposes the shipped §5.5 re-rank — rescore by
// freshness^weight × seriesWeight, re-sort, THEN cut to limit — to the
// cross-driver contract suite. It is a pass-through: no behaviour lives here,
// so a test that passes through this function is testing applyRecency itself.
func ApplyRecencyForTest(in []SearchResult, cfg RecencyConfig, now time.Time, limit int) []SearchResult {
	return applyRecency(in, cfg, now, limit)
}
