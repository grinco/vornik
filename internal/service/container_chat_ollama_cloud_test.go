package service

import (
	"context"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/config"
)

// identifiableStubProvider is a minimal chat.Provider + ModelOverridable
// stub carrying an identity tag that survives WithModel cloning, so a
// test can prove WHICH sub-provider a router dispatch landed on without
// depending on chat.Client's unexported fields.
type identifiableStubProvider struct {
	id    string
	model string
}

func (s *identifiableStubProvider) Complete(_ context.Context, _ []chat.Message) (*chat.ChatResponse, error) {
	return nil, nil
}
func (s *identifiableStubProvider) CompleteWithTools(_ context.Context, _ []chat.Message, _ []chat.Tool) (*chat.ChatResponse, error) {
	return nil, nil
}
func (s *identifiableStubProvider) CompleteWithToolsStream(_ context.Context, _ []chat.Message, _ []chat.Tool, _ chat.StreamCallback) (*chat.ChatResponse, error) {
	return nil, nil
}
func (s *identifiableStubProvider) Model() string              { return s.model }
func (s *identifiableStubProvider) SetMetrics(_ *chat.Metrics) {}
func (s *identifiableStubProvider) WithModel(m string) chat.Provider {
	clone := *s
	clone.model = m
	return &clone
}

// TestDefaultRouterRoutesForSubs_OllamaCloud_NoAutoRoute pins the design's
// central deviation from every other sub-provider (see
// https://docs.vornik.io §2.2/§4): enabling
// ollama_cloud must NOT add any default route naming it, because Ollama
// Cloud's catalogue re-hosts other vendors' models under their own bare
// names (gpt-oss, gemini-3-flash-preview) which would otherwise collide
// with the existing gpt-/gemini- default routes.
func TestDefaultRouterRoutesForSubs_OllamaCloud_NoAutoRoute(t *testing.T) {
	subs := map[string]chat.Provider{"ollama_cloud": nil, "codex-subscription": nil, "vertex": nil}
	routes := defaultRouterRoutesForSubs(subs)

	for _, r := range routes {
		assert.NotEqual(t, "ollama_cloud", r.Kind,
			"ollama_cloud must never receive an auto-added default route")
	}
}

// TestDefaultRouterRoutesForSubs_OllamaCloud_DoesNotAffectGptDefault is the
// companion-review-requested negative test: enabling ollama_cloud must not
// change routing for a real, unrelated model. "gpt-4" (an actual OpenAI
// model with no operator override) must still resolve to
// codex-subscription exactly as it would if ollama_cloud were disabled.
func TestDefaultRouterRoutesForSubs_OllamaCloud_DoesNotAffectGptDefault(t *testing.T) {
	subs := map[string]chat.Provider{"ollama_cloud": nil, "codex-subscription": nil}
	routes := defaultRouterRoutesForSubs(subs)

	var sawGptDefault bool
	for _, r := range routes {
		if r.Prefix == "gpt-" && r.Kind == "codex-subscription" {
			sawGptDefault = true
		}
	}
	assert.True(t, sawGptDefault,
		"gpt- -> codex-subscription default must be present and unaffected by ollama_cloud being enabled")
}

// TestInitChatRouter_OllamaCloudRequiresAPIKey verifies the enabled
// sub-provider fails fast without an api_key.
func TestInitChatRouter_OllamaCloudRequiresAPIKey(t *testing.T) {
	c := &Container{Logger: zerolog.Nop(), Config: &config.Config{}}
	c.Config.Chat = config.ChatConfig{
		Enabled:  true,
		Provider: "router",
		Router: config.ChatRouterConfig{
			Default:     "ollama_cloud",
			OllamaCloud: config.ChatOllamaCloudSubConfig{Enabled: true}, // no api_key
		},
	}
	err := c.initChatRouter(c.Config.Chat)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "api_key")
}

