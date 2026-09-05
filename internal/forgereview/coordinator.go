// Package forgereview holds the rules that decide whether a forge change
// request gets reviewed right now — the per-PR pause gate and the coalescing
// decision that stops a push burst becoming one review per push.
//
// PROVIDER-NEUTRAL BY CONSTRUCTION. Everything here is keyed on
// forge.ForgeJob and persistence types; there is no GitHub in it. That is the
// point: this project has two forge ingresses (the GitHub App channel and the
// relay-aware generic webhook), and a third provider is on the roadmap with
// ProviderGitLab already reserved in internal/forge/config.go.
//
// The rules therefore live in ONE place that every ingress calls, rather than
// in whichever ingress happened to be built first. Phases 1–4 of the design
// shipped the logic inside the GitHub channel's task creator; that made it
// unreachable from the generic path — which is the path this deployment
// actually uses — and would have made a GitLab ingress import a package named
// `github` to get coalescing.
//
// Design: https://docs.vornik.io
// §5.2 (ABSORBING/CLOSING), §7 (pause), §13.3 (why it is extracted).
package forgereview

import (
	"context"
	"errors"

	"github.com/rs/zerolog"

	forgeapi "vornik.io/vornik/internal/forge"
	"vornik.io/vornik/internal/persistence"
)

// StateStore is the per-PR review state this package needs. Narrower than the
// full repository interface on purpose: the coordinator neither marks reviews
// nor closes them, and a smaller surface is a smaller thing for a second
// implementation to get wrong.
type StateStore interface {
	Get(ctx context.Context, projectID, repo string, number int) (*persistence.ForgePRReviewState, error)
	ClaimOrSupersede(ctx context.Context, projectID, repo string, number int, headSHA string) (string, error)
	SetTask(ctx context.Context, projectID, repo string, number int, taskID string) error
	SetPaused(ctx context.Context, projectID, repo string, number int, paused bool) error
}

// TaskStatusReader answers the one question the coalescing rule asks of the
// task store: what state is this task in, and does it still exist.
//
// Deliberately NOT persistence.TaskRepository. The claim is DERIVED from a task
// row, so this package must read one — but it has no business creating,
// leasing, or cancelling tasks, and depending on the whole repository would let
// it grow that ability by accident.
type TaskStatusReader interface {
	TaskStatus(ctx context.Context, taskID string) (status persistence.TaskStatus, found bool, err error)
}

// Decision is the coordinator's answer.
type Decision struct {
	// Skip means "do not create a task for this delivery". The delivery has
	// still been recorded — a superseding push advanced the head the in-flight
	// review will read.
	Skip bool

	// Reason is a short machine-ish token for logs and metrics.
	Reason string
}

// Coordinator applies the pause and coalescing rules.
type Coordinator struct {
	state  StateStore
	tasks  TaskStatusReader
	logger zerolog.Logger
}

// New builds a Coordinator. Both dependencies may be nil, in which case every
// decision is "enqueue" — see Decide.
func New(state StateStore, tasks TaskStatusReader, logger zerolog.Logger) *Coordinator {
	return &Coordinator{state: state, tasks: tasks, logger: logger}
}

