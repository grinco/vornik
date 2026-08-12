package memory

import (
	"context"
	"errors"
	"testing"
)

// What the retrieval path DID, as opposed to what it was configured to do.
//
// On 2026-08-12 three `--tier2-only` benchmark runs — a mode whose help text
// promises "no answer generation, no judge, no model credentials" and whose
// design rationale added "no reranker, nothing billable" — each billed ten cloud
// reranker calls, and produced three different chunk rankings over a
// byte-identical corpus. Nothing in the recall response said a rerank had
// happened, so the only witness was the usage ledger, consulted by hand after
// the determinism gate refused the runs.
//
// Config cannot answer this question. `memory.reranker.enabled: true` means a
// rerank MAY run: it also needs opts.Rerank from the caller, a non-Noop reranker,
// and more than one result. Worse, the call can blow its 8s deadline and be
// discarded — the 2026-08-14 baseline reranked 355 of 400 queries, so a single
// run is a MIXTURE of reranked and RRF orderings. A gate told "the reranker is
// enabled" learns nothing actionable, and a gate told "it is disabled" is
// trusting a config read for a claim about behaviour.
//
// So the searcher reports what it actually did, per search.

func TestRetrievalObservation_RerankedOnlyWhenARerankSucceeded(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	s := NewSearcher(Config{}, r, nil)
	s.SetReranker(&stubReranker{})

	mock.ExpectQuery("ts_rank").
		WithArgs("p", "q", 15, "q").
		WillReturnRows(makeRR([]string{"a", "b", "c"}, []float64{0.9, 0.7, 0.5}))

	obs := &RetrievalObservation{}
	ctx := WithRetrievalObservation(context.Background(), obs)
	if _, err := s.SearchWithOptions(ctx, "p", "q", SearchOptions{Limit: 5, Rerank: true}); err != nil {
		t.Fatal(err)
	}

	if !obs.Reranked {
		t.Error("a successful rerank was not observed; the recall response would then " +
			"describe this as an unreranked search, which is how a tier-2 gate ends up " +
			"gating a path it did not measure")
	}
	if !obs.RerankAttempted {
		t.Error("RerankAttempted should be true when the rerank ran")
	}
}

// TestRetrievalObservation_NotRerankedWhenTheRerankerErrors is the mixture case,
// and the reason this cannot be derived from configuration. The reranker is
// enabled, wired, and requested — and the ordering the caller receives is plain
// RRF, because the call failed and the searcher kept the RRF order. Reporting
// "reranked" here would be a lie told by a correctly-configured system.
func TestRetrievalObservation_NotRerankedWhenTheRerankerErrors(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	s := NewSearcher(Config{}, r, nil)
	s.SetReranker(&stubReranker{err: errors.New("deadline exceeded")})

	mock.ExpectQuery("ts_rank").
		WithArgs("p", "q", 15, "q").
		WillReturnRows(makeRR([]string{"a", "b", "c"}, []float64{0.9, 0.7, 0.5}))

	obs := &RetrievalObservation{}
	ctx := WithRetrievalObservation(context.Background(), obs)
	if _, err := s.SearchWithOptions(ctx, "p", "q", SearchOptions{Limit: 5, Rerank: true}); err != nil {
		t.Fatal(err)
	}

	if obs.Reranked {
		t.Error("a FAILED rerank was reported as reranked; the results are RRF order, and " +
			"the 2026-08-14 baseline lost 45 of 400 reranks to the 8s deadline, so this is " +
			"the common case, not a corner")
	}
	// It was still attempted, and that distinction is the point: an operator
	// debugging a mixed run needs to tell "never tried" from "tried and lost it".
	if !obs.RerankAttempted {
		t.Error("RerankAttempted should be true even when the rerank errored — otherwise a " +
			"deployment silently losing every rerank to its deadline looks identical to one " +
			"with the reranker switched off")
	}
}

func TestRetrievalObservation_NotRerankedWhenNotRequested(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	s := NewSearcher(Config{}, r, nil)
	s.SetReranker(&stubReranker{})

	// Rerank not opted into → no widened fetch, no rerank.
	mock.ExpectQuery("ts_rank").
		WithArgs("p", "q", 5, "q").
		WillReturnRows(makeRR([]string{"a", "b"}, []float64{0.9, 0.7}))

	obs := &RetrievalObservation{}
	ctx := WithRetrievalObservation(context.Background(), obs)
	if _, err := s.SearchWithOptions(ctx, "p", "q", SearchOptions{Limit: 5}); err != nil {
		t.Fatal(err)
	}

	if obs.Reranked || obs.RerankAttempted {
		t.Errorf("an unreranked search reported rerank activity: %+v — a reranker being "+
			"WIRED is not a reranker RUNNING, which is exactly the confusion that let a "+
			"tier-2-only run bill 30 reranker calls", obs)
	}
}

// TestRetrievalObservation_AbsentBagIsSafe: every existing caller passes a ctx
// with no observation in it. Stamping must be optional, or adding this reporting
// breaks every recall path in the daemon.
func TestRetrievalObservation_AbsentBagIsSafe(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	s := NewSearcher(Config{}, r, nil)
	s.SetReranker(&stubReranker{})

	mock.ExpectQuery("ts_rank").
		WithArgs("p", "q", 15, "q").
		WillReturnRows(makeRR([]string{"a", "b"}, []float64{0.9, 0.7}))

	if _, err := s.SearchWithOptions(context.Background(), "p", "q", SearchOptions{Limit: 5, Rerank: true}); err != nil {
		t.Fatalf("a search with no observation bag must behave exactly as before: %v", err)
	}
}

func TestRetrievalObservationFromContext_NilAndMissing(t *testing.T) {
	if got := RetrievalObservationFromContext(context.Background()); got != nil {
		t.Errorf("expected nil for an unstamped ctx, got %+v", got)
	}
	//nolint:staticcheck // deliberately passing nil: callers thread optionally.
	if ctx := WithRetrievalObservation(context.Background(), nil); RetrievalObservationFromContext(ctx) != nil {
		t.Error("stamping nil must leave the ctx without an observation rather than " +
			"installing a bag nothing can write to")
	}
}
