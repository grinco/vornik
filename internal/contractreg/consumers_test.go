package contractreg

import "testing"

// TestSharedTable_BothConsumersReadItOppositely guards the coupling risk the
// review flagged (finding 7): one table, two consumers reading it in opposite
// directions, so a schema change can break either.
//
//   - DRIFT asks "does everything the contracts name exist?" — it needs the
//     Declared rows AND their Status, so it can demand existence only for docs
//     that claim to have shipped.
//   - REACHABILITY asks "is everything that exists called or named?" — it must
//     IGNORE Declared rows entirely, because a promise in prose is not evidence
//     that code is live.
//
// Those requirements are contradictory by design, which is exactly why the Status
// column exists. This test exercises both directions against one table so a
// future change cannot satisfy one consumer while silently breaking the other.
func TestSharedTable_BothConsumersReadItOppositely(t *testing.T) {
	tbl := New()

	// Reachability-relevant surfaces.
	tbl.Add(KindAgentToolDispatch, "memory_search", "entrypoint.sh:1665")
	tbl.Add(KindSystemHandler, "forge.post_review", "wiring")

	// Drift-relevant rows: one shipped promise, one unbuilt promise.
	tbl.AddWithStatus(KindDeclared, "internal/shipped/thing.go", "shipped-design.md", "shipped")
	tbl.AddWithStatus(KindDeclared, "internal/future/thing.go", "future-design.md", "design")

	// --- the REACHABILITY reading ---
	// AnyNamed is the live-by-contract predicate. It must see the dispatch and
	// handler names and must NOT see either Declared row.
	for _, live := range []string{"memory_search", "forge.post_review"} {
		if !tbl.AnyNamed(live) {
			t.Errorf("AnyNamed(%q) = false — reachability would report a name-dispatched "+
				"entry point as dead", live)
		}
	}
	for _, declared := range []string{"internal/shipped/thing.go", "internal/future/thing.go"} {
		if tbl.AnyNamed(declared) {
			t.Errorf("AnyNamed(%q) = true — a delivers: promise must never rescue a symbol, "+
				"or static analysis becomes hostage to doc upkeep", declared)
		}
	}

	// --- the DRIFT reading ---
	// The same rows must be visible WITH their status, so the drift consumer can
	// filter to shipped docs before demanding the artefact exist.
	shipped := tbl.Get(KindDeclared, "internal/shipped/thing.go")
	if shipped == nil || shipped.Status != "shipped" {
		t.Fatalf("drift consumer cannot see the shipped Declared row with its status: %+v", shipped)
	}
	future := tbl.Get(KindDeclared, "internal/future/thing.go")
	if future == nil || future.Status != "design" {
		t.Fatalf("drift consumer cannot see the unbuilt Declared row with its status: %+v", future)
	}
	if len(tbl.Names(KindDeclared)) != 2 {
		t.Errorf("Names(KindDeclared) = %v, want both promises — the drift consumer enumerates them",
			tbl.Names(KindDeclared))
	}
}

// TestTable_AddMergesSourcesRatherThanDuplicating — a name declared on several
// lines of one file is one entry with several sources, because a finding wants to
// point at every declaration site.
func TestTable_AddMergesSourcesRatherThanDuplicating(t *testing.T) {
	tbl := New()
	tbl.Add(KindAgentToolDispatch, "grep", "entrypoint.sh:10")
	tbl.Add(KindAgentToolDispatch, "grep", "entrypoint.sh:20")
	tbl.Add(KindAgentToolDispatch, "grep", "entrypoint.sh:10") // repeat

	if names := tbl.Names(KindAgentToolDispatch); len(names) != 1 {
		t.Fatalf("Names = %v, want one entry", names)
	}
	e := tbl.Get(KindAgentToolDispatch, "grep")
	if e == nil || len(e.Sources) != 2 {
		t.Fatalf("Sources = %v, want the two distinct sites deduped", e)
	}
	if e.Sources[0] != "entrypoint.sh:10" || e.Sources[1] != "entrypoint.sh:20" {
		t.Errorf("Sources = %v, want sorted for stable output", e.Sources)
	}
}

// TestTable_EmptyNameIsIgnored — a parser that yields "" must not create a
// phantom entry that then fails a check against nothing.
func TestTable_EmptyNameIsIgnored(t *testing.T) {
	tbl := New()
	tbl.Add(KindAgentToolDispatch, "", "somewhere")
	tbl.Add(KindAgentToolDispatch, "   ", "somewhere")
	if names := tbl.Names(KindAgentToolDispatch); len(names) != 0 {
		t.Errorf("Names = %v, want none", names)
	}
}

// TestTable_NamesIsEmptyNotNil — an absent surface is a legitimate state, and
// callers range over the result without a nil check.
func TestTable_NamesIsEmptyNotNil(t *testing.T) {
	tbl := New()
	if got := tbl.Names(KindExtractor); got == nil {
		t.Error("Names must return an empty slice, never nil")
	}
	if got := tbl.Set(KindExtractor); got == nil {
		t.Error("Set must return an empty map, never nil")
	}
}
