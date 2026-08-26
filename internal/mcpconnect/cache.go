package mcpconnect

import (
	"context"
	"sync"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// The read-through token cache that makes per-call credential binding
// affordable.
//
// Before the 2026-08-26 connector-auth design, an OAuth bearer was resolved
// ONCE per wiring pass and frozen into mcp.ServerConfig.AuthHeaders. That is
// why a grant could die silently: the only thing that re-resolved a credential
// was a config reload, and on the production deployment two reloads were
// 58h41m apart while the token lived 8 hours. Binding per call fixes it, but
// moves the resolution from "once per reload" to "once per tool call" — so
// without a cache every agent action would carry a database round trip.
//
// Three coherence rules, from review round 1 finding C. They are the whole
// contract; everything else here is bookkeeping:
//
//  1. An entry within refreshSkew of expiry is a MISS. The cache can therefore
//     never serve a token past the point the uncached path would have
//     refreshed it.
//  2. The refresh winner writes through, so the entry is never behind the row.
//  3. A 401 evicts (InvalidateToken), which is what lets the one-shot retry
//     read from the store rather than from the thing that just failed.
//
// Cross-process staleness needs no extra mechanism: another daemon's refresh
// rotates the token, our copy hits rule 1 at its own skew boundary, and the
// conditional SwapRefreshToken already makes a losing writer reload.
//
// One benign reorder falls out of that, and it is NOT a bug to fix: if another
// process refreshes T1 -> T2 while our entry is still outside its own skew, we
// keep serving T1 for a little longer. T1 is valid until its expiry, the skew
// is exactly the margin in which either token is acceptable, and a T1 that was
// actually revoked produces a 401 which rule 3 evicts. Closing this window
// would need cross-process cache invalidation — a listener, a channel, a
// heartbeat — to buy nothing. Left alone deliberately (review round 2).
//
// Design: https://docs.vornik.io §3.1.

// cachedToken is one entry. It holds only what rule 1 needs to decide
// freshness plus the value itself — never NeedsReconnect, which is read from
// the row on every fall-through and must never be answered from memory.
type cachedToken struct {
	token     string
	expiresAt *time.Time
}

// fresh reports whether this entry may be served without touching the store.
//
// A nil expiry means the authorization server issued no expires_in. That is
// NOT "never expires" — it is "we were not told", and the honest answer to an
// unknown lifetime is to re-read. Caching it indefinitely would recreate the
// frozen-bearer bug with extra steps.
func (c cachedToken) fresh(now time.Time, skew time.Duration) bool {
	if c.token == "" || c.expiresAt == nil {
		return false
	}
	return now.Add(skew).Before(*c.expiresAt)
}

// cacheKey is the (project, server) pair a grant is stored under. A
// daemon-scope grant lives at project "" and is shared by every project that
// subscribes to that server by name; a project that declares its own transport
// owns its own credential. Getting this wrong hands one project's token to
// another, so the key is built in exactly one place.
func cacheKey(projectID, serverName string) string {
	return projectID + "\x00" + serverName
}

// tokenCacheMap is the per-connector store. Declared as its own type so the
// zero value of Connector is usable without an explicit constructor — the
// wiring layer builds Connector as a struct literal.
type tokenCacheMap = sync.Map

// CachedAccessToken returns the bearer to present to a server, serving it from
// the in-process cache when rule 1 allows and falling through to AccessToken —
// which owns refresh, the cross-process lock and the needs_reconnect marking —
// when it does not.
//
// Returns ("", nil) for a server with no grant. That is an ordinary answer, not
// an error: an oauth-mode server the operator has never connected registers and
// calls unauthenticated, so its tools stay visible and the operator can see
// what needs connecting (auth design §8). It is deliberately NOT cached — the
// answer flips the moment consent completes.
func (c *Connector) CachedAccessToken(ctx context.Context, ref ServerRef) (string, error) {
	key := cacheKey(ref.ProjectID, ref.ServerName)
	if v, ok := c.tokenCache.Load(key); ok {
		if entry, ok := v.(cachedToken); ok && entry.fresh(time.Now(), refreshSkew) {
			return entry.token, nil
		}
		// Stale by rule 1. Drop it rather than leave it to be re-evaluated
		// on every call.
		c.tokenCache.Delete(key)
	}

	// Rule 2: resolveAccessToken hands back the expiry belonging to the token
	// it resolved — including one it just minted under the refresh lock — so
	// the entry is never behind the row it mirrors, and the cold path costs
	// one read rather than two.
	token, expiresAt, err := c.resolveAccessToken(ctx, ref)
	if err != nil {
		// Never cache a failure. needs_reconnect in particular must be
		// re-read every call: it is the flag every operator surface reads,
		// and it is cleared by a reconnect this process does not observe.
		c.tokenCache.Delete(key)
		return "", err
	}
	if token == "" {
		return "", nil
	}
	c.tokenCache.Store(key, cachedToken{token: token, expiresAt: expiresAt})
	return token, nil
}

// InvalidateToken drops the cached credential for a pair.
//
// Called on exactly one condition: the vendor answered 401/403 to a credential
// this daemon believed was valid. The stored expires_at is the vendor's
// ADVERTISED lifetime and is not binding on the vendor — a grant revoked in
// their console dies before its stated expiry with nothing to tell us — so the
// 401 is the only authoritative signal, and eviction is what makes the one-shot
// retry meaningful.
func (c *Connector) InvalidateToken(projectID, serverName string) {
	c.tokenCache.Delete(cacheKey(projectID, serverName))
}

// primeCachedToken is the rule-2 write-through used by the grant path, so a
// freshly consented credential is served from cache immediately rather than
// costing one extra read.
func (c *Connector) primeCachedToken(projectID, serverName string, tok *persistence.MCPOAuthToken) {
	if tok == nil || tok.AccessToken == "" {
		return
	}
	c.tokenCache.Store(cacheKey(projectID, serverName),
		cachedToken{token: tok.AccessToken, expiresAt: tok.ExpiresAt})
}
