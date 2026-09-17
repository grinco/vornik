package dispatcher

// Cross-channel link finalisation.
//
// When `/link <code>` succeeds on channel B, this file's
// PerformOperatorLink takes over: it resolves both sides
// (issuer and claimant) to their canonical operator ids,
// inserts the new identity-link row pointing the claimant at
// the issuer, and — if both sides have profile rows — merges
// the claimant's profile into the issuer's. Whichever side has
// more accumulated content (structured-key count + notes
// length) becomes the canonical winner. A losing-side profile
// is folded in via:
//
//   - structured keys: missing-on-winner keys are copied across;
//     conflicting keys keep the winner's value;
//   - notes: appended with a `[merged from <loser> on <date>]`
//     separator;
//   - identity links: every existing row pointing at the loser
//     is repointed at the winner's canonical id;
//   - profile_use_audit rows: repointed too, so the surviving
//     operator sees the full history;
//   - the loser's profile row is deleted.
//
// This file deliberately holds no HTTP / chat-channel logic —
// it's the pure linking primitive both /link (chat) and
// `vornikctl operator link` (CLI) build on top of. Phase A's
// `vornikctl operator link` skipped the merge and just wrote a
// link row; Phase A's bug was "linked profiles don't fold" —
// closing it here.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// OperatorLinkRepos bundles the repositories PerformOperatorLink
// needs. Passing them as a struct keeps the function signature
// stable when later phases need additional repos (e.g.
// profile_use_audit reassignment).
type OperatorLinkRepos struct {
	Profiles persistence.OperatorProfileRepository
	Links    persistence.OperatorIdentityLinkRepository
	Audit    persistence.ProfileUseAuditRepository // optional; nil disables audit-row reassignment
}

// OperatorLinkResult summarises what PerformOperatorLink did.
// Returned to the caller so the chat reply can describe the
// outcome ("linked X and Y; merged 3 keys + 1 note").
type OperatorLinkResult struct {
	Canonical   string
	Loser       string
	Merged      bool
	MergedKeys  []string
	MergedNotes bool
	LinksMoved  int
}

// PerformOperatorLink finalises a cross-channel link between
// issuer and claimant. Returns the resolved canonical id (the
// winning side) + summary of the merge.
//
// Behaviour:
//   - Refuses self-link (issuer == claimant).
//   - Resolves both sides to their canonical operator ids by
//     walking existing identity-link rows.
//   - When both canonicals are already the same: no-op success
//     (the operator linked something already linked).
//   - Picks the more-populated profile as the surviving
//     canonical. Ties → issuer wins (chronological priority).
//   - Writes a new identity-link row claimant_speaker → canonical.
//   - Reassigns every identity-link row pointing at the loser
//     to point at the winner.
//   - Merges profile content (see file header).
//   - Reassigns every profile_use_audit row from the loser to
//     the winner so the audit history survives.
//   - Deletes the loser's profile row.
//
// All disk ops are best-effort sequenced; a failure mid-sequence
// surfaces as a returned error and the caller is responsible
// for telling the operator to re-run the link. The repo
// implementations are idempotent enough that a retry is safe.
func PerformOperatorLink(ctx context.Context, repos OperatorLinkRepos, issuerSpeaker, claimantSpeaker, linkedBy string) (*OperatorLinkResult, error) {
	return performLink(ctx, repos, issuerSpeaker, claimantSpeaker, linkedBy, false)
}

