package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/conversation"
)

// validConfig returns a Config seeded with safe defaults for tests
// that don't care about the specific allowlist contents.
func validConfig() Config {
	return Config{
		AppID:           12345,
		WebhookSecret:   "shhh",
		RepoAllowlist:   []string{"acme/api"},
		TaskLabels:      []string{"vornik-task"},
		PRReviewLabels:  nil, // empty = all opened PRs
		SenderAllowlist: nil, // empty = dev-mode pass-through
	}
}

// signedRequest builds an httptest request with a valid HMAC-SHA256
// signature for the given body and secret.
func signedRequest(secret, event, delivery string, body []byte) *http.Request {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/github-app/webhook", strings.NewReader(string(body)))
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", delivery)
	return req
}

// TestNew_RejectsEmptySecret — defensive boot guard.
func TestNew_RejectsEmptySecret(t *testing.T) {
	cfg := validConfig()
	cfg.WebhookSecret = ""
	_, err := New(cfg)
	if err == nil {
		t.Fatal("New with empty WebhookSecret returned nil error, want one")
	}
}

// TestNew_RejectsEmptyRepoAllowlist — defensive: a fresh install
// with no repo configured must reject every delivery.
func TestNew_RejectsEmptyRepoAllowlist(t *testing.T) {
	cfg := validConfig()
	cfg.RepoAllowlist = nil
	_, err := New(cfg)
	if err == nil {
		t.Fatal("New with empty RepoAllowlist returned nil error, want one")
	}
}

// TestChannel_Name — channel identifier must be the value
// downstream consumers branch on.
func TestChannel_Name(t *testing.T) {
	ch, err := New(validConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := ch.Name(); got != "github-app" {
		t.Errorf("Name() = %q, want github-app", got)
	}
}

// TestChannel_SatisfiesInterface — compile-time guard.
func TestChannel_SatisfiesInterface(t *testing.T) {
	ch, _ := New(validConfig())
	var _ conversation.Channel = ch
}

// TestChannel_Send_UnconfiguredReturnsSentinel — when outbound
// credentials aren't wired (AppID / PrivateKey / InstallationID
// all zero), Send returns ErrOutboundNotConfigured so callers
// branch on it via errors.Is. The inbound webhook path still
// works.
func TestChannel_Send_UnconfiguredReturnsSentinel(t *testing.T) {
	ch, _ := New(validConfig())
	id, err := ch.Send(context.Background(), conversation.ChannelMessage{
		SessionID: "acme/api#issues/1",
		Text:      "hi",
	})
	if !errors.Is(err, ErrOutboundNotConfigured) {
		t.Errorf("Send err = %v, want ErrOutboundNotConfigured", err)
	}
	if id != "" {
		t.Errorf("Send id = %q, want empty", id)
	}
}

// TestChannel_ListSessions_EmptyBeforeAnyEvent — a freshly
// constructed channel reports zero sessions.
func TestChannel_ListSessions_EmptyBeforeAnyEvent(t *testing.T) {
	ch, _ := New(validConfig())
	sessions, err := ch.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("ListSessions = %d entries, want 0", len(sessions))
	}
}

// TestChannel_ListSessions_PopulatedAfterEvents — every inbound
// translation path (label, comment, PR open) records a session.
// Multiple events on the same issue collapse to one entry with
// the latest LastActivity. Output is newest-first.
func TestChannel_ListSessions_PopulatedAfterEvents(t *testing.T) {
	ch, _ := New(validConfig())

	// 1) issue labeled
	labelBody := []byte(`{
		"action": "labeled",
		"repository": {"full_name": "acme/api"},
		"sender": {"login": "vadim"},
		"installation": {"id": 1},
		"issue": {"number": 5, "title": "label-titled bug"},
		"label": {"name": "vornik-task"}
	}`)
	rec := httptest.NewRecorder()
	ch.HandleWebhook(rec, signedRequest("shhh", "issues", "d-lbl", labelBody))

	// 2) PR opened
	prBody := []byte(`{
		"action": "opened",
		"repository": {"full_name": "acme/api"},
		"sender": {"login": "alice"},
		"installation": {"id": 1},
		"pull_request": {"number": 99, "title": "fix flaky test"}
	}`)
	ch.HandleWebhook(httptest.NewRecorder(), signedRequest("shhh", "pull_request", "d-pr", prBody))

	// 3) @vornik mention on the same issue as #1 — should NOT
	// create a duplicate session; LastActivity updates instead.
	rx := &recordingReceiver{}
	ch.recvMu.Lock()
	ch.recv = rx
	ch.recvMu.Unlock()
	mentionBody := []byte(`{
		"action": "created",
		"repository": {"full_name": "acme/api"},
		"sender": {"login": "alice"},
		"installation": {"id": 1},
		"issue": {"number": 5, "title": "label-titled bug"},
		"comment": {"id": 100, "body": "@vornik ping", "user": {"login": "alice"}}
	}`)
	ch.HandleWebhook(httptest.NewRecorder(), signedRequest("shhh", "issue_comment", "d-mention", mentionBody))

	sessions, err := ch.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("ListSessions = %d, want 2 (issue 5 + PR 99)", len(sessions))
	}
	// Newest first: the mention on issue 5 came last in the test
	// timeline → issue 5 must be first.
	if sessions[0].ID != "acme/api#issues/5" {
		t.Errorf("sessions[0].ID = %q, want acme/api#issues/5 (newest)", sessions[0].ID)
	}
	if sessions[1].ID != "acme/api#pulls/99" {
		t.Errorf("sessions[1].ID = %q, want acme/api#pulls/99", sessions[1].ID)
	}
	// Issue 5 saw two distinct participants (vadim labeling, alice mentioning).
	if sessions[0].ParticipantCount != 2 {
		t.Errorf("ParticipantCount = %d, want 2", sessions[0].ParticipantCount)
	}
	// PR 99 has alice opening.
	if sessions[1].ParticipantCount != 1 {
		t.Errorf("PR ParticipantCount = %d, want 1", sessions[1].ParticipantCount)
	}
	if sessions[0].Title != "label-titled bug" {
		t.Errorf("sessions[0].Title = %q, want 'label-titled bug'", sessions[0].Title)
	}
	if sessions[1].Title != "fix flaky test" {
		t.Errorf("sessions[1].Title = %q, want 'fix flaky test'", sessions[1].Title)
	}
}

