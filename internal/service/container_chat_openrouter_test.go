package service

import (
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/config"
)

// TestDefaultRouterRoutesIncludeOpenRouterWhenEnabled verifies the default
// table routes ":free" models (by suffix) and "openrouter/" IDs to the
// openrouter sub-provider, and that the suffix route precedes the vendor
// "google/" route so a google/...:free ID can't be claimed by vertex.
func TestDefaultRouterRoutesIncludeOpenRouterWhenEnabled(t *testing.T) {
	subs := map[string]chat.Provider{"openrouter": nil, "vertex": nil}
	routes := defaultRouterRoutesForSubs(subs)

	var freeIdx, googleIdx = -1, -1
	var sawOpenRouterPrefix bool
	for i, r := range routes {
		if r.Suffix == ":free" && r.Kind == "openrouter" {
			freeIdx = i
		}
		if r.Prefix == "openrouter/" && r.Kind == "openrouter" {
			sawOpenRouterPrefix = true
		}
		if r.Prefix == "google/" {
			googleIdx = i
		}
	}
	require.GreaterOrEqual(t, freeIdx, 0, "default routes must include a :free suffix → openrouter route")
	assert.True(t, sawOpenRouterPrefix, "default routes must include openrouter/ → openrouter")
	require.GreaterOrEqual(t, googleIdx, 0)
	assert.Less(t, freeIdx, googleIdx, ":free suffix route must precede the google/ prefix route")
}

// TestMergeWithDefaultRoutes_SuffixDedup verifies the merge keys on the
// (prefix, suffix) pair: an operator's :free route isn't duplicated by the
// default :free route, and a legacy empty-prefix catch-all coexists with a
// suffix-only route (both have Prefix=="").
func TestMergeWithDefaultRoutes_SuffixDedup(t *testing.T) {
	user := []config.ChatRouteConfig{
		{Suffix: ":free", Kind: "openrouter"},
		{Prefix: "", Kind: "bedrock"}, // legacy catch-all, also Prefix==""
	}
	defaults := []config.ChatRouteConfig{
		{Suffix: ":free", Kind: "openrouter"}, // duplicate — must be dropped
		{Prefix: "google/", Kind: "vertex"},   // gap-fill
	}
	merged := mergeWithDefaultRoutes(user, defaults)

	freeCount := 0
	for _, r := range merged {
		if r.Suffix == ":free" {
			freeCount++
		}
	}
	assert.Equal(t, 1, freeCount, "duplicate :free suffix route must be deduped")
	// Both user routes preserved + the one non-duplicate default.
	require.Len(t, merged, 3)
	assert.Equal(t, "google/", merged[2].Prefix)
}

// TestInitChatRouter_OpenRouterRequiresAPIKey verifies the enabled
// sub-provider fails fast without an api_key.
func TestInitChatRouter_OpenRouterRequiresAPIKey(t *testing.T) {
	c := &Container{Logger: zerolog.Nop(), Config: &config.Config{}}
	c.Config.Chat = config.ChatConfig{
		Enabled:  true,
		Provider: "router",
		Router: config.ChatRouterConfig{
			Default:    "openrouter",
			OpenRouter: config.ChatOpenRouterSubConfig{Enabled: true}, // no api_key
		},
	}
	err := c.initChatRouter(c.Config.Chat)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "api_key")
}

// TestInitChatRouter_OpenRouterBuildsClient verifies a fully-configured
// openrouter sub-provider builds without error and becomes the routable
// chat client (no network call — construction only).
func TestInitChatRouter_OpenRouterBuildsClient(t *testing.T) {
	c := &Container{Logger: zerolog.Nop(), Config: &config.Config{}}
	c.Config.Chat = config.ChatConfig{
		Enabled:  true,
		Provider: "router",
		Model:    "deepseek/deepseek-r1:free",
		Router: config.ChatRouterConfig{
			Default: "openrouter",
			OpenRouter: config.ChatOpenRouterSubConfig{
				Enabled:  true,
				APIKey:   "sk-or-test",
				Model:    "deepseek/deepseek-r1:free",
				FreeOnly: true,
				Title:    "vornik",
			},
		},
	}
	require.NoError(t, c.initChatRouter(c.Config.Chat))
	require.NotNil(t, c.ChatClient)
	assert.Equal(t, "deepseek/deepseek-r1:free", c.ChatClient.Model())
}

// TestResolveOpenRouterEndpoint verifies the baked-in default and the
// override path.
func TestResolveOpenRouterEndpoint(t *testing.T) {
	assert.Equal(t, "https://openrouter.ai/api/v1", resolveOpenRouterEndpoint(""))
	assert.Equal(t, "https://proxy.internal/v1", resolveOpenRouterEndpoint("https://proxy.internal/v1"))
}

// TestOpenRouterAttributionHeaders verifies the default identity (vornik
// by name+version, no website URL) and that operator overrides win.
func TestOpenRouterAttributionHeaders(t *testing.T) {
	def := openRouterAttributionHeaders("", "")
	assert.Equal(t, "vornik", def["X-Title"])
	assert.True(t, strings.HasPrefix(def["HTTP-Referer"], "vornik/"),
		"default referer identifies vornik by version, not a URL; got %q", def["HTTP-Referer"])
	assert.NotContains(t, def["HTTP-Referer"], "http", "default referer must not be a website URL")

	custom := openRouterAttributionHeaders("https://my.app", "MyApp")
	assert.Equal(t, "https://my.app", custom["HTTP-Referer"])
	assert.Equal(t, "MyApp", custom["X-Title"])
}