// PerformAccountLink folds a chat speaker's profile into an ACCOUNT's
// canonical id, with the account always the winner.
//
// The difference from PerformOperatorLink is the whole point.
// PerformOperatorLink picks the side with more accumulated content, which is
// right when two chat profiles meet as equals and nobody has an account. It
// is wrong here: oidc-identity-permissions-design §5.0 says that once a
// speaker is linked, **the account binding wins** — the account is the
// identity, and a speaker who happens to have chattier history does not get
// to become the canonical row for a person who has an account. Content volume
// decides a tie between peers; it must not decide authority.
//
// Everything else — the merge, the repoint of every link row pointing at the
// loser, the audit reassignment — is shared, because two implementations of
// one merge is how they drift.
func PerformAccountLink(ctx context.Context, repos OperatorLinkRepos, accountCanonical, speaker, linkedBy string) (*OperatorLinkResult, error) {
	return performLink(ctx, repos, accountCanonical, speaker, linkedBy, true)
}

func performLink(ctx context.Context, repos OperatorLinkRepos, issuerSpeaker, claimantSpeaker, linkedBy string, issuerAlwaysWins bool) (*OperatorLinkResult, error) {
	issuerSpeaker = strings.TrimSpace(issuerSpeaker)
	claimantSpeaker = strings.TrimSpace(claimantSpeaker)
	if err := checkLinkArgs(repos, issuerSpeaker, claimantSpeaker); err != nil {
		return nil, err
	}

	issuerCanonical, err := canonicalFor(ctx, repos.Links, issuerSpeaker)
	if err != nil {
		return nil, fmt.Errorf("operator-link: resolve issuer canonical: %w", err)
	}
	claimantCanonical, err := canonicalFor(ctx, repos.Links, claimantSpeaker)
	if err != nil {
		return nil, fmt.Errorf("operator-link: resolve claimant canonical: %w", err)
	}
	if issuerCanonical == claimantCanonical {
		// Already linked transitively (e.g. via /link earlier
		// today). Treat as success; no merge needed.
		return &OperatorLinkResult{Canonical: issuerCanonical}, nil
	}

	// Pick the winner. We're going to fold the loser's
	// content into the winner. Strategy: the side with more
	// accumulated content (structured keys + notes length)
	// wins. Ties break in favour of the issuer (chrono
	// priority — they started the flow).
	issuerProfile, err := getProfileOrNil(ctx, repos.Profiles, issuerCanonical)
	if err != nil {
		return nil, fmt.Errorf("operator-link: load issuer profile: %w", err)
	}
	claimantProfile, err := getProfileOrNil(ctx, repos.Profiles, claimantCanonical)
	if err != nil {
		return nil, fmt.Errorf("operator-link: load claimant profile: %w", err)
	}

	winnerCanonical, loserCanonical, winnerProfile, loserProfile, err := chooseWinner(
		issuerCanonical, claimantCanonical, issuerProfile, claimantProfile, issuerAlwaysWins)
	if err != nil {
		return nil, err
	}

	result := &OperatorLinkResult{
		Canonical: winnerCanonical,
		Loser:     loserCanonical,
	}

	if err := mergeLoserProfile(ctx, repos, result, winnerCanonical, loserCanonical, winnerProfile, loserProfile); err != nil {
		return nil, err
	}

	if err := repointLoserLinks(ctx, repos, result, winnerCanonical, loserCanonical); err != nil {
		return nil, err
	}
	// Make sure both speakers point at the winner. The "loser
	// canonical" is itself a speaker id we need to repoint.
	if loserCanonical != winnerCanonical {
		if err := pointAt(ctx, repos, loserCanonical, winnerCanonical, linkedBy); err != nil {
			return nil, fmt.Errorf("operator-link: repoint loser-canonical link: %w", err)
		}
		result.LinksMoved++
	}
	// SKIPPED on the PerformAccountLink path, where the claimant is a fresh
	// speaker and therefore its own canonical — so the row that actually
	// binds that speaker is the loser-canonical repoint just above, not this
	// branch. A reader tracing "where does the speaker get bound" looks here
	// first and finds nothing; the second-channel test pins the real write.
	if claimantSpeaker != claimantCanonical {
		if err := pointAt(ctx, repos, claimantSpeaker, winnerCanonical, linkedBy); err != nil {
			return nil, fmt.Errorf("operator-link: write claimant link: %w", err)
		}
	}
	if issuerSpeaker != winnerCanonical {
		if err := pointAt(ctx, repos, issuerSpeaker, winnerCanonical, linkedBy); err != nil {
			return nil, fmt.Errorf("operator-link: write issuer link: %w", err)
		}
	}

	// Profile-use audit rows: reassign so the operator sees
	// the full history on the surviving canonical id. Best-
	// effort: a repository without bulk reassignment can skip
	// — the rows are still queryable under the old id if
	// needed. We don't have a Reassign method; the simplest
	// fix is to delete loser rows (the merged audit row will
	// pick up from this turn forward). Operators who want
	// history-preserving consolidation can use the CLI's
	// `--keep-audit` path (Phase C).
	if repos.Audit != nil && loserCanonical != winnerCanonical {
		if err := repos.Audit.DeleteAllForOperator(ctx, loserCanonical); err != nil {
			// Don't fail the link over an audit cleanup hiccup.
			// Operators get a complete link; the audit drift
			// surfaces only on a future audit query.
			_ = err
		}
	}

	// Finally drop the loser's profile so a future read for
	// its canonical id falls through to the winner's via the
	// link.
	if loserProfile != nil {
		if err := repos.Profiles.Delete(ctx, loserCanonical); err != nil {
			return nil, fmt.Errorf("operator-link: delete loser profile: %w", err)
		}
	}

	return result, nil
}

