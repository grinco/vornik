package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/aidisclosure"
	"vornik.io/vornik/internal/executor"
	forgeapi "vornik.io/vornik/internal/forge"
	"vornik.io/vornik/internal/persistence"
)

// Discloser supplies the EU AI Act Art 50(1) notice for authored artifacts.
// A local one-method interface so this package depends on the policy, not on
// the service that owns per-session state it has no use for.
type Discloser interface {
	PublicationNotice() aidisclosure.Notice
}

// PostReviewHandler implements the "forge.post_review" system step: post the
// reviewer agent's prose against the change request as a review/note. The body
// is the LLM's text; the posting itself is deterministic.
type PostReviewHandler struct {
	resolver   ProviderResolver
	disclosure Discloser

	// reviewState advances the incremental baseline once a review has actually
	// posted (design §6). Optional: nil simply means no baseline is recorded,
	// and the next review is a full one.
	reviewState persistence.ForgePRReviewStateRepository

	// ciOutcomes answers "did CI fail on the head this review covers", which
	// gates an APPROVE (2026-09-08-forge-ci-outcomes-design.md §16). Nil = the
	// gate cannot engage, i.e. exactly today's behaviour for any project
	// without CI ingestion.
	ciOutcomes persistence.ForgeCIOutcomeRepository
	// blocksApproval resolves the operator's switch FOR A PROJECT. A function
	// rather than a bool because this handler is registered once for the
	// daemon while `block_approval_on_failure` is per-project config — baking
	// one project's answer in at construction would apply it to every project.
	// Nil = the gate cannot engage.
	blocksApproval func(projectID string) bool
}

// WithCIGate attaches the CI-outcome store and the per-project switch, enabling
// the refusal to submit an APPROVE over a red pipeline.
func (h *PostReviewHandler) WithCIGate(o persistence.ForgeCIOutcomeRepository, blocks func(projectID string) bool) *PostReviewHandler {
	if h != nil {
		h.ciOutcomes, h.blocksApproval = o, blocks
	}
	return h
}

// WithReviewState attaches the PR review-state store so a posted review
// advances the incremental baseline.
func (h *PostReviewHandler) WithReviewState(s persistence.ForgePRReviewStateRepository) *PostReviewHandler {
	if h != nil {
		h.reviewState = s
	}
	return h
}

// NewPostReviewHandler wires the handler.
//
// disclosure is REQUIRED. A review comment reaches a human — the developer whose
// pull request it lands on — so it carries the Art 50(1) notice, and this
// surface does not go through the dispatcher chokepoint that covers channels
// (G6 finding A, 2026-07-29). A nil disclosure makes Execute refuse rather than
// publish undisclosed.
func NewPostReviewHandler(resolver ProviderResolver, disclosure Discloser) *PostReviewHandler {
	return &PostReviewHandler{resolver: resolver, disclosure: disclosure}
}

// discloseSuffix is the separator between the review and its notice. A rule plus
// its own line so the notice survives being quoted into another thread, and so
// it reads as a distinct statement rather than part of the reviewer's prose
// (Art 50(5) "clear and distinguishable").
const discloseSuffix = "\n\n---\n"

// withDisclosure appends the Art 50(1) publication notice to a review body.
func withDisclosure(body string, n aidisclosure.Notice) string {
	return body + discloseSuffix + n.Text
}

// Name implements executor.SystemHandler.
func (h *PostReviewHandler) Name() string { return "forge.post_review" }

// reviewInput is read from the previous (reviewer agent) step's result. Accepts
// an explicit {body,event}, the agent step's {message:"..."} envelope, or a bare
// {result|output:"..."}, defaulting the event to a non-gating comment.
type reviewInput struct {
	Body    string `json:"body"`
	Event   string `json:"event"`
	Message string `json:"message"`
	Result  string `json:"result"`
	Output  string `json:"output"`
}

// reviewStructured is the reviewer role's structured output (see the `reviewer`
// outputSchema in the swarm presets: {"review":{approved,all_done,feedback,
// checked_commit,summary,remaining}}). post_review renders feedback+summary
// from this into clean markdown so GitHub never sees the raw JSON envelope or
// the agent's reasoning prose.
type reviewStructured struct {
	Approved  *bool  `json:"approved"`
	Feedback  string `json:"feedback"`
	Summary   string `json:"summary"`
	Remaining []any  `json:"remaining"`
}

