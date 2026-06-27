// Package ui: tests for the per-action helpers under
// TaskConversationAction (uiAnswerCheckpoint / uiAmendBrief /
// uiSimpleFlip / uiCloseTask). Each returns a notice string the
// dispatcher embeds in the post-action redirect; the contract is
// "right notice for the right outcome, no panic on missing deps".
package ui

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// uiTcStubMsgRepo is the minimum TaskMessageRepository for these
// internal helpers — Insert + MarkCheckpointResolved. We use a
// per-test new instance so the inserts slice isn't shared.
type uiTcStubMsgRepo struct {
	insertErr  error
	resolveErr error
	inserts    []*persistence.TaskMessage
	checkpoint *persistence.TaskMessage
}

func (s *uiTcStubMsgRepo) Insert(_ context.Context, m *persistence.TaskMessage) error {
	if s.insertErr != nil {
		return s.insertErr
	}
	s.inserts = append(s.inserts, m)
	return nil
}
func (s *uiTcStubMsgRepo) List(_ context.Context, _ persistence.TaskMessageFilter) ([]*persistence.TaskMessage, error) {
	return nil, nil
}
func (s *uiTcStubMsgRepo) GetOpenCheckpoint(_ context.Context, _ string) (*persistence.TaskMessage, error) {
	return s.checkpoint, nil
}
func (s *uiTcStubMsgRepo) MarkCheckpointResolved(_ context.Context, _, _ string) error {
	return s.resolveErr
}

func makeFormRequest(form url.Values) *postRequest {
	return &postRequest{form: form}
}

// postRequest is a thin adapter that ParseForm-isn't needed: we
// build *http.Request directly via httptest with form-encoded body.
type postRequest struct{ form url.Values }

func uiActionRequest(form url.Values) *postRequest { return &postRequest{form: form} }

// --- uiAnswerCheckpoint ----------------------------------------------

