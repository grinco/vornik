package chat

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file pins the model→route (kind) selection for the realistic
// production route table — the one defaultRouterRoutesForSubs builds in
// internal/service/container_chat.go and feeds into NewRouter. The
// router itself lives here in package chat, so we reconstruct an
// equivalent table directly (suffix-first ":free"/"openrouter/", then
// the vendor prefixes, then the full Bedrock publisher catalogue) and a
// plain ollama-style HTTP fallback, then assert providerFor picks the
// right sub-provider name for each model class.
//
// Existing router_test.go / router_suffix_test.go already cover
// WithModel pinning, empty-model handling, suffix-precedence in
// isolation, and fallback pass-through. The GAP filled here is the
// end-to-end fidelity of the *whole* configured table at once: that a
// realistic multi-vendor route list selects the documented kind for
// every model family the daemon actually sees, including the
// bedrock-catalog prefixes (minimax./zai./moonshotai./openai.) and the
// plain "qwen3.6:35b" ollama name landing on the http sub-provider
// fallback rather than being mis-routed to a vendor prefix.

// rrSubs is the set of distinct sub-providers used to build the
// regression route table. Each is an overridableNamedStub (reused from
// router_test.go) so providerFor reports a stable Name and WithModel
// clones rather than mutating.
type rrSubs struct {
	openrouter *overridableNamedStub
	vertex     *overridableNamedStub
	bedrock    *overridableNamedStub
	http       *overridableNamedStub // the fallback (plain ollama-style names)
}

// rrNewProductionRouter builds a Router whose route table mirrors the
// shipped default table for an install where openrouter + vertex +
// bedrock sub-providers are enabled and the fallback ("default") is the
// http sub-provider that forwards plain ollama-style names to a remote
// ollama /v1/chat/completions endpoint.
//
// Route order deliberately matches defaultRouterRoutesForSubs: suffix
// route first, then openrouter/ prefix, then vertex prefixes, then the
// Bedrock publisher catalogue. The fallback is http, labelled via
// WithRouterFallbackName so unrouted names pass through (general-purpose
// proxy) — that is exactly what makes "qwen3.6:35b" reach the http sub.
func rrNewProductionRouter(t *testing.T) (*Router, rrSubs) {
	t.Helper()
	subs := rrSubs{
		openrouter: &overridableNamedStub{namedStubProvider: namedStubProvider{name: "openrouter"}},
		vertex:     &overridableNamedStub{namedStubProvider: namedStubProvider{name: "vertex"}},
		bedrock:    &overridableNamedStub{namedStubProvider: namedStubProvider{name: "bedrock"}},
		http:       &overridableNamedStub{namedStubProvider: namedStubProvider{name: "http", lastModel: "http-default"}},
	}

	routes := []Route{
		// OpenRouter free-tier: suffix-first (precedence over any vendor prefix).
		{Suffix: FreeModelSuffix, Provider: subs.openrouter, Name: "openrouter"},
		{Prefix: "openrouter/", Provider: subs.openrouter, Name: "openrouter"},
		// Vertex prefixes.
		{Prefix: "gemini-", Provider: subs.vertex, Name: "vertex"},
		{Prefix: "google/", Provider: subs.vertex, Name: "vertex"},
		// Bedrock publisher catalogue (subset that matters for the
		// task: minimax./zai./moonshotai./openai. plus a couple of
		// neighbours to prove the prefix discrimination is exact).
		{Prefix: "amazon.", Provider: subs.bedrock, Name: "bedrock"},
		{Prefix: "anthropic.", Provider: subs.bedrock, Name: "bedrock"},
		{Prefix: "deepseek.", Provider: subs.bedrock, Name: "bedrock"},
		{Prefix: "meta.", Provider: subs.bedrock, Name: "bedrock"},
		{Prefix: "minimax.", Provider: subs.bedrock, Name: "bedrock"},
		{Prefix: "moonshotai.", Provider: subs.bedrock, Name: "bedrock"},
		{Prefix: "openai.", Provider: subs.bedrock, Name: "bedrock"},
		{Prefix: "qwen.", Provider: subs.bedrock, Name: "bedrock"},
		{Prefix: "zai.", Provider: subs.bedrock, Name: "bedrock"},
	}

	// Fallback is the http sub-provider, labelled so unrouted (plain
	// ollama-style) names pass through to it rather than being pinned to
	// the fallback's own default.
	r, err := NewRouter(subs.http, routes, WithRouterFallbackName("http"))
	require.NoError(t, err)
	return r, subs
}

