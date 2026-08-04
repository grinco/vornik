package mcpauth

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestValidate covers every rule in the design's §4 validation list, plus the
// fail-loud additions noted in validate.go. The table is the specification:
// each case names the operator mistake it catches.
func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name      string
		transport string
		auth      Auth
		wantErr   string // substring; "" = must pass
	}{
		{
			name:      "zero value on any transport is today's behaviour",
			transport: "stdio",
			auth:      Auth{},
		},
		{
			name:      "explicit none is accepted",
			transport: "streamable-http",
			auth:      Auth{Mode: ModeNone},
		},
		{
			name:      "unknown mode",
			transport: "streamable-http",
			auth:      Auth{Mode: "bearer"},
			wantErr:   `unknown mode "bearer"`,
		},
		{
			name:      "mode is case-sensitive",
			transport: "streamable-http",
			auth:      Auth{Mode: "Static", ValueFrom: "secret://t"},
			wantErr:   "unknown mode",
		},

		// --- static ---
		{
			name:      "static minimal",
			transport: "streamable-http",
			auth:      Auth{Mode: ModeStatic, ValueFrom: "secret://n8n_token"},
		},
		{
			name:      "static with explicit header and prefix",
			transport: "sse",
			auth:      Auth{Mode: ModeStatic, Header: "X-Api-Key", ValueFrom: "secret://k", ValuePrefix: "Bearer "},
		},
		{
			name:      "static without a value",
			transport: "streamable-http",
			auth:      Auth{Mode: ModeStatic},
			wantErr:   "value_from is required",
		},
		{
			name:      "static with a literal credential",
			transport: "streamable-http",
			auth:      Auth{Mode: ModeStatic, ValueFrom: "PLACEHOLDER-not-a-secret-ref"},
			wantErr:   "must be a secret:// reference",
		},
		{
			name:      "static with a legacy ${ENV} placeholder",
			transport: "streamable-http",
			auth:      Auth{Mode: ModeStatic, ValueFrom: "${N8N_TOKEN}"},
			wantErr:   "must be a secret:// reference",
		},
		{
			name:      "static on stdio has no request to sign",
			transport: "stdio",
			auth:      Auth{Mode: ModeStatic, ValueFrom: "secret://k"},
			wantErr:   "not valid on the stdio transport",
		},
		{
			name:      "static header collides with a protocol-owned header",
			transport: "streamable-http",
			auth:      Auth{Mode: ModeStatic, Header: "mcp-session-id", ValueFrom: "secret://k"},
			wantErr:   "protocol-owned",
		},
		{
			name:      "static header is not a valid header name",
			transport: "streamable-http",
			auth:      Auth{Mode: ModeStatic, Header: "X Api Key", ValueFrom: "secret://k"},
			wantErr:   "not a valid HTTP header name",
		},
		{
			name:      "static value prefix cannot smuggle a header",
			transport: "streamable-http",
			auth:      Auth{Mode: ModeStatic, ValueFrom: "secret://k", ValuePrefix: "Bearer \r\nX-Evil: 1"},
			wantErr:   "must not contain CR or LF",
		},
		{
			// review-20260804-350e finding 3: value_prefix is the one
			// credential-adjacent field holding a literal, so it is where the
			// "references, never secrets" invariant could be defeated by
			// pasting the token in.
			name:      "static value prefix long enough to be the credential",
			transport: "streamable-http",
			auth:      Auth{Mode: ModeStatic, ValueFrom: "secret://k", ValuePrefix: "Bearer PLACEHOLDER-NOT-A-REAL-CREDENTIAL-0000000000"},
			wantErr:   "longer than 32 characters",
		},

		// --- env ---
		{
			name:      "env minimal",
			transport: "stdio",
			auth:      Auth{Mode: ModeEnv, EnvFrom: map[string]string{"REDDIT_CLIENT_ID": "secret://reddit_id"}},
		},
		{
			name:      "env on a remote transport has no subprocess",
			transport: "streamable-http",
			auth:      Auth{Mode: ModeEnv, EnvFrom: map[string]string{"A": "secret://a"}},
			wantErr:   "only valid on the stdio transport",
		},
		{
			name:      "env without any mapping",
			transport: "stdio",
			auth:      Auth{Mode: ModeEnv},
			wantErr:   "env_from is required",
		},
		{
			name:      "env with a literal credential",
			transport: "stdio",
			auth:      Auth{Mode: ModeEnv, EnvFrom: map[string]string{"TOKEN": "hunter2"}},
			wantErr:   "must be a secret:// reference",
		},
		{
			name:      "env with an invalid variable name",
			transport: "stdio",
			auth:      Auth{Mode: ModeEnv, EnvFrom: map[string]string{"BAD NAME": "secret://a"}},
			wantErr:   "not a valid environment variable name",
		},
		{
			name:      "env cannot shadow the daemon's own namespace",
			transport: "stdio",
			auth:      Auth{Mode: ModeEnv, EnvFrom: map[string]string{"VORNIK_ADMIN_KEY": "secret://a"}},
			wantErr:   "VORNIK_",
		},

		// --- oauth ---
		{
			name:      "oauth discovered with DCR",
			transport: "streamable-http",
			auth:      Auth{Mode: ModeOAuth, Scopes: []string{"read:jira-work"}},
		},
		{
			name:      "oauth confidential client",
			transport: "streamable-http",
			auth:      Auth{Mode: ModeOAuth, ClientID: "1234.5678", ClientSecretFrom: "secret://slack_secret"},
		},
		{
			name:      "oauth manual endpoints",
			transport: "streamable-http",
			auth: Auth{
				Mode: ModeOAuth, ClientID: "abc",
				AuthorizationEndpoint: "https://app.intercom.com/oauth",
				TokenEndpoint:         "https://api.intercom.io/auth/eagle/token",
			},
		},
		{
			name:      "oauth authorization endpoint without token endpoint",
			transport: "streamable-http",
			auth:      Auth{Mode: ModeOAuth, AuthorizationEndpoint: "https://a/oauth"},
			wantErr:   "must be set together",
		},
		{
			name:      "oauth token endpoint without authorization endpoint",
			transport: "streamable-http",
			auth:      Auth{Mode: ModeOAuth, TokenEndpoint: "https://a/token"},
			wantErr:   "must be set together",
		},
		{
			name:      "oauth endpoints must be https",
			transport: "streamable-http",
			auth: Auth{
				Mode:                  ModeOAuth,
				AuthorizationEndpoint: "http://app.intercom.com/oauth",
				TokenEndpoint:         "https://api.intercom.io/token",
			},
			wantErr: "must be https",
		},
		{
			name:      "oauth client secret without a client id",
			transport: "streamable-http",
			auth:      Auth{Mode: ModeOAuth, ClientSecretFrom: "secret://s"},
			wantErr:   "client_id is required",
		},
		{
			name:      "oauth on stdio is forbidden by the MCP spec",
			transport: "stdio",
			auth:      Auth{Mode: ModeOAuth},
			wantErr:   "not valid on the stdio transport",
		},

		// --- cross-mode stray fields: a silent no-op today, a typo always ---
		{
			name:      "static field under env mode",
			transport: "stdio",
			auth:      Auth{Mode: ModeEnv, EnvFrom: map[string]string{"A": "secret://a"}, ValueFrom: "secret://b"},
			wantErr:   "value_from is not valid",
		},
		{
			name:      "env field under static mode",
			transport: "streamable-http",
			auth:      Auth{Mode: ModeStatic, ValueFrom: "secret://a", EnvFrom: map[string]string{"A": "secret://a"}},
			wantErr:   "env_from is not valid",
		},
		{
			name:      "oauth field under static mode",
			transport: "streamable-http",
			auth:      Auth{Mode: ModeStatic, ValueFrom: "secret://a", Scopes: []string{"read"}},
			wantErr:   "scopes is not valid",
		},
		{
			name:      "credential under mode none",
			transport: "streamable-http",
			auth:      Auth{Mode: ModeNone, ValueFrom: "secret://a"},
			wantErr:   "not valid",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.auth.Validate(tc.transport)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestValidate_ValuePrefixRejectionNeverEchoesTheCredential — the whole point of
// the length rule is that the value looks like a secret, so the error must not
// print it.
func TestValidate_ValuePrefixRejectionNeverEchoesTheCredential(t *testing.T) {
	// Deliberately NOT shaped like any vendor's real token grammar: a fixture
	// that pattern-matches a live credential format trips GitHub push
	// protection on the CE export, blocking a push for everyone downstream
	// (2026-08-04). The rule under test only cares about LENGTH.
	const pasted = "Bearer PLACEHOLDER-NOT-A-REAL-CREDENTIAL-0000000000"
	err := Auth{Mode: ModeStatic, ValueFrom: "secret://k", ValuePrefix: pasted}.Validate("streamable-http")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "PLACEHOLDER")
}

// TestValidate_ErrorNeverEchoesACredential — validation errors surface in boot
// logs and in the control-plane proposal UI. A message that quotes the
// offending value would publish the very literal the rule exists to reject.
func TestValidate_ErrorNeverEchoesACredential(t *testing.T) {
	const literal = "PLACEHOLDER-not-a-secret-ref"
	err := Auth{Mode: ModeStatic, ValueFrom: literal}.Validate("streamable-http")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), literal)

	err = Auth{Mode: ModeEnv, EnvFrom: map[string]string{"TOKEN": literal}}.Validate("stdio")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), literal)
	// The FIELD must still be named, or the operator cannot find the mistake.
	assert.Contains(t, err.Error(), "TOKEN")
}

// TestValidate_UnknownTransportStillValidatesTheBlock keeps validation from
// silently passing a malformed auth block on a name-only project entry (whose
// transport is inherited from the daemon catalog at wiring time).
func TestValidate_UnknownTransportStillValidatesTheBlock(t *testing.T) {
	err := Auth{Mode: "nonsense"}.Validate("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown mode")

	// …but a transport-dependent rule cannot fire when the transport is
	// not yet known: that check belongs to the wiring layer, which resolves
	// the inherited transport first.
	require.NoError(t, Auth{Mode: ModeEnv, EnvFrom: map[string]string{"A": "secret://a"}}.Validate(""))
	require.NoError(t, Auth{Mode: ModeStatic, ValueFrom: "secret://a"}.Validate(""))
}

func TestReservedHeaderNames_AreLowercaseAndComplete(t *testing.T) {
	// The set is compared case-insensitively, so storing anything but
	// lower case would silently miss "Mcp-Session-Id".
	for _, h := range ReservedHeaderNames() {
		assert.Equal(t, strings.ToLower(h), h, "reserved header names must be stored lower case")
	}
	assert.ElementsMatch(t,
		[]string{"content-type", "accept", "mcp-protocol-version", "mcp-session-id"},
		ReservedHeaderNames())
}
