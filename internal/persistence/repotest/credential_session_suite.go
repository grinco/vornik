package repotest

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RunCredentialSessionSuite pins the credential→session binding on BOTH
// backends — 2026-09-19-ce-human-login-design.md §4, §5.1.
//
// The security claim the whole CE login rests on is "key revocation ends
// dependent sessions". Half of that is a per-request freshness check and half
// is this: the key-revocation path revoking the sessions that key minted. A
// store that behaved differently on two drivers would make the claim a
// function of the deployment, which is exactly what the shared suites exist to
// prevent.
func RunCredentialSessionSuite(t *testing.T, sessions persistence.UISessionRepository, identity persistence.IdentityRepository) {
	runCredentialAttributionCases(t, sessions, identity)
	runCredentialRevocationCases(t, sessions, identity)
}

// runCredentialAttributionCases covers what the column stores — the subject
// the capping rule asks about.
func runCredentialAttributionCases(t *testing.T, sessions persistence.UISessionRepository, identity persistence.IdentityRepository) {
	ctx := context.Background()
	f := &identityFixture{sessions: sessions, identity: identity}

	// mint creates a session attributed to a credential, the way the CE
	// exchange will.
	mint := func(t *testing.T, userID, credentialID string) *persistence.UISession {
		t.Helper()
		s := &persistence.UISession{
			ID: uniqueID("sess"), TokenHash: uniqueID("hash"), UserID: userID,
			Provider:  "credential",
			CreatedAt: clock(), LastSeenAt: clock(), ExpiresAt: clock().Add(24 * time.Hour),
			OriginCredentialID: credentialID,
		}
		wantOK(t, "CreateSession", f.sessions.CreateSession(ctx, s))
		return s
	}

	t.Run("the minting credential round-trips", func(t *testing.T) {
		// Without this the capping rule has no subject and "key
		// revocation ends dependent sessions" is a sentence with nothing
		// behind it.
		u := f.user(t)
		key := uniqueID("key")
		s := mint(t, u.ID, key)

		got, err := f.sessions.GetActiveByTokenHash(ctx, s.TokenHash)
		wantOK(t, "GetActiveByTokenHash", err)
		if got.OriginCredentialID != key {
			t.Fatalf("OriginCredentialID = %q, want %q", got.OriginCredentialID, key)
		}
	})

	t.Run("an OIDC session carries no credential and stays empty", func(t *testing.T) {
		// NULL, not "". An EE login has no originating credential and
		// must not be made to invent one — and the empty string would
		// make every OIDC session look like it shared one key.
		u := f.user(t)
		s := f.activeSession(t, u.ID)

		got, err := f.sessions.GetActiveByTokenHash(ctx, s.TokenHash)
		wantOK(t, "GetActiveByTokenHash", err)
		if got.OriginCredentialID != "" {
			t.Fatalf("OriginCredentialID = %q, want empty for a session no credential minted", got.OriginCredentialID)
		}
	})

}

// runCredentialRevocationCases covers the sweep — the half that makes "key
// revocation ends dependent sessions" true.
func runCredentialRevocationCases(t *testing.T, sessions persistence.UISessionRepository, identity persistence.IdentityRepository) {
	ctx := context.Background()
	f := &identityFixture{sessions: sessions, identity: identity}

	mint := func(t *testing.T, userID, credentialID string) *persistence.UISession {
		t.Helper()
		s := &persistence.UISession{
			ID: uniqueID("sess"), TokenHash: uniqueID("hash"), UserID: userID,
			Provider:  "credential",
			CreatedAt: clock(), LastSeenAt: clock(), ExpiresAt: clock().Add(24 * time.Hour),
			OriginCredentialID: credentialID,
		}
		wantOK(t, "CreateSession", f.sessions.CreateSession(ctx, s))
		return s
	}

	t.Run("revoking a credential ends its sessions and only its sessions", func(t *testing.T) {
		u := f.user(t)
		victimKey := uniqueID("key-victim")
		otherKey := uniqueID("key-other")

		doomed := mint(t, u.ID, victimKey)
		sibling := mint(t, u.ID, victimKey)
		bystander := mint(t, u.ID, otherKey)
		oidc := f.activeSession(t, u.ID)

		wantOK(t, "RevokeSessionsForCredential", identity.RevokeSessionsForCredential(ctx, victimKey))

		for _, s := range []*persistence.UISession{doomed, sibling} {
			if _, err := f.sessions.GetActiveByTokenHash(ctx, s.TokenHash); err == nil {
				t.Fatalf("session %s minted by the revoked key is still active", s.ID)
			}
		}
		if _, err := f.sessions.GetActiveByTokenHash(ctx, bystander.TokenHash); err != nil {
			t.Fatalf("a session minted by a DIFFERENT key was revoked: %v", err)
		}
		// The one that matters most: an OIDC session stores NULL, so it
		// must never be swept up by any key's revocation.
		if _, err := f.sessions.GetActiveByTokenHash(ctx, oidc.TokenHash); err != nil {
			t.Fatalf("an OIDC session was revoked by a credential revocation: %v", err)
		}
	})

	t.Run("revoking a credential with no sessions is not an error", func(t *testing.T) {
		// The ordinary case: most keys are machine credentials and never
		// open a browser session. If this errored, every key revocation
		// in the system would start failing.
		wantOK(t, "RevokeSessionsForCredential(none)", identity.RevokeSessionsForCredential(ctx, uniqueID("key-unused")))
	})

	t.Run("revoking is idempotent", func(t *testing.T) {
		u := f.user(t)
		key := uniqueID("key-twice")
		mint(t, u.ID, key)
		wantOK(t, "first revoke", identity.RevokeSessionsForCredential(ctx, key))
		wantOK(t, "second revoke", identity.RevokeSessionsForCredential(ctx, key))
	})

	t.Run("an empty credential id is refused rather than matching nothing", func(t *testing.T) {
		// SQL never matches NULL with =, so an empty id would sweep
		// nothing and report success — a silent no-op on a security
		// path. Refusing is louder than either behaviour and costs
		// nothing.
		u := f.user(t)
		oidc := f.activeSession(t, u.ID)

		if err := identity.RevokeSessionsForCredential(ctx, ""); err == nil {
			t.Fatal("RevokeSessionsForCredential(\"\") = nil, want a refusal")
		}
		if _, err := f.sessions.GetActiveByTokenHash(ctx, oidc.TokenHash); err != nil {
			t.Fatalf("the refused call still revoked something: %v", err)
		}
	})

	t.Run("a user disable still ends credential-minted sessions", func(t *testing.T) {
		// The two revocation paths are independent and must not have
		// become exclusive: disabling the account kills the session
		// whatever minted it.
		u := f.user(t)
		s := mint(t, u.ID, uniqueID("key-disable"))

		wantOK(t, "RevokeSessionsForUser", identity.RevokeSessionsForUser(ctx, u.ID))
		if _, err := f.sessions.GetActiveByTokenHash(ctx, s.TokenHash); err == nil {
			t.Fatal("a credential-minted session survived its user's session revocation")
		}
	})
}
