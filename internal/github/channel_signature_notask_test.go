package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// This file closes the security-critical half of the inbound
// GitHub-App webhook journey: an UNAUTHENTICATED delivery must never
// reach the TaskCreator.
//
// The existing signature-rejection tests
// (TestHandleWebhook_RejectsBadSignature / RejectsMissingSignature /
// RejectsMalformedSignature in channel_test.go) assert only the
// HTTP 401 status. They do NOT wire a TaskCreator, so they cannot
// prove that no task was created — a regression that reordered
// verification after dispatch, or that 401'd the response while
// still firing the creator, would pass them.
//
// These tests wire a stubTaskCreator and use the EXACT payloads that
// the positive-path tests (TestChannel_IssuesLabeled_FiresTaskCreator
// / TestChannel_PullRequestOpened_FiresTaskCreator) prove DO create a
// task when correctly signed. The only difference is the signature.
// The wired assertion is: bad/missing/malformed signature => 401 AND
// zero TaskCreator.Create calls.
//
// Characterization: HandleWebhook verifies the HMAC before parsing
// the body or branching on the event, so these must PASS. A failure
// (a forged-signature webhook creating a task) would be a real
// signature-bypass security bug.

// labeledTaskBody is the issues.labeled payload that
// TestChannel_IssuesLabeled_FiresTaskCreator proves fires the
// TaskCreator when signed with the configured secret ("shhh").
var labeledTaskBody = []byte(`{
	"action": "labeled",
	"repository": {"full_name": "acme/api"},
	"sender": {"login": "vadim"},
	"installation": {"id": 9000},
	"issue": {"number": 5, "title": "bug", "body": "details", "labels": [{"name": "vornik-task"}]},
	"label": {"name": "vornik-task"}
}`)

// prOpenedTaskBody is the pull_request.opened payload that
// TestChannel_PullRequestOpened_FiresTaskCreator proves fires the
// TaskCreator when correctly signed.
var prOpenedTaskBody = []byte(`{
	"action": "opened",
	"repository": {"full_name": "acme/api"},
	"sender": {"login": "vadim"},
	"installation": {"id": 9001},
	"pull_request": {"number": 12, "title": "PR title", "body": "PR body", "labels": [{"name": "needs-review"}]}
}`)

// configWithCreator returns a valid config wired to a fresh
// stubTaskCreator, returning both so the test can assert on it.
func configWithCreator() (Config, *stubTaskCreator) {
	cfg := validConfig()
	tc := &stubTaskCreator{}
	cfg.TaskCreator = tc
	return cfg, tc
}

// postUnauthenticated builds a POST request to the webhook endpoint
// with the given (untrusted) signature header. An empty sig string
// omits the header entirely.
func postUnauthenticated(event, sig string, body []byte) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/github-app/webhook", strings.NewReader(string(body)))
	r.Header.Set("X-GitHub-Event", event)
	r.Header.Set("X-GitHub-Delivery", "d-forged")
	if sig != "" {
		r.Header.Set("X-Hub-Signature-256", sig)
	}
	return r
}

