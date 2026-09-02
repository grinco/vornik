package github

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Phase 3 of https://docs.vornik.io:
// on-demand review from a PR comment (§7).
//
// Before this, an @vornik mention on a PR routed to Receiver.Receive — the
// conversational path — so it could never reach the review pipeline. The
// customer asked for exactly this: a way to re-run the review after addressing
// feedback.

// commentBody builds an issue_comment delivery. isPR and userType are threaded
// because they gate the two things §7.1 insists happen BEFORE parsing.
func commentBody(body, login, userType string, isPR bool) []byte {
	pr := ""
	if isPR {
		pr = `, "pull_request": {"url": "https://api.github.com/repos/acme/api/pulls/12"}`
	}
	ut := ""
	if userType != "" {
		ut = `, "type": "` + userType + `"`
	}
	return []byte(`{
		"action": "created",
		"repository": {"full_name": "acme/api"},
		"sender": {"login": "` + login + `"},
		"installation": {"id": 9001},
		"issue": {"number": 12, "title": "a pull request"` + pr + `},
		"comment": {"id": 100, "body": "` + body + `", "user": {"login": "` + login + `"` + ut + `}}
	}`)
}

func dispatchComment(t *testing.T, cfg Config, body, login, userType string, isPR bool, delivery string) (*stubTaskCreator, *recordingReceiver) {
	t.Helper()
	tc := &stubTaskCreator{}
	cfg.TaskCreator = tc
	ch, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rx := &recordingReceiver{}
	ch.recvMu.Lock()
	ch.recv = rx
	ch.recvMu.Unlock()

	w := httptest.NewRecorder()
	ch.HandleWebhook(w, signedRequest("shhh", "issue_comment", delivery, commentBody(body, login, userType, isPR)))
	if w.Code != http.StatusOK {
		t.Fatalf("Code = %d, want 200", w.Code)
	}
	return tc, rx
}

// The headline behaviour the customer asked for.
func TestCommand_ReviewOnAPullRequest_CreatesAReviewTask(t *testing.T) {
	tc, _ := dispatchComment(t, validConfig(), "@vornik review", "alice", "User", true, "d-cmd-review")
	got := tc.copyEvents()
	if len(got) != 1 {
		t.Fatalf("TaskCreator saw %d events, want 1 — the command did not reach the review pipeline", len(got))
	}
	if got[0].Kind != "pull_request.comment_command" {
		t.Errorf("Kind = %q, want pull_request.comment_command", got[0].Kind)
	}
	if !got[0].OnDemand {
		t.Error("OnDemand = false; a command must not be coalesced away")
	}
	if got[0].Number != 12 {
		t.Errorf("Number = %d, want 12", got[0].Number)
	}
}

// "full review" must be distinguishable, because phase 4 uses it to ignore the
// incremental baseline.
func TestCommand_FullReview_SetsTheFullFlag(t *testing.T) {
	tc, _ := dispatchComment(t, validConfig(), "@vornik full review", "alice", "User", true, "d-cmd-full")
	got := tc.copyEvents()
	if len(got) != 1 {
		t.Fatalf("events = %d, want 1", len(got))
	}
	if !got[0].FullReview {
		t.Error("FullReview = false for an explicit full review request")
	}
}

// THE DENIAL-OF-WALLET GUARD. On a public repo a review is real model spend, so
// a command from someone not on the allowlist must not run one.
func TestCommand_FromNonAllowlistedSender_CreatesNoTask(t *testing.T) {
	cfg := validConfig()
	cfg.SenderAllowlist = []string{"alice"} // mallory is not on it
	tc, rx := dispatchComment(t, cfg, "@vornik review", "mallory", "User", true, "d-cmd-mallory")
	if got := tc.copyEvents(); len(got) != 0 {
		t.Fatalf("a non-allowlisted sender triggered %d review task(s)", len(got))
	}
	if len(rx.copyGot()) != 0 {
		t.Error("a non-allowlisted sender also reached the conversational path")
	}
}

// THE LOOP GUARD. The review this system posts is itself a PR comment. If a
// bot-authored comment could match a command, the system triggers itself.
func TestCommand_FromABot_IsIgnoredBeforeParsing(t *testing.T) {
	tc, rx := dispatchComment(t, validConfig(), "@vornik review", "vornik[bot]", "Bot", true, "d-cmd-bot")
	if got := tc.copyEvents(); len(got) != 0 {
		t.Fatalf("a bot comment triggered %d review task(s) — this is the self-trigger loop", len(got))
	}
	if len(rx.copyGot()) != 0 {
		t.Error("a bot comment reached the conversational path")
	}
}

// A command on a plain ISSUE is not a review request — there is no PR to review.
func TestCommand_OnAnIssueNotAPR_FallsBackToChat(t *testing.T) {
	tc, rx := dispatchComment(t, validConfig(), "@vornik review", "alice", "User", false, "d-cmd-issue")
	if got := tc.copyEvents(); len(got) != 0 {
		t.Fatalf("a command on a plain issue created %d review task(s)", len(got))
	}
	if len(rx.copyGot()) != 1 {
		t.Errorf("conversational replies = %d, want 1 — a mention on an issue must still get an answer", len(rx.copyGot()))
	}
}

// A mention that matches NO command keeps today's behaviour exactly. This design
// adds a command path in front of the chat path; it does not replace it, so an
// unrecognised phrasing degrades to "a human gets an answer", never to silence.
func TestCommand_UnrecognisedMention_StillReachesTheReceiver(t *testing.T) {
	tc, rx := dispatchComment(t, validConfig(), "@vornik what do you think of this?", "alice", "User", true, "d-cmd-chat")
	if got := tc.copyEvents(); len(got) != 0 {
		t.Fatalf("an ordinary mention created %d review task(s)", len(got))
	}
	if len(rx.copyGot()) != 1 {
		t.Fatalf("conversational replies = %d, want 1 — an unrecognised mention must not vanish", len(rx.copyGot()))
	}
}

// pause / resume are state operations, not tasks.
func TestCommand_PauseAndResume_DoNotCreateTasks(t *testing.T) {
	for _, body := range []string{"@vornik pause", "@vornik resume"} {
		t.Run(body, func(t *testing.T) {
			tc, _ := dispatchComment(t, validConfig(), body, "alice", "User", true, "d-cmd-"+body)
			if got := tc.copyEvents(); len(got) != 0 {
				t.Errorf("%q created %d task(s), want 0", body, len(got))
			}
		})
	}
}
