package secrethygiene

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The LooksLikeRawSecret / IsEnvSourcedRaw cases below are the unit tests
// that lived in internal/api (doctor_config_secret_hygiene_test.go and
// helpers_coverage_test.go) before the 2026-09-13 extraction. They moved
// with the code; the api package keeps its DoctorCheck-level tests.

func TestLooksLikeRawSecret(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{name: "empty", in: "   ", want: false},
		{name: "env placeholder", in: "${DB_PASSWORD}", want: false},
		{name: "known placeholder marker", in: "CHANGE_ME-super-secret", want: false},
		{name: "too short", in: "short-password", want: false},
		{name: "long raw secret", in: "this_is_a_very_long_raw_secret_12345", want: true},
		{name: "env var dollar only", in: "$ORACLE_HOME", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, LooksLikeRawSecret(tt.in))
		})
	}
}

// TestIsEnvSourcedRaw_SkipsTemplateRefs — the secret-hygiene
// doctor needs to treat `${VAR}` references as non-secrets
// (their actual value is supplied by the environment at start).
// Empty strings also pass (nothing to leak). Anything else is
// candidate-secret material.
func TestIsEnvSourcedRaw_SkipsTemplateRefs(t *testing.T) {
	cases := map[string]bool{
		"":              true,
		"   ":           true,
		"${SECRET}":     true,
		"${X}":          true,
		"sk-real-token": false,
		"$SECRET":       false, // un-braced form is NOT considered safe by this check
		"prefix${X}":    false, // partial template; doctor flags
	}
	for in, want := range cases {
		if got := IsEnvSourcedRaw(in); got != want {
			t.Errorf("IsEnvSourcedRaw(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestLooksLikeRawSecret_RejectsPlaceholders — the doctor's
// false-positive defence: things like "CHANGE_ME" / "<your-key>"
// must NOT trip the secret-leak warning, otherwise operators
// learn to ignore the check.
func TestLooksLikeRawSecret_RejectsPlaceholders(t *testing.T) {
	for _, placeholder := range []string{
		"", "   ",
		"${ENV_VAR}",
		"CHANGE_ME",
		"changeme",
		"YOUR_KEY_HERE",
		"<your-token>",
		"<replace-me>",
		"dev-key-123",
	} {
		if LooksLikeRawSecret(placeholder) {
			t.Errorf("LooksLikeRawSecret(%q) flagged a placeholder", placeholder)
		}
	}
}

// TestLooksLikeRawSecret_FlagsRealKeys — the positive path. Real
// API keys and tokens (~32+ chars of mixed characters) MUST trip
// the heuristic.
func TestLooksLikeRawSecret_FlagsRealKeys(t *testing.T) {
	for _, candidate := range []string{
		"sk-1234567890abcdefghijklmnopqrstuv",
		"AKIAIOSFODNN7EXAMPLE",                     // looks like an AWS key
		"ghp_1234567890abcdefghijklmnopqrstuvwxyz", // looks like a GitHub PAT
	} {
		if !LooksLikeRawSecret(candidate) {
			t.Errorf("LooksLikeRawSecret(%q) missed a real-shape secret", candidate)
		}
	}
}

// TestLooksLikeRawSecret_EveryPlaceholderMarker pins the whole
// placeholder list: each marker, embedded in an otherwise secret-shaped
// 40-char value, must suppress the finding. A marker dropped from the
// list surfaces here by name.
func TestLooksLikeRawSecret_EveryPlaceholderMarker(t *testing.T) {
	require.NotEmpty(t, placeholders)
	for _, marker := range placeholders {
		padded := "abcdefghij" + marker + "0123456789abcdefghij"
		require.GreaterOrEqual(t, len(padded), minRawSecretLen)
		if LooksLikeRawSecret(padded) {
			t.Errorf("placeholder marker %q did not suppress the finding", marker)
		}
		// Case-insensitive: the marker upper-cased must suppress too.
		if LooksLikeRawSecret(strings.ToUpper(padded)) {
			t.Errorf("placeholder marker %q (upper-cased) did not suppress the finding", marker)
		}
	}
}

// TestLooksLikeRawSecret_SixteenCharFloor pins the exact boundary: 15
// chars is not a secret, 16 is. Surrounding whitespace does not count
// toward the length.
func TestLooksLikeRawSecret_SixteenCharFloor(t *testing.T) {
	assert.Equal(t, 16, minRawSecretLen)
	fifteen := "abcdefghijklmno"
	sixteen := "abcdefghijklmnop"
	require.Len(t, fifteen, 15)
	require.Len(t, sixteen, 16)
	assert.False(t, LooksLikeRawSecret(fifteen), "15 chars must be under the floor")
	assert.True(t, LooksLikeRawSecret(sixteen), "16 chars is the floor")
	assert.False(t, LooksLikeRawSecret("  "+fifteen+"  "), "whitespace must not pad a value past the floor")
}

// TestLooksLikeRawSecret_EnvRefExempt: a braced ${VAR} of any length is
// never a finding (an unset var is a runtime problem, not a hygiene one);
// the un-braced $VAR is only exempt by being short.
func TestLooksLikeRawSecret_EnvRefExempt(t *testing.T) {
	assert.False(t, LooksLikeRawSecret("${A_VERY_LONG_ENVIRONMENT_VARIABLE_NAME_HERE}"))
	assert.False(t, LooksLikeRawSecret("  ${A_VERY_LONG_ENVIRONMENT_VARIABLE_NAME_HERE}  "))
	assert.True(t, LooksLikeRawSecret("$A_VERY_LONG_ENVIRONMENT_VARIABLE_NAME_HERE"),
		"un-braced $VAR is not the exempt form and, at this length, is a finding")
	assert.True(t, LooksLikeRawSecret("prefix-${VAR}-and-more-text-here"),
		"a partial template is not exempt")
}

func TestLookupYAMLPath(t *testing.T) {
	doc := map[string]interface{}{
		"database": map[string]interface{}{
			"password": "hunter2-hunter2-hunter2",
			"port":     5432,
		},
		"legacy": map[interface{}]interface{}{
			"token": "legacy-shape-token-value",
		},
	}
	got, ok := LookupYAMLPath(doc, "database.password")
	assert.True(t, ok)
	assert.Equal(t, "hunter2-hunter2-hunter2", got)

	got, ok = LookupYAMLPath(doc, "legacy.token")
	assert.True(t, ok, "map[any]any nesting must be walked too")
	assert.Equal(t, "legacy-shape-token-value", got)

	_, ok = LookupYAMLPath(doc, "database.port")
	assert.False(t, ok, "a non-string leaf is not a hit")
	_, ok = LookupYAMLPath(doc, "database.missing")
	assert.False(t, ok)
	_, ok = LookupYAMLPath(doc, "database.password.deeper")
	assert.False(t, ok, "walking through a scalar is not a hit")
}

func TestSecretFieldPaths_CoversTheKnownFields(t *testing.T) {
	for _, want := range []string{
		"database.password",
		"chat.api_key",
		"chat.router.http.api_key",
		"runtime.agent_llm.api_key",
		"telegram.bot_token",
		"memory.embedding_api_key",
		"auth.providers.github.client_secret",
		"auth.providers.github.client_secret_file",
	} {
		assert.Contains(t, SecretFieldPaths, want)
	}
}

func writeConfig(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(p, []byte(body), mode))
	return p
}

func TestEvaluate_NilSnapshotIsNotEvaluable(t *testing.T) {
	findings, ok := Evaluate(writeConfig(t, "test: data", 0o644), nil)
	assert.False(t, ok)
	assert.Nil(t, findings)
}

func TestEvaluate_EmptySnapshotIsEvaluableAndClean(t *testing.T) {
	findings, ok := Evaluate(writeConfig(t, "test: data", 0o600), map[string]string{})
	assert.True(t, ok)
	assert.Empty(t, findings)
}

func TestEvaluate_PermissionsFinding(t *testing.T) {
	p := writeConfig(t, "test: data", 0o644)
	findings, ok := Evaluate(p, map[string]string{})
	require.True(t, ok)
	require.Len(t, findings, 1)
	assert.Equal(t, KindPermissions, findings[0].Kind)
	assert.Equal(t, p, findings[0].File)
	assert.Equal(t, "", findings[0].Key)
	assert.Equal(t, "config.yaml at "+p+" has mode 0644 (> 0640); recommend `chmod 600 "+p+"`", findings[0].Detail)
}

func TestEvaluate_MissingFileSkipsPermissionsButStillLintsSnapshot(t *testing.T) {
	findings, ok := Evaluate("/nonexistent/path/to/config.yaml", map[string]string{
		"db.password": "SomeSecretValue123456",
	})
	require.True(t, ok)
	require.Len(t, findings, 1)
	assert.Equal(t, KindRawSecret, findings[0].Kind)
	assert.Equal(t, "db.password", findings[0].Key)
}

func TestEvaluate_EmptyPathStillLintsSnapshot(t *testing.T) {
	findings, ok := Evaluate("", map[string]string{"db.password": "SomeSecretValue123456"})
	require.True(t, ok)
	require.Len(t, findings, 1)
	assert.Equal(t, "", findings[0].File)
}

// TestEvaluate_StableOrdering: permissions first, then raw-secret findings
// sorted by key — so a report is byte-identical between runs.
func TestEvaluate_StableOrdering(t *testing.T) {
	p := writeConfig(t, "test: data", 0o644)
	findings, ok := Evaluate(p, map[string]string{
		"z.password":    "ThisIsALongRealSecretValue123",
		"a.token":       "AnotherLongSecretValue456789",
		"m.placeholder": "change_me",
	})
	require.True(t, ok)
	require.Len(t, findings, 3)
	assert.Equal(t, KindPermissions, findings[0].Kind)
	assert.Equal(t, "a.token", findings[1].Key)
	assert.Equal(t, "z.password", findings[2].Key)
	assert.Equal(t, "a.token appears to be a raw plaintext secret (28 chars); recommend moving to ${ENV_VAR} and setting the variable in the systemd unit", findings[1].Detail)
	for _, f := range findings[1:] {
		assert.Equal(t, KindRawSecret, f.Kind)
		assert.Equal(t, p, f.File)
	}
}

// TestEvaluate_EnvSourcedRawIsExempt pins the 2026-05-08 fix: the
// snapshot holds the EXPANDED value, so only re-reading the raw YAML can
// tell a ${VAR} author from a paste. Same expanded value, opposite verdicts.
func TestEvaluate_EnvSourcedRawIsExempt(t *testing.T) {
	expanded := "sk-1234567890abcdefghijklmnopqrstuv"
	snapshot := map[string]string{"chat.api_key": expanded}

	envSourced := writeConfig(t, "chat:\n  api_key: ${CHAT_API_KEY}\n", 0o600)
	findings, ok := Evaluate(envSourced, snapshot)
	require.True(t, ok)
	assert.Empty(t, findings, "a ${VAR}-authored field must not be a finding")

	pasted := writeConfig(t, "chat:\n  api_key: "+expanded+"\n", 0o600)
	findings, ok = Evaluate(pasted, snapshot)
	require.True(t, ok)
	require.Len(t, findings, 1, "the same expanded value pasted literally IS a finding")
	assert.Equal(t, "chat.api_key", findings[0].Key)
}

// TestEvaluate_FileSiblingIsExempt pins the 2026-07-25 fix: a
// `<field>_file:` sibling means the loader resolved a 0600 file into the
// snapshotted field, which is the strongest option and never a finding.
func TestEvaluate_FileSiblingIsExempt(t *testing.T) {
	secretFile := filepath.Join(t.TempDir(), "oidc-github")
	require.NoError(t, os.WriteFile(secretFile, []byte("0123456789abcdef0123456789abcdef01234567"), 0o600))
	snapshot := map[string]string{
		"auth.providers.github.client_secret": "0123456789abcdef0123456789abcdef01234567",
	}

	fileSourced := writeConfig(t, "auth:\n  providers:\n    github:\n      client_id: \"Ov23xxxx\"\n      client_secret_file: \""+secretFile+"\"\n", 0o600)
	findings, ok := Evaluate(fileSourced, snapshot)
	require.True(t, ok)
	assert.Empty(t, findings, "a file-sourced secret must not be a finding")

	inline := writeConfig(t, "auth:\n  providers:\n    github:\n      client_secret: \"0123456789abcdef0123456789abcdef01234567\"\n", 0o600)
	findings, ok = Evaluate(inline, snapshot)
	require.True(t, ok)
	require.Len(t, findings, 1, "an inline pasted secret must still be flagged")
}

// TestEvaluate_UnparseableYAMLFallsBackToSnapshot: garbage on disk means
// no raw values to exempt by, so the post-expansion heuristic decides —
// the pre-extraction behaviour.
func TestEvaluate_UnparseableYAMLFallsBackToSnapshot(t *testing.T) {
	p := writeConfig(t, "::: not yaml [", 0o600)
	findings, ok := Evaluate(p, map[string]string{"chat.api_key": "sk-1234567890abcdefghijklmnopqrstuv"})
	require.True(t, ok)
	require.Len(t, findings, 1)
}

// TestEvaluate_IsFresh: the gate must see the state of the file NOW, not
// a cached verdict (design §7.3). Paste a secret between two calls.
func TestEvaluate_IsFresh(t *testing.T) {
	p := writeConfig(t, "chat:\n  api_key: ${CHAT_API_KEY}\n", 0o600)
	snapshot := map[string]string{"chat.api_key": "sk-1234567890abcdefghijklmnopqrstuv"}
	findings, _ := Evaluate(p, snapshot)
	require.Empty(t, findings)
	require.NoError(t, os.WriteFile(p, []byte("chat:\n  api_key: sk-1234567890abcdefghijklmnopqrstuv\n"), 0o600))
	findings, _ = Evaluate(p, snapshot)
	require.Len(t, findings, 1, "a paste since the last evaluation must be seen")
}

func TestRefusal(t *testing.T) {
	assert.Equal(t, "", Refusal(nil))
	assert.Equal(t, "", Refusal([]Finding{}))

	one := Refusal([]Finding{{Kind: KindRawSecret, Key: "database.password", File: "/etc/vornik/config.yaml", Detail: "database.password appears to be a raw plaintext secret (32 chars); recommend moving to ${ENV_VAR} and setting the variable in the systemd unit"}})
	assert.Equal(t, "config_secret_hygiene: database.password appears to be a raw plaintext secret in /etc/vornik/config.yaml", one)

	// Real findings from Evaluate, end to end: the refusal names the finding
	// and the file (design test 33), and never the value.
	p := writeConfig(t, "test: data", 0o644)
	findings, ok := Evaluate(p, map[string]string{"chat.api_key": "sk-1234567890abcdefghijklmnopqrstuv"})
	require.True(t, ok)
	require.Len(t, findings, 2)
	got := Refusal(findings)
	assert.True(t, strings.HasPrefix(got, "config_secret_hygiene: "), got)
	assert.Contains(t, got, "config.yaml at "+p+" has mode 0644")
	assert.Contains(t, got, "chat.api_key appears to be a raw plaintext secret in "+p)
	assert.NotContains(t, got, "sk-1234567890abcdefghijklmnopqrstuv")

	// A ScanText finding has no file; the refusal still names the key.
	noFile := Refusal([]Finding{{Kind: KindRawSecret, Key: "password"}})
	assert.Equal(t, "config_secret_hygiene: password appears to be a raw plaintext secret", noFile)
}

func TestIsSecretBearingName(t *testing.T) {
	// Mirrors internal/secrets.TestRedactConfig_MasksSecretShapedKeys so the
	// two surfaces cannot drift: what the dump redacts, the diff screen sees.
	cases := map[string]bool{
		"password":           true,
		"api_key":            true,
		"APIKey":             true,
		"bot_token":          true,
		"BotToken":           true,
		"client_secret":      true,
		"oauth_token":        true,
		"credential_blob":    true,
		"dsn":                true,
		"connection_string":  true,
		"private_key":        true,
		"GITHUB_TOKEN":       true,
		"database_url":       true,
		"max_tokens":         false,
		"max_history_tokens": false,
		"thinking_budget":    false,
		"max_per_role":       false,
		"external_base_url":  false,
		"WebUIBaseURL":       false,
		"endpoint":           false,
		"model":              false,
		"name":               false,
		"":                   false,
	}
	for name, want := range cases {
		assert.Equal(t, want, IsSecretBearingName(name), "IsSecretBearingName(%q)", name)
	}
}

func TestScanText(t *testing.T) {
	secret := "sk-1234567890abcdefghijklmnopqrstuv"
	tests := []struct {
		name     string
		in       string
		wantKeys []string
	}{
		{name: "empty", in: "", wantKeys: nil},
		{name: "prose", in: "please rotate the database password soon", wantKeys: nil},
		{name: "yaml pasted secret", in: "database:\n  password: " + secret + "\n", wantKeys: []string{"password"}},
		{name: "quoted value", in: "  api_key: \"" + secret + "\"\n", wantKeys: []string{"api_key"}},
		{name: "single-quoted value", in: "  api_key: '" + secret + "'\n", wantKeys: []string{"api_key"}},
		{name: "env ref is not a finding", in: "  api_key: ${CHAT_API_KEY}\n", wantKeys: nil},
		{name: "placeholder is not a finding", in: "  api_key: CHANGE_ME_before_you_deploy_this\n", wantKeys: nil},
		{name: "short value is not a finding", in: "  password: hunter2\n", wantKeys: nil},
		{name: "non-secret key with long value", in: "  model: nvidia.nemotron-nano-9b-v2-with-a-long-name\n", wantKeys: nil},
		{name: "max_tokens carve-out", in: "  max_tokens: 4096000000000000000\n", wantKeys: nil},
		{name: "public source url is not a finding", in: "  url: https://example.com/news/world/rss\n", wantKeys: nil},
		{name: "blocked urls schema hint is not a finding", in: "  blocked_urls: [{url, reason, permanent}, ...]\n", wantKeys: nil},
		{name: "token-bearing source url remains a finding", in: "  url: https://example.com/feed?api_key=" + secret + "\n", wantKeys: []string{"url"}},
		{name: "diff added line", in: "+    bot_token: " + secret + "\n", wantKeys: []string{"bot_token"}},
		{name: "diff removed line", in: "-    bot_token: " + secret + "\n", wantKeys: []string{"bot_token"}},
		{name: "diff context line", in: "     bot_token: " + secret + "\n", wantKeys: []string{"bot_token"}},
		{name: "yaml list item", in: "- password: " + secret + "\n", wantKeys: []string{"password"}},
		{name: "diff added list item", in: "+- client_secret: " + secret + "\n", wantKeys: []string{"client_secret"}},
		{name: "diff headers ignored", in: "--- a/config.yaml\n+++ b/config.yaml\n@@ -1,3 +1,3 @@\n", wantKeys: nil},
		{name: "url in value does not split on scheme colon", in: "  database_url: postgres://user:" + secret + "@host/db\n", wantKeys: []string{"database_url"}},
		{name: "_file key names a location", in: "  client_secret_file: /var/lib/vornik/secrets/github-oauth-client\n", wantKeys: nil},
		{name: "_env key names a variable", in: "  imap_password_env: VORNIK_IMAP_PASSWORD_LONG_NAME\n", wantKeys: nil},
		{name: "trailing comment does not pad", in: "  password: hunter2 # rotated 2026-01-01 by ops\n", wantKeys: nil},
		{name: "comment line ignored", in: "# password: " + secret + "\n", wantKeys: nil},
		{name: "nested map key has no value", in: "  secrets:\n    child: x\n", wantKeys: nil},
		{name: "markdown bullet", in: "* api_key: " + secret, wantKeys: nil},
		{name: "two findings keep order", in: "a:\n  password: " + secret + "\nb:\n  api_key: " + secret + "\n", wantKeys: []string{"password", "api_key"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ScanText(tt.in)
			var keys []string
			for _, f := range got {
				keys = append(keys, f.Key)
				assert.Equal(t, KindRawSecret, f.Kind)
				assert.Equal(t, "", f.File)
				assert.NotContains(t, f.Detail, secret, "a finding must never carry the value")
			}
			assert.Equal(t, tt.wantKeys, keys)
		})
	}
}

// TestScanText_DetailNamesLineAndKey: the finding is what gets logged and
// shown, so it carries the line number and key — never the material.
func TestScanText_DetailNamesLineAndKey(t *testing.T) {
	got := ScanText("chat:\n  model: x\n  api_key: sk-1234567890abcdefghijklmnopqrstuv\n")
	require.Len(t, got, 1)
	assert.Equal(t, "line 3: api_key appears to be a raw plaintext secret (35 chars)", got[0].Detail)
}
