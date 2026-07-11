package secrets

import (
	"encoding/json"
	"testing"
)

// TestRedactConfig_MasksSecretShapedKeys pins the behaviour lifted out
// of internal/api/config_show_handler.go's former unexported
// redactSecrets (fix-it doctor task 3.1, https://docs.vornik.io
// fix-it-doctor-design.md §5.1): both the api package's config-show
// handler and the fix-it grounding assembler share this one masking
// implementation, so a regression here would silently affect both.
func TestRedactConfig_MasksSecretShapedKeys(t *testing.T) {
	cases := []struct {
		name       string
		key        string
		value      string
		wantMasked bool
	}{
		{"password", "password", "hunter2", true},
		{"api_key", "api_key", "sk-abc123", true},
		{"bot_token", "bot_token", "123:ABC", true},
		{"generic_secret", "client_secret", "raw-secret-value", true},
		{"oauth", "oauth_token", "oauth-raw", true},
		{"credential", "credential_blob", "cred-raw", true},
		{"dsn", "dsn", "postgres://u:p@h/db", true},
		{"connection_string", "connection_string", "Server=x;Password=y", true},
		{"private_key", "private_key", "-----BEGIN KEY-----", true},
		{"max_tokens_carveout", "max_tokens", "4096", false},
		{"external_base_url_carveout", "external_base_url", "https://vornik.example.com", false},
		{"webuibaseurl_carveout", "WebUIBaseURL", "https://ui.example.com", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			generic := map[string]any{tc.key: tc.value}
			out := RedactConfig(generic).(map[string]any)
			got, _ := out[tc.key].(string)
			if tc.wantMasked {
				if got == tc.value {
					t.Fatalf("expected %q to be masked, got raw value %q", tc.key, got)
				}
				if got != "<redacted>" {
					t.Fatalf("expected placeholder <redacted>, got %q", got)
				}
			} else if got != tc.value {
				t.Fatalf("expected %q to survive unmasked, got %q", tc.key, got)
			}
		})
	}
}

// TestRedactConfig_EnvMapRedactedWholesale pins the D1 regression fix
// (2026-06-10): MCP server env maps carry expanded secret values under
// arbitrary key names, so the whole map's values are redacted
// regardless of key shape, while the "env" key itself (and which vars
// are configured) survives.
func TestRedactConfig_EnvMapRedactedWholesale(t *testing.T) {
	generic := map[string]any{
		"env": map[string]any{
			"GITHUB_TOKEN": "ghp_realsecretvalue",
			"PUBLIC_FLAG":  "not-actually-secret",
		},
	}
	out := RedactConfig(generic).(map[string]any)
	env := out["env"].(map[string]any)
	if env["GITHUB_TOKEN"] != "<redacted>" {
		t.Fatalf("expected GITHUB_TOKEN redacted, got %v", env["GITHUB_TOKEN"])
	}
	if env["PUBLIC_FLAG"] != "<redacted>" {
		t.Fatalf("expected PUBLIC_FLAG redacted (env values redact wholesale), got %v", env["PUBLIC_FLAG"])
	}
	if _, ok := env["GITHUB_TOKEN"]; !ok {
		t.Fatalf("expected GITHUB_TOKEN key to survive (only the value is masked)")
	}
}

// TestRedactConfig_EmptyStringsAndArraysPreserveShape pins that unset
// fields don't get a placeholder (which would falsely imply a secret
// was configured) and that arrays/nested objects are walked
// recursively rather than blanked wholesale.
func TestRedactConfig_EmptyStringsAndArraysPreserveShape(t *testing.T) {
	generic := map[string]any{
		"api_key": "",
		"tokens":  []any{"a", "b"}, // "tokens" contains "token" substring -> secret-shaped list
		"nested": map[string]any{
			"password": "inner-secret",
		},
	}
	out := RedactConfig(generic).(map[string]any)
	if out["api_key"] != "" {
		t.Fatalf("expected empty string to stay empty, got %v", out["api_key"])
	}
	list := out["tokens"].([]any)
	if len(list) != 2 || list[0] != "<redacted>" || list[1] != "<redacted>" {
		t.Fatalf("expected list of placeholders preserving length, got %v", list)
	}
	nested := out["nested"].(map[string]any)
	if nested["password"] != "<redacted>" {
		t.Fatalf("expected nested secret masked, got %v", nested["password"])
	}
}

// TestRedactConfig_RoundTripsThroughJSON exercises the real call
// pattern (json.Marshal -> generic any -> RedactConfig) both api call
// sites use, rather than only hand-built map[string]any literals.
func TestRedactConfig_RoundTripsThroughJSON(t *testing.T) {
	type inner struct {
		BotToken string `json:"bot_token"`
		Public   string `json:"public_value"`
	}
	type cfg struct {
		Inner inner `json:"inner"`
	}
	raw, err := json.Marshal(cfg{Inner: inner{BotToken: "raw-token-value", Public: "keep-me"}})
	if err != nil {
		t.Fatal(err)
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	redacted := RedactConfig(generic).(map[string]any)
	innerMap := redacted["inner"].(map[string]any)
	if innerMap["bot_token"] != "<redacted>" {
		t.Fatalf("expected bot_token redacted, got %v", innerMap["bot_token"])
	}
	if innerMap["public_value"] != "keep-me" {
		t.Fatalf("expected public_value untouched, got %v", innerMap["public_value"])
	}
}