func TestUIAnswerCheckpoint_MissingCheckpoint(t *testing.T) {
	srv := &Server{}
	task := &persistence.Task{ID: "t1", Status: persistence.TaskStatusAwaitingInput}
	req := httptest.NewRequest("POST", "/", strings.NewReader(url.Values{}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	got := srv.uiAnswerCheckpoint(context.Background(), task, req)
	if got != "missing-checkpoint" {
		t.Errorf("got %q", got)
	}
}

func TestUIAnswerCheckpoint_EmptyAnswer(t *testing.T) {
	srv := &Server{}
	task := &persistence.Task{ID: "t1", Status: persistence.TaskStatusAwaitingInput}
	form := url.Values{"checkpoint_id": []string{"cp1"}}
	req := httptest.NewRequest("POST", "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if got := srv.uiAnswerCheckpoint(context.Background(), task, req); got != "empty-answer" {
		t.Errorf("got %q", got)
	}
}

func TestUIAnswerCheckpoint_InvalidTransition(t *testing.T) {
	srv := &Server{}
	task := &persistence.Task{ID: "t1", Status: persistence.TaskStatusClosed}
	form := url.Values{"checkpoint_id": []string{"cp1"}, "content": []string{"yes"}}
	req := httptest.NewRequest("POST", "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if got := srv.uiAnswerCheckpoint(context.Background(), task, req); got != "checkpoint-stale" {
		t.Errorf("got %q", got)
	}
}

func TestUIAnswerCheckpoint_WrongCheckpointID(t *testing.T) {
	srv := &Server{}
	open := "cp-other"
	task := &persistence.Task{
		ID:               "t1",
		Status:           persistence.TaskStatusAwaitingInput,
		OpenCheckpointID: &open,
	}
	form := url.Values{"checkpoint_id": []string{"cp1"}, "content": []string{"yes"}}
	req := httptest.NewRequest("POST", "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if got := srv.uiAnswerCheckpoint(context.Background(), task, req); got != "checkpoint-resolved" {
		t.Errorf("got %q", got)
	}
}

func TestUIAnswerCheckpoint_HappyPath(t *testing.T) {
	open := "cp1"
	msgRepo := &uiTcStubMsgRepo{}
	taskRepo := &mocks.MockTaskRepository{
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			return true, nil
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo), WithTaskMessageRepository(msgRepo))
	task := &persistence.Task{
		ID:               "t1",
		Status:           persistence.TaskStatusAwaitingInput,
		OpenCheckpointID: &open,
	}
	form := url.Values{
		"checkpoint_id": []string{"cp1"},
		"content":       []string{"yes"},
		"choice":        []string{"approve"},
		"author":        []string{"alice"},
	}
	req := httptest.NewRequest("POST", "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if got := srv.uiAnswerCheckpoint(context.Background(), task, req); got != "answer-sent" {
		t.Errorf("got %q", got)
	}
	if len(msgRepo.inserts) != 1 {
		t.Fatalf("expected 1 inserted message; got %d", len(msgRepo.inserts))
	}
	if msgRepo.inserts[0].AuthorID == nil || *msgRepo.inserts[0].AuthorID != "alice" {
		t.Errorf("authorID: got %v", msgRepo.inserts[0].AuthorID)
	}
}

func TestUIAnswerCheckpoint_InsertError(t *testing.T) {
	open := "cp1"
	srv := NewServer(
		WithTaskMessageRepository(&uiTcStubMsgRepo{insertErr: errors.New("boom")}),
	)
	task := &persistence.Task{
		ID:               "t1",
		Status:           persistence.TaskStatusAwaitingInput,
		OpenCheckpointID: &open,
	}
	form := url.Values{"checkpoint_id": []string{"cp1"}, "content": []string{"yes"}}
	req := httptest.NewRequest("POST", "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if got := srv.uiAnswerCheckpoint(context.Background(), task, req); got != "answer-failed" {
		t.Errorf("got %q", got)
	}
}

func TestUIAnswerCheckpoint_TransitionFails(t *testing.T) {
	open := "cp1"
	taskRepo := &mocks.MockTaskRepository{
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			return false, nil
		},
	}
	srv := NewServer(
		WithTaskRepository(taskRepo),
		WithTaskMessageRepository(&uiTcStubMsgRepo{}),
	)
	task := &persistence.Task{
		ID:               "t1",
		Status:           persistence.TaskStatusAwaitingInput,
		OpenCheckpointID: &open,
	}
	form := url.Values{"checkpoint_id": []string{"cp1"}, "content": []string{"yes"}}
	req := httptest.NewRequest("POST", "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if got := srv.uiAnswerCheckpoint(context.Background(), task, req); got != "checkpoint-stale" {
		t.Errorf("got %q", got)
	}
}

// --- uiAmendBrief ----------------------------------------------------

func TestUIAmendBrief_EmptyNewBrief(t *testing.T) {
	srv := &Server{}
	task := &persistence.Task{ID: "t1", Status: persistence.TaskStatusAwaitingInput}
	form := url.Values{}
	req := httptest.NewRequest("POST", "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if got := srv.uiAmendBrief(context.Background(), task, req); got != "empty-amend" {
		t.Errorf("got %q", got)
	}
}

func TestUIAmendBrief_TerminalTask(t *testing.T) {
	srv := &Server{}
	task := &persistence.Task{ID: "t1", Status: persistence.TaskStatusClosed}
	form := url.Values{"new_brief": []string{"new"}}
	req := httptest.NewRequest("POST", "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if got := srv.uiAmendBrief(context.Background(), task, req); got != "task-terminal" {
		t.Errorf("got %q", got)
	}
}

func TestUIAmendBrief_HappyPath(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			return true, nil
		},
	}
	msgRepo := &uiTcStubMsgRepo{}
	srv := NewServer(
		WithTaskRepository(taskRepo),
		WithTaskMessageRepository(msgRepo),
	)
	task := &persistence.Task{ID: "t1", Status: persistence.TaskStatusAwaitingInput}
	form := url.Values{
		"new_brief": []string{"do better"},
		"reason":    []string{"misread requirements"},
		"author":    []string{"bob"},
	}
	req := httptest.NewRequest("POST", "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if got := srv.uiAmendBrief(context.Background(), task, req); got != "brief-amended" {
		t.Errorf("got %q", got)
	}
}

