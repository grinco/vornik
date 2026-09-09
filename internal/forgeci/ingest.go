// Package forgeci records completed CI runs and decides what they trigger.
//
// Design: https://docs.vornik.io §4, §5.
//
// SHARED BY BOTH INGRESSES, for the same reason forgereview.Coordinator is: the
// GitHub App channel and the generic relay webhook are two doors into the same
// behaviour, and this repository has already shipped a forge feature that
// reached one and missed the other. One implementation means a rule change
// cannot land on one path alone.
package forgeci

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/aidisclosure"
	"vornik.io/vornik/internal/forge"
	"vornik.io/vornik/internal/persistence"
)

// Run is what an ingress hands in: the reference a webhook carried.
type Run struct {
	ProjectID    string
	Repo         string
	RunID        int64
	HeadSHA      string
	Number       int
	Conclusion   string
	WorkflowName string
	WorkflowPath string
}

// Ingest records completed runs and applies the trigger rules.
//
// Nil-safe throughout: a project without CI ingestion has none, and must behave
// exactly as it did before this feature existed.
type Ingest struct {
	outcomes persistence.ForgeCIOutcomeRepository
	reader   forge.CIReader
	cfg      Config
	Logger   zerolog.Logger

	// commenter posts the status comment; disclosure supplies the Art 50(1)
	// notice it must carry. Both nil = no commenting, which is what a
	// deployment without CI ingestion configured has.
	commenter  Commenter
	disclosure Discloser

	// reviewState answers "has this head already been reviewed", the question
	// that turns a failure into a comment. Nil = always answer no, i.e. always
	// review.
	reviewState persistence.ForgePRReviewStateRepository
}

// Commenter is the forge ability this needs: post a plain comment, never a
// review state. Narrower than ForgeProvider so a caller can be tested without
// one, and named for the capability rather than the vendor.
type Commenter interface {
	PostComment(ctx context.Context, repo string, number int, body string) error
}

// Discloser supplies the Art 50(1) publication notice. An interface here so
// this package does not depend on internal/aidisclosure, matching how the forge
// handlers take theirs.
type Discloser interface {
	PublicationNotice() aidisclosure.Notice
}

// WithCommenting attaches the comment sink and its disclosure. Both are
// required together: a commenter without a discloser refuses to post.
func (g *Ingest) WithCommenting(c Commenter, d Discloser) *Ingest {
	if g != nil {
		g.commenter, g.disclosure = c, d
	}
	return g
}

// WithReviewState attaches the per-PR review state, enabling the
// already-reviewed test that turns a failure into a comment.
func (g *Ingest) WithReviewState(r persistence.ForgePRReviewStateRepository) *Ingest {
	if g != nil {
		g.reviewState = r
	}
	return g
}

// New builds an Ingest. A nil outcome store yields a nil Ingest: the caller's
// nil check is then the single place "CI ingestion is off" is expressed.
func New(outcomes persistence.ForgeCIOutcomeRepository, reader forge.CIReader, cfg Config, logger zerolog.Logger) *Ingest {
	if outcomes == nil {
		return nil
	}
	return &Ingest{outcomes: outcomes, reader: reader, cfg: cfg, Logger: logger}
}

// Cfg exposes the resolved configuration to a caller applying Decide.
func (g *Ingest) Cfg() Config {
	if g == nil {
		return Config{}
	}
	return g.cfg
}

