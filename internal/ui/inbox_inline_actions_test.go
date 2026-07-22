// Package ui: tests for task 4.3 — the "Needs your attention" queue's
// inline actions (design §5.6/§5.7). Covers the HTMX fragment-render
// path added to TaskConversationAction (approve/reject/answer) and
// TaskRetry, plus the inbox.html row rendering (classification,
// checkpoint payload threading, fix-it link).
package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// newStatefulTaskRepo is a MockTaskRepository whose Get/TransitionConditional/
// RequeueTerminalTask all read and mutate the SAME *persistence.Task, so a
// test can fire two requests back-to-back and have the second one observe
// the state the first one produced — the shape needed to pin the §7
// idempotency contract (a stale double-click re-renders current state, no
// double-flip) rather than just asserting each call in isolation.
func newStatefulTaskRepo(task *persistence.Task) *mocks.MockTaskRepository {
	repo := &mocks.MockTaskRepository{}
	repo.GetFunc = func(_ context.Context, _ string) (*persistence.Task, error) {
		cp := *task
		return &cp, nil
	}
	repo.TransitionConditionalFunc = func(_ context.Context, _ string, from []persistence.TaskStatus, to persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
		for _, f := range from {
			if f == task.Status {
				task.Status = to
				return true, nil
			}
		}
		return false, nil
	}
	repo.RequeueTerminalTaskFunc = func(_ context.Context, _ string, _, _ int) (bool, error) {
		switch task.Status {
		case persistence.TaskStatusFailed, persistence.TaskStatusCancelled,
			persistence.TaskStatusCompleted, persistence.TaskStatusPending:
			task.Status = persistence.TaskStatusQueued
			return true, nil
		default:
			return false, nil
		}
	}
	return repo
}

