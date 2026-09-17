package dispatcher

import (
	"context"
	"encoding/json"
	"testing"

	"vornik.io/vornik/internal/authz"
	"vornik.io/vornik/internal/persistence"
)

// PerformAccountLink differs from PerformOperatorLink in exactly one way, and
// it is the point: the account always wins.
//
// oidc-identity-permissions-design §5.0 says that once a speaker is linked,
// the account binding wins. PerformOperatorLink picks the side with MORE
// accumulated content, which is right when two chat profiles meet as equals
// and nobody has an account — and wrong here, because a speaker with chattier
// history would become the canonical row for a person who has an account.
// Content volume settles a tie between peers; it must not settle authority.

func TestPerformAccountLink_AccountWinsAgainstAFatterSpeakerProfile(t *testing.T) {
	ctx := context.Background()
	links, profiles := newFakeLinkRepo(), newFakeProfileRepo()

	// The speaker has a rich profile; the account has none at all. Under
	// PerformOperatorLink's content rule the speaker would win outright.
	profiles.profiles["telegram:42"] = &persistence.OperatorProfile{
		OperatorID: "telegram:42",
		Notes:      "a long accumulated history of preferences and context",
		Structured: []byte(`{"tone":"terse","time_zone":"Europe/Prague","verbosity":"short"}`),
	}

	res, err := PerformAccountLink(ctx, OperatorLinkRepos{Links: links, Profiles: profiles},
		"account:user_1", "telegram:42", "link-code")
	if err != nil {
		t.Fatalf("PerformAccountLink: %v", err)
	}
	if res.Canonical != "account:user_1" {
		t.Fatalf("canonical = %q, want the ACCOUNT to win regardless of content volume", res.Canonical)
	}

	// The speaker must now resolve to the account, which is what makes
	// §5.0's claim true through the UNCHANGED dispatcher resolver.
	row, err := links.Get(ctx, "telegram:42")
	if err != nil {
		t.Fatalf("Get(telegram:42): %v", err)
	}
	if row.OperatorID != "account:user_1" {
		t.Errorf("telegram:42 resolves to %q, want account:user_1", row.OperatorID)
	}

	// And the speaker's accumulated profile must have followed, not been
	// abandoned: linking is a merge, not a reset.
	merged := profiles.profiles["account:user_1"]
	if merged == nil {
		t.Fatal("the account has no profile row: the speaker's history was dropped rather than merged")
	}
	var keys map[string]string
	if err := json.Unmarshal(merged.Structured, &keys); err != nil {
		t.Fatalf("merged profile structured keys unreadable: %v", err)
	}
	if keys["time_zone"] != "Europe/Prague" {
		t.Errorf("merged profile = %v, want the speaker's accumulated keys folded in", keys)
	}
}

// TestPerformOperatorLink_StillPicksByContent — the peer-to-peer rule is
// unchanged. The two functions share one merge, so a change to the shared
// half must not quietly reverse the chat→chat tie-break.
func TestPerformOperatorLink_StillPicksByContent(t *testing.T) {
	ctx := context.Background()
	links, profiles := newFakeLinkRepo(), newFakeProfileRepo()
	profiles.profiles["telegram:42"] = &persistence.OperatorProfile{
		OperatorID: "telegram:42",
		Notes:      "much more accumulated content than the other side has",
		Structured: []byte(`{"tone":"terse","time_zone":"Europe/Prague"}`),
	}

	// Issuer has nothing; claimant is rich. Content wins, so the CLAIMANT
	// becomes canonical — the opposite of PerformAccountLink.
	res, err := PerformOperatorLink(ctx, OperatorLinkRepos{Links: links, Profiles: profiles},
		"slack:U1", "telegram:42", "self")
	if err != nil {
		t.Fatalf("PerformOperatorLink: %v", err)
	}
	if res.Canonical != "telegram:42" {
		t.Errorf("canonical = %q, want the richer side (telegram:42) — the peer rule must not change", res.Canonical)
	}
}

