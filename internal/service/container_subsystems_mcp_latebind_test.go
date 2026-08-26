package service

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/mcpconnect"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
	"vornik.io/vornik/internal/storage"
)

// Regression for the 2026-08-25 P0: a connector that loses auth degrades into a
// success-shaped task.
//
// applyMCPOAuthToken used to resolve the bearer HERE and freeze it into
// cfg.AuthHeaders. This function runs only inside initMCP — at boot, on config
// reload, and on consent — so the token it wrote was valid for the vendor's
// access-token lifetime and dead for however long the daemon ran afterwards.
// Measured on the production deployment: an 8-hour Atlassian token, 58h41m
// between two rewires, and ~51 hours of unbroken 401s while every status
// surface reported the grant connected and healthy.
//
// Design: https://docs.vornik.io §3.1.

// stubTokens is a minimal MCPOAuthTokenRepository whose stored token the test
// can rotate underneath the container, which is the whole point: the wiring
// must not have captured it.
type stubTokens struct {
	persistence.MCPOAuthTokenRepository
	tok *persistence.MCPOAuthToken
}

func (s *stubTokens) Get(_ context.Context, _, _ string) (*persistence.MCPOAuthToken, error) {
	if s.tok == nil {
		return nil, persistence.ErrNotFound
	}
	cp := *s.tok
	return &cp, nil
}

// A double that disagrees with production about what ABSENCE looks like
// certifies a broken path instead of failing. That matters more than usual
// here: (nil, ErrNotFound) is what makes an unconnected oauth server register
// and call unauthenticated rather than error, and a stub that returned
// (nil, nil) would make TestUnconnectedOAuthServerStillRegisters pass for the
// wrong reason.
func TestStubTokensHonoursTheMissContract(t *testing.T) {
	s := &stubTokens{}
	repotest.AssertMissRepo(t, "MCPOAuthTokenRepository.Get",
		func(ctx context.Context, id string) (*persistence.MCPOAuthToken, error) {
			return s.Get(ctx, "", id)
		})
}

func containerWithGrant(t *testing.T, tok *persistence.MCPOAuthToken) *Container {
	t.Helper()
	c := &Container{
		Logger:   zerolog.Nop(),
		Registry: writeMCPInheritFixture(t, oauthProjectYAML),
		Config:   &config.Config{},
		repos:    &storage.Repositories{MCPOAuthTokens: &stubTokens{tok: tok}},
	}
	return c
}

const oauthProjectYAML = `mcp:
  servers:
    - name: "atlassian"
      transport: "streamable-http"
      url: "https://mcp.atlassian.com/v1/mcp/authv2"
      auth:
        mode: oauth
        scopes: ["read:jira-work"]
`

func liveGrant(token string) *persistence.MCPOAuthToken {
	exp := time.Now().Add(8 * time.Hour)
	return &persistence.MCPOAuthToken{
		ProjectID: "test-project", ServerName: "atlassian",
		AccessToken: token, RefreshToken: "r1",
		Resource:  "https://mcp.atlassian.com/v1/mcp/authv2",
		ExpiresAt: &exp,
	}
}

// The credential must NOT be captured into the static map at wiring time.
func TestOAuthCredentialIsNotFrozenIntoAuthHeaders(t *testing.T) {
	c := containerWithGrant(t, liveGrant("boot-token"))

	servers := c.mcpDesiredServers()["test-project"]
	if len(servers) != 1 {
		t.Fatalf("want 1 server, got %d", len(servers))
	}
	if got := servers[0].AuthHeaders["Authorization"]; got != "" {
		t.Fatalf("the bearer was frozen into AuthHeaders at wiring time (%q) — "+
			"that is the 2026-08-25 P0", got)
	}
	if servers[0].AuthHeaderProvider == nil {
		t.Fatal("an oauth server must be wired with a per-request credential provider")
	}
}

// The provider must observe a token that changed AFTER wiring — the property
// the frozen map could not have.
func TestProviderSeesATokenRotatedAfterWiring(t *testing.T) {
	repoTok := liveGrant("boot-token")
	c := containerWithGrant(t, repoTok)

	servers := c.mcpDesiredServers()["test-project"]
	provider := servers[0].AuthHeaderProvider
	if provider == nil {
		t.Fatal("no provider wired")
	}

	got, err := provider(context.Background())
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if got["Authorization"] != "Bearer boot-token" {
		t.Fatalf("first resolve returned %q", got["Authorization"])
	}

	// Simulate the refresh that a rewire used to be required for.
	repoTok.AccessToken = "refreshed-token"
	servers[0].AuthInvalidator(context.Background())

	got, err = provider(context.Background())
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if got["Authorization"] != "Bearer refreshed-token" {
		t.Fatalf("the credential did not follow the store: got %q — the daemon is still "+
			"presenting a bearer it captured at wiring time", got["Authorization"])
	}
}

// A grant flagged needs_reconnect must make the provider FAIL, not return an
// empty credential. Returning nothing would send the request unauthenticated,
// the vendor would 401, the agent would narrate it, and the task would report
// success — the exact degradation this design removes.
func TestProviderFailsWhenTheGrantNeedsReconnect(t *testing.T) {
	tok := liveGrant("dead-token")
	tok.NeedsReconnect = true
	c := containerWithGrant(t, tok)

	servers := c.mcpDesiredServers()["test-project"]
	if len(servers) != 1 {
		t.Fatalf("a server needing reconnect must still be REGISTERED so the operator can see it; got %d", len(servers))
	}
	provider := servers[0].AuthHeaderProvider
	if provider == nil {
		t.Fatal("no provider wired")
	}
	got, err := provider(context.Background())
	if err == nil {
		t.Fatal("a grant needing reconnect must fail the call, not degrade it to unauthenticated")
	}
	if !isNeedsReconnect(err) {
		t.Fatalf("want ErrNeedsReconnect, got %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("no credential may be returned on the error path, got %v", got)
	}
}

// An oauth server the operator never connected still registers and still calls
// unauthenticated, so its tools stay visible in every agent's catalog and the
// operator can see what needs connecting (auth design §8).
func TestUnconnectedOAuthServerStillRegisters(t *testing.T) {
	c := containerWithGrant(t, nil)

	servers := c.mcpDesiredServers()["test-project"]
	if len(servers) != 1 {
		t.Fatalf("want the server registered, got %d", len(servers))
	}
	got, err := servers[0].AuthHeaderProvider(context.Background())
	if err != nil {
		t.Fatalf("an unconnected server must not error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want no credential, got %v", got)
	}
}

func isNeedsReconnect(err error) bool {
	for err != nil {
		if err == mcpconnect.ErrNeedsReconnect {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
