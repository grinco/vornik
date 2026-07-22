package ui

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/persistence"
)

// uiFakeWebWriteRepo is a small in-memory persistence.WebWriteRepo (plus the
// persistence.WebWritePendingLister query) for the inbox approval tests. It
// records the Approve/Reject calls so a test can assert exactly what the
// handler minted and whether a mutation fired at all. Deliberately local to the
// ui test package (the dispatcher package's fake is package-private there).
type uiFakeWebWriteRepo struct {
	mu   sync.Mutex
	rows map[string]*persistence.WebWriteAction

	approveCalls  int
	approveHashes []string
	approveActor  []string
	rejectCalls   int
	rejectActor   []string
}

func newUIFakeWebWriteRepo() *uiFakeWebWriteRepo {
	return &uiFakeWebWriteRepo{rows: map[string]*persistence.WebWriteAction{}}
}

func (r *uiFakeWebWriteRepo) put(a *persistence.WebWriteAction) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *a
	r.rows[a.SubmissionID] = &cp
}

func (r *uiFakeWebWriteRepo) Create(_ context.Context, a *persistence.WebWriteAction) error {
	r.put(a)
	return nil
}

func (r *uiFakeWebWriteRepo) Get(_ context.Context, submissionID string) (*persistence.WebWriteAction, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.rows[submissionID]
	if !ok {
		return nil, sql.ErrNoRows
	}
	cp := *a
	return &cp, nil
}

func (r *uiFakeWebWriteRepo) Approve(_ context.Context, submissionID, tokenHash, approver string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.rows[submissionID]
	if !ok || a.Status != "pending" {
		return persistence.ErrNoTransition
	}
	a.Status = "approved"
	a.ApprovalTokenHash = tokenHash
	a.Approver = approver
	a.DecidedAt = sql.NullTime{Time: time.Now().UTC(), Valid: true}
	r.approveCalls++
	r.approveHashes = append(r.approveHashes, tokenHash)
	r.approveActor = append(r.approveActor, approver)
	return nil
}

func (r *uiFakeWebWriteRepo) CASToSubmitting(_ context.Context, _ string) (bool, error) {
	return false, nil
}

func (r *uiFakeWebWriteRepo) Finalize(_ context.Context, _, _ string) error {
	return nil
}

func (r *uiFakeWebWriteRepo) Resolve(_ context.Context, _, _ string) error {
	return nil
}

func (r *uiFakeWebWriteRepo) Reject(_ context.Context, submissionID, approver string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.rows[submissionID]
	if !ok || (a.Status != "pending" && a.Status != "approved") {
		return persistence.ErrNoTransition
	}
	a.Status = "rejected"
	a.Approver = approver
	r.rejectCalls++
	r.rejectActor = append(r.rejectActor, approver)
	return nil
}

func (r *uiFakeWebWriteRepo) ListPendingByProject(_ context.Context, projectIDs []string) ([]*persistence.WebWriteAction, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	allow := map[string]bool{}
	for _, id := range projectIDs {
		allow[id] = true
	}
	var out []*persistence.WebWriteAction
	for _, a := range r.rows {
		if a.Status != "pending" {
			continue
		}
		if len(allow) > 0 && !allow[a.ProjectID] {
			continue
		}
		cp := *a
		out = append(out, &cp)
	}
	return out, nil
}

// pendingWebWriteFixture is a canned pending row with a two-field table
// (one agent-bound, one volatile) for the render + action tests.
func pendingWebWriteFixture() *persistence.WebWriteAction {
	return &persistence.WebWriteAction{
		SubmissionID:   "ww_test_1",
		ProjectID:      "p1",
		AgentRunID:     "run_abc",
		TargetURL:      "https://claims.airline.com/submit?ref=42",
		TargetHost:     "claims.airline.com",
		ScreenshotRef:  "", // exercise the placeholder path
		FieldTableJSON: []byte(`[{"name":"passenger","label":"Passenger name","value":"Ada Lovelace","provenance":"agent-bound","bound":true},{"name":"csrf_token","label":"","value":"xyz","provenance":"volatile","bound":false}]`),
		Status:         "pending",
		CreatedAt:      time.Now().Add(-3 * time.Minute),
	}
}

