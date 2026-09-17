package service

import (
	"context"
	"fmt"

	"vornik.io/vornik/internal/authz"
	"vornik.io/vornik/internal/dispatcher"
)

// profileLinker implements authz.ProfileLinker by folding a redeemed chat
// speaker's operator profile onto the account's canonical id
// (oidc-identity-permissions-design §5.2; operator-profile-design Phase A).
//
// It lives here rather than in internal/authz because the merge is the
// dispatcher's — one merge implementation, shared with `/link`'s chat→chat
// flow, since two would drift on the tie-breaking rules that are the whole
// substance of the operation.
// It is intentionally ADAPTER-ONLY: it translates shapes and delegates, and
// holds nothing but the repositories. Logic that appears here is logic in the
// wrong package — the merge rules belong beside the merge.
//
// On auditing: the repoint is NOT audited here, and does not need to be. The
// redemption that triggers it writes one admin-audit row carrying
// profile_repoint = merged | no_profile_store | failed, so the profile half of
// §5.5's "every mutation is audited" is covered by the row for the mutation
// that caused it. The Audit repository threaded into OperatorLinkRepos below
// is the PROFILE-USE audit (profile_use_audit), a different table with a
// different purpose — the merge uses it to reassign a loser's usage history,
// not to record that a link happened (review-20260915-b945, F-Audit).
type profileLinker struct{ repos dispatcher.OperatorLinkRepos }

// LinkSpeakerToAccount repoints the speaker onto "account:<user_id>" and
// merges the profile content, with the ACCOUNT always the winner.
//
// The speaker id shape is the one the dispatcher resolves against
// (`<channel>:<external_id>`, e.g. `telegram:42`), so the existing
// resolveCanonicalOperatorID — unchanged — returns the account id from the
// next turn on. That is how §5.0's "once linked, the account binding wins"
// becomes true without a second resolver consulting a second table.
func (p profileLinker) LinkSpeakerToAccount(ctx context.Context, channel, externalID, userID string) error {
	if p.repos.Links == nil || p.repos.Profiles == nil {
		return nil // nothing to move; not a failure
	}
	speaker := channel + ":" + externalID
	if _, err := dispatcher.PerformAccountLink(
		ctx, p.repos, authz.AccountOperatorID(userID), speaker, "link-code",
	); err != nil {
		return fmt.Errorf("profile link: %w", err)
	}
	return nil
}

// profileLinkerFor returns the linker when both profile repositories exist,
// and nil otherwise — a deployment without them has no profile to move, which
// is not the same as a failure to move one.
func (c *Container) profileLinkerFor() authz.ProfileLinker {
	if c.repos == nil || c.repos.OperatorProfiles == nil || c.repos.OperatorIdentityLinks == nil {
		return nil
	}
	return profileLinker{repos: dispatcher.OperatorLinkRepos{
		Profiles: c.repos.OperatorProfiles,
		Links:    c.repos.OperatorIdentityLinks,
		Audit:    c.repos.ProfileUseAudit,
	}}
}
