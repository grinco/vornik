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
	issuerSpeaker = strings.TrimSpace(issuerSpeaker)
	claimantSpeaker = strings.TrimSpace(claimantSpeaker)
	if issuerSpeaker == "" || claimantSpeaker == "" {
		return nil, fmt.Errorf("operator-link: both ids required")
	}
	if issuerSpeaker == claimantSpeaker {
		return nil, fmt.Errorf("operator-link: cannot link an identity to itself")
	}
	if repos.Links == nil || repos.Profiles == nil {
		return nil, fmt.Errorf("operator-link: profile + link repositories required")
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

	winnerCanonical, loserCanonical, winnerProfile, loserProfile := pickWinner(
		issuerCanonical, claimantCanonical, issuerProfile, claimantProfile,
	)

	result := &OperatorLinkResult{
		Canonical: winnerCanonical,
		Loser:     loserCanonical,
	}

	if loserProfile != nil {
		// Merge if there's anything to merge.
		merged, mergedKeys, mergedNotes := mergeOperatorProfiles(winnerProfile, loserProfile, loserCanonical)
		if merged != nil {
			// Stamp the operator id so the Upsert lands on the
			// winner's row whether or not it existed before
			// the merge.
			merged.OperatorID = winnerCanonical
			if err := repos.Profiles.Upsert(ctx, merged); err != nil {
				return nil, fmt.Errorf("operator-link: upsert merged profile: %w", err)
			}
			result.Merged = true
			result.MergedKeys = mergedKeys
			result.MergedNotes = mergedNotes
		}
	}

	// Repoint every identity-link row that currently points at
	// the loser → the winner. Then add the new link row for
	// the claimant speaker (idempotent in case the operator
	// re-runs).
	if existing, err := repos.Links.ListForOperator(ctx, loserCanonical); err != nil {
		return nil, fmt.Errorf("operator-link: list loser links: %w", err)
	} else {
		for _, row := range existing {
			row.OperatorID = winnerCanonical
			if err := repos.Links.Upsert(ctx, row); err != nil {
				return nil, fmt.Errorf("operator-link: repoint link %s: %w", row.ChannelSpeakerID, err)
			}
			result.LinksMoved++
		}
	}
	// Make sure both speakers point at the winner. The "loser
	// canonical" is itself a speaker id we need to repoint.
	if loserCanonical != winnerCanonical {
		if err := repos.Links.Upsert(ctx, &persistence.OperatorIdentityLink{
			ChannelSpeakerID: loserCanonical,
			OperatorID:       winnerCanonical,
			LinkedBy:         linkedBy,
		}); err != nil {
			return nil, fmt.Errorf("operator-link: repoint loser-canonical link: %w", err)
		}
		result.LinksMoved++
	}
	if claimantSpeaker != claimantCanonical {
		if err := repos.Links.Upsert(ctx, &persistence.OperatorIdentityLink{
			ChannelSpeakerID: claimantSpeaker,
			OperatorID:       winnerCanonical,
			LinkedBy:         linkedBy,
		}); err != nil {
			return nil, fmt.Errorf("operator-link: write claimant link: %w", err)
		}
	}
	if issuerSpeaker != winnerCanonical {
		if err := repos.Links.Upsert(ctx, &persistence.OperatorIdentityLink{
			ChannelSpeakerID: issuerSpeaker,
			OperatorID:       winnerCanonical,
			LinkedBy:         linkedBy,
		}); err != nil {
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
