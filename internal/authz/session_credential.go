package authz

// The credential half of the CE login capping rule —
// 2026-09-19-ce-human-login-design.md §5.
//
// A browser session minted by an API key carries no more authority than that
// key currently has. This answers the per-request question the session backend
// asks: is the key that minted this session still live, and still this user's?

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrCredentialNoLongerValid is returned when the key that minted a session
// can no longer carry it — revoked, expired, gone, or reassigned.
//
// ONE sentinel for all four, deliberately. The caller turns it into a 401 and
// revokes the session; telling a browser WHICH of the four applies would say
// more about another account's key than the holder of a dead cookie should
// learn. The operator-facing distinction lives in the audit trail.
var ErrCredentialNoLongerValid = errors.New("authz: the credential that minted this session no longer carries it")

// CheckSessionCredential reports whether keyID may still carry a session for
// userID.
//
// Both halves are checked because either alone is a hole:
//
//   - the KEY must still exist, be unrevoked and unexpired — the obvious half,
//     and the one an operator exercises when they revoke a leaked key;
//   - it must still MAP to this user. A key reassigned to somebody else must
//     not keep carrying the previous owner's browser session, which is exactly
//     what checking only the key's liveness would allow.
//
// It reads through to the stores on every call. That is the point: the
// session backend's principal cache is keyed by user id and cannot be
// invalidated by a key revocation, so a cached answer here would reintroduce
// the window this rule exists to close.
func (a *Accounts) CheckSessionCredential(ctx context.Context, keyID, userID string) error {
	if err := a.ready(); err != nil {
		return err
	}
	if a.keys == nil {
		// The session says a credential minted it and this deployment
		// cannot check credentials. Refusing is the only honest answer;
		// treating it as uncapped would be a control reporting "clean"
		// over a surface it never examined.
		return fmt.Errorf("%w: no key store is wired to check it", ErrCredentialNoLongerValid)
	}
	if keyID == "" || userID == "" {
		return fmt.Errorf("%w: incomplete session attribution", ErrCredentialNoLongerValid)
	}

	key, err := a.keys.GetByID(ctx, keyID)
	if err != nil {
		// Gone, or unreadable. A store error must NOT read as "still
		// valid": this is the path an operator relies on after revoking
		// a key, and failing open here would make the revocation a
		// suggestion. The session backend surfaces it as a refusal.
		return fmt.Errorf("%w: %v", ErrCredentialNoLongerValid, err)
	}
	if key == nil {
		return fmt.Errorf("%w: no such key", ErrCredentialNoLongerValid)
	}
	if key.RevokedAt != nil {
		return fmt.Errorf("%w: revoked", ErrCredentialNoLongerValid)
	}
	if key.ExpiresAt != nil && !key.ExpiresAt.After(a.nowUTC()) {
		return fmt.Errorf("%w: expired", ErrCredentialNoLongerValid)
	}

	owner, err := a.keyOwner(ctx, keyID)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCredentialNoLongerValid, err)
	}
	if owner != userID {
		// Covers both "unmapped now" (owner == "") and "someone else's".
		return fmt.Errorf("%w: no longer mapped to this account", ErrCredentialNoLongerValid)
	}
	return nil
}

// nowUTC keeps CheckSessionCredential usable on an Accounts built without a
// clock, which the tests and some wiring paths do.
func (a *Accounts) nowUTC() time.Time {
	if a.now == nil {
		return time.Now().UTC()
	}
	return a.now().UTC()
}
