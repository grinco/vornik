package api

import (
	"encoding/json"
	"net/http"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/secrets"
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
	if !s.requireOperatorScope(w, r) {
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

// redactSecrets walks a JSON-decoded value and blanks any map values
// whose keys look secret. Lifted into internal/secrets.RedactConfig
// (2026-07, fix-it doctor task 3.1) so the grounding-bundle assembler
// reuses the exact same masking logic instead of forking it; this is a
// thin package-local alias so the two existing call sites in this
// package (GetConfig below, support_report_handlers.go) don't need to
// change.
func redactSecrets(v any) any {
	return secrets.RedactConfig(v)
}

// ensureConfigRefAvailable is a build-time reminder that the server
// must hold a config reference for this handler to work. It is a no-op
// at runtime and exists only so dropping the field from api.Server
// surfaces as a compile error here.
var _ = func(s *Server) *config.Config { return s.config }
