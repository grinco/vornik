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
	assert.Equal(t, "OK", got.Status)
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