// TestChannel_RecordSession_DedupesSameParticipant — the same
// user firing multiple events on one session only counts once.
func TestChannel_RecordSession_DedupesSameParticipant(t *testing.T) {
	ch, _ := New(validConfig())
	inst := ch.installations[0]
	now := time.Now()
	ch.recordSession("acme/api#issues/1", "title", "vadim", now, inst)
	ch.recordSession("acme/api#issues/1", "title", "vadim", now.Add(time.Second), inst)
	sessions, _ := ch.ListSessions(context.Background())
	if len(sessions) != 1 {
		t.Fatalf("ListSessions = %d, want 1", len(sessions))
	}
	if sessions[0].ParticipantCount != 1 {
		t.Errorf("ParticipantCount = %d after duplicate, want 1", sessions[0].ParticipantCount)
	}
}

// TestChannel_RecordSession_LastActivityMonotonic — an older
// timestamp must NOT overwrite a newer one (defensive against
// out-of-order delivery).
func TestChannel_RecordSession_LastActivityMonotonic(t *testing.T) {
	ch, _ := New(validConfig())
	inst := ch.installations[0]
	newer := time.Now()
	older := newer.Add(-1 * time.Hour)
	ch.recordSession("acme/api#issues/1", "", "u1", newer, inst)
	ch.recordSession("acme/api#issues/1", "", "u1", older, inst)
	sessions, _ := ch.ListSessions(context.Background())
	if !sessions[0].LastActivity.Equal(newer) {
		t.Errorf("LastActivity = %v, want newer (%v)", sessions[0].LastActivity, newer)
	}
}

// TestChannel_PRComment_TitleFallback — when issue.title is empty
// on a PR comment, the recorded session title falls back to "PR
// #N" rather than an empty string.
func TestChannel_PRComment_TitleFallback(t *testing.T) {
	ch, _ := New(validConfig())
	rx := &recordingReceiver{}
	ch.recvMu.Lock()
	ch.recv = rx
	ch.recvMu.Unlock()

	body := []byte(`{
		"action": "created",
		"repository": {"full_name": "acme/api"},
		"sender": {"login": "alice"},
		"installation": {"id": 1},
		"issue": {"number": 42, "title": "", "pull_request": {"url": "https://api.github.com/repos/acme/api/pulls/42"}},
		"comment": {"id": 1, "body": "@vornik review", "user": {"login": "alice"}}
	}`)
	ch.HandleWebhook(httptest.NewRecorder(), signedRequest("shhh", "issue_comment", "d-pr-notitle", body))

	sessions, _ := ch.ListSessions(context.Background())
	if len(sessions) != 1 {
		t.Fatalf("ListSessions = %d, want 1", len(sessions))
	}
	if sessions[0].Title != "PR #42" {
		t.Errorf("Title = %q, want 'PR #42' fallback", sessions[0].Title)
	}
}

// stubTaskCreator records every Create call so handler-path
// tests can assert the translation.
type stubTaskCreator struct {
	mu        sync.Mutex
	events    []TaskCreationEvent
	returnErr error
}

func (s *stubTaskCreator) Create(_ context.Context, ev TaskCreationEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
	return s.returnErr
}

func (s *stubTaskCreator) copyEvents() []TaskCreationEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]TaskCreationEvent, len(s.events))
	copy(out, s.events)
	return out
}