// TestPerformOperatorLink_CannotUndoAnAccountLink — the defect this guards is
// reachable by neither flow alone, which is why neither flow's own tests saw
// it.
//
// A speaker who redeems a §5.2 link code resolves to "account:<user>". If
// they later run the chat→chat /link with a chattier peer, pickWinner's
// content rule would make the ACCOUNT the loser and repoint every row
// pointing at it onto a chat speaker — so the account stops being canonical
// for a person who has one. §5.0 says the account binding wins; content
// volume settles a tie between peers and must not settle authority, wherever
// the authority turns up.
func TestPerformOperatorLink_CannotUndoAnAccountLink(t *testing.T) {
	ctx := context.Background()
	links, profiles := newFakeLinkRepo(), newFakeProfileRepo()

	// telegram:42 is already linked to an account, with a thin profile.
	if err := links.Upsert(ctx, &persistence.OperatorIdentityLink{
		ChannelSpeakerID: "telegram:42", OperatorID: "account:user_1", LinkedBy: "link-code",
	}); err != nil {
		t.Fatalf("seed link: %v", err)
	}
	// slack:U1 is unlinked and has a much richer profile — under the content
	// rule it would win outright.
	profiles.profiles["slack:U1"] = &persistence.OperatorProfile{
		OperatorID: "slack:U1",
		Notes:      "a great deal of accumulated history and context and preferences",
		Structured: []byte(`{"tone":"terse","time_zone":"Europe/Prague","verbosity":"short"}`),
	}

	res, err := PerformOperatorLink(ctx, OperatorLinkRepos{Links: links, Profiles: profiles},
		"slack:U1", "telegram:42", "self")
	if err != nil {
		t.Fatalf("PerformOperatorLink: %v", err)
	}
	if res.Canonical != "account:user_1" {
		t.Fatalf("canonical = %q, want account:user_1 — a chat link must not be able to "+
			"demote the account a speaker is bound to", res.Canonical)
	}
	// And the previously-linked speaker must still resolve to the account.
	row, err := links.Get(ctx, "telegram:42")
	if err != nil {
		t.Fatalf("Get(telegram:42): %v", err)
	}
	if row.OperatorID != "account:user_1" {
		t.Errorf("telegram:42 now resolves to %q; the account link was undone", row.OperatorID)
	}
	// The peer's history is still merged in — this is a merge, not a refusal.
	if !res.Merged {
		t.Error("the richer peer's profile was dropped rather than folded onto the account")
	}
}

// TestPerformOperatorLink_RefusesToMergeTwoAccounts — two accounts is either
// two different people or one person who should unlink, and an
// unauthenticated chat OTP is not the authority to decide which.
func TestPerformOperatorLink_RefusesToMergeTwoAccounts(t *testing.T) {
	ctx := context.Background()
	links, profiles := newFakeLinkRepo(), newFakeProfileRepo()
	for speaker, account := range map[string]string{"telegram:42": "account:user_1", "slack:U1": "account:user_2"} {
		if err := links.Upsert(ctx, &persistence.OperatorIdentityLink{
			ChannelSpeakerID: speaker, OperatorID: account, LinkedBy: "link-code",
		}); err != nil {
			t.Fatalf("seed %s: %v", speaker, err)
		}
	}

	if _, err := PerformOperatorLink(ctx, OperatorLinkRepos{Links: links, Profiles: profiles},
		"slack:U1", "telegram:42", "self"); err == nil {
		t.Fatal("two accounts were merged by a chat OTP")
	}
	// Neither side moved.
	for speaker, want := range map[string]string{"telegram:42": "account:user_1", "slack:U1": "account:user_2"} {
		row, err := links.Get(ctx, speaker)
		if err != nil || row.OperatorID != want {
			t.Errorf("%s = %+v (err %v), want %s untouched", speaker, row, err, want)
		}
	}
}

// TestAccountCanonicalPrefix_MatchesTheAuthzHelper — two spellings of one
// namespace is how they drift. authz mints the id; this package recognises
// it; nothing but this test connects them.
func TestAccountCanonicalPrefix_MatchesTheAuthzHelper(t *testing.T) {
	minted := authz.AccountOperatorID("user_1")
	if !isAccountCanonical(minted) {
		t.Fatalf("authz mints %q and this package does not recognise it as an account canonical; "+
			"the account-wins rule would silently stop applying", minted)
	}
	if isAccountCanonical("telegram:42") || isAccountCanonical("slack:U1") || isAccountCanonical("") {
		t.Error("a chat speaker id was recognised as an account canonical")
	}
}

// TestPerformAccountLink_SecondChannelJoinsTheSameAccount — a person links
// Telegram, then Slack, to one account. The second link must behave like the
// first: the account stays canonical and both speakers resolve to it.
//
// Worth pinning because the write that creates the binding is not the obvious
// one. pointAt(claimantSpeaker, winner) is SKIPPED when the claimant is
// already its own canonical, so the row is actually written by the
// loser-canonical repoint. A refactor that "simplified" either branch would
// break linking while every single-link test kept passing.
func TestPerformAccountLink_SecondChannelJoinsTheSameAccount(t *testing.T) {
	ctx := context.Background()
	links, profiles := newFakeLinkRepo(), newFakeProfileRepo()
	acct := authz.AccountOperatorID("user_1")
	repos := OperatorLinkRepos{Links: links, Profiles: profiles}

	for _, speaker := range []string{"telegram:42", "slack:U1"} {
		if _, err := PerformAccountLink(ctx, repos, acct, speaker, "link-code"); err != nil {
			t.Fatalf("PerformAccountLink(%s): %v", speaker, err)
		}
	}
	for _, speaker := range []string{"telegram:42", "slack:U1"} {
		row, err := links.Get(ctx, speaker)
		if err != nil || row.OperatorID != acct {
			t.Errorf("%s = %+v (err %v), want %s", speaker, row, err, acct)
		}
	}
	// Re-redeeming from an already-linked speaker is a no-op success, not a
	// self-referential row or an error.
	if _, err := PerformAccountLink(ctx, repos, acct, "telegram:42", "link-code"); err != nil {
		t.Errorf("re-link: %v", err)
	}
	if _, err := links.Get(ctx, acct); err == nil {
		t.Error("a self-referential row was written for the account canonical itself")
	}
}
