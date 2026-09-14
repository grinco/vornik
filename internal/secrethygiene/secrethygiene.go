// Package secrethygiene is the one implementation of "does this string look
// like a pasted secret, and does this config carry one". It is a LEAF —
// stdlib plus gopkg.in/yaml.v3 — because three surfaces run the same
// heuristic and a safety check with two implementations has one that is
// wrong (usually the newer):
//
//   - the doctor's config_secret_hygiene check (internal/api), which reports
//     findings as an advisory WARNING;
//   - the configuration assistant's egress gate
//     (https://docs.vornik.io §7.3),
//     which makes the same finding a named REFUSAL before any model call and
//     runs the same heuristic over a diff before the judge/consult calls;
//   - the config-dump redactor (internal/secrets.RedactConfig), which shares
//     the secret-bearing KEY-NAME convention so the assistant's diff screen
//     and the operator-facing dump agree on which fields carry secrets.
//
// Extracted from internal/api/doctor_config_secret_hygiene.go on 2026-09-13
// (config-assistant design §7: "the reuse is an EXTRACTION, not a call").
// Every helper here keeps the behaviour it had in the api package; the
// incident notes that shaped that behaviour travelled with the code.
package secrethygiene

import (
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Finding kinds. The vocabulary is deliberately small: the doctor check
// renders each kind as one report line and the assistant refuses on either.
const (
	// KindRawSecret means a secret-bearing field holds a value that looks like
	// pasted plaintext rather than a ${VAR} reference, a placeholder or a
	// file-sourced value.
	KindRawSecret = "raw_secret"
	// KindPermissions means the config file is group- or world-readable
	// (mode > 0640), so any local process can scrape whatever it carries.
	KindPermissions = "permissions"
)

// Finding is one raw-secret or permission finding.
type Finding struct {
	// Key is the dotted config path (database.password) for a raw-secret
	// finding, the bare key name for a ScanText finding, and empty for a
	// permissions finding.
	Key string
	// Detail is the human-readable finding, worded exactly as the doctor
	// check has always reported it. It never contains the secret value.
	Detail string
	// Kind is KindRawSecret or KindPermissions.
	Kind string
	// File is the config file the finding was evaluated against; empty for
	// ScanText findings, which have no file. Carried so Refusal can name
	// "the finding and the file" (design test 33) from the finding alone.
	File string
}

// SecretFieldPaths are the dotted config paths the hygiene evaluation lints.
// They mirror the YAML hierarchy 1:1 (the loader uses tag-derived names with
// no remapping). When that stops being true, add a translation table in
// Evaluate rather than a second list.
var SecretFieldPaths = []string{
	"database.password",
	"chat.api_key",
	"chat.router.http.api_key",
	"runtime.agent_llm.api_key",
	"telegram.bot_token",
	"memory.embedding_api_key",
	"auth.providers.github.client_secret",
	// `_file` siblings: presence means the secret is externalized to a
	// file, which the loader resolves INTO the field above at startup.
	// Looked up so the lint can tell "resolved from a 0600 file" apart
	// from "pasted into config.yaml" — post-resolution they are identical.
	"auth.providers.github.client_secret_file",
}

// Evaluate flags two operational hazards in the main config.yaml:
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
// snapshot is the POST-expansion map (dotted key → value) captured at boot;
// a nil snapshot means no snapshot was wired (direct handler tests, a daemon
// started without a file) and returns (nil, false): "not evaluable", which
// the doctor renders as SKIPPED rather than a false positive. Evaluate
// re-reads the YAML at configPath WITHOUT env expansion so a field authored
// as ${VAR} is not mistaken for a pasted key. Findings come back in a stable
// order — permissions first, then raw-secret findings sorted by key — so a
// report is byte-identical between runs.
//
// Every call is a FRESH evaluation of the file on disk (design §7.3: a gate
// that reads a cached verdict is a gate against the state of an hour ago).
func Evaluate(configPath string, snapshot map[string]string) (findings []Finding, evaluable bool) {
	if snapshot == nil {
		return nil, false
	}

	// 1. File permissions. 0600 is the target; 0640 is acceptable when
	// a trusted group reads it. 0644+ is never acceptable for a file
	// that may carry secrets.
	if configPath != "" {
		if info, err := os.Stat(configPath); err == nil {
			mode := info.Mode().Perm()
			if mode > 0o640 {
				findings = append(findings, Finding{
					Kind: KindPermissions,
					File: configPath,
					Detail: fmt.Sprintf(
						"config.yaml at %s has mode %04o (> 0640); recommend `chmod 600 %s`",
						configPath, mode, configPath,
					),
				})
			}
		}
	}

	// 2. Plaintext-secret detection. The snapshot holds POST-expansion
	// values, so a field that was authored as ${OLLAMA_API_KEY} is
	// indistinguishable from a pasted-in raw key once os.ExpandEnv runs.
	// We re-read the YAML here without env expansion and let any path
	// whose raw value is ${VAR} (or absent) skip the heuristic. Without
	// this hop the check fired on every operator who'd done the right
	// thing — observed 2026-05-08 against a config with
	// chat.router.http.api_key and telegram.bot_token both correctly
	// env-sourced.
	rawSecrets := loadRawSecretFields(configPath)

	// The map isn't sorted — sort the keys so the report lines are
	// stable between doctor runs.
	keys := make([]string, 0, len(snapshot))
	for k := range snapshot {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if raw, ok := rawSecrets[key]; ok && IsEnvSourcedRaw(raw) {
			continue // operator did the right thing — env-sourced
		}
		// A `<field>_file:` sibling is the STRONGEST option available (the
		// loader reads a 0600 file at startup into the very field we snapshot),
		// so the resolved value being secret-shaped is expected, not a finding.
		// Without this the check punished the most secure configuration —
		// observed 2026-07-25 against auth.providers.github.client_secret_file,
		// the same class of false positive the env-sourced hop above fixed.
		if rawSecrets[key+"_file"] != "" {
			continue
		}
		if LooksLikeRawSecret(snapshot[key]) {
			findings = append(findings, Finding{
				Kind: KindRawSecret,
				Key:  key,
				File: configPath,
				Detail: fmt.Sprintf(
					"%s appears to be a raw plaintext secret (%d chars); recommend moving to ${ENV_VAR} and setting the variable in the systemd unit",
					key, len(snapshot[key]),
				),
			})
		}
	}
	return findings, true
}

// loadRawSecretFields re-reads the YAML at configPath WITHOUT env
// expansion and returns the raw string at each known secret-bearing
// dotted path. Missing file or unparseable YAML returns an empty map
// (which makes every lookup fall back to the post-expansion heuristic
// — same behaviour as before this fix). Keys mirror the snapshot map
// the doctor populates in SetServerConfig.
func loadRawSecretFields(configPath string) map[string]string {
	out := map[string]string{}
	if configPath == "" {
		return out
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return out
	}
	var raw map[string]interface{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return out
	}
	for _, p := range SecretFieldPaths {
		if v, ok := LookupYAMLPath(raw, p); ok {
			out[p] = v
		}
	}
	return out
}

// LookupYAMLPath walks a parsed YAML map by a dotted path. Returns
// the string at the path and true on hit; false on missing key or a
// non-string value. yaml.v3 decodes nested maps as map[string]any
// (when keys are strings) or map[any]any depending on the document
// — handle both so the walk doesn't trip on either shape.
func LookupYAMLPath(m map[string]interface{}, path string) (string, bool) {
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

// IsEnvSourcedRaw reports whether the supplied raw YAML value (taken
// before os.ExpandEnv runs) is an env-var reference. The loader
// honours both ${VAR} and $VAR syntax via os.ExpandEnv, but every
// secret-bearing field in the example config uses ${VAR} so we
// special-case the safer braced form. An empty string or a value
// that doesn't reference an env var falls through to the post-
// expansion heuristic — that's where literal-secret detection lives.
func IsEnvSourcedRaw(raw string) bool {
	trim := strings.TrimSpace(raw)
	if trim == "" {
		return true // empty source can't be a leaked secret
	}
	if strings.HasPrefix(trim, "${") && strings.HasSuffix(trim, "}") {
		return true
	}
	return false
}

// placeholders are the dev / placeholder markers we ship in examples. A
// value containing any of them (case-insensitively) is never a finding.
var placeholders = []string{
	"change_me", "changeme", "placeholder", "replace_me",
	"your_", "example_", "sample_", "todo_", "vornik-dev",
	"<your", "<replace", "dev-key", "localpassword",
}

// minRawSecretLen is the length floor below which a value is not treated
// as a secret. A short string (< 16 chars) is unlikely to be a real secret
// — modern API keys are 30+ chars. Avoids flagging "disabled" or "true" or
// a short deliberate dev password.
const minRawSecretLen = 16

// LooksLikeRawSecret returns true when the supplied string smells
// like a real secret (as opposed to an empty field, obvious dev
// placeholder, or already-expanded env reference). Deliberately
// conservative to keep false-positive noise low — a WARNING that
// over-fires on "CHANGE_ME" trains operators to ignore the check.
func LooksLikeRawSecret(s string) bool {
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
	lower := strings.ToLower(trim)
	for _, p := range placeholders {
		if strings.Contains(lower, p) {
			return false
		}
	}
	return len(trim) >= minRawSecretLen
}

// refusalCheckName is the doctor check whose finding the assistant's refusal
// cites. Design §7.3: the check's NAME is an implementation detail — the
// control is "the doctor finding that policy-violating secrets exist in
// config" — but the refusal quotes it so the operator can go straight to
// the doctor report that explains the finding.
const refusalCheckName = "config_secret_hygiene"

// Refusal renders findings as the named refusal the configuration assistant
// returns instead of making a model call (design §7.3, test 33): it names
// the finding AND the file so the operator can fix the pre-existing defect
// rather than being told only that the assistant declined. Returns "" for no
// findings. Never includes a secret value — findings carry none.
//
//	config_secret_hygiene: database.password appears to be a raw plaintext secret in /etc/vornik/config.yaml
func Refusal(findings []Finding) string {
	if len(findings) == 0 {
		return ""
	}
	parts := make([]string, 0, len(findings))
	for _, f := range findings {
		switch {
		case f.Kind == KindRawSecret && f.File != "":
			parts = append(parts, fmt.Sprintf("%s appears to be a raw plaintext secret in %s", f.Key, f.File))
		case f.Kind == KindRawSecret:
			parts = append(parts, fmt.Sprintf("%s appears to be a raw plaintext secret", f.Key))
		default:
			parts = append(parts, f.Detail)
		}
	}
	return refusalCheckName + ": " + strings.Join(parts, "; ")
}

// ScanText runs the raw-secret heuristic over free text — a unified diff, a
// YAML fragment, a markdown question — line by line. It is the screen the
// assistant's judge and consult calls run before anything leaves the box
// (design §7.3: "the judge call by looksLikeRawSecret on the diff"). A line
// is a finding when it has the shape `key: value`, the key name is
// secret-bearing by the same convention the config-dump redactor uses
// (IsSecretBearingName), and the value LooksLikeRawSecret.
//
// Diff markers (a leading `+`/`-`) and a YAML list dash are stripped before
// the key is read, so a `+    password: "…"` diff line screens the same as
// the YAML line it adds. Keys ending in `_file` or `_env` name WHERE a secret
// lives (a path, an env-var name), not the material itself, and are skipped —
// the same distinction Evaluate's `_file` sibling exemption draws.
//
// Findings never carry the value: Detail names the key and the line number,
// because the finding's own text is what gets logged and shown.
func ScanText(s string) []Finding {
	var out []Finding
	for i, line := range strings.Split(s, "\n") {
		key, value, ok := splitKeyValueLine(line)
		if !ok {
			continue
		}
		if !IsSecretBearingName(key) || isSecretLocationName(key) {
			continue
		}
		if isPublicSourceURLField(key) && !looksCredentialBearingURL(value) {
			continue
		}
		if !LooksLikeRawSecret(value) {
			continue
		}
		out = append(out, Finding{
			Kind:   KindRawSecret,
			Key:    key,
			Detail: fmt.Sprintf("line %d: %s appears to be a raw plaintext secret (%d chars)", i+1, key, len(strings.TrimSpace(value))),
		})
	}
	return out
}

// splitKeyValueLine extracts `key: value` from one line of YAML, markdown
// or a unified diff. Returns ok=false for lines with no such shape (prose,
// section headers, values that are themselves nested maps).
func splitKeyValueLine(line string) (key, value string, ok bool) {
	t := strings.TrimSpace(line)
	// Unified-diff marker: one leading '+' or '-' precedes the original
	// line. A bare YAML list dash ("- key: v") is stripped by the same
	// branch; a diff-added list item ("+- key: v") loses the marker here and
	// the dash below. Diff headers ("--- a/x", "+++ b/x") have no `key: `
	// shape left after stripping and fall out at the colon check.
	if len(t) > 0 && (t[0] == '+' || t[0] == '-') {
		t = strings.TrimSpace(t[1:])
	}
	// YAML list item: "- key: value".
	t = strings.TrimSpace(strings.TrimPrefix(t, "- "))
	if t == "" || strings.HasPrefix(t, "#") {
		return "", "", false
	}
	idx := strings.Index(t, ":")
	if idx <= 0 {
		return "", "", false
	}
	// A colon only counts as the key/value separator when followed by
	// whitespace or end of line — `https://…` in a value must not split.
	if idx+1 < len(t) && t[idx+1] != ' ' && t[idx+1] != '\t' {
		return "", "", false
	}
	key = strings.TrimSpace(t[:idx])
	if key == "" || strings.ContainsAny(key, " \t{}[]") {
		return "", "", false
	}
	value = strings.TrimSpace(t[idx+1:])
	// Strip one layer of matching quotes so `password: "…"` is screened on
	// the material, not on the quote characters.
	if len(value) >= 2 {
		if q := value[0]; (q == '"' || q == '\'') && value[len(value)-1] == q {
			value = value[1 : len(value)-1]
		}
	}
	// Strip a trailing YAML/markdown comment so `# rotated 2026-01` doesn't
	// pad a short dev password past the length floor.
	if c := strings.Index(value, " #"); c >= 0 {
		value = strings.TrimSpace(value[:c])
	}
	return key, value, true
}

// isSecretLocationName reports whether a key names where a secret lives
// (a file path, an env-var name) rather than the secret itself.
func isSecretLocationName(key string) bool {
	lower := strings.ToLower(key)
	return strings.HasSuffix(lower, "_file") || strings.HasSuffix(lower, "_env")
}

// isPublicSourceURLField carves out plain URL-bearing source lists from
// ScanText. Config redaction must hide broad URL fields because endpoint URLs
// sometimes carry tokens, but the assistant's egress gate cannot treat every
// public source URL in project guidance as a pasted secret.
func isPublicSourceURLField(key string) bool {
	collapsed := strings.ReplaceAll(strings.ToLower(key), "_", "")
	switch collapsed {
	case "url", "urls", "blockedurl", "blockedurls":
		return true
	default:
		return false
	}
}

func looksCredentialBearingURL(value string) bool {
	u, err := url.Parse(strings.TrimSpace(value))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	if u.User != nil {
		return true
	}
	for key, vals := range u.Query() {
		if !IsSecretBearingName(key) || isSecretLocationName(key) {
			continue
		}
		for _, v := range vals {
			if LooksLikeRawSecret(v) {
				return true
			}
		}
	}
	return false
}

// secretKeyTokens are substrings that, if present in a field's key
// (with underscores stripped and lowercased), mark the field as
// secret-bearing. Keys get their underscores removed before matching so
// snake_case (bot_token) and Go field names (BotToken → bottoken after
// tolower) both hit. Shared with internal/secrets.RedactConfig — extend
// this list, never fork it.
var secretKeyTokens = []string{
	"password",
	"apikey", // covers api_key, APIKey, api_keys
	"secret",
	"bottoken", // covers bot_token, BotToken
	"oauth",
	"credential",
	// D1 (audit 2026-06-10): expandEnvPlaceholders substitutes REAL
	// secret values into token/url/dsn-shaped fields before the config
	// dump is marshalled. These tokens close that leak. The url/dsn/
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

// nonSecretExactKeys are exact lowercased keys that superficially
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
// over-redact. Matched against the lowercased key BEFORE the
// underscore-collapse step, so entries here are the exact lowercased
// marshalled key (Go field name when there's no json tag, snake_case
// when there is). Keep this list tight — over-listing re-opens a leak.
var d1NonSecretExactKeys = map[string]bool{
	"external_base_url": true,
	"webuibaseurl":      true,
	"endpoint":          true,
}

// IsSecretBearingName reports whether a config key NAME denotes a
// secret-bearing field by the convention internal/secrets.RedactConfig has
// always used for the config dump: the lowercased key, with underscores
// stripped, contains one of the secret tokens ("password", "api_key",
// "token", "secret", "bot_token", ...), and is not on the short exact-key
// carve-out lists (max_tokens, external_base_url, ...). Deliberately
// conservative in the other direction from LooksLikeRawSecret: any future
// secret-bearing field that uses one of these obvious names is covered
// automatically without a coordinated code change.
func IsSecretBearingName(name string) bool {
	lowered := strings.ToLower(name)
	if nonSecretExactKeys[lowered] || d1NonSecretExactKeys[lowered] {
		return false
	}
	collapsed := strings.ReplaceAll(lowered, "_", "")
	for _, tok := range secretKeyTokens {
		if strings.Contains(collapsed, tok) {
			return true
		}
	}
	return false
}