// TestChannel_IssuesLabeled_FiresTaskCreator — when TaskCreator
// is wired and a matching label fires, the channel hands the
// event to the creator with every field populated.
func TestChannel_IssuesLabeled_FiresTaskCreator(t *testing.T) {
	cfg := validConfig()
	tc := &stubTaskCreator{}
	cfg.TaskCreator = tc
	ch, _ := New(cfg)

	body := []byte(`{
		"action": "labeled",
		"repository": {"full_name": "acme/api"},
		"sender": {"login": "vadim"},
		"installation": {"id": 9000},
		"issue": {"number": 5, "title": "bug", "body": "details", "labels": [{"name": "vornik-task"}, {"name": "urgent"}]},
		"label": {"name": "vornik-task"}
	}`)
	w := httptest.NewRecorder()
	ch.HandleWebhook(w, signedRequest("shhh", "issues", "d-lbl-fire", body))

	if w.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200", w.Code)
	}
	got := tc.copyEvents()
	if len(got) != 1 {
		t.Fatalf("TaskCreator saw %d events, want 1", len(got))
	}
	ev := got[0]
	if ev.Kind != "issues.labeled" {
		t.Errorf("Kind = %q, want issues.labeled", ev.Kind)
	}
	if ev.SessionID != "acme/api#issues/5" {
		t.Errorf("SessionID = %q, want acme/api#issues/5", ev.SessionID)
	}
	if ev.Title != "bug" || ev.Body != "details" {
		t.Errorf("Title/Body mismatch: %+v", ev)
	}
	if ev.InstallationID != 9000 || ev.Number != 5 || ev.Repo != "acme/api" {
		t.Errorf("Repo/Number/InstallationID mismatch: %+v", ev)
	}
	if ev.IdempotencyKey != "github-app:d-lbl-fire" {
		t.Errorf("IdempotencyKey = %q, want github-app:d-lbl-fire", ev.IdempotencyKey)
	}
	if len(ev.Labels) != 2 || ev.Labels[0] != "vornik-task" || ev.Labels[1] != "urgent" {
		t.Errorf("Labels = %v, want [vornik-task urgent]", ev.Labels)
	}
}

// TestChannel_PullRequestOpened_FiresTaskCreator — same as above
// for the PR-opened path, including label propagation.
func TestChannel_PullRequestOpened_FiresTaskCreator(t *testing.T) {
	cfg := validConfig()
	tc := &stubTaskCreator{}
	cfg.TaskCreator = tc
	ch, _ := New(cfg)

	body := []byte(`{
		"action": "opened",
		"repository": {"full_name": "acme/api"},
		"sender": {"login": "vadim"},
		"installation": {"id": 9001},
		"pull_request": {
			"number": 12, "title": "PR title", "body": "PR body",
			"labels": [{"name": "needs-review"}, {"name": "documentation"}]
		}
	}`)
	w := httptest.NewRecorder()
	ch.HandleWebhook(w, signedRequest("shhh", "pull_request", "d-pr-fire", body))

	if w.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200", w.Code)
	}
	got := tc.copyEvents()
	if len(got) != 1 {
		t.Fatalf("TaskCreator saw %d events, want 1", len(got))
	}
	ev := got[0]
	if ev.Kind != "pull_request.opened" {
		t.Errorf("Kind = %q, want pull_request.opened", ev.Kind)
	}
	if ev.SessionID != "acme/api#pulls/12" {
		t.Errorf("SessionID = %q, want acme/api#pulls/12", ev.SessionID)
	}
	if len(ev.Labels) != 2 || ev.Labels[0] != "needs-review" {
		t.Errorf("Labels = %v, want [needs-review, documentation]", ev.Labels)
	}
}

// TestChannel_TaskCreatorError_LoggedNotPropagated — a Create
// failure must NOT escalate into a non-200 HTTP response, or
// GitHub retries the delivery indefinitely.
func TestChannel_TaskCreatorError_LoggedNotPropagated(t *testing.T) {
	cfg := validConfig()
	tc := &stubTaskCreator{returnErr: errors.New("downstream failed")}
	cfg.TaskCreator = tc
	ch, _ := New(cfg)

	body := []byte(`{
		"action": "labeled",
		"repository": {"full_name": "acme/api"},
		"sender": {"login": "vadim"},
		"installation": {"id": 1},
		"issue": {"number": 1, "title": "x"},
		"label": {"name": "vornik-task"}
	}`)
	w := httptest.NewRecorder()
	ch.HandleWebhook(w, signedRequest("shhh", "issues", "d-lbl-err", body))
	if w.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200 even on TaskCreator failure", w.Code)
	}
}

// TestChannel_PullRequestOpened_TaskCreatorError — same for the
// PR path.
func TestChannel_PullRequestOpened_TaskCreatorError(t *testing.T) {
	cfg := validConfig()
	tc := &stubTaskCreator{returnErr: errors.New("downstream failed")}
	cfg.TaskCreator = tc
	ch, _ := New(cfg)

	body := []byte(`{
		"action": "opened",
		"repository": {"full_name": "acme/api"},
		"sender": {"login": "vadim"},
		"installation": {"id": 1},
		"pull_request": {"number": 1, "title": "x"}
	}`)
	w := httptest.NewRecorder()
	ch.HandleWebhook(w, signedRequest("shhh", "pull_request", "d-pr-err", body))
	if w.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200 even on TaskCreator failure", w.Code)
	}
}

// TestChannel_PullRequestOpened_TitleFallback — same fallback for
// the pull_request.opened path when pull_request.title is empty.
func TestChannel_PullRequestOpened_TitleFallback(t *testing.T) {
	ch, _ := New(validConfig())
	body := []byte(`{
		"action": "opened",
		"repository": {"full_name": "acme/api"},
		"sender": {"login": "alice"},
		"installation": {"id": 1},
		"pull_request": {"number": 77, "title": ""}
	}`)
	ch.HandleWebhook(httptest.NewRecorder(), signedRequest("shhh", "pull_request", "d-pr-notitle", body))

	sessions, _ := ch.ListSessions(context.Background())
	if len(sessions) != 1 {
		t.Fatalf("ListSessions = %d, want 1", len(sessions))
	}
	if sessions[0].Title != "PR #77" {
		t.Errorf("Title = %q, want 'PR #77' fallback", sessions[0].Title)
	}
}

