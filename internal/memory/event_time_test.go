package memory

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Slice 0a of https://docs.vornik.io
// §4.1. Chunks carry an event time — when the content PERTAINS TO — distinct
// from created_at, which is when we wrote it. Every temporal *filter* moves to
// COALESCE(event_time, created_at); the three uses in §4.1.1 deliberately stay
// on created_at.

// eventTimeExpr is the read-path expression every temporal filter must use.
// NULL event_time falls back to created_at, which is what makes the migration
// strictly widening: pre-migration chunks behave exactly as before.
const eventTimeExpr = "COALESCE(event_time, created_at)"

// temporalPredicateRE matches a temporal *comparison* against the chunk
// timestamp — the thing that must migrate. Deliberately NOT matching every
// mention of created_at: ORDER BY (recent_memory), the TTL freshness leg, and
// backfill scan order are correct on created_at and must not trip this
// (design §4.1.1).
var temporalPredicateRE = regexp.MustCompile(`(?:\w+\.)?created_at\s*[<>]=`)

// eventTimePredicateRE is the migrated form of the same comparison.
var eventTimePredicateRE = regexp.MustCompile(`COALESCE\(\s*(?:\w+\.)?event_time\s*,\s*(?:\w+\.)?created_at\s*\)\s*[<>]=`)

// wantTemporalFuncs is the checked-in membership set: the functions in
// repository.go that filter by chunk time and therefore must use
// eventTimeExpr. Six functions holding eight predicate pairs (sixteen lines) —
// some functions carry two pairs because their SQL has both a semantic and a
// keyword arm.
//
// Keyed on FUNCTION NAME, not line number, deliberately — line numbers drift
// on every unrelated edit above them, and a guard that nags constantly is a
// guard that gets deleted. See design §4.1 and round-2 review
// review-20260810-637c finding 2.
//
// This list is longer than the design's first draft: the guard's own first run
// surfaced substringSearchTemporal and substringSearchWithEpochsTemporal,
// which the design table had mislabelled as "routing/widen path" (the line
// numbers were right, the attribution was not). That is the guard doing the
// job it was added for, on its first execution.
var wantTemporalFuncs = []string{
	"hybridSearchTemporal",
	"keywordSearchTemporal",
	"keywordSearchWithEpochsTemporal",
	"HybridSearchWithEpochs",
	"substringSearchTemporal",
	"substringSearchWithEpochsTemporal",
}

// TestTemporalPredicates_UseEventTimeFallback is the migration completeness
// gate. A PARTIAL migration is the dangerous failure: one stale predicate
// means one retrieval path answers on a different clock than its siblings,
// which is close to undiagnosable from outside.
func TestTemporalPredicates_UseEventTimeFallback(t *testing.T) {
	src, err := os.ReadFile("repository.go")
	if err != nil {
		t.Fatalf("read repository.go: %v", err)
	}
	for i, line := range strings.Split(string(src), "\n") {
		if !temporalPredicateRE.MatchString(line) {
			continue
		}
		// A migrated predicate contains created_at inside the COALESCE, so it
		// matches temporalPredicateRE too. Only an UNmigrated one lacks the
		// COALESCE wrapper.
		if eventTimePredicateRE.MatchString(line) {
			continue
		}
		t.Errorf("repository.go:%d filters on bare created_at; must use %s\n\t%s\n"+
			"(design §4.1 — if this is deliberately an ingest-time use, see §4.1.1)",
			i+1, eventTimeExpr, strings.TrimSpace(line))
	}
}

// TestTemporalFuncMembership_Unchanged fails when the SET of chunk-time-
// filtering functions changes in either direction.
//
// An ADDITION means someone wrote a ninth temporal query and must consciously
// choose its clock — this catches the copy-paste case even when the new code
// uses a different SQL form, because it keys on existence rather than pattern.
// A REMOVAL means this expectation went stale and the guard is no longer
// guarding what it claims.
func TestTemporalFuncMembership_Unchanged(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "repository.go", nil, 0)
	if err != nil {
		t.Fatalf("parse repository.go: %v", err)
	}

	got := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			return true
		}
		ast.Inspect(fn.Body, func(inner ast.Node) bool {
			lit, ok := inner.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if temporalPredicateRE.MatchString(lit.Value) ||
				eventTimePredicateRE.MatchString(lit.Value) {
				got[fn.Name.Name] = true
			}
			return true
		})
		return true
	})

	gotNames := make([]string, 0, len(got))
	for name := range got {
		gotNames = append(gotNames, name)
	}
	sort.Strings(gotNames)

	want := append([]string(nil), wantTemporalFuncs...)
	sort.Strings(want)

	if strings.Join(gotNames, ",") != strings.Join(want, ",") {
		t.Errorf("set of chunk-time-filtering functions changed.\n got: %v\nwant: %v\n"+
			"A NEW function here must choose its clock deliberately (design §4.1 vs §4.1.1);\n"+
			"a REMOVED one means wantTemporalFuncs is stale. Update this list with the reason.",
			gotNames, want)
	}
}

// TestIngestTimeUses_StayOnCreatedAt pins the three deliberate
// non-migrations (design §4.1.1). Found between review rounds and raised by
// neither reviewer: sweeping the file for "the old clock" breaks all three,
// and that direction of error degrades live behaviour rather than leaving it
// unchanged.
//
//   - recent_memory ordering answers "what just landed" — a write-order
//     question. On event time, a freshly-ingested 2019 document sorts as old
//     and vanishes from the digest that exists to surface it.
//   - the TTL freshness leg means "how long ago did we learn this". A doc
//     about 2019 ingested yesterday is FRESH knowledge.
//   - backfill scan order must stay monotonic or a resumable scan skips rows.
func TestIngestTimeUses_StayOnCreatedAt(t *testing.T) {
	src, err := os.ReadFile("repository.go")
	if err != nil {
		t.Fatalf("read repository.go: %v", err)
	}
	text := string(src)

	for _, want := range []string{
		"ORDER BY created_at ASC",  // backfill scan order
		"ORDER BY created_at DESC", // recent_memory / audit listings
	} {
		if !strings.Contains(text, want) {
			t.Errorf("%q no longer present in repository.go — an ingest-time use was "+
				"migrated to event time. See design §4.1.1: recency digests, TTL "+
				"freshness and backfill order are questions about when we LEARNED "+
				"something, not when it happened.", want)
		}
	}

	// The freshness/TTL leg selects created_at; if that column stopped being
	// selected the P3 trust verdict would lose its input.
	if !strings.Contains(text, "c.created_at") {
		t.Error("repository.go no longer selects c.created_at — the P3 freshness leg " +
			"(routing.go) needs ingest time to score staleness (design §4.1.1)")
	}
}