// reviewBodyEvent picks the review body + event from the prior step result.
// It prefers the reviewer's STRUCTURED output rendered to human-readable
// markdown; only when no structured review is present does it fall back to the
// raw body (back-compat for reviewers that emit plain prose).
//
// gating selects the review SEMANTICS. When false (the default), the body is
// always posted as a non-gating COMMENT — even when the reviewer approved — so
// the automation never gates a PR unless an operator opts in. When true, the
// event is derived from the reviewer's verdict (see gatingEvent), so an
// approval becomes a real forge APPROVE.
func reviewBodyEvent(prev json.RawMessage, gating bool) (string, forgeapi.ReviewEvent) {
	var in reviewInput
	if len(prev) > 0 {
		_ = json.Unmarshal(prev, &in)
	}
	rawBody := firstNonEmpty(in.Body, in.Message, in.Result, in.Output)

	body := rawBody
	if rendered, ok := renderStructuredReview(prev, rawBody); ok {
		body = rendered
	}

	event := forgeapi.ReviewComment
	if gating {
		event = gatingEvent(prev, rawBody, in.Event)
	}
	return body, event
}

// gatingEvent maps the reviewer's verdict onto a gating review event. An
// explicit `event` field on the reviewer output wins (a deliberate signal);
// otherwise the structured `review.approved` bool drives it (true → APPROVE,
// false → REQUEST_CHANGES). With no usable verdict it stays a non-gating
// COMMENT, so an unparseable review never silently approves a PR.
func gatingEvent(prev json.RawMessage, rawBody, explicit string) forgeapi.ReviewEvent {
	switch strings.ToLower(strings.TrimSpace(explicit)) {
	case "approve":
		return forgeapi.ReviewApprove
	case "request_changes", "request-changes":
		return forgeapi.ReviewRequestChanges
	}
	if r := extractReview(prev, rawBody); r != nil && r.Approved != nil {
		if *r.Approved {
			return forgeapi.ReviewApprove
		}
		return forgeapi.ReviewRequestChanges
	}
	return forgeapi.ReviewComment
}

// renderStructuredReview extracts the reviewer's structured output (from the
// prev envelope's top level, or as the last JSON object embedded in the body
// text after any reasoning prose) and renders feedback + summary + remaining
// into markdown. Returns ok=false when no structured review with usable text
// is found, so the caller falls back to the raw body.
func renderStructuredReview(prev json.RawMessage, rawBody string) (string, bool) {
	r := extractReview(prev, rawBody)
	if r == nil {
		return "", false
	}
	feedback := strings.TrimSpace(r.Feedback)
	summary := strings.TrimSpace(r.Summary)
	if feedback == "" && summary == "" {
		return "", false
	}

	var b strings.Builder
	if r.Approved != nil {
		if *r.Approved {
			b.WriteString("**✅ Approved**\n\n")
		} else {
			b.WriteString("**🛠 Changes requested**\n\n")
		}
	}
	if feedback != "" {
		b.WriteString(feedback)
		b.WriteString("\n")
	}
	if summary != "" {
		b.WriteString("\n**Summary:** ")
		b.WriteString(summary)
		b.WriteString("\n")
	}
	if items := renderRemaining(r.Remaining); items != "" {
		b.WriteString("\n**Remaining:**\n")
		b.WriteString(items)
	}
	return strings.TrimSpace(b.String()), true
}

// extractReview finds the reviewer's structured object, trying the prev
// envelope top level first (a `review` key hoisted onto it) then the last
// balanced JSON object inside the body text (reasoning prose + final JSON).
func extractReview(prev json.RawMessage, rawBody string) *reviewStructured {
	if r := reviewFromObject(prev); r != nil {
		return r
	}
	if obj := executor.ExtractLastJSONObject([]byte(rawBody)); obj != nil {
		if r := reviewFromObject(obj); r != nil {
			return r
		}
	}
	return nil
}

// reviewFromObject parses b as either {"review":{...}} or a bare {...} review
// object, returning it only when it carries usable feedback/summary text.
func reviewFromObject(b []byte) *reviewStructured {
	if len(b) == 0 {
		return nil
	}
	var wrap struct {
		Review *reviewStructured `json:"review"`
	}
	if json.Unmarshal(b, &wrap) == nil && wrap.Review != nil &&
		(strings.TrimSpace(wrap.Review.Feedback) != "" || strings.TrimSpace(wrap.Review.Summary) != "") {
		return wrap.Review
	}
	var bare reviewStructured
	if json.Unmarshal(b, &bare) == nil &&
		(strings.TrimSpace(bare.Feedback) != "" || strings.TrimSpace(bare.Summary) != "") {
		return &bare
	}
	return nil
}

