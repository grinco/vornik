package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/persistence"
)

// The API-key door of oidc-identity-permissions-design §5.0.
//
// Before this check the invariant "revoking the account revokes every door
// that authorizes" named the API-key door with nothing enforcing it: this
// backend authenticates from key_hash alone and knows nothing about any
// person, so a claimed key whose owner was disabled kept working (round-3
// finding F1).

// ownerDisabledSpy records every call so a test can prove the check ran (or
// did not) rather than inferring it from the outcome.
type ownerDisabledSpy struct {
	disabled bool
	err      error
	calls    []string
}

// check adapts the existing disabled-only spy to the OwnerAccess shape. The
// owner is mapped and granted the key's project, so these tests continue to
// exercise the DISABLED door alone (audit CA-04 widened the check; it did
// not replace this door).
func (s *ownerDisabledSpy) check(_ context.Context, keyID string) (OwnerAccess, error) {
	s.calls = append(s.calls, keyID)
	return OwnerAccess{Mapped: true, Disabled: s.disabled, Projects: []string{"proj"}}, s.err
}

func newKeyCred(t *testing.T) (string, *persistence.APIKey) {
	t.Helper()
	const projectID = "proj"
	raw, err := apikey.Generate(projectID)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return raw, &persistence.APIKey{
		ID:        "akey_owner",
		ProjectID: projectID,
		Name:      "owned",
		KeyHash:   apikey.Hash(raw),
		CreatedAt: time.Now().UTC(),
	}
}

// TestDBKeys_RefusesWhenMappedOwnerIsDisabled is the door itself.
func TestDBKeys_RefusesWhenMappedOwnerIsDisabled(t *testing.T) {
	raw, row := newKeyCred(t)
	spy := &ownerDisabledSpy{disabled: true}
	b := NewDBKeysBackend(&stubLookup{hash: apikey.Hash(raw), row: row}, nil)
	b.OwnerAccess = spy.check

	_, err := b.Authenticate(context.Background(), Credential{BearerToken: raw})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Authenticate = %v, want ErrUnauthorized", err)
	}
	if len(spy.calls) != 1 || spy.calls[0] != row.ID {
		t.Errorf("the check was called with %v, want exactly [%s]", spy.calls, row.ID)
	}
}

// TestDBKeys_RefusalDoesNotFallThroughToStatic pins §5.0's short-circuit
// (ledger bullet F5-4). ErrNoCredential falls through the chain to the static
// break-glass backend; ErrUnauthorized does not. A disabled owner must NOT be
// rescued by Basic Auth — break-glass exists for an IdP outage, not for a
// disabled account.
//
// This is a different shape from the Phase-1 no-fall-through property, where
// the backend FAILED. Here it succeeded and a later check refused, which no
// existing test covers.
func TestDBKeys_RefusalDoesNotFallThroughToStatic(t *testing.T) {
	raw, row := newKeyCred(t)
	spy := &ownerDisabledSpy{disabled: true}
	b := NewDBKeysBackend(&stubLookup{hash: apikey.Hash(raw), row: row}, nil)
	b.OwnerAccess = spy.check

	_, err := b.Authenticate(context.Background(), Credential{BearerToken: raw})
	if errors.Is(err, ErrNoCredential) {
		t.Fatal("a disabled owner returned ErrNoCredential, which falls through to the static break-glass backend")
	}
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Authenticate = %v, want ErrUnauthorized (terminal)", err)
	}
}

// TestDBKeys_UnmappedKeyIsUnaffected — the common case, and every key on a
// deployment where nobody has claimed one. The check still runs (deciding
// whether a key is mapped IS the lookup) but changes nothing.
func TestDBKeys_UnmappedKeyIsUnaffected(t *testing.T) {
	raw, row := newKeyCred(t)
	spy := &ownerDisabledSpy{disabled: false}
	b := NewDBKeysBackend(&stubLookup{hash: apikey.Hash(raw), row: row}, nil)
	b.OwnerAccess = spy.check

	id, err := b.Authenticate(context.Background(), Credential{BearerToken: raw})
	if err != nil {
		t.Fatalf("Authenticate = %v, want success", err)
	}
	if id.Subject != row.ID {
		t.Errorf("Subject = %q, want %q", id.Subject, row.ID)
	}
	if len(spy.calls) != 1 {
		t.Errorf("the check ran %d times, want exactly 1", len(spy.calls))
	}
}

// TestDBKeys_NoCheckWiredBehavesAsBefore: a deployment that never maps keys
// constructs the backend without the check, and nothing changes.
func TestDBKeys_NoCheckWiredBehavesAsBefore(t *testing.T) {
	raw, row := newKeyCred(t)
	b := NewDBKeysBackend(&stubLookup{hash: apikey.Hash(raw), row: row}, nil)

	if _, err := b.Authenticate(context.Background(), Credential{BearerToken: raw}); err != nil {
		t.Fatalf("Authenticate without an OwnerAccess check = %v, want success", err)
	}
}

// TestDBKeys_CheckErrorFailsClosed: the check is an access control, so a
// database hiccup must refuse rather than admit. The alternative — treating an
// error as "not disabled" — would make the door openable by breaking the
// lookup it depends on.
func TestDBKeys_CheckErrorFailsClosed(t *testing.T) {
	raw, row := newKeyCred(t)
	spy := &ownerDisabledSpy{err: errors.New("db down")}
	b := NewDBKeysBackend(&stubLookup{hash: apikey.Hash(raw), row: row}, nil)
	b.OwnerAccess = spy.check

	if _, err := b.Authenticate(context.Background(), Credential{BearerToken: raw}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Authenticate with a failing check = %v, want ErrUnauthorized (fail closed)", err)
	}
}
