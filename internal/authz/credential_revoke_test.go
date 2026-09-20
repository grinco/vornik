package authz

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

func TestRevokeCredential_RevokesTheKeyThenItsSessions(t *testing.T) {
	f := newCredentialRevokeFixture(t)

	if err := f.accounts.RevokeCredential(context.Background(), "key-1", Actor{Principal: "user:op", Source: "api"}); err != nil {
		t.Fatalf("RevokeCredential() = %v", err)
	}
	if !f.keys.revoked["key-1"] {
		t.Fatal("the key was not revoked")
	}
	if !f.repo.sessionsRevokedFor["key-1"] {
		t.Fatal("the key's browser sessions were not revoked")
	}
	// Order is the security property: a crash between the two must leave a
	// DEAD key and live session rows, never the reverse.
	if (*f.order)[0] != "key" || (*f.order)[1] != "sessions" {
		t.Fatalf("order = %v, want the key revoked before its sessions", *f.order)
	}
}

func TestRevokeCredential_ASessionSweepFailureLeavesTheKeyRevoked(t *testing.T) {
	// The asymmetry §5.1 relies on: the session rows are refused on their
	// next request by the capping rule, so failing to clear them is
	// recoverable. A live key is not.
	f := newCredentialRevokeFixture(t)
	boom := errors.New("database is gone")
	f.repo.revokeSessionsErr = boom

	err := f.accounts.RevokeCredential(context.Background(), "key-1", Actor{Principal: "user:op", Source: "api"})
	if err == nil {
		t.Fatal("RevokeCredential() = nil, want the sweep failure surfaced")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the underlying cause", err)
	}
	if !f.keys.revoked["key-1"] {
		t.Fatal("the key was un-revoked by a session sweep failure")
	}
	if !strings.Contains(err.Error(), "capping rule") {
		t.Fatalf("err = %q, want it to say why the leftover rows are safe", err)
	}
}

func TestRevokeCredential_AKeyFailureDoesNotTouchSessions(t *testing.T) {
	f := newCredentialRevokeFixture(t)
	f.keys.revokeErr = errors.New("no such key")

	if err := f.accounts.RevokeCredential(context.Background(), "key-1", Actor{Principal: "user:op", Source: "api"}); err == nil {
		t.Fatal("RevokeCredential() = nil, want the key failure")
	}
	if f.repo.sessionsRevokedFor["key-1"] {
		t.Fatal("sessions were revoked for a key that was not")
	}
}

func TestRevokeCredential_AuditsEvenWhenTheSweepFails(t *testing.T) {
	// The key revocation HAPPENED. An audit trail that recorded it only on
	// full success would omit exactly the case an operator must see.
	f := newCredentialRevokeFixture(t)
	f.repo.revokeSessionsErr = errors.New("database is gone")

	_ = f.accounts.RevokeCredential(context.Background(), "key-1", Actor{Principal: "user:op", Source: "api"})
	if len(f.audit.entries) != 1 {
		t.Fatalf("%d audit entries, want 1", len(f.audit.entries))
	}
	if got := f.audit.entries[0].action; got != "credential.revoke" {
		t.Fatalf("action = %q", got)
	}
}

func TestRevokeCredential_RefusesAnEmptyKeyID(t *testing.T) {
	f := newCredentialRevokeFixture(t)
	if err := f.accounts.RevokeCredential(context.Background(), "", Actor{Principal: "user:op", Source: "api"}); err == nil {
		t.Fatal("RevokeCredential(\"\") = nil, want a refusal")
	}
	if len(*f.order) != 0 {
		t.Fatalf("an empty key id reached the stores: %v", *f.order)
	}
}

func TestRevokeCredential_RefusesWhenTheKeyStoreIsNotWired(t *testing.T) {
	f := newCredentialRevokeFixture(t)
	f.accounts.keys = nil
	err := f.accounts.RevokeCredential(context.Background(), "key-1", Actor{Principal: "user:op", Source: "api"})
	if !errors.Is(err, ErrCredentialRevokeUnavailable) {
		t.Fatalf("err = %v, want ErrCredentialRevokeUnavailable", err)
	}
}

// --- fixture ---

type credentialRevokeFixture struct {
	accounts *Accounts
	keys     *revokeKeyStore
	repo     *revokeIdentityRepo
	audit    *recordingAudit
	order    *[]string
}

type revokeKeyStore struct {
	persistence.APIKeyRepository
	revoked   map[string]bool
	revokeErr error
	order     *[]string
}

func (s *revokeKeyStore) Revoke(_ context.Context, keyID string) error {
	if s.revokeErr != nil {
		return s.revokeErr
	}
	s.revoked[keyID] = true
	*s.order = append(*s.order, "key")
	return nil
}

type revokeIdentityRepo struct {
	persistence.IdentityRepository
	sessionsRevokedFor map[string]bool
	revokeSessionsErr  error
	order              *[]string
}

func (r *revokeIdentityRepo) RevokeSessionsForCredential(_ context.Context, keyID string) error {
	*r.order = append(*r.order, "sessions")
	if r.revokeSessionsErr != nil {
		return r.revokeSessionsErr
	}
	r.sessionsRevokedFor[keyID] = true
	return nil
}

type auditEntry struct{ action, target string }

type recordingAudit struct {
	persistence.AdminAuditRepository
	entries []auditEntry
}

func (a *recordingAudit) Insert(_ context.Context, e *persistence.AdminAuditEntry) error {
	a.entries = append(a.entries, auditEntry{action: e.Action, target: e.Target})
	return nil
}

func newCredentialRevokeFixture(t *testing.T) *credentialRevokeFixture {
	t.Helper()
	order := &[]string{}
	keys := &revokeKeyStore{revoked: map[string]bool{}, order: order}
	repo := &revokeIdentityRepo{sessionsRevokedFor: map[string]bool{}, order: order}
	audit := &recordingAudit{}
	f := &credentialRevokeFixture{
		accounts: &Accounts{repo: repo, audit: audit, keys: keys, now: time.Now},
		keys:     keys, repo: repo, audit: audit, order: order,
	}
	return f
}
