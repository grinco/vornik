// Package forge abstracts a code-hosting forge (GitHub, GitLab, Gitea) behind a
// provider-neutral interface so vornik's deterministic, daemon-side automation
// (issue → change request, change-request reviews) works the same regardless of
// which forge a project uses. Nothing outside internal/forge/<provider> knows a
// provider-specific noun: "pull request" / "merge request" are both a
// ChangeRequest, "review" / "note" are both a ReviewSpec.
//
// A ForgeProvider instance is constructed already bound to its credentials (see
// New + the provider config blocks), so authentication never appears in any
// method signature, in the ForgeJob, or in the workflow that drives them.
package forge

import (
	"context"
	"net/http"
)

// ReviewEvent is the kind of review PostReview records. Forges with a first-class
// review entity (GitHub) map these onto it directly; forges without one (GitLab,
// Gitea) map non-Comment events onto their nearest equivalent (an approval call
// plus a note). The workflow never has to know which.
type ReviewEvent string

const (
	ReviewComment        ReviewEvent = "comment"
	ReviewApprove        ReviewEvent = "approve"
	ReviewRequestChanges ReviewEvent = "request_changes"
)

// ForgeJob is the provider-neutral description of a unit of forge work, produced
// by ClassifyEvent from an inbound webhook and recorded on the task so the
// publish step needs no free-text parsing.
//
// The (Repo, Number) PAIR addresses the issue/change-request on every forge: Repo
// is the full namespace/project path and Number is repo/project-scoped (a GitHub
// PR/issue number or a GitLab project-scoped IID — both integers). No forge uses
// a forge-global number in these calls, so the pair needs no string encoding.
type ForgeJob struct {
	Provider        string   `json:"provider"`
	Repo            string   `json:"repo"`
	Action          string   `json:"action"`
	Number          int      `json:"number"`
	Labels          []string `json:"labels,omitempty"`
	DefaultBranch   string   `json:"default_branch,omitempty"`
	IsChangeRequest bool     `json:"is_change_request"`
	// HeadRef is the git ref that materializes a change request's head in a
	// working tree (GitHub: refs/pull/<n>/head — resolves even for fork PRs).
	// Set only for change-request jobs; the executor checks it out before
	// branching a worktree so a reviewer agent sees the PR's actual files
	// instead of the base/default branch (incident 2026-06-13: the reviewer
	// "couldn't locate any new files" because the tree was reset to default).
	HeadRef string `json:"head_ref,omitempty"`

	// HeadSHA is the head COMMIT this job's review covers, as distinct from
	// HeadRef's branch name. A branch name moves; the review has to name the
	// commit it actually read, or the recorded baseline means nothing.
	HeadSHA string `json:"head_sha,omitempty"`

	// FullReview asks for the whole change request, ignoring any incremental
	// baseline. Set by the explicit "full review" command.
	FullReview bool `json:"full_review,omitempty"`

	// OnDemand marks a job a HUMAN explicitly asked for rather than one an
	// event produced. Never paused away and never coalesced away: asking a
	// second time is asking for a fresh answer.
	OnDemand bool `json:"on_demand,omitempty"`

	// Command names the explicit instruction behind an on-demand job ("review",
	// "full review", "pause", "resume"). Empty for event-driven jobs. Carried so
	// the ingress can act on pause/resume without re-parsing the comment.
	Command string `json:"command,omitempty"`

	// CommentBody and CommentAuthor carry the request a review is ANSWERING, so
	// the posted review can quote it.
	//
	// Without them a review triggered by a comment is orphaned from its request:
	// a reader sees a verdict with no idea what was asked, and if the comment is
	// later edited or deleted the context is gone for good. Empty for
	// event-driven jobs, which have no request to quote.
	CommentBody   string `json:"comment_body,omitempty"`
	CommentAuthor string `json:"comment_author,omitempty"`

	// AuthorIsTrusted reports that the comment behind this job came from someone
	// entitled to spend the project's review budget.
	//
	// SET BY THE PROVIDER, like AuthorIsBot and for the same reason: the signal
	// is provider-specific while the rule is not. A review is real model spend
	// and on a PUBLIC repository anyone can comment, so an ungated command is a
	// denial-of-wallet primitive. Callers MUST refuse a command whose job does
	// not carry this.
	//
	// Deliberately fails CLOSED: an unrecognised or absent signal is untrusted.
	// The cost of that is a maintainer occasionally having to re-run a review by
	// hand; the cost of the opposite is a stranger draining the budget.
	AuthorIsTrusted bool `json:"author_is_trusted,omitempty"`

	// AuthorIsBot reports that the comment behind this job was written by a bot
	// rather than a person.
	//
	// SET BY THE PROVIDER, because detection is provider-specific — GitHub has
	// user.type == "Bot", GitLab does not. The RULE it serves is neutral and
	// belongs to every provider: the review this system posts is itself a
	// comment, so a bot-authored command would let a review trigger another
	// review. Callers must refuse to act on a job with this set.
	AuthorIsBot bool `json:"author_is_bot,omitempty"`
	// Title and Body are the issue/CR text, carried so the agent can be given a
	// clean spec (not the raw webhook JSON) and the change request gets a
	// meaningful title/body instead of a bare "Fix #N".
	Title string `json:"title,omitempty"`
	Body  string `json:"body,omitempty"`
	// Kind discriminates how the job originated, selecting which publish
	// templates the forge.open_change_request handler applies:
	//   - "" or "issue" — issue-driven (today's shape): the (Repo, Number)
	//     pair addresses a real issue/CR and the CR body closes it.
	//   - "backlog" — an autonomy BACKLOG.md item with NO inbound issue.
	//     Number is absent; Slug supplies the deterministic branch name and
	//     the CR body carries no "Closes #N" line. Stamped by the autonomy
	//     backlog tick (internal/autonomy) for projects with a resolvable
	//     outbound repo, so a backlog-item workflow can publish a draft PR.
	Kind string `json:"kind,omitempty"`
	// Slug is the deterministic branch slug for numberless (backlog-origin)
	// jobs: with no issue Number to key the publish branch off, the branch is
	// "backlog/<Slug>". Ignored for issue-driven jobs. Deterministic — the
	// same backlog item always yields the same slug, so a re-dispatched item
	// produces the same branch and the forge-side idempotency (lookup by
	// head) holds.
	Slug string `json:"slug,omitempty"`
}

