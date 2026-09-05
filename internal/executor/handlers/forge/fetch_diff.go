package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/rs/zerolog"

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
	diff, scope, head, err := h.fetchAndClose(ctx, provider, in.Task.ProjectID, job, name)
	if err != nil {
		return executor.SystemStepResult{}, err
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

// maxFetchAttempts bounds the re-fetch loop in fetchAndClose.
//
// Each attempt is one forge round-trip, and another attempt is only needed
// because a push landed DURING the previous one — so converging takes a quiet
// moment, not a fixed number of tries. Three is generous for a human push
// burst and small enough that a pathological pusher cannot hold a review step
// open indefinitely.
const maxFetchAttempts = 3

// fetchAndClose fetches the diff and performs the ABSORBING → CLOSING
// transition (design §5.2), re-fetching when a push superseded the head while
// the fetch was in flight.
//
// THE FETCH IS THE COMPARISON POINT, AND IT IS NOT INSTANT. §5.2 requires the
// pending-head read and the transition to be one atomic step "so no trigger can
// land between them"; a real forge round-trip sits between them here, and for
// its whole duration the claim is still held — so a push landing in that window
// is ABSORBED, its delivery skipped as "superseded", and only pending_head_sha
// advances. Committing to the pre-fetch head then strands it: the baseline
// advances past a head nobody read, and the ONLY consumer of pending_head_sha
// is the read at the top of this loop. If the developer does not push again,
// that head is permanently unreviewed while a posted review implies coverage —
// the §5.2 "SHA_C is lost" failure the design calls unacceptable.
//
// So the transition is a compare-and-set, and a refusal means "a push you
// absorbed is still yours to review": fetch again at the head it names. This is
// §5.1's re-arm applied at the point that actually needs it. It converges as
// soon as one fetch completes with no push landing during it.
//
// FAILING THE STEP IS THE SAFE DIRECTION when it does not converge, which is
// not obvious. Continuing with an unreleased claim looks kinder — a diff is
// already in hand — but it leaves this task ABSORBING for the rest of its run,
// so a push arriving before it finishes is swallowed and, if the developer then
// stops, that head is never reviewed. Failing instead drives the task terminal,
// at which point the claim reads as dead and the next push enqueues its own
// review. We lose this run and keep the coverage — and a failed task is loud,
// where a narrowed review is silent.
func (h *FetchDiffHandler) fetchAndClose(
	ctx context.Context,
	provider forgeapi.ForgeProvider,
	projectID string,
	job *forgeapi.ForgeJob,
	name string,
) ([]byte, string, string, error) {
	// reported is the pending head the previous refusal named. It is the
	// comparison point of last resort: see the unreadable-row case below.
	reported, haveReported := "", false

	for attempt := 1; ; attempt++ {
		diff, scope, head, observed, err := h.diffForJob(ctx, provider, projectID, job)
		if err != nil {
			return nil, "", "", fmt.Errorf("%s: fetch diff for %s#%d: %w", name, job.Repo, job.Number, err)
		}
		if h.reviewState == nil {
			// No coalescing bookkeeping: nothing can have been absorbed, so
			// there is nothing to reconcile. Failing a review because the
			// bookkeeping is unavailable would trade a duplicate review for no
			// review at all.
			return diff, scope, head, nil
		}

		// AN UNREADABLE ROW MUST NOT FAIL THE REVIEW. diffForJob resolves a
		// failed Get to a full review and reports no observation — and with no
		// observation the compare-and-set has nothing true to compare, so it
		// would refuse every attempt and the review would die because the
		// BOOKKEEPING was unavailable. That is the opposite of this handler's
		// stated asymmetry (see WithReviewState) and of §6's "every uncertainty
		// resolves to full".
		//
		// A refusal always names the row's current pending head, so after one
		// refusal we have a comparison point that did not come through Get.
		// Using it converges in two attempts even while Get keeps failing, and
		// it is still a compare-and-set — a push landing after that refusal
		// refuses again rather than being stranded:
		//
		//   attempt 1  unknown, expects ""      → refused, reported = SHA_A
		//   push lands, row advances to SHA_B
		//   attempt 2  unknown, expects SHA_A   → refused, reported = SHA_B
		//   attempt 3  unknown, expects SHA_B   → applies
		//
		// The two-attempt convergence is bounded by maxFetchAttempts like any
		// other; it is not a second counter. A push arriving during those
		// attempts moves the reported head without resetting the bound, which
		// is right — an outage plus a sustained burst is exactly the case that
		// should give up and let the claim die with the task.
		expected := observed.value
		if !observed.known && haveReported {
			expected = reported
			// Rare and worth seeing: the store is failing reads while this
			// review is trying to close. Logged only on the fallback, so the
			// nominal path stays silent.
			zerolog.Ctx(ctx).Warn().
				Str("repo", job.Repo).Int("number", job.Number).
				Str("expected_head", expected).Int("attempt", attempt).
				Msg("forge.fetch_diff: review state unreadable; closing against the head the last refusal reported")
		}

		out, err := h.reviewState.BeginClosing(ctx, projectID, job.Repo, job.Number, head, expected)
		if err != nil {
			return nil, "", "", fmt.Errorf("%s: release review claim for %s#%d: %w", name, job.Repo, job.Number, err)
		}
		if out.Applied {
			return diff, scope, head, nil
		}
		reported, haveReported = out.PendingHeadSHA, true

		if attempt >= maxFetchAttempts {
			return nil, "", "", fmt.Errorf(
				"%s: %s#%d kept being superseded while fetching (%d attempts, last head %q, now %q); "+
					"failing so the claim dies with this task and the next trigger enqueues its own review",
				name, job.Repo, job.Number, attempt, head, out.PendingHeadSHA)
		}
		// Loop: diffForJob re-reads the row, so the next attempt fetches the
		// head that superseded this one.
	}
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
//
// It also returns the PendingHeadSHA it observed, which is the value the
// caller's compare-and-set must be made against — not the head it fetched. The
// two differ whenever the row's pending head was empty or the job's own head
// won, and comparing the wrong one would refuse transitions nothing superseded.
func (h *FetchDiffHandler) diffForJob(ctx context.Context, provider forgeapi.ForgeProvider, projectID string, job *forgeapi.ForgeJob) ([]byte, string, string, pendingObservation, error) {
	head := job.HeadSHA
	observed := pendingObservation{}
	full := func() ([]byte, string, string, pendingObservation, error) {
		d, err := provider.FetchDiff(ctx, job.Repo, job.Number)
		return d, scopeFull, head, observed, err
	}

	// An explicit "full review" command, or no state to compare against.
	if job.FullReview || h.reviewState == nil {
		if job.FullReview && h.reviewState != nil {
			// Still read the row: a full review absorbs pushes exactly like an
			// incremental one, so its transition needs the same comparison
			// point. Only the SCOPE ignores the baseline — skipping the
			// compare-and-set here would leave the dropped-push hole open on
			// the one command a human explicitly asked for.
			//
			// The head it RECORDS stays the job's own, deliberately. An
			// incremental review names its upper bound in the request
			// (CompareDiff base..head) so it can claim that head; a full fetch
			// returns whatever the forge had at some instant during the call,
			// which we cannot name. Claiming the pending head would assert
			// coverage of commits we only hope were included. Under-claiming
			// costs a repeated finding; over-claiming loses a commit.
			if st, err := h.reviewState.Get(ctx, projectID, job.Repo, job.Number); err == nil {
				observed = observePending(st)
			}
		}
		return full()
	}

	st, err := h.reviewState.Get(ctx, projectID, job.Repo, job.Number)
	if err != nil {
		// Not fatal: an unreadable baseline means we do not know what was
		// already reviewed, and the honest answer to that is "review it all".
		return full()
	}
	observed = observePending(st)
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
		return []byte("No new commits since the last review of " + head + "."), scopeNoChange, head, observed, nil
	}

	d, cerr := provider.CompareDiff(ctx, job.Repo, st.LastReviewedHeadSHA, head)
	if cerr != nil {
		// The usual cause is a force-push that rewrote the baseline out of the
		// branch, so it is no longer an ancestor of head. We cannot know what
		// was already reviewed, so review everything.
		return full()
	}
	return d, scopeIncremental, head, observed, nil
}

// pendingObservation distinguishes "the row says there is no pending head" from
// "the row could not be read".
//
// Both used to be the empty string, and conflating them is what made an
// unreadable row indistinguishable from a genuinely empty pending head — which
// turned a bookkeeping outage into a refused transition on every attempt, and
// so into a failed review.
type pendingObservation struct {
	value string
	known bool
}

func observePending(st *persistence.ForgePRReviewState) pendingObservation {
	// A nil row is a REAL observation: the PR has no state yet, so its pending
	// head is genuinely empty. Only a failed read is unknown.
	if st == nil {
		return pendingObservation{known: true}
	}
	return pendingObservation{value: st.PendingHeadSHA, known: true}
}