// TestInitChatRouter_OllamaCloudBuildsClient verifies a fully-configured
// ollama_cloud sub-provider builds without error and becomes the routable
// chat client (no network call — construction only).
func TestInitChatRouter_OllamaCloudBuildsClient(t *testing.T) {
	c := &Container{Logger: zerolog.Nop(), Config: &config.Config{}}
	c.Config.Chat = config.ChatConfig{
		Enabled:  true,
		Provider: "router",
		Model:    "glm-5.2:cloud",
		Router: config.ChatRouterConfig{
			Default: "ollama_cloud",
			OllamaCloud: config.ChatOllamaCloudSubConfig{
				Enabled: true,
				APIKey:  "test-ollama-key",
				Model:   "glm-5.2:cloud",
			},
		},
	}
	require.NoError(t, c.initChatRouter(c.Config.Chat))
	require.NotNil(t, c.ChatClient)
	assert.Equal(t, "glm-5.2:cloud", c.ChatClient.Model())
}

// TestInitChatRouter_OllamaCloudAsDefault verifies router.default:
// ollama_cloud makes it the fallback, and that an unrouted model name is
// forwarded through verbatim (fallbackPassesModelThrough includes
// "ollama_cloud" — it's a general-purpose OpenAI-compat proxy, same
// treatment as bedrock/http/vertex/openrouter).
func TestInitChatRouter_OllamaCloudAsDefault(t *testing.T) {
	c := &Container{Logger: zerolog.Nop(), Config: &config.Config{}}
	c.Config.Chat = config.ChatConfig{
		Enabled:  true,
		Provider: "router",
		Router: config.ChatRouterConfig{
			Default: "ollama_cloud",
			OllamaCloud: config.ChatOllamaCloudSubConfig{
				Enabled: true,
				APIKey:  "test-ollama-key",
				Model:   "gpt-oss:120b",
			},
		},
	}
	require.NoError(t, c.initChatRouter(c.Config.Chat))
	require.NotNil(t, c.ChatClient)

	overridable, ok := c.ChatClient.(chat.ModelOverridable)
	require.True(t, ok, "router must implement ModelOverridable")
	pinned := overridable.WithModel("qwen3-coder:480b")
	assert.Equal(t, "qwen3-coder:480b", pinned.Model(),
		"unrouted model must be forwarded verbatim to the ollama_cloud fallback")
}

// TestResolveOllamaCloudEndpoint verifies the baked-in default and the
// override path.
func TestResolveOllamaCloudEndpoint(t *testing.T) {
	assert.Equal(t, "https://ollama.com/v1", resolveOllamaCloudEndpoint(""))
	assert.Equal(t, "https://proxy.internal/v1", resolveOllamaCloudEndpoint("https://proxy.internal/v1"))
}

// TestMergeWithDefaultRoutes_OllamaCloudExplicitRouteWinsOverGptDefault
// proves the merge-order invariant §4 leans on: an operator's explicit
// {prefix:"gpt-oss", kind:"ollama_cloud"} route must win over the
// built-in {prefix:"gpt-", kind:"codex-subscription"} default, even
// though both prefixes match "gpt-oss:120b" — because operator routes
// are prepended ahead of defaults and Router.providerFor is
// first-match-wins.
func TestMergeWithDefaultRoutes_OllamaCloudExplicitRouteWinsOverGptDefault(t *testing.T) {
	user := []config.ChatRouteConfig{
		{Prefix: "gpt-oss", Kind: "ollama_cloud"},
	}
	defaults := []config.ChatRouteConfig{
		{Prefix: "gpt-", Kind: "codex-subscription"},
	}
	merged := mergeWithDefaultRoutes(user, defaults)

	ollamaProvider := &identifiableStubProvider{id: "ollama_cloud"}
	codexProvider := &identifiableStubProvider{id: "codex-subscription"}
	routes := make([]chat.Route, 0, len(merged))
	for _, r := range merged {
		var p chat.Provider = codexProvider
		if r.Kind == "ollama_cloud" {
			p = ollamaProvider
		}
		routes = append(routes, chat.Route{Prefix: r.Prefix, Suffix: r.Suffix, Provider: p, Name: r.Kind})
	}
	router, err := chat.NewRouter(codexProvider, routes)
	require.NoError(t, err)

	pinned := router.WithModel("gpt-oss:120b")
	overridden, ok := pinned.(*identifiableStubProvider)
	require.True(t, ok)
	assert.Equal(t, "ollama_cloud", overridden.id,
		"explicit gpt-oss route must win over the generic gpt- default")
}
