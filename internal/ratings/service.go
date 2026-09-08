package ratings

import (
	"context"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ArmReader is the persistence surface the rollup needs. Narrower than the
// repository interface so a caller can be tested without one.
type ArmReader interface {
	SkillRatingArms(ctx context.Context, skillID string, since time.Time) ([]persistence.SkillRatingArms, error)
}

// ContextResult is one (project, workflow) context and its verdict.
type ContextResult struct {
	ProjectID  string
	WorkflowID string
	Result     Result
}

// SkillRollupResult is everything known about one skill in one window.
type SkillRollupResult struct {
	SkillID string
	// Window is the span measured, carried so a surface can say what the
	// verdict is a verdict ABOUT.
	Window time.Duration
	// Contexts is one result per (project, workflow). Never pooled: a digest
	// and a code review are not comparable outputs, and folding them together
	// would put the difference between two workflows into the skill's lift.
	Contexts []ContextResult
	// Summary is the WORST verdict across the contexts — see summarise.
	Summary string
	// Cause explains a not_measurable Summary. Empty whenever the summary
	// stands on measured arms: a verdict with evidence behind it has nothing
	// to explain (see body_rollup.go).
	Cause Cause
}

// SkillRollup measures one skill over one window.
//
// The cutoff is computed ONCE and passed down, so every context in a rollup is
// measured over the same span. Arms compared across different windows are not
// arms, and a per-context cutoff would be an easy way to get that wrong.
func SkillRollup(ctx context.Context, repo ArmReader, skillID string, window time.Duration, cfg Config) (SkillRollupResult, error) {
	out := SkillRollupResult{SkillID: skillID, Window: window, Summary: VerdictNotMeasurable}

	since := time.Now().UTC().Add(-window)
	arms, err := repo.SkillRatingArms(ctx, skillID, since)
	if err != nil {
		return out, fmt.Errorf("read rating arms for skill %s: %w", skillID, err)
	}
	// No arms means the skill was never injected in the window, so there is no
	// attribution surface to join on. That is not_measurable, which is a
	// different fact from "too few ratings" and the operator acts differently
	// on it — the skill is not being used, rather than not being judged.
	if len(arms) == 0 {
		return out, nil
	}

	for _, a := range arms {
		out.Contexts = append(out.Contexts, ContextResult{
			ProjectID:  a.ProjectID,
			WorkflowID: a.WorkflowID,
			Result:     Evaluate(toArm(a.Treatment), toArm(a.Baseline), cfg),
		})
	}
	out.Summary = summarise(out.Contexts)
	return out, nil
}

func toArm(a persistence.RatingArm) Arm {
	return Arm{
		EligibleN:  a.EligibleN,
		RatedN:     a.RatedN,
		UpN:        a.UpN,
		ContestedN: a.ContestedN,
	}
}

// verdictSeverity orders the verdicts for the summary badge.
//
// The operator's question is "is this hurting ANYWHERE", not "on average", so
// the worst context wins rather than a mean — and a mean across contexts would
// be the pooled figure the design refuses anyway.
//
// not_comparable outranks unknown deliberately: unknown means there was not
// enough to look at, while not_comparable means there WAS and it could not be
// used. The second is the one worth an operator's attention, because it is a
// property of how the skill is being watched rather than of how much has
// happened yet.
var verdictSeverity = map[string]int{
	VerdictHelping:       0,
	VerdictNotMeasurable: 1,
	VerdictUnknown:       2,
	VerdictNotComparable: 3,
	VerdictLowLift:       4,
}

func summarise(contexts []ContextResult) string {
	worst := VerdictHelping
	for _, c := range contexts {
		if verdictSeverity[c.Result.Verdict] > verdictSeverity[worst] {
			worst = c.Result.Verdict
		}
	}
	return worst
}
