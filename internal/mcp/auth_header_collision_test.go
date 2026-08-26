package mcp

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/mcpauth"
)

// This file is named in mcp-server-authentication-design.md §10 so it cannot be
// quietly dropped. It is the test that would have caught the mistake the design's
// first draft made: reserving `Authorization` as a protocol-owned header looks
// like obvious hardening and would have silently unauthenticated place_order,
// because the trading broker's shared secret rides the same Headers map.

// TestAuthorizationIsNotReserved is assertion 1 of 2 from the design.
func TestAuthorizationIsNotReserved(t *testing.T) {
	if _, reserved := reservedMCPHeaders["authorization"]; reserved {
		t.Fatal("Authorization must NOT be in reservedMCPHeaders: applyConfigHeaders DROPS reserved keys, " +
			"and brokerHeadersFor ships the broker's bearer through that same map — reserving it " +
			"silently unauthenticates place_order")
	}
}

// TestBrokerBearerSurvivesAuthComposition is assertion 2 of 2: a client
// configured the way brokerHeadersFor configures one still emits its bearer
// after auth injection has run.
func TestBrokerBearerSurvivesAuthComposition(t *testing.T) {
	c := &Client{
		logger: zerolog.Nop(),
		config: ServerConfig{
			Name: "broker",
			Headers: map[string]string{
				"Authorization":  "Bearer broker-shared-secret",
				"X-Project-ID":   "ibkr-trader",
				"X-Project-Caps": `{"max_position_usd":2500}`,
			},
			// A different server's auth block must not bleed in, but the
			// composition path runs for every client either way.
			AuthHeaders: nil,
		},
	}
	req, err := http.NewRequest(http.MethodPost, "http://broker/mcp", nil)
	require.NoError(t, err)
	require.NoError(t, c.applyConfigHeaders(context.Background(), req))

	assert.Equal(t, "Bearer broker-shared-secret", req.Header.Get("Authorization"))
	assert.Equal(t, "ibkr-trader", req.Header.Get("X-Project-ID"))
	assert.Equal(t, `{"max_position_usd":2500}`, req.Header.Get("X-Project-Caps"))
}

// TestAuthHeadersAppliedLastAndWin pins the ordering the design specifies: an
// auth-managed credential deterministically beats an operator-set header of the
// same name, and the overwrite is logged once at Warn rather than silently.
func TestAuthHeadersAppliedLastAndWin(t *testing.T) {
	var logBuf bytes.Buffer
	c := &Client{
		logger: zerolog.New(&logBuf),
		config: ServerConfig{
			Name:        "n8n",
			Headers:     map[string]string{"Authorization": "Bearer stale-operator-value"},
			AuthHeaders: map[string]string{"Authorization": "Bearer auth-managed-value"},
		},
	}
	req, err := http.NewRequest(http.MethodPost, "http://n8n/mcp", nil)
	require.NoError(t, err)
	require.NoError(t, c.applyConfigHeaders(context.Background(), req))

	assert.Equal(t, "Bearer auth-managed-value", req.Header.Get("Authorization"))
	assert.Contains(t, logBuf.String(), "overwrit")
	// The log must name the header but never the credential.
	assert.NotContains(t, logBuf.String(), "auth-managed-value")
	assert.NotContains(t, logBuf.String(), "stale-operator-value")
}

// TestAuthHeadersCannotHijackProtocolHeaders is defence in depth: config
// validation already rejects a protocol-owned header name, but a block reaching
// the client through some other path must not be able to hijack the session id.
func TestAuthHeadersCannotHijackProtocolHeaders(t *testing.T) {
	c := &Client{
		logger: zerolog.Nop(),
		config: ServerConfig{
			Name:        "evil",
			AuthHeaders: map[string]string{"Mcp-Session-Id": "attacker-session"},
		},
	}
	req, err := http.NewRequest(http.MethodPost, "http://evil/mcp", nil)
	require.NoError(t, err)
	req.Header.Set("Mcp-Session-Id", "real-session")
	require.NoError(t, c.applyConfigHeaders(context.Background(), req))

	assert.Equal(t, "real-session", req.Header.Get("Mcp-Session-Id"))
}

// TestReservedMCPHeaders_MatchMCPAuthList pins the two lists together.
// internal/mcpauth must stay a leaf so it cannot import this package, so it
// carries its own copy for validation; this test is what keeps the copy honest.
// Without it, adding a reserved header here would make mcpauth accept a static
// header the client then silently drops.
func TestReservedMCPHeaders_MatchMCPAuthList(t *testing.T) {
	ours := make([]string, 0, len(reservedMCPHeaders))
	for h := range reservedMCPHeaders {
		ours = append(ours, h)
	}
	assert.ElementsMatch(t, ours, mcpauth.ReservedHeaderNames(),
		"internal/mcp and internal/mcpauth disagree on the protocol-owned header set")
}
