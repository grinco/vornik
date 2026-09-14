package github

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Phase 1 of https://docs.vornik.io:
// trigger parity. Before this, HandleWebhook dispatched a review on
// pull_request.opened ONLY — synchronize, reopened and ready_for_review all fell
// through to the default arm and were acked + discarded, so a PR could never be
// re-reviewed after its first review. docs/public/features/forge.md already
// claimed "opened, reopened, or marked ready for review", so the code was also
// behind its own published contract.

// prBody builds a pull_request delivery. draft is threaded because the draft
// gate is the difference between reviewing work in progress and not.
func prBody(action string, draft bool) []byte {
	d := "false"
	if draft {
		d = "true"
	}
	return []byte(`{
		"action": "` + action + `",
		"repository": {"full_name": "acme/api"},
		"sender": {"login": "vadim"},
		"installation": {"id": 9001},
		"pull_request": {
			"number": 12, "title": "PR title", "body": "PR body",
			"draft": ` + d + `,
			"head": {"sha": "abc123def4567890abc123def4567890abc12345"},
			"labels": [{"name": "needs-review"}]
		}
	}`)
}

func dispatchPR(t *testing.T, cfg Config, action, delivery string, draft bool) *stubTaskCreator {
	t.Helper()
	tc := &stubTaskCreator{}
	cfg.TaskCreator = tc
	ch, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w := httptest.NewRecorder()
	ch.HandleWebhook(w, signedRequest("shhh", "pull_request", delivery, prBody(action, draft)))
	if w.Code != http.StatusOK {
		t.Fatalf("Code = %d, want 200", w.Code)
	}
	return tc
}

// The three actions that were silently dropped. Each must reach TaskCreator with
// its OWN kind — not remapped onto pull_request.opened, because the task creator
// routes and the reviewer prompt both read the kind.
func TestChannel_ReReviewTriggers_FireTaskCreator(t *testing.T) {
	for _, tc := range []struct {
		action   string
		wantKind string
	}{
		{"reopened", "pull_request.reopened"},
		{"ready_for_review", "pull_request.ready_for_review"},
		{"synchronize", "pull_request.synchronize"},
	} {
		t.Run(tc.action, func(t *testing.T) {
			creator := dispatchPR(t, validConfig(), tc.action, "d-"+tc.action, false)
			got := creator.copyEvents()
			if len(got) != 1 {
				t.Fatalf("TaskCreator saw %d events, want 1 — %s is still being dropped", len(got), tc.action)
			}
			if got[0].Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q", got[0].Kind, tc.wantKind)
			}
			if got[0].SessionID != "acme/api#pulls/12" {
				t.Errorf("SessionID = %q, want acme/api#pulls/12", got[0].SessionID)
			}
		})
	}
}

// THE DRAFT RULE AND THE PUSH OFF-SWITCH MOVED OFF THIS CHANNEL on 2026-09-13.
//
// Three tests here asserted them at this layer — TestChannel_DraftPR_NotAutoReviewed,
// TestChannel_AutoReviewOnPushDisabled_OnlySuppressesSynchronize and
// TestChannel_AutoReviewOnPush_DefaultsOn — against per-INSTALLATION config
// fields. That location was the defect: the generic `webhooks.sources` relay,
// the ingress this deployment actually receives deliveries through, had no
// project-wide push off switch at all and could not opt into draft review
// (BACKLOG 2026-09-03). Both rules now live in internal/forgereview, which BOTH
// ingresses call, and TestPolicy_* there own the behaviour.
//
// What this channel still owns is DELIVERY of the facts the rule reads. These
// two tests pin that: every review action reaches the task creator, and the
// draft flag reaches it intact — a gate cannot apply a fact it is never told.
func TestChannel_DraftPR_ReachesTheTaskCreatorCarryingItsDraftFlag(t *testing.T) {
	for _, action := range []string{"opened", "synchronize", "reopened", "ready_for_review"} {
		t.Run(action, func(t *testing.T) {
			creator := dispatchPR(t, validConfig(), action, "d-draft-"+action, true)
			got := creator.copyEvents()
			if len(got) != 1 {
				t.Fatalf("draft PR produced %d task(s) on %s, want 1 — suppression is the policy's call now", len(got), action)
			}
			if !got[0].Draft {
				t.Errorf("Draft = false on a draft %s delivery; the shared policy gate cannot suppress what it is not told", action)
			}
		})
	}
}

// A non-draft delivery must not arrive claiming to be one, or the shared gate
// would suppress ordinary pull requests.
func TestChannel_NonDraftPR_CarriesNoDraftFlag(t *testing.T) {
	got := dispatchPR(t, validConfig(), "synchronize", "d-nondraft-sync", false).copyEvents()
	if len(got) != 1 {
		t.Fatalf("synchronize produced %d task(s), want 1", len(got))
	}
	if got[0].Draft {
		t.Error("Draft = true on a non-draft delivery")
	}
}

// The complement of the trigger set: pull_request carries many actions that are
// NOT review triggers (closed, labeled, assigned, edited...). They must remain
// acked-and-dropped. Without this the trigger set could quietly widen to "any
// pull_request action" and nobody would notice until the review bill did.
func TestChannel_NonTriggerPullRequestActions_CreateNoTask(t *testing.T) {
	for _, action := range []string{"closed", "labeled", "assigned", "edited", "locked"} {
		t.Run(action, func(t *testing.T) {
			creator := dispatchPR(t, validConfig(), action, "d-nontrigger-"+action, false)
			if got := creator.copyEvents(); len(got) != 0 {
				t.Errorf("pull_request.%s produced %d task(s), want 0", action, len(got))
			}
		})
	}
}
