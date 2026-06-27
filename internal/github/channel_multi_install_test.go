package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// multiInstallConfig returns a Config with two distinct
// installations. project-A owns installation 100 with repo
// acme/api; project-B owns installation 200 with repo widgets/web.
// Outbound credentials are left zero so the inbound paths are
// what's under test here.
func multiInstallConfig(t *testing.T, tcA, tcB TaskCreator) Config {
	t.Helper()
	return Config{
		WebhookSecret: "shhh",
		Installations: []InstallationConfig{
			{
				ProjectID:       "project-A",
				InstallationID:  100,
				RepoAllowlist:   []string{"acme/api"},
				TaskLabels:      []string{"vornik-task"},
				PRReviewLabels:  nil,
				SenderAllowlist: nil,
				TaskCreator:     tcA,
			},
			{
				ProjectID:       "project-B",
				InstallationID:  200,
				RepoAllowlist:   []string{"widgets/web"},
				TaskLabels:      []string{"vornik-task"},
				PRReviewLabels:  nil,
				SenderAllowlist: nil,
				TaskCreator:     tcB,
			},
		},
	}
}

// TestNew_RejectsDuplicateInstallationID — two installations with
// the same installation_id is a config bug; New must surface it.
func TestNew_RejectsDuplicateInstallationID(t *testing.T) {
	cfg := Config{
		WebhookSecret: "shhh",
		Installations: []InstallationConfig{
			{ProjectID: "a", InstallationID: 100, RepoAllowlist: []string{"a/a"}},
			{ProjectID: "b", InstallationID: 100, RepoAllowlist: []string{"b/b"}},
		},
	}
	_, err := New(cfg)
	if err == nil || !strings.Contains(err.Error(), "duplicate installation_id") {
		t.Errorf("err = %v, want duplicate-installation_id failure", err)
	}
}

// TestNew_RejectsInstallationWithEmptyRepoAllowlist — defensive
// deny-all: each installation needs at least one repo.
func TestNew_RejectsInstallationWithEmptyRepoAllowlist(t *testing.T) {
	cfg := Config{
		WebhookSecret: "shhh",
		Installations: []InstallationConfig{
			{ProjectID: "a", InstallationID: 100, RepoAllowlist: []string{}},
		},
	}
	_, err := New(cfg)
	if err == nil || !strings.Contains(err.Error(), "empty RepoAllowlist") {
		t.Errorf("err = %v, want empty-RepoAllowlist failure", err)
	}
}

// TestNew_RejectsInstallationWithZeroInstallationID — defensive:
// without an installation_id the channel can't route the inbound.
func TestNew_RejectsInstallationWithZeroInstallationID(t *testing.T) {
	cfg := Config{
		WebhookSecret: "shhh",
		Installations: []InstallationConfig{
			{ProjectID: "a", InstallationID: 0, RepoAllowlist: []string{"a/a"}},
		},
	}
	_, err := New(cfg)
	if err == nil || !strings.Contains(err.Error(), "missing InstallationID") {
		t.Errorf("err = %v, want missing-InstallationID failure", err)
	}
}

