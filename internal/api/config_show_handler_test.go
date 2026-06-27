package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/config"
)

func Test_GetConfig_WithConfig_ReturnsServerAddress(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Address: ":8080",
		},
	}
	server := NewServer(WithLogger(zerolog.Nop()), WithConfig(cfg))

	req := authDisabledReq(httptest.NewRequest(http.MethodGet, "/api/v1/config", nil))
	rec := httptest.NewRecorder()

	server.GetConfig(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var result map[string]any
	err := json.Unmarshal(rec.Body.Bytes(), &result)
	require.NoError(t, err)

	// Verify server address is in response
	if serverVal, ok := result["server"].(map[string]any); ok {
		if address, ok := serverVal["address"].(string); ok {
			assert.Equal(t, ":8080", address)
		}
	}
}

func Test_GetConfig_NilConfig_ReturnsServiceUnavailable(t *testing.T) {
	server := NewServer(WithLogger(zerolog.Nop()))
	// config field is nil by default

	req := authDisabledReq(httptest.NewRequest(http.MethodGet, "/api/v1/config", nil))
	rec := httptest.NewRecorder()

	server.GetConfig(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "CONFIG_UNAVAILABLE")
}

func Test_GetConfig_NonGetMethod_ReturnsMethodNotAllowed(t *testing.T) {
	cfg := &config.Config{}
	server := NewServer(WithLogger(zerolog.Nop()), WithConfig(cfg))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/config", nil)
	rec := httptest.NewRecorder()

	server.GetConfig(rec, req)

	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.Contains(t, rec.Body.String(), "METHOD_NOT_ALLOWED")
}

// TestGetConfig_NeverLeaksProviderSecret pins that the GitHub OAuth client_secret
// never appears in the /api/v1/config response body. With json:"-" on the field
// it is absent entirely from JSON output — the API redaction layer is defense-in-depth.
func TestGetConfig_NeverLeaksProviderSecret(t *testing.T) {
	gh := &config.GitHubProviderSettings{
		ClientID:     "id",
		ClientSecret: "raw-secret-value",
	}
	cfg := &config.Config{
		Auth: config.AuthSettings{
			Providers: config.ProviderSettings{
				GitHub: gh,
			},
		},
	}
	server := NewServer(WithLogger(zerolog.Nop()), WithConfig(cfg))

	req := authDisabledReq(httptest.NewRequest(http.MethodGet, "/api/v1/config", nil))
	rec := httptest.NewRecorder()

	server.GetConfig(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.NotContains(t, body, "raw-secret-value",
		"resolved client_secret must never appear in the config response")
	// With json:"-" the field is absent entirely; no "client_secret" key with a value.
	assert.NotContains(t, body, `"client_secret":"raw-secret-value"`,
		"client_secret field must not be serialised at all")
}

// Test_GetConfig_D1_RedactsExpandedMCPSecrets is the regression test for
// audit finding D1 (2026-06-10): expandEnvPlaceholders substitutes REAL
// secret values into MCP server Env maps and SSE URL fields before the
// config is marshalled, but the redaction allowlist only covered
// password/apikey/secret/bottoken/oauth/credential — so GITHUB_TOKEN,
// DATABASE_URL (DSN with embedded password), and SSE url tokens survived
// in GET /api/v1/config + `vornikctl config show`. The fix redacts MCP Env
// VALUES wholesale and extends the token list (token/url/dsn/...), while
// keeping public *_url keys (web_ui_base_url, external_base_url) visible.
func Test_GetConfig_D1_RedactsExpandedMCPSecrets(t *testing.T) {
	cfg := &config.Config{
		MCP: config.MCPConfig{
			Servers: []config.MCPServerConfig{
				{
					Name:      "github",
					Transport: "stdio",
					Command:   "mcp-github",
					Env: map[string]string{
						"GITHUB_TOKEN": "ghp_realsecretvalue",
						"DATABASE_URL": "postgres://user:dbpass@host:5432/db",
						"PUBLIC_FLAG":  "not-actually-secret-but-env-opaque",
					},
				},
				{
					Name:      "remote",
					Transport: "sse",
					URL:       "https://mcp.example.com/sse?token=urlsecret123",
				},
			},
		},
		Auth: config.AuthSettings{
			ExternalBaseURL: "https://vornik.example.com",
		},
	}
	server := NewServer(WithLogger(zerolog.Nop()), WithConfig(cfg))

	req := authDisabledReq(httptest.NewRequest(http.MethodGet, "/api/v1/config", nil))
	rec := httptest.NewRecorder()
	server.GetConfig(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()

	// MCP env values (opaque, frequently secret) must be redacted wholesale.
	assert.NotContains(t, body, "ghp_realsecretvalue", "GITHUB_TOKEN env value must be redacted")
	assert.NotContains(t, body, "postgres://user:dbpass@host:5432/db", "DATABASE_URL DSN env value must be redacted")
	assert.NotContains(t, body, "not-actually-secret-but-env-opaque", "all MCP env values are redacted wholesale")
	// SSE URL token must be redacted.
	assert.NotContains(t, body, "urlsecret123", "SSE url (token-bearing) must be redacted")

	// Public *_url keys must still pass through unredacted.
	assert.Contains(t, body, "https://vornik.example.com", "external_base_url is a public key and must NOT be redacted")
}

// Test_GetConfig_D1_PublicWebUIBaseURLNotRedacted pins that the web_ui_base_url
// public key survives the broadened url-token redaction (D1 carve-out).
func Test_GetConfig_D1_PublicWebUIBaseURLNotRedacted(t *testing.T) {
	cfg := &config.Config{
		Telegram: config.TelegramConfig{
			WebUIBaseURL: "https://ui.vornik.example.com",
		},
	}
	server := NewServer(WithLogger(zerolog.Nop()), WithConfig(cfg))

	req := authDisabledReq(httptest.NewRequest(http.MethodGet, "/api/v1/config", nil))
	rec := httptest.NewRecorder()
	server.GetConfig(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "https://ui.vornik.example.com",
		"web_ui_base_url is a public key and must NOT be redacted by the url token")
}

func Test_GetConfig_RequiresAdmin(t *testing.T) {
	cfg := &config.Config{}
	server := NewServer(WithLogger(zerolog.Nop()), WithConfig(cfg), WithAdminConfig(config.AdminConfig{
		Enabled:     true,
		AllowedKeys: []string{"sk-admin"},
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
	req = req.WithContext(context.WithValue(req.Context(), apiKeyKey, "sk-project"))
	rec := httptest.NewRecorder()
	server.GetConfig(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}
