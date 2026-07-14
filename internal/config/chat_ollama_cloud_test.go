package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// TestChatOllamaCloudSubConfig_Unmarshal verifies the full ollama_cloud
// sub-provider block round-trips, and that an explicit route naming it
// is preserved (the sub-provider ships with no auto-added default
// route — see https://docs.vornik.io §4).
func TestChatOllamaCloudSubConfig_Unmarshal(t *testing.T) {
	const doc = `
chat:
  provider: router
  router:
    default: ollama_cloud
    ollama_cloud:
      enabled: true
      api_key: "test-ollama-key"
      endpoint: "https://ollama.com/v1"
      model: "glm-5.2:cloud"
      max_tokens: 4096
    routes:
      - { prefix: "gpt-oss", kind: "ollama_cloud" }
`
	var cfg Config
	require.NoError(t, yaml.Unmarshal([]byte(doc), &cfg))

	oc := cfg.Chat.Router.OllamaCloud
	assert.True(t, oc.Enabled)
	assert.Equal(t, "test-ollama-key", oc.APIKey)
	assert.Equal(t, "https://ollama.com/v1", oc.Endpoint)
	assert.Equal(t, "glm-5.2:cloud", oc.Model)
	assert.Equal(t, 4096, oc.MaxTokens)

	require.Len(t, cfg.Chat.Router.Routes, 1)
	assert.Equal(t, "gpt-oss", cfg.Chat.Router.Routes[0].Prefix)
	assert.Equal(t, "ollama_cloud", cfg.Chat.Router.Routes[0].Kind)
}

// TestChatOllamaCloudSubConfig_ZeroValue verifies an omitted block leaves
// the sub-provider disabled with empty fields (no accidental enablement).
func TestChatOllamaCloudSubConfig_ZeroValue(t *testing.T) {
	const doc = `
chat:
  provider: router
  router:
    default: bedrock
`
	var cfg Config
	require.NoError(t, yaml.Unmarshal([]byte(doc), &cfg))
	assert.False(t, cfg.Chat.Router.OllamaCloud.Enabled)
	assert.Empty(t, cfg.Chat.Router.OllamaCloud.Endpoint)
	assert.Empty(t, cfg.Chat.Router.OllamaCloud.Model)
}
