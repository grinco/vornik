package forgereview

import forgeapi "vornik.io/vornik/internal/forge"

// Policy is a project's resolved review-trigger settings: which automatic
// triggers are allowed to spend a review.
//
// Plain bools, never pointers. Every "unset means X" question is answered
// where the config is read (registry.Project.ForgeReview), so nothing here —
// and nothing in either ingress — has to know the fallback order or can get it
// differently wrong. That divergence is the bug this type exists to close:
// until 2026-09-13 both knobs lived on the GitHub App channel's installation
// config and reached only that ingress, while the generic relay path had no
// project-wide off switch at all (BACKLOG 2026-09-03).
type Policy struct {
	// AutoReviewOnPush allows a new commit on a change-request branch to
	// trigger a fresh review. Defaults ON — the behaviour
	// docs/public/features/forge.md promises.
	AutoReviewOnPush bool

	// ReviewDraftPRs opts INTO reviewing drafts. Defaults OFF.
	ReviewDraftPRs bool
}

// DefaultPolicy is what an unresolvable project gets: review pushes, skip
// drafts. Chosen so a node that cannot look a project up still behaves like
// the documented default rather than like "no rules" — an unresolved lookup
// must not become a licence to review every draft it sees, because that spends
// the budget on the failure.
func DefaultPolicy() Policy { return Policy{AutoReviewOnPush: true} }

// PolicyResolver answers what a project's policy is. One resolver, held by the
// one Coordinator both ingresses share, is what stops the two doors disagreeing.
type PolicyResolver func(projectID string) Policy

// Suppresses reports whether this policy refuses an AUTOMATIC trigger, and why.
//
// On-demand jobs are exempt and never reach the body: a human asking for a
// review is asking for one, and a knob that could swallow that would be a trap
// the operator cannot escape from the very thread they configured.
//
// Exported because one caller cannot go through the Coordinator: the generic
// ingress holds its coordinator as an INTERFACE, so a deployment with none has
// a nil interface value that cannot be called at all. That path applies
// DefaultPolicy through this same function rather than restating the rule —
// two spellings of one rule is how the two ingresses drifted to begin with.
func (p Policy) Suppresses(job forgeapi.ForgeJob, onDemand bool) (string, bool) {
	if onDemand || !job.IsChangeRequest {
		return "", false
	}
	// The push trigger is the only automatic one an operator can switch off,
	// because it is the only one that fires repeatedly on a single change
	// request. It deliberately does NOT suppress opened / reopened /
	// ready_for_review: someone who wants quiet pushes has not asked to lose
	// first review as well.
	if !p.AutoReviewOnPush && job.Action == "synchronize" {
		return "auto_review_on_push_disabled", true
	}
	// A draft is work in progress; reviewing it spends budget on code nobody
	// has said is ready. ready_for_review is EXEMPT because it is the action
	// that ENDS draft state — GitHub still reports draft:true on some
	// deliveries of it, and suppressing it there would swallow the very
	// transition that is supposed to start the review.
	if job.IsDraft && !p.ReviewDraftPRs && job.Action != "ready_for_review" {
		return "draft_not_reviewed", true
	}
	return "", false
}
