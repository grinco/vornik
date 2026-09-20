package authz

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

type capKeyStore struct {
	persistence.APIKeyRepository
	key *persistence.APIKey
	err error
}

func (s *capKeyStore) GetByID(_ context.Context, id string) (*persistence.APIKey, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.key == nil || s.key.ID != id {
		return nil, errors.New("not found")
	}
	return s.key, nil
}

type capIdentityRepo struct {
	persistence.IdentityRepository
	owner    string
	ownerErr error
}

func (r *capIdentityRepo) ResolvePrincipalRows(_ context.Context, _, _ string) ([]persistence.PrincipalRow, error) {
	if r.ownerErr != nil {
		return nil, r.ownerErr
	}
	if r.owner == "" {
		return nil, nil
	}
	return []persistence.PrincipalRow{{UserID: r.owner}}, nil
}

func capAccounts(key *persistence.APIKey, owner string) (*Accounts, *capKeyStore, *capIdentityRepo) {
	keys := &capKeyStore{key: key}
	repo := &capIdentityRepo{owner: owner}
	return &Accounts{repo: repo, audit: &recordingAudit{}, keys: keys, now: time.Now}, keys, repo
}

func liveKey() *persistence.APIKey {
	return &persistence.APIKey{ID: "key-1", ProjectID: "p"}
}

func TestCheckSessionCredential_AcceptsALiveMappedKey(t *testing.T) {
	a, _, _ := capAccounts(liveKey(), "user-1")
	if err := a.CheckSessionCredential(context.Background(), "key-1", "user-1"); err != nil {
		t.Fatalf("CheckSessionCredential() = %v, want nil", err)
	}
}

func TestCheckSessionCredential_Refusals(t *testing.T) {
	past := time.Now().UTC().Add(-time.Hour)

	tests := []struct {
		name    string
		key     *persistence.APIKey
		owner   string
		keyErr  error
		wantSay string
	}{
		{
			// The case an operator exercises when they revoke a leaked
			// key, and the whole reason the rule runs per request.
			name:    "revoked key",
			key:     &persistence.APIKey{ID: "key-1", RevokedAt: &past},
			owner:   "user-1",
			wantSay: "revoked",
		},
		{
			name:    "expired key",
			key:     &persistence.APIKey{ID: "key-1", ExpiresAt: &past},
			owner:   "user-1",
			wantSay: "expired",
		},
		{
			name:    "key is gone",
			key:     nil,
			owner:   "user-1",
			wantSay: "",
		},
		{
			// Checking only the key's liveness would let a REASSIGNED
			// key keep carrying the previous owner's browser session.
			name:    "key reassigned to somebody else",
			key:     liveKey(),
			owner:   "user-2",
			wantSay: "no longer mapped",
		},
		{
			name:    "key no longer mapped to anyone",
			key:     liveKey(),
			owner:   "",
			wantSay: "no longer mapped",
		},
		{
			// A store error must not read as "still valid": this is the
			// path an operator relies on after revoking a key, and
			// failing open makes the revocation a suggestion.
			name:    "key store is unreadable",
			key:     liveKey(),
			owner:   "user-1",
			keyErr:  errors.New("database is gone"),
			wantSay: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, keys, _ := capAccounts(tt.key, tt.owner)
			keys.err = tt.keyErr

			err := a.CheckSessionCredential(context.Background(), "key-1", "user-1")
			if err == nil {
				t.Fatal("CheckSessionCredential() = nil, want a refusal")
			}
			if !errors.Is(err, ErrCredentialNoLongerValid) {
				t.Fatalf("err = %v, want ErrCredentialNoLongerValid", err)
			}
			if tt.wantSay != "" && !strings.Contains(err.Error(), tt.wantSay) {
				t.Fatalf("err = %q, want it to mention %q", err, tt.wantSay)
			}
		})
	}
}

func TestCheckSessionCredential_AnUnreadableOwnerMappingRefuses(t *testing.T) {
	a, _, repo := capAccounts(liveKey(), "user-1")
	repo.ownerErr = errors.New("database is gone")

	if err := a.CheckSessionCredential(context.Background(), "key-1", "user-1"); !errors.Is(err, ErrCredentialNoLongerValid) {
		t.Fatalf("err = %v, want ErrCredentialNoLongerValid", err)
	}
}

func TestCheckSessionCredential_RefusesWithNoKeyStoreWired(t *testing.T) {
	// A session that says a credential minted it, on a deployment that
	// cannot check credentials. Treating it as uncapped would be a control
	// reporting "clean" over a surface it never examined.
	a, _, _ := capAccounts(liveKey(), "user-1")
	a.keys = nil

	err := a.CheckSessionCredential(context.Background(), "key-1", "user-1")
	if !errors.Is(err, ErrCredentialNoLongerValid) {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if !strings.Contains(err.Error(), "no key store") {
		t.Fatalf("err = %q, want it to name the missing wiring", err)
	}
}

func TestCheckSessionCredential_RefusesIncompleteAttribution(t *testing.T) {
	a, _, _ := capAccounts(liveKey(), "user-1")
	for _, tc := range []struct{ keyID, userID string }{{"", "user-1"}, {"key-1", ""}} {
		if err := a.CheckSessionCredential(context.Background(), tc.keyID, tc.userID); !errors.Is(err, ErrCredentialNoLongerValid) {
			t.Fatalf("CheckSessionCredential(%q,%q) = %v, want a refusal", tc.keyID, tc.userID, err)
		}
	}
}

func TestCheckSessionCredential_RefusalsAreOneSentinel(t *testing.T) {
	// A browser holding a dead cookie must not learn WHICH of revoked,
	// expired, gone or reassigned applies — that says more about another
	// account's key than the holder should know. The operator-facing
	// distinction lives in the audit trail.
	past := time.Now().UTC().Add(-time.Hour)
	for _, key := range []*persistence.APIKey{
		{ID: "key-1", RevokedAt: &past},
		{ID: "key-1", ExpiresAt: &past},
		nil,
	} {
		a, _, _ := capAccounts(key, "user-1")
		err := a.CheckSessionCredential(context.Background(), "key-1", "user-1")
		if !errors.Is(err, ErrCredentialNoLongerValid) {
			t.Fatalf("err = %v, want the single sentinel", err)
		}
	}
}