// Config is the resolved per-project CI configuration (design §8).
// Config is the resolved per-project CI configuration (design §8).
type Config struct {
	ArtifactName      string
	MaxArtifactBytes  int64
	MaxExcerptBytes   int
	ReviewOnFailure   bool
	SuccessWorkflowID string
	// SuccessWorkflowNeedsChangeRequest is true when the success workflow's
	// steps include a forge handler that refuses a job with no pull request
	// (NeedsChangeRequest over the workflow's system handlers). Resolved at
	// wiring time from the registry; Decide cannot see the workflow itself.
	//
	// headmatch, 2026-09-09: the operator pointed success_workflow_id at the
	// REVIEW workflow so a completed run is the single review trigger. A merged
	// push to main is green and has no pull request, and forge.fetch_diff
	// refused the PR-less job on all three attempts. The design's success hook
	// was written for deposits on default-branch builds, which have no PR by
	// nature — so "PR present: either" was right for deposits and wrong for
	// reviews, and only the workflow's own steps can tell the two apart.
	SuccessWorkflowNeedsChangeRequest bool
	// CommentOnFailure posts a factual status comment when a failure cannot
	// produce a review. Defaults TRUE, unlike its siblings: their failure mode
	// when enabled is an unwanted behaviour change, while this one's failure
	// mode when disabled is silent signal loss (design §13.7).
	CommentOnFailure bool

	// WorkflowPaths limits which runs are RECORDED; TriggerWorkflowPaths limits
	// which of those may TRIGGER. Both empty = everything, i.e. the behaviour
	// before either existed. Design §14.
	//
	// WorkflowPaths lives HERE, in the code both ingresses share, and not in
	// either ingress. It was previously enforced only inside the GitHub App
	// channel, so on the generic relay ingress — the door this deployment
	// actually uses — it parsed and did nothing. Third instance of that class
	// in this feature; §14.4.
	WorkflowPaths        []string
	TriggerWorkflowPaths []string
}

// Watches reports whether a run on this workflow path is recorded at all.
//
// Matched on PATH rather than display name: a name is text an author can change
// without noticing anything depends on it.
func (c Config) Watches(path string) bool { return pathAllowed(c.WorkflowPaths, path) }

// Triggers reports whether a run on this workflow path may trigger, given that
// it is watched. Governs the failure and the green trigger alike — "is this run
// the gate?" is not a question about what the run concluded.
func (c Config) Triggers(path string) bool { return pathAllowed(c.TriggerWorkflowPaths, path) }

// pathAllowed: an empty filter allows everything, which is what makes both keys
// additive to every project already deployed.
func pathAllowed(allowed []string, path string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, a := range allowed {
		if a == path {
			return true
		}
	}
	return false
}

// Record fetches the run's full detail and stores it.
//
// Best-effort by design: a recording failure must not stop the trigger
// decision. The event already carries the conclusion, which is what the
// decision reads — losing the stored row costs a later review its context, not
// this delivery its behaviour.
func (g *Ingest) Record(ctx context.Context, ev Run) *persistence.ForgeCIOutcome {
	if g == nil || g.outcomes == nil {
		return nil
	}
	// An unwatched workflow is not recorded, and — because this sits BEFORE the
	// provider fetch below — does not cost an API call either. The cost being
	// avoided is the round-trip, not just the row (design §14.4).
	if !g.cfg.Watches(ev.WorkflowPath) {
		g.Logger.Debug().
			Str("repo", ev.Repo).Int64("run_id", ev.RunID).
			Str("workflow_path", ev.WorkflowPath).
			Msg("forgeci: workflow not watched; not recording")
		return nil
	}

	now := time.Now().UTC()
	out := &persistence.ForgeCIOutcome{
		ProjectID:    ev.ProjectID,
		Repo:         ev.Repo,
		RunID:        ev.RunID,
		HeadSHA:      ev.HeadSHA,
		Number:       ev.Number,
		WorkflowName: ev.WorkflowName,
		WorkflowPath: ev.WorkflowPath,
		RunAttempt:   1,
		Conclusion:   ev.Conclusion,
		CompletedAt:  now,
		RecordedAt:   now,
	}

	// The provider fills in what the webhook does not carry: per-job
	// conclusions, the run's real timing, and the artifact.
	if g.reader != nil {
		if run, err := g.reader.FetchCIRun(ctx, ev.Repo, ev.RunID); err == nil {
			applyRun(out, run)
		} else {
			g.Logger.Warn().Err(err).
				Str("repo", ev.Repo).Int64("run_id", ev.RunID).
				Msg("github task creator: CI run detail unavailable; recording the conclusion alone")
		}
		g.attachArtifact(ctx, out, ev)
	}

	if err := g.outcomes.Upsert(ctx, out); err != nil {
		g.Logger.Warn().Err(err).
			Str("repo", ev.Repo).Int64("run_id", ev.RunID).
			Msg("github task creator: CI outcome upsert failed")
	}
	return out
}

