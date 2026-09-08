package persistence

import (
	"context"
	"time"
)

// The REPORTED half of instinct lift
// (https://docs.vornik.io §3).
//
// instinct_lift measures whether surfacing an instinct helped, by the system's
// own success predicate. This measures whether the people who saw the output
// thought so. The two sit side by side on a retire proposal, and the case
// worth naming is when they disagree.
//
// Recovery domain only, and that is a property of the schema rather than a
// scoping choice: InstinctApplication.ExecutionID is populated at the
// lead_recovery surface and nowhere else, so budget and architect
// applications have no execution for a per-execution rating to attach to.
// They are not_measurable by construction, permanently.

// InstinctRatingArms is one recovery instinct's two arms over one window,
// measured in the (project, role, error_class) context the instinct's own
// trigger defines.
//
// Both arms come back from ONE query, as with the skill rollup: they are only
// comparable if they share a context and a window, and returning them together
// makes that true by construction rather than leaving a caller to pair them.
type InstinctRatingArms struct {
	InstinctID string
	ProjectID  string
	Role       string
	ErrorClass string
	Treatment  RatingArm
	Baseline   RatingArm
}

// InstinctRatingRollupRepository reads the reported arms. Read-only, like the
// skill rollup: a rating annotates a retire proposal and never files, gates or
// blocks one.
type InstinctRatingRollupRepository interface {
	// InstinctRecoveryRatingArms returns both arms for one recovery-domain
	// instinct.
	//
	// Treatment = executions in the context that carried a resolved
	// lead_recovery application of this instinct. Baseline = executions in the
	// SAME (project, role, error_class) context and window that did not —
	// instinct lift's concurrent complement, reused rather than reinvented.
	//
	// projectID "" drops the project constraint, matching
	// RecoveryComplementOutcomes' handling of a global-scope instinct.
	InstinctRecoveryRatingArms(ctx context.Context, instinctID, projectID, role, errorClass string,
		since time.Time) (InstinctRatingArms, error)
}