// sameOriginPost builds a mutating request that carries a trustworthy
// same-origin signal (Sec-Fetch-Site: same-origin) so the in-handler CSRF gate
// admits it — the browser-equivalent of the inbox form's own-origin POST.
func sameOriginPost(target string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, target, nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	return req
}

func TestInboxWebWriteApprove_GETRejected(t *testing.T) {
	repo := newUIFakeWebWriteRepo()
	repo.put(pendingWebWriteFixture())
	var delivered int
	srv := NewServer(
		WithWebWriteRepo(repo),
		WithWebWriteApprovalDeliver(func(_, _, _ string) error { delivered++; return nil }),
	)

	rec := httptest.NewRecorder()
	// A GET must never mint a capability, even with a same-origin signal.
	req := httptest.NewRequest(http.MethodGet, "/ui/inbox/web-write/ww_test_1/approve", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	srv.WebWriteApprove(rec, req, "ww_test_1")

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET approve status = %d, want 405", rec.Code)
	}
	if repo.approveCalls != 0 {
		t.Errorf("GET approve called Approve %d times, want 0", repo.approveCalls)
	}
	if delivered != 0 {
		t.Errorf("GET approve delivered token %d times, want 0", delivered)
	}
}

func TestInboxWebWriteApprove_POSTNoCSRFRejected(t *testing.T) {
	repo := newUIFakeWebWriteRepo()
	repo.put(pendingWebWriteFixture())
	var delivered int
	srv := NewServer(
		WithWebWriteRepo(repo),
		WithWebWriteApprovalDeliver(func(_, _, _ string) error { delivered++; return nil }),
	)

	rec := httptest.NewRecorder()
	// POST with neither Sec-Fetch-Site nor Origin — no same-origin signal → fail closed.
	req := httptest.NewRequest(http.MethodPost, "/ui/inbox/web-write/ww_test_1/approve", nil)
	srv.WebWriteApprove(rec, req, "ww_test_1")

	if rec.Code != http.StatusForbidden {
		t.Fatalf("no-CSRF approve status = %d, want 403", rec.Code)
	}
	if repo.approveCalls != 0 {
		t.Errorf("no-CSRF approve called Approve %d times, want 0", repo.approveCalls)
	}
	if delivered != 0 {
		t.Errorf("no-CSRF approve delivered token %d times, want 0", delivered)
	}
}

func TestInboxWebWriteApprove_POSTWithCSRFMintsAndDelivers(t *testing.T) {
	repo := newUIFakeWebWriteRepo()
	row := pendingWebWriteFixture()
	repo.put(row)

	var (
		gotSubmission, gotAgentRun, gotToken string
		deliverCalls                         int
	)
	srv := NewServer(
		WithWebWriteRepo(repo),
		WithWebWriteApprovalDeliver(func(submissionID, agentRunID, token string) error {
			deliverCalls++
			gotSubmission, gotAgentRun, gotToken = submissionID, agentRunID, token
			return nil
		}),
	)

	rec := httptest.NewRecorder()
	srv.WebWriteApprove(rec, sameOriginPost("/ui/inbox/web-write/ww_test_1/approve"), "ww_test_1")

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("approve status = %d, want 303 (redirect to inbox)", rec.Code)
	}
	if repo.approveCalls != 1 {
		t.Fatalf("Approve called %d times, want 1", repo.approveCalls)
	}
	if len(repo.approveHashes) != 1 || strings.TrimSpace(repo.approveHashes[0]) == "" {
		t.Fatalf("Approve got empty token hash: %#v", repo.approveHashes)
	}
	if deliverCalls != 1 {
		t.Fatalf("deliver hook invoked %d times, want 1", deliverCalls)
	}
	if gotSubmission != "ww_test_1" || gotAgentRun != "run_abc" {
		t.Errorf("deliver got (submission=%q agentRun=%q), want (ww_test_1, run_abc)", gotSubmission, gotAgentRun)
	}
	if strings.TrimSpace(gotToken) == "" {
		t.Fatal("deliver got empty raw token")
	}
	// The delivered raw token must hash (bound to the row) to exactly the
	// stored approval_token_hash — this is the seam submit re-derives.
	wantHash := dispatcher.WebWriteApprovalTokenHash(gotToken, row)
	if repo.approveHashes[0] != wantHash {
		t.Errorf("stored token hash %q != hash(delivered token, row) %q", repo.approveHashes[0], wantHash)
	}
	// The raw token must NEVER appear in the operator-facing response.
	if strings.Contains(rec.Body.String(), gotToken) {
		t.Error("raw approval token leaked into the UI response body")
	}
	if loc := rec.Header().Get("Location"); loc != "" && strings.Contains(loc, gotToken) {
		t.Error("raw approval token leaked into the redirect Location")
	}
}

