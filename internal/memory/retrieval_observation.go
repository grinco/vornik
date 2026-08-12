package memory

import "context"

// RetrievalObservation is what a search actually DID, written back through ctx
// for a caller that needs to describe the path rather than request it.
//
// It exists because "is the reranker on?" has no useful answer at the config
// layer. A rerank runs only when the reranker is enabled AND non-Noop AND the
// caller opted in AND there is more than one result — and even then the call can
// blow its deadline and be discarded, leaving the caller holding RRF order. The
// 2026-08-14 baseline reranked 355 of 400 queries, so one run is routinely a
// MIXTURE of both orderings.
//
// The cost of having no such report was measured. Three `--tier2-only` benchmark
// runs on 2026-08-12 each billed ten cloud reranker calls and returned three
// different rankings of a byte-identical corpus, in a mode documented as needing
// no reranker and nothing billable. The recall response said nothing either way,
// so the only witness was the usage ledger — read by hand, after the determinism
// gate had already refused the runs.
//
// Optional by construction: every existing caller threads a ctx with no bag, and
// a search with no bag behaves exactly as it did before.
type RetrievalObservation struct {
	// Reranked is true only when a rerank ran AND returned an ordering that was
	// used. A rerank that errored leaves this false, because the results the
	// caller received are RRF order and calling them reranked would be a lie
	// told by a correctly-configured system.
	Reranked bool
	// RerankAttempted is true when the searcher tried. It separates "never
	// tried" from "tried and lost it to the deadline" — without the split, a
	// deployment silently losing every rerank looks identical to one with the
	// reranker switched off.
	RerankAttempted bool
}

// Method renders the observation as a recall-method token, matching the
// vocabulary of the benchmark harness's --recall-method flag so an observed value
// can be compared against, or substituted for, a declared one.
func (o *RetrievalObservation) Method() string {
	if o == nil {
		return ""
	}
	if o.Reranked {
		return "context-assembly+rerank"
	}
	return "context-assembly"
}

type retrievalObservationKey struct{}

// WithRetrievalObservation stamps a writable observation onto ctx. Returns ctx
// unchanged when obs is nil, so callers thread it without nil-checking — and so a
// nil never installs a bag that nothing can write to.
func WithRetrievalObservation(ctx context.Context, obs *RetrievalObservation) context.Context {
	if obs == nil {
		return ctx
	}
	return context.WithValue(ctx, retrievalObservationKey{}, obs)
}

// RetrievalObservationFromContext returns the stamped observation, or nil.
//
// Deliberately a POINTER, unlike RetrievalContextFromContext's value copy: this
// bag exists to be written back into by the search path.
func RetrievalObservationFromContext(ctx context.Context) *RetrievalObservation {
	if v, ok := ctx.Value(retrievalObservationKey{}).(*RetrievalObservation); ok {
		return v
	}
	return nil
}