// applyCIRun copies the provider's detail onto the row, keeping the webhook's
// values wherever the API returned nothing.
func applyRun(out *persistence.ForgeCIOutcome, run forge.CIRun) {
	if run.Conclusion != "" {
		out.Conclusion = run.Conclusion
	}
	if run.HeadSHA != "" {
		out.HeadSHA = run.HeadSHA
	}
	if run.WorkflowPath != "" {
		out.WorkflowPath = run.WorkflowPath
	}
	if run.WorkflowName != "" {
		out.WorkflowName = run.WorkflowName
	}
	if run.RunAttempt > 0 {
		out.RunAttempt = run.RunAttempt
	}
	if !run.StartedAt.IsZero() {
		out.StartedAt = run.StartedAt
	}
	if !run.CompletedAt.IsZero() {
		out.CompletedAt = run.CompletedAt
	}
	// The webhook's pull request wins when the API has none, and vice versa:
	// either source may be the one that saw it.
	if out.Number == 0 && run.Number > 0 {
		out.Number = run.Number
	}
	for _, j := range run.Jobs {
		out.Jobs = append(out.Jobs, persistence.ForgeCIJob{
			Name: j.Name, Status: j.Status, Conclusion: j.Conclusion,
		})
	}
}

// attachCIArtifact downloads the configured artifact, if the run uploaded one.
//
// Every failure here is a WARNING and leaves the excerpt empty. A missing
// artifact is a fact about the pipeline, not a failure of the ingestion, and an
// operator reading a review needs the conclusions either way.
func (g *Ingest) attachArtifact(ctx context.Context, out *persistence.ForgeCIOutcome, ev Run) {
	name := g.cfg.ArtifactName
	if name == "" {
		return // content is opt-in; conclusions only
	}
	arts, err := g.reader.ListCIArtifacts(ctx, ev.Repo, ev.RunID)
	if err != nil {
		g.Logger.Warn().Err(err).Str("repo", ev.Repo).
			Msg("github task creator: artifact listing failed")
		return
	}
	var match *forge.CIArtifact
	for i := range arts {
		if arts[i].Name == name && !arts[i].Expired {
			match = &arts[i]
			break
		}
	}
	if match == nil {
		g.Logger.Debug().Str("artifact", name).Str("repo", ev.Repo).
			Msg("github task creator: run uploaded no such artifact; recording conclusions only")
		return
	}

	data, err := g.reader.FetchCIArtifact(ctx, ev.Repo, match.ID, g.cfg.MaxArtifactBytes)
	if err != nil {
		// A too-large artifact is reported at INFO rather than swallowed: the
		// operator's response is to raise the cap or upload less, and they can
		// only do that if they know it happened.
		if errors.Is(err, forge.ErrCIArtifactTooLarge) {
			g.Logger.Info().Err(err).Str("artifact", name).
				Msg("github task creator: artifact exceeds the configured ceiling; not stored")
			return
		}
		g.Logger.Warn().Err(err).Str("artifact", name).
			Msg("github task creator: artifact download failed")
		return
	}

	excerpt, truncated := capExcerpt(string(data), g.cfg.MaxExcerptBytes)
	out.ArtifactExcerpt = excerpt
	out.ArtifactBytes = len(excerpt)
	out.ArtifactTruncated = truncated
}

// capExcerpt trims to the byte cap, reporting whether it had to.
//
// Truncation is RETURNED, never inferred by a caller comparing lengths: a
// truncated plan that reads as a complete one is a wrong answer presented as a
// right one, and the flag is what lets the renderer say so.
// capExcerpt trims to the byte cap.
func capExcerpt(s string, limit int) (string, bool) {
	if limit <= 0 || len(s) <= limit {
		return s, false
	}
	return s[:limit], true
}