// TestMultiInstall_RoutesIssueLabeledToOwningProject — two
// configured installations, two webhook events for different
// installation_ids. Each event must hit ONLY the corresponding
// project's TaskCreator and pin ONLY the owning project's
// session.
func TestMultiInstall_RoutesIssueLabeledToOwningProject(t *testing.T) {
	tcA := &stubTaskCreator{}
	tcB := &stubTaskCreator{}
	ch, err := New(multiInstallConfig(t, tcA, tcB))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Event for installation 100 → project-A, repo acme/api.
	bodyA := []byte(`{
		"action": "labeled",
		"repository": {"full_name": "acme/api"},
		"sender": {"login": "vadim"},
		"installation": {"id": 100},
		"issue": {"number": 5, "title": "A bug"},
		"label": {"name": "vornik-task"}
	}`)
	rec := httptest.NewRecorder()
	ch.HandleWebhook(rec, signedRequest("shhh", "issues", "d-A", bodyA))
	if rec.Code != http.StatusOK {
		t.Errorf("install-A Code = %d, want 200", rec.Code)
	}

	// Event for installation 200 → project-B, repo widgets/web.
	bodyB := []byte(`{
		"action": "labeled",
		"repository": {"full_name": "widgets/web"},
		"sender": {"login": "alice"},
		"installation": {"id": 200},
		"issue": {"number": 99, "title": "B bug"},
		"label": {"name": "vornik-task"}
	}`)
	recB := httptest.NewRecorder()
	ch.HandleWebhook(recB, signedRequest("shhh", "issues", "d-B", bodyB))
	if recB.Code != http.StatusOK {
		t.Errorf("install-B Code = %d, want 200", recB.Code)
	}

	gotA := tcA.copyEvents()
	gotB := tcB.copyEvents()
	if len(gotA) != 1 || len(gotB) != 1 {
		t.Fatalf("event counts: A=%d B=%d, want 1+1", len(gotA), len(gotB))
	}
	if gotA[0].Repo != "acme/api" || gotA[0].InstallationID != 100 {
		t.Errorf("A event = %+v, want repo=acme/api installation=100", gotA[0])
	}
	if gotB[0].Repo != "widgets/web" || gotB[0].InstallationID != 200 {
		t.Errorf("B event = %+v, want repo=widgets/web installation=200", gotB[0])
	}
	// Project pinning: each session belongs to its own project.
	if got := ch.ProjectForSession("acme/api#issues/5"); got != "project-A" {
		t.Errorf("ProjectForSession(acme/api#issues/5) = %q, want project-A", got)
	}
	if got := ch.ProjectForSession("widgets/web#issues/99"); got != "project-B" {
		t.Errorf("ProjectForSession(widgets/web#issues/99) = %q, want project-B", got)
	}
}

// TestMultiInstall_UnknownInstallationID_DropsWith200 — an event
// carrying an installation_id that doesn't match any configured
// route must be 200-acked + audit-logged, NEVER 4xx (GitHub
// retries non-200 indefinitely).
func TestMultiInstall_UnknownInstallationID_DropsWith200(t *testing.T) {
	tcA := &stubTaskCreator{}
	tcB := &stubTaskCreator{}
	ch, _ := New(multiInstallConfig(t, tcA, tcB))

	body := []byte(`{
		"action": "labeled",
		"repository": {"full_name": "acme/api"},
		"sender": {"login": "vadim"},
		"installation": {"id": 9999},
		"issue": {"number": 5, "title": "stranger"},
		"label": {"name": "vornik-task"}
	}`)
	rec := httptest.NewRecorder()
	ch.HandleWebhook(rec, signedRequest("shhh", "issues", "d-unknown", body))
	if rec.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200 (GitHub retries on non-200)", rec.Code)
	}
	if n := len(tcA.copyEvents()) + len(tcB.copyEvents()); n != 0 {
		t.Errorf("TaskCreators fired %d events for unknown installation, want 0", n)
	}
}

// TestMultiInstall_DisallowedRepoWithinKnownInstall_Forbidden —
// a known installation_id but a repo not in THAT installation's
// allowlist still 403s + audit-logs (preserves existing
// behaviour). The cross-installation check matters here: the
// event for installation 100 must reject "widgets/web" even
// though widgets/web is allowed for installation 200.
func TestMultiInstall_DisallowedRepoWithinKnownInstall_Forbidden(t *testing.T) {
	tcA := &stubTaskCreator{}
	tcB := &stubTaskCreator{}
	ch, _ := New(multiInstallConfig(t, tcA, tcB))

	body := []byte(`{
		"action": "labeled",
		"repository": {"full_name": "widgets/web"},
		"sender": {"login": "vadim"},
		"installation": {"id": 100},
		"issue": {"number": 5, "title": "cross-tenant escape attempt"},
		"label": {"name": "vornik-task"}
	}`)
	rec := httptest.NewRecorder()
	ch.HandleWebhook(rec, signedRequest("shhh", "issues", "d-cross", body))
	if rec.Code != http.StatusForbidden {
		t.Errorf("Code = %d, want 403", rec.Code)
	}
	if n := len(tcA.copyEvents()) + len(tcB.copyEvents()); n != 0 {
		t.Errorf("TaskCreator fired %d events on cross-tenant attempt, want 0", n)
	}
}

