package api

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/authz"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
)

// TestAuthMiddleware_WiresTheOwnerAccessCheck closes a gap this test exists
// because of: the §5.0 API-key door was implemented in internal/auth AND in
// internal/authz, and the two were never JOINED. DBKeysBackend.OwnerAccess
// stayed nil in production, so the door the design claims — "revoking the
// account revokes every door that authorizes" — was unenforced on the key
// path, while the unit tests passed because they inject the check directly.
//
// That is the exact failure this design spent nine review rounds naming: an
// invariant asserted for a surface whose enforcement path does not exist. It
// reproduced at the WIRING layer, which no amount of reviewing either half in
// isolation would have caught.
//
// So this asserts the production constructor actually carries the check.
func TestAuthMiddleware_WiresTheOwnerAccessCheck(t *testing.T) {
	called := false
	cfg := AuthConfig{
		Enabled:      true,
		APIKeyLookup: stubLookupRepo{},
		OwnerAccess: func(context.Context, string) (auth.OwnerAccess, error) {
			called = true
			return auth.OwnerAccess{}, nil
		},
	}
	backends := authBackends(cfg)

	var dbKeys *auth.DBKeysBackend
	for _, b := range backends {
		if k, ok := b.(*auth.DBKeysBackend); ok {
			dbKeys = k
		}
	}
	if dbKeys == nil {
		t.Fatal("no DBKeysBackend in the chain")
	}
	if dbKeys.OwnerAccess == nil {
		t.Fatal("DBKeysBackend.OwnerAccess is nil: the API-key door is built but not wired, so a disabled owner's key — or one whose owner lost the project — still authenticates")
	}
	if _, err := dbKeys.OwnerAccess(context.Background(), "akey_1"); err != nil {
		t.Fatalf("wired check returned %v", err)
	}
	if !called {
		t.Error("the wired check was not the one supplied by AuthConfig")
	}
}

type stubLookupRepo struct{}

func (stubLookupRepo) LookupActiveByHash(context.Context, string) (*persistence.APIKey, error) {
	// ErrAPIKeyNotFound, which is what BOTH real repositories return
	// (postgres/api_key_repository.go, sqlite/api_key_repository.go). This
	// double returned persistence.ErrNotFound until 2026-09-21 and the miss
	// contract CERTIFIED it, because the contract's row for this method was
	// itself false. A double diverging from production is the exact failure
	// misscontract exists to prevent; here the table was the thing guessing.
	return nil, persistence.ErrAPIKeyNotFound
}

// A double that answers a miss differently from the real repositories would
// make this file's "the door is wired" claim rest on a lookup the daemon
// never performs.
//
// This used to call AssertMiss against the miss contract, and that is how the
// divergence survived: the contract declared MissErrNotFound for this method,
// production returns ErrAPIKeyNotFound, and the double satisfied the contract
// rather than production. LookupActiveByHash is now EXCLUDED from the contract
// (the two-behaviour vocabulary cannot express a domain sentinel), so the
// assertion names the sentinel production actually returns.
func TestStubLookupRepo_MissContract(t *testing.T) {
	_, err := stubLookupRepo{}.LookupActiveByHash(context.Background(), "missing")
	if !errors.Is(err, persistence.ErrAPIKeyNotFound) {
		t.Fatalf("stub miss = %v, want ErrAPIKeyNotFound (what both real repositories return)", err)
	}
}

// TestPrimaryChain_JoinsTheDoorFromTheAccountsService asserts the join at the
// layer that actually runs in production: a Server with an accounts service
// must produce an AuthConfig carrying the door. Testing authBackends alone
// would have kept passing through the entire period the door was unwired.
func TestPrimaryChain_JoinsTheDoorFromTheAccountsService(t *testing.T) {
	repo, audit := newAccountsStubRepo(), &accountsStubAudit{}
	s := NewServer(WithAccountsService(authz.NewAccounts(repo, audit)))

	cfg := BuildAuthConfig(&config.Config{}, serverAuthOptions(s)...)
	if cfg.OwnerAccess == nil {
		t.Fatal("a server WITH an accounts service produced an AuthConfig with no owner-disabled check: the API-key door is unenforced in production")
	}

	bare := NewServer()
	if BuildAuthConfig(&config.Config{}, serverAuthOptions(bare)...).OwnerAccess != nil {
		t.Error("a server with no accounts service must not claim a door it cannot enforce")
	}
}
