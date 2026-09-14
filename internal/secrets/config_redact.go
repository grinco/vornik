package secrets

import (
	"strings"

	"vornik.io/vornik/internal/secrethygiene"
)

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
// allowlist. Do not fork this logic.
//
// Redaction is implemented with a small allowlist of field-name tokens
// ("password", "api_key", "token", "secret", "bot_token", ...). A field
// matches if its lowercased JSON / map key contains any of those
// tokens. This is deliberately conservative: any future secret-bearing
// field that uses one of these obvious names is redacted automatically
// without requiring a coordinated code change. Non-secret fields that
// happen to contain "token" as a substring (e.g. max_tokens) are
// excluded via a short explicit denylist. The token list and the
// denylists live in internal/secrethygiene (IsSecretBearingName) since
// 2026-09-13, because the configuration assistant screens outbound
// diffs by the same key-name convention and the two must not drift —
// extend them there, never here.
func RedactConfig(v any) any {
	switch typed := v.(type) {
	case map[string]any:
		for k, inner := range typed {
			// D1: MCP server "env" maps carry expanded secret values
			// (GITHUB_TOKEN, DATABASE_URL DSNs, ...). Env values are
			// opaque and frequently secret, and their inner KEYS are
			// arbitrary (PUBLIC_FLAG vs GITHUB_TOKEN), so we can't rely
			// on key-name matching inside them — redact every value
			// wholesale. The map "env" key itself stays so operators
			// still see WHICH vars are configured, just not their values.
			// ("env" is on neither carve-out list, so testing it before
			// the name predicate keeps the original branch order.)
			if strings.ToLower(k) == "env" {
				typed[k] = redactConfigScalar(inner)
				continue
			}
			if secrethygiene.IsSecretBearingName(k) {
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