func TestUIAmendBrief_InsertError(t *testing.T) {
	srv := NewServer(
		WithTaskMessageRepository(&uiTcStubMsgRepo{insertErr: errors.New("oops")}),
	)
	task := &persistence.Task{ID: "t1", Status: persistence.TaskStatusAwaitingInput}
	form := url.Values{"new_brief": []string{"new"}}
	req := httptest.NewRequest("POST", "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if got := srv.uiAmendBrief(context.Background(), task, req); got != "amend-failed" {
		t.Errorf("got %q", got)
	}
}

// --- uiSimpleFlip ----------------------------------------------------

func TestUISimpleFlip_HappyPath(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			return true, nil
		},
	}
	srv := NewServer(
		WithTaskRepository(taskRepo),
		WithTaskMessageRepository(&uiTcStubMsgRepo{}),
	)
	task := &persistence.Task{ID: "t1", Status: persistence.TaskStatusRunning}
	got := srv.uiSimpleFlip(context.Background(), task,
		[]persistence.TaskStatus{persistence.TaskStatusRunning},
		persistence.TaskStatusPaused, "paused", false)
	if got != "paused" {
		t.Errorf("got %q", got)
	}
}

func TestUISimpleFlip_TransitionStale(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			return false, nil
		},
	}
	srv := NewServer(
		WithTaskRepository(taskRepo),
		WithTaskMessageRepository(&uiTcStubMsgRepo{}),
	)
	task := &persistence.Task{ID: "t1", Status: persistence.TaskStatusRunning}
	got := srv.uiSimpleFlip(context.Background(), task,
		[]persistence.TaskStatus{persistence.TaskStatusPaused},
		persistence.TaskStatusQueued, "resumed", true)
	if got != "resumed-stale" {
		t.Errorf("got %q", got)
	}
}

func TestUISimpleFlip_InsertError(t *testing.T) {
	srv := NewServer(
		WithTaskMessageRepository(&uiTcStubMsgRepo{insertErr: errors.New("boom")}),
	)
	task := &persistence.Task{ID: "t1", Status: persistence.TaskStatusRunning}
	got := srv.uiSimpleFlip(context.Background(), task,
		[]persistence.TaskStatus{persistence.TaskStatusRunning},
		persistence.TaskStatusPaused, "paused", false)
	if got != "paused-failed" {
		t.Errorf("got %q", got)
	}
}

// --- uiCloseTask -----------------------------------------------------

func TestUICloseTask_InvalidState(t *testing.T) {
	srv := &Server{}
	task := &persistence.Task{ID: "t1", Status: persistence.TaskStatusRunning}
	form := url.Values{}
	req := httptest.NewRequest("POST", "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if got := srv.uiCloseTask(context.Background(), task, req); got != "close-not-eligible" {
		t.Errorf("got %q", got)
	}
}

func TestUICloseTask_HappyPath(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			return true, nil
		},
	}
	srv := NewServer(
		WithTaskRepository(taskRepo),
		WithTaskMessageRepository(&uiTcStubMsgRepo{}),
	)
	task := &persistence.Task{ID: "t1", Status: persistence.TaskStatusCompleted}
	form := url.Values{"reason": []string{"deliverable shipped"}, "author": []string{"alice"}}
	req := httptest.NewRequest("POST", "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	got := srv.uiCloseTask(context.Background(), task, req)
	if !strings.Contains(got, "closed") {
		t.Errorf("got %q", got)
	}
}

