package mcpconnect

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
)

// Regression suite for the 2026-08-19 miss-contract normalisation.
//
// MCPOAuthTokenRepository.Get used to answer (nil, nil) for a pair with no
// grant; it now answers persistence.ErrNotFound like every other lookup. Each
// case below covers a connector path that treated ANY error as fatal and so
// changed behaviour the moment the repository stopped being permissive. The
// package's fakeTokens double was itself permissive, which is why none of
// these were exercised before.

// Disconnect must remain idempotent: an operator who disconnects a server
// that was never connected gets a clean result and an audit row, not an error.
func TestDisconnect_onAPairWithNoGrant_stillRecords(t *testing.T) {
	tokens := newFakeTokens()
	audit := &fakeAudit{}
	c := newConnector(t, tokens, audit, "https://v.example.com")

	err := c.Disconnect(context.Background(), "p-never", "linear", "bob")

	require.NoError(t, err, "disconnecting a never-connected pair must not error")
	require.Len(t, audit.entries, 1, "the disconnect must still be audited")
	assert.Equal(t, "mcp.oauth.disconnect", audit.entries[0].Action)
}

// storedClient falls back to the daemon-scope row when a project has no grant
// of its own. Propagating ErrNotFound from the project lookup skipped the
// fallback, so a project-scoped connect would re-register a DCR client that
// the deployment already had.
func TestStoredClient_fallsBackToDaemonScope_whenTheProjectHasNoGrant(t *testing.T) {
	tokens := newFakeTokens()
	require.NoError(t, tokens.Upsert(context.Background(), &persistence.MCPOAuthToken{
		ProjectID:  "",
		ServerName: "linear",
		ClientID:   "daemon-dcr-client",
	}))
	c := newConnector(t, tokens, &fakeAudit{}, "https://v.example.com")

	creds, err := c.storedClient(context.Background(), ServerRef{ProjectID: "p-none", ServerName: "linear"})

	require.NoError(t, err)
	assert.Equal(t, "daemon-dcr-client", creds.ID,
		"a project with no grant of its own must inherit the daemon-scope DCR client")
}

// Grant is the status endpoint's accessor and advertises "nil when there is
// none". It translates the repository's ErrNotFound rather than propagating
// it, so the control-plane row renders "not connected" instead of an error.
func TestGrant_reportsAbsenceAsNilRatherThanAnError(t *testing.T) {
	c := newConnector(t, newFakeTokens(), &fakeAudit{}, "https://v.example.com")

	got, err := c.Grant(context.Background(), "p-none", "linear")

	require.NoError(t, err, "an unconnected server is an ordinary answer for the status endpoint")
	assert.Nil(t, got)
}
