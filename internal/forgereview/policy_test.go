package forgereview

import (
	"context"
	"testing"

	"github.com/rs/zerolog"

	forgeapi "vornik.io/vornik/internal/forge"
)

// The two review-trigger knobs — the push off-switch and the draft opt-in —
// live here, on the shared coordinator, because they are decisions about
// REVIEWING and not about one door into the building.
//
// They shipped on the GitHub App channel in 2026.9.1 and reached only that
// ingress. The generic `webhooks.sources` relay — the ingress this deployment
// actually receives deliveries through — had neither: an operator who wanted
// "review on open, never on push" had the per-PR `pause` command, re-issued by
// hand on every new pull request, or a source event filter that also drops the
// deliveries carrying comment commands. BACKLOG 2026-09-03.
//
// Design: https://docs.vornik.io
// §13.4.

func policyCoord(p Policy) *Coordinator {
	c := New(nil, nil, zerolog.Nop())
	c.SetPolicyResolver(func(string) Policy { return p })
	return c
}

func pushJob() forgeapi.ForgeJob {
	return forgeapi.ForgeJob{
		Provider: forgeapi.ProviderGitHub, Repo: "acme/api", Number: 12,
		Action: "synchronize", IsChangeRequest: true, HeadSHA: "sha-new",
	}
}

func TestPolicy_PushSuppressedWhenAutoReviewOnPushIsOff(t *testing.T) {
	d := policyCoord(Policy{AutoReviewOnPush: false}).
		Decide(context.Background(), "p1", pushJob(), false)
	if !d.Skip {
		t.Fatal("auto_review_on_push=false must suppress a synchronize delivery")
	}
	if d.Reason != "auto_review_on_push_disabled" {
		t.Errorf("Reason = %q, want auto_review_on_push_disabled", d.Reason)
	}
}

// The off-switch must never take FIRST review with it. That is the whole
// reason it is scoped to the push trigger: someone who wants quiet pushes has
// not asked to stop reviewing pull requests.
func TestPolicy_FirstReviewSurvivesThePushOffSwitch(t *testing.T) {
	for _, action := range []string{"opened", "reopened", "ready_for_review"} {
		job := pushJob()
		job.Action = action
		d := policyCoord(Policy{AutoReviewOnPush: false}).
			Decide(context.Background(), "p1", job, false)
		if d.Skip {
			t.Errorf("%s suppressed by the PUSH off-switch (reason %q)", action, d.Reason)
		}
	}
}

// A human asking is never suppressed — the same exemption pause has, for the
// same reason: an operator must be able to get a review out of a pull request
// they have configured quiet.
func TestPolicy_OnDemandIgnoresBothKnobs(t *testing.T) {
	job := pushJob()
	job.Action = "created"
	job.OnDemand = true
	job.Command = "review"
	job.AuthorIsTrusted = true
	job.IsDraft = true

	d := policyCoord(Policy{AutoReviewOnPush: false, ReviewDraftPRs: false}).
		Decide(context.Background(), "p1", job, true)
	if d.Skip {
		t.Fatalf("an explicit request must survive both knobs, got %q", d.Reason)
	}
}

func TestPolicy_DraftSuppressedByDefault(t *testing.T) {
	job := pushJob()
	job.Action = "opened"
	job.IsDraft = true
	d := policyCoord(Policy{AutoReviewOnPush: true, ReviewDraftPRs: false}).
		Decide(context.Background(), "p1", job, false)
	if !d.Skip || d.Reason != "draft_not_reviewed" {
		t.Fatalf("a draft must be suppressed by default, got Skip=%v reason=%q", d.Skip, d.Reason)
	}
}

func TestPolicy_DraftReviewedWhenOptedIn(t *testing.T) {
	job := pushJob()
	job.Action = "opened"
	job.IsDraft = true
	d := policyCoord(Policy{AutoReviewOnPush: true, ReviewDraftPRs: true}).
		Decide(context.Background(), "p1", job, false)
	if d.Skip {
		t.Fatalf("review_draft_prs=true must review a draft, got %q", d.Reason)
	}
}

// ready_for_review ENDS draft state, and GitHub still reports draft:true on
// some deliveries of it. Suppressing there would swallow the one transition
// the whole draft rule exists to wait for.
func TestPolicy_ReadyForReviewIsExemptFromTheDraftGate(t *testing.T) {
	job := pushJob()
	job.Action = "ready_for_review"
	job.IsDraft = true
	d := policyCoord(Policy{AutoReviewOnPush: true, ReviewDraftPRs: false}).
		Decide(context.Background(), "p1", job, false)
	if d.Skip {
		t.Fatalf("ready_for_review must not be suppressed as a draft, got %q", d.Reason)
	}
}

// A coordinator with no resolver — a DMZ relay node, early boot, a test —
// applies the DEFAULTS rather than nothing: push reviews on, drafts off. The
// alternative is a node that reviews every draft it sees because it could not
// look up a setting, which spends budget on the failure.
func TestPolicy_UnresolvedFallsBackToTheDefaults(t *testing.T) {
	c := New(nil, nil, zerolog.Nop())
	job := pushJob()
	job.Action = "opened"
	job.IsDraft = true
	if d := c.Decide(context.Background(), "p1", job, false); !d.Skip {
		t.Error("no resolver must still suppress a draft (defaults, not nothing)")
	}
	if d := c.Decide(context.Background(), "p1", pushJob(), false); d.Skip {
		t.Errorf("no resolver must still review a push (defaults, not nothing), got %q", d.Reason)
	}
}

// The gate must survive a nil *Coordinator, because the generic ingress calls
// Decide on a deployment that may have no review state at all, and a nil
// receiver there previously meant "enqueue everything".
func TestPolicy_NilCoordinatorStillAppliesTheDefaults(t *testing.T) {
	var c *Coordinator
	job := pushJob()
	job.Action = "opened"
	job.IsDraft = true
	if d := c.Decide(context.Background(), "p1", job, false); !d.Skip {
		t.Error("a nil coordinator must still suppress a draft")
	}
}