// TestMultiInstall_IsolationOfSessions — recordSession on
// installation A must not contaminate installation B's session
// map.
func TestMultiInstall_IsolationOfSessions(t *testing.T) {
	tcA := &stubTaskCreator{}
	tcB := &stubTaskCreator{}
	ch, _ := New(multiInstallConfig(t, tcA, tcB))

	// Comment from project-A (mention path).
	rx := &recordingReceiver{}
	ch.recvMu.Lock()
	ch.recv = rx
	ch.recvMu.Unlock()

	bodyA := []byte(`{
		"action": "created",
		"repository": {"full_name": "acme/api"},
		"sender": {"login": "vadim"},
		"installation": {"id": 100},
		"issue": {"number": 5, "title": "A"},
		"comment": {"id": 1, "body": "@vornik hi from A", "user": {"login": "vadim"}}
	}`)
	ch.HandleWebhook(httptest.NewRecorder(), signedRequest("shhh", "issue_comment", "d-cA", bodyA))

	bodyB := []byte(`{
		"action": "created",
		"repository": {"full_name": "widgets/web"},
		"sender": {"login": "alice"},
		"installation": {"id": 200},
		"issue": {"number": 7, "title": "B"},
		"comment": {"id": 2, "body": "@vornik hi from B", "user": {"login": "alice"}}
	}`)
	ch.HandleWebhook(httptest.NewRecorder(), signedRequest("shhh", "issue_comment", "d-cB", bodyB))

	// Both sessions should exist and each pin the right project.
	if got := ch.ProjectForSession("acme/api#issues/5"); got != "project-A" {
		t.Errorf("acme/api#issues/5 project = %q, want project-A", got)
	}
	if got := ch.ProjectForSession("widgets/web#issues/7"); got != "project-B" {
		t.Errorf("widgets/web#issues/7 project = %q, want project-B", got)
	}
	// Cross-project sessions don't leak.
	if got := ch.ProjectForSession("acme/api#issues/9999"); got != "" {
		t.Errorf("unknown session leaked project: %q, want empty", got)
	}
}