// Decision is what a completed run does beyond being recorded.
type Decision struct {
	// Enqueue is false for the record-only cases.
	Enqueue bool
	// Comment asks for a factual CI-status comment instead of a review
	// (design §13.3). Set when a failure lands on a head whose review has
	// already been posted, where enqueuing a review would only produce one
	// §16's guard then refuses.
	//
	// Never set together with Enqueue: they are alternatives, and a run that
	// did both would comment AND spawn the review the comment exists to
	// replace.
	Comment bool
	// WorkflowID names the workflow to run; empty means the caller's default.
	WorkflowID string
	// Reason is a short token for the log.
	Reason string
}

// Decide applies the design's §5 table.
func Decide(out *persistence.ForgeCIOutcome, cfg Config, alreadyReviewed bool) Decision {
	switch {
	case out == nil:
		return Decision{Reason: "no outcome recorded"}

	case !cfg.Triggers(out.WorkflowPath):
		// Recorded, so a review that runs for another reason still renders this
		// run's outcome — but not itself a trigger. The case neither key could
		// express alone (design §14.1): without it, a repo running five
		// workflows either posts five reviews on a green push or hides four
		// workflows' results from the one review it does post.
		return Decision{Reason: "workflow is not a trigger"}

	case out.Failed():
		// A failure is where a human wants Forge to speak. It needs a pull
		// request to speak ON, though: a failed default-branch build — or a
		// fork PR, whose event carries no pull request — is recorded and says
		// nothing, because there is no thread to post a review to.
		if !cfg.ReviewOnFailure {
			return Decision{Reason: "review_on_failure disabled"}
		}
		if !out.HasPullRequest() {
			return Decision{Reason: "failed run has no pull request to review"}
		}
		// ALREADY REVIEWED → COMMENT, NOT REVIEW (design §13.3). Enqueuing here
		// would run a reviewer against a no-change diff and produce a review
		// that post_review's guard then refuses — a paid agent run for silence.
		// The comment carries the same information for one API call.
		//
		// Optimistic, not a guarantee: this reads the baseline NOW, and a
		// review completing before ours posts is still caught by the guard
		// (§13.3). Uncertainty falls through to the review.
		if alreadyReviewed && cfg.CommentOnFailure {
			return Decision{Comment: true, Reason: "ci failed on an already-reviewed head"}
		}
		return Decision{Enqueue: true, Reason: "ci failed"}

	case out.Conclusion == "success":
		// Green enriches silently unless the operator asked for something. Off
		// by default: an empty workflow id.
		if cfg.SuccessWorkflowID == "" {
			return Decision{Reason: "green, no success workflow configured"}
		}
		// A workflow that needs a pull request cannot run on a build that has
		// none; enqueuing it buys a refusal per attempt. Recorded, and the
		// reason says so (design §18).
		if cfg.SuccessWorkflowNeedsChangeRequest && !out.HasPullRequest() {
			return Decision{Reason: "green run has no pull request, and the success workflow needs one"}
		}
		return Decision{
			Enqueue: true, WorkflowID: cfg.SuccessWorkflowID, Reason: "ci succeeded",
		}

	default:
		// skipped / neutral / action_required: nothing was attempted, so
		// there is nothing to say.
		return Decision{Reason: "conclusion " + out.Conclusion + " is not actionable"}
	}
}

// changeRequestHandlers are the forge system handlers that refuse a job with no
// pull request (executor/handlers/forge.forgeJobFromTask: "issue-driven: repo +
// number>0"). forge.fetch_ci is NOT here on purpose — it joins on head_sha and
// is the first step of a deposit on a merged-main build (design §3.3, §5.1).
//
// One declaration. The handlers package cannot be imported from here (it
// imports the executor), so this list is asserted against the handlers' own
// behaviour by a test on that side.
var changeRequestHandlers = map[string]bool{
	"forge.fetch_diff":          true,
	"forge.post_review":         true,
	"forge.open_change_request": true,
}

