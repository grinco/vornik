package persistence

import (
	"context"
	"time"
)

// Rating rollup — the arms phase 2 compares
// (https://docs.vornik.io).
//
// The method is instinct lift's concurrent complement. What differs is that a
// rating exists only where a human chose to write one, so these queries return
// an ELIGIBLE count alongside the rated one: coverage is the evidence that the
// two arms were observed comparably, and without it a lift figure measures who
// was watching.

// RatingArm is one side of a rollup comparison, already collapsed to the
// per-EXECUTION grain.
type RatingArm struct {
	// EligibleN is executions in this arm's context in the window, rated or
	// not. The denominator coverage needs.
	EligibleN int
	// RatedN counts executions carrying a usable rating. An execution several
	// operators rated the same way counts ONCE; a contested one is not here.
	RatedN int
	// UpN is how many of RatedN were rated up.
	UpN int
	// ContestedN is executions whose raters disagreed — excluded from RatedN
	// and UpN, and carried so the exclusion is visible (design §4.1).
	ContestedN int
}

// SkillRatingArms is one (project, workflow) context a skill appeared in, with
// both arms measured over the same window.
//
// Both arms come back from ONE query rather than two calls, unlike instinct
// lift's split adapter. The arms are only comparable if they share a context
// and a window, and returning them together makes that true by construction
// instead of leaving a caller to pair them correctly.
type SkillRatingArms struct {
	SkillID    string
	ProjectID  string
	WorkflowID string
	Treatment  RatingArm
	Baseline   RatingArm
}

// RatingRollupRepository reads the arms. Read-only: the rollup computes and
// renders, and writes nothing back — a rating never gates a run.
type RatingRollupRepository interface {
	// SkillRatingArms returns one row per (project, workflow) context in which
	// the skill was injected during the window.
	//
	// Treatment = executions with this skill injected. Baseline = executions in
	// the SAME (project, workflow) and window WITHOUT it — marginal lift, so
	// the baseline may contain other skills, exactly as instinct lift defines
	// it. Empty slice when the skill was never injected in the window.
	SkillRatingArms(ctx context.Context, skillID string, since time.Time) ([]SkillRatingArms, error)
}