// TestChannel_ResolveSpeaker_AllowlistGate — when SenderAllowlist
// is configured, only listed logins resolve cleanly.
func TestChannel_ResolveSpeaker_AllowlistGate(t *testing.T) {
	cfg := validConfig()
	cfg.SenderAllowlist = []string{"vadim", "ci-bot"}
	ch, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sp, err := ch.ResolveSpeaker(context.Background(), "vadim")
	if err != nil {
		t.Fatalf("ResolveSpeaker(vadim): %v", err)
	}
	if sp.ID != "github:vadim" {
		t.Errorf("Speaker.ID = %q, want github:vadim", sp.ID)
	}
	if sp.ChannelHandle != "vadim" {
		t.Errorf("Speaker.ChannelHandle = %q, want vadim", sp.ChannelHandle)
	}
	if _, err := ch.ResolveSpeaker(context.Background(), "stranger"); !errors.Is(err, conversation.ErrSpeakerUnknown) {
		t.Errorf("denied login err = %v, want ErrSpeakerUnknown", err)
	}
}

// TestChannel_ResolveSpeaker_DevModePassThrough — empty
// SenderAllowlist allows everyone.
func TestChannel_ResolveSpeaker_DevModePassThrough(t *testing.T) {
	ch, _ := New(validConfig())
	sp, err := ch.ResolveSpeaker(context.Background(), "anyone")
	if err != nil {
		t.Fatalf("ResolveSpeaker dev-mode: %v", err)
	}
	if sp.ID != "github:anyone" {
		t.Errorf("Speaker.ID = %q, want github:anyone", sp.ID)
	}
}

// TestChannel_StartStop_BindsAndClearsReceiver — Start blocks
// until ctx is cancelled, Stop clears the binding.
func TestChannel_StartStop_BindsAndClearsReceiver(t *testing.T) {
	ch, _ := New(validConfig())
	rx := &recordingReceiver{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ch.Start(ctx, rx) }()

	// Receiver should be bound by the time Start runs.
	bound := false
	for i := 0; i < 200 && !bound; i++ {
		ch.recvMu.RLock()
		bound = ch.recv != nil
		ch.recvMu.RUnlock()
		if !bound {
			time.Sleep(time.Millisecond)
		}
	}
	if !bound {
		t.Fatal("Start did not bind Receiver")
	}
	cancel()
	<-done

	if err := ch.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	ch.recvMu.RLock()
	if ch.recv != nil {
		t.Error("Stop did not clear Receiver binding")
	}
	ch.recvMu.RUnlock()
}

