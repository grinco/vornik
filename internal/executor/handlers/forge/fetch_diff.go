package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"vornik.io/vornik/internal/executor"
	forgeapi "vornik.io/vornik/internal/forge"
	"vornik.io/vornik/internal/persistence"
)

// FetchDiffHandler implements the "forge.fetch_diff" system step: fetch a change
// request's diff daemon-side and pass it to the next step as the result message,
// so the reviewer agent never needs forge CLI / network access. Deterministic.
type FetchDiffHandler struct {
	resolver ProviderResolver

	// reviewState carries the ABSORBING → CLOSING transition (design §5.2).
	// Optional: nil means no coalescing bookkeeping, and the diff is still
	// fetched. Failing a review because the bookkeeping is unavailable would
	// trade a duplicate review for no review at all.
	reviewState persistence.ForgePRReviewStateRepository
}

// NewFetchDiffHandler wires the handler.
func NewFetchDiffHandler(resolver ProviderResolver) *FetchDiffHandler {
	return &FetchDiffHandler{resolver: resolver}
}

// WithReviewState attaches the PR review-state store, enabling the
// ABSORBING → CLOSING transition.
func (h *FetchDiffHandler) WithReviewState(s persistence.ForgePRReviewStateRepository) *FetchDiffHandler {
	if h != nil {
		h.reviewState = s
	}
	return h
}

// Name implements executor.SystemHandler.
func (h *FetchDiffHandler) Name() string { return "forge.fetch_diff" }

// Execute implements executor.SystemHandler. The result carries both a `message`
// (the diff, so the next agent step receives it as its prior-step context) and a
// `diff` field for any structured consumer.
func (h *FetchDiffHandler) Execute(ctx context.Context, in executor.SystemStepInput) (executor.SystemStepResult, error) {
	const name = "forge.fetch_diff"
	if h == nil || h.resolver == nil {
		return executor.SystemStepResult{}, errors.New(name + ": handler is missing required dependencies (resolver)")
	}
	job, err := forgeJobFromTask(in.Task, name)
	if err != nil {
		return executor.SystemStepResult{}, err
	}
	provider, err := h.resolver.ForgeProvider(ctx, in.Task.ProjectID)
	if err != nil {
		return executor.SystemStepResult{}, fmt.Errorf("%s: resolve provider: %w", name, err)
	}
	diff, scope, head, err := h.diffForJob(ctx, provider, in.Task.ProjectID, job)
	if err != nil {
		return executor.SystemStepResult{}, fmt.Errorf("%s: fetch diff for %s#%d: %w", name, job.Repo, job.Number, err)
	}
	// ABSORBING → CLOSING (design §5.2). The fetch above is this review's
	// comparison point: from here it can no longer observe a newer head, so it
	// must stop absorbing pushes. Releasing the claim is what makes a later
	// push enqueue its OWN task rather than supersede a SHA nobody will read —
	// without it, a push arriving after this line is silently lost whenever the
	// developer then stops pushing, and a posted review sits on a head it never
	// examined.
	//
	// FAILING THE STEP IS THE SAFE DIRECTION HERE, which is not obvious.
	// Continuing with an unreleased claim looks kinder — the diff is already in
	// hand — but it leaves this task ABSORBING for the rest of its run, so a
	// push arriving before it finishes is swallowed and, if the developer then
	// stops, that head is never reviewed. Failing instead drives the task
	// terminal, at which point the claim reads as dead and the next push
	// enqueues its own review. We lose this run and keep the coverage.
	if h.reviewState != nil {
		if err := h.reviewState.BeginClosing(ctx, in.Task.ProjectID, job.Repo, job.Number, head); err != nil {
			return executor.SystemStepResult{}, fmt.Errorf("%s: release review claim for %s#%d: %w", name, job.Repo, job.Number, err)
		}
	}

	out, _ := json.Marshal(map[string]any{
		"message": string(diff),
		"diff":    string(diff),
		"repo":    job.Repo,
		"number":  job.Number,
		// The reviewer must know whether it is looking at the whole change
		// request or only what changed since the last review — otherwise an
		// incremental diff reads as a suspiciously small PR and the prose
		// describes the wrong thing.
		"scope":    scope,
		"head_sha": head,
	})
	return executor.SystemStepResult{Result: out}, nil
}

// diffScope names what a fetched diff covers, so the reviewer's prose can say so.
const (
	scopeFull        = "full"
	scopeIncremental = "incremental"
	scopeNoChange    = "no-change"
)

// diffForJob picks between the whole change request and the range since the last
// review, and returns which it chose.
//
// EVERY UNCERTAINTY RESOLVES TO FULL. No state, an unreadable row, a missing
// head SHA, an unreachable baseline after a force-push — all fall back to the
// complete diff. The asymmetry is deliberate and is the core of §6: a full
// review costs tokens and repeats some findings, while a wrongly-narrowed one
// silently omits code nobody will look at again.
func (h *FetchDiffHandler) diffForJob(ctx context.Context, provider forgeapi.ForgeProvider, projectID string, job *forgeapi.ForgeJob) ([]byte, string, string, error) {
	head := job.HeadSHA
	full := func() ([]byte, string, string, error) {
		d, err := provider.FetchDiff(ctx, job.Repo, job.Number)
		return d, scopeFull, head, err
	}

	// An explicit "full review" command, or no state to compare against.
	if job.FullReview || h.reviewState == nil {
		return full()
	}

	st, err := h.reviewState.Get(ctx, projectID, job.Repo, job.Number)
	if err != nil {
		// Not fatal: an unreadable baseline means we do not know what was
		// already reviewed, and the honest answer to that is "review it all".
		return full()
	}
	// REVIEW THE SUPERSEDED HEAD, NOT THE ONE THAT CREATED THIS TASK.
	//
	// job.HeadSHA is the head of the delivery that enqueued this review; a push
	// absorbed while it was ABSORBING only advanced pending_head_sha. Fetching
	// the creating SHA would review an already-stale head and leave the
	// absorbed commits with no review and no task of their own — and if no
	// further push arrives, never reviewed at all. That is the dropped-push
	// failure coalescing must not introduce, and it is what §5 means by "the
	// running review picks up the latest SHA when it fetches the diff".
	if st != nil && st.PendingHeadSHA != "" {
		head = st.PendingHeadSHA
	}
	// A COMMENT COMMAND CARRIES NO HEAD. GitHub's issue_comment payload has an
	// issue object, and an issue has no head commit — so `@vornik review`
	// arrives with an empty HeadSHA. Bailing to a full review on that emptiness
	// (which this did until the recorded head was consulted first) made the
	// documented incremental command silently never incremental. With neither a
	// job head nor a recorded one there is nothing to bound a range with, and a
	// full review is the only honest answer.
	if head == "" {
		return full()
	}
	if st == nil || st.LastReviewedHeadSHA == "" {
		return full() // never reviewed
	}
	if st.LastReviewedHeadSHA == head {
		// Head is exactly what was last reviewed. Re-reading the whole PR would
		// repeat every finding the human has already seen; saying so plainly is
		// better than handing the reviewer an empty diff to invent prose about.
		return []byte("No new commits since the last review of " + head + "."), scopeNoChange, head, nil
	}

	d, cerr := provider.CompareDiff(ctx, job.Repo, st.LastReviewedHeadSHA, head)
	if cerr != nil {
		// The usual cause is a force-push that rewrote the baseline out of the
		// branch, so it is no longer an ancestor of head. We cannot know what
		// was already reviewed, so review everything.
		return full()
	}
	return d, scopeIncremental, head, nil
}
