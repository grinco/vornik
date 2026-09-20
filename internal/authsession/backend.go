// Package authsession is the browser-session stack SHARED BY BOTH EDITIONS:
// the session store (mint, validate, revoke, hashed tokens, idle and absolute
// expiry), the auth.Backend that authenticates the vornik_session cookie, the
// cookie names both login paths set, and the credential capping rule.
//
// IT IS CE, DELIBERATELY. The package doc used to describe this as "the
// Enterprise-edition browser-identity stack" living "under internal/enterprise/
// so the CE export strips it" — true until 2026-09-19, when
// 2026-09-19-ce-human-login-design.md §7 extracted it so Community's
// credential→session exchange could use the primitives that already existed
// rather than growing a second set. A reader who believed the old comment
// would avoid importing this from CE, which is the one thing the extraction
// exists to allow.
//
// Enterprise's OIDC login flow imports this package; an import law asserts
// both directions, because "CE does not import EE" is satisfied perfectly by
// an EE that quietly keeps its own copy — and two session stores with slightly
// different expiry arithmetic is a security bug that presents as an
// inconsistency.
//
// The CE-visible auth seam (auth.Backend/Credential/Identity/Chain and the
// auth.RoleAdmin/RoleUser + auth.ExtraSession* contract constants) stays in
// internal/auth — the SessionBackend implements auth.Backend and stamps those
// Extra keys, so it injects into the auth chain opaquely.
package authsession

import (
	"context"
	"errors"
	"sync"
	"time"

	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/authz"
	"vornik.io/vornik/internal/persistence"
)

// ErrSessionDead is the sentinel a SessionValidator returns for
// "no usable session" (unknown / expired / idle / revoked). The
// provider wiring wraps *session.Store in a tiny adapter
// translating session.ErrNoSession → ErrSessionDead, keeping the
// session package and this backend decoupled.
var ErrSessionDead = errors.New("identity: session dead")

// SessionValidator is the narrow slice of session.Store this
// backend needs. Dead-cookie outcomes MUST surface as
// ErrSessionDead.
type SessionValidator interface {
	Validate(ctx context.Context, raw string) (*persistence.UISession, error)
}

// UserPrincipalResolver resolves a principal by user id (the
// narrow slice of authz.Service this backend needs).
type UserPrincipalResolver interface {
	ResolveUser(ctx context.Context, userID string) (*authz.Principal, error)
}

// SessionBackend authenticates vornik_session cookies: cookie →
// active session row → authz principal, with a per-user TTL cache
// (default 60s) so group changes propagate without a resolver
// query on every request (design §4.3).
//
// Sentinels: no/dead cookie → auth.ErrNoCredential (a stale cookie must
// not block a valid Bearer on the same request; a random session
// token cannot collide into the key backends). Session alive but
// user disabled or vanished → auth.ErrUnauthorized (hard cut-off,
// within the cache TTL).
type SessionBackend struct {
	Store    SessionValidator
	Resolver UserPrincipalResolver
	TTL      time.Duration

	// Credentials applies the capping rule to a session minted by a
	// credential (ce-human-login-design §5). Nil is safe ONLY for a
	// deployment that never mints them — a session that declares an
	// originating credential with no checker wired is refused, not waved
	// through. See ErrCredentialUncheckable.
	Credentials CredentialChecker
	// Revoker ends a session whose credential no longer carries it, so a
	// doomed row does not linger failing every request forever.
	Revoker SessionRevoker

	mu sync.Mutex
	// cache grows with distinct logged-in users and has no
	// eviction — bounded by the human population of the instance,
	// which is small by design. Revisit with a sweeper if user
	// cardinality ever grows beyond that.
	cache map[string]cachedPrincipal // user_id → principal
	now   func() time.Time
}

type cachedPrincipal struct {
	p   *authz.Principal
	exp time.Time
}

// NewSessionBackend constructs the backend. ttl <= 0 defaults 60s.
func NewSessionBackend(store SessionValidator, resolver UserPrincipalResolver, ttl time.Duration) *SessionBackend {
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	return &SessionBackend{Store: store, Resolver: resolver, TTL: ttl,
		cache: map[string]cachedPrincipal{}, now: time.Now}
}