// ChangeRequestSpec describes a pull/merge request to open. Title/Body are
// templated daemon-side from the issue, never LLM-authored.
type ChangeRequestSpec struct {
	Repo   string
	Head   string
	Base   string
	Title  string
	Body   string
	Labels []string
	Draft  bool
}

// ReviewSpec is the content of a review to post on a change request. Body is the
// reviewer agent's prose; Event selects the review semantics.
type ReviewSpec struct {
	Body  string
	Event ReviewEvent
}

// ForgeProvider abstracts a code-hosting forge. Implementations live in
// internal/forge/<provider> and are constructed already bound to their
// credentials, so none of these methods take an auth argument.
type ForgeProvider interface {
	// Name is the provider discriminator ("github" | "gitlab" | "gitea").
	Name() string
	// ClassifyEvent turns a verified inbound webhook into a ForgeJob,
	// deterministically. ok is false for events this provider ignores.
	ClassifyEvent(h http.Header, body []byte) (job ForgeJob, ok bool)
	// FetchDiff returns the unified diff for a change request, daemon-side, so the
	// reviewer agent never needs forge CLI access.
	FetchDiff(ctx context.Context, repo string, number int) ([]byte, error)
	// CompareDiff returns the unified diff between two commits, so a re-review
	// can look at what CHANGED since the last one rather than re-reading the
	// whole change request.
	//
	// It MUST return an error when base is not an ancestor of head — after a
	// force-push the recorded baseline can be unreachable, and the caller
	// treats that error as "fall back to the full diff". Returning an empty or
	// partial diff there would silently narrow a review, which is the failure
	// this whole feature exists to prevent.
	CompareDiff(ctx context.Context, repo, base, head string) ([]byte, error)
	// PushBranch publishes branch at sha to the forge, pushing from the daemon's
	// local clone at gitDir (every supported forge is git-backed, so a local git
	// dir is provider-neutral). It MUST be idempotent — a no-op when the remote
	// ref already points at sha — and MUST NOT force-push a divergent ref (a
	// non-fast-forward push is rejected, not forced). Implementations MUST keep
	// credentials out of process argv.
	PushBranch(ctx context.Context, gitDir, repo, branch, sha string) error
	// OpenChangeRequest opens a PR/MR and returns its URL. It MUST be idempotent:
	// if a change request already exists for s.Head, return its URL rather than
	// opening a duplicate.
	OpenChangeRequest(ctx context.Context, s ChangeRequestSpec) (url string, err error)
	// PostReview posts r against the change request identified by (repo, number).
	PostReview(ctx context.Context, repo string, number int, r ReviewSpec) error
	// VerifyPushAccess reports whether the provider's credentials can push
	// branches (the permission OpenChangeRequest needs). A non-nil error means
	// the integration is mis-permissioned or unreachable — callers log it at boot
	// so an operator fixes it before the first publish fails. Provider-neutral:
	// GitHub checks the App's contents:write; GitLab/Gitea check their token scope.
	VerifyPushAccess(ctx context.Context) error
}
