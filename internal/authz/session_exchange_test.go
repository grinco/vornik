package authz

import (
	"context"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

type exchangeIdentityRepo struct {
	persistence.IdentityRepository
	owner   string
	account *persistence.UserAdminView
}

func (r *exchangeIdentityRepo) ResolvePrincipalRows(_ context.Context, _, _ string) ([]persistence.PrincipalRow, error) {
	if r.owner == "" {
		return nil, nil
	}
	return []persistence.PrincipalRow{{UserID: r.owner}}, nil
}

func (r *exchangeIdentityRepo) ListUsers(context.Context) ([]persistence.UserAdminView, error) {
	if r.account == nil {
		return nil, nil
	}
	return []persistence.UserAdminView{*r.account}, nil
}

const exchangeUserID = "user-1"

// exchangeAccounts builds the happy path: the key maps to exchangeUserID and
// that account exists. Cases that need a different mapping replace a.repo in
// their own mutate closure, which is why this takes no owner parameter.
func exchangeAccounts(account *persistence.UserAdminView) *Accounts {
	return &Accounts{
		repo:   &exchangeIdentityRepo{owner: exchangeUserID, account: account},
		audit:  &recordingAudit{},
		keys:   &capKeyStore{key: liveKey()},
		now:    time.Now,
		claims: newClaimLimiter(),
	}
}

func activeAccount(role string) *persistence.UserAdminView {
	return &persistence.UserAdminView{UserID: exchangeUserID, Role: role}
}

func TestResolveExchangeSubject_AcceptsALiveMappedKey(t *testing.T) {
	a := exchangeAccounts(activeAccount("admin"))

	subject, err := a.ResolveExchangeSubject(context.Background(), "key-1")
	if err != nil {
		t.Fatalf("ResolveExchangeSubject() = %v", err)
	}
	if subject.UserID != "user-1" || subject.Role != "admin" {
		t.Fatalf("subject = %+v", subject)
	}
}

func TestResolveExchangeSubject_RefusalsShareOneSentinel(t *testing.T) {
	// The exchange is an oracle by construction: success says "a live key
	// mapped to an account", failure says otherwise. What is avoidable is
	// a SECOND signal telling a prober WHICH of the causes applied, since
	// the differences are facts about someone else's account.
	past := time.Now().UTC().Add(-time.Hour)

	tests := []struct {
		name   string
		mutate func(*Accounts)
		keyID  string
	}{
		{name: "no key id", keyID: ""},
		{
			name:  "key mapped to nobody",
			keyID: "key-1",
			mutate: func(a *Accounts) {
				a.repo = &exchangeIdentityRepo{owner: "", account: activeAccount("user")}
			},
		},
		{
			name:  "account is disabled",
			keyID: "key-1",
			mutate: func(a *Accounts) {
				acct := activeAccount("user")
				acct.Disabled = true
				a.repo = &exchangeIdentityRepo{owner: "user-1", account: acct}
			},
		},
		{
			name:  "account is gone",
			keyID: "key-1",
			mutate: func(a *Accounts) {
				a.repo = &exchangeIdentityRepo{owner: "user-1", account: nil}
			},
		},
		{
			name:   "key is revoked",
			keyID:  "key-1",
			mutate: func(a *Accounts) { a.keys = &capKeyStore{key: &persistence.APIKey{ID: "key-1", RevokedAt: &past}} },
		},
		{
			name:   "key is expired",
			keyID:  "key-1",
			mutate: func(a *Accounts) { a.keys = &capKeyStore{key: &persistence.APIKey{ID: "key-1", ExpiresAt: &past}} },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := exchangeAccounts(activeAccount("user"))
			if tt.mutate != nil {
				tt.mutate(a)
			}
			_, err := a.ResolveExchangeSubject(context.Background(), tt.keyID)
			if !errors.Is(err, ErrExchangeRefused) {
				t.Fatalf("err = %v, want the single ErrExchangeRefused", err)
			}
		})
	}
}

func TestResolveExchangeSubject_SharesTheClaimPathsBound(t *testing.T) {
	// Both doors prove possession of a key, so they share one bucket keyed
	// by key id — a per-door bucket would let an attacker spend one bound
	// and then the other.
	a := exchangeAccounts(activeAccount("user"))

	var limited bool
	for i := 0; i < ClaimAttemptLimit+2; i++ {
		if _, err := a.ResolveExchangeSubject(context.Background(), "key-1"); errors.Is(err, ErrExchangeRateLimited) {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatalf("the exchange never hit the shared bound after %d attempts", ClaimAttemptLimit+2)
	}

	// And the bound is per KEY, not global: a different key is unaffected,
	// which is what makes the shared bucket a targeted defence rather than
	// a global outage.
	if _, err := a.ResolveExchangeSubject(context.Background(), "key-other"); errors.Is(err, ErrExchangeRateLimited) {
		t.Fatal("exhausting one key's bound also closed a different key's door")
	}
}

func TestResolveExchangeSubject_MintsOnlyWhatTheCapWouldKeep(t *testing.T) {
	// A session must not be mintable under conditions that would refuse it
	// on its very next request, so the exchange asks the SAME question the
	// per-request capping rule asks.
	past := time.Now().UTC().Add(-time.Hour)
	a := exchangeAccounts(activeAccount("user"))
	a.keys = &capKeyStore{key: &persistence.APIKey{ID: "key-1", RevokedAt: &past}}

	if _, err := a.ResolveExchangeSubject(context.Background(), "key-1"); !errors.Is(err, ErrExchangeRefused) {
		t.Fatalf("err = %v, want a refusal for a key the capping rule would reject", err)
	}
}
