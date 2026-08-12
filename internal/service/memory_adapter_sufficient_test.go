package service

import (
	"testing"

	"vornik.io/vornik/internal/api"
)

// TestRecallRouting_PropagatesSufficient is a regression test for a silently
// dropped option.
//
// The companion recall handler PREFERS RecallRouting when the adapter supports it.
// That method built its own SearchOptions and omitted Sufficient, so a caller
// asking for the context-assembly retrieval mode had the request accepted at the
// tool boundary and discarded one layer down: opts.Sufficient reached the adapter
// and never reached the searcher.
//
// Consequence: RecallSufficient — the only thing that sets opts.Rerank — was never
// invoked from the companion surface, so the reranker could be fully ACTIVE and
// still never fire. That is exactly what the production ledger showed: 151,818 LLM
// usage rows, zero reranker calls.
//
// This test asserts the option survives the adapter. It is deliberately a
// field-level assertion rather than an end-to-end one: the bug was a struct literal
// missing a field, and only a field-level check catches that class of mistake.
func TestRecallRouting_PropagatesSufficient(t *testing.T) {
	// Build the options a companion caller would produce.
	in := api.RecallOptions{
		Limit:       8,
		RepoScope:   "membench/native/shared",
		StrictScope: true,
		Sufficient:  true,
	}

	got := recallRoutingSearchOptions(in)

	if !got.Rerank {
		t.Error("Sufficient did not map onto SearchOptions.Rerank — the " +
			"context-assembly mode is silently dropped and the reranker never fires, " +
			"because rerankOn := opts.Rerank && s.rerankerActive()")
	}
	if !got.Routing {
		t.Error("Routing must stay on; the confidence-verdict path is not being replaced")
	}
	if !got.StrictScope || got.RepoScope != in.RepoScope || got.Limit != in.Limit {
		t.Errorf("unrelated options mangled: %+v", got)
	}
}

// TestRecallRouting_SufficientDefaultsOff — the interactive default must not
// change. Reranking adds an LLM call per recall.
func TestRecallRouting_SufficientDefaultsOff(t *testing.T) {
	got := recallRoutingSearchOptions(api.RecallOptions{Limit: 5})
	if got.Rerank {
		t.Error("Rerank defaulted on; interactive recall would pay for a rerank " +
			"nobody asked for")
	}
}

// TestRecallSearchOptions_PropagatesSufficient — the non-routing fallback path must
// carry it too, or a deployment without routing support loses the mode.
func TestRecallSearchOptions_PropagatesSufficient(t *testing.T) {
	got := recallSearchOptions(api.RecallOptions{Sufficient: true, Limit: 3})
	if !got.Rerank {
		t.Error("Sufficient dropped on the non-routing recall path")
	}
}