// TestTaskRetry_HX_RendersRetryingRow pins the honest-retry behaviour
// (fix/retry/dismiss design 2026-07-22): an inline Retry requeues the task AND
// keeps the row visible as an informational "Retrying…" row, instead of the
// requeue emptying the fragment (row removed) as if the failure were resolved.
func TestTaskRetry_HX_RendersRetryingRow(t *testing.T) {
	task := &persistence.Task{ID: "t1", ProjectID: "p1", Status: persistence.TaskStatusFailed, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	repo := newStatefulTaskRepo(task)
	srv := NewServer(WithTaskRepository(repo))

	req := httptest.NewRequest(http.MethodPost, "/ui/tasks/t1/retry", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	srv.TaskRetry(rec, req, "t1")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if task.Status != persistence.TaskStatusQueued {
		t.Fatalf("retry must requeue the task; got %s", task.Status)
	}
	body := rec.Body.String()
	if body == "" {
		t.Fatal("retry must render the Retrying row in place, not an empty (removed) fragment")
	}
	if !strings.Contains(body, "Retrying") {
		t.Errorf("expected the informational Retrying row, got:\n%s", body)
	}
	if strings.Contains(body, "/ui/tasks/t1/retry") || strings.Contains(body, "/ui/tasks/t1/close") {
		t.Error("the Retrying row must be buttonless (no inline retry/close action)")
	}
}

// --- TaskConversationAction: HTMX fragment path (approve/reject) ----

func TestTaskConversationAction_HXApprove_ReturnsFragmentAndFlipsState(t *testing.T) {
	task := &persistence.Task{ID: "t1", ProjectID: "p1", Status: persistence.TaskStatusAwaitingApproval, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	repo := newStatefulTaskRepo(task)
	srv := NewServer(WithTaskRepository(repo), WithTaskMessageRepository(&uiTcStubMsgRepo{}))

	sub, unsub := srv.sseBus.Subscribe("t1")
	defer unsub()

	req := httptest.NewRequest(http.MethodPost, "/tasks/t1/approve", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	srv.TaskConversationAction(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (fragment, not a redirect)", rec.Code)
	}
	if rec.Header().Get("Location") != "" {
		t.Error("HX-Request response must not carry a Location header")
	}
	if task.Status != persistence.TaskStatusQueued {
		t.Errorf("approve must flip AWAITING_APPROVAL -> QUEUED via the reused uiApproveTask/uiSimpleFlip logic; got %s", task.Status)
	}
	// The row resolved out of the attention set (no longer AWAITING_APPROVAL)
	// — hx-swap="outerHTML" against an empty body removes it from the DOM.
	if body := rec.Body.String(); body != "" {
		t.Errorf("resolved row should render empty (removed), got %q", body)
	}
	select {
	case ev := <-sub.Events():
		if ev.Kind != "status" {
			t.Errorf("SSE event kind = %q, want status", ev.Kind)
		}
	default:
		t.Error("expected an SSEEvent published on approve")
	}
}

func TestTaskConversationAction_HXReject_ReturnsFragmentAndFlipsState(t *testing.T) {
	task := &persistence.Task{ID: "t1", ProjectID: "p1", Status: persistence.TaskStatusAwaitingApproval, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	repo := newStatefulTaskRepo(task)
	srv := NewServer(WithTaskRepository(repo), WithTaskMessageRepository(&uiTcStubMsgRepo{}))

	req := httptest.NewRequest(http.MethodPost, "/tasks/t1/reject", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	srv.TaskConversationAction(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if task.Status != persistence.TaskStatusCancelled {
		t.Errorf("reject must flip AWAITING_APPROVAL -> CANCELLED; got %s", task.Status)
	}
	if body := rec.Body.String(); body != "" {
		t.Errorf("resolved row should render empty, got %q", body)
	}
}

// TestTaskConversationAction_NonHTMX_Still303 pins backward compat: a
// caller that doesn't send HX-Request (curl, the old plain <form> POST)
// keeps getting the original POST-redirect-GET behavior.
func TestTaskConversationAction_NonHTMX_Still303(t *testing.T) {
	task := &persistence.Task{ID: "t1", ProjectID: "p1", Status: persistence.TaskStatusAwaitingApproval}
	repo := newStatefulTaskRepo(task)
	srv := NewServer(WithTaskRepository(repo), WithTaskMessageRepository(&uiTcStubMsgRepo{}))

	req := httptest.NewRequest(http.MethodPost, "/tasks/t1/approve", nil)
	rec := httptest.NewRecorder()
	srv.TaskConversationAction(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("non-HTMX caller: want 303, got %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Location"), "/ui/tasks/t1") {
		t.Errorf("location = %q", rec.Header().Get("Location"))
	}
}

// TestTaskConversationAction_HXApprove_StaleDoubleClickNoDoubleFlip is the
// §7 idempotency pin: a second approve after the task already left
// AWAITING_APPROVAL must not re-flip it, and must still respond
// gracefully (200, current-state fragment) rather than erroring.
func TestTaskConversationAction_HXApprove_StaleDoubleClickNoDoubleFlip(t *testing.T) {
	task := &persistence.Task{ID: "t1", ProjectID: "p1", Status: persistence.TaskStatusAwaitingApproval, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	repo := newStatefulTaskRepo(task)
	srv := NewServer(WithTaskRepository(repo), WithTaskMessageRepository(&uiTcStubMsgRepo{}))

	doApprove := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/tasks/t1/approve", nil)
		req.Header.Set("HX-Request", "true")
		rec := httptest.NewRecorder()
		srv.TaskConversationAction(rec, req)
		return rec
	}

	rec1 := doApprove()
	if rec1.Code != http.StatusOK || task.Status != persistence.TaskStatusQueued {
		t.Fatalf("first approve should flip to QUEUED; status=%d task=%s", rec1.Code, task.Status)
	}
	rec2 := doApprove()
	if rec2.Code != http.StatusOK {
		t.Fatalf("stale second approve should still 200 gracefully, got %d", rec2.Code)
	}
	if task.Status != persistence.TaskStatusQueued {
		t.Errorf("stale approve must not move the task off QUEUED (no double-flip); got %s", task.Status)
	}
	if body := rec2.Body.String(); body != "" {
		t.Errorf("stale approve's re-render should show current (resolved) state — empty row, got %q", body)
	}
}

// --- TaskConversationAction: HTMX fragment path (answer) -------------

func TestTaskConversationAction_HXAnswerDecision_FlipsAndReturnsFragment(t *testing.T) {
	open := "cp1"
	task := &persistence.Task{ID: "t1", ProjectID: "p1", Status: persistence.TaskStatusAwaitingInput, OpenCheckpointID: &open, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	repo := newStatefulTaskRepo(task)
	msgRepo := &uiTcStubMsgRepo{
		checkpoint: &persistence.TaskMessage{ID: "cp1", Metadata: []byte(`{"kind":"decision","options":[{"id":"go","label":"Go ahead"}]}`)},
	}
	srv := NewServer(WithTaskRepository(repo), WithTaskMessageRepository(msgRepo))

	form := url.Values{"checkpoint_id": {"cp1"}, "choice": {"go"}}
	req := httptest.NewRequest(http.MethodPost, "/tasks/t1/answer", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	srv.TaskConversationAction(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if task.Status != persistence.TaskStatusQueued {
		t.Errorf("answer must flip AWAITING_INPUT -> QUEUED via uiAnswerCheckpoint; got %s", task.Status)
	}
	if len(msgRepo.inserts) != 1 || msgRepo.inserts[0].Content != "Go ahead" {
		t.Errorf("expected the chosen option's label recorded as the answer content; inserts=%+v", msgRepo.inserts)
	}
}

// TestTaskConversationAction_HXAnswer_NoOpenCheckpoint_Graceful is the §7
// "answer with no open checkpoint" case: uiAnswerCheckpoint returns
// missing-checkpoint (no state change), and the row must still re-render
// (200, current state) rather than erroring — and without an answer
// control since there's no open checkpoint to answer.
func TestTaskConversationAction_HXAnswer_NoOpenCheckpoint_Graceful(t *testing.T) {
	task := &persistence.Task{ID: "t1", ProjectID: "p1", Status: persistence.TaskStatusAwaitingInput, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	repo := newStatefulTaskRepo(task)
	msgRepo := &uiTcStubMsgRepo{} // no open checkpoint configured
	srv := NewServer(WithTaskRepository(repo), WithTaskMessageRepository(msgRepo))

	req := httptest.NewRequest(http.MethodPost, "/tasks/t1/answer", strings.NewReader(url.Values{}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	srv.TaskConversationAction(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (graceful, no panic)", rec.Code)
	}
	if task.Status != persistence.TaskStatusAwaitingInput {
		t.Errorf("no-op answer must not change task status; got %s", task.Status)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Needs input") {
		t.Errorf("row should still render (task remains actionable), got %q", body)
	}
	if strings.Contains(body, `name="choice"`) {
		t.Errorf("no open checkpoint means no answer control should render, got %q", body)
	}
}

// TestTaskConversationAction_HXAnswer_ConcurrentStaleAnswerNoDoubleEffect
// is the task 4.3 review nit folded into 4.4: two callers (e.g. two open
// browser tabs, or a slow double-submit) share the same task state. After
// caller A's answer resolves the checkpoint (AWAITING_INPUT -> QUEUED),
// caller B's answer to the SAME now-closed checkpoint must degrade
// gracefully (no error, no re-transition) and — the "no double-effect"
// half of the nit — must NOT insert a second answer message. The
// TransitionConditional guard mocks a real DB's atomic CAS: a from-set of
// [AWAITING_INPUT] only matches once.
func TestTaskConversationAction_HXAnswer_ConcurrentStaleAnswerNoDoubleEffect(t *testing.T) {
	open := "cp1"
	task := &persistence.Task{ID: "t1", ProjectID: "p1", Status: persistence.TaskStatusAwaitingInput, OpenCheckpointID: &open, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	repo := newStatefulTaskRepo(task)
	msgRepo := &uiTcStubMsgRepo{
		checkpoint: &persistence.TaskMessage{ID: "cp1", Metadata: []byte(`{"kind":"decision","options":[{"id":"go","label":"Go ahead"}]}`)},
	}
	srv := NewServer(WithTaskRepository(repo), WithTaskMessageRepository(msgRepo))

	answer := func() *httptest.ResponseRecorder {
		form := url.Values{"checkpoint_id": {"cp1"}, "choice": {"go"}}
		req := httptest.NewRequest(http.MethodPost, "/tasks/t1/answer", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		rec := httptest.NewRecorder()
		srv.TaskConversationAction(rec, req)
		return rec
	}

	// Caller A: answers first, flips the task to QUEUED.
	recA := answer()
	if recA.Code != http.StatusOK {
		t.Fatalf("caller A: status = %d, want 200", recA.Code)
	}
	if task.Status != persistence.TaskStatusQueued {
		t.Fatalf("caller A's answer must flip AWAITING_INPUT -> QUEUED; got %s", task.Status)
	}
	if len(msgRepo.inserts) != 1 {
		t.Fatalf("caller A: expected exactly 1 answer message inserted, got %d", len(msgRepo.inserts))
	}

	// Caller B: answers the SAME checkpoint after A already resolved it.
	recB := answer()
	if recB.Code != http.StatusOK {
		t.Errorf("caller B's stale answer should still 200 gracefully, got %d", recB.Code)
	}
	if task.Status != persistence.TaskStatusQueued {
		t.Errorf("caller B's stale answer must not move the task off QUEUED (no double-flip); got %s", task.Status)
	}
	if len(msgRepo.inserts) != 1 {
		t.Errorf("caller B's stale answer must not insert a second answer message (no double-effect); got %d inserts", len(msgRepo.inserts))
	}
	if body := recB.Body.String(); body != "" {
		t.Errorf("caller B's stale-answer re-render should show current (resolved, no-open-checkpoint) state — empty row, got %q", body)
	}
}

// TestTaskConversationAction_HXRequest_ScopeGate_Rejected — the scope
// gate runs before the HTMX branch, so a cross-project HX-Request caller
// gets the same 404 a plain caller would (never a leaked fragment).
func TestTaskConversationAction_HXRequest_ScopeGate_Rejected(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, ProjectID: "p2", Status: persistence.TaskStatusAwaitingApproval}, nil
		},
	}
	srv := &Server{taskRepo: taskRepo, taskMessageRepo: &uiTcStubMsgRepo{}}

	req := scopedReq(http.MethodPost, "/tasks/t1/approve", "p1", "")
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	srv.TaskConversationAction(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-project HX-Request action: want 404, got %d", rec.Code)
	}
}

// --- TaskRetry: HTMX fragment path ------------------------------------

func TestTaskRetry_HXRequest_ReturnsFragmentAndPublishesSSE(t *testing.T) {
	task := &persistence.Task{ID: "t1", ProjectID: "p1", Status: persistence.TaskStatusFailed, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	repo := newStatefulTaskRepo(task)
	srv := NewServer(WithTaskRepository(repo))

	sub, unsub := srv.sseBus.Subscribe("t1")
	defer unsub()

	req := httptest.NewRequest(http.MethodPost, "/tasks/t1/retry", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	srv.TaskRetry(rec, req, "t1")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (fragment, not a redirect)", rec.Code)
	}
	if task.Status != persistence.TaskStatusQueued {
		t.Errorf("retry must requeue the failed task; got %s", task.Status)
	}
	// Honest retry (fix/retry/dismiss design 2026-07-22): the row STAYS as an
	// informational "Retrying…" row — it does NOT render empty (removed) as if
	// the failure were resolved. (Row-content specifics are pinned by
	// TestTaskRetry_HX_RendersRetryingRow.)
	if body := rec.Body.String(); !strings.Contains(body, "Retrying") {
		t.Errorf("retried row should render the Retrying state, got %q", body)
	}
	select {
	case ev := <-sub.Events():
		if ev.Kind != "status" || ev.Data != "QUEUED" {
			t.Errorf("unexpected SSE event: %+v", ev)
		}
	default:
		t.Error("expected an SSEEvent published on retry")
	}
}

// TestTaskRetry_HXRequest_NotRetriable_GracefulFragment — a task not in a
// retriable state must still respond gracefully via the fragment path
// (current state re-rendered, no error), mirroring the non-HTMX
// task-not-retriable notice.
func TestTaskRetry_HXRequest_NotRetriable_GracefulFragment(t *testing.T) {
	task := &persistence.Task{ID: "t1", ProjectID: "p1", Status: persistence.TaskStatusRunning, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	repo := newStatefulTaskRepo(task)
	srv := NewServer(WithTaskRepository(repo))

	req := httptest.NewRequest(http.MethodPost, "/tasks/t1/retry", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	srv.TaskRetry(rec, req, "t1")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if task.Status != persistence.TaskStatusRunning {
		t.Errorf("not-retriable task must be untouched; got %s", task.Status)
	}
}

// TestTaskRetry_ScopeGate_HXRequest_Rejected — scope check now runs up
// front (task 4.3 refactor) so a cross-project retry attempt 404s before
// RequeueTerminalTask is ever called, HTMX or not.
func TestTaskRetry_ScopeGate_HXRequest_Rejected(t *testing.T) {
	requeued := false
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, ProjectID: "p2", Status: persistence.TaskStatusFailed}, nil
		},
		RequeueTerminalTaskFunc: func(_ context.Context, _ string, _, _ int) (bool, error) {
			requeued = true
			return true, nil
		},
	}
	srv := NewServer(WithTaskRepository(repo))

	req := scopedReq(http.MethodPost, "/tasks/t1/retry", "p1", "")
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	srv.TaskRetry(rec, req, "t1")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if requeued {
		t.Error("scope-mismatched retry must not requeue the foreign task")
	}
}

// --- renderInboxItemFragment: direct unit coverage --------------------

func TestRenderInboxItemFragment_NoTaskRepo(t *testing.T) {
	srv := &Server{}
	rec := httptest.NewRecorder()
	srv.renderInboxItemFragment(context.Background(), rec, "t1")
	if rec.Body.Len() != 0 {
		t.Errorf("expected empty body when taskRepo is nil, got %q", rec.Body.String())
	}
}

func TestRenderInboxItemFragment_TaskNotFound(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) { return nil, nil },
	}
	srv := NewServer(WithTaskRepository(repo))
	rec := httptest.NewRecorder()
	srv.renderInboxItemFragment(context.Background(), rec, "gone")
	if rec.Body.Len() != 0 {
		t.Errorf("expected empty body for a task that no longer exists, got %q", rec.Body.String())
	}
}

// TestRenderInboxItemFragment_CheckpointNoMetadata — an open checkpoint
// with no parsable payload (malformed / never populated) must not crash
// the fragment render; the row still shows, just without an answer
// control (there's nothing to build one from).
func TestRenderInboxItemFragment_CheckpointNoMetadata(t *testing.T) {
	open := "cp1"
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return &persistence.Task{ID: "t1", ProjectID: "p1", Status: persistence.TaskStatusAwaitingInput, OpenCheckpointID: &open}, nil
		},
	}
	msgRepo := &uiTcStubMsgRepo{checkpoint: &persistence.TaskMessage{ID: "cp1"}} // no Metadata
	srv := NewServer(WithTaskRepository(repo), WithTaskMessageRepository(msgRepo))
	rec := httptest.NewRecorder()
	srv.renderInboxItemFragment(context.Background(), rec, "t1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Needs input") {
		t.Errorf("row should still render, got %q", body)
	}
	if strings.Contains(body, `name="choice"`) || strings.Contains(body, `name="content"`) {
		t.Errorf("no checkpoint payload means no answer control should render, got %q", body)
	}
}

// TestRenderInboxItemFragment_FailedAgedPastWindow — a FAILED task whose
// UpdatedAt fell outside the 24h recency window between the triggering
// action and this re-render is treated exactly like Inbox() treats it:
// excluded, empty fragment (row removed).
func TestRenderInboxItemFragment_FailedAgedPastWindow(t *testing.T) {
	stale := time.Now().Add(-48 * time.Hour)
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return &persistence.Task{ID: "t1", ProjectID: "p1", Status: persistence.TaskStatusFailed, CreatedAt: stale, UpdatedAt: stale}, nil
		},
	}
	srv := NewServer(WithTaskRepository(repo))
	rec := httptest.NewRecorder()
	srv.renderInboxItemFragment(context.Background(), rec, "t1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("expected empty body for a FAILED task aged past the 24h window, got %q", rec.Body.String())
	}
}

func TestByStatusCategory(t *testing.T) {
	if _, ok := byStatusCategory(persistence.TaskStatusAwaitingExternal); ok {
		t.Error("AWAITING_EXTERNAL must not map to any inbox category")
	}
	c, ok := byStatusCategory(persistence.TaskStatusAwaitingApproval)
	if !ok || c.kind != inboxKindNeedsApproval {
		t.Errorf("got %+v, ok=%v", c, ok)
	}
}

func TestNewAttentionItem_FailedGetsFixItURL(t *testing.T) {
	now := time.Now()
	task := &persistence.Task{ID: "t1", Status: persistence.TaskStatusFailed, CreatedAt: now, UpdatedAt: now}
	c, ok := byStatusCategory(persistence.TaskStatusFailed)
	if !ok {
		t.Fatal("expected a Failed category")
	}
	item, ok := newAttentionItem(task, c, now.Add(-24*time.Hour))
	if !ok {
		t.Fatal("expected ok=true for a fresh failure")
	}
	if item.FixItURL != "/ui/fixit/failed_task/t1" {
		t.Errorf("FixItURL = %q, want /ui/fixit/failed_task/t1", item.FixItURL)
	}
}

// --- Inbox() page: inline controls rendered in the pinned queue -------

func TestInbox_ApprovalItemHasInlineApproveRejectForms(t *testing.T) {
	now := time.Now()
	seed := []*persistence.Task{
		{ID: "t-appr", ProjectID: "p1", Status: persistence.TaskStatusAwaitingApproval, CreatedAt: now, UpdatedAt: now},
	}
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			return mocks.FilterTasks(seed, f), nil
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo))
	rec := httptest.NewRecorder()
	srv.Inbox(rec, httptest.NewRequest(http.MethodGet, "/ui/inbox", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `hx-post="/ui/tasks/t-appr/approve"`) {
		t.Errorf("expected inline approve form:\n%s", body)
	}
	if !strings.Contains(body, `hx-post="/ui/tasks/t-appr/reject"`) {
		t.Errorf("expected inline reject form:\n%s", body)
	}
}

func TestInbox_FailedItemHasFixItLinkAndRetryButton(t *testing.T) {
	now := time.Now()
	seed := []*persistence.Task{
		{ID: "t-failed", ProjectID: "p1", Status: persistence.TaskStatusFailed, CreatedAt: now, UpdatedAt: now},
	}
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			return mocks.FilterTasks(seed, f), nil
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo))
	rec := httptest.NewRecorder()
	srv.Inbox(rec, httptest.NewRequest(http.MethodGet, "/ui/inbox", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `href="/ui/fixit/failed_task/t-failed"`) {
		t.Errorf("expected fix-it deep link in body:\n%s", body)
	}
	if !strings.Contains(body, "Help me fix this") {
		t.Error("expected 'Help me fix this' label")
	}
	if !strings.Contains(body, `hx-post="/ui/tasks/t-failed/retry"`) {
		t.Error("expected inline retry form action")
	}
}

func TestInbox_CheckpointItemRendersDecisionOptions(t *testing.T) {
	now := time.Now()
	seed := []*persistence.Task{
		{ID: "t-cp", ProjectID: "p1", Status: persistence.TaskStatusAwaitingInput, CreatedAt: now, UpdatedAt: now},
	}
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			return mocks.FilterTasks(seed, f), nil
		},
	}
	msgRepo := &uiTcStubMsgRepo{
		checkpoint: &persistence.TaskMessage{ID: "cp9", Metadata: []byte(`{"kind":"decision","options":[{"id":"a","label":"Option A"}]}`)},
	}
	srv := NewServer(WithTaskRepository(taskRepo), WithTaskMessageRepository(msgRepo))
	rec := httptest.NewRecorder()
	srv.Inbox(rec, httptest.NewRequest(http.MethodGet, "/ui/inbox", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "Option A") {
		t.Errorf("expected the decision option's label rendered:\n%s", body)
	}
	if !strings.Contains(body, `value="cp9"`) {
		t.Error("expected the hidden checkpoint_id field to carry the open checkpoint's ID")
	}
}

func TestInbox_CheckpointActionRequiredRendersTextInput(t *testing.T) {
	now := time.Now()
	seed := []*persistence.Task{
		{ID: "t-cp", ProjectID: "p1", Status: persistence.TaskStatusAwaitingInput, CreatedAt: now, UpdatedAt: now},
	}
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			return mocks.FilterTasks(seed, f), nil
		},
	}
	msgRepo := &uiTcStubMsgRepo{
		checkpoint: &persistence.TaskMessage{ID: "cp9", Metadata: []byte(`{"kind":"action_required","question":"What now?"}`)},
	}
	srv := NewServer(WithTaskRepository(taskRepo), WithTaskMessageRepository(msgRepo))
	rec := httptest.NewRecorder()
	srv.Inbox(rec, httptest.NewRequest(http.MethodGet, "/ui/inbox", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `name="content"`) {
		t.Errorf("expected a free-text answer input for an action_required checkpoint:\n%s", body)
	}
	if strings.Contains(body, `name="choice"`) {
		t.Error("action_required checkpoint must not render decision option buttons")
	}
}
