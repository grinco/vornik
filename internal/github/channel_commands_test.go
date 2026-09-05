package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// Phase 3 of https://docs.vornik.io:
// on-demand review from a PR comment (§7).
//
// Before this, an @vornik-companion mention on a PR routed to Receiver.Receive — the
// conversational path — so it could never reach the review pipeline. The
// customer asked for exactly this: a way to re-run the review after addressing
// feedback.

// commentBody builds an issue_comment delivery. isPR, userType and assoc are
// threaded because they gate the three things §7.1 insists happen BEFORE a
// command acts: it must be on a pull request, not authored by a bot, and
// written by someone with standing in the repository.
//
// assoc is GitHub's `author_association`. It is an explicit parameter rather
// than a fixture default so no test can assert a command runs without saying
// who was allowed to run it — the 2026-09-03 audit found this ingress with no
// author gate at all, and a fixture that quietly supplied a trusted value would
// have let the same hole reopen silently.
func commentBody(body, login, userType, assoc string, isPR bool) []byte {
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
		"comment": {"id": 100, "body": "` + body + `", "author_association": "` + assoc + `", "user": {"login": "` + login + `"` + ut + `}}
	}`)
}

func dispatchComment(t *testing.T, cfg Config, body, login, userType, assoc string, isPR bool, delivery string) (*stubTaskCreator, *recordingReceiver) {
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
	ch.HandleWebhook(w, signedRequest("shhh", "issue_comment", delivery, commentBody(body, login, userType, assoc, isPR)))
	if w.Code != http.StatusOK {
		t.Fatalf("Code = %d, want 200", w.Code)
	}
	return tc, rx
}

// The headline behaviour the customer asked for.
func TestCommand_ReviewOnAPullRequest_CreatesAReviewTask(t *testing.T) {
	tc, _ := dispatchComment(t, validConfig(), "@vornik-companion review", "alice", "User", "OWNER", true, "d-cmd-review")
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
	tc, _ := dispatchComment(t, validConfig(), "@vornik-companion full review", "alice", "User", "OWNER", true, "d-cmd-full")
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
	tc, rx := dispatchComment(t, cfg, "@vornik-companion review", "mallory", "User", "OWNER", true, "d-cmd-mallory")
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
	tc, rx := dispatchComment(t, validConfig(), "@vornik-companion review", "vornik[bot]", "Bot", "OWNER", true, "d-cmd-bot")
	if got := tc.copyEvents(); len(got) != 0 {
		t.Fatalf("a bot comment triggered %d review task(s) — this is the self-trigger loop", len(got))
	}
	if len(rx.copyGot()) != 0 {
		t.Error("a bot comment reached the conversational path")
	}
}

// A command on a plain ISSUE is not a review request — there is no PR to review.
func TestCommand_OnAnIssueNotAPR_FallsBackToChat(t *testing.T) {
	tc, rx := dispatchComment(t, validConfig(), "@vornik-companion review", "alice", "User", "OWNER", false, "d-cmd-issue")
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
	tc, rx := dispatchComment(t, validConfig(), "@vornik-companion what do you think of this?", "alice", "User", "OWNER", true, "d-cmd-chat")
	if got := tc.copyEvents(); len(got) != 0 {
		t.Fatalf("an ordinary mention created %d review task(s)", len(got))
	}
	if len(rx.copyGot()) != 1 {
		t.Fatalf("conversational replies = %d, want 1 — an unrecognised mention must not vanish", len(rx.copyGot()))
	}
}

// pause / resume are state operations, not tasks.
func TestCommand_PauseAndResume_DoNotCreateTasks(t *testing.T) {
	for _, body := range []string{"@vornik-companion pause", "@vornik-companion resume"} {
		t.Run(body, func(t *testing.T) {
			tc, _ := dispatchComment(t, validConfig(), body, "alice", "User", "OWNER", true, "d-cmd-"+body)
			if got := tc.copyEvents(); len(got) != 0 {
				t.Errorf("%q created %d task(s), want 0", body, len(got))
			}
		})
	}
}

// THE DENIAL-OF-WALLET GUARD THE APP CHANNEL WAS MISSING — regression for the
// 2026-09-03 four-week audit's P1.
//
// The stranger-command fix (9ecedb2c) landed only on the GENERIC webhook
// ingress. This channel dispatches the identical command grammar and gated it
// solely on SenderAllowlist, which is documented and coded as "empty allows all
// logins (dev-mode pass-through)" — the permitted default. So on a public
// repository, before this fix, any account that could type a comment could
// start paid reviews at will or silence automatic review of any PR.
//
// validConfig() leaves SenderAllowlist empty deliberately here: that is the
// configuration the hole existed in, so the test would prove nothing against a
// closed allowlist.
func TestCommand_FromAnAuthorWithoutStanding_CreatesNoTask(t *testing.T) {
	for _, assoc := range []string{
		"NONE",
		// CONTRIBUTOR is EXCLUDED on purpose: on GitHub it means "has had a
		// pull request merged here", which describes the past, not permission
		// to spend the review budget at will.
		"CONTRIBUTOR",
		"FIRST_TIME_CONTRIBUTOR",
		"MANNEQUIN",
		// Absent or unrecognised fails CLOSED — an unfamiliar payload shape
		// must not read as standing.
		"",
		"SOMETHING_GITHUB_ADDED_LATER",
	} {
		t.Run(assoc, func(t *testing.T) {
			cfg := validConfig()
			if len(cfg.SenderAllowlist) != 0 {
				t.Fatalf("precondition: SenderAllowlist = %v, want empty — this test asserts the gate that "+
					"holds when the allowlist does not", cfg.SenderAllowlist)
			}
			tc, _ := dispatchComment(t, cfg, "@vornik-companion review", "mallory", "User", assoc, true, "d-cmd-untrusted-"+assoc)
			if got := tc.copyEvents(); len(got) != 0 {
				t.Fatalf("author_association=%q triggered %d review task(s) — a stranger can spend the review budget", assoc, len(got))
			}
		})
	}
}

// Same gate on pause/resume, which never reach the shared coordinator at all:
// they are state writes, not task creations. An ungated `pause` lets a stranger
// silence automatic review of any pull request.
func TestCommand_PauseFromAnAuthorWithoutStanding_DoesNotPause(t *testing.T) {
	for _, body := range []string{"@vornik-companion pause", "@vornik-companion resume"} {
		t.Run(body, func(t *testing.T) {
			cfg := validConfig()
			tc := &pausingTaskCreator{}
			cfg.TaskCreator = tc
			ch, err := New(cfg)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			w := httptest.NewRecorder()
			ch.HandleWebhook(w, signedRequest("shhh", "issue_comment", "d-pause-untrusted-"+body,
				commentBody(body, "mallory", "User", "NONE", true)))
			if w.Code != http.StatusOK {
				t.Fatalf("Code = %d, want 200", w.Code)
			}
			if n := tc.pauseCalls(); n != 0 {
				t.Fatalf("%q from an author without standing changed the pause state %d time(s)", body, n)
			}
		})
	}
}

// The trusted set, asserted positively so the gate cannot be "fixed" by
// refusing everyone.
func TestCommand_FromAnAuthorWithStanding_CreatesTheTask(t *testing.T) {
	for _, assoc := range []string{"OWNER", "MEMBER", "COLLABORATOR", "collaborator", " Member "} {
		t.Run(assoc, func(t *testing.T) {
			tc, _ := dispatchComment(t, validConfig(), "@vornik-companion review", "alice", "User", assoc, true, "d-cmd-trusted-"+assoc)
			if got := tc.copyEvents(); len(got) != 1 {
				t.Fatalf("author_association=%q produced %d review task(s), want 1", assoc, len(got))
			}
		})
	}
}

// The association must REACH the task creator, not merely be checked here: the
// shared coordinator is the floor under both ingresses and it reads
// ForgeJob.AuthorIsTrusted, which the service container derives from this field.
// Carrying it no further would leave the floor with nothing to stand on.
func TestCommand_CarriesAuthorAssociationOntoTheEvent(t *testing.T) {
	tc, _ := dispatchComment(t, validConfig(), "@vornik-companion review", "alice", "User", "MEMBER", true, "d-cmd-assoc")
	got := tc.copyEvents()
	if len(got) != 1 {
		t.Fatalf("events = %d, want 1", len(got))
	}
	if got[0].AuthorAssociation != "MEMBER" {
		t.Errorf("AuthorAssociation = %q, want MEMBER — the coordinator cannot gate what it is not told", got[0].AuthorAssociation)
	}
}

// THE RENAMED BOT — the audit's secondary gap on this ingress. forge.MentionHandle
// was wired only into the generic path's provider; mentionsHandle and
// ParseCommandFor here were hardcoded to forgereview.DefaultHandle, so a
// deployment that renamed its bot got commands on one ingress and silence on
// the other.
func TestCommand_UsesTheConfiguredMentionHandle(t *testing.T) {
	cfg := validConfig()
	cfg.MentionHandle = "acme-reviewer"

	tc, _ := dispatchComment(t, cfg, "@acme-reviewer review", "alice", "User", "OWNER", true, "d-cmd-handle")
	if got := tc.copyEvents(); len(got) != 1 {
		t.Fatalf("the configured handle produced %d review task(s), want 1 — a renamed bot never matches", len(got))
	}

	// And the default no longer matches once a handle is configured: two live
	// handles would mean a deployment answers to a name its operator did not
	// choose, which is the bug the configurability exists to fix.
	tc2, rx := dispatchComment(t, cfg, "@vornik-companion review", "alice", "User", "OWNER", true, "d-cmd-handle-default")
	if got := tc2.copyEvents(); len(got) != 0 {
		t.Fatalf("the default handle still created %d review task(s) after a handle was configured", len(got))
	}
	if len(rx.copyGot()) != 0 {
		t.Error("a mention of the unconfigured default reached the conversational path")
	}
}

// An unset handle keeps today's behaviour — the App's own slug — matching the
// generic ingress's provider fallback exactly.
func TestCommand_UnsetMentionHandle_FallsBackToTheDefault(t *testing.T) {
	cfg := validConfig()
	if cfg.MentionHandle != "" {
		t.Fatalf("precondition: MentionHandle = %q, want unset", cfg.MentionHandle)
	}
	tc, _ := dispatchComment(t, cfg, "@vornik-companion review", "alice", "User", "OWNER", true, "d-cmd-handle-unset")
	if got := tc.copyEvents(); len(got) != 1 {
		t.Fatalf("the default handle produced %d review task(s), want 1", len(got))
	}
}

// pausingTaskCreator is a TaskCreator that ALSO implements ReviewController, the
// shape the channel type-asserts for when handling pause / resume.
type pausingTaskCreator struct {
	stubTaskCreator
	mu     sync.Mutex
	paused int
}

func (p *pausingTaskCreator) SetAutoReviewPaused(_ context.Context, _ string, _ int, _ bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.paused++
	return nil
}

func (p *pausingTaskCreator) pauseCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.paused
}
