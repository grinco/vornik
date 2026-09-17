package sqlite_test

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// TestDisable_LeavesTheProfileLinkAndTheIdentityRows — §5.0's revocation
// invariant is scoped to "every door that AUTHORIZES", and §5.7 asserts that
// the operator_identity_link profile row is deliberately UNCHANGED: disabling
// an account is not erasure, the profile is data rather than an access grant,
// and a disabled user's speaker is already refused at the resolver before any
// profile is read. The profile's own removal path is `operator forget`.
//
// Nothing asserted it. That was tolerable while redemption wrote no profile
// row — the claim was vacuous, because there was nothing to leave alone. It
// stopped being vacuous when redemption started repointing the profile onto
// the account, so it gets a test now: two repositories over one database,
// asserting a disable touches neither the binding nor the profile link.
//
// The identity rows are asserted too, for the symmetric reason in §5.0: they
// are RETAINED through a disable ("DENIED AT RESOLVE; the rows are retained"),
// so a disable that deleted them would break re-enable and lose the record of
// who the person was.
func TestDisable_LeavesTheProfileLinkAndTheIdentityRows(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	identity := sqlite.NewIdentityRepository(db.DB)
	links := sqlite.NewOperatorIdentityLinkRepository(db.DB)

	user := &persistence.User{ID: "user_1", DisplayName: "Vadim", CreatedAt: time.Now().UTC()}
	if err := identity.CreateUser(ctx, user); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	bound, err := identity.BindIdentity(ctx, &persistence.UserIdentity{
		ID: "uident_1", UserID: user.ID, Channel: "telegram", ExternalID: "42",
		Display: "Vadim", CreatedAt: time.Now().UTC(),
	})
	if err != nil || !bound {
		t.Fatalf("BindIdentity: bound=%v err=%v", bound, err)
	}
	// The row redemption now writes: the speaker's profile repointed onto
	// the account's canonical id.
	if err := links.Upsert(ctx, &persistence.OperatorIdentityLink{
		ChannelSpeakerID: "telegram:42",
		OperatorID:       "account:" + user.ID,
		LinkedBy:         "link-code",
	}); err != nil {
		t.Fatalf("Upsert link: %v", err)
	}

	if err := identity.SetUserDisabled(ctx, user.ID, true); err != nil {
		t.Fatalf("SetUserDisabled: %v", err)
	}

	// The door is shut.
	rows, err := identity.ResolvePrincipalRows(ctx, "telegram", "42")
	if err != nil {
		t.Fatalf("ResolvePrincipalRows: %v", err)
	}
	if len(rows) == 0 || !rows[0].Disabled {
		t.Fatalf("after disable the resolver reports %+v; the door must be shut", rows)
	}

	// And nothing was erased.
	link, err := links.Get(ctx, "telegram:42")
	if err != nil {
		t.Fatalf("the profile link was destroyed by a disable: %v — disable is not erasure, and "+
			"the profile's removal path is `operator forget`", err)
	}
	if link.OperatorID != "account:"+user.ID {
		t.Errorf("the profile link was repointed by a disable: %+v", link)
	}

	// Re-enable restores the door without re-linking anything.
	if err := identity.SetUserDisabled(ctx, user.ID, false); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	rows, err = identity.ResolvePrincipalRows(ctx, "telegram", "42")
	if err != nil {
		t.Fatalf("ResolvePrincipalRows after re-enable: %v", err)
	}
	if len(rows) == 0 || rows[0].Disabled {
		t.Errorf("after re-enable the resolver reports %+v; the retained rows should make this whole", rows)
	}
}

// TestOperatorIdentityLink_RoundTrips — the repository was a no-op stub until
// 2026-09-15: Upsert returned nil and stored nothing, Get returned
// ErrNotFound, and every caller read that as "this speaker is its own
// canonical id". `/link` between two chats reported a consolidation it never
// performed, and Phase-4 redemption audited a profile repoint that never
// happened.
//
// These are the assertions a stub passes vacuously and an implementation has
// to earn.
func TestOperatorIdentityLink_RoundTrips(t *testing.T) {
	ctx := context.Background()
	links := sqlite.NewOperatorIdentityLinkRepository(newTestDB(t).DB)

	if _, err := links.Get(ctx, "telegram:absent"); err == nil {
		t.Fatal("an absent link returned no error")
	}

	// The caller's LinkedAt is deliberately IGNORED — the repository stamps
	// its own clock, as the contract says and as the Postgres twin does. A
	// first cut honoured the caller's value, which let the two drivers
	// disagree about a column an auditor reads.
	before := time.Now().UTC().Add(-time.Second)
	if err := links.Upsert(ctx, &persistence.OperatorIdentityLink{
		ChannelSpeakerID: "telegram:42", OperatorID: "account:user_1",
		LinkedBy: "link-code", LinkedAt: time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, err := links.Get(ctx, "telegram:42")
	if err != nil {
		t.Fatalf("Get after Upsert: %v — the write was acknowledged and discarded", err)
	}
	if got.OperatorID != "account:user_1" || got.LinkedBy != "link-code" {
		t.Errorf("round trip = %+v, want account:user_1 / link-code", got)
	}
	if got.LinkedAt.Before(before) {
		t.Errorf("linked_at = %v, want the repository's own clock (>= %v), not the caller's 1999",
			got.LinkedAt, before)
	}
	first := got.LinkedAt

	// A repoint keeps the ORIGINAL linked_at: the column records when the
	// operator first confirmed the relationship, not when a merge last
	// rewrote the row. Same contract as the Postgres twin.
	if err := links.Upsert(ctx, &persistence.OperatorIdentityLink{
		ChannelSpeakerID: "telegram:42", OperatorID: "account:user_2",
		LinkedBy: "cli",
	}); err != nil {
		t.Fatalf("repoint: %v", err)
	}
	got, err = links.Get(ctx, "telegram:42")
	if err != nil {
		t.Fatalf("Get after repoint: %v", err)
	}
	if got.OperatorID != "account:user_2" {
		t.Errorf("repoint did not move the link: %+v", got)
	}
	if !got.LinkedAt.Equal(first) {
		t.Errorf("linked_at = %v after repoint, want the original %v", got.LinkedAt, first)
	}

	// ListForOperator is what the merge path walks to repoint a loser's rows.
	if err := links.Upsert(ctx, &persistence.OperatorIdentityLink{
		ChannelSpeakerID: "slack:U1", OperatorID: "account:user_2", LinkedBy: "link-code",
	}); err != nil {
		t.Fatalf("Upsert slack: %v", err)
	}
	list, err := links.ListForOperator(ctx, "account:user_2")
	if err != nil {
		t.Fatalf("ListForOperator: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("ListForOperator returned %d rows, want 2 — the merge path repoints what this returns", len(list))
	}

	// Delete is idempotent; DeleteAllForOperator clears the set.
	if err := links.Delete(ctx, "telegram:absent"); err != nil {
		t.Errorf("deleting an absent link errored: %v", err)
	}
	if err := links.DeleteAllForOperator(ctx, "account:user_2"); err != nil {
		t.Fatalf("DeleteAllForOperator: %v", err)
	}
	if list, err = links.ListForOperator(ctx, "account:user_2"); err != nil || len(list) != 0 {
		t.Errorf("after DeleteAllForOperator: %d rows, err %v", len(list), err)
	}
}
