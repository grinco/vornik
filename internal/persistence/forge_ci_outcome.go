package persistence

import (
	"context"
	"time"
)

// ForgeCIOutcome is one row in `forge_ci_outcomes` — what a completed CI run
// concluded, recorded so a review can read it and a trigger can act on it.
//
// Design: https://docs.vornik.io §4.1.
//
// WHY IT IS PERSISTED RATHER THAN FETCHED LIVE. A review runs minutes or hours
// after CI finished, and the trigger decision (§5) happens at delivery time
// while the review happens later — so the two would otherwise ask the Actions
// API at different moments and could get different answers about the same run.
// Recording it once also gives the failure-triggered review a stable head SHA
// to coalesce on, which a live fetch could not.
type ForgeCIOutcome struct {
	// ProjectID scopes the row, for the same reason ForgePRReviewState is
	// project-scoped rather than installation-scoped: two projects may watch
	// one repository under different config, and must not share state.
	ProjectID string

	// Repo is the "owner/name" full name; RunID the provider's run identifier.
	// Together with ProjectID they are the primary key, so a redelivery of the
	// same completed run updates rather than appends.
	Repo  string
	RunID int64

	// HeadSHA is the commit CI ran against, and the JOIN KEY a review uses. A
	// review is about a commit, not about a pull request number — which is also
	// what keeps the enrichment path working for a fork PR, where the event
	// carries no pull request at all (design §3.3).
	HeadSHA string

	// Number is the pull request this run belongs to, or 0 when there is none.
	//
	// ZERO IS A REAL VALUE, not a missing one: a push to the default branch has
	// no pull request, and so does a run for a pull request opened from a FORK,
	// because GitHub leaves `pull_requests[]` empty in that case. Everything
	// PR-scoped is conditional on this being non-zero.
	Number int

	// WorkflowName and WorkflowPath identify which workflow ran. WorkflowPath
	// (e.g. ".github/workflows/terraform-plan.yml") is what the config's
	// workflow filter matches on, because a name is display text an author can
	// change without noticing anything depends on it.
	WorkflowName string
	WorkflowPath string

	// RunAttempt distinguishes a re-run from the original.
	RunAttempt int

	// Conclusion is the provider's own vocabulary — "success", "failure",
	// "cancelled", "timed_out", "skipped", "neutral", "action_required" — kept
	// verbatim rather than mapped to a vornik enum. A mapping would be a second
	// place the provider's vocabulary is written down, and it is the provider's
	// to change.
	Conclusion string

	StartedAt   time.Time
	CompletedAt time.Time

	// Jobs is the per-job breakdown. The "watch the terraform-plan job"
	// capability the customer asked for is a QUERY over this, not a schema
	// concept — nothing here knows what terraform is.
	Jobs []ForgeCIJob

	// ArtifactExcerpt is the opt-in content (design §4.2): the first
	// ArtifactBytes of an artifact the pipeline uploaded under the configured
	// name. Empty when content is not configured, when the run uploaded no
	// such artifact, or when it exceeded the download ceiling.
	//
	// UNTRUSTED. Attacker-controlled: anyone who can push a branch can make CI
	// write this. It reaches a prompt only through untrusted.WrapLabeled, and
	// only via the forge.fetch_ci step (design §6).
	ArtifactExcerpt string
	// ArtifactBytes is the size of the excerpt actually stored.
	ArtifactBytes int
	// ArtifactTruncated records that the artifact was LARGER than the excerpt
	// kept. Never silent: a truncated plan that reads as a complete one is a
	// wrong answer presented as a right one.
	ArtifactTruncated bool

	RecordedAt time.Time
}

// ForgeCIJob is one job within a run.
type ForgeCIJob struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// HasPullRequest reports whether this run can enter the PR-scoped paths.
//
// A method rather than an inline `Number > 0` at each call site, because the
// zero value carries meaning here and the reason it does is worth one place to
// document (a fork PR looks identical to a branch push in the event payload).
func (o ForgeCIOutcome) HasPullRequest() bool { return o.Number > 0 }

// Failed reports whether this run's conclusion is one a human would want
// looked at.
//
// "cancelled" and "timed_out" count: from the reviewer's point of view they
// mean the same thing as failure — CI did not certify this commit. "skipped"
// and "neutral" do NOT, because nothing was attempted.
func (o ForgeCIOutcome) Failed() bool {
	switch o.Conclusion {
	case "failure", "timed_out", "cancelled":
		return true
	}
	return false
}

// ForgeCIOutcomeRepository stores and reads CI outcomes.
type ForgeCIOutcomeRepository interface {
	// Upsert records a completed run. Idempotent on (project_id, repo, run_id)
	// so a webhook redelivery updates the row rather than appending a second.
	Upsert(ctx context.Context, o *ForgeCIOutcome) error

	// ListByHeadSHA returns every run recorded for one commit, newest
	// CompletedAt first, so a review renders the most recent state of each
	// workflow. Empty slice when nothing has run — NOT an error: "no CI has
	// completed for this commit" is a fact a review must be able to state.
	ListByHeadSHA(ctx context.Context, projectID, repo, headSHA string) ([]*ForgeCIOutcome, error)

	// PruneBefore deletes outcomes completed before the cutoff, returning how
	// many rows went. A CI outcome is evidence about a run and loses its value
	// once the run is unreachable; it does not earn the long horizon a human
	// rating does.
	PruneBefore(ctx context.Context, cutoff time.Time) (int64, error)
}
