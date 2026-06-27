package api

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// checkConfigSecretHygiene flags two operational hazards in the main
// config.yaml:
//
//  1. Plaintext secret-bearing fields that were NOT loaded from an
//     environment variable. The config loader already runs
//     os.ExpandEnv over string fields, so an operator who uses
//     ${DB_PASSWORD} gets env substitution. But operators frequently
//     paste the real password in during setup and forget to rotate
//     to ${VAR}. We can't distinguish "literal" from "expanded" after
//     the fact — os.ExpandEnv is destructive — so this check
//     fingerprints obvious placeholders and flags anything that looks
//     like a raw secret.
//  2. config.yaml world- or group-readable (mode > 0640). Any process
//     running as another local user can scrape the file.
//
// Complements secrets_permissions (which scans ~/.config/vornik/secrets
// for key material files); this one focuses on vornik's own config.
func (h *DoctorHandlers) checkConfigSecretHygiene() DoctorCheck {
	const name = "config_secret_hygiene"
	if h.secretFields == nil {
		// Container hasn't wired the snapshot — happens in direct
		// handler tests. Skip quietly rather than emit a false-positive.
		return DoctorCheck{Name: name, Status: "OK", Message: "no config snapshot captured, skipping"}
	}

	var items []string

	// 1. File permissions. 0600 is the target; 0640 is acceptable when
	// a trusted group reads it. 0644+ is never acceptable for a file
	// that may carry secrets.
	if h.configPath != "" {
		if info, err := os.Stat(h.configPath); err == nil {
			mode := info.Mode().Perm()
			if mode > 0o640 {
				items = append(items, fmt.Sprintf(
					"config.yaml at %s has mode %04o (> 0640); recommend `chmod 600 %s`",
					h.configPath, mode, h.configPath,
				))
			}
		}
	}

	// 2. Plaintext-secret detection. The snapshot captured at boot
	// (h.secretFields) holds POST-expansion values, so a field that
	// was authored as ${OLLAMA_API_KEY} is indistinguishable from a
	// pasted-in raw key once os.ExpandEnv runs. We re-read the YAML
	// here without env expansion and let any path whose raw value
	// is ${VAR} (or absent) skip the heuristic. Without this hop the
	// check fired on every operator who'd done the right thing —
	// observed 2026-05-08 against a config with chat.router.http.api_key
	// and telegram.bot_token both correctly env-sourced.
	rawSecrets := h.loadRawSecretFields()

	// The map isn't sorted — sort the keys so the report lines are
	// stable between doctor runs.
	keys := make([]string, 0, len(h.secretFields))
	for k := range h.secretFields {
		keys = append(keys, k)
	}
	sortStrings(keys)
	for _, key := range keys {
		if raw, ok := rawSecrets[key]; ok && isEnvSourcedRaw(raw) {
			continue // operator did the right thing — env-sourced
		}
		if looksLikeRawSecret(h.secretFields[key]) {
			items = append(items, fmt.Sprintf(
				"%s appears to be a raw plaintext secret (%d chars); recommend moving to ${ENV_VAR} and setting the variable in the systemd unit",
				key, len(h.secretFields[key]),
			))
		}
	}

	if len(items) == 0 {
		return DoctorCheck{
			Name:    name,
			Status:  "OK",
			Message: "config.yaml permissions tight; sensitive fields sourced from env or empty",
		}
	}
	return DoctorCheck{
		Name:    name,
		Status:  "WARNING",
		Message: fmt.Sprintf("%d config secret-hygiene finding(s)", len(items)),
		Items:   items,
	}
}