// NeedsChangeRequest reports whether a workflow whose system steps run the
// given handlers can only run against a pull request.
func NeedsChangeRequest(handlers []string) bool {
	for _, h := range handlers {
		if changeRequestHandlers[h] {
			return true
		}
	}
	return false
}

// Context is the reference a triggered task carries.
func Context(out *persistence.ForgeCIOutcome) map[string]string {
	if out == nil {
		return nil
	}
	return map[string]string{
		"ci_run_id":        strconv.FormatInt(out.RunID, 10),
		"ci_head_sha":      out.HeadSHA,
		"ci_conclusion":    out.Conclusion,
		"ci_workflow_name": out.WorkflowName,
		"ci_workflow_path": out.WorkflowPath,
	}
}

// Comment posts the factual CI-status comment for a run (design §13).
//
// NO MODEL IS IN THE LOOP. The body is assembled from what Forge already
// recorded — the workflow, the conclusion, the failing jobs — which is the
// whole reason this exists instead of letting a reviewer run on an empty diff:
// there is nothing here to invent.
//
// Returns whether a comment was posted. Not posting is the normal outcome for a
// run already commented on, and is not an error.
func (g *Ingest) Comment(ctx context.Context, out *persistence.ForgeCIOutcome) (bool, error) {
	if g == nil || g.commenter == nil || out == nil || !out.HasPullRequest() {
		return false, nil
	}
	if g.disclosure == nil {
		// Fail closed, exactly as forge.post_review and
		// forge.open_change_request do: unable to disclose means unable to
		// post. A third forge sink disclosing differently is the thing the Art
		// 50 perimeter argument cannot survive.
		return false, errors.New("forgeci: refusing to post an undisclosed CI comment (EU AI Act Art 50(1))")
	}

	// POST ONCE (design §13.6), by CLAIMING THE ROW BEFORE POSTING.
	//
	// The claim is what enforces this, not a check on `out`. The outcome here
	// was built from the webhook event and enrichment, so its CommentedAt is
	// always the zero value — the earlier `if !out.CommentedAt.IsZero()` read a
	// field nothing populates and let EVERY redelivery comment again (observed
	// live on headmatch PR #52, 2026-09-09: two comments for one run).
	//
	// Claiming BEFORE the post, rather than marking after it, also settles the
	// concurrent case: GitHub can have two deliveries of one run in flight, and
	// a read-then-post pair would let both see NULL and both post.
	claimed, err := g.outcomes.ClaimComment(ctx, out.ProjectID, out.Repo, out.RunID, time.Now().UTC())
	if err != nil {
		return false, fmt.Errorf("claim ci comment: %w", err)
	}
	if !claimed {
		return false, nil
	}

	body := renderCIComment(out) + "\n\n---\n" + g.disclosure.PublicationNotice().Text
	if err := g.commenter.PostComment(ctx, out.Repo, out.Number, body); err != nil {
		// Give the claim back so a redelivery retries. A missing comment is the
		// failure this whole section exists to prevent, and holding a claim for
		// a comment that was never published would cause exactly that.
		if rel := g.outcomes.ReleaseComment(ctx, out.ProjectID, out.Repo, out.RunID); rel != nil {
			g.Logger.Warn().Err(rel).
				Str("repo", out.Repo).Int64("run_id", out.RunID).
				Msg("forgeci: could not release the comment claim after a failed post; no retry will run")
		}
		return false, err
	}
	return true, nil
}

