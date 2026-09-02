package github

import (
	"net/http"
	"testing"

	forgeapi "vornik.io/vornik/internal/forge"
)

// §13.3: the generic webhook path types events by CLASSIFYING THE BODY, because
// the DMZ relay envelope carries no HTTP headers. Every re-review trigger has to
// be recognised here or it never reaches the daemon at all — which is how phases
// 1-4 came to work only on the channel.

func classify(t *testing.T, body string) (forgeapi.ForgeJob, bool) {
	t.Helper()
	// Empty header on purpose: this is the relay-forwarded shape.
	return (&Provider{}).ClassifyEvent(http.Header{}, []byte(body))
}

func prPayload(action string, draft bool) string {
	d := "false"
	if draft {
		d = "true"
	}
	return `{"action":"` + action + `","pull_request":{"number":12,"title":"t","body":"b","draft":` + d +
		`,"head":{"sha":"sha-head"}},"repository":{"full_name":"acme/api","default_branch":"main"}}`
}

// synchronize is the whole point of the feature and was NOT classified.
func TestClassify_PullRequestActions(t *testing.T) {
	for _, tc := range []struct {
		action string
		want   bool
	}{
		{"opened", true},
		{"reopened", true},
		{"ready_for_review", true},
		{"synchronize", true},
		{"closed", false},
		{"labeled", false},
		{"assigned", false},
	} {
		t.Run(tc.action, func(t *testing.T) {
			job, ok := classify(t, prPayload(tc.action, false))
			if ok != tc.want {
				t.Fatalf("ok = %v for %s, want %v", ok, tc.action, tc.want)
			}
			if !ok {
				return
			}
			if !job.IsChangeRequest {
				t.Error("IsChangeRequest = false for a pull request")
			}
			if job.Action != tc.action {
				t.Errorf("Action = %q, want %q", job.Action, tc.action)
			}
			// Without this the incremental range has no upper bound and every
			// review silently falls back to the full diff.
			if job.HeadSHA != "sha-head" {
				t.Errorf("HeadSHA = %q, want sha-head", job.HeadSHA)
			}
		})
	}
}

// A draft is work in progress; ready_for_review is the transition that starts
// review, and it must not be suppressed by the draft flag it clears.
func TestClassify_DraftPullRequest(t *testing.T) {
	if _, ok := classify(t, prPayload("synchronize", true)); ok {
		t.Error("a draft PR was classified as actionable on synchronize")
	}
	if _, ok := classify(t, prPayload("opened", true)); ok {
		t.Error("a draft PR was classified as actionable on opened")
	}
	job, ok := classify(t, prPayload("ready_for_review", true))
	if !ok {
		t.Fatal("ready_for_review was suppressed by the draft flag it ends")
	}
	// The exempted path must still carry a head, or the review it starts has no
	// upper bound for its range (review round 1 of phase A, minor 3).
	if job.HeadSHA != "sha-head" {
		t.Errorf("HeadSHA = %q on the draft-exempt path, want sha-head", job.HeadSHA)
	}
}

func commentPayload(body, userType string, isPR bool) string {
	pr := ""
	if isPR {
		pr = `,"pull_request":{"url":"https://api.github.com/repos/acme/api/pulls/12"}`
	}
	return `{"action":"created","issue":{"number":12,"title":"t"` + pr + `},` +
		`"comment":{"body":"` + body + `","user":{"login":"alice","type":"` + userType + `"}},` +
		`"repository":{"full_name":"acme/api","default_branch":"main"}}`
}

// The comment arm did not exist at all, so a command could never reach the
// review pipeline on this path.
func TestClassify_CommentCommand(t *testing.T) {
	job, ok := classify(t, commentPayload("@vornik review", "User", true))
	if !ok {
		t.Fatal("a review command on a PR was not classified")
	}
	if !job.IsChangeRequest || job.Number != 12 {
		t.Errorf("job = %+v, want a change request on #12", job)
	}
	if !job.OnDemand {
		t.Error("OnDemand = false; an explicit request would be coalesced away")
	}
	if job.AuthorIsBot {
		t.Error("AuthorIsBot = true for a human author")
	}
}

func TestClassify_FullReviewCommand(t *testing.T) {
	job, ok := classify(t, commentPayload("@vornik full review", "User", true))
	if !ok {
		t.Fatal("full review was not classified")
	}
	if !job.FullReview {
		t.Error("FullReview = false for an explicit full review")
	}
}

// THE LOOP GUARD, provider-side. Our own review is posted as a comment; if a
// bot-authored comment classified as a command, a review would trigger another.
func TestClassify_BotComment_FlaggedNotActedOn(t *testing.T) {
	job, ok := classify(t, commentPayload("@vornik review", "Bot", true))
	if ok && !job.AuthorIsBot {
		t.Fatal("a bot-authored command was classified without the bot flag — the self-trigger loop is open")
	}
}

// A comment that is not a command, and a command on a plain issue, are both
// non-events for the review pipeline.
func TestClassify_NonCommandAndIssueComments(t *testing.T) {
	if _, ok := classify(t, commentPayload("@vornik what do you think?", "User", true)); ok {
		t.Error("an ordinary mention classified as a forge job")
	}
	if _, ok := classify(t, commentPayload("@vornik review", "User", false)); ok {
		t.Error("a review command on a plain ISSUE classified as a change request")
	}
}

// DENIAL-OF-WALLET GUARD, provider side. A review is real model spend, and on a
// PUBLIC repository anyone can comment. GitHub stamps every comment with the
// author's association to the repo, which is exactly the "closed set of trusted
// users" §7.1 requires — and it needs no operator-maintained list and no extra
// API call.
func commentPayloadAssoc(body, userType, assoc string) string {
	return `{"action":"created","issue":{"number":12,"title":"t",` +
		`"pull_request":{"url":"https://api.github.com/repos/acme/api/pulls/12"}},` +
		`"comment":{"body":"` + body + `","author_association":"` + assoc + `",` +
		`"user":{"login":"someone","type":"` + userType + `"}},` +
		`"repository":{"full_name":"acme/api","default_branch":"main"}}`
}

func TestClassify_CommandAuthorTrust(t *testing.T) {
	for _, tc := range []struct {
		assoc     string
		wantTrust bool
	}{
		{"OWNER", true},
		{"MEMBER", true},
		{"COLLABORATOR", true},
		// A drive-by contributor or a total stranger must NOT be able to spend
		// the budget. CONTRIBUTOR means "has had a PR merged", which is not the
		// same as "may run reviews at will".
		{"CONTRIBUTOR", false},
		{"FIRST_TIME_CONTRIBUTOR", false},
		{"NONE", false},
		{"", false},
	} {
		t.Run(tc.assoc, func(t *testing.T) {
			job, ok := classify(t, commentPayloadAssoc("@vornik review", "User", tc.assoc))
			if !ok {
				t.Fatal("the command was not classified at all")
			}
			if job.AuthorIsTrusted != tc.wantTrust {
				t.Errorf("AuthorIsTrusted = %v for %q, want %v", job.AuthorIsTrusted, tc.assoc, tc.wantTrust)
			}
		})
	}
}
