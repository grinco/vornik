package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"vornik.io/vornik/internal/config"
)

// GetConfig handles GET /api/v1/config. Returns the effective Config
// with secret-bearing fields redacted. Used by vornikctl config show
// (and future debugging UIs) so operators can see what the daemon
// actually loaded without having to decode YAML + env-var substitutions
// by hand.
//
// Redaction is implemented with a small allowlist of field-name tokens
// ("password", "api_key", "token", "secret", "bot_token"). A field
// matches if its lowercased JSON / map key contains any of those
// tokens. This is deliberately conservative: any future secret-bearing
// field that uses one of these obvious names is redacted automatically
// without requiring a coordinated code change. Non-secret fields that
// happen to contain "token" as a substring (e.g. max_tokens) are
// excluded via a short explicit denylist.
func (s *Server) GetConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use GET")
		return
	}
	if !s.requireAdminGate(w, r) {
		return
	}
	if s.config == nil {
		respondError(w, http.StatusServiceUnavailable, "CONFIG_UNAVAILABLE", "config not wired into API server")
		return
	}

	// Marshal/unmarshal through generic JSON so we don't have to mirror
	// the whole config schema with parallel "redacted" struct tags.
	raw, err := json.Marshal(s.config)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "encode config: "+err.Error())
		return
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "decode config: "+err.Error())
		return
	}
	redacted := redactSecrets(generic)
	respondJSON(w, http.StatusOK, redacted)
}

// secretKeyTokens are substrings that, if present in a field's key (with
// underscores stripped and lowercased), cause its value to be replaced
// with a "<redacted>" placeholder. Keys get their underscores removed
// before matching so snake_case (bot_token) and Go field names (BotToken
// → bottoken after tolower) both hit.
var secretKeyTokens = []string{
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
	// in d1NonSecretExactKeys.
	"token", // covers GITHUB_TOKEN-shaped keys, *_token
	"url",   // SSE endpoints + DSN-style *_url values are secret-bearing
	"dsn",
	"connectionstring", // covers connection_string, ConnectionString
	"privatekey",       // covers private_key, PrivateKey
}

// nonSecretKeySuffixes are exact lowercased keys that superficially
// match a token above but are known not to be secret (e.g. max_tokens,
// thinking_budget uses "budget" not "token" but listing defensively).
// Keep this list tight — over-listing opens a leak vector.
var nonSecretExactKeys = map[string]bool{
	"max_tokens":         true,
	"max_history_tokens": true,
	"thinking_budget":    true,
	"max_per_role":       true,
}

// d1NonSecretExactKeys carves out genuinely-public keys that the
// broadened D1 token list ("token"/"url"/...) would otherwise
// over-redact. Matched against the lowercased JSON key BEFORE the
// underscore-collapse step, so entries here are the exact lowercased
// marshalled key (Go field name when there's no json tag, snake_case
// when there is). Keep this list tight — over-listing re-opens a leak.
//
// Casing reference (the config structs mostly carry yaml-only tags, so
// json.Marshal emits the Go field name verbatim):
//   - Auth.ExternalBaseURL  → json tag → "external_base_url"
//   - Telegram.WebUIBaseURL → no json tag → "WebUIBaseURL" → lower "webuibaseurl"
//   - tracing/s3/llm endpoints → "endpoint" (no url/token substring, but
//     listed defensively so a future token addition can't catch them)
var d1NonSecretExactKeys = map[string]bool{
	"external_base_url": true,
	"webuibaseurl":      true,
	"endpoint":          true,
}

// redactSecrets walks a JSON-decoded value and blanks any map values
// whose keys look secret. Arrays are descended recursively; scalars
// pass through untouched.
func redactSecrets(v any) any {
	switch typed := v.(type) {
	case map[string]any:
		for k, inner := range typed {
			lowered := strings.ToLower(k)
			if nonSecretExactKeys[lowered] || d1NonSecretExactKeys[lowered] {
				typed[k] = redactSecrets(inner)
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
				typed[k] = redactScalar(inner)
				continue
			}
			collapsed := strings.ReplaceAll(lowered, "_", "")
			if isSecretKey(collapsed) {
				typed[k] = redactScalar(inner)
				continue
			}
			typed[k] = redactSecrets(inner)
		}
		return typed
	case []any:
		for i, inner := range typed {
			typed[i] = redactSecrets(inner)
		}
		return typed
	default:
		return v
	}
}

func isSecretKey(loweredKey string) bool {
	for _, tok := range secretKeyTokens {
		if strings.Contains(loweredKey, tok) {
			return true
		}
	}
	return false
}

// redactScalar preserves the shape of the redacted value so a list of
// API keys remains a list of placeholders (count leaks, but not the
// keys themselves). Empty strings / nil / zero-length arrays stay as
// they are — there's nothing to hide, and emitting a placeholder on
// an unset field would falsely imply a secret was configured.
func redactScalar(v any) any {
	switch typed := v.(type) {
	case string:
		if typed == "" {
			return ""
		}
		return "<redacted>"
	case []any:
		out := make([]any, len(typed))
		for i := range typed {
			out[i] = redactScalar(typed[i])
		}
		return out
	case map[string]any:
		// Nested object under a secret-ish key — descend but redact
		// everything inside too.
		for k, inner := range typed {
			typed[k] = redactScalar(inner)
		}
		return typed
	case nil:
		return nil
	default:
		// Numbers, bools — not secret even if the key shape suggested it.
		return typed
	}
}

// ensureConfigRefAvailable is a build-time reminder that the server
// must hold a config reference for this handler to work. It is a no-op
// at runtime and exists only so dropping the field from api.Server
// surfaces as a compile error here.
var _ = func(s *Server) *config.Config { return s.config }