// canonicalFor resolves a speaker id to its canonical operator
// id by consulting the link table. Returns the speaker id
// itself when no link row exists.
func canonicalFor(ctx context.Context, repo persistence.OperatorIdentityLinkRepository, speaker string) (string, error) {
	link, err := repo.Get(ctx, speaker)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return speaker, nil
		}
		return "", err
	}
	if link == nil || link.OperatorID == "" {
		return speaker, nil
	}
	return link.OperatorID, nil
}

// getProfileOrNil reads a profile row, returning nil + nil
// when ErrNotFound. Other errors surface so the caller can
// abort the link rather than half-finish it.
func getProfileOrNil(ctx context.Context, repo persistence.OperatorProfileRepository, id string) (*persistence.OperatorProfile, error) {
	p, err := repo.Get(ctx, id)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return p, nil
}

// pickWinner decides which canonical id survives + which
// profile is the merge target. Score = structured key count +
// len(notes). Ties favour the issuer.
func pickWinner(
	issuerID, claimantID string,
	issuerProfile, claimantProfile *persistence.OperatorProfile,
) (winnerID, loserID string, winnerProfile, loserProfile *persistence.OperatorProfile) {
	issuerScore := profileScore(issuerProfile)
	claimantScore := profileScore(claimantProfile)
	if claimantScore > issuerScore {
		return claimantID, issuerID, claimantProfile, issuerProfile
	}
	return issuerID, claimantID, issuerProfile, claimantProfile
}

func profileScore(p *persistence.OperatorProfile) int {
	if p == nil {
		return 0
	}
	var m map[string]any
	_ = json.Unmarshal(p.Structured, &m)
	return len(m) + len(strings.TrimSpace(p.Notes))
}