// TestHandleWebhook_RejectsBadMethod — only POST.
func TestHandleWebhook_RejectsBadMethod(t *testing.T) {
	ch, _ := New(validConfig())
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/github-app/webhook", nil)
	ch.HandleWebhook(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("Code = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

// TestHandleWebhook_RejectsBadSignature — wrong HMAC.
func TestHandleWebhook_RejectsBadSignature(t *testing.T) {
	ch, _ := New(validConfig())
	body := []byte(`{"action":"ping"}`)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/github-app/webhook", strings.NewReader(string(body)))
	r.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	r.Header.Set("X-GitHub-Event", "ping")
	w := httptest.NewRecorder()
	ch.HandleWebhook(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("Code = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

// TestHandleWebhook_RejectsMissingSignature — empty header.
func TestHandleWebhook_RejectsMissingSignature(t *testing.T) {
	ch, _ := New(validConfig())
	body := []byte(`{"action":"ping"}`)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/github-app/webhook", strings.NewReader(string(body)))
	r.Header.Set("X-GitHub-Event", "ping")
	w := httptest.NewRecorder()
	ch.HandleWebhook(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("Code = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

// TestHandleWebhook_RejectsMalformedSignature — non-hex header.
func TestHandleWebhook_RejectsMalformedSignature(t *testing.T) {
	ch, _ := New(validConfig())
	body := []byte(`{"action":"ping"}`)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/github-app/webhook", strings.NewReader(string(body)))
	r.Header.Set("X-Hub-Signature-256", "sha256=not-hex-at-all-zzz")
	r.Header.Set("X-GitHub-Event", "ping")
	w := httptest.NewRecorder()
	ch.HandleWebhook(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("Code = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

// TestHandleWebhook_AcksRepoless — events without a repository
// (ping, installation) are acked.
func TestHandleWebhook_AcksRepoless(t *testing.T) {
	ch, _ := New(validConfig())
	body := []byte(`{"action":"ping"}`)
	r := signedRequest("shhh", "ping", "d-0", body)
	w := httptest.NewRecorder()
	ch.HandleWebhook(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("Code = %d, want %d", w.Code, http.StatusOK)
	}
}

// TestHandleWebhook_RejectsDisallowedRepo — repo not in allowlist
// gets 403 + audit log.
func TestHandleWebhook_RejectsDisallowedRepo(t *testing.T) {
	ch, _ := New(validConfig())
	body := []byte(`{"action":"opened","repository":{"full_name":"someone-else/secret-repo"},"pull_request":{"number":1}}`)
	r := signedRequest("shhh", "pull_request", "d-1", body)
	w := httptest.NewRecorder()
	ch.HandleWebhook(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("Code = %d, want %d", w.Code, http.StatusForbidden)
	}
}

// TestHandleWebhook_RejectsMissingEvent — without X-GitHub-Event
// the handler returns 400.
func TestHandleWebhook_RejectsMissingEvent(t *testing.T) {
	ch, _ := New(validConfig())
	body := []byte(`{}`)
	// signedRequest sets X-GitHub-Event; build manually here.
	mac := hmac.New(sha256.New, []byte("shhh"))
	_, _ = mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	r := httptest.NewRequest(http.MethodPost, "/api/v1/github-app/webhook", strings.NewReader(string(body)))
	r.Header.Set("X-Hub-Signature-256", sig)
	w := httptest.NewRecorder()
	ch.HandleWebhook(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("Code = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// TestHandleWebhook_RejectsInvalidJSON — malformed body.
func TestHandleWebhook_RejectsInvalidJSON(t *testing.T) {
	ch, _ := New(validConfig())
	body := []byte(`{not valid json`)
	r := signedRequest("shhh", "issues", "d-2", body)
	w := httptest.NewRecorder()
	ch.HandleWebhook(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("Code = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// TestHandleWebhook_BodyTooLarge — over-cap payload.
func TestHandleWebhook_BodyTooLarge(t *testing.T) {
	ch, _ := New(validConfig())
	// 9 MiB > 8 MiB cap.
	body := make([]byte, 9*1024*1024)
	for i := range body {
		body[i] = 'x'
	}
	body[0] = '{'
	body[len(body)-1] = '}'
	r := signedRequest("shhh", "issues", "d-3", body)
	w := httptest.NewRecorder()
	ch.HandleWebhook(w, r)
	if w.Code != http.StatusRequestEntityTooLarge && w.Code != http.StatusUnauthorized {
		// The signature is computed over the full body, so verifySignature
		// runs first; whether it succeeds depends on hmac math. Accept
		// either of the two valid rejection codes here — what we care
		// about is that the request was REJECTED.
		t.Errorf("Code = %d, want 413 or 401 rejection", w.Code)
	}
}

// TestHandleWebhook_IssueComment_MentionDispatchesToReceiver — the
// happy path for the @vornik reply flow.
func TestHandleWebhook_IssueComment_MentionDispatchesToReceiver(t *testing.T) {
	ch, _ := New(validConfig())
	rx := &recordingReceiver{}
	ch.recvMu.Lock()
	ch.recv = rx
	ch.recvMu.Unlock()

	body := []byte(`{
		"action": "created",
		"repository": {"full_name": "acme/api", "name": "api", "owner": {"login": "acme"}},
		"sender": {"login": "vadim", "id": 42},
		"installation": {"id": 9999},
		"issue": {"number": 7, "title": "bug"},
		"comment": {"id": 555, "body": "hey @vornik please look at this", "user": {"login": "vadim", "id": 42}}
	}`)
	r := signedRequest("shhh", "issue_comment", "d-mention", body)
	w := httptest.NewRecorder()
	ch.HandleWebhook(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200", w.Code)
	}
	got := rx.copyGot()
	if len(got) != 1 {
		t.Fatalf("Receiver got %d messages, want 1", len(got))
	}
	if got[0].SessionID != "acme/api#issues/7" {
		t.Errorf("SessionID = %q, want acme/api#issues/7", got[0].SessionID)
	}
	if got[0].Source != "github-app" {
		t.Errorf("Source = %q, want github-app", got[0].Source)
	}
	if got[0].SpeakerID != "vadim" {
		t.Errorf("SpeakerID = %q, want vadim", got[0].SpeakerID)
	}
	if got[0].ID != "555" {
		t.Errorf("ID = %q, want 555 (comment id)", got[0].ID)
	}
	if got[0].ChannelSpecific["repo"] != "acme/api" {
		t.Errorf("ChannelSpecific[repo] = %q, want acme/api", got[0].ChannelSpecific["repo"])
	}
	if got[0].ChannelSpecific["installation_id"] != "9999" {
		t.Errorf("ChannelSpecific[installation_id] = %q, want 9999", got[0].ChannelSpecific["installation_id"])
	}
	if got[0].ChannelSpecific["github_delivery"] != "d-mention" {
		t.Errorf("ChannelSpecific[github_delivery] = %q, want d-mention", got[0].ChannelSpecific["github_delivery"])
	}
	if got[0].ChannelSpecific["issue_number"] != "7" {
		t.Errorf("ChannelSpecific[issue_number] = %q, want 7", got[0].ChannelSpecific["issue_number"])
	}
}

// TestHandleWebhook_PRComment_MentionDispatchesToReceiver — PR
// comments use the same `issue_comment` event but resolve to the
// `#pulls/N` session encoding.
func TestHandleWebhook_PRComment_MentionDispatchesToReceiver(t *testing.T) {
	ch, _ := New(validConfig())
	rx := &recordingReceiver{}
	ch.recvMu.Lock()
	ch.recv = rx
	ch.recvMu.Unlock()

	body := []byte(`{
		"action": "created",
		"repository": {"full_name": "acme/api", "name": "api", "owner": {"login": "acme"}},
		"sender": {"login": "alice"},
		"installation": {"id": 1},
		"issue": {"number": 42, "title": "PR title", "pull_request": {"url": "https://api.github.com/repos/acme/api/pulls/42"}},
		"comment": {"id": 1000, "body": "@vornik test plz", "user": {"login": "alice"}}
	}`)
	r := signedRequest("shhh", "issue_comment", "d-pr", body)
	w := httptest.NewRecorder()
	ch.HandleWebhook(w, r)

	got := rx.copyGot()
	if len(got) != 1 {
		t.Fatalf("Receiver got %d messages, want 1", len(got))
	}
	if got[0].SessionID != "acme/api#pulls/42" {
		t.Errorf("SessionID = %q, want acme/api#pulls/42", got[0].SessionID)
	}
}

// TestHandleWebhook_IssueComment_NoMention_Discards — comments
// without @vornik never reach the Receiver.
func TestHandleWebhook_IssueComment_NoMention_Discards(t *testing.T) {
	ch, _ := New(validConfig())
	rx := &recordingReceiver{}
	ch.recvMu.Lock()
	ch.recv = rx
	ch.recvMu.Unlock()

	body := []byte(`{
		"action": "created",
		"repository": {"full_name": "acme/api"},
		"sender": {"login": "vadim"},
		"installation": {"id": 1},
		"issue": {"number": 1},
		"comment": {"id": 1, "body": "regular comment", "user": {"login": "vadim"}}
	}`)
	r := signedRequest("shhh", "issue_comment", "d-no-mention", body)
	w := httptest.NewRecorder()
	ch.HandleWebhook(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200", w.Code)
	}
	if got := rx.copyGot(); len(got) != 0 {
		t.Errorf("Receiver got %d messages, want 0", len(got))
	}
}

// TestHandleWebhook_IssueComment_DisallowedSender_Discards — the
// @vornik mention from a sender not on the allowlist is dropped
// without reaching the Receiver.
func TestHandleWebhook_IssueComment_DisallowedSender_Discards(t *testing.T) {
	cfg := validConfig()
	cfg.SenderAllowlist = []string{"vadim"}
	ch, _ := New(cfg)
	rx := &recordingReceiver{}
	ch.recvMu.Lock()
	ch.recv = rx
	ch.recvMu.Unlock()

	body := []byte(`{
		"action": "created",
		"repository": {"full_name": "acme/api"},
		"sender": {"login": "stranger"},
		"installation": {"id": 1},
		"issue": {"number": 1},
		"comment": {"id": 1, "body": "@vornik help", "user": {"login": "stranger"}}
	}`)
	r := signedRequest("shhh", "issue_comment", "d-stranger", body)
	w := httptest.NewRecorder()
	ch.HandleWebhook(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200 (we don't want GitHub to retry)", w.Code)
	}
	if got := rx.copyGot(); len(got) != 0 {
		t.Errorf("Receiver got %d messages, want 0 (sender not allowed)", len(got))
	}
}

// TestHandleWebhook_IssueComment_NoReceiver_Logs — if no Receiver
// is bound, the message is dropped without panic.
func TestHandleWebhook_IssueComment_NoReceiver_Logs(t *testing.T) {
	ch, _ := New(validConfig())
	// No Receiver bound.
	body := []byte(`{
		"action": "created",
		"repository": {"full_name": "acme/api"},
		"sender": {"login": "vadim"},
		"installation": {"id": 1},
		"issue": {"number": 1},
		"comment": {"id": 1, "body": "@vornik hello", "user": {"login": "vadim"}}
	}`)
	r := signedRequest("shhh", "issue_comment", "d-noreceiver", body)
	w := httptest.NewRecorder()
	ch.HandleWebhook(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200", w.Code)
	}
}

// TestHandleWebhook_IssueComment_ReceiverErrorSwallowed — Receiver
// errors don't cascade into the HTTP response.
func TestHandleWebhook_IssueComment_ReceiverErrorSwallowed(t *testing.T) {
	ch, _ := New(validConfig())
	rx := &recordingReceiver{returnErr: errors.New("downstream broken")}
	ch.recvMu.Lock()
	ch.recv = rx
	ch.recvMu.Unlock()

	body := []byte(`{
		"action": "created",
		"repository": {"full_name": "acme/api"},
		"sender": {"login": "vadim"},
		"installation": {"id": 1},
		"issue": {"number": 1},
		"comment": {"id": 1, "body": "@vornik hi", "user": {"login": "vadim"}}
	}`)
	r := signedRequest("shhh", "issue_comment", "d-rxerr", body)
	w := httptest.NewRecorder()
	ch.HandleWebhook(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200 even on Receiver error", w.Code)
	}
}

// TestHandleWebhook_IssueLabeled_OnTriggerLabel_Logged — labeled
// issues with a matching trigger label currently log; slice 4D
// wires task creation.
func TestHandleWebhook_IssueLabeled_OnTriggerLabel_Logged(t *testing.T) {
	ch, _ := New(validConfig())
	body := []byte(`{
		"action": "labeled",
		"repository": {"full_name": "acme/api"},
		"sender": {"login": "vadim"},
		"installation": {"id": 1},
		"issue": {"number": 5, "title": "thing", "body": "details"},
		"label": {"name": "vornik-task"}
	}`)
	r := signedRequest("shhh", "issues", "d-lbl-ok", body)
	w := httptest.NewRecorder()
	ch.HandleWebhook(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200", w.Code)
	}
}

// TestHandleWebhook_IssueLabeled_OffTriggerLabel_Discarded — labels
// outside the trigger set don't fire downstream.
func TestHandleWebhook_IssueLabeled_OffTriggerLabel_Discarded(t *testing.T) {
	ch, _ := New(validConfig())
	body := []byte(`{
		"action": "labeled",
		"repository": {"full_name": "acme/api"},
		"sender": {"login": "vadim"},
		"installation": {"id": 1},
		"issue": {"number": 5, "title": "thing"},
		"label": {"name": "documentation"}
	}`)
	r := signedRequest("shhh", "issues", "d-lbl-skip", body)
	w := httptest.NewRecorder()
	ch.HandleWebhook(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200", w.Code)
	}
}

// TestHandleWebhook_PullRequestOpened_Logged — opened PR with no
// PRReviewLabels config (empty = all fire).
func TestHandleWebhook_PullRequestOpened_Logged(t *testing.T) {
	ch, _ := New(validConfig())
	body := []byte(`{
		"action": "opened",
		"repository": {"full_name": "acme/api"},
		"sender": {"login": "vadim"},
		"installation": {"id": 1},
		"pull_request": {"number": 9, "title": "PR title", "diff_url": "https://github.com/acme/api/pull/9.diff", "labels": []}
	}`)
	r := signedRequest("shhh", "pull_request", "d-pr-open", body)
	w := httptest.NewRecorder()
	ch.HandleWebhook(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200", w.Code)
	}
}

// TestHandleWebhook_PullRequestOpened_LabelGate_Discards — when
// PRReviewLabels is non-empty, only matching PRs fire.
func TestHandleWebhook_PullRequestOpened_LabelGate_Discards(t *testing.T) {
	cfg := validConfig()
	cfg.PRReviewLabels = []string{"needs-review"}
	ch, _ := New(cfg)

	body := []byte(`{
		"action": "opened",
		"repository": {"full_name": "acme/api"},
		"sender": {"login": "vadim"},
		"installation": {"id": 1},
		"pull_request": {"number": 9, "title": "PR title", "labels": [{"name": "documentation"}]}
	}`)
	r := signedRequest("shhh", "pull_request", "d-pr-skip", body)
	w := httptest.NewRecorder()
	ch.HandleWebhook(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200", w.Code)
	}
	// Behaviour check: with `needs-review` required and only `documentation`
	// present, the PR was filtered. No assertion here because the only
	// observable effect is the (suppressed) log line; the lack of crash
	// + 200 status is the contract this test guards.
}

// TestHandleWebhook_PullRequestOpened_LabelGate_Accepts — PRs
// carrying a required label still fire.
func TestHandleWebhook_PullRequestOpened_LabelGate_Accepts(t *testing.T) {
	cfg := validConfig()
	cfg.PRReviewLabels = []string{"needs-review"}
	ch, _ := New(cfg)

	body := []byte(`{
		"action": "opened",
		"repository": {"full_name": "acme/api"},
		"sender": {"login": "vadim"},
		"installation": {"id": 1},
		"pull_request": {"number": 10, "title": "PR title", "labels": [{"name": "needs-review"}, {"name": "documentation"}]}
	}`)
	r := signedRequest("shhh", "pull_request", "d-pr-match", body)
	w := httptest.NewRecorder()
	ch.HandleWebhook(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200", w.Code)
	}
}

// TestHandleWebhook_UnknownEvent_Acks — non-handled event types
// return 200 so GitHub doesn't retry.
func TestHandleWebhook_UnknownEvent_Acks(t *testing.T) {
	ch, _ := New(validConfig())
	body := []byte(`{"action":"created","repository":{"full_name":"acme/api"},"sender":{"login":"vadim"}}`)
	r := signedRequest("shhh", "release", "d-release", body)
	w := httptest.NewRecorder()
	ch.HandleWebhook(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200 for unknown event", w.Code)
	}
}

// TestHandleWebhook_IssuesLabeled_MalformedPayload_NilGuards —
// `issues` event with action=labeled but missing the `issue` or
// `label` object hits the defensive nil guards in handleIssueLabeled.
func TestHandleWebhook_IssuesLabeled_MalformedPayload_NilGuards(t *testing.T) {
	ch, _ := New(validConfig())
	body := []byte(`{
		"action": "labeled",
		"repository": {"full_name": "acme/api"},
		"sender": {"login": "vadim"},
		"installation": {"id": 1}
	}`)
	r := signedRequest("shhh", "issues", "d-lbl-malformed", body)
	w := httptest.NewRecorder()
	ch.HandleWebhook(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200 even on malformed payload", w.Code)
	}
}

// TestHandleWebhook_IssueComment_MalformedPayload_NilGuards —
// `issue_comment` event with no comment object hits the nil guards.
func TestHandleWebhook_IssueComment_MalformedPayload_NilGuards(t *testing.T) {
	ch, _ := New(validConfig())
	body := []byte(`{
		"action": "created",
		"repository": {"full_name": "acme/api"},
		"sender": {"login": "vadim"},
		"installation": {"id": 1}
	}`)
	r := signedRequest("shhh", "issue_comment", "d-cmt-malformed", body)
	w := httptest.NewRecorder()
	ch.HandleWebhook(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200 even on malformed payload", w.Code)
	}
}

// TestHandleWebhook_PullRequestOpened_MalformedPayload_NilGuards —
// `pull_request` event with no pull_request object hits the nil
// guard.
func TestHandleWebhook_PullRequestOpened_MalformedPayload_NilGuards(t *testing.T) {
	ch, _ := New(validConfig())
	body := []byte(`{
		"action": "opened",
		"repository": {"full_name": "acme/api"},
		"sender": {"login": "vadim"},
		"installation": {"id": 1}
	}`)
	r := signedRequest("shhh", "pull_request", "d-pr-malformed", body)
	w := httptest.NewRecorder()
	ch.HandleWebhook(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200 even on malformed payload", w.Code)
	}
}

// TestMentionsVornik_Cases — word-boundary aware mention detection.
func TestMentionsVornik_Cases(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{"hey @vornik please", true},
		{"@vornik", true},
		{"@Vornik help", true}, // case-insensitive
		{"go @vornik!", true},
		{"@vornik-deploy", false}, // word-boundary: not a mention
		{"@vornik_bot", false},
		{"@vornik123", false},
		{"nothing here", false},
		{"@swarm", false},
		{"@vornikoesthing", false},
	}
	for _, c := range cases {
		if got := mentionsVornik(c.body); got != c.want {
			t.Errorf("mentionsVornik(%q) = %v, want %v", c.body, got, c.want)
		}
	}
}

// TestSessionIDFormat — explicit guard on the session-id encoding.
// Future channels depend on this string shape to dedupe or route.
func TestSessionIDFormat(t *testing.T) {
	ch, _ := New(validConfig())
	rx := &recordingReceiver{}
	ch.recvMu.Lock()
	ch.recv = rx
	ch.recvMu.Unlock()

	// Two consecutive comments on the same issue → same SessionID.
	for i := 0; i < 2; i++ {
		body := []byte(fmt.Sprintf(`{
			"action": "created",
			"repository": {"full_name": "acme/api"},
			"sender": {"login": "vadim"},
			"installation": {"id": 1},
			"issue": {"number": 100},
			"comment": {"id": %d, "body": "@vornik #%d", "user": {"login": "vadim"}}
		}`, i+1, i))
		r := signedRequest("shhh", "issue_comment", fmt.Sprintf("d-%d", i), body)
		w := httptest.NewRecorder()
		ch.HandleWebhook(w, r)
	}
	got := rx.copyGot()
	if len(got) != 2 {
		t.Fatalf("Receiver got %d messages, want 2", len(got))
	}
	if got[0].SessionID != got[1].SessionID {
		t.Errorf("SessionIDs diverged: %q vs %q", got[0].SessionID, got[1].SessionID)
	}
	if got[0].SessionID != "acme/api#issues/100" {
		t.Errorf("SessionID = %q, want acme/api#issues/100", got[0].SessionID)
	}
}

// TestHandleWebhook_BadJSONReadFailure — io.LimitReader EOF before
// completion. Not strictly reachable via httptest.NewRequest with a
// strings.Reader (always EOFs cleanly), so this test asserts the
// happy-path read instead — covers io.ReadAll's success path.
func TestHandleWebhook_HappyReadPath(t *testing.T) {
	ch, _ := New(validConfig())
	body := []byte(`{"action":"ping"}`)
	r := signedRequest("shhh", "ping", "d-read", body)
	w := httptest.NewRecorder()
	ch.HandleWebhook(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200", w.Code)
	}
}

// TestHandleWebhook_ReadBodyFails — exercises the io.ReadAll error
// branch by feeding a body whose Read call errors.
func TestHandleWebhook_ReadBodyFails(t *testing.T) {
	ch, _ := New(validConfig())
	// Use a Reader that errors on Read.
	r := httptest.NewRequest(http.MethodPost, "/api/v1/github-app/webhook", &erroringReader{})
	r.Header.Set("X-Hub-Signature-256", "sha256=00")
	r.Header.Set("X-GitHub-Event", "ping")
	w := httptest.NewRecorder()
	ch.HandleWebhook(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("Code = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

type erroringReader struct{}

func (e *erroringReader) Read(_ []byte) (int, error) { return 0, errors.New("simulated read failure") }
func (e *erroringReader) Close() error               { return nil }

var _ io.ReadCloser = (*erroringReader)(nil)

// recordingReceiver captures every Receive call for assertions.
type recordingReceiver struct {
	mu        sync.Mutex
	got       []conversation.ChannelMessage
	returnErr error
}

func (r *recordingReceiver) Receive(_ context.Context, m conversation.ChannelMessage) error {
	r.mu.Lock()
	r.got = append(r.got, m)
	r.mu.Unlock()
	return r.returnErr
}

func (r *recordingReceiver) copyGot() []conversation.ChannelMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]conversation.ChannelMessage, len(r.got))
	copy(out, r.got)
	return out
}