// TestUICloseTask_FromFailed — regression for the dead "Close —
// won't pursue" button. recovery_actions.go offers that button on
// every FAILED task, but uiCloseTask used to reject FAILED (only
// COMPLETED/AWAITING_* were close-eligible), so clicking it did
// nothing. A FAILED task must close cleanly.
// (companion-example task_20260603131232 / task_20260602214128).
func TestUICloseTask_FromFailed(t *testing.T) {
	var gotFrom []persistence.TaskStatus
	var gotTo persistence.TaskStatus
	taskRepo := &mocks.MockTaskRepository{
		TransitionConditionalFunc: func(_ context.Context, _ string, from []persistence.TaskStatus, to persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			gotFrom, gotTo = from, to
			return true, nil
		},
	}
	srv := NewServer(
		WithTaskRepository(taskRepo),
		WithTaskMessageRepository(&uiTcStubMsgRepo{}),
	)
	task := &persistence.Task{ID: "t1", Status: persistence.TaskStatusFailed}
	form := url.Values{"reason": []string{"retries exhausted, won't pursue"}, "author": []string{"vadim"}}
	req := httptest.NewRequest("POST", "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if got := srv.uiCloseTask(context.Background(), task, req); got != "task-closed" {
		t.Fatalf("uiCloseTask(FAILED) = %q, want task-closed", got)
	}
	if gotTo != persistence.TaskStatusClosed {
		t.Errorf("transition target = %q, want CLOSED", gotTo)
	}
	// The conditional from-set must include FAILED, else the DB
	// update would never match a failed task.
	found := false
	for _, s := range gotFrom {
		if s == persistence.TaskStatusFailed {
			found = true
		}
	}
	if !found {
		t.Errorf("TransitionConditional from-set %v missing FAILED", gotFrom)
	}
}

// closeNotifySpy records NotifyChildTerminal calls so the close
// path can be asserted on without booting an executor. The other
// ExecutorInterface methods aren't exercised by uiCloseTask.
type closeNotifySpy struct {
	calls []string
}

func (s *closeNotifySpy) Cancel(string) error       { return nil }
func (s *closeNotifySpy) Pause(string) error        { return nil }
func (s *closeNotifySpy) ResumePaused(string) error { return nil }
func (s *closeNotifySpy) ResumeTask(string) error   { return nil }
func (s *closeNotifySpy) NotifyChildTerminal(_ context.Context, childTaskID string) {
	s.calls = append(s.calls, childTaskID)
}

// TestUICloseTask_NotifiesExecutorOnChildClose — when the closed
// task has a parent, the close path must drive the executor's
// parent-unblock sweep so the parent doesn't sit in
// WAITING_FOR_CHILDREN forever (task_20260521111852_8016a4a902b4f959,
// 2026-05-21).
func TestUICloseTask_NotifiesExecutorOnChildClose(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			return true, nil
		},
	}
	spy := &closeNotifySpy{}
	srv := NewServer(
		WithTaskRepository(taskRepo),
		WithTaskMessageRepository(&uiTcStubMsgRepo{}),
		WithExecutor(spy),
	)
	parentID := "parent-1"
	task := &persistence.Task{
		ID:           "child-1",
		Status:       persistence.TaskStatusAwaitingInput,
		ParentTaskID: &parentID,
	}
	req := httptest.NewRequest("POST", "/", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	got := srv.uiCloseTask(context.Background(), task, req)
	if got != "task-closed" {
		t.Fatalf("uiCloseTask = %q, want task-closed", got)
	}
	if len(spy.calls) != 1 || spy.calls[0] != "child-1" {
		t.Errorf("NotifyChildTerminal calls = %v, want [child-1]", spy.calls)
	}
}

