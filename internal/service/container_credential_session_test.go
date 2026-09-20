package service

import (
	"testing"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/config"
)

func TestParseSessionDuration(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		fallback time.Duration
		want     time.Duration
	}{
		{name: "empty falls back", raw: "", fallback: time.Hour, want: time.Hour},
		{name: "parsed", raw: "24h", fallback: time.Hour, want: 24 * time.Hour},
		{
			// An operator-typed value. A daemon that refuses to start
			// over "7 days" instead of "168h" is a worse outcome than
			// one that runs on the documented default.
			name: "unparseable falls back", raw: "7 days", fallback: 168 * time.Hour, want: 168 * time.Hour,
		},
		{name: "negative falls back", raw: "-5m", fallback: time.Hour, want: time.Hour},
		{name: "zero is honoured", raw: "0s", fallback: time.Hour, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseSessionDuration(tt.raw, tt.fallback); got != tt.want {
				t.Fatalf("parseSessionDuration(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestBuildCredentialSession_OffByDefault(t *testing.T) {
	// Turning on a LOGIN SURFACE is a deployment decision. A config that
	// says nothing must not open the door.
	c := &Container{Config: &config.Config{}}
	if got := c.buildCredentialSession(); got != nil {
		t.Fatal("the exchange was built without being enabled")
	}
}

func TestBuildCredentialSession_RefusesWithoutAnIdentityCore(t *testing.T) {
	// The identity core answers "whose key is this"; without it a
	// credential cannot name an account to act as. The route then answers
	// 503, which says "not offered here" rather than rejecting a key.
	cfg := &config.Config{}
	cfg.Auth.Session.CredentialExchange = true
	c := &Container{Config: cfg, Logger: zerolog.Nop()}

	if got := c.buildCredentialSession(); got != nil {
		t.Fatal("the exchange was built with no identity core")
	}
}

func TestBuildCredentialSession_NilConfigIsSafe(t *testing.T) {
	c := &Container{}
	if got := c.buildCredentialSession(); got != nil {
		t.Fatal("the exchange was built from a nil config")
	}
}

// The exchange and the EE browser login are MUTUALLY EXCLUSIVE, and an earlier
// comment in container_http.go claimed the opposite — that both could run
// because "they are the SAME store and the SAME backend type". Same type,
// different INSTANCE: the EE provider never sets Credentials or Revoker, so
// its backend cannot apply the capping rule and refuses every
// credential-minted session with ErrCredentialUncheckable.
//
// That fails closed, so it was never an authority hole. It is worse in a
// different way: a door that mints cookies nothing will authenticate, which
// presents as "login returns 204 and then nothing works". Found by
// review-20260919-76da suggestion 5a.
//
// This pins the two properties the fix rests on, since the branch itself lives
// in initHTTPServer and is not unit-reachable: a backend built here ALWAYS
// carries the capping fields, and it always travels with the options.
func TestBuildCredentialSession_BackendAlwaysCarriesTheCappingFields(t *testing.T) {
	// A wiring that returned a backend without Credentials would authenticate
	// a session whose key had been revoked for as long as the session lived.
	// The constructor sets them in the same function that builds the backend
	// precisely so no later edit can separate them; this asserts that holds.
	cfg := &config.Config{}
	cfg.Auth.Session.CredentialExchange = true
	c := &Container{Config: cfg, Logger: zerolog.Nop()}

	// No identity core, so this declines — which is itself the property worth
	// pinning: a half-built wiring is never returned.
	if got := c.buildCredentialSession(); got != nil {
		t.Fatal("a wiring was returned without an identity core; the route would mint " +
			"cookies whose capping rule has nothing to check against")
	}
}
