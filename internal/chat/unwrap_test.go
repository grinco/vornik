package chat

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// listerSub is a leaf provider that can enumerate models and answer a ping.
type listerSub struct {
	models  []ModelInfo
	listErr error
	pinged  bool
	pingErr error
}

func (s *listerSub) Complete(context.Context, []Message) (*ChatResponse, error) {
	return &ChatResponse{}, nil
}
func (s *listerSub) CompleteWithTools(context.Context, []Message, []Tool) (*ChatResponse, error) {
	return &ChatResponse{}, nil
}
func (s *listerSub) CompleteWithToolsStream(context.Context, []Message, []Tool, StreamCallback) (*ChatResponse, error) {
	return &ChatResponse{}, nil
}
func (s *listerSub) Model() string             { return "leaf" }
func (s *listerSub) SetMetrics(*Metrics)       {}
func (s *listerSub) WithModel(string) Provider { return s }
func (s *listerSub) ListModels(context.Context) ([]ModelInfo, error) {
	return s.models, s.listErr
}
func (s *listerSub) Ping(context.Context) error {
	s.pinged = true
	return s.pingErr
}

// plainSub implements nothing optional — the "provider that genuinely cannot
// enumerate" case, which must stay distinguishable from a broken chain.
type plainSub struct{}

func (plainSub) Complete(context.Context, []Message) (*ChatResponse, error) {
	return &ChatResponse{}, nil
}
func (plainSub) CompleteWithTools(context.Context, []Message, []Tool) (*ChatResponse, error) {
	return &ChatResponse{}, nil
}
func (plainSub) CompleteWithToolsStream(context.Context, []Message, []Tool, StreamCallback) (*ChatResponse, error) {
	return &ChatResponse{}, nil
}
func (plainSub) Model() string             { return "plain" }
func (plainSub) SetMetrics(*Metrics)       {}
func (plainSub) WithModel(string) Provider { return plainSub{} }

func newTestRouter(t *testing.T, sub Provider) *Router {
	t.Helper()
	r, err := NewRouter(sub, []Route{{Prefix: "leaf", Provider: sub, Name: "leaf"}},
		WithRouterSubs(map[string]Provider{"leaf": sub}))
	require.NoError(t, err)
	return r
}

// This is the production chain that broke `vornikctl models list`: the daemon
// wraps the router in FallbackProvider whenever chat.router.model_fallbacks is
// configured, then in LoggingProvider. FallbackProvider forwarded neither
// discovery method, and LoggingProvider's hand-written shim could only see
// through a QueuedProvider or a bare *Router — so the whole chain reported zero
// models with no error, and the empty list was indistinguishable from "the
// provider cannot enumerate". Regressed 2026-07-18 (d5180cdd).
func TestAggregateModelsSeesRouterThroughFallbackAndLogging(t *testing.T) {
	sub := &listerSub{models: []ModelInfo{{ID: "leaf-1"}, {ID: "leaf-2"}}}
	router := newTestRouter(t, sub)
	chain := NewLoggingProvider(
		NewFallbackProvider(router, map[string]string{"a": "b"}, zerolog.Nop()),
		zerolog.Nop())

	result, ok := AggregateModels(context.Background(), chain)
	require.True(t, ok, "the router must remain reachable through the decorator chain")
	require.Len(t, result.Providers["leaf"], 2)
	require.Empty(t, result.Errors)
}

func TestAggregateModelsFindsBareRouter(t *testing.T) {
	sub := &listerSub{models: []ModelInfo{{ID: "leaf-1"}}}
	result, ok := AggregateModels(context.Background(), newTestRouter(t, sub))
	require.True(t, ok)
	require.Len(t, result.Providers["leaf"], 1)
}

// Single-provider deployments have no router at all; the flat lister must still
// be found through a decorator, and stamped with the conventional name.
func TestAggregateModelsFindsFlatListerThroughDecorator(t *testing.T) {
	sub := &listerSub{models: []ModelInfo{{ID: "solo"}}}
	chain := NewLoggingProvider(sub, zerolog.Nop())

	result, ok := AggregateModels(context.Background(), chain)
	require.True(t, ok)
	require.Len(t, result.Providers["chat"], 1)
	require.Equal(t, "chat", result.Providers["chat"][0].Provider)
}

// A lister that fails must report an error, not an empty list — the caller has
// to be able to tell "nothing installed" from "discovery is broken".
func TestAggregateModelsSurfacesListerError(t *testing.T) {
	sub := &listerSub{listErr: errors.New("upstream 403")}
	result, ok := AggregateModels(context.Background(), NewLoggingProvider(sub, zerolog.Nop()))
	require.True(t, ok, "the provider answered — it answered with a failure")
	require.Contains(t, result.Errors["chat"], "upstream 403")
	require.Empty(t, result.Providers["chat"])
}

func TestAggregateModelsReportsUnsupportedWhenNothingCanEnumerate(t *testing.T) {
	_, ok := AggregateModels(context.Background(),
		NewLoggingProvider(NewFallbackProvider(plainSub{}, map[string]string{"a": "b"}, zerolog.Nop()),
			zerolog.Nop()))
	require.False(t, ok, "an empty result must stay distinguishable from an unsupported chain")
}