// TestUICloseTask_NoNotifyForRootTask — closing a task with no
// parent must not call NotifyChildTerminal (there's nothing to
// unblock). Guards against a future regression where the close
// path eagerly notifies on every close.
func TestUICloseTask_NoNotifyForRootTask(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			return true, nil
		},
	}
	spy := &closeNotifySpy{}
	srv := NewServer(
		WithTaskRepository(taskRepo),
		WithTaskMessageRepository(&uiTcStubMsgRepo{}),
		WithExecutor(spy),
	)
	task := &persistence.Task{ID: "root-1", Status: persistence.TaskStatusCompleted}
	req := httptest.NewRequest("POST", "/", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if got := srv.uiCloseTask(context.Background(), task, req); got != "task-closed" {
		t.Fatalf("uiCloseTask = %q, want task-closed", got)
	}
	if len(spy.calls) != 0 {
		t.Errorf("NotifyChildTerminal calls = %v, want none for root task", spy.calls)
	}
}

// keep helper unused-friendly while we evolve this file
var _ = makeFormRequest
var _ = uiActionRequest

// TestUIAnswerCheckpoint_ChoiceOnlyGetsLabelContent — regression pin
// for task …a691c512ebd1c4fd (2026-06-06): an operator who picked a
// decision option WITHOUT typing free text got an answer row with
// Content:"" — the chat log showed an empty answer and the lead agent
// (which reads Content) ignored the decision and re-asked. A
// choice-only answer must persist the chosen option's LABEL as
// Content.
func TestUIAnswerCheckpoint_ChoiceOnlyGetsLabelContent(t *testing.T) {
	open := "cp1"
	msgRepo := &uiTcStubMsgRepo{
		checkpoint: &persistence.TaskMessage{
			ID:          "cp1",
			MessageKind: persistence.TaskMessageKindCheckpoint,
			Metadata: []byte(`{"kind":"decision","options":[
				{"id":"retry_research_pathfix","label":"Retry from researcher with an explicit corrective hint."},
				{"id":"abort","label":"Abort with explanation."}]}`),
		},
	}
	taskRepo := &mocks.MockTaskRepository{
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			return true, nil
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo), WithTaskMessageRepository(msgRepo))
	task := &persistence.Task{
		ID:               "t1",
		Status:           persistence.TaskStatusAwaitingInput,
		OpenCheckpointID: &open,
	}
	form := url.Values{
		"checkpoint_id": []string{"cp1"},
		"choice":        []string{"retry_research_pathfix"},
	}
	req := httptest.NewRequest("POST", "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if got := srv.uiAnswerCheckpoint(context.Background(), task, req); got != "answer-sent" {
		t.Fatalf("got %q, want answer-sent", got)
	}
	if len(msgRepo.inserts) != 1 {
		t.Fatalf("inserts = %d, want 1", len(msgRepo.inserts))
	}
	ans := msgRepo.inserts[0]
	if ans.Content != "Retry from researcher with an explicit corrective hint." {
		t.Errorf("answer Content = %q, want the chosen option's label", ans.Content)
	}
}

// TestUIAnswerCheckpoint_ChoiceOnlyUnknownIDFallsBackToChoice — the
// label lookup is best-effort; an id that doesn't resolve (stale
// options, free-form choice) still records the id itself so the
// answer is never empty.
func TestUIAnswerCheckpoint_ChoiceOnlyUnknownIDFallsBackToChoice(t *testing.T) {
	open := "cp1"
	msgRepo := &uiTcStubMsgRepo{}
	taskRepo := &mocks.MockTaskRepository{
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			return true, nil
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo), WithTaskMessageRepository(msgRepo))
	task := &persistence.Task{
		ID:               "t1",
		Status:           persistence.TaskStatusAwaitingInput,
		OpenCheckpointID: &open,
	}
	form := url.Values{
		"checkpoint_id": []string{"cp1"},
		"choice":        []string{"abort"},
	}
	req := httptest.NewRequest("POST", "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if got := srv.uiAnswerCheckpoint(context.Background(), task, req); got != "answer-sent" {
		t.Fatalf("got %q, want answer-sent", got)
	}
	if len(msgRepo.inserts) != 1 || msgRepo.inserts[0].Content != "abort" {
		t.Fatalf("answer Content = %q, want fallback to the choice id", msgRepo.inserts[0].Content)
	}
}