// Name returns the audit-trail identifier.
func (b *SessionBackend) Name() string { return "session" }

// Authenticate maps a session cookie to an auth.Identity.
func (b *SessionBackend) Authenticate(ctx context.Context, cred auth.Credential) (*auth.Identity, error) {
	if b == nil || b.Store == nil || cred.SessionToken == "" {
		return nil, auth.ErrNoCredential
	}
	sess, err := b.Store.Validate(ctx, cred.SessionToken)
	if err != nil {
		if errors.Is(err, ErrSessionDead) {
			return nil, auth.ErrNoCredential
		}
		// Transient store error — opt into graceful degrade so a
		// session-store blip doesn't lock out a valid Bearer on the
		// same request. The chain falls through on this sentinel;
		// bare errors now fail closed (see auth.Chain).
		return nil, errors.Join(auth.ErrBackendUnavailable, err)
	}
	// BEFORE the resolver, and therefore before its per-user cache. A
	// key revocation cannot invalidate a user-keyed cache entry, so a cap
	// applied after it would keep authenticating a revoked credential for
	// up to the TTL.
	if err := b.checkCredentialCap(ctx, sess.ID, sess.OriginCredentialID, sess.UserID); err != nil {
		return nil, errors.Join(auth.ErrUnauthorized, err)
	}
	p, err := b.resolveUser(ctx, sess.UserID)
	if err != nil {
		if errors.Is(err, authz.ErrUserDisabled) || errors.Is(err, authz.ErrUnknownIdentity) {
			return nil, auth.ErrUnauthorized
		}
		// Transient resolver error (DB blip) — same graceful-degrade
		// contract as the store error above.
		return nil, errors.Join(auth.ErrBackendUnavailable, err)
	}
	return &auth.Identity{
		Subject:     "user:" + sess.UserID,
		Projects:    p.Projects,
		DisplayName: p.DisplayName,
		IssuedAt:    sess.CreatedAt,
		ExpiresAt:   sess.ExpiresAt,
		Extra: map[string]any{
			auth.ExtraSessionRole:   p.Role,
			auth.ExtraSessionID:     sess.ID,
			auth.ExtraSessionUserID: sess.UserID,
		},
	}, nil
}

// resolveUser returns the cached principal or queries the
// resolver. DELIBERATE non-goals (do not "fix" without reading):
//   - The mutex is dropped across the resolver call, so N
//     concurrent requests for an uncached user issue N queries
//     (thundering herd). Bounded by per-user concurrency on a
//     TTL boundary; holding the lock across a DB call would be
//     worse. Single-flight is overkill at this population.
//   - Failures (incl. disabled/vanished users) are NOT cached:
//     negatives must re-check every request so un-disabling
//     takes effect immediately. Consequence: a disabled user
//     with a live cookie costs one resolver query per request
//     until the session dies — disabling now also revokes the
//     user's sessions (A2: IdentityRepository.SetUserDisabled),
//     so the session backend rejects them on the next request via
//     a dead-session lookup, well within the TTL.
//   - A2 (https://docs.vornik.io): ADMIN
//     principals are never served from (or written to) the
//     positive cache. Role demotion does not revoke sessions, so
//     caching an admin would let a demoted admin keep admin for up
//     to the TTL. Admins re-resolve every request (negligible cost
//     at this population) and lose admin on the very next request.
func (b *SessionBackend) resolveUser(ctx context.Context, userID string) (*authz.Principal, error) {
	now := b.now()
	b.mu.Lock()
	if c, ok := b.cache[userID]; ok && now.Before(c.exp) {
		b.mu.Unlock()
		return c.p, nil
	}
	b.mu.Unlock()
	p, err := b.Resolver.ResolveUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	// Do not cache elevated (admin) principals — a demotion must take
	// effect on the next request, not after the TTL (A2). A stale cache
	// entry from a prior non-admin resolve is harmless to leave (it will
	// expire); we simply never write an admin one.
	if p.Role != auth.RoleAdmin {
		b.mu.Lock()
		b.cache[userID] = cachedPrincipal{p: p, exp: now.Add(b.TTL)}
		b.mu.Unlock()
	}
	return p, nil
}
