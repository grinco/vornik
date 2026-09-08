package ui

import (
	"context"
	"fmt"
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
// The arms always appear. The DIFFERENCE appears only for the verdicts that
// stand behind it: on not_comparable and unknown the number exists and does not
// mean what it looks like, and the design's review made the point that
// operators ignore the badge and quote the number. Deciding that here rather
// than in the template keeps the rule in one place, testable, and out of reach
// of a future template edit.
func renderRatingContext(c ratings.ContextResult) string {
	line := fmt.Sprintf("%s / %s — %s: %d/%d up with, %d/%d without (coverage %.0f%% vs %.0f%%)",
		c.ProjectID, c.WorkflowID, c.Result.Verdict,
		c.Result.TreatmentUp, c.Result.TreatmentN,
		c.Result.BaselineUp, c.Result.BaselineN,
		c.Result.TreatmentCoverage*100, c.Result.BaselineCoverage*100)

	if c.Result.Verdict == ratings.VerdictLowLift || c.Result.Verdict == ratings.VerdictHelping {
		line += fmt.Sprintf(", %+.0f pp", c.Result.Lift*100)
	}
	if n := c.Result.TreatmentContested + c.Result.BaselineContested; n > 0 {
		line += fmt.Sprintf(", %d contested and excluded", n)
	}
	return line
}