// TestMultiInstall_PerInstallationTokenCache — outbound Sends to
// two different installations mint TWO tokens, one per
// installation, and reuse each one within its TTL.
func TestMultiInstall_PerInstallationTokenCache(t *testing.T) {
	var mintCalls atomic.Int64
	tokens := map[int64]string{
		100: "tokenA",
		200: "tokenB",
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/access_tokens") {
			mintCalls.Add(1)
			// Parse the installation ID from /app/installations/<id>/access_tokens
			// to return the right token.
			var instID int64
			for _, v := range []int64{100, 200} {
				if strings.Contains(r.URL.Path, "/installations/"+itoa(v)+"/") {
					instID = v
				}
			}
			tok := tokens[instID]
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"` + tok + `","expires_at":"2099-01-01T00:00:00Z"}`))
			return
		}
		if strings.Contains(r.URL.Path, "/issues/") && strings.HasSuffix(r.URL.Path, "/comments") {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":777}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	keyA, _ := rsa.GenerateKey(rand.Reader, 2048)
	keyB, _ := rsa.GenerateKey(rand.Reader, 2048)
	cfg := Config{
		WebhookSecret: "shhh",
		APIBaseURL:    server.URL,
		HTTPClient:    server.Client(),
		Installations: []InstallationConfig{
			{
				ProjectID:      "project-A",
				InstallationID: 100,
				AppID:          1,
				PrivateKey:     keyA,
				RepoAllowlist:  []string{"acme/api"},
				TaskLabels:     []string{"vornik-task"},
			},
			{
				ProjectID:      "project-B",
				InstallationID: 200,
				AppID:          2,
				PrivateKey:     keyB,
				RepoAllowlist:  []string{"widgets/web"},
				TaskLabels:     []string{"vornik-task"},
			},
		},
	}
	ch, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Pin both sessions via inbound events.
	inA := ch.installations[0]
	inB := ch.installations[1]
	now := time.Now()
	ch.recordSession("acme/api#issues/5", "A", "v", now, inA)
	ch.recordSession("widgets/web#issues/7", "B", "v", now, inB)

	// Send to A twice, then B twice → 2 mints (one each), 4 sends.
	for i := 0; i < 2; i++ {
		if _, err := ch.sendIssueComment(context.Background(), "acme/api#issues/5", "hi A"); err != nil {
			t.Fatalf("send A #%d: %v", i, err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := ch.sendIssueComment(context.Background(), "widgets/web#issues/7", "hi B"); err != nil {
			t.Fatalf("send B #%d: %v", i, err)
		}
	}

	if got := mintCalls.Load(); got != 2 {
		t.Errorf("mint called %d times, want 2 (one per installation, reused within TTL)", got)
	}
	// Each installation's token cache holds its own token.
	if inA.token != "tokenA" {
		t.Errorf("installation A token = %q, want tokenA", inA.token)
	}
	if inB.token != "tokenB" {
		t.Errorf("installation B token = %q, want tokenB", inB.token)
	}
}

// TestMultiInstall_SenderAllowlist_PerInstallation — sender
// allowlists scope per-installation: vadim allowed on A but not
// B, alice the reverse. A's webhook from alice (not on A's list)
// is dropped; B's webhook from alice is accepted.
func TestMultiInstall_SenderAllowlist_PerInstallation(t *testing.T) {
	cfg := Config{
		WebhookSecret: "shhh",
		Installations: []InstallationConfig{
			{
				ProjectID:       "project-A",
				InstallationID:  100,
				RepoAllowlist:   []string{"acme/api"},
				SenderAllowlist: []string{"vadim"},
			},
			{
				ProjectID:       "project-B",
				InstallationID:  200,
				RepoAllowlist:   []string{"widgets/web"},
				SenderAllowlist: []string{"alice"},
			},
		},
	}
	ch, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rx := &recordingReceiver{}
	ch.recvMu.Lock()
	ch.recv = rx
	ch.recvMu.Unlock()

	// Alice mentions @vornik on project-A's repo — A's allowlist
	// doesn't include alice → dropped.
	bodyA := []byte(`{
		"action": "created",
		"repository": {"full_name": "acme/api"},
		"sender": {"login": "alice"},
		"installation": {"id": 100},
		"issue": {"number": 1, "title": "x"},
		"comment": {"id": 1, "body": "@vornik hi", "user": {"login": "alice"}}
	}`)
	ch.HandleWebhook(httptest.NewRecorder(), signedRequest("shhh", "issue_comment", "d-A-alice", bodyA))

	// Alice mentions on project-B's repo — B's allowlist includes
	// alice → accepted.
	bodyB := []byte(`{
		"action": "created",
		"repository": {"full_name": "widgets/web"},
		"sender": {"login": "alice"},
		"installation": {"id": 200},
		"issue": {"number": 1, "title": "y"},
		"comment": {"id": 2, "body": "@vornik hi", "user": {"login": "alice"}}
	}`)
	ch.HandleWebhook(httptest.NewRecorder(), signedRequest("shhh", "issue_comment", "d-B-alice", bodyB))

	got := rx.copyGot()
	if len(got) != 1 {
		t.Fatalf("Receiver got %d messages, want 1 (only the B-alice mention)", len(got))
	}
	if got[0].SessionID != "widgets/web#issues/1" {
		t.Errorf("Receiver got session %q, want widgets/web#issues/1", got[0].SessionID)
	}
}

// TestSend_MultiInstall_UnknownSession_ErrorsCleanly — outbound
// Send for a SessionID the channel has never recorded must
// surface an explicit error rather than blasting through the
// wrong installation.
func TestSend_MultiInstall_UnknownSession_ErrorsCleanly(t *testing.T) {
	tcA := &stubTaskCreator{}
	tcB := &stubTaskCreator{}
	ch, _ := New(multiInstallConfig(t, tcA, tcB))

	// Make outbound credentials valid on at least one installation so
	// the error is from the session lookup, not the missing creds.
	ch.installations[0].appID = 1
	ch.installations[0].privateKey, _ = rsa.GenerateKey(rand.Reader, 2048)
	ch.installations[1].appID = 2
	ch.installations[1].privateKey, _ = rsa.GenerateKey(rand.Reader, 2048)

	_, err := ch.sendIssueComment(context.Background(), "unknown/repo#issues/42", "hi")
	if err == nil {
		t.Fatal("Send to unknown session returned nil, want explicit error")
	}
	if !strings.Contains(err.Error(), "no installation pinned") {
		t.Errorf("err = %v, want no-installation-pinned failure", err)
	}
}

// itoa is a tiny strconv.Itoa-style helper for the test stub.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
