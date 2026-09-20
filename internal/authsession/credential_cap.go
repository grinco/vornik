package authsession

// The capping rule — 2026-09-19-ce-human-login-design.md §5, §5.1.
//
// A session minted by a credential carries no more authority than that
// credential currently has. Stated as the check that runs on EVERY request,
// not at login: a cap evaluated once at login is not a cap, it is a snapshot,
// and "key revocation ends dependent sessions" would then be false for up to a
// session lifetime — precisely the window an operator revoking a leaked key
// cares about.
//
// WHY IT LIVES IN FRONT OF THE PRINCIPAL CACHE. `SessionBackend` caches
// resolved principals by USER ID. A key revocation cannot invalidate a
// user-keyed entry, because the key is not in its key — so a cap evaluated
// after the cache would keep authenticating a revoked credential until the
// entry expired. That defect is on this repository's record twice already
// (`oidc-identity-permissions-design.md` §4.3 advertising a 60s TTL while §5.7
// names "serves a disabled user until a cache expires" as the thing it exists
// to prevent). So the credential check is UNCACHED and runs before the
// resolver is consulted at all.

import (
	"context"
	"errors"
	"fmt"
)

// ErrCredentialUncheckable is returned when a session declares an originating
// credential and no checker is wired to verify it.
//
// FAIL CLOSED, deliberately. The alternative — treat an unwired checker as "no
// cap" — is a control that cannot distinguish "examined and clean" from "never
// examined" and reports the first. A deployment that mints credential-backed
// sessions must wire the checker; one that does not mint them never reaches
// this path, because the column is NULL for every session it creates.
var ErrCredentialUncheckable = errors.New("authsession: session declares an originating credential but none can be checked")

// CredentialChecker answers whether the credential that minted a session may
// still carry it.
//
// The implementation must verify BOTH halves, because either alone is a hole:
// the key still exists, is not revoked and is not expired; AND it is still
// mapped to this session's user. A key reassigned to somebody else must not
// keep carrying the previous owner's browser session.
type CredentialChecker interface {
	CheckSessionCredential(ctx context.Context, keyID, userID string) error
}

// SessionRevoker ends one session by id.
type SessionRevoker interface {
	RevokeSession(ctx context.Context, sessionID string) error
}

// checkCredentialCap applies the rule to one session.
//
// On failure it REVOKES the session as well as refusing the request. Refusing
// alone would leave a row that fails on every request forever — the right
// outcome for the caller, and a slow leak of dead rows that also means the
// next operator reading the session list cannot tell live from doomed.
//
// The revoke is best-effort: a revoke that fails must not turn a refusal into
// an acceptance, which is what returning its error instead would do.
func (b *SessionBackend) checkCredentialCap(ctx context.Context, sessionID, keyID, userID string) error {
	if keyID == "" {
		// No credential minted this session — an OIDC login. It is not
		// capped by a credential because there is none, which is the
		// correct answer rather than a skipped check.
		return nil
	}
	if b.Credentials == nil {
		return ErrCredentialUncheckable
	}
	if err := b.Credentials.CheckSessionCredential(ctx, keyID, userID); err != nil {
		b.revokeQuietly(ctx, sessionID)
		return fmt.Errorf("authsession: session %s is no longer carried by credential %s: %w", sessionID, keyID, err)
	}
	return nil
}

// revokeQuietly ends a session and swallows the outcome.
func (b *SessionBackend) revokeQuietly(ctx context.Context, sessionID string) {
	if b.Revoker == nil || sessionID == "" {
		return
	}
	_ = b.Revoker.RevokeSession(ctx, sessionID)
}
