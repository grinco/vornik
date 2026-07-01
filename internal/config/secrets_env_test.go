package config

import (
	"os"
	"path/filepath"
	"testing"
)

// writeSecretsEnv creates <dir>/secrets/<name> with the given content,
// returning the secrets dir's parent (the config dir) so callers can place
// a config.yaml alongside it.
func writeSecretsEnv(t *testing.T, configDir, name, content string) {
	t.Helper()
	secretsDir := filepath.Join(configDir, "secrets")
	if err := os.MkdirAll(secretsDir, 0o700); err != nil {
		t.Fatalf("mkdir secrets: %v", err)
	}
	if err := os.WriteFile(filepath.Join(secretsDir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// TestParseEnvFile_Unexported exercises the internal parseEnvFile directly,
// covering edge cases (spaced keys, lines without '=', empty keys) that the
// exported ParseEnvFile pass-through inherits.
func TestParseEnvFile_Unexported(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chat.env")
	content := "# a comment\n" +
		"\n" +
		"VORNIK_CHAT_API_KEY=sk-plain\n" +
		"export EXPORTED_KEY=exported-value\n" +
		"QUOTED_DOUBLE=\"with spaces\"\n" +
		"QUOTED_SINGLE='single'\n" +
		"  SPACED_KEY = spaced-val \n" +
		"NO_EQUALS_LINE\n" +
		"=emptykey\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := parseEnvFile(path)
	want := map[string]string{
		"VORNIK_CHAT_API_KEY": "sk-plain",
		"EXPORTED_KEY":        "exported-value",
		"QUOTED_DOUBLE":       "with spaces",
		"QUOTED_SINGLE":       "single",
		"SPACED_KEY":          "spaced-val",
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %d keys, want %d: %#v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("key %q = %q, want %q", k, got[k], v)
		}
	}
}

// TestSourceSecretsEnvFiles_FillsEmpty verifies a key that is unset/empty in
// the environment is populated from a secrets/*.env file.
func TestSourceSecretsEnvFiles_FillsEmpty(t *testing.T) {
	dir := t.TempDir()
	writeSecretsEnv(t, dir, "chat.env", "VORNIK_TEST_FILL=from-file\n")
	t.Setenv("VORNIK_TEST_FILL", "") // present but empty → treated as unset

	sourceSecretsEnvFiles(filepath.Join(dir, "config.yaml"))

	if got := os.Getenv("VORNIK_TEST_FILL"); got != "from-file" {
		t.Fatalf("VORNIK_TEST_FILL = %q, want %q", got, "from-file")
	}
}

// TestSourceSecretsEnvFiles_ExplicitWins verifies an explicit non-empty
// environment value is NOT overwritten by the secrets file (deployment env
// wins — systemd EnvironmentFile / compose environment / operator export).
func TestSourceSecretsEnvFiles_ExplicitWins(t *testing.T) {
	dir := t.TempDir()
	writeSecretsEnv(t, dir, "chat.env", "VORNIK_TEST_EXPLICIT=from-file\n")
	t.Setenv("VORNIK_TEST_EXPLICIT", "from-env")

	sourceSecretsEnvFiles(filepath.Join(dir, "config.yaml"))

	if got := os.Getenv("VORNIK_TEST_EXPLICIT"); got != "from-env" {
		t.Fatalf("VORNIK_TEST_EXPLICIT = %q, want explicit env to win (%q)", got, "from-env")
	}
}

// TestLoadFromPath_ResolvesChatKeyFromSecretsEnv is the regression test for
// the onboarding "wizard 503 even after restart" incident: the chat API key
// written to secrets/chat.env must resolve the ${VORNIK_CHAT_API_KEY}
// placeholder in config.yaml on load, with no deployment env wiring.
func TestLoadFromPath_ResolvesChatKeyFromSecretsEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VORNIK_CHAT_API_KEY", "") // emulate compose's `${VORNIK_CHAT_API_KEY:-}` empty default
	writeSecretsEnv(t, dir, "chat.env", "VORNIK_CHAT_API_KEY=sk-onboarded\n")

	configYAML := "api:\n" +
		"  auth_enabled: false\n" +
		"chat:\n" +
		"  enabled: true\n" +
		"  provider: http\n" +
		"  endpoint: https://example.test/v1\n" +
		"  model: test-model\n" +
		"  api_key: \"${VORNIK_CHAT_API_KEY}\"\n"
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(configYAML), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadFromPath(cfgPath)
	if err != nil {
		t.Fatalf("LoadFromPath: %v", err)
	}
	if cfg.Chat.APIKey != "sk-onboarded" {
		t.Fatalf("chat.api_key = %q, want %q (secrets/chat.env not sourced)", cfg.Chat.APIKey, "sk-onboarded")
	}
}

func TestParseEnvFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.env")
	body := "# comment\nexport FOO=bar\nBAZ=\"qu ux\"\nEMPTY=\n\nQUOTED='v'\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got := ParseEnvFile(p)
	if got["FOO"] != "bar" {
		t.Errorf("FOO=%q", got["FOO"])
	}
	if got["BAZ"] != "qu ux" {
		t.Errorf("BAZ=%q", got["BAZ"])
	}
	if got["QUOTED"] != "v" {
		t.Errorf("QUOTED=%q", got["QUOTED"])
	}
	if _, ok := got["EMPTY"]; !ok {
		t.Errorf("EMPTY should be present (empty value)")
	}
	// Missing file → empty map, no panic.
	if len(ParseEnvFile(filepath.Join(dir, "nope.env"))) != 0 {
		t.Errorf("missing file should yield empty map")
	}
}
