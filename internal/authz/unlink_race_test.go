package authz

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// Regression: audit 2026-09-15 CA-10 — "Unlink Ownership Check Can Race a
// Reassignment".
//
// Self-unlink read an account snapshot from ListUsers, confirmed the binding
// belonged to the caller, and THEN revoked by (channel, external_id) with no
// expected owner in the predicate. An administrator reassigning that key in
// between meant the original owner's unlink revoked the NEW owner's binding
// and killed the new owner's sessions — while the audit row still named the
// old owner as the target. Both drivers used the unqualified predicate, so
// this was not a driver quirk.
//
// The ownership check and the revoke have to be one atomic operation.
func TestUnlink_DoesNotRevokeABindingReassignedMidFlight(t *testing.T) {
	ctx := context.Background()
	base := newMemRepo()
	accounts := NewAccounts(base, &memAudit{})

	for _, u := range []*persistence.User{
		{ID: "user_old", DisplayName: "Old"},
		{ID: "user_new", DisplayName: "New"},
	} {
		if err := base.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	base.bindings["api_key:akey_1"] = "user_old"

	// The interleaving, made deterministic rather than raced for: the old
	// owner's client read a view in which the binding was theirs, and an
	// administrator's AssignKey landed before their unlink reached the
	// database. Whether the service re-reads first is exactly the bug — the
	// decision has to be made by the WRITE, against the state the write
	// sees.
	views, err := base.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = views // the stale snapshot the old code trusted
	base.bindings["api_key:akey_1"] = "user_new"

	err = accounts.Unlink(ctx, "user_old", "api_key", "akey_1", Actor{Principal: "user_old"})
	if err == nil {
		t.Fatal("unlink succeeded against a binding that had been reassigned: it revoked another account's binding")
	}
	if !errors.Is(err, ErrIdentityNotOwned) && !errors.Is(err, persistence.ErrIdentityNotFound) {
		t.Fatalf("want an ownership refusal, got %v", err)
	}
	if base.revoked["api_key:akey_1"] {
		t.Fatal("the new owner's binding was revoked by the old owner's unlink")
	}
	if owner := base.bindings["api_key:akey_1"]; owner != "user_new" {
		t.Fatalf("the reassignment was undone (owner now %q)", owner)
	}
}

// The ordinary case must keep working: an owner unlinking their own binding.
func TestUnlink_OwnerCanStillUnlinkTheirOwnBinding(t *testing.T) {
	ctx := context.Background()
	repo := newMemRepo()
	accounts := NewAccounts(repo, &memAudit{})
	if err := repo.CreateUser(ctx, &persistence.User{ID: "user_old", DisplayName: "Old"}); err != nil {
		t.Fatal(err)
	}
	repo.bindings["api_key:akey_1"] = "user_old"

	if err := accounts.Unlink(ctx, "user_old", "api_key", "akey_1", Actor{Principal: "user_old"}); err != nil {
		t.Fatalf("owner unlink failed: %v", err)
	}
	if !repo.revoked["api_key:akey_1"] {
		t.Fatal("the binding was not revoked")
	}
}
