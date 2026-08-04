package mcpauth

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mapSecrets is a SecretSource over a literal map.
type mapSecrets map[string]string

func (m mapSecrets) Get(name string) (string, bool) {
	v, ok := m[name]
	return v, ok && v != ""
}

func TestResolve_NoneIsEmpty(t *testing.T) {
	for _, a := range []Auth{{}, {Mode: ModeNone}} {
		inj, err := Resolve(a, "streamable-http", mapSecrets{}, Grants{Unrestricted: true})
		require.NoError(t, err)
		assert.True(t, inj.IsEmpty())
		assert.Nil(t, inj.Headers)
		assert.Nil(t, inj.Env)
	}
}

func TestResolve_StaticDefaultsToAuthorizationHeader(t *testing.T) {
	inj, err := Resolve(
		Auth{Mode: ModeStatic, ValueFrom: "secret://n8n_token", ValuePrefix: "Bearer "},
		"streamable-http",
		mapSecrets{"n8n_token": "tok-123"},
		Grants{Allowed: []string{"n8n_token"}},
	)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"Authorization": "Bearer tok-123"}, inj.Headers)
	assert.Empty(t, inj.Env)
}

func TestResolve_StaticHonoursExplicitHeaderAndNoPrefix(t *testing.T) {
	inj, err := Resolve(
		Auth{Mode: ModeStatic, Header: "X-Api-Key", ValueFrom: "secret://k"},
		"sse",
		mapSecrets{"k": "raw-key"},
		Grants{Allowed: []string{"k"}},
	)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"X-Api-Key": "raw-key"}, inj.Headers)
}

func TestResolve_EnvReachesSubprocessEnvOnly(t *testing.T) {
	inj, err := Resolve(
		Auth{Mode: ModeEnv, EnvFrom: map[string]string{
			"REDDIT_CLIENT_ID":     "secret://reddit_id",
			"REDDIT_CLIENT_SECRET": "secret://reddit_secret",
		}},
		"stdio",
		mapSecrets{"reddit_id": "id-1", "reddit_secret": "sec-1"},
		Grants{Allowed: []string{"reddit_id", "reddit_secret"}},
	)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"REDDIT_CLIENT_ID":     "id-1",
		"REDDIT_CLIENT_SECRET": "sec-1",
	}, inj.Env)
	// A stdio credential must never become an HTTP header — there is no
	// request to put it on, and doing so would widen where it can leak.
	assert.Empty(t, inj.Headers)
}

func TestResolve_MissingSecretNamesTheRefNotTheValue(t *testing.T) {
	_, err := Resolve(
		Auth{Mode: ModeStatic, ValueFrom: "secret://n8n_token"},
		"streamable-http",
		mapSecrets{},
		Grants{Allowed: []string{"n8n_token"}},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "n8n_token")
	assert.True(t, errors.Is(err, ErrSecretUnresolved), "want ErrSecretUnresolved, got %v", err)
}

func TestResolve_EmptySecretCountsAsMissing(t *testing.T) {
	// The secret store is env-backed and an empty variable is how "not
	// set" presents there (projectdoctor.EnvSecrets.Has uses != ""). An
	// empty bearer would otherwise be sent as "Bearer " and read as a
	// server-side auth bug.
	_, err := Resolve(
		Auth{Mode: ModeStatic, ValueFrom: "secret://tok"},
		"streamable-http",
		mapSecrets{"tok": ""},
		Grants{Allowed: []string{"tok"}},
	)
	require.ErrorIs(t, err, ErrSecretUnresolved)
}

func TestResolve_EnforcesGrantsAsDefenceInDepth(t *testing.T) {
	// Config validation is the primary gate; this is the second one, so a
	// server reaching the wiring layer through some path that skipped
	// validation still cannot resolve an ungranted secret.
	_, err := Resolve(
		Auth{Mode: ModeStatic, ValueFrom: "secret://other_project_token"},
		"streamable-http",
		mapSecrets{"other_project_token": "tok"},
		Grants{Allowed: []string{"n8n_token"}},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "other_project_token")
}

func TestResolve_UnrestrictedGrantsAreForDaemonScopeOnly(t *testing.T) {
	inj, err := Resolve(
		Auth{Mode: ModeStatic, ValueFrom: "secret://admin_tok"},
		"streamable-http",
		mapSecrets{"admin_tok": "tok"},
		Grants{Unrestricted: true},
	)
	require.NoError(t, err)
	assert.Equal(t, "tok", inj.Headers["Authorization"])
}

func TestResolve_OAuthIsNotWiredYet(t *testing.T) {
	// Step 2 ships the config surface for oauth but none of the flow. The
	// wiring layer must be able to tell "not implemented" apart from a
	// genuine failure so it can log once and leave the server usable
	// (unauthenticated) rather than dropping it.
	inj, err := Resolve(
		Auth{Mode: ModeOAuth, Scopes: []string{"read:jira-work"}},
		"streamable-http",
		mapSecrets{},
		Grants{Unrestricted: true},
	)
	require.ErrorIs(t, err, ErrOAuthNotWired)
	assert.True(t, inj.IsEmpty())
}

func TestResolve_RejectsAnInvalidBlockRatherThanInjectingHalfOfIt(t *testing.T) {
	_, err := Resolve(
		Auth{Mode: ModeStatic, ValueFrom: "PLACEHOLDER-not-a-secret-ref"},
		"streamable-http",
		mapSecrets{},
		Grants{Unrestricted: true},
	)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "PLACEHOLDER-not-a-secret-ref")
}

func TestInjection_IsEmpty(t *testing.T) {
	assert.True(t, Injection{}.IsEmpty())
	assert.False(t, Injection{Headers: map[string]string{"A": "b"}}.IsEmpty())
	assert.False(t, Injection{Env: map[string]string{"A": "b"}}.IsEmpty())
	assert.True(t, Injection{Headers: map[string]string{}}.IsEmpty())
}
