package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"vornik.io/vornik/internal/executor"
	forgeapi "vornik.io/vornik/internal/forge"
)

// PostReviewHandler implements the "forge.post_review" system step: post the
// reviewer agent's prose against the change request as a review/note. The body
// is the LLM's text; the posting itself is deterministic.
type PostReviewHandler struct {
	resolver ProviderResolver
}

// NewPostReviewHandler wires the handler.
func NewPostReviewHandler(resolver ProviderResolver) *PostReviewHandler {
	return &PostReviewHandler{resolver: resolver}
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
	if err := provider.PostReview(ctx, job.Repo, job.Number, forgeapi.ReviewSpec{Body: body, Event: event}); err != nil {
		return executor.SystemStepResult{}, fmt.Errorf("%s: post review: %w", name, err)
	}
	out, _ := json.Marshal(map[string]any{"posted": true, "number": job.Number, "event": string(event)})
	return executor.SystemStepResult{Result: out}, nil
}