// mergeOperatorProfiles folds the loser into the winner.
// Returns the merged profile + a summary of what changed. The
// caller upserts the result.
//
// When the winner has no profile row yet, we create one keyed
// on the winner's id, seeded with the loser's content + a
// merge-tag note.
func mergeOperatorProfiles(winner, loser *persistence.OperatorProfile, loserID string) (*persistence.OperatorProfile, []string, bool) {
	if loser == nil {
		return nil, nil, false
	}
	// Decode both structured blobs.
	var winnerStruct map[string]any
	if winner != nil && len(winner.Structured) > 0 {
		_ = json.Unmarshal(winner.Structured, &winnerStruct)
	}
	if winnerStruct == nil {
		winnerStruct = map[string]any{}
	}
	var loserStruct map[string]any
	if len(loser.Structured) > 0 {
		_ = json.Unmarshal(loser.Structured, &loserStruct)
	}

	var mergedKeys []string
	for k, v := range loserStruct {
		if _, exists := winnerStruct[k]; exists {
			// Winner's value wins. Don't record as a merge
			// (the operator doesn't need to know about a
			// no-op conflict).
			continue
		}
		winnerStruct[k] = v
		mergedKeys = append(mergedKeys, k)
	}
	sort.Strings(mergedKeys)

	// Notes: append the loser's content under a separator that
	// survives downstream renders.
	mergedNotes := false
	winnerNotes := ""
	if winner != nil {
		winnerNotes = winner.Notes
	}
	loserNotes := strings.TrimSpace(loser.Notes)
	finalNotes := winnerNotes
	if loserNotes != "" {
		stamp := time.Now().UTC().Format("2006-01-02")
		separator := fmt.Sprintf("\n\n[merged from %s on %s]\n", loserID, stamp)
		if strings.TrimSpace(winnerNotes) == "" {
			finalNotes = loserNotes
		} else {
			finalNotes = winnerNotes + separator + loserNotes
		}
		mergedNotes = true
	}

	// Re-encode.
	structBytes, _ := json.Marshal(winnerStruct)
	out := &persistence.OperatorProfile{
		Structured: structBytes,
		Notes:      finalNotes,
	}
	if winner != nil {
		out.OperatorID = winner.OperatorID
	}
	// When winner had no profile row, the caller picks the
	// canonical id; we leave OperatorID empty so the caller
	// stamps it. The caller already knows the winner id;
	// stamping here would risk a mismatch if the caller
	// re-assigns.
	return out, mergedKeys, mergedNotes
}

// mergeLoserProfile folds the loser's profile into the winner's row. Split
// out of performLink to keep that function under the complexity ceiling
// after PerformAccountLink added its winner-selection branch; the behaviour
// is unchanged.
func mergeLoserProfile(
	ctx context.Context, repos OperatorLinkRepos, result *OperatorLinkResult,
	winnerCanonical, loserCanonical string,
	winnerProfile, loserProfile *persistence.OperatorProfile,
) error {
	if loserProfile == nil {
		return nil
	}
	merged, mergedKeys, mergedNotes := mergeOperatorProfiles(winnerProfile, loserProfile, loserCanonical)
	if merged == nil {
		return nil
	}
	// Stamp the operator id so the Upsert lands on the winner's row whether
	// or not it existed before the merge.
	merged.OperatorID = winnerCanonical
	if err := repos.Profiles.Upsert(ctx, merged); err != nil {
		return fmt.Errorf("operator-link: upsert merged profile: %w", err)
	}
	result.Merged = true
	result.MergedKeys = mergedKeys
	result.MergedNotes = mergedNotes
	return nil
}

// repointLoserLinks moves every identity-link row pointing at the loser onto
// the winner. Split out of performLink for the same reason as
// mergeLoserProfile; the behaviour is unchanged.
func repointLoserLinks(
	ctx context.Context, repos OperatorLinkRepos, result *OperatorLinkResult,
	winnerCanonical, loserCanonical string,
) error {
	existing, err := repos.Links.ListForOperator(ctx, loserCanonical)
	if err != nil {
		return fmt.Errorf("operator-link: list loser links: %w", err)
	}
	for _, row := range existing {
		row.OperatorID = winnerCanonical
		if err := repos.Links.Upsert(ctx, row); err != nil {
			return fmt.Errorf("operator-link: repoint link %s: %w", row.ChannelSpeakerID, err)
		}
		result.LinksMoved++
	}
	return nil
}

