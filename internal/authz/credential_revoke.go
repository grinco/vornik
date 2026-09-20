package authz

// Revoking a credential must also end the browser sessions that credential
// minted — 2026-09-19-ce-human-login-design.md §5.1.
//
// WHY THIS IS ONE FUNCTION AND NOT FOUR CALL SITES. `apiKeyRepo.Revoke` is
// called from four places across `internal/api` and `internal/ui`, plus
// `RevokeByName` for task keys. Adding "…and revoke its sessions" to each of
// them is the shape this repository has paid for three times: a safety step
// with N implementations has one that is missing, and the missing one is
// usually the newest. So the two writes live behind one verb, and the call
// sites move to it.

import (
	"context"
	"errors"
	"fmt"
)

// ErrCredentialRevokeUnavailable is returned when the key store is not wired.
var ErrCredentialRevokeUnavailable = errors.New("authz: credential revocation not available")

// RevokeCredential revokes an API key and ends every browser session that key
// minted.
//
// ORDER IS LOAD-BEARING: the KEY first, then the sessions. The two tables live
// in two repositories, both holding a DBTX with no BeginTx, so this cannot be
// one transaction — and given that, the question is which half survives a
// crash between them.
//
//   - Key first: the worst case is session rows that outlive the key. They are
//     already dead in practice, because the per-request capping rule refuses a
//     session whose originating credential is revoked, and revokes it there.
//   - Sessions first: the worst case is a LIVE KEY that the operator believes
//     they revoked. Nothing else catches that.
//
// So a failure to revoke the sessions is reported but does not un-revoke the
// key, and the session sweep is best-effort in exactly the way the freshness
// check makes safe. That asymmetry is the design's, not an implementation
// convenience: §5.1 takes both halves precisely so each covers the other.
func (a *Accounts) RevokeCredential(ctx context.Context, keyID string, actor Actor) error {
	if err := a.ready(); err != nil {
		return err
	}
	if a.keys == nil {
		return ErrCredentialRevokeUnavailable
	}
	if keyID == "" {
		return fmt.Errorf("authz: revoke credential: key id is required")
	}

	if err := a.keys.Revoke(ctx, keyID); err != nil {
		return fmt.Errorf("authz: revoke credential: %w", err)
	}
	// From here the key is dead whatever happens next.
	sessionErr := a.repo.RevokeSessionsForCredential(ctx, keyID)

	// Audited before the session error is returned: the key revocation
	// HAPPENED, and an audit trail that records it only when the whole verb
	// succeeded would omit exactly the case an operator needs to see.
	auditErr := a.record(ctx, actor, "credential.revoke", keyID, map[string]any{
		"sessions_revoked": sessionErr == nil,
	})

	switch {
	case sessionErr != nil && auditErr != nil:
		// BOTH failed, and an earlier version returned only the session
		// error — dropping the audit failure on the exact case the
		// comment above says must always be recorded
		// (review-20260919-76da F2). Both are reported, because an
		// operator who learns the sweep failed and not that the record
		// failed will believe there is a row to find later.
		return fmt.Errorf("authz: key %s is revoked, but its browser sessions could not be (%v) "+
			"AND the revocation was not audited (%w) — the sessions are refused on their next "+
			"request by the capping rule, but nothing recorded that this key was revoked",
			keyID, sessionErr, auditErr)
	case sessionErr != nil:
		return fmt.Errorf("authz: key %s is revoked, but its browser sessions could not be: %w "+
			"(they are refused on their next request by the capping rule; re-run to clear the rows)",
			keyID, sessionErr)
	case auditErr != nil:
		return auditErr
	}
	return nil
}
