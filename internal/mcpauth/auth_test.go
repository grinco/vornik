package mcpauth

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEffectiveMode_ZeroValueIsNone(t *testing.T) {
	// The zero value must be byte-for-byte today's unauthenticated
	// behaviour: every existing mcp.servers entry has no auth block.
	assert.Equal(t, ModeNone, Auth{}.EffectiveMode())
	assert.True(t, Auth{}.IsZero())
	assert.False(t, Auth{Mode: ModeStatic}.IsZero())
}

func TestParseSecretRef(t *testing.T) {
	for _, tc := range []struct {
		in     string
		name   string
		wantOK bool
	}{
		{"secret://n8n_mcp_token", "n8n_mcp_token", true},
		{"secret://A-b.c_1", "A-b.c_1", true},
		{"secret://", "", false},
		{"", "", false},
		{"n8n_mcp_token", "", false},             // bare literal
		{"${N8N_TOKEN}", "", false},              // legacy placeholder
		{"secret://tok en", "", false},           // whitespace
		{"secret://../../etc/passwd", "", false}, // path-ish
		{"SECRET://tok", "", false},              // scheme is case-sensitive
		{"secret://tok\nX-Evil: 1", "", false},   // header smuggling
		{" secret://tok", "tok", true},           // surrounding space tolerated
		{"secret://tok ", "tok", true},           //
	} {
		name, ok := ParseSecretRef(tc.in)
		assert.Equal(t, tc.wantOK, ok, "ParseSecretRef(%q) ok", tc.in)
		assert.Equal(t, tc.name, name, "ParseSecretRef(%q) name", tc.in)
	}
}

func TestSecretRefs_EnumeratesEveryCredentialField(t *testing.T) {
	// SecretRefs feeds the permissions.secrets allowlist check, so a
	// credential field it forgets is a credential that escapes the grant.
	a := Auth{
		Mode:             ModeOAuth,
		ClientID:         "1234.5678",
		ClientSecretFrom: "secret://slack_client_secret",
		ValueFrom:        "secret://static_token",
		EnvFrom: map[string]string{
			"REDDIT_CLIENT_ID":     "secret://reddit_id",
			"REDDIT_CLIENT_SECRET": "secret://reddit_secret",
		},
	}
	assert.ElementsMatch(t,
		[]string{"slack_client_secret", "static_token", "reddit_id", "reddit_secret"},
		a.SecretRefs())
}

func TestSecretRefs_IgnoresNonRefs(t *testing.T) {
	// A literal is a validation error elsewhere; SecretRefs must not
	// report it as a granted name (that would let an invalid config
	// pass an allowlist check by naming a secret it never resolves).
	a := Auth{Mode: ModeStatic, ValueFrom: "literal-token"}
	assert.Empty(t, a.SecretRefs())
}

func TestValidateSecretGrants(t *testing.T) {
	a := Auth{Mode: ModeStatic, ValueFrom: "secret://n8n_token"}

	require.NoError(t, a.ValidateSecretGrants([]string{"other", "n8n_token"}))

	err := a.ValidateSecretGrants([]string{"other"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "n8n_token")
	assert.Contains(t, err.Error(), "permissions.secrets")

	// Deny by default: an empty grant list grants nothing. This is the
	// first place permissions.secrets is actually enforced, so it must
	// not inherit the "empty means all" convention used by allowedTools.
	require.Error(t, a.ValidateSecretGrants(nil))
}
