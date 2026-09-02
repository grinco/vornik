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

// A draft PR is explicitly "not ready to be looked at". Auto-reviewing it burns
// budget on work in progress, and ready_for_review is the transition that starts
// review — which is what forge.md already promises.
func TestChannel_DraftPR_NotAutoReviewed(t *testing.T) {
	for _, action := range []string{"opened", "synchronize", "reopened"} {
		t.Run(action, func(t *testing.T) {
			creator := dispatchPR(t, validConfig(), action, "d-draft-"+action, true)
			if got := creator.copyEvents(); len(got) != 0 {
				t.Errorf("draft PR produced %d task(s) on %s, want 0", len(got), action)
			}
		})
	}
}

// ready_for_review is the one action that fires ON a PR leaving draft state, so
// the draft flag in its payload must not suppress it.
func TestChannel_ReadyForReview_FiresEvenWhenPayloadSaysDraft(t *testing.T) {
	creator := dispatchPR(t, validConfig(), "ready_for_review", "d-rfr-draft", true)
	if got := creator.copyEvents(); len(got) != 1 {
		t.Fatalf("ready_for_review produced %d task(s), want 1 — the draft gate must not swallow the transition OUT of draft", len(got))
	}
}

// The off switch. auto_review_on_push=false stops the push trigger and NOTHING
// else: opened must still review, so an operator who wants quiet pushes does not
// silently lose first-review too.
func TestChannel_AutoReviewOnPushDisabled_OnlySuppressesSynchronize(t *testing.T) {
	cfg := validConfig()
	off := false
	cfg.AutoReviewOnPush = &off // validConfig() is single-installation mode

	if got := dispatchPR(t, cfg, "synchronize", "d-off-sync", false).copyEvents(); len(got) != 0 {
		t.Errorf("synchronize produced %d task(s) with auto_review_on_push=false, want 0", len(got))
	}
	if got := dispatchPR(t, cfg, "opened", "d-off-open", false).copyEvents(); len(got) != 1 {
		t.Errorf("opened produced %d task(s) with auto_review_on_push=false, want 1 — the switch is push-only", len(got))
	}
}

// Unset must mean ON: it is the behaviour the customer asked for and the one
// forge.md already documents.
func TestChannel_AutoReviewOnPush_DefaultsOn(t *testing.T) {
	if got := dispatchPR(t, validConfig(), "synchronize", "d-default-sync", false).copyEvents(); len(got) != 1 {
		t.Fatalf("synchronize produced %d task(s) with auto_review_on_push unset, want 1 (default on)", len(got))
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
