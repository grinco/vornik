package membench

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrTier2PathUnverified is terminal: a tier-2-only run could not establish that
// it measured the deterministic retrieval path.
//
// Deliberately neutral about WHY. It covers both a confirmed rerank and a system
// that would not say, and wording it as "measured a reranked path" would make the
// unverifiable case read as a false accusation — the wrapped message carries the
// specifics either way.
var ErrTier2PathUnverified = errors.New("membench: tier-2-only run could not verify it measured the deterministic retrieval path")

// verifyTier2Path refuses a tier-2-only recall whose observed method is reranked,
// or unreportable.
//
// The mode exists to gate retrieval per-change, which rests on the retrieval path
// being deterministic — sd exactly 0.0000 across ten RRF runs, against sd ~4.5%
// for judged accuracy at n=30. An LLM reranker inside the path destroys that
// (§13.9), and it is billed per call. On 2026-08-12 three tier-2-only runs of a
// ten-question fixture billed 30 cloud reranker calls and produced three different
// chunk rankings of a byte-identical corpus, in a mode whose own documentation
// promised neither could happen.
//
// FAILS CLOSED on an empty method. An unreportable path is not a verified-clean
// path, and treating it as one produces a check that passes exactly when it
// learned nothing — which is what `--i-know-this-wipes` did before writing twelve
// documents into the production corpus.
func verifyTier2Path(observed string) error {
	method := strings.TrimSpace(observed)
	if method == "" {
		return fmt.Errorf("%w: the system did not report which retrieval path it took, so "+
			"this run cannot show it measured the deterministic one. A daemon predating "+
			"recall's `retrieval_method` field cannot serve a tier-2 gate: upgrade it, or "+
			"run without --tier2-only and accept a judged run's variance",
			ErrTier2PathUnverified)
	}
	if strings.Contains(method, "rerank") {
		return fmt.Errorf("%w: recall reported %q. An LLM reranker reorders results between "+
			"otherwise identical runs and is billed per call, so a gate on this path fires on "+
			"reranker noise rather than on a retrieval regression. Set "+
			"`memory.reranker.enabled: false` on the deployment under test — --tier2-only "+
			"stops REQUESTING the reranked path but cannot switch off a reranker the daemon "+
			"applies anyway",
			ErrTier2PathUnverified, method)
	}
	return nil
}

// observedRecallMethod collapses the methods seen across a run into one value for
// the comparability key.
//
// A single method is that method. Several — the reranked path's deadline makes a
// mixture normal — are joined, because two runs that reranked different fractions
// of their queries are not the same experiment and must not share a key. That is
// the 2026-08-11 failure exactly: pre-fix and post-fix reranker runs shared a
// byte-identical key because the key carried no recall method at all.
func observedRecallMethod(seen map[string]struct{}) string {
	if len(seen) == 0 {
		return ""
	}
	methods := make([]string, 0, len(seen))
	for m := range seen {
		methods = append(methods, m)
	}
	sort.Strings(methods) // stable key across runs that saw the same mixture
	// "|" rather than "+", because the method names THEMSELVES contain "+":
	// joining with "+" renders a mixture of context-assembly and
	// context-assembly+rerank as "context-assembly+context-assembly+rerank",
	// which reads like one exotic third method instead of two.
	return strings.Join(methods, "|")
}

// GateSuppressesRerank reports whether a run should ask its system NOT to rerank.
//
// True only in GATE mode: tier-2-only without an explicit acceptance of an
// unverified path. That is the one case wanting determinism more than fidelity.
//
// Once the operator accepts an unverified path they are measuring systems as they
// ship — and a competitor that reranks internally, with no switch to disable it,
// must not be compared against an artificially unreranked vornik. That difference
// would be manufactured by our own gate plumbing rather than by either product.
func GateSuppressesRerank(tier2Only, acceptUnverifiedPath bool) bool {
	return tier2Only && !acceptUnverifiedPath
}