// unhashableSub has VALUE receivers and holds a map, so the dynamic type stored
// in the Provider interface is not comparable. Using such a value as a map key —
// or comparing two of them with == as interface values — panics at runtime with
// "hash of unhashable type". Walking the chain must do neither: callers pass
// arbitrary implementations, and the real stub that caught this
// (api.modelListingStub) was exactly this shape. A pointer would have been safe.
type unhashableSub struct {
	tags   map[string]string
	models []ModelInfo
}

func (unhashableSub) Complete(context.Context, []Message) (*ChatResponse, error) {
	return &ChatResponse{}, nil
}
func (unhashableSub) CompleteWithTools(context.Context, []Message, []Tool) (*ChatResponse, error) {
	return &ChatResponse{}, nil
}
func (unhashableSub) CompleteWithToolsStream(context.Context, []Message, []Tool, StreamCallback) (*ChatResponse, error) {
	return &ChatResponse{}, nil
}
func (unhashableSub) Model() string               { return "unhashable" }
func (unhashableSub) SetMetrics(*Metrics)         {}
func (s unhashableSub) WithModel(string) Provider { return s }
func (s unhashableSub) ListModels(context.Context) ([]ModelInfo, error) {
	return s.models, nil
}

func TestProviderChainToleratesUnhashableProviders(t *testing.T) {
	leaf := unhashableSub{
		tags:   map[string]string{"k": "v"},
		models: []ModelInfo{{ID: "solo"}},
	}

	require.NotPanics(t, func() {
		chain := ProviderChain(NewLoggingProvider(leaf, zerolog.Nop()))
		require.Len(t, chain, 2)
	})

	require.NotPanics(t, func() {
		result, ok := AggregateModels(context.Background(), NewLoggingProvider(leaf, zerolog.Nop()))
		require.True(t, ok)
		require.Len(t, result.Providers["chat"], 1)
	})
}

// A decorator whose Unwrap returns itself must not hang the walk.
type selfWrapper struct{ listerSub }

func (s *selfWrapper) Unwrap() Provider { return s }

func TestProviderChainTerminatesOnSelfReference(t *testing.T) {
	done := make(chan []Provider, 1)
	go func() { done <- ProviderChain(&selfWrapper{}) }()
	select {
	case chain := <-done:
		require.LessOrEqual(t, len(chain), maxUnwrapDepth)
	case <-time.After(2 * time.Second):
		t.Fatal("ProviderChain did not terminate on a self-referential decorator")
	}
}

// Every decorator must expose its inner provider so a capability lookup does not
// need a bespoke shim per wrapper — the omission that caused this bug.
func TestEveryDecoratorUnwraps(t *testing.T) {
	leaf := &listerSub{}
	reg := &sync.Map{}
	for name, decorated := range map[string]Provider{
		"logging":  NewLoggingProvider(leaf, zerolog.Nop()),
		"fallback": NewFallbackProvider(leaf, map[string]string{"a": "b"}, zerolog.Nop()),
		"queued":   NewQueuedProvider(leaf, 1),
		"bounded":  NewBoundedRouteProvider(leaf, "leaf", 1, 0, nil),
		"gated":    NewHealthGatedProvider(leaf, "leaf", reg, DefaultHealthConfig()),
	} {
		u, ok := decorated.(Unwrapper)
		require.True(t, ok, "%s must implement Unwrapper", name)
		require.Equal(t, Provider(leaf), u.Unwrap(), "%s must return its inner provider", name)
	}
}

// Router.ListModels skipped any sub that did not itself implement ModelLister.
// Both per-route wrappers drop the method, so a route's provider was invisible.
func TestRouterListModelsSeesThroughPerRouteWrappers(t *testing.T) {
	sub := &listerSub{models: []ModelInfo{{ID: "wrapped-1"}}}
	gated := NewHealthGatedProvider(sub, "leaf", &sync.Map{}, DefaultHealthConfig())
	bounded := NewBoundedRouteProvider(gated, "leaf", 1, 0, nil)

	r, err := NewRouter(bounded, []Route{{Prefix: "leaf", Provider: bounded, Name: "leaf"}})
	require.NoError(t, err)

	result := r.ListModels(context.Background())
	require.Len(t, result.Providers["leaf"], 1, "a wrapped sub-provider must still be discoverable")
}

// Same defect class: Router.Ping treats a non-Pinger sub as "ready by
// construction", and the per-route wrappers drop Ping — so readiness probes
// silently verified nothing.
func TestRouterPingReachesThroughPerRouteWrappers(t *testing.T) {
	sub := &listerSub{}
	bounded := NewBoundedRouteProvider(
		NewHealthGatedProvider(sub, "leaf", &sync.Map{}, DefaultHealthConfig()),
		"leaf", 1, 0, nil)

	r, err := NewRouter(bounded, []Route{{Prefix: "leaf", Provider: bounded, Name: "leaf"}})
	require.NoError(t, err)
	require.NoError(t, r.Ping(context.Background()))
	require.True(t, sub.pinged, "the readiness probe must actually reach the leaf provider")
}

// An unwired provider must report "nothing answered" rather than panicking on
// an empty chain (regression: AggregateModels indexed chain[len-1] unguarded).
func TestAggregateModelsHandlesNilProvider(t *testing.T) {
	require.NotPanics(t, func() {
		_, ok := AggregateModels(context.Background(), nil)
		require.False(t, ok)
	})
	require.Nil(t, ProviderChain(nil))

	probed, err := PingChain(context.Background(), nil)
	require.False(t, probed)
	require.NoError(t, err)
}
