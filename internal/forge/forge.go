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
	// Title and Body are the issue/CR text, carried so the agent can be given a
	// clean spec (not the raw webhook JSON) and the change request gets a
	// meaningful title/body instead of a bare "Fix #N".
	Title string `json:"title,omitempty"`
	Body  string `json:"body,omitempty"`
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
