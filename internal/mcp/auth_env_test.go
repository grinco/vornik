package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// TestBuildStdioEnv_AuthEnvReachesTheSubprocess covers mode: env — the Plane 2
// case where the MCP server holds its own upstream app credentials.
func TestBuildStdioEnv_AuthEnvReachesTheSubprocess(t *testing.T) {
	env := buildStdioEnv(ServerConfig{
		Name:    "reddit",
		Env:     map[string]string{"REDDIT_USER_AGENT": "vornik"},
		AuthEnv: map[string]string{"REDDIT_CLIENT_SECRET": "sec-1"},
	})
	assert.Contains(t, env, "REDDIT_USER_AGENT=vornik")
	assert.Contains(t, env, "REDDIT_CLIENT_SECRET=sec-1")
}

// TestBuildStdioEnv_AuthEnvIsNotShellExpanded is the regression that matters
// most here: the legacy Env map runs through expandSafe, and a secret
// containing `$` (base64 and JWT-ish values routinely do) would be silently
// mangled — or worse, expand to another variable's value.
func TestBuildStdioEnv_AuthEnvIsNotShellExpanded(t *testing.T) {
	t.Setenv("HOME", "/root")
	env := buildStdioEnv(ServerConfig{
		Name:    "reddit",
		Env:     map[string]string{"LEGACY": "${HOME}/x"},
		AuthEnv: map[string]string{"SECRET": "abc${HOME}def$1"},
	})
	assert.Contains(t, env, "LEGACY=/root/x", "the legacy env map keeps ${VAR} expansion")
	assert.Contains(t, env, "SECRET=abc${HOME}def$1", "a resolved secret must be passed verbatim")
}

// TestBuildStdioEnv_AuthEnvWinsOverLegacyEnv — if an operator has both, the
// auth-managed value is the one that was resolved from the secret store.
func TestBuildStdioEnv_AuthEnvWinsOverLegacyEnv(t *testing.T) {
	env := buildStdioEnv(ServerConfig{
		Name:    "reddit",
		Env:     map[string]string{"TOKEN": "stale"},
		AuthEnv: map[string]string{"TOKEN": "fresh"},
	})
	var got []string
	for _, kv := range env {
		if strings.HasPrefix(kv, "TOKEN=") {
			got = append(got, kv)
		}
	}
	// Later entries win in exec's env, so the auth value must come last.
	require.NotEmpty(t, got)
	assert.Equal(t, "TOKEN=fresh", got[len(got)-1])
}

// TestCollidingAuthEnvKeys — auth winning over the legacy Env map is the intent,
// but it must not be silent: the header path Warns on the same collision, and an
// operator debugging "my env value never reaches the server" needs the same clue
// (review-20260804-350e finding 4).
func TestCollidingAuthEnvKeys(t *testing.T) {
	assert.Equal(t, []string{"BOTH", "TOKEN"}, collidingAuthEnvKeys(ServerConfig{
		Env:     map[string]string{"BOTH": "a", "TOKEN": "stale", "ONLY_ENV": "x"},
		AuthEnv: map[string]string{"BOTH": "b", "TOKEN": "fresh", "ONLY_AUTH": "y"},
	}), "collisions must be reported sorted, and only where both maps set the key")

	assert.Empty(t, collidingAuthEnvKeys(ServerConfig{
		Env:     map[string]string{"A": "1"},
		AuthEnv: map[string]string{"B": "2"},
	}))
	assert.Empty(t, collidingAuthEnvKeys(ServerConfig{AuthEnv: map[string]string{"A": "1"}}))
	assert.Empty(t, collidingAuthEnvKeys(ServerConfig{Env: map[string]string{"A": "1"}}))
}

// TestServerConfig_AuthMaterialNeverSerializes keeps resolved credentials out of
// every rendered surface: ServerConfig is JSON-encoded into the daemon's MCP
// discovery API and marshalled in operator-facing dumps.
func TestServerConfig_AuthMaterialNeverSerializes(t *testing.T) {
	const secret = "xoxb-resolved-secret-value"
	cfg := ServerConfig{
		Name:        "n8n",
		Transport:   "streamable-http",
		URL:         "https://n8n.example.com/mcp/abc",
		AuthHeaders: map[string]string{"Authorization": "Bearer " + secret},
		AuthEnv:     map[string]string{"TOKEN": secret},
	}

	asJSON, err := json.Marshal(cfg)
	require.NoError(t, err)
	assert.NotContains(t, string(asJSON), secret)

	asYAML, err := yaml.Marshal(cfg)
	require.NoError(t, err)
	assert.NotContains(t, string(asYAML), secret)
}