// signedWith computes a valid-looking sha256= header but using the
// WRONG secret — i.e. a structurally correct hex HMAC that simply
// doesn't match. This exercises the hmac.Equal mismatch branch
// (distinct from "deadbeef", which is short, and from non-hex junk).
func signedWith(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// TestSecurity_BadSignature_IssuesLabeled_CreatesNoTask — a payload
// that WOULD fire the TaskCreator if signed correctly must create no
// task when carrying a forged (wrong-secret) signature.
func TestSecurity_BadSignature_IssuesLabeled_CreatesNoTask(t *testing.T) {
	cfg, tc := configWithCreator()
	ch, _ := New(cfg)

	// HMAC computed with the wrong secret: well-formed hex, wrong value.
	forged := signedWith("not-the-secret", labeledTaskBody)
	w := httptest.NewRecorder()
	ch.HandleWebhook(w, postUnauthenticated("issues", forged, labeledTaskBody))

	if w.Code != http.StatusUnauthorized {
		t.Errorf("Code = %d, want %d", w.Code, http.StatusUnauthorized)
	}
	if got := tc.copyEvents(); len(got) != 0 {
		t.Fatalf("SECURITY: forged-signature issues.labeled created %d task(s), want 0: %+v", len(got), got)
	}
}

// TestSecurity_BadSignature_PullRequestOpened_CreatesNoTask — same
// for the PR-opened task path.
func TestSecurity_BadSignature_PullRequestOpened_CreatesNoTask(t *testing.T) {
	cfg, tc := configWithCreator()
	ch, _ := New(cfg)

	forged := signedWith("not-the-secret", prOpenedTaskBody)
	w := httptest.NewRecorder()
	ch.HandleWebhook(w, postUnauthenticated("pull_request", forged, prOpenedTaskBody))

	if w.Code != http.StatusUnauthorized {
		t.Errorf("Code = %d, want %d", w.Code, http.StatusUnauthorized)
	}
	if got := tc.copyEvents(); len(got) != 0 {
		t.Fatalf("SECURITY: forged-signature pull_request.opened created %d task(s), want 0: %+v", len(got), got)
	}
}

// TestSecurity_MissingSignature_IssuesLabeled_CreatesNoTask — an
// entirely unsigned delivery of a task-creating payload must create
// no task.
func TestSecurity_MissingSignature_IssuesLabeled_CreatesNoTask(t *testing.T) {
	cfg, tc := configWithCreator()
	ch, _ := New(cfg)

	w := httptest.NewRecorder()
	ch.HandleWebhook(w, postUnauthenticated("issues", "", labeledTaskBody))

	if w.Code != http.StatusUnauthorized {
		t.Errorf("Code = %d, want %d", w.Code, http.StatusUnauthorized)
	}
	if got := tc.copyEvents(); len(got) != 0 {
		t.Fatalf("SECURITY: unsigned issues.labeled created %d task(s), want 0: %+v", len(got), got)
	}
}

// TestSecurity_MalformedSignature_PullRequestOpened_CreatesNoTask —
// a non-hex signature header on a task-creating payload must create
// no task (exercises the hex.DecodeString error branch).
func TestSecurity_MalformedSignature_PullRequestOpened_CreatesNoTask(t *testing.T) {
	cfg, tc := configWithCreator()
	ch, _ := New(cfg)

	w := httptest.NewRecorder()
	ch.HandleWebhook(w, postUnauthenticated("pull_request", "sha256=not-hex-zz", prOpenedTaskBody))

	if w.Code != http.StatusUnauthorized {
		t.Errorf("Code = %d, want %d", w.Code, http.StatusUnauthorized)
	}
	if got := tc.copyEvents(); len(got) != 0 {
		t.Fatalf("SECURITY: malformed-signature pull_request.opened created %d task(s), want 0: %+v", len(got), got)
	}
}

// TestSecurity_ValidSignatureControl_CreatesTask — a positive control
// living next to the negatives: the SAME payload + secret, correctly
// signed, DOES create exactly one task. This guards against a test
// that vacuously passes because the payload stopped being
// task-creating (e.g. label/config drift). If this breaks while the
// negatives still pass, the negatives have gone vacuous.
func TestSecurity_ValidSignatureControl_CreatesTask(t *testing.T) {
	cfg, tc := configWithCreator()
	ch, _ := New(cfg)

	good := signedWith("shhh", labeledTaskBody) // "shhh" == validConfig secret
	w := httptest.NewRecorder()
	ch.HandleWebhook(w, postUnauthenticated("issues", good, labeledTaskBody))

	if w.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200", w.Code)
	}
	if got := tc.copyEvents(); len(got) != 1 {
		t.Fatalf("control: correctly-signed issues.labeled created %d task(s), want 1", len(got))
	}
}