// renderRemaining renders the reviewer's `remaining` items as a markdown bullet
// list. Items are usually strings; non-string entries are rendered as compact
// JSON so nothing is silently dropped.
func renderRemaining(items []any) string {
	var b strings.Builder
	for _, it := range items {
		s := ""
		if str, ok := it.(string); ok {
			s = str
		} else if raw, err := json.Marshal(it); err == nil {
			s = string(raw)
		}
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		b.WriteString("- ")
		b.WriteString(s)
		b.WriteString("\n")
	}
	return b.String()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// Execute implements executor.SystemHandler.
func (h *PostReviewHandler) Execute(ctx context.Context, in executor.SystemStepInput) (executor.SystemStepResult, error) {
	const name = "forge.post_review"
	if h == nil || h.resolver == nil {
		return executor.SystemStepResult{}, errors.New(name + ": handler is missing required dependencies (resolver)")
	}
	// Fail closed: unable to disclose means unable to post. Publishing an
	// undisclosed AI-authored comment to a human is the non-conformity; failing
	// the step is merely a broken build.
	if h.disclosure == nil {
		return executor.SystemStepResult{}, errors.New(name +
			": handler is missing required dependencies (disclosure) — refusing to post " +
			"an undisclosed AI-authored review (EU AI Act Art 50(1))")
	}
	job, err := forgeJobFromTask(in.Task, name)
	if err != nil {
		return executor.SystemStepResult{}, err
	}
	gating := in.Step != nil && in.Step.GatingReviews
	body, event := reviewBodyEvent(in.PrevResult, gating)
	if strings.TrimSpace(body) == "" {
		return executor.SystemStepResult{}, fmt.Errorf("%s: empty review body from the prior step (expected {body|result|output})", name)
	}
	provider, err := h.resolver.ForgeProvider(ctx, in.Task.ProjectID)
	if err != nil {
		return executor.SystemStepResult{}, fmt.Errorf("%s: resolve provider: %w", name, err)
	}
	// NOTHING NEW TO REVIEW → POST NOTHING (design §16).
	//
	// fetch_diff already reports scope "no-change" when the head has not moved
	// since the last review, and NOTHING consumed it: the reviewer ran with a
	// one-sentence diff, invented a review of a different pull request, and this
	// handler submitted it as a real APPROVAL. The guard belongs here, on the
	// outward-facing act, beside the disclosure refusal above.
	if head, refuse := h.alreadyReviewed(ctx, in.Task.ProjectID, job); refuse {
		out, _ := json.Marshal(map[string]any{
			"posted": false,
			"number": job.Number,
			"reason": "no new commits since the last review of " + head,
		})
		zerolog.Ctx(ctx).Info().
			Str("repo", job.Repo).Int("number", job.Number).Str("head_sha", head).
			Msg("forge.post_review: nothing new since the last review; posting nothing")
		return executor.SystemStepResult{Result: out}, nil
	}

	// AN APPROVE OVER A RED PIPELINE IS NOT SUBMITTED (design §16).
	//
	// Three approvals were posted on headmatch PR #53 in one afternoon, each
	// while a recorded run for that head had FAILED, and each dismissing the
	// failure differently: "a pre-existing environment issue", a fabricated
	// passing mypy output, and finally "in a file this diff does not touch" —
	// about the only file the diff touched. The third came AFTER the reviewer's
	// prompt was given an explicit blocker rule, and lifted its excuse from the
	// list of legitimate overrides that rule supplied.
	//
	// So this does not consult the reasoning. It reads the same durable rows
	// fetch_ci reads and refuses the submission — the pattern §16.7 of the
	// re-review design named: when a guard and a write depend on one ground
	// truth, both read the row.
	if failing, blocked := h.approvalBlockedByCI(ctx, in.Task.ProjectID, job, event); blocked {
		body = ciWithheldNotice(failing) + body
		body = withRequestQuote(body, job)
		body = withDisclosure(body, h.disclosure.PublicationNotice())
		// A COMMENT, not REQUEST_CHANGES. The prose says the change is fine;
		// posting it under a rejecting state puts approving words in a
		// rejecting envelope and a reader cannot tell which half to believe.
		if err := provider.PostComment(ctx, job.Repo, job.Number, body); err != nil {
			return executor.SystemStepResult{}, fmt.Errorf("%s: post withheld-approval comment: %w", name, err)
		}
		out, _ := json.Marshal(map[string]any{
			"posted": true, "number": job.Number, "event": "COMMENT",
			"approval_withheld": true, "ci_failed_workflow": failing,
		})
		zerolog.Ctx(ctx).Warn().
			Str("repo", job.Repo).Int("number", job.Number).Str("workflow", failing).
			Msg("forge.post_review: APPROVE withheld — CI failed on the reviewed head; posted as a comment")
		return executor.SystemStepResult{Result: out}, nil
	}

	body = withRequestQuote(body, job)
	body = withDisclosure(body, h.disclosure.PublicationNotice())
	if err := provider.PostReview(ctx, job.Repo, job.Number, forgeapi.ReviewSpec{Body: body, Event: event}); err != nil {
		return executor.SystemStepResult{}, fmt.Errorf("%s: post review: %w", name, err)
	}
	// ADVANCE THE BASELINE, AND ONLY NOW (design §6). The review is on the pull
	// request; the commits it covered are genuinely reviewed. Advancing when a
	// review merely STARTED would let a crashed or failed run mark commits as
	// reviewed that no human ever saw — silently losing coverage, which is the
	// failure this whole feature exists to prevent.
	//
	// Best-effort: the review is already published and cannot be unposted, so
	// failing the step here would report a failure for work that succeeded. The
	// cost of a missed advance is one wider review next time, which is the safe
	// direction.
	if h.reviewState != nil {
		// The head the review actually FETCHED, not the one that created the
		// task: a push absorbed before the fetch is covered by this review and
		// must be marked as such. Falls back to the job's SHA when the state is
		// unreadable, and marks nothing at all when neither is known — never a
		// guess, because a wrong baseline silently skips commits.
		head := job.HeadSHA
		if st, gerr := h.reviewState.Get(ctx, in.Task.ProjectID, job.Repo, job.Number); gerr == nil && st != nil && st.ReviewingHeadSHA != "" {
			head = st.ReviewingHeadSHA
		}
		if head == "" {
			return finishPostReview(job, event)
		}
		if err := h.reviewState.MarkReviewed(ctx, in.Task.ProjectID, job.Repo, job.Number, head, time.Now()); err != nil {
			log := zerolog.Ctx(ctx)
			log.Warn().Err(err).
				Str("repo", job.Repo).Int("number", job.Number).Str("head_sha", head).
				Msg("forge.post_review: review posted but the baseline was not advanced; the next review will be wider")
		}
	}

	return finishPostReview(job, event)
}

// finishPostReview builds the step result. Extracted so the baseline-advance
// block above can return early without duplicating it.
func finishPostReview(job *forgeapi.ForgeJob, event forgeapi.ReviewEvent) (executor.SystemStepResult, error) {
	out, _ := json.Marshal(map[string]any{"posted": true, "number": job.Number, "event": string(event)})
	return executor.SystemStepResult{Result: out}, nil
}

// maxQuotedRequestChars bounds the quoted request so a long comment cannot push
// the review itself out of view. Generous enough for a normal multi-point ask.
const maxQuotedRequestChars = 600

// withRequestQuote prefixes a review with the request it is answering.
//
// A review posted in reply to a comment is otherwise orphaned from that comment:
// the reader sees a verdict with no idea which question produced it. Worse, a
// comment can be edited or deleted afterwards — which happened on the very first
// pull request this feature reviewed, leaving three reviews and no trace of what
// two of them were asked. Quoting puts the request inside the durable artifact.
//
// Event-driven reviews get NOTHING added: a push has no request to quote, and a
// header invented for it would be noise on every single review.
func withRequestQuote(body string, job *forgeapi.ForgeJob) string {
	if job == nil || !job.OnDemand {
		return body
	}
	req := strings.TrimSpace(job.CommentBody)
	if req == "" {
		return body
	}
	if len(req) > maxQuotedRequestChars {
		// Cut on a rune boundary so a multi-byte character is never split.
		req = strings.ToValidUTF8(req[:maxQuotedRequestChars], "") + "…"
	}

	who := strings.TrimSpace(job.CommentAuthor)
	if who == "" {
		who = "A maintainer"
	} else {
		who = "@" + who
	}

	var b strings.Builder
	b.WriteString("*Requested by ")
	b.WriteString(who)
	b.WriteString(":*\n\n")
	for _, line := range strings.Split(req, "\n") {
		b.WriteString("> ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString("\n---\n\n")
	b.WriteString(body)
	return b.String()
}

// alreadyReviewed reports whether this review covers a head that has already
// been reviewed and posted about, and the head it resolved (design §16.4).
//
// It RE-DERIVES the fact from the durable row rather than reading a flag passed
// down from fetch_diff. A flag can be dropped by any workflow that omits a step
// or is written by an author who never heard of it — the same class of failure
// as the `gates:` block that silently never matched. Two readings of one row
// cannot drift.
//
// The head is resolved exactly as the MarkReviewed call below resolves it, so
// the head this compares is the head a post would have RECORDED. That is what
// stops a refusal from disagreeing with the write it is standing in for.
//
// EVERY UNCERTAINTY RESOLVES TO POSTING — the opposite of the disclosure refusal
// above, and deliberately so. There, uncertainty risks publishing an undisclosed
// comment; here it risks withholding review coverage, and a redundant review is
// cheaper than a review nobody ever gets.
func (h *PostReviewHandler) alreadyReviewed(ctx context.Context, projectID string, job *forgeapi.ForgeJob) (string, bool) {
	if h.reviewState == nil {
		return "", false // the incremental machinery is not wired at all
	}
	// Asking is consent (§7): a human who typed "review" or "full review" on an
	// unchanged head wants the answer again, and silence would look broken.
	if job.OnDemand || job.FullReview {
		return "", false
	}
	st, err := h.reviewState.Get(ctx, projectID, job.Repo, job.Number)
	if err != nil || st == nil {
		return "", false // unreadable or never reviewed → review
	}
	head := st.ReviewingHeadSHA
	if head == "" {
		head = job.HeadSHA
	}
	if head == "" || st.LastReviewedHeadSHA == "" {
		return "", false // we do not know what this covers, or nothing is reviewed yet
	}
	return head, head == st.LastReviewedHeadSHA
}

// approvalBlockedByCI reports the failing workflow, and whether an APPROVE must
// be withheld because of it (design §16.3).
//
// Deliberately narrow. It engages only on an APPROVE — REQUEST_CHANGES is
// already the blocking answer and nothing about a red pipeline makes it wrong —
// and only on a run it can NAME, because "nothing ingested" is not "CI failed".
// That is the same distinction fetch_ci draws between a blank and a pass, and
// it keeps a project recording no outcomes behaving exactly as it did before.
func (h *PostReviewHandler) approvalBlockedByCI(ctx context.Context, projectID string,
	job *forgeapi.ForgeJob, event forgeapi.ReviewEvent) (string, bool) {
	if h == nil || h.ciOutcomes == nil || h.blocksApproval == nil || !h.blocksApproval(projectID) {
		return "", false
	}
	if event != forgeapi.ReviewApprove {
		return "", false
	}
	head := job.HeadSHA
	if h.reviewState != nil {
		if st, err := h.reviewState.Get(ctx, projectID, job.Repo, job.Number); err == nil && st != nil && st.ReviewingHeadSHA != "" {
			// The head the review actually COVERS, matching what the baseline
			// advance below records. Reading a different head here than the one
			// being marked reviewed is how a guard and a write drift apart.
			head = st.ReviewingHeadSHA
		}
	}
	if head == "" {
		return "", false
	}
	outs, err := h.ciOutcomes.ListByHeadSHA(ctx, projectID, job.Repo, head)
	if err != nil {
		// Unreadable state must not block a review: the failure mode of
		// refusing on an error is a reviewing outage, which is worse than the
		// one wrong approval this prevents. Same posture as alreadyReviewed.
		zerolog.Ctx(ctx).Warn().Err(err).
			Str("repo", job.Repo).Int("number", job.Number).
			Msg("forge.post_review: CI outcomes unreadable; not gating the approval")
		return "", false
	}
	for _, o := range outs {
		if o != nil && o.Failed() {
			name := o.WorkflowPath
			if name == "" {
				name = o.WorkflowName
			}
			return name, true
		}
	}
	return "", false
}

// ciWithheldNotice leads the comment with why the approval was not submitted.
//
// On the pull request, not only in a log: "why is there no approval here" must
// be answerable by the person looking at the pull request.
func ciWithheldNotice(workflow string) string {
	return "**Approval withheld — CI is failing on this commit.**\n\n" +
		"The reviewer below concluded the change is fine, but `" +
		sanitiseWorkflowName(workflow) + "` failed for the commit under review, so " +
		"this was posted as a comment rather than submitted as an approval.\n\n---\n\n"
}

// sanitiseWorkflowName keeps a payload-supplied name inside its code span. The
// workflow name comes from the repository CI config, so whoever can push a
// branch chooses it, and a backtick would close the span and let the remainder
// render as live markdown. Same hole forgeci.sanitiseCIText closes.
func sanitiseWorkflowName(s string) string {
	s = strings.ReplaceAll(s, "`", "'")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	if strings.TrimSpace(s) == "" {
		return "a CI workflow"
	}
	return s
}
