package authz

import (
	"context"
	"testing"
)

// Operator report, 2026-09-16: "the key Identifiers are something like
// akey_20260627092635_6d9fd4db0d825d42 — which I expect to be the name that I
// set when minting one, not its id; same in the accounts, linked identities
// list."
//
// THE SEAM, and why the test lives here rather than on either UI page.
// Both surfaces render `user_identities.display`, which the schema comment in
// oidc-identity-permissions-design.md §3.1 describes as operator-friendly
// ("@grinco"). keyIdentity stamped the KEY ID into it — a verbatim copy of
// external_id, carrying no information the row did not already hold. Every
// reader that preferred Display over a resolved name therefore printed the
// opaque id, and a UI-side fix on one page would have left the other wrong
// and the bad rows still being written. So the bug is fixed where the value
// is produced, and asserted here.
//
// Why EMPTY rather than the key's name: §5.4's governing decision is that the
// observed fact is stored and the person is resolved at READ time. A key's
// name lives in api_keys and changes when it is renamed; a copy stamped at
// claim time would be stale from the first rename, and the mapping row is not
// the place that owns it.
func TestKeyIdentity_StoresNoDisplay(t *testing.T) {
	f := newClaimFixture(t)
	id := f.key(t, "akey_1", "sk-vornik-secret-1")

	if err := f.accounts.ClaimKey(context.Background(), f.owner.ID, id, "sk-vornik-secret-1",
		Actor{Principal: "owner", Source: "api"}); err != nil {
		t.Fatalf("ClaimKey: %v", err)
	}

	row, ok := f.repo.boundRows["api_key:"+id]
	if !ok {
		t.Fatalf("no identity row written for %s", id)
	}
	if row.Display != "" {
		t.Errorf("Display = %q, want empty — an api_key mapping must not copy the key id into the "+
			"operator-friendly field; every surface that prefers Display then shows akey_<ts>_<hex>", row.Display)
	}
	if row.ExternalID != id {
		t.Errorf("ExternalID = %q, want %q — the id belongs here and only here", row.ExternalID, id)
	}
}

// The admin-assign path builds the same row through the same helper. Pinning
// it separately is what stops a future fix landing on one path only — which is
// how the two claim paths have diverged before (review-20260914-3c36 F11).
func TestAssignKey_StoresNoDisplay(t *testing.T) {
	f := newClaimFixture(t)
	id := f.key(t, "akey_2", "sk-vornik-secret-2")

	if err := f.accounts.AssignKey(context.Background(), f.owner.ID, id,
		Actor{Principal: "admin", Source: "ui"}); err != nil {
		t.Fatalf("AssignKey: %v", err)
	}
	if row := f.repo.boundRows["api_key:"+id]; row.Display != "" {
		t.Errorf("Display = %q, want empty on the admin-assign path too", row.Display)
	}
}
