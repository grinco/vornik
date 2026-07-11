package secrets

import "strings"

// RedactConfig walks a JSON-decoded value (the output of
// json.Unmarshal into `any`) and blanks any map values whose keys look
// secret-bearing. Arrays are descended recursively; scalars pass
// through untouched.
//
// This is the shared config-dump masker: lifted out of
// internal/api/config_show_handler.go (originally unexported
// `redactSecrets`) so any surface that needs to show a config snapshot
// — GET /api/v1/config, the support-report bundle, and the fix-it
// doctor's grounding bundle (https://docs.vornik.io
// §5.1/§6) — reuses one masking implementation instead of each
// reinventing (and potentially under-covering) the secret-key
// allowlist. Do not fork this logic; extend configSecretKeyTokens /
// configNonSecretExactKeys / configD1NonSecretExactKeys below instead.
//
// Redaction is implemented with a small allowlist of field-name tokens
// ("password", "api_key", "token", "secret", "bot_token", ...). A field
// matches if its lowercased JSON / map key contains any of those
// tokens. This is deliberately conservative: any future secret-bearing
// field that uses one of these obvious names is redacted automatically
// without requiring a coordinated code change. Non-secret fields that
// happen to contain "token" as a substring (e.g. max_tokens) are
// excluded via a short explicit denylist.
func RedactConfig(v any) any {
	switch typed := v.(type) {
	case map[string]any:
		for k, inner := range typed {
			lowered := strings.ToLower(k)
			if configNonSecretExactKeys[lowered] || configD1NonSecretExactKeys[lowered] {
				typed[k] = RedactConfig(inner)
				continue
			}
			// D1: MCP server "env" maps carry expanded secret values
			// (GITHUB_TOKEN, DATABASE_URL DSNs, ...). Env values are
			// opaque and frequently secret, and their inner KEYS are
			// arbitrary (PUBLIC_FLAG vs GITHUB_TOKEN), so we can't rely
			// on key-name matching inside them — redact every value
			// wholesale. The map "env" key itself stays so operators
			// still see WHICH vars are configured, just not their values.
			if lowered == "env" {
				typed[k] = redactConfigScalar(inner)
				continue
			}
			collapsed := strings.ReplaceAll(lowered, "_", "")
			if isConfigSecretKey(collapsed) {
				typed[k] = redactConfigScalar(inner)
				continue
			}
			typed[k] = RedactConfig(inner)
		}
		return typed
	case []any:
		for i, inner := range typed {
			typed[i] = RedactConfig(inner)
		}
		return typed
	default:
		return v
	}
}

// configSecretKeyTokens are substrings that, if present in a field's key
// (with underscores stripped and lowercased), cause its value to be
// replaced with a "<redacted>" placeholder. Keys get their underscores
// removed before matching so snake_case (bot_token) and Go field names
// (BotToken → bottoken after tolower) both hit.
var configSecretKeyTokens = []string{
	"password",
	"apikey", // covers api_key, APIKey, api_keys
	"secret",
	"bottoken", // covers bot_token, BotToken
	"oauth",
	"credential",
	// D1 (audit 2026-06-10): expandEnvPlaceholders substitutes REAL
	// secret values into token/url/dsn-shaped fields before this dump
	// is marshalled. These tokens close that leak. The url/dsn/
	// connectionstring tokens are broad on purpose — endpoints
	// frequently carry credentials in the query string — so the
	// public *_url keys that are genuinely safe are carved out below
	// in configD1NonSecretExactKeys.
	"token", // covers GITHUB_TOKEN-shaped keys, *_token
	"url",   // SSE endpoints + DSN-style *_url values are secret-bearing
	"dsn",
	"connectionstring", // covers connection_string, ConnectionString
	"privatekey",       // covers private_key, PrivateKey
}

// configNonSecretExactKeys are exact lowercased keys that superficially
// match a token above but are known not to be secret (e.g. max_tokens,
// thinking_budget uses "budget" not "token" but listing defensively).
// Keep this list tight — over-listing opens a leak vector.
var configNonSecretExactKeys = map[string]bool{
	"max_tokens":         true,
	"max_history_tokens": true,
	"thinking_budget":    true,
	"max_per_role":       true,
}

// configD1NonSecretExactKeys carves out genuinely-public keys that the
// broadened D1 token list ("token"/"url"/...) would otherwise
// over-redact. Matched against the lowercased JSON key BEFORE the
// underscore-collapse step, so entries here are the exact lowercased
// marshalled key (Go field name when there's no json tag, snake_case
// when there is). Keep this list tight — over-listing re-opens a leak.
var configD1NonSecretExactKeys = map[string]bool{
	"external_base_url": true,
	"webuibaseurl":      true,
	"endpoint":          true,
}

func isConfigSecretKey(loweredKey string) bool {
	for _, tok := range configSecretKeyTokens {
		if strings.Contains(loweredKey, tok) {
			return true
		}
	}
	return false
}

// redactConfigScalar preserves the shape of the redacted value so a
// list of API keys remains a list of placeholders (count leaks, but
// not the keys themselves). Empty strings / nil / zero-length arrays
// stay as they are — there's nothing to hide, and emitting a
// placeholder on an unset field would falsely imply a secret was
// configured.
func redactConfigScalar(v any) any {
	switch typed := v.(type) {
	case string:
		if typed == "" {
			return ""
		}
		return "<redacted>"
	case []any:
		out := make([]any, len(typed))
		for i := range typed {
			out[i] = redactConfigScalar(typed[i])
		}
		return out
	case map[string]any:
		// Nested object under a secret-ish key — descend but redact
		// everything inside too.
		for k, inner := range typed {
			typed[k] = redactConfigScalar(inner)
		}
		return typed
	case nil:
		return nil
	default:
		// Numbers, bools — not secret even if the key shape suggested it.
		return typed
	}
}