// Decide reports whether this delivery should be skipped.
//
// EVERY UNCERTAINTY RESOLVES TO ENQUEUE. A nil store, an unreadable row, a
// lookup error, a claim naming a task that is gone — all produce a review.
// Coalescing is a cost optimisation: reviewing twice wastes a run, while not
// reviewing at all is the failure this whole feature exists to prevent. That
// asymmetry is why Decide returns no error; there is none a caller could act on.
//
// onDemand marks a review a human explicitly asked for. Those are never paused
// away and never coalesced away: asking a second time is asking for a fresh
// answer, and pause must not become a trap the operator cannot escape from the
// very thread they set it in.
func (c *Coordinator) Decide(ctx context.Context, projectID string, job forgeapi.ForgeJob, onDemand bool) Decision {
	// AUTHOR TRUST IS THE FIRST GATE, ahead of every early return below —
	// including the nil-store one. A deployment with no review state still must
	// not let a stranger spend its review budget, and "uncertainty resolves to
	// enqueue" is a rule about COALESCING, not about authorization.
	//
	// onDemand means a human typed a command. Automatic triggers are exempt by
	// construction: a push carries no author standing, and gating it would stop
	// reviewing pull requests altogether (design §7.1).
	//
	// It lives HERE rather than in an ingress because the generic webhook path
	// got this gate (9ecedb2c, webhook_handlers.go) and the GitHub App channel
	// did not, while both dispatch the identical command grammar. The App
	// channel leaned on its SenderAllowlist, which is documented and coded as
	// "empty allows all logins" — a default-open gate on a public repository is
	// a denial-of-wallet primitive. Both ingresses reach this function, so
	// putting the refusal here is what makes the promise in
	// docs/public/features/forge.md ("only people with standing in the
	// repository can run these") true of the product rather than of one path.
	//
	// The generic ingress ALSO refuses earlier, where it can record a
	// webhook_events row and answer the delivery; this is the floor under it,
	// not a replacement for it.
	if onDemand && !job.AuthorIsTrusted {
		if c != nil {
			c.logger.Warn().
				Str("project_id", projectID).
				Str("repo", job.Repo).Int("number", job.Number).
				Str("command", job.Command).
				Msg("forgereview: command from an author without repository standing; refusing")
		}
		return Decision{Skip: true, Reason: "author_untrusted"}
	}

	if c == nil || c.state == nil || !job.IsChangeRequest || job.Repo == "" || job.Number == 0 {
		return Decision{}
	}

	if !onDemand && c.pausedFor(ctx, projectID, job) {
		return Decision{Skip: true, Reason: "auto_review_paused"}
	}

	// Record this head as the newest observed for the PR, and learn which task
	// (if any) currently holds the claim. One call: a read-then-write would let
	// two deliveries both observe an empty claim and both enqueue.
	prior, err := c.state.ClaimOrSupersede(ctx, projectID, job.Repo, job.Number, job.HeadSHA)
	if err != nil {
		c.logger.Warn().Err(err).
			Str("repo", job.Repo).Int("number", job.Number).
			Msg("forgereview: coalescing lookup failed; enqueueing anyway")
		return Decision{}
	}
	// An explicit request supersedes the head like any other delivery — the
	// in-flight review should still read the newest code — but is never
	// absorbed by it.
	if onDemand || prior == "" {
		return Decision{}
	}

	// THE CLAIM IS DERIVED, NOT TRUSTED. Whether `prior` is a LIVE claim is
	// decided by the task's own status. A stored boolean could outlive its task
	// and absorb every later push into a corpse, leaving the PR unreviewed until
	// a daemon restart.
	if c.tasks == nil {
		return Decision{}
	}
	status, found, err := c.tasks.TaskStatus(ctx, prior)
	if err != nil || !found {
		c.logger.Debug().
			Str("repo", job.Repo).Int("number", job.Number).Str("claim_task_id", prior).
			Msg("forgereview: review claim names a task that is gone; enqueueing")
		return Decision{}
	}
	if !canStillReview(status) {
		return Decision{}
	}

	c.logger.Info().
		Str("repo", job.Repo).Int("number", job.Number).
		Str("claim_task_id", prior).Str("head_sha", job.HeadSHA).
		Msg("forgereview: a review is already in flight for this PR; superseding its head instead of enqueueing")
	return Decision{Skip: true, Reason: "superseded"}
}

// Claim points the PR's state row at the review task that now owns it.
//
// Called AFTER the task is created, never before: a claim naming a task that
// failed to insert would absorb every later push into something that does not
// exist. The window that opens between the create and this call is accepted —
// a push landing in it enqueues a duplicate review, which is the safe
// direction; the alternative ordering trades that for a wedged PR.
func (c *Coordinator) Claim(ctx context.Context, projectID string, job forgeapi.ForgeJob, taskID string) {
	if c == nil || c.state == nil || !job.IsChangeRequest || job.Repo == "" || job.Number == 0 {
		return
	}
	if err := c.state.SetTask(ctx, projectID, job.Repo, job.Number, taskID); err != nil {
		// Non-fatal: the review is enqueued and running it is the point.
		// Losing the claim costs one duplicate review on the next push.
		c.logger.Warn().Err(err).
			Str("repo", job.Repo).Int("number", job.Number).
			Msg("forgereview: could not record the review claim; the next push may enqueue a duplicate")
	}
}

// SetPaused sets or clears the per-PR automatic-review suppression.
func (c *Coordinator) SetPaused(ctx context.Context, projectID, repo string, number int, paused bool) error {
	if c == nil || c.state == nil {
		return errNoStore
	}
	return c.state.SetPaused(ctx, projectID, repo, number, paused)
}

// pausedFor reports whether automatic review is suppressed for this PR.
//
// An unreadable flag reads as NOT paused. A phantom pause costs every review on
// the PR; a lost one costs a single unwanted review.
func (c *Coordinator) pausedFor(ctx context.Context, projectID string, job forgeapi.ForgeJob) bool {
	st, err := c.state.Get(ctx, projectID, job.Repo, job.Number)
	if err != nil {
		c.logger.Warn().Err(err).
			Str("repo", job.Repo).Int("number", job.Number).
			Msg("forgereview: could not read the pause flag; treating this PR as not paused")
		return false
	}
	return st != nil && st.AutoReviewPaused
}

// canStillReview reports whether the task holding a claim might yet produce a
// review.
//
// CLOSED counts as terminal here. The package-level isTerminalTaskStatus in
// internal/service answers a different question for the orphan-skip rule and
// does not count it; for coalescing a closed task will never review anything
// again, and treating its claim as live would wedge the PR.
func canStillReview(s persistence.TaskStatus) bool {
	switch s {
	case persistence.TaskStatusCompleted,
		persistence.TaskStatusFailed,
		persistence.TaskStatusCancelled,
		persistence.TaskStatusClosed:
		return false
	default:
		return true
	}
}

// errNoStore is returned when a state operation is asked of a coordinator with
// no store wired. Distinct from a silent no-op: a pause the operator asked for
// and did not get must be reported, not swallowed.
var errNoStore = errors.New("forgereview: review state is not wired; cannot record the pause state")
