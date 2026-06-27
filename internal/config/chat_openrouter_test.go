package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// TestChatOpenRouterSubConfig_Unmarshal verifies the full openrouter
// sub-provider block round-trips, including the new free_only / referer /
// title fields and the route Suffix field.
func TestChatOpenRouterSubConfig_Unmarshal(t *testing.T) {
	const doc = `
chat:
  provider: router
  router:
    default: openrouter
    openrouter:
      enabled: true
      api_key: "sk-or-test"
      endpoint: "https://openrouter.ai/api/v1"
      model: "deepseek/deepseek-r1:free"
      free_only: true
      referer: "vornik/2026.4.5"
      title: "vornik"
      max_tokens: 4096
    routes:
      - { suffix: ":free", kind: "openrouter" }
      - { prefix: "google/", kind: "vertex" }
`
	var cfg Config
	require.NoError(t, yaml.Unmarshal([]byte(doc), &cfg))

	or := cfg.Chat.Router.OpenRouter
	assert.True(t, or.Enabled)
	assert.Equal(t, "sk-or-test", or.APIKey)
	assert.Equal(t, "https://openrouter.ai/api/v1", or.Endpoint)
	assert.Equal(t, "deepseek/deepseek-r1:free", or.Model)
	assert.True(t, or.FreeOnly)
	assert.Equal(t, "vornik/2026.4.5", or.Referer)
	assert.Equal(t, "vornik", or.Title)
	assert.Equal(t, 4096, or.MaxTokens)

	require.Len(t, cfg.Chat.Router.Routes, 2)
	assert.Equal(t, ":free", cfg.Chat.Router.Routes[0].Suffix)
	assert.Equal(t, "openrouter", cfg.Chat.Router.Routes[0].Kind)
	assert.Empty(t, cfg.Chat.Router.Routes[0].Prefix)
	assert.Equal(t, "google/", cfg.Chat.Router.Routes[1].Prefix)
}

// TestChatOpenRouterSubConfig_ZeroValue verifies an omitted block leaves
// the sub-provider disabled with empty fields (no accidental enablement).
func TestChatOpenRouterSubConfig_ZeroValue(t *testing.T) {
	const doc = `
chat:
  provider: router
  router:
    default: bedrock
`
	var cfg Config
	require.NoError(t, yaml.Unmarshal([]byte(doc), &cfg))
	assert.False(t, cfg.Chat.Router.OpenRouter.Enabled)
	assert.Empty(t, cfg.Chat.Router.OpenRouter.Endpoint)
	assert.False(t, cfg.Chat.Router.OpenRouter.FreeOnly)
}