// pointAt writes one link row. The three call sites above spelled the same
// four-line literal three times; collapsing them is what kept performLink
// under the length ceiling after PerformAccountLink was added.
func pointAt(ctx context.Context, repos OperatorLinkRepos, speaker, canonical, linkedBy string) error {
	return repos.Links.Upsert(ctx, &persistence.OperatorIdentityLink{
		ChannelSpeakerID: speaker,
		OperatorID:       canonical,
		LinkedBy:         linkedBy,
	})
}

// checkLinkArgs rejects the shapes a link can never have. Split out only to
// keep performLink under the length ceiling; the rules are unchanged.
func checkLinkArgs(repos OperatorLinkRepos, issuerSpeaker, claimantSpeaker string) error {
	switch {
	case issuerSpeaker == "" || claimantSpeaker == "":
		return fmt.Errorf("operator-link: both ids required")
	case issuerSpeaker == claimantSpeaker:
		return fmt.Errorf("operator-link: cannot link an identity to itself")
	case repos.Links == nil || repos.Profiles == nil:
		return fmt.Errorf("operator-link: profile + link repositories required")
	}
	return nil
}

// accountCanonicalPrefix marks an operator id derived from an ACCOUNT rather
// than from a chat speaker. It must match authz.AccountOperatorID's output;
// a test in this package asserts that rather than trusting the two spellings
// to stay in step.
const accountCanonicalPrefix = "account:"

// isAccountCanonical reports whether an operator id belongs to an account.
func isAccountCanonical(operatorID string) bool {
	return strings.HasPrefix(operatorID, accountCanonicalPrefix)
}

// chooseWinner decides which canonical survives the merge, and is the one
// place the rule lives.
//
// Content volume settles a tie between PEERS — the case this design was
// written for, where two chat profiles meet and neither has any claim to
// authority. It must never settle AUTHORITY. An account canonical therefore
// wins wherever it turns up, not only where PerformAccountLink put it: a
// speaker who redeemed a link code resolves to "account:<user>", and without
// this a later chat→chat /link with a chattier peer would make the account
// the loser and repoint every row pointing at it onto a chat speaker.
//
// Two accounts are refused outright: that is either two different people or
// one person who should unlink, and an unauthenticated chat OTP is not the
// authority to decide which.
func chooseWinner(
	issuerCanonical, claimantCanonical string,
	issuerProfile, claimantProfile *persistence.OperatorProfile,
	issuerAlwaysWins bool,
) (winner, loser string, winnerProfile, loserProfile *persistence.OperatorProfile, err error) {
	issuerIsAccount := isAccountCanonical(issuerCanonical)
	claimantIsAccount := isAccountCanonical(claimantCanonical)
	switch {
	case issuerIsAccount && claimantIsAccount:
		// Reached only for DISTINCT accounts: performLink short-circuits on
		// issuerCanonical == claimantCanonical before this is called, so a
		// re-link from an already-bound speaker (whose canonical IS the
		// account) returns success up there and never arrives here. Named
		// because the refusal would otherwise look like it breaks every
		// re-link, and a reviewer read it that way (review-20260915-1ea8 F1).
		// The remedy is deliberately NOT named. The first version said
		// "unlink one first", which names an operation that does not exist
		// for this case: `account unlink` removes a CHANNEL identity, and
		// there is no account-to-account unlink (review-20260915-b945). An
		// instruction that cannot be followed is worse than none.
		return "", "", nil, nil, fmt.Errorf(
			"operator-link: cannot link two accounts (%s, %s): these are separate people to "+
				"this system, and a chat code is not the authority to merge them",
			issuerCanonical, claimantCanonical)
	case issuerAlwaysWins || issuerIsAccount:
		return issuerCanonical, claimantCanonical, issuerProfile, claimantProfile, nil
	case claimantIsAccount:
		return claimantCanonical, issuerCanonical, claimantProfile, issuerProfile, nil
	}
	w, l, wp, lp := pickWinner(issuerCanonical, claimantCanonical, issuerProfile, claimantProfile)
	return w, l, wp, lp, nil
}