// sortStrings is a tiny helper so this file doesn't need to import
// `sort` just for the stable-report ordering above.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// loadRawSecretFields re-reads the YAML at h.configPath WITHOUT env
// expansion and returns the raw string at each known secret-bearing
// dotted path. Missing file or unparseable YAML returns an empty map
// (which makes every lookup fall back to the post-expansion heuristic
// — same behaviour as before this fix). Keys mirror the
// h.secretFields map populated by SetServerConfig.
func (h *DoctorHandlers) loadRawSecretFields() map[string]string {
	out := map[string]string{}
	if h.configPath == "" {
		return out
	}
	data, err := os.ReadFile(h.configPath)
	if err != nil {
		return out
	}
	var raw map[string]interface{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return out
	}
	// The dotted-path keys we lint mirror the YAML hierarchy 1:1 (the
	// loader uses tag-derived names with no remapping). When that
	// stops being true, add a translation table here.
	paths := []string{
		"database.password",
		"chat.api_key",
		"chat.router.http.api_key",
		"runtime.agent_llm.api_key",
		"telegram.bot_token",
		"memory.embedding_api_key",
		"auth.providers.github.client_secret",
	}
	for _, p := range paths {
		if v, ok := lookupYAMLPath(raw, p); ok {
			out[p] = v
		}
	}
	return out
}

// lookupYAMLPath walks a parsed YAML map by a dotted path. Returns
// the string at the path and true on hit; false on missing key or a
// non-string value. yaml.v3 decodes nested maps as map[string]any
// (when keys are strings) or map[any]any depending on the document
// — handle both so the walk doesn't trip on either shape.
func lookupYAMLPath(m map[string]interface{}, path string) (string, bool) {
	parts := strings.Split(path, ".")
	var cur interface{} = m
	for _, p := range parts {
		switch v := cur.(type) {
		case map[string]interface{}:
			next, ok := v[p]
			if !ok {
				return "", false
			}
			cur = next
		case map[interface{}]interface{}:
			next, ok := v[p]
			if !ok {
				return "", false
			}
			cur = next
		default:
			return "", false
		}
	}
	if s, ok := cur.(string); ok {
		return s, true
	}
	return "", false
}

// isEnvSourcedRaw reports whether the supplied raw YAML value (taken
// before os.ExpandEnv runs) is an env-var reference. The loader
// honours both ${VAR} and $VAR syntax via os.ExpandEnv, but every
// secret-bearing field in the example config uses ${VAR} so we
// special-case the safer braced form. An empty string or a value
// that doesn't reference an env var falls through to the post-
// expansion heuristic — that's where literal-secret detection lives.
func isEnvSourcedRaw(raw string) bool {
	trim := strings.TrimSpace(raw)
	if trim == "" {
		return true // empty source can't be a leaked secret
	}
	if strings.HasPrefix(trim, "${") && strings.HasSuffix(trim, "}") {
		return true
	}
	return false
}

// looksLikeRawSecret returns true when the supplied string smells
// like a real secret (as opposed to an empty field, obvious dev
// placeholder, or already-expanded env reference). Deliberately
// conservative to keep false-positive noise low — a WARNING that
// over-fires on "CHANGE_ME" trains operators to ignore the check.
func looksLikeRawSecret(s string) bool {
	trim := strings.TrimSpace(s)
	if trim == "" {
		return false
	}
	// An explicit ${VAR} that didn't get expanded — env var was
	// unset. This is a DIFFERENT problem (config will fail at
	// runtime), not a hygiene issue.
	if strings.HasPrefix(trim, "${") && strings.HasSuffix(trim, "}") {
		return false
	}
	// Dev / placeholder markers we ship in examples.
	lower := strings.ToLower(trim)
	placeholders := []string{
		"change_me", "changeme", "placeholder", "replace_me",
		"your_", "example_", "sample_", "todo_", "vornik-dev",
		"<your", "<replace", "dev-key", "localpassword",
	}
	for _, p := range placeholders {
		if strings.Contains(lower, p) {
			return false
		}
	}
	// A short string (< 16 chars) is unlikely to be a real secret —
	// modern API keys are 30+ chars. Avoids flagging "disabled" or
	// "true" or a short deliberate dev password.
	if len(trim) < 16 {
		return false
	}
	return true
}
