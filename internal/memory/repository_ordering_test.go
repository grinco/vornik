package memory

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Recall must be REPRODUCIBLE: equal fused scores have to come back in a stable
// order, or the same query against unchanged data returns different results.
//
// Regression for the 2026-08-11 finding. RRF scores are a function of rank position,
// so ties are common — two arms' candidates routinely fuse to identical scores. With
// a bare `ORDER BY score DESC`, Postgres is free to return tied rows in any order,
// and it did: two runs of the same query over the same corpus swapped positions 3 and
// 4. The tier-2 metrics happened not to notice (same document SET, gold document not
// at the boundary), which is precisely what makes it dangerous — a tie sitting at the
// budget boundary would flip recall and MRR, and the exact-equality CI gate this
// harness relies on would fail on nothing at all.
func TestRecallQueriesOrderDeterministically(t *testing.T) {
	src := readRepositorySource(t)

	// Every recall query that ranks by fused score must carry a tiebreak after it.
	bare := regexp.MustCompile(`ORDER BY score DESC\s+LIMIT`)
	if locs := bare.FindAllStringIndex(src, -1); len(locs) > 0 {
		t.Errorf("%d recall query(ies) order by score with no tiebreak; equal RRF scores "+
			"would return in arbitrary order and recall would not be reproducible", len(locs))
	}

	// And the tiebreak must be on a unique column, or it is not a tiebreak.
	withTiebreak := strings.Count(src, "ORDER BY score DESC, c.id")
	if withTiebreak == 0 {
		t.Error("no recall query orders by score with a c.id tiebreak")
	}
}

// TestRankWindowsBreakTiesDeterministically — the rank each arm assigns must also be
// stable, since an arbitrary rank feeds an arbitrary fused score. row_number() over a
// window with ties assigns them in unspecified order.
func TestRankWindowsBreakTiesDeterministically(t *testing.T) {
	src := readRepositorySource(t)
	// The semantic arm orders by vector distance; ties need a stable secondary key.
	if strings.Contains(src, "row_number() OVER (ORDER BY embedding <=> $4::vector)") {
		t.Error("the semantic rank window has no tiebreak: chunks at equal distance " +
			"get arbitrary ranks, which propagates into the fused score")
	}
}

func readRepositorySource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("repository.go")
	if err != nil {
		t.Fatalf("read repository.go: %v", err)
	}
	return string(b)
}
