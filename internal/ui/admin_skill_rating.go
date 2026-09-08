package ui

import (
	"context"
	"time"

	"vornik.io/vornik/internal/ratings"
)

// The rating rollup column on /ui/admin/skills
// (LLD 2026-09-07-execution-ratings-rollup-design.md, phase 2).
//
// The page already shows fired / worked / corrected — the APPLIED side only,
// which is half of a comparison and exactly the gap the rollup closes: a skill
// that "worked" 80% of the time looks good until you notice comparable work
// nobody injected it into also worked 80%.

// adminSkillRatingWindow is the rollup window for the browser. A week, matching
// the API's default, so the column and `vornikctl knowledge rollup` cannot
// disagree about what period a verdict covers.
const adminSkillRatingWindow = 168 * time.Hour

// skillRatingBadge computes one skill's rollup for the browser.
//
// Best-effort: a nil return hides the column for that row rather than failing
// the page. An advisory column is not worth a 500 on the surface an operator
// uses to approve and retire skills.
func (s *Server) skillRatingBadge(ctx context.Context, skillID string) *AdminSkillRating {
	if s.ratingRollupRepo == nil {
		return nil
	}
	res, err := ratings.SkillRollup(ctx, s.ratingRollupRepo, skillID,
		adminSkillRatingWindow, ratings.DefaultConfig())
	if err != nil {
		s.logger.Debug().Err(err).Str("skill_id", skillID).
			Msg("skill rating rollup failed; column suppressed for this row")
		return nil
	}

	badge := &AdminSkillRating{Verdict: res.Summary}
	for _, c := range res.Contexts {
		badge.Detail = append(badge.Detail, renderRatingContext(c))
	}
	return badge
}

// renderRatingContext writes one context's line.
//
// Delegates to ratings.RenderContext: the rule about which verdicts may show a
// difference lives in ONE place (LLD 2026-09-08-execution-ratings-approval-
// paths-design.md §2.1), because phase 3 adds three more surfaces that need
// the same rule and the second copy of a safety rule is the one that is wrong.
func renderRatingContext(c ratings.ContextResult) string {
	return ratings.RenderContext(c)
}
