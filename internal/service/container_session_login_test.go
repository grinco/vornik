package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/storage"
)

// Phase 2c: the EE browser-login construction moved to
// internal/enterprise/identity behind the neutral service.IdentityProvider
// seam. These CE-side tests pin the container's behaviour around that seam:
//   - providers.Identity == nil (Community)  → login disabled (nil wiring)
//   - provider returns ok=false              → login disabled (nil wiring)
//   - provider returns ok=true               → non-nil wiring, NEUTRAL types
//
// The EE construction details (Postgres-gate, sqlite warn, GitHub provider
// wiring, the ErrNoSession→ErrSessionDead adapter) are tested in
// internal/enterprise/identity.

// stubLoginBackend is a minimal auth.Backend the fake provider hands back.
type stubLoginBackend struct{}

func (stubLoginBackend) Name() string { return "session" }
func (stubLoginBackend) Authenticate(_ context.Context, _ auth.Credential) (*auth.Identity, error) {
	return nil, auth.ErrNoCredential
}

// fakeIdentityProvider is a test double for service.IdentityProvider whose
// LoginWiring returns canned values.
type fakeIdentityProvider struct {
	ok            bool
	backend       auth.Backend
	login         http.Handler
	logout        http.HandlerFunc
	providerNames []string
	gotDeps       *IdentityDeps
}

func (f *fakeIdentityProvider) LoginWiring(deps IdentityDeps) (auth.Backend, http.Handler, http.HandlerFunc, []string, bool) {
	f.gotDeps = &deps
	return f.backend, f.login, f.logout, f.providerNames, f.ok
}

func githubConfig() *config.Config {
	return &config.Config{
		Auth: config.AuthSettings{
			ExternalBaseURL: "https://vornik.example.com",
			Session:         config.SessionSettings{Lifetime: "168h", IdleTimeout: "24h"},
			Providers: config.ProviderSettings{
				GitHub: &config.GitHubProviderSettings{
					ClientID:     "Iv1.abc",
					ClientSecret: "shh",
					Org:          "grinco",
				},
			},
		},
	}
}

// TestBuildSessionLogin_NilProvider_Community asserts the core CE invariant:
// when providers.Identity is nil (the Community default), buildSessionLogin
// returns nil even when the GitHub provider config AND identity/session repos
// are fully present — the provider-presence edition gate blocks first.
func TestBuildSessionLogin_NilProvider_Community(t *testing.T) {
	c := &Container{
		Logger:    zerolog.Nop(),
		Config:    githubConfig(),
		repos:     &storage.Repositories{Identity: stubIdentityRepo{}, UISessions: stubUISessionRepo{}},
		providers: CommunityProviders(), // Identity == nil
	}
	if got := c.buildSessionLogin(); got != nil {
		t.Fatal("providers.Identity=nil (Community) → expected nil wiring (edition gate must block)")
	}
}

// TestBuildSessionLogin_ProviderNotOK_ReturnsNil asserts that when the EE
// provider reports ok=false (its inner config/backend gate — e.g. no GitHub
// block, or sqlite), the container maps that to nil wiring (login disabled).
func TestBuildSessionLogin_ProviderNotOK_ReturnsNil(t *testing.T) {
	c := &Container{
		Logger:    zerolog.Nop(),
		Config:    githubConfig(),
		repos:     &storage.Repositories{Identity: stubIdentityRepo{}, UISessions: stubUISessionRepo{}},
		providers: ProviderSet{Identity: &fakeIdentityProvider{ok: false}},
	}
	if got := c.buildSessionLogin(); got != nil {
		t.Fatal("provider LoginWiring ok=false → expected nil wiring")
	}
}

// TestBuildSessionLogin_ProviderOK_BuildsNeutralWiring asserts the EE happy
// path through the seam: a provider returning ok=true yields non-nil wiring
// carrying only the neutral CE types, and the container forwards the CE-held
// deps (config, repos, registry) to the provider.
func TestBuildSessionLogin_ProviderOK_BuildsNeutralWiring(t *testing.T) {
	login := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	logout := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	fp := &fakeIdentityProvider{
		ok:            true,
		backend:       stubLoginBackend{},
		login:         login,
		logout:        logout,
		providerNames: []string{"github"},
	}
	c := &Container{
		Logger:    zerolog.Nop(),
		Config:    githubConfig(),
		repos:     &storage.Repositories{Identity: stubIdentityRepo{}, UISessions: stubUISessionRepo{}},
		providers: ProviderSet{Identity: fp},
	}
	w := c.buildSessionLogin()
	if w == nil {
		t.Fatal("provider ok=true → expected non-nil wiring")
	}
	if w.backend == nil || w.loginHandler == nil || w.logoutHandler == nil {
		t.Fatalf("wiring has nil members: %+v", w)
	}
	if len(w.providerNames) != 1 || w.providerNames[0] != "github" {
		t.Errorf("providerNames = %v, want [github]", w.providerNames)
	}
	if w.backend.Name() != "session" {
		t.Errorf("backend.Name() = %q, want session", w.backend.Name())
	}
	// The container must forward the CE-held deps to the provider.
	if fp.gotDeps == nil {
		t.Fatal("provider received no deps")
	}
	if fp.gotDeps.Auth.ExternalBaseURL != "https://vornik.example.com" {
		t.Errorf("deps.Auth not forwarded: %+v", fp.gotDeps.Auth)
	}
	if fp.gotDeps.Identity == nil || fp.gotDeps.UISessions == nil {
		t.Error("deps repos not forwarded")
	}
	if fp.gotDeps.SessionIDFromContext == nil {
		t.Error("deps.SessionIDFromContext not forwarded")
	}
}

// stubIdentityRepo / stubUISessionRepo are no-op identity-core repos sufficient
// to satisfy the deps the container forwards. buildSessionLogin only forwards
// (never queries) them on the CE side.
type stubIdentityRepo struct{ persistence.IdentityRepository }
type stubUISessionRepo struct {
	persistence.UISessionRepository
}