// TestRouter_ProductionTable_SelectsExpectedKind is the table-driven core:
// for each representative model string, providerFor must report the
// expected sub-provider Name (kind) and the expected matched flag.
//
// The "qwen3.6:35b" case asserts matched==false (it hits no explicit
// route) yet resolves to the http fallback — the very behaviour that
// lets a plain ollama-style name forward to the remote ollama endpoint.
func TestRouter_ProductionTable_SelectsExpectedKind(t *testing.T) {
	r, subs := rrNewProductionRouter(t)

	cases := []struct {
		name        string
		model       string
		wantKind    string   // providerFor's returned name
		wantMatched bool     // whether an explicit route matched
		wantSub     Provider // identity of the selected sub-provider
	}{
		{
			name:        "free suffix → openrouter (deepseek vendor)",
			model:       "deepseek/deepseek-r1:free",
			wantKind:    "openrouter",
			wantMatched: true,
			wantSub:     subs.openrouter,
		},
		{
			name:        "free suffix → openrouter (qwen vendor, not qwen. bedrock)",
			model:       "qwen/qwen-2.5-coder-32b-instruct:free",
			wantKind:    "openrouter",
			wantMatched: true,
			wantSub:     subs.openrouter,
		},
		{
			name:        "openrouter/ prefix → openrouter",
			model:       "openrouter/auto",
			wantKind:    "openrouter",
			wantMatched: true,
			wantSub:     subs.openrouter,
		},
		{
			name:        "google/ prefix → vertex",
			model:       "google/gemini-2.5-pro",
			wantKind:    "vertex",
			wantMatched: true,
			wantSub:     subs.vertex,
		},
		{
			name:        "gemini- prefix → vertex",
			model:       "gemini-2.5-flash",
			wantKind:    "vertex",
			wantMatched: true,
			wantSub:     subs.vertex,
		},
		{
			name:        "minimax. prefix → bedrock",
			model:       "minimax.minimax-m2.5",
			wantKind:    "bedrock",
			wantMatched: true,
			wantSub:     subs.bedrock,
		},
		{
			name:        "zai. prefix → bedrock",
			model:       "zai.glm-4.7-flash",
			wantKind:    "bedrock",
			wantMatched: true,
			wantSub:     subs.bedrock,
		},
		{
			name:        "moonshotai. prefix → bedrock",
			model:       "moonshotai.kimi-k2.5",
			wantKind:    "bedrock",
			wantMatched: true,
			wantSub:     subs.bedrock,
		},
		{
			name:        "openai. prefix → bedrock",
			model:       "openai.gpt-oss-120b-1:0",
			wantKind:    "bedrock",
			wantMatched: true,
			wantSub:     subs.bedrock,
		},
		{
			name:        "plain ollama-style name → unmatched, http fallback",
			model:       "qwen3.6:35b",
			wantKind:    "fallback",
			wantMatched: false,
			wantSub:     subs.http,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			p, name, matched := r.providerFor(tc.model)
			assert.Equal(t, tc.wantMatched, matched,
				"matched flag for %q", tc.model)
			assert.Equal(t, tc.wantKind, name,
				"route kind for %q", tc.model)
			assert.Same(t, tc.wantSub, p,
				"selected sub-provider identity for %q", tc.model)
		})
	}
}

// TestRouter_ProductionTable_SuffixBeatsVendorPrefix pins the precedence
// invariant within the full table: a "google/...:free" ID must land on
// OpenRouter (suffix) even though a "google/" prefix route (→ vertex)
// also matches and appears in the same table. This is the realistic
// collision — every vendor publishes ":free" variants — distinct from
// the isolated two-route precedence test in router_suffix_test.go.
func TestRouter_ProductionTable_SuffixBeatsVendorPrefix(t *testing.T) {
	r, subs := rrNewProductionRouter(t)

	cases := []struct {
		model    string
		wantSub  Provider
		wantKind string
	}{
		{"google/gemini-2.0-flash-exp:free", subs.openrouter, "openrouter"}, // suffix wins over google/→vertex
		{"google/gemini-2.5-pro", subs.vertex, "vertex"},                    // paid → vertex prefix
		{"openrouter/auto:free", subs.openrouter, "openrouter"},             // suffix and openrouter/ agree
	}
	for _, tc := range cases {
		p, name, matched := r.providerFor(tc.model)
		assert.True(t, matched, "%q should match an explicit route", tc.model)
		assert.Equal(t, tc.wantKind, name, "kind for %q", tc.model)
		assert.Same(t, tc.wantSub, p, "sub for %q", tc.model)
	}
}

// TestRouter_ProductionTable_BedrockCatalogExhaustive walks every
// Bedrock publisher prefix the task names plus a few neighbours, proving
// each lands on the bedrock kind and that prefix discrimination is exact
// (e.g. "minimax." matches "minimax.x" but a bare "minimax" without the
// dot does NOT — it falls through to the http fallback). Guards against
// a regression that loosens HasPrefix matching or drops a catalogue
// entry, which would silently mis-route a publisher to the wrong proxy.
func TestRouter_ProductionTable_BedrockCatalogExhaustive(t *testing.T) {
	r, subs := rrNewProductionRouter(t)

	// Each of these must route to bedrock.
	bedrockModels := []string{
		"amazon.nova-pro-v1:0",
		"anthropic.claude-sonnet-4-5",
		"deepseek.r1-v1:0",
		"meta.llama3-70b",
		"minimax.minimax-m2.5",
		"moonshotai.kimi-k2.5",
		"openai.gpt-oss-120b-1:0",
		"qwen.qwen3-235b",
		"zai.glm-4.7-flash",
	}
	for _, m := range bedrockModels {
		p, name, matched := r.providerFor(m)
		assert.True(t, matched, "%q should match a bedrock route", m)
		assert.Equal(t, "bedrock", name, "kind for %q", m)
		assert.Same(t, subs.bedrock, p, "sub for %q", m)
	}

	// Publisher token WITHOUT the trailing dot must NOT match the
	// "<publisher>." prefix — it is not a Bedrock-shaped ID and should
	// fall through to the http fallback (unmatched).
	for _, m := range []string{"minimax", "zai", "openai", "qwen3.6:35b"} {
		p, name, matched := r.providerFor(m)
		assert.False(t, matched, "%q must not match a bedrock publisher prefix", m)
		assert.Equal(t, "fallback", name, "kind for unmatched %q", m)
		assert.Same(t, subs.http, p, "unmatched %q must resolve to the http fallback", m)
	}
}