// renderCIComment writes the comment body. Facts only.
//
// The run LINK is CONSTRUCTED from the repository identity and the numeric run
// id — never from the workflow name or any other payload string (design §13.2).
// Rendering an attacker-supplied string as a URL in a pull request is a
// phishing surface, and the workflow name is the natural thing to have made
// clickable.
//
// The artifact excerpt is deliberately absent: it is attacker-controlled and a
// comment has no untrusted-content wrapper, so it stays where it does — in
// forge.fetch_ci, into a model's context.
func renderCIComment(out *persistence.ForgeCIOutcome) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**CI %s** on `%s`\n\n", sanitiseCIText(out.Conclusion), shortSHA(out.HeadSHA))
	fmt.Fprintf(&b, "Workflow `%s` concluded **%s**.\n",
		displayCIWorkflow(out), sanitiseCIText(out.Conclusion))

	var failed []string
	for _, j := range out.Jobs {
		if j.Conclusion != "" && j.Conclusion != "success" && j.Conclusion != "skipped" {
			failed = append(failed, fmt.Sprintf("`%s` (%s)",
				sanitiseCIText(j.Name), sanitiseCIText(j.Conclusion)))
		}
	}
	if len(failed) > 0 {
		fmt.Fprintf(&b, "\nFailing jobs: %s\n", strings.Join(failed, ", "))
	}
	fmt.Fprintf(&b, "\n[View the run](https://github.com/%s/actions/runs/%d)\n", out.Repo, out.RunID)
	b.WriteString("\nNo review was posted: this commit has already been reviewed, " +
		"so there is no new code to look at.\n")
	return b.String()
}

// displayCIWorkflow names the workflow, never empty, and always safe to render.
func displayCIWorkflow(out *persistence.ForgeCIOutcome) string {
	if out.WorkflowPath != "" {
		return sanitiseCIText(out.WorkflowPath)
	}
	if out.WorkflowName != "" {
		return sanitiseCIText(out.WorkflowName)
	}
	return fmt.Sprintf("run %d", out.RunID)
}

// sanitiseCIText makes a payload-supplied string safe to render inside a
// markdown code span.
//
// The workflow name and job names come from the repository's own CI config, so
// anyone who can push a branch chooses them. Wrapping them in backticks is NOT
// enough on its own: a name containing a backtick CLOSES the span, and the rest
// renders as live markdown — a link, an image, anything. Found by the test for
// design §13.2, which had only required the run LINK to be constructed rather
// than echoed; the same argument applies to every payload string in the body,
// one level down.
//
// Backticks are removed rather than escaped: this is a display label, the
// authoritative reference is the constructed run link, and a mangled label is a
// better outcome than a clever escape that a future markdown dialect unpicks.
// Newlines go for the same reason — a name is one line.
func sanitiseCIText(s string) string {
	s = strings.TrimSpace(strings.NewReplacer("`", "", "\n", " ", "\r", " ").Replace(s))
	if len(s) > maxCITextChars {
		s = s[:maxCITextChars] + "…"
	}
	return s
}

// maxCITextChars bounds a rendered label so a pathological name cannot push the
// facts out of view.
const maxCITextChars = 120

// shortSHA abbreviates for display without pretending a truncated SHA is the
// identifier — the link above carries the authoritative reference.
func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// AlreadyReviewed reports whether this run's head has already been reviewed and
// posted about (design §13.4).
//
// Reads ForgePRReviewState — the SAME durable row forge.post_review's guard
// reads, through the same repository. A second source of this fact could
// disagree with the guard, and the two disagreeing is a review both suppressed
// and never commented on.
//
// EVERY UNCERTAINTY RESOLVES TO FALSE, i.e. to reviewing: an unwired
// repository, an unreadable row, a missing row or an empty head all fall
// through to the review path, which then either reviews something real or is
// refused by the guard with nothing lost but tokens.
func (g *Ingest) AlreadyReviewed(ctx context.Context, projectID string, out *persistence.ForgeCIOutcome) bool {
	if g == nil || g.reviewState == nil || out == nil || out.HeadSHA == "" {
		return false
	}
	st, err := g.reviewState.Get(ctx, projectID, out.Repo, out.Number)
	if err != nil || st == nil || st.LastReviewedHeadSHA == "" {
		return false
	}
	return st.LastReviewedHeadSHA == out.HeadSHA
}
