package authz

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// The disabled invariant has TWO enforcement sites — the resolver for web and
// chat, and the API-key door — and two sites that can drift are how the gap
// reopens a round later (round-4 finding F4-2). These tests pin that both read
// ONE predicate over `disabled_at`, and that neither consults
// `access_revoked_at`, which gates a different question at a different time
// (§5.0's two-column table).

// TestUserDisabled_ReadsDisabledAndNothingElse pins the shared predicate
// itself. It is a pure function over the resolver rows, so the two call sites
// cannot diverge in the way a duplicated `rows[0].Disabled` check could.
func TestUserDisabled_ReadsDisabledAndNothingElse(t *testing.T) {
	if UserDisabled(nil) {
		t.Error("no rows must not read as disabled — that is unknown, not denied")
	}
	if UserDisabled([]persistence.PrincipalRow{{UserID: "u", Disabled: false}}) {
		t.Error("an active user must not read as disabled")
	}
	if !UserDisabled([]persistence.PrincipalRow{{UserID: "u", Disabled: true}}) {
		t.Error("disabled_at set must read as disabled")
	}
}

// TestKeyOwnerDisabled_RefusesOnlyForAMappedDisabledOwner is the API-key half.
// An UNMAPPED key is the common case and must stay unaffected — that is every
// key on a deployment where nobody has claimed one yet.
func TestKeyOwnerDisabled_RefusesOnlyForAMappedDisabledOwner(t *testing.T) {
	f := newClaimFixture(t)
	ctx := context.Background()
	id := f.key(t, "akey_disabled", "sk-vornik-d")

	// Unmapped: no owner, so nothing to refuse.
	disabled, err := f.accounts.KeyOwnerDisabled(ctx, id)
	if err != nil {
		t.Fatalf("KeyOwnerDisabled: %v", err)
	}
	if disabled {
		t.Error("an unmapped key must never report a disabled owner")
	}

	if err := f.accounts.ClaimKey(ctx, f.owner.ID, id, "sk-vornik-d", Actor{Principal: "owner"}); err != nil {
		t.Fatalf("ClaimKey: %v", err)
	}
	if d, err := f.accounts.KeyOwnerDisabled(ctx, id); err != nil || d {
		t.Fatalf("mapped to an ACTIVE owner: disabled=%v err=%v, want false", d, err)
	}

	f.repo.disabled[f.owner.ID] = true
	d, err := f.accounts.KeyOwnerDisabled(ctx, id)
	if err != nil {
		t.Fatalf("KeyOwnerDisabled: %v", err)
	}
	if !d {
		t.Error("a key mapped to a DISABLED owner must refuse — this is the door §5.0 claimed and could not enforce")
	}
}

// TestBothSites_IgnoreAccessRevokedAt is the negative half of the paired test
// (F5-1). access_revoked_at gates whether a bootstrap login may re-grant
// access; it is NOT a second access gate, and a site that started reading it
// would deny where the design says allow.
func TestBothSites_IgnoreAccessRevokedAt(t *testing.T) {
	f := newClaimFixture(t)
	ctx := context.Background()
	id := f.key(t, "akey_revokedmarker", "sk-vornik-rm")
	if err := f.accounts.ClaimKey(ctx, f.owner.ID, id, "sk-vornik-rm", Actor{Principal: "owner"}); err != nil {
		t.Fatalf("ClaimKey: %v", err)
	}

	// Set ONLY the deliberate-revocation marker, leaving disabled_at clear.
	now := time.Now().UTC()
	f.repo.accessRevoked = map[string]*time.Time{f.owner.ID: &now}

	// The key door stays open...
	if d, err := f.accounts.KeyOwnerDisabled(ctx, id); err != nil || d {
		t.Errorf("the key door consulted access_revoked_at: disabled=%v err=%v", d, err)
	}
	// ...and so does the resolver, because the shared predicate reads
	// disabled_at only.
	rows, err := f.repo.ResolvePrincipalRows(ctx, keyChannel, id)
	if err != nil {
		t.Fatalf("ResolvePrincipalRows: %v", err)
	}
	if UserDisabled(rows) {
		t.Error("the resolver predicate consulted access_revoked_at")
	}
}
