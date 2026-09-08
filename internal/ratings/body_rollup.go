package ratings

import (
	"context"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// The rollup an APPROVAL PROMPT asks for.
//
// Design: https://docs.vornik.io §4.
//
// SkillRollup answers "how is this skill doing". An operator at an approval
// prompt is asking something narrower: what do we know about the BODY in front
// of me. Those diverge because approval binds to a body_sha256 while the
// rollup joins on skill id, and re-proposing a skill edits it in place under
// the same id — so the unscoped answer can describe the very body being
// replaced.

// BodyArmReader reads arms scoped to one body of a skill.
type BodyArmReader interface {
	SkillRatingArmsForBody(ctx context.Context, skillID, bodySHA256 string, since time.Time) ([]persistence.SkillRatingArms, error)
}

// ProvenanceReader explains an empty result: nobody has rated this body, and
// the useful question is whether anything rated any OTHER body of it.
type ProvenanceReader interface {
	SkillInjectionProvenance(ctx context.Context, skillID, bodySHA256 string, since time.Time) (persistence.InjectionProvenance, error)
}

// SkillRollupForBody measures one body of one skill over one window.
//
// When the body has been rated it behaves exactly as SkillRollup, scoped. When
// it has not, it consults the provenance split and returns the CAUSE, because
// "nobody has rated this yet", "the ratings judged a body you are replacing"
// and "the ratings cannot be placed against any body" are three different
// facts and an operator acts differently on each.
func SkillRollupForBody(ctx context.Context, arms BodyArmReader, prov ProvenanceReader,
	skillID, bodySHA256 string, window time.Duration, cfg Config) (SkillRollupResult, error) {
	out := SkillRollupResult{SkillID: skillID, Window: window, Summary: VerdictNotMeasurable}

	since := time.Now().UTC().Add(-window)
	rows, err := arms.SkillRatingArmsForBody(ctx, skillID, bodySHA256, since)
	if err != nil {
		return out, fmt.Errorf("read rating arms for skill %s body %s: %w", skillID, bodySHA256, err)
	}

	if len(rows) > 0 {
		for _, a := range rows {
			out.Contexts = append(out.Contexts, ContextResult{
				ProjectID:  a.ProjectID,
				WorkflowID: a.WorkflowID,
				Result:     Evaluate(toArm(a.Treatment), toArm(a.Baseline), cfg),
			})
		}
		out.Summary = summarise(out.Contexts)
		return out, nil
	}

	// Nothing to measure. WHY is the whole value of this call.
	p, err := prov.SkillInjectionProvenance(ctx, skillID, bodySHA256, since)
	if err != nil {
		// Never degrade a failed read into "no evidence". Silence from a broken
		// query and silence from an unrated skill look identical on the prompt,
		// and only one of them is a fact about the skill.
		return out, fmt.Errorf("read injection provenance for skill %s: %w", skillID, err)
	}
	out.Cause = causeOf(p)
	return out, nil
}

// causeOf reads the provenance split.
//
// A superseded body outranks unknown provenance when both are present: "the
// ratings you can see judged a different body" is a stronger and more
// actionable statement than "the ratings cannot be placed", and an operator
// who learns the first does not need the second.
func causeOf(p persistence.InjectionProvenance) Cause {
	switch {
	case p.OtherBodyN > 0:
		return CauseSupersededBody
	case p.UnknownBodyN > 0:
		return CauseUnknownProvenance
	default:
		return CauseAbsence
	}
}

// ApprovalWindow is the span every approval surface measures over.
//
// A week, matching the API default and the admin column, so the Telegram card,
// the Slack blocks, the MCP tools and the browser cannot disagree about what
// period a verdict covers.
const ApprovalWindow = 168 * time.Hour

// SkillApprovalLine is the one line an approval prompt shows for one skill.
//
// Best-effort by construction: any failure returns the not_measurable sentence
// rather than an error or an empty string. A blank line at an approval prompt
// reports "examined and clean" and means "never examined", so there is no
// failure mode in which this surface says nothing.
func SkillApprovalLine(ctx context.Context, arms BodyArmReader, prov ProvenanceReader,
	skillID, bodySHA256 string) string {
	if arms == nil || prov == nil {
		return VerdictSentence(VerdictNotMeasurable, CauseAbsence)
	}
	res, err := SkillRollupForBody(ctx, arms, prov, skillID, bodySHA256, ApprovalWindow, DefaultConfig())
	if err != nil {
		// Say that the evidence could not be read, rather than that there is
		// none. They are different facts and only one of them is about the
		// skill.
		return "rating evidence could not be read for this skill — treat as unexamined"
	}
	return ApprovalLine(res)
}

// ApprovalLine renders a rollup for a prompt: the worst context when there are
// contexts, and the cause sentence when there are none.
func ApprovalLine(res SkillRollupResult) string {
	if len(res.Contexts) == 0 {
		return VerdictSentence(res.Summary, res.Cause)
	}
	// The worst context is what an operator acts on — "is this hurting
	// ANYWHERE", not "on average" — and it is what Summary already selects.
	for _, c := range res.Contexts {
		if c.Result.Verdict == res.Summary {
			return RenderContext(c)
		}
	}
	return RenderContext(res.Contexts[0])
}