func TestInboxWebWriteApprove_NoDeliverHookStillPersists(t *testing.T) {
	repo := newUIFakeWebWriteRepo()
	repo.put(pendingWebWriteFixture())
	// No WithWebWriteApprovalDeliver — the Task-10 seam is unwired.
	srv := NewServer(WithWebWriteRepo(repo))

	rec := httptest.NewRecorder()
	srv.WebWriteApprove(rec, sameOriginPost("/ui/inbox/web-write/ww_test_1/approve"), "ww_test_1")

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("approve status = %d, want 303", rec.Code)
	}
	if repo.approveCalls != 1 {
		t.Errorf("Approve called %d times, want 1 (approval must persist even without a deliver hook)", repo.approveCalls)
	}
}

func TestInboxWebWriteReject_POST(t *testing.T) {
	repo := newUIFakeWebWriteRepo()
	repo.put(pendingWebWriteFixture())
	srv := NewServer(WithWebWriteRepo(repo))

	rec := httptest.NewRecorder()
	srv.WebWriteReject(rec, sameOriginPost("/ui/inbox/web-write/ww_test_1/reject"), "ww_test_1")

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("reject status = %d, want 303", rec.Code)
	}
	if repo.rejectCalls != 1 {
		t.Errorf("Reject called %d times, want 1", repo.rejectCalls)
	}
	if repo.approveCalls != 0 {
		t.Errorf("reject minted an approval (Approve called %d times)", repo.approveCalls)
	}
}

func TestInboxWebWriteReject_GETRejected(t *testing.T) {
	repo := newUIFakeWebWriteRepo()
	repo.put(pendingWebWriteFixture())
	srv := NewServer(WithWebWriteRepo(repo))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ui/inbox/web-write/ww_test_1/reject", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	srv.WebWriteReject(rec, req, "ww_test_1")

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET reject status = %d, want 405", rec.Code)
	}
	if repo.rejectCalls != 0 {
		t.Errorf("GET reject called Reject %d times, want 0", repo.rejectCalls)
	}
}

func TestInboxWebWriteRouter_DispatchesAndRejectsGET(t *testing.T) {
	repo := newUIFakeWebWriteRepo()
	repo.put(pendingWebWriteFixture())
	srv := NewServer(WithWebWriteRepo(repo))

	// Route a same-origin POST through the real router → approve mints once.
	rec := httptest.NewRecorder()
	srv.webWriteInboxRouter(rec, sameOriginPost("/ui/inbox/web-write/ww_test_1/approve"))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("router POST approve status = %d, want 303", rec.Code)
	}
	if repo.approveCalls != 1 {
		t.Fatalf("router POST approve called Approve %d times, want 1", repo.approveCalls)
	}
}

func TestInbox_RendersPendingWebWriteCard(t *testing.T) {
	repo := newUIFakeWebWriteRepo()
	repo.put(pendingWebWriteFixture())
	srv := NewServer(WithWebWriteRepo(repo))

	rec := httptest.NewRecorder()
	srv.Inbox(rec, httptest.NewRequest(http.MethodGet, "/ui/inbox", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("inbox status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	for _, want := range []string{
		"ww_test_1",                             // submission id
		"claims.airline.com",                    // target host
		"Passenger name",                        // field label from field_table_json
		"agent-bound",                           // provenance chip
		"volatile",                              // volatile field provenance
		"/ui/inbox/web-write/ww_test_1/approve", // approve action
		"/ui/inbox/web-write/ww_test_1/reject",  // reject action
	} {
		if !strings.Contains(body, want) {
			t.Errorf("inbox body missing %q", want)
		}
	}
	// The volatile field's actual value must NOT be rendered (value-exempt).
	if strings.Contains(body, ">xyz<") {
		t.Error("volatile field value rendered; it must be value-exempt")
	}
	// No screenshot ref → the placeholder must render, not a broken <img>.
	if !strings.Contains(body, "screenshot unavailable") {
		t.Error("expected screenshot placeholder for a row with no screenshot_ref")
	}
}
