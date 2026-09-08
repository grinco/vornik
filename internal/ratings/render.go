package ratings

import (
	"fmt"
	"strings"
)

// The one place a rating verdict is turned into words.
//
// Design: https://docs.vornik.io §2.1.
//
// Phase 2 shipped the suppression rule — the arms always render, the DIFFERENCE
// renders only for the verdicts that stand behind it — in two places, and phase
// 3 adds three more surfaces (the MCP skill tools, the Telegram review card,
// the Slack review blocks) plus the retire proposal. Five copies of a safety
// rule is how the rule dies, and the newer copy is usually the wrong one. Every
// surface asks this file instead.
//
// Surfaces still choose their own LAYOUT — a Slack block, a CLI table and a
// badge are not the same shape. What they may not choose is whether a number
// appears, or what a verdict means.

// Cause explains a verdict that carries no number. It is meaningful only for
// VerdictNotMeasurable, where four materially different facts share one
// verdict and imply four different operator actions.
type Cause string

// The not_measurable causes (design §2.2).
const (
	// CauseNone is the zero value: the verdict speaks for itself.
	CauseNone Cause = ""
	// CauseAbsence — the subject has an attribution surface, and nobody has
	// rated anything on it in this window yet. The operator may wait.
	CauseAbsence Cause = "absence"
	// CauseConstruction — the subject can NEVER be rated: its domain records
	// no execution for a rating to attach to. Waiting will not help, and
	// saying only "no rating evidence" would invite it.
	CauseConstruction Cause = "construction"
	// CauseUnknownProvenance — ratings exist but predate body provenance
	// (design §4), so they cannot be tied to any body of this skill.
	CauseUnknownProvenance Cause = "unknown_provenance"
	// CauseSupersededBody — ratings exist and judged an EARLIER body of this
	// skill, not the one being approved. The only case where a real,
	// well-evidenced figure exists and is still withheld.
	CauseSupersededBody Cause = "superseded_body"
)

// ShowsDifference reports whether a verdict has earned the right to print a
// lift figure beside its arms.
//
// Deny by default. An unrecognised verdict — a future one added to the
// evaluator and not here — prints no figure rather than an unqualified one:
// the failure direction that costs an operator information is safer than the
// one that hands them a number nothing stands behind.
func ShowsDifference(verdict string) bool {
	return verdict == VerdictLowLift || verdict == VerdictHelping
}

// VerdictSentence says what a verdict means in the terms an operator acts on.
//
// Never empty. A blank sentence renders as an empty column, which reports
// "examined and clean" and means "never examined".
func VerdictSentence(verdict string, cause Cause) string {
	switch verdict {
	case VerdictLowLift:
		return "rated materially worse than comparable work — worth a look"
	case VerdictHelping:
		// The weakest wording available on purpose: with a reported signal the
		// ceiling is silence, so this can only ever mean no harm was detected.
		return "no sign it is hurting (this cannot show a benefit, only the absence of harm)"
	case VerdictUnknown:
		return "too few ratings in this context to say anything"
	case VerdictNotComparable:
		return "the two groups were watched at very different rates, so this " +
			"comparison would measure attention rather than quality"
	case VerdictNotMeasurable:
		return notMeasurableSentence(cause)
	}
	return "no verdict — this comparison was not evaluated"
}

// notMeasurableSentence distinguishes the four ways a subject can carry no
// usable rating evidence. They are not interchangeable: one invites the
// operator to wait, one tells them not to, and two tell them evidence exists
// but does not bear on the decision in front of them.
func notMeasurableSentence(cause Cause) string {
	switch cause {
	case CauseConstruction:
		return "this domain records no execution to attach a rating to — it cannot be rated, now or later"
	case CauseUnknownProvenance:
		return "ratings exist but predate body provenance — they cannot be tied to any body of this skill"
	case CauseSupersededBody:
		return "ratings exist, but for an earlier body of this skill — none of them judged the body you are approving"
	}
	// CauseAbsence, and anything unset: the honest default is the one that
	// claims least.
	return "never injected in this window — no rating evidence"
}

// RenderContext writes one context as a single line, for the surfaces that
// have room for a line rather than a block: the admin skills column, the
// Telegram card, the Slack blocks, the MCP tools.
//
// The arms always appear. The difference appears only when ShowsDifference
// allows it — and for a superseded body nothing numeric appears at all, since
// every figure there describes a body the reader is not deciding about.
func RenderContext(c ContextResult) string {
	if c.Result.Cause == CauseSupersededBody {
		return fmt.Sprintf("%s / %s — %s: %s",
			c.ProjectID, c.WorkflowID, c.Result.Verdict,
			VerdictSentence(c.Result.Verdict, c.Result.Cause))
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s / %s — %s: %d/%d up with, %d/%d without (coverage %.0f%% vs %.0f%%)",
		c.ProjectID, c.WorkflowID, c.Result.Verdict,
		c.Result.TreatmentUp, c.Result.TreatmentN,
		c.Result.BaselineUp, c.Result.BaselineN,
		c.Result.TreatmentCoverage*100, c.Result.BaselineCoverage*100)

	if ShowsDifference(c.Result.Verdict) {
		fmt.Fprintf(&b, ", %+.0f pp", c.Result.Lift*100)
	}
	if n := c.Result.TreatmentContested + c.Result.BaselineContested; n > 0 {
		fmt.Fprintf(&b, ", %d contested and excluded", n)
	}
	return b.String()
}

// ContestedNote surfaces excluded executions rather than letting them vanish.
// Exported so the block-rendering surfaces annotate an arm the same way the
// line-rendering ones do.
func ContestedNote(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(", %d contested and excluded", n)
}
