package service

import (
	"net/http"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/auth"
)

// sessionLoginWiring bundles the pieces initHTTPServer mounts when browser
// login is configured AND the Postgres-only identity core is present. All
// fields are neutral CE types (auth.Backend + http handlers) so internal/service
// never names the EE login types (*loginflow.Handler / *session.Store /
// authz.*). The construction lives in the EE IdentityProvider (Phase 2c);
// this struct is the neutral hand-off. All fields are non-nil together or the
// whole struct is nil (the caller treats nil as "login disabled").
type sessionLoginWiring struct {
	// backend rides the auth chain (prepended) to authenticate the
	// vornik_session cookie. It is the opaque auth.Backend seam — the
	// concrete EE *identity.SessionBackend is hidden behind it.
	backend auth.Backend
	// loginHandler serves /auth/<provider>/{start,callback}.
	loginHandler http.Handler
	// logoutHandler serves POST /ui/logout.
	logoutHandler http.HandlerFunc
	// providerNames is the ordered list the login page renders as
	// buttons.
	providerNames []string
}

// buildSessionLogin constructs the browser-login wiring via the EE
// IdentityProvider, or returns nil when login isn't available.
//
// Gating (Phase 2c — provider-presence is the OUTER edition gate):
//   - No IdentityProvider for this edition (providers.Identity == nil →
//     Community) → login disabled; return nil and log the omission at Info.
//     This replaces the former providers.OIDC bool edition gate.
//   - The EE provider's LoginWiring returns ok=false for the inner config /
//     backend gates (no GitHub provider block; or the Postgres identity core
//     is absent on sqlite). The provider owns the Info/Warn logging for those
//     cases; here we just map ok=false → nil.
//
// The returned wiring carries only neutral CE types; the EE login machinery
// (loginflow/oidc/session/authz/SessionBackend) lives entirely in
// internal/enterprise/identity.
func (c *Container) buildSessionLogin() *sessionLoginWiring {
	if c.providers.Identity == nil {
		c.Logger.Info().Str("capability", "oidc").Str("edition", c.Edition()).Msg("EE capability omitted by edition")
		return nil
	}

	deps := IdentityDeps{
		Logger:               c.Logger.With().Str("component", "loginflow").Logger(),
		SessionIDFromContext: api.SessionIDFromContext,
	}
	if c.Config != nil {
		deps.Auth = c.Config.Auth
	}
	if c.repos != nil {
		deps.Identity = c.repos.Identity
		deps.UISessions = c.repos.UISessions
	}
	deps.Registry = c.Registry

	backend, login, logout, names, ok := c.providers.Identity.LoginWiring(deps)
	if !ok {
		return nil
	}
	return &sessionLoginWiring{
		backend:       backend,
		loginHandler:  login,
		logoutHandler: logout,
		providerNames: names,
	}
}
