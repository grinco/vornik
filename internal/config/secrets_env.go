package config

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// sourceSecretsEnvFiles loads every `<configDir>/secrets/*.env` file into
// the process environment so that `${VAR}` placeholders in config.yaml
// (and the direct VORNIK_* env overrides) resolve without depending on
// the deployment's own env wiring.
//
// Why this exists: onboarding writes the chat API key to
// `<configDir>/secrets/chat.env` as `VORNIK_CHAT_API_KEY` and puts
// `chat.api_key: "${VORNIK_CHAT_API_KEY}"` in config.yaml. On the systemd
// host deploy that resolves because the unit lists
// `EnvironmentFile=-…/secrets/chat.env`; but on a podman-compose deploy the
// container's env comes from the host's compose `.env`, and nothing sources
// the onboarding-written file — so the placeholder expanded to empty, chat
// init failed, and the project wizard 503'd. Having the daemon source its
// own secrets dir removes that deployment-specific wiring requirement: the
// same chat.env activates chat on every deployment after a restart.
//
// Precedence: a key is only populated when it is currently empty (unset or
// set to ""). An explicit, non-empty value already in the environment —
// from systemd's EnvironmentFile, a compose `environment:` entry, or an
// operator export — always wins. This mirrors what systemd's
// `EnvironmentFile=` already does at process start and makes the call
// idempotent, so it is safe to re-run on every config (re)load.
//
// configPath is the resolved path to config.yaml; the secrets directory is
// its sibling `secrets/` folder (the same location onboarding writes to and
// onboardingSecretsDir derives). A missing secrets dir is not an error.
func sourceSecretsEnvFiles(configPath string) {
	if configPath == "" {
		return
	}
	secretsDir := filepath.Join(filepath.Dir(configPath), "secrets")
	matches, err := filepath.Glob(filepath.Join(secretsDir, "*.env"))
	if err != nil || len(matches) == 0 {
		return
	}
	// Deterministic order so overlapping keys resolve predictably
	// (first file to set a key wins, since later files see it non-empty).
	sort.Strings(matches)
	for _, file := range matches {
		for k, v := range parseEnvFile(file) {
			if os.Getenv(k) == "" {
				_ = os.Setenv(k, v)
			}
		}
	}
}

// ParseEnvFile reads a systemd-style EnvironmentFile / dotenv file into a
// map (KEY=VALUE, optional leading `export `, `#` comments, optional
// surrounding quotes). A missing/unreadable file yields an empty map. This
// is the exported entry point used by `vornik-enterprise migrate-ce`.
func ParseEnvFile(path string) map[string]string {
	return parseEnvFile(path)
}

// parseEnvFile reads a systemd-style EnvironmentFile / dotenv file into a
// map. It accepts `KEY=VALUE` lines, an optional leading `export `, blank
// lines, and `#` comments. Surrounding single or double quotes on the value
// are stripped. Lines without `=` or with an empty key are skipped. Parsing
// is best-effort: an unreadable file yields an empty map rather than an
// error, matching the optional (`-`) semantics of the systemd directive.
func parseEnvFile(path string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		if key == "" {
			continue
		}
		val := strings.TrimSpace(line[eq+1:])
		val = unquoteEnvValue(val)
		out[key] = val
	}
	return out
}

// unquoteEnvValue strips a single matched pair of surrounding single or
// double quotes from an env value. Unquoted or mismatched values are
// returned unchanged.
func unquoteEnvValue(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}
