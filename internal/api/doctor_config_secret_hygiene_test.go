package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDoctorConfigSecretHygiene_NoSecretFields(t *testing.T) {
	h := &DoctorHandlers{}
	// When secretFields is nil (e.g., in direct handler tests without SetServerConfig),
	// the check should return OK with a skip message.
	got := h.checkConfigSecretHygiene()
	assert.Equal(t, "config_secret_hygiene", got.Name)
	assert.Equal(t, "SKIPPED", got.Status)
	assert.Contains(t, got.Message, "skipping")
}

func TestDoctorConfigSecretHygiene_PermissiveFilePermissions(t *testing.T) {
	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")

	// Create a config file with world-readable permissions (0644 > 0640)
	require := assert.New(t)
	require.NoError(os.WriteFile(configFile, []byte("test: data"), 0o644))

	h := &DoctorHandlers{
		configPath:   configFile,
		secretFields: map[string]string{}, // Empty but non-nil
	}
	got := h.checkConfigSecretHygiene()

	assert.Equal(t, "config_secret_hygiene", got.Name)
	assert.Equal(t, "WARNING", got.Status)
	assert.Len(t, got.Items, 1, "expected one finding for permissive permissions")
	assert.Contains(t, got.Items[0], "has mode 0644")
	assert.Contains(t, got.Items[0], "chmod 600")
}

func TestDoctorConfigSecretHygiene_SecretDetectionAndStableOrdering(t *testing.T) {
	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")
	require := assert.New(t)
	require.NoError(os.WriteFile(configFile, []byte("test: data"), 0o600))

	h := &DoctorHandlers{
		configPath: configFile,
		secretFields: map[string]string{
			"z.password":    "ThisIsALongRealSecretValue123",
			"a.token":       "AnotherLongSecretValue456789",
			"m.placeholder": "change_me",
		},
	}
	got := h.checkConfigSecretHygiene()

	assert.Equal(t, "WARNING", got.Status)
	assert.Len(t, got.Items, 2, "only raw secrets should be flagged")
	assert.True(t, strings.HasPrefix(got.Items[0], "a.token appears to be a raw plaintext secret"), "items should be sorted by key")
	assert.True(t, strings.HasPrefix(got.Items[1], "z.password appears to be a raw plaintext secret"), "items should be sorted by key")
	assert.Contains(t, got.Message, "2 config secret-hygiene finding(s)")
}

func TestDoctorConfigSecretHygiene_MissingConfigFile(t *testing.T) {
	// When configPath is set but the file doesn't exist (os.Stat returns error),
	// the permission check should be skipped gracefully.
	h := &DoctorHandlers{
		configPath:   "/nonexistent/path/to/config.yaml",
		secretFields: map[string]string{"db.password": "SomeSecretValue123456"},
	}
	got := h.checkConfigSecretHygiene()

	// Should still check secrets even when config file doesn't exist
	assert.Equal(t, "config_secret_hygiene", got.Name)
	assert.Contains(t, got.Message, "secret-hygiene finding")
}

func TestDoctorConfigSecretHygiene_AllSecretsSafe(t *testing.T) {
	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")
	require := assert.New(t)
	require.NoError(os.WriteFile(configFile, []byte("test: data"), 0o600))

	h := &DoctorHandlers{
		configPath: configFile,
		secretFields: map[string]string{
			"db.password": "${DB_PASSWORD}",
			"api.key":     "${API_KEY}",
			"token":       "CHANGE_ME_placeholder",
		},
	}
	got := h.checkConfigSecretHygiene()

	assert.Equal(t, "OK", got.Status)
	assert.Contains(t, got.Message, "permissions tight")
	assert.Len(t, got.Items, 0)
}

func TestDoctorConfigSecretHygiene_GitHubClientSecretPath(t *testing.T) {
	// Regression: auth.providers.github.client_secret must be linted by
	// the hygiene check — operators who inline the secret should be steered
	// to client_secret_file.
	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")
	require := assert.New(t)
	require.NoError(os.WriteFile(configFile, []byte("test: data"), 0o600))

	h := &DoctorHandlers{
		configPath: configFile,
		secretFields: map[string]string{
			"auth.providers.github.client_secret": "ghsec_this_is_a_long_raw_github_oauth_secret",
		},
	}
	got := h.checkConfigSecretHygiene()

	assert.Equal(t, "WARNING", got.Status)
	assert.Len(t, got.Items, 1)
	assert.Contains(t, got.Items[0], "auth.providers.github.client_secret")
}

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
			assert.Equal(t, tt.want, looksLikeRawSecret(tt.in))
		})
	}
}

// TestConfigSecretHygiene_FileSourcedSecretIsNotAFinding pins the fix for a
// false positive found on the dev box: the operator externalized the GitHub
// OAuth secret to a 0600 file via client_secret_file — the STRONGEST option the
// loader offers — and the loader resolves that file INTO
// auth.providers.github.client_secret at startup. The check snapshots the
// post-resolution value, saw a 40-char secret-shaped string with no raw
// `client_secret:` key to match against, and reported it as pasted plaintext.
// It punished the most secure configuration (same class as the env-sourced
// false positive the `${VAR}` hop already fixed).
func TestConfigSecretHygiene_FileSourcedSecretIsNotAFinding(t *testing.T) {
	dir := t.TempDir()
	secretFile := filepath.Join(dir, "oidc-github")
	if err := os.WriteFile(secretFile, []byte("0123456789abcdef0123456789abcdef01234567"), 0o600); err != nil {
		t.Fatalf("write secret file: %v", err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := "auth:\n  providers:\n    github:\n      client_id: \"Ov23xxxx\"\n      client_secret_file: \"" + secretFile + "\"\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	h := &DoctorHandlers{
		configPath: cfgPath,
		// Mirrors the loader: client_secret_file has been resolved into the
		// client_secret field by the time the doctor snapshots it.
		secretFields: map[string]string{
			"auth.providers.github.client_secret": "0123456789abcdef0123456789abcdef01234567",
		},
	}
	got := h.checkConfigSecretHygiene()
	if got.Status != "OK" {
		t.Fatalf("a file-sourced secret must not be a finding, got %+v", got)
	}
}

// TestConfigSecretHygiene_InlineSecretStillFlagged is the negative control: an
// actually-pasted secret with no _file sibling must still be reported.
func TestConfigSecretHygiene_InlineSecretStillFlagged(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := "auth:\n  providers:\n    github:\n      client_secret: \"0123456789abcdef0123456789abcdef01234567\"\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	h := &DoctorHandlers{
		configPath: cfgPath,
		secretFields: map[string]string{
			"auth.providers.github.client_secret": "0123456789abcdef0123456789abcdef01234567",
		},
	}
	got := h.checkConfigSecretHygiene()
	if got.Status != "WARNING" {
		t.Fatalf("an inline pasted secret must still be flagged, got %+v", got)
	}
}
