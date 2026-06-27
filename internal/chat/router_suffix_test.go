package chat

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dispatchModel routes a model through the router and returns the name of
// the sub-provider that served it. namedStubProvider stamps its name on
// the response ID, and overridableNamedStub.WithModel clones (carrying the
// name), so the response ID identifies the dispatch target even though
// WithModel returns a clone rather than mutating the original stub.
func dispatchModel(t *testing.T, r *Router, model string) string {
	t.Helper()
	resp, err := r.WithModel(model).CompleteWithTools(context.Background(), nil, nil)
	require.NoError(t, err)
	return resp.ID
}

// TestRouter_SuffixRoute_Matches verifies a route with a Suffix dispatches
// when the model ends with it, even though no Prefix matches.
func TestRouter_SuffixRoute_Matches(t *testing.T) {
	openrouter := &overridableNamedStub{namedStubProvider: namedStubProvider{name: "openrouter"}}
	vertex := &overridableNamedStub{namedStubProvider: namedStubProvider{name: "vertex"}}
	fallback := &overridableNamedStub{namedStubProvider: namedStubProvider{name: "bedrock"}}

	r, err := NewRouter(fallback, []Route{
		{Suffix: ":free", Provider: openrouter, Name: "openrouter"},
		{Prefix: "google/", Provider: vertex, Name: "vertex"},
	}, WithRouterFallbackName("bedrock"))
	require.NoError(t, err)

	assert.Equal(t, "openrouter", dispatchModel(t, r, "deepseek/deepseek-r1:free"))
}

// TestRouter_SuffixPrecedesPrefix is the collision regression: the suffix
// route must win over an overlapping prefix route regardless of list
// order, because mergeWithDefaultRoutes appends defaults AFTER operator
// routes. A google/...:free ID goes to OpenRouter; a paid google/... ID
// goes to Vertex.
func TestRouter_SuffixPrecedesPrefix(t *testing.T) {
	openrouter := &overridableNamedStub{namedStubProvider: namedStubProvider{name: "openrouter"}}
	vertex := &overridableNamedStub{namedStubProvider: namedStubProvider{name: "vertex"}}
	fallback := &overridableNamedStub{namedStubProvider: namedStubProvider{name: "bedrock"}}

	// Deliberately list the prefix route FIRST to prove suffix precedence
	// is not order-dependent.
	r, err := NewRouter(fallback, []Route{
		{Prefix: "google/", Provider: vertex, Name: "vertex"},
		{Suffix: ":free", Provider: openrouter, Name: "openrouter"},
	}, WithRouterFallbackName("bedrock"))
	require.NoError(t, err)

	assert.Equal(t, "openrouter", dispatchModel(t, r, "google/gemini-2.0-flash-exp:free"),
		"google/...:free must route to openrouter via suffix precedence")
	assert.Equal(t, "vertex", dispatchModel(t, r, "google/gemini-2.5-pro"),
		"paid google/ ID must still route to vertex via prefix")
}

// TestRouter_EmptySuffix_BackCompat verifies a legacy empty-prefix
// catch-all still matches everything, while a suffix-only route doesn't
// swallow non-matching models.
func TestRouter_EmptySuffix_BackCompat(t *testing.T) {
	catchAll := &overridableNamedStub{namedStubProvider: namedStubProvider{name: "catchall"}}
	suffixSub := &overridableNamedStub{namedStubProvider: namedStubProvider{name: "suffix"}}
	fallback := &overridableNamedStub{namedStubProvider: namedStubProvider{name: "fb"}}

	r, err := NewRouter(fallback, []Route{
		{Suffix: ":free", Provider: suffixSub, Name: "suffix"},
		{Prefix: "", Provider: catchAll, Name: "catchall"}, // legacy catch-all
	}, WithRouterFallbackName("http"))
	require.NoError(t, err)

	// Non-free model: skips the suffix route, lands on the catch-all.
	assert.Equal(t, "catchall", dispatchModel(t, r, "openai/gpt-4o"))
	// Free model: suffix route wins.
	assert.Equal(t, "suffix", dispatchModel(t, r, "x:free"))
}

// TestRouter_ProviderForEmptyModel verifies the empty-model guard returns
// the fallback (unmatched) — callers that don't override model land on the
// default sub-provider rather than tripping the suffix/prefix passes.
func TestRouter_ProviderForEmptyModel(t *testing.T) {
	fallback := &overridableNamedStub{namedStubProvider: namedStubProvider{name: "fb", lastModel: "fb-default"}}
	sub := &overridableNamedStub{namedStubProvider: namedStubProvider{name: "sub"}}
	r, err := NewRouter(fallback, []Route{
		{Suffix: ":free", Provider: sub, Name: "openrouter"},
		{Prefix: "x", Provider: sub, Name: "x"},
	}, WithRouterFallbackName("http"))
	require.NoError(t, err)

	p, name, matched := r.providerFor("")
	assert.False(t, matched, "empty model must not match any route")
	assert.Equal(t, "fallback", name)
	assert.Equal(t, fallback, p)
}

// TestRouter_FallbackPassesModelThrough_OpenRouterKind verifies OpenRouter
// joins the general-purpose-proxy set: an unrouted model on an openrouter
// fallback is forwarded verbatim (not pinned to the fallback's default).
func TestRouter_FallbackPassesModelThrough_OpenRouterKind(t *testing.T) {
	fallback := &overridableNamedStub{namedStubProvider: namedStubProvider{name: "openrouter", lastModel: "deepseek/deepseek-r1:free"}}
	r, err := NewRouter(fallback, nil, WithRouterFallbackName("openrouter"))
	require.NoError(t, err)

	sub := r.WithModel("meta-llama/llama-3.3-70b-instruct:free")
	assert.Equal(t, "meta-llama/llama-3.3-70b-instruct:free", sub.Model(),
		"openrouter fallback must honour the request's model when no route matches")
}
