package chat

import (
	"context"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// namedStubProvider is a Provider that records which requests hit it
// so router tests can assert dispatch decisions without spinning up
// real subprocesses.
type namedStubProvider struct {
	name      string
	hits      int
	lastModel string
}

func (s *namedStubProvider) Complete(_ context.Context, _ []Message) (*ChatResponse, error) {
	s.hits++
	return &ChatResponse{ID: s.name, Model: s.name}, nil
}

func (s *namedStubProvider) CompleteWithTools(_ context.Context, _ []Message, _ []Tool) (*ChatResponse, error) {
	s.hits++
	return &ChatResponse{ID: s.name, Model: s.name}, nil
}

func (s *namedStubProvider) CompleteWithToolsStream(_ context.Context, _ []Message, _ []Tool, _ StreamCallback) (*ChatResponse, error) {
	s.hits++
	return &ChatResponse{ID: s.name, Model: s.name}, nil
}

func (s *namedStubProvider) Model() string         { return s.lastModel }
func (s *namedStubProvider) SetMetrics(_ *Metrics) {}

// overridableNamedStub adds ModelOverridable. WithModel returns a
// clone so each per-request override is a fresh instance (mirroring
// CLIClient/CodexCLIClient semantics).
type overridableNamedStub struct{ namedStubProvider }

func (o *overridableNamedStub) WithModel(m string) Provider {
	clone := *o
	clone.lastModel = m
	return &clone
}

func TestRouter_FallbackWhenNoPrefixMatches(t *testing.T) {
	fallback := &namedStubProvider{name: "fallback"}
	r, err := NewRouter(fallback, nil)
	require.NoError(t, err)

	// Default flow — fallback handles everything.
	_, err = r.CompleteWithTools(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, fallback.hits)
}

func TestRouter_DispatchesByPrefix(t *testing.T) {
	claude := &overridableNamedStub{namedStubProvider: namedStubProvider{name: "claude"}}
	codex := &overridableNamedStub{namedStubProvider: namedStubProvider{name: "codex"}}
	fallback := &overridableNamedStub{namedStubProvider: namedStubProvider{name: "http"}}

	r, err := NewRouter(fallback, []Route{
		{Prefix: "claude-", Provider: claude, Name: "claude-cli"},
		{Prefix: "gpt-", Provider: codex, Name: "codex-subscription"},
		{Prefix: "o3-", Provider: codex, Name: "codex-subscription"},
	})
	require.NoError(t, err)

	// claude-sonnet-4-6 → claude
	sub := r.WithModel("claude-sonnet-4-6")
	assert.Equal(t, "claude-sonnet-4-6", sub.Model(), "WithModel should pin the sub-provider to the requested model")
	// WithModel returns a CLONE — the original claude stub's model
	// field is untouched (stays at its zero-value "").
	assert.Equal(t, "", claude.lastModel, "WithModel must not mutate the original sub-provider")

	// gpt-5.3-codex → codex
	sub = r.WithModel("gpt-5.3-codex")
	assert.Equal(t, "gpt-5.3-codex", sub.Model())

	// o3-mini → codex (o3- prefix)
	sub = r.WithModel("o3-mini")
	assert.Equal(t, "o3-mini", sub.Model())

	// Unknown prefix → fallback AT ITS CONFIGURED DEFAULT (no override).
	// Passing an unrecognised name through to e.g. Claude's --model
	// flag makes the CLI exit 1 with "model not supported"; skipping
	// the override lets the fallback respond with its own default
	// model (what the operator picked when configuring the router).
	fallback.lastModel = "fallback-default"
	sub = r.WithModel("mystery-model-99")
	assert.Equal(t, "fallback-default", sub.Model(),
		"unknown model must NOT be forwarded to the fallback — use its own default instead")
}

// TestRouter_FallbackPassesModelThrough_BedrockKind — the
// regression guard for the silent-substitution incident on
// 2026-05-07: with default:bedrock and an un-routed
// moonshotai.kimi-k2.5 request, the router used to return the
// fallback un-pinned and bedrock served with its own default
// (zai.glm-4.7-flash). Now: when the fallback kind is a general-
// purpose proxy (bedrock / http / vertex), un-routed model
// names ARE forwarded to the fallback so the caller's choice is
// honoured.
func TestRouter_FallbackPassesModelThrough_BedrockKind(t *testing.T) {
	fallback := &overridableNamedStub{namedStubProvider: namedStubProvider{name: "bedrock", lastModel: "zai.glm-4.7-flash"}}
	r, err := NewRouter(fallback, []Route{
		{Prefix: "claude-", Provider: &overridableNamedStub{namedStubProvider: namedStubProvider{name: "claude"}}, Name: "claude-subscription"},
	}, WithRouterFallbackName("bedrock"))
	require.NoError(t, err)

	// Un-routed model on a general-purpose fallback — pass through.
	sub := r.WithModel("moonshotai.kimi-k2.5")
	assert.Equal(t, "moonshotai.kimi-k2.5", sub.Model(),
		"general-purpose fallback (bedrock/http/vertex) must honour the request's model when no route matches")
}

// TestRouter_FallbackPassesModelThrough_CLIKind — flip side: when
// the fallback is a specialised CLI (claude-cli / codex-cli), un-
// routed model names must NOT pass through. The CLI exits 1 on
// unrecognised IDs; better to use its configured default.
func TestRouter_FallbackPassesModelThrough_CLIKind(t *testing.T) {
	fallback := &overridableNamedStub{namedStubProvider: namedStubProvider{name: "claude-cli", lastModel: "claude-sonnet-4-6"}}
	r, err := NewRouter(fallback, nil, WithRouterFallbackName("claude-cli"))
	require.NoError(t, err)

	// Un-routed model on a CLI fallback — keep its default.
	sub := r.WithModel("mystery-model-99")
	assert.Equal(t, "claude-sonnet-4-6", sub.Model(),
		"specialised CLI fallback must NOT receive arbitrary un-routed model names")
}

// TestRouter_FallbackPassesModelThrough_UnnamedDefault — when the
// router was constructed without WithRouterFallbackName (older
// test paths, pre-2026-05-07 callers), behaviour falls back to
// the historical CLI-style behaviour (don't pass through). This
// keeps the legacy contract intact.
func TestRouter_FallbackPassesModelThrough_UnnamedDefault(t *testing.T) {
	fallback := &overridableNamedStub{namedStubProvider: namedStubProvider{name: "fallback", lastModel: "default-model"}}
	r, err := NewRouter(fallback, nil) // no WithRouterFallbackName
	require.NoError(t, err)

	sub := r.WithModel("mystery-model-99")
	assert.Equal(t, "default-model", sub.Model(),
		"unlabelled fallback must keep historical behaviour (no pass-through)")
}

func TestRouter_FallbackWhenSubNotOverridable(t *testing.T) {
	// When the matching sub-provider doesn't implement
	// ModelOverridable, WithModel should return it verbatim (Model()
	// reflects the sub-provider's own configured value, not the
	// requested one).
	plainClaude := &namedStubProvider{name: "plain-claude", lastModel: "claude-fixed"}
	fallback := &overridableNamedStub{namedStubProvider: namedStubProvider{name: "fallback"}}

	r, err := NewRouter(fallback, []Route{
		{Prefix: "claude-", Provider: plainClaude, Name: "plain-claude"},
	})
	require.NoError(t, err)

	sub := r.WithModel("claude-anything")
	assert.Equal(t, "claude-fixed", sub.Model(),
		"non-overridable sub-provider keeps its own model regardless of request")
}

func TestRouter_NilFallbackRejected(t *testing.T) {
	_, err := NewRouter(nil, nil)
	assert.Error(t, err)
}

func TestRouter_NilRouteProviderRejected(t *testing.T) {
	_, err := NewRouter(&namedStubProvider{}, []Route{{Prefix: "x-", Provider: nil}})
	assert.Error(t, err, "constructor must reject routes with nil providers so we fail at startup, not mid-request")
}

func TestRouter_SetMetricsFansOutUnique(t *testing.T) {
	// Same provider attached under two prefixes — SetMetrics should
	// call it once, not twice (no cost, just a cleanliness check).
	shared := &namedStubProvider{name: "shared"}
	fallback := &namedStubProvider{name: "fallback"}
	r, err := NewRouter(fallback, []Route{
		{Prefix: "a-", Provider: shared},
		{Prefix: "b-", Provider: shared},
	})
	require.NoError(t, err)

	// Nothing to assert on the namedStubProvider since SetMetrics is
	// a no-op; this test exists to exercise the fan-out code path for
	// coverage, not to check a specific side effect. The compile-time
	// guarantee is that SetMetrics doesn't panic on duplicate routes.
	r.SetMetrics(nil)
	_ = zerolog.Nop()
}

func TestRouter_DefaultFallbackAccessorsForwardToFallback(t *testing.T) {
	// Model() and the un-overridden Complete* methods should forward
	// to the fallback unchanged.
	fallback := &namedStubProvider{name: "fallback", lastModel: "fallback-default"}
	r, err := NewRouter(fallback, nil)
	require.NoError(t, err)

	assert.Equal(t, "fallback-default", r.Model())
	resp, err := r.Complete(context.Background(), []Message{{Role: "user", Content: "x"}})
	require.NoError(t, err)
	assert.Equal(t, "fallback", resp.ID)
}

// listingStub adds ModelLister to namedStubProvider so the router's
// aggregator can include it in ListModels output.
type listingStub struct {
	namedStubProvider
	models []ModelInfo
	err    error
}

func (l *listingStub) ListModels(_ context.Context) ([]ModelInfo, error) {
	if l.err != nil {
		return nil, l.err
	}
	return l.models, nil
}

func TestRouter_ListModels_AggregatesFromSubs(t *testing.T) {
	a := &listingStub{
		namedStubProvider: namedStubProvider{name: "a"},
		models:            []ModelInfo{{ID: "a-1", Source: "live"}, {ID: "a-2", Source: "live"}},
	}
	b := &listingStub{
		namedStubProvider: namedStubProvider{name: "b"},
		models:            []ModelInfo{{ID: "b-1", Source: "static", OwnedBy: "vendor"}},
	}
	r, err := NewRouter(a,
		[]Route{{Prefix: "b-", Provider: b, Name: "bee"}},
		WithRouterSubs(map[string]Provider{"alpha": a, "bee": b}),
	)
	require.NoError(t, err)

	got := r.ListModels(context.Background())
	assert.Empty(t, got.Errors)
	assert.Len(t, got.Providers, 2)
	assert.Len(t, got.Providers["alpha"], 2)
	assert.Len(t, got.Providers["bee"], 1)
	// Provider field stamped by the router.
	assert.Equal(t, "alpha", got.Providers["alpha"][0].Provider)
	assert.Equal(t, "bee", got.Providers["bee"][0].Provider)
	assert.Equal(t, "vendor", got.Providers["bee"][0].OwnedBy)
}

func TestRouter_ListModels_RecordsPerSubError(t *testing.T) {
	good := &listingStub{
		namedStubProvider: namedStubProvider{name: "good"},
		models:            []ModelInfo{{ID: "g-1", Source: "live"}},
	}
	bad := &listingStub{
		namedStubProvider: namedStubProvider{name: "bad"},
		err:               assertError("boom"),
	}
	r, err := NewRouter(good, nil,
		WithRouterSubs(map[string]Provider{"good": good, "bad": bad}),
	)
	require.NoError(t, err)

	got := r.ListModels(context.Background())
	// Healthy provider returns its list…
	assert.Len(t, got.Providers["good"], 1)
	// …and the failing one shows up under Errors instead of dropping silently.
	assert.Contains(t, got.Errors, "bad")
	assert.Contains(t, got.Errors["bad"], "boom")
}

func TestRouter_ListModels_FallsBackToRoutesWhenSubsUnset(t *testing.T) {
	// When operator constructs a Router without WithRouterSubs, the
	// aggregator derives the named set from the route table + fallback.
	// Sub-providers that AREN'T routed are missed in this path —
	// documented limitation, asserted here so the behaviour can't
	// drift unnoticed.
	a := &listingStub{
		namedStubProvider: namedStubProvider{name: "a"},
		models:            []ModelInfo{{ID: "a-1", Source: "live"}},
	}
	b := &listingStub{
		namedStubProvider: namedStubProvider{name: "b"},
		models:            []ModelInfo{{ID: "b-1", Source: "static"}},
	}
	r, err := NewRouter(a, []Route{{Prefix: "b-", Provider: b, Name: "bee"}})
	require.NoError(t, err)

	got := r.ListModels(context.Background())
	// Route's named provider visible…
	assert.Len(t, got.Providers["bee"], 1)
	// …and the fallback shows up under "fallback" since it's not routed.
	assert.Len(t, got.Providers["fallback"], 1)
}

// listingStub doesn't satisfy ModelLister via embedding because the
// namedStubProvider has its own zero-value ListModels — so an
// assertError helper is the simplest path to a stable error value
// without dragging errors.New into every line.
func assertError(msg string) error { return testErr(msg) }

type testErr string

func (e testErr) Error() string { return string(e) }

// metricsCountingStub counts SetMetrics calls so the dedup test can
// assert "called once per distinct provider, not once per route".
type metricsCountingStub struct {
	namedStubProvider
	setMetricsCalls int
}

func (s *metricsCountingStub) SetMetrics(_ *Metrics) { s.setMetricsCalls++ }

// TestRouter_SetMetrics_DedupesAcrossRoutesAndFallback pins the
// behaviour of forEachSubProvider: even when the same provider is
// the fallback AND appears under multiple prefixes, SetMetrics
// reaches it exactly once. Regression guard for the 2026-05-16
// router dedup refactor.
func TestRouter_SetMetrics_DedupesAcrossRoutesAndFallback(t *testing.T) {
	shared := &metricsCountingStub{namedStubProvider: namedStubProvider{name: "shared"}}
	fallback := &metricsCountingStub{namedStubProvider: namedStubProvider{name: "fallback"}}
	r, err := NewRouter(fallback, []Route{
		{Prefix: "a-", Provider: shared},
		{Prefix: "b-", Provider: shared},   // duplicate of "a-"
		{Prefix: "c-", Provider: fallback}, // duplicate of the fallback itself
	})
	require.NoError(t, err)
	r.SetMetrics(nil)
	assert.Equal(t, 1, shared.setMetricsCalls, "shared provider received SetMetrics more than once")
	assert.Equal(t, 1, fallback.setMetricsCalls, "fallback received SetMetrics more than once")
}

// TestRouter_ResolveSubs_PrefersExplicitSubs verifies the
// WithRouterSubs override wins over the route-derived best-effort
// path even when route names and the explicit map disagree.
func TestRouter_ResolveSubs_PrefersExplicitSubs(t *testing.T) {
	fallback := &namedStubProvider{name: "fallback"}
	a := &namedStubProvider{name: "a"}
	explicit := &namedStubProvider{name: "explicit-extra"}
	r, err := NewRouter(fallback,
		[]Route{{Prefix: "a-", Provider: a, Name: "a"}},
		WithRouterSubs(map[string]Provider{"explicit": explicit}),
	)
	require.NoError(t, err)
	got := r.resolveSubs()
	_, hasExplicit := got["explicit"]
	_, hasRouteName := got["a"]
	assert.True(t, hasExplicit, "explicit sub missing from resolved map")
	assert.False(t, hasRouteName, "route-derived name leaked into explicitly-set subs map")
}
