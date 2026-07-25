package chat

import "context"

// Unwrapper is implemented by every decorator in this package: LoggingProvider,
// FallbackProvider, QueuedProvider, BoundedRouteProvider, HealthGatedProvider.
//
// Providers here are composed as decorator chains — a router wrapped in a
// model-fallback layer wrapped in logging, with each route additionally wrapped
// in a queue bound and a health gate. Only the mandatory Provider methods are
// forwarded by every layer. Optional capabilities (ModelLister, Pinger, the
// router's own aggregating ListModels) were reached by type-asserting the
// outermost value, so every new decorator silently severed whatever it did not
// happen to re-declare.
//
// That failed three times. LoggingProvider and QueuedProvider each grew a
// bespoke ListModelsAggregated shim; FallbackProvider (2026-07-18, d5180cdd)
// was missed, and because it sits between logging and the router, `vornikctl
// models list` returned an empty list with no error for anyone who configured
// chat.router.model_fallbacks. The per-route wrappers drop Ping the same way,
// which quietly turned every sub-provider readiness probe into a no-op.
//
// Exposing the inner provider once, here, is what makes capability lookup a
// property of the chain rather than of whichever type happens to be outermost.
// A new decorator only has to implement Unwrap.
type Unwrapper interface {
	Unwrap() Provider
}

// maxUnwrapDepth bounds the walk so a provider that (incorrectly) returns
// itself, or a cycle built by a future decorator, cannot hang a request. It is
// the ONLY termination guard: a Provider's dynamic type need not be comparable
// (a struct holding a map or slice, passed by value, is a legitimate
// implementation), so identity bookkeeping is impossible here. Using such a
// value as a map key — or comparing two of them with == as interface values —
// panics at runtime with "hash of unhashable type".
const maxUnwrapDepth = 16

// ProviderChain returns p and each successively unwrapped inner provider,
// outermost first. It never returns nil entries and always includes p.
//
// The walk is depth-bounded rather than cycle-detected; see maxUnwrapDepth.
func ProviderChain(p Provider) []Provider {
	if p == nil {
		return nil
	}
	chain := make([]Provider, 0, 4)
	for cur := p; cur != nil && len(chain) < maxUnwrapDepth; {
		chain = append(chain, cur)

		u, ok := cur.(Unwrapper)
		if !ok {
			break
		}
		inner := u.Unwrap()
		if inner == nil {
			break
		}
		cur = inner
	}
	return chain
}

// FindInChain returns the first provider in p's decorator chain that satisfies
// T, searching outermost first. Use it instead of asserting on p directly, so a
// capability held by an inner provider is not hidden by its wrappers.
func FindInChain[T any](p Provider) (T, bool) {
	for _, candidate := range ProviderChain(p) {
		if match, ok := candidate.(T); ok {
			return match, true
		}
	}
	var zero T
	return zero, false
}

// AggregateModels resolves model discovery across a decorator chain and reports
// whether anything in it could answer at all.
//
// The bool is the part callers depend on: it separates "discovery ran and found
// nothing" from "no provider in this chain can enumerate models". Returning an
// empty list for both is what made the original bug invisible.
//
// Resolution order, most specific first:
//
//  1. *Router anywhere in the chain — its per-sub-provider breakdown is the
//     richest answer, so prefer it even when an outer decorator also offers
//     ListModelsAggregated.
//  2. any ModelAggregator that answers ok — a future decorator may aggregate
//     without being a router.
//  3. the innermost provider, when it is a ModelLister — a single-provider
//     deployment with no router.
//
// Step 3 deliberately consults ONLY the innermost provider. A decorator's own
// ListModels returns (nil, nil) when its inner cannot enumerate, which is
// indistinguishable from a leaf reporting an empty catalogue; asking the leaf
// directly removes the ambiguity in both directions. Treating nil as "did not
// answer" would instead misreport a genuinely empty catalogue as unsupported.
func AggregateModels(ctx context.Context, p Provider) (ListModelsResult, bool) {
	chain := ProviderChain(p)
	if len(chain) == 0 {
		// Nil / unwired provider: nothing answered, and callers must be able
		// to tell that apart from an empty catalogue.
		return ListModelsResult{}, false
	}

	for _, candidate := range chain {
		if router, ok := candidate.(*Router); ok {
			return router.ListModels(ctx), true
		}
	}

	for _, candidate := range chain {
		agg, ok := candidate.(ModelAggregator)
		if !ok {
			continue
		}
		if result, answered := agg.ListModelsAggregated(ctx); answered {
			return result, true
		}
	}

	if lister, ok := chain[len(chain)-1].(ModelLister); ok {
		models, err := lister.ListModels(ctx)
		if err != nil {
			return ListModelsResult{
				Providers: map[string][]ModelInfo{},
				Errors:    map[string]string{"chat": err.Error()},
			}, true
		}
		stamped := make([]ModelInfo, len(models))
		for j, m := range models {
			if m.Provider == "" {
				m.Provider = "chat"
			}
			stamped[j] = m
		}
		return ListModelsResult{Providers: map[string][]ModelInfo{"chat": stamped}}, true
	}

	return ListModelsResult{}, false
}

// PingChain runs the readiness probe of the innermost provider that has one.
// The bool reports whether any probe was found at all — a chain of decorators
// over a leaf that implements Pinger must still be probed, and "no probe
// available" has to stay distinguishable from "probe succeeded".
func PingChain(ctx context.Context, p Provider) (probed bool, err error) {
	chain := ProviderChain(p)
	for i := len(chain) - 1; i >= 0; i-- {
		if pinger, ok := chain[i].(Pinger); ok {
			return true, pinger.Ping(ctx)
		}
	}
	return false, nil
}
