package service

// The CE credential→session wiring —
// 2026-09-19-ce-human-login-design.md §4, §7.
//
// WHY IT IS A SEPARATE PATH FROM buildSessionLogin. That one constructs the
// browser login through the EE IdentityProvider and returns nil the moment
// `providers.Identity == nil`, which IS Community. So on CE nothing builds a
// session store and, more importantly, nothing puts a SessionBackend on the
// auth chain — a cookie could be minted and no subsequent request could
// authenticate it. The exchange without this is a 204 followed by every call
// still falling through to the Bearer path, which presents as "login silently
// does not work".
//
// It shares the primitives rather than copying them: `internal/authsession`
// is the same store and the same backend the EE flow uses, extracted to CE for
// exactly this reason, with an import law in both directions.

import (
	"context"
	"errors"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/authsession"
	"vornik.io/vornik/internal/authz"
	"vornik.io/vornik/internal/persistence"
)

// credentialSessionWiring is what the container mounts when the exchange is
// enabled. Nil means the door is not offered.
type credentialSessionWiring struct {
	// backend authenticates the session cookie. It PREPENDS to the auth
	// chain, so a cookie is tried before the Bearer backends.
	backend auth.Backend
	// options carry the exchange route's own dependencies.
	options []api.ServerOption
}

// buildCredentialSession constructs the CE exchange, or returns nil.
//
// Every gate below refuses by returning nil and saying why at Info. A door
// that is half-wired must not exist at all: the route answers 503 when the
// options are absent, which is the honest "not offered here" rather than a
// 401 that would make an unwired deployment look like a rejected key.
func (c *Container) buildCredentialSession() *credentialSessionWiring {
	if c.Config == nil || !c.Config.Auth.Session.CredentialExchange {
		return nil
	}
	accounts := c.accountsService()
	if accounts == nil {
		// The identity core answers "whose key is this", and without it
		// a credential cannot name an account to act as.
		c.Logger.Warn().
			Msg("credential session exchange is enabled but the identity core is absent; the route will answer 503")
		return nil
	}
	if c.repos == nil || c.repos.UISessions == nil {
		c.Logger.Warn().
			Msg("credential session exchange is enabled but no session store is wired; the route will answer 503")
		return nil
	}

	lifetime := c.sessionDuration("auth.session.lifetime", c.Config.Auth.Session.Lifetime, 168*time.Hour)
	idle := c.sessionDuration("auth.session.idle_timeout", c.Config.Auth.Session.IdleTimeout, 0)
	store := authsession.New(c.repos.UISessions, lifetime, idle)

	// The CE resolver — the same one the chat door reads through, so
	// "what may this person do" has one answer across every channel.
	// Accounts owns account MANAGEMENT; Service owns resolution, and the
	// backend needs the second.
	resolver := authz.NewService(c.repos.Identity)
	backend := authsession.NewSessionBackend(sessionStoreValidator{store}, resolver, 0)
	// The capping rule (§5). Without these two the backend would
	// authenticate a session whose credential had been revoked, for as
	// long as the session lived — which is the whole thing this design
	// exists to prevent, so they are set together with the backend rather
	// than somewhere a later edit could separate them.
	backend.Credentials = accounts
	backend.Revoker = sessionRevoker{store}

	if c.Config.Auth.Session.AllowInsecureExchange {
		// Loud, once, at boot. An operator who set this deliberately
		// sees it confirmed; one who inherited it from a copied config
		// finds out before a cookie travels in the clear.
		c.Logger.Warn().
			Msg("auth.session.allow_insecure_exchange is ON: session cookies may be minted over plaintext HTTP, " +
				"and a session cookie in the clear is the whole session. Development only.")
	}

	return &credentialSessionWiring{
		backend: backend,
		options: []api.ServerOption{
			api.WithSessionExchange(accounts, store, lifetime, c.Config.Auth.Session.AllowInsecureExchange),
		},
	}
}

// sessionStoreValidator adapts the store's ErrNoSession to the backend's
// ErrSessionDead sentinel — the same adapter shape the EE provider uses, and
// for the same reason: the store and the backend stay decoupled.
type sessionStoreValidator struct{ store *authsession.Store }

func (v sessionStoreValidator) Validate(ctx context.Context, raw string) (*persistence.UISession, error) {
	sess, err := v.store.Validate(ctx, raw)
	if err != nil {
		if errors.Is(err, authsession.ErrNoSession) {
			return nil, authsession.ErrSessionDead
		}
		return nil, err
	}
	return sess, nil
}

// sessionRevoker lets the capping rule end a session whose credential no
// longer carries it.
type sessionRevoker struct{ store *authsession.Store }

func (r sessionRevoker) RevokeSession(ctx context.Context, id string) error {
	return r.store.Revoke(ctx, id)
}

// sessionDuration parses a config duration and SAYS SO when it falls back.
//
// The fallback itself is right — a daemon that refuses to start over "7 days"
// instead of "168h" is a worse outcome than one that runs on the documented
// default. What was wrong was doing it silently: an operator who set a
// lifetime and got the default had no way to find out
// (review-20260919-76da, minor).
func (c *Container) sessionDuration(key, raw string, fallback time.Duration) time.Duration {
	if raw != "" && parseSessionDuration(raw, fallback) == fallback {
		if d, err := time.ParseDuration(raw); err != nil || d < 0 {
			c.Logger.Warn().Str("key", key).Str("value", raw).Dur("using", fallback).
				Msg("session duration is not a valid Go duration; falling back to the default")
		}
	}
	return parseSessionDuration(raw, fallback)
}

// parseSessionDuration parses a config duration, falling back on empty or
// unparseable input.
//
// An unparseable lifetime falls back rather than failing the boot: the value
// is operator-typed, and a daemon that refuses to start over "7 days" instead
// of "168h" is a worse outcome than one that runs on the documented default
// and says so.
func parseSessionDuration(raw string, fallback time.Duration) time.Duration {
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return fallback
	}
	return d
}
