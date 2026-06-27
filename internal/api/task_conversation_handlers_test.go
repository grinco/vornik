// Package api: hermetic HTTP-handler tests for the conversational
// task-lifecycle surface (ListTaskMessages / AnswerCheckpoint /
// AmendBrief / Pause / Resume / CloseTask / SummarizeThread).
//
// All tests wire stub persistence into a Server; no DB, no network.
// The existing task_conversation_directive_test.go pins the directive
// re-queue matrix — this file fills in the rest of the surface.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// --- stub task-message repo with full surface ------------------------

type tcStubMessageRepo struct {
	insertFn           func(ctx context.Context, m *persistence.TaskMessage) error
	listFn             func(ctx context.Context, f persistence.TaskMessageFilter) ([]*persistence.TaskMessage, error)
	getOpenFn          func(ctx context.Context, taskID string) (*persistence.TaskMessage, error)
	markResolvedFn     func(ctx context.Context, taskID, checkpointID string) error
	inserts            []*persistence.TaskMessage
	listFilter         persistence.TaskMessageFilter
	resolvedTaskID     string
	resolvedCheckpoint string
}

func (s *tcStubMessageRepo) Insert(ctx context.Context, m *persistence.TaskMessage) error {
	if m.ID == "" {
		m.ID = "msg-stub"
	}
	s.inserts = append(s.inserts, m)
	if s.insertFn != nil {
		return s.insertFn(ctx, m)
	}
	return nil
}

func (s *tcStubMessageRepo) List(ctx context.Context, f persistence.TaskMessageFilter) ([]*persistence.TaskMessage, error) {
	s.listFilter = f
	if s.listFn != nil {
		return s.listFn(ctx, f)
	}
	return nil, nil
}

func (s *tcStubMessageRepo) GetOpenCheckpoint(ctx context.Context, taskID string) (*persistence.TaskMessage, error) {
	if s.getOpenFn != nil {
		return s.getOpenFn(ctx, taskID)
	}
	return nil, nil
}

func (s *tcStubMessageRepo) MarkCheckpointResolved(ctx context.Context, taskID, checkpointID string) error {
	s.resolvedTaskID = taskID
	s.resolvedCheckpoint = checkpointID
	if s.markResolvedFn != nil {
		return s.markResolvedFn(ctx, taskID, checkpointID)
	}
	return nil
}

// helpers ------------------------------------------------------------

const (
	tcProjectID = "proj-x"
	tcTaskID    = "task-x"
)

func tcURL(suffix string) string {
	return "/api/v1/projects/" + tcProjectID + "/tasks/" + tcTaskID + suffix
}

func tcServer(taskRepo persistence.TaskRepository, msgRepo persistence.TaskMessageRepository) *Server {
	return &Server{taskRepo: taskRepo, taskMessageRepo: msgRepo}
}

func tcTask(status persistence.TaskStatus) *persistence.Task {
	return &persistence.Task{ID: tcTaskID, ProjectID: tcProjectID, Status: status, MaxAttempts: 3}
}

// --- ListTaskMessages -------------------------------------------------

func TestListTaskMessages_LifecycleDisabled(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodGet, tcURL("/messages"), nil)
	rec := httptest.NewRecorder()
	srv.ListTaskMessages(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503", rec.Code)
	}
}

func TestListTaskMessages_TaskNotFound(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return nil, persistence.ErrNotFound
		},
	}
	srv := tcServer(taskRepo, &tcStubMessageRepo{})
	req := httptest.NewRequest(http.MethodGet, tcURL("/messages"), nil)
	rec := httptest.NewRecorder()
	srv.ListTaskMessages(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", rec.Code)
	}
}

func TestListTaskMessages_TaskRepoError(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return nil, errors.New("db down")
		},
	}
	srv := tcServer(taskRepo, &tcStubMessageRepo{})
	req := httptest.NewRequest(http.MethodGet, tcURL("/messages"), nil)
	rec := httptest.NewRecorder()
	srv.ListTaskMessages(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
}

func TestListTaskMessages_ProjectMismatch(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: tcTaskID, ProjectID: "different-project"}, nil
		},
	}
	srv := tcServer(taskRepo, &tcStubMessageRepo{})
	req := httptest.NewRequest(http.MethodGet, tcURL("/messages"), nil)
	rec := httptest.NewRecorder()
	srv.ListTaskMessages(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404 (project mismatch)", rec.Code)
	}
}

func TestListTaskMessages_FiltersAndLimit(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return tcTask(persistence.TaskStatusRunning), nil
		},
	}
	msgRepo := &tcStubMessageRepo{
		listFn: func(_ context.Context, f persistence.TaskMessageFilter) ([]*persistence.TaskMessage, error) {
			return []*persistence.TaskMessage{
				{ID: "m1", TaskID: f.TaskID},
				{ID: "m2", TaskID: f.TaskID},
			}, nil
		},
	}
	srv := tcServer(taskRepo, msgRepo)
	req := httptest.NewRequest(http.MethodGet, tcURL("/messages?after=m0&limit=50&kind=message,answer"), nil)
	rec := httptest.NewRecorder()
	srv.ListTaskMessages(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	if msgRepo.listFilter.After == nil || *msgRepo.listFilter.After != "m0" {
		t.Errorf("after: got %v, want m0", msgRepo.listFilter.After)
	}
	if msgRepo.listFilter.Limit != 50 {
		t.Errorf("limit: got %d, want 50", msgRepo.listFilter.Limit)
	}
	if len(msgRepo.listFilter.MessageKinds) != 2 {
		t.Errorf("kinds: got %v", msgRepo.listFilter.MessageKinds)
	}
}

func TestListTaskMessages_ListError(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return tcTask(persistence.TaskStatusRunning), nil
		},
	}
	msgRepo := &tcStubMessageRepo{
		listFn: func(_ context.Context, _ persistence.TaskMessageFilter) ([]*persistence.TaskMessage, error) {
			return nil, errors.New("query failed")
		},
	}
	srv := tcServer(taskRepo, msgRepo)
	req := httptest.NewRequest(http.MethodGet, tcURL("/messages"), nil)
	rec := httptest.NewRecorder()
	srv.ListTaskMessages(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
}

// --- PostTaskMessage helper paths not covered by directive matrix ----

func TestPostTaskMessage_MethodNotAllowed(t *testing.T) {
	srv := tcServer(&mocks.MockTaskRepository{}, &tcStubMessageRepo{})
	req := httptest.NewRequest(http.MethodGet, tcURL("/messages"), nil)
	rec := httptest.NewRecorder()
	srv.PostTaskMessage(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d, want 405", rec.Code)
	}
}

func TestPostTaskMessage_LifecycleDisabled(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodPost, tcURL("/messages"), strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	srv.PostTaskMessage(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503", rec.Code)
	}
}

func TestPostTaskMessage_InvalidJSON(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return tcTask(persistence.TaskStatusRunning), nil
		},
	}
	srv := tcServer(taskRepo, &tcStubMessageRepo{})
	req := httptest.NewRequest(http.MethodPost, tcURL("/messages"), strings.NewReader("not-json"))
	rec := httptest.NewRecorder()
	srv.PostTaskMessage(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
}

func TestPostTaskMessage_InvalidKind(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return tcTask(persistence.TaskStatusRunning), nil
		},
	}
	srv := tcServer(taskRepo, &tcStubMessageRepo{})
	req := httptest.NewRequest(http.MethodPost, tcURL("/messages"),
		strings.NewReader(`{"kind":"checkpoint","content":"x"}`))
	rec := httptest.NewRecorder()
	srv.PostTaskMessage(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "INVALID_KIND") {
		t.Errorf("missing INVALID_KIND: %s", rec.Body.String())
	}
}

func TestPostTaskMessage_EmptyContent(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return tcTask(persistence.TaskStatusRunning), nil
		},
	}
	srv := tcServer(taskRepo, &tcStubMessageRepo{})
	req := httptest.NewRequest(http.MethodPost, tcURL("/messages"),
		strings.NewReader(`{"kind":"message","content":"   "}`))
	rec := httptest.NewRecorder()
	srv.PostTaskMessage(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
}

func TestPostTaskMessage_InsertError(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return tcTask(persistence.TaskStatusRunning), nil
		},
	}
	msgRepo := &tcStubMessageRepo{
		insertFn: func(_ context.Context, _ *persistence.TaskMessage) error {
			return errors.New("insert failed")
		},
	}
	srv := tcServer(taskRepo, msgRepo)
	req := httptest.NewRequest(http.MethodPost, tcURL("/messages"),
		strings.NewReader(`{"kind":"message","content":"hi"}`))
	rec := httptest.NewRecorder()
	srv.PostTaskMessage(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
}

func TestPostTaskMessage_AuthorAndMetadataPassThrough(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return tcTask(persistence.TaskStatusRunning), nil
		},
	}
	msgRepo := &tcStubMessageRepo{}
	srv := tcServer(taskRepo, msgRepo)
	req := httptest.NewRequest(http.MethodPost, tcURL("/messages"),
		strings.NewReader(`{"kind":"message","content":"hi","authorId":"alice","metadata":{"a":1}}`))
	rec := httptest.NewRecorder()
	srv.PostTaskMessage(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	if len(msgRepo.inserts) != 1 {
		t.Fatalf("inserts: got %d, want 1", len(msgRepo.inserts))
	}
	m := msgRepo.inserts[0]
	if m.AuthorID == nil || *m.AuthorID != "alice" {
		t.Errorf("authorID: got %v, want alice", m.AuthorID)
	}
	if string(m.Metadata) == "" {
		t.Errorf("metadata not passed through: %s", string(m.Metadata))
	}
}

// --- AnswerCheckpoint -------------------------------------------------

func TestAnswerCheckpoint_MethodNotAllowed(t *testing.T) {
	srv := tcServer(&mocks.MockTaskRepository{}, &tcStubMessageRepo{})
	req := httptest.NewRequest(http.MethodGet, tcURL("/messages/cp1/answer"), nil)
	rec := httptest.NewRecorder()
	srv.AnswerCheckpoint(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestAnswerCheckpoint_LifecycleDisabled(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodPost, tcURL("/messages/cp1/answer"), strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	srv.AnswerCheckpoint(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestAnswerCheckpoint_InvalidState(t *testing.T) {
	// CLOSED task → ValidateTransition will reject
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return tcTask(persistence.TaskStatusClosed), nil
		},
	}
	srv := tcServer(taskRepo, &tcStubMessageRepo{})
	req := httptest.NewRequest(http.MethodPost, tcURL("/messages/cp1/answer"),
		strings.NewReader(`{"content":"yes"}`))
	rec := httptest.NewRecorder()
	srv.AnswerCheckpoint(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409", rec.Code)
	}
}

func TestAnswerCheckpoint_CheckpointMismatch(t *testing.T) {
	openCP := "cp-other"
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			t := tcTask(persistence.TaskStatusAwaitingInput)
			t.OpenCheckpointID = &openCP
			return t, nil
		},
	}
	srv := tcServer(taskRepo, &tcStubMessageRepo{})
	req := httptest.NewRequest(http.MethodPost, tcURL("/messages/cp1/answer"),
		strings.NewReader(`{"content":"yes"}`))
	rec := httptest.NewRecorder()
	srv.AnswerCheckpoint(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409 (cp mismatch)", rec.Code)
	}
}

func TestAnswerCheckpoint_EmptyContentAndChoice(t *testing.T) {
	cp := "cp1"
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			t := tcTask(persistence.TaskStatusAwaitingInput)
			t.OpenCheckpointID = &cp
			return t, nil
		},
	}
	srv := tcServer(taskRepo, &tcStubMessageRepo{})
	req := httptest.NewRequest(http.MethodPost, tcURL("/messages/cp1/answer"),
		strings.NewReader(`{"content":"","choice":""}`))
	rec := httptest.NewRecorder()
	srv.AnswerCheckpoint(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
}

func TestAnswerCheckpoint_HappyPath_WithChoiceAndMetadata(t *testing.T) {
	cp := "cp1"
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			t := tcTask(persistence.TaskStatusAwaitingInput)
			t.OpenCheckpointID = &cp
			return t, nil
		},
		TransitionConditionalFunc: func(_ context.Context, id string, from []persistence.TaskStatus, _ persistence.TaskStatus, opts persistence.TransitionOpts) (bool, error) {
			if !opts.ClearLease {
				t.Errorf("expected ClearLease=true; got %+v", opts)
			}
			return true, nil
		},
	}
	msgRepo := &tcStubMessageRepo{}
	srv := tcServer(taskRepo, msgRepo)
	req := httptest.NewRequest(http.MethodPost, tcURL("/messages/cp1/answer"),
		strings.NewReader(`{"choice":"option_a","metadata":{"reason":"approved"}}`))
	rec := httptest.NewRecorder()
	srv.AnswerCheckpoint(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	if msgRepo.resolvedCheckpoint != "cp1" {
		t.Errorf("MarkCheckpointResolved: got %q, want cp1", msgRepo.resolvedCheckpoint)
	}
	var body map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&body)
	if body["requeued"].(bool) != true {
		t.Errorf("requeued: %v", body["requeued"])
	}
}

func TestAnswerCheckpoint_InsertError(t *testing.T) {
	cp := "cp1"
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			t := tcTask(persistence.TaskStatusAwaitingInput)
			t.OpenCheckpointID = &cp
			return t, nil
		},
	}
	msgRepo := &tcStubMessageRepo{
		insertFn: func(_ context.Context, _ *persistence.TaskMessage) error {
			return errors.New("insert failed")
		},
	}
	srv := tcServer(taskRepo, msgRepo)
	req := httptest.NewRequest(http.MethodPost, tcURL("/messages/cp1/answer"),
		strings.NewReader(`{"content":"yes"}`))
	rec := httptest.NewRecorder()
	srv.AnswerCheckpoint(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
}

func TestAnswerCheckpoint_ResolveError(t *testing.T) {
	cp := "cp1"
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			t := tcTask(persistence.TaskStatusAwaitingInput)
			t.OpenCheckpointID = &cp
			return t, nil
		},
	}
	msgRepo := &tcStubMessageRepo{
		markResolvedFn: func(_ context.Context, _, _ string) error {
			return errors.New("resolve failed")
		},
	}
	srv := tcServer(taskRepo, msgRepo)
	req := httptest.NewRequest(http.MethodPost, tcURL("/messages/cp1/answer"),
		strings.NewReader(`{"content":"yes"}`))
	rec := httptest.NewRecorder()
	srv.AnswerCheckpoint(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
}

func TestAnswerCheckpoint_TransitionDrift(t *testing.T) {
	cp := "cp1"
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			t := tcTask(persistence.TaskStatusAwaitingInput)
			t.OpenCheckpointID = &cp
			return t, nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			return false, nil // simulate drift
		},
	}
	srv := tcServer(taskRepo, &tcStubMessageRepo{})
	req := httptest.NewRequest(http.MethodPost, tcURL("/messages/cp1/answer"),
		strings.NewReader(`{"content":"yes"}`))
	rec := httptest.NewRecorder()
	srv.AnswerCheckpoint(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409", rec.Code)
	}
}

func TestAnswerCheckpoint_MissingCheckpointID(t *testing.T) {
	cp := "cp1"
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			t := tcTask(persistence.TaskStatusAwaitingInput)
			t.OpenCheckpointID = &cp
			return t, nil
		},
	}
	srv := tcServer(taskRepo, &tcStubMessageRepo{})
	// URL with no checkpoint segment after /messages/
	req := httptest.NewRequest(http.MethodPost, tcURL("/messages//answer"),
		strings.NewReader(`{"content":"yes"}`))
	rec := httptest.NewRecorder()
	srv.AnswerCheckpoint(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
}

// --- AmendBrief -------------------------------------------------------

func TestAmendBrief_MethodNotAllowed(t *testing.T) {
	srv := tcServer(&mocks.MockTaskRepository{}, &tcStubMessageRepo{})
	req := httptest.NewRequest(http.MethodGet, tcURL("/amend"), nil)
	rec := httptest.NewRecorder()
	srv.AmendBrief(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestAmendBrief_LifecycleDisabled(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodPost, tcURL("/amend"), strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	srv.AmendBrief(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestAmendBrief_EmptyBrief(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return tcTask(persistence.TaskStatusRunning), nil
		},
	}
	srv := tcServer(taskRepo, &tcStubMessageRepo{})
	req := httptest.NewRequest(http.MethodPost, tcURL("/amend"),
		strings.NewReader(`{"newBrief":"   "}`))
	rec := httptest.NewRecorder()
	srv.AmendBrief(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
}

func TestAmendBrief_TerminalState(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return tcTask(persistence.TaskStatusClosed), nil
		},
	}
	srv := tcServer(taskRepo, &tcStubMessageRepo{})
	req := httptest.NewRequest(http.MethodPost, tcURL("/amend"),
		strings.NewReader(`{"newBrief":"new"}`))
	rec := httptest.NewRecorder()
	srv.AmendBrief(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409", rec.Code)
	}
}

func TestAmendBrief_HappyPath_Requeues(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return tcTask(persistence.TaskStatusAwaitingInput), nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, opts persistence.TransitionOpts) (bool, error) {
			if opts.BriefAmendedAt == nil {
				t.Errorf("expected BriefAmendedAt set")
			}
			return true, nil
		},
	}
	msgRepo := &tcStubMessageRepo{}
	srv := tcServer(taskRepo, msgRepo)
	req := httptest.NewRequest(http.MethodPost, tcURL("/amend"),
		strings.NewReader(`{"newBrief":"do the thing better","reason":"misunderstood","authorId":"vadim"}`))
	rec := httptest.NewRecorder()
	srv.AmendBrief(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	if len(msgRepo.inserts) != 1 {
		t.Fatalf("inserts: %d", len(msgRepo.inserts))
	}
	if !strings.Contains(msgRepo.inserts[0].Content, "misunderstood") {
		t.Errorf("reason missing from content: %s", msgRepo.inserts[0].Content)
	}
}

func TestAmendBrief_OnRunning_DoesNotTransition(t *testing.T) {
	called := false
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return tcTask(persistence.TaskStatusRunning), nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			called = true
			return true, nil
		},
	}
	srv := tcServer(taskRepo, &tcStubMessageRepo{})
	req := httptest.NewRequest(http.MethodPost, tcURL("/amend"),
		strings.NewReader(`{"newBrief":"new"}`))
	rec := httptest.NewRecorder()
	srv.AmendBrief(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d", rec.Code)
	}
	if called {
		t.Errorf("RUNNING tasks must not transition on amend")
	}
}

func TestAmendBrief_InsertError(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return tcTask(persistence.TaskStatusAwaitingInput), nil
		},
	}
	msgRepo := &tcStubMessageRepo{
		insertFn: func(_ context.Context, _ *persistence.TaskMessage) error {
			return errors.New("oops")
		},
	}
	srv := tcServer(taskRepo, msgRepo)
	req := httptest.NewRequest(http.MethodPost, tcURL("/amend"),
		strings.NewReader(`{"newBrief":"new"}`))
	rec := httptest.NewRecorder()
	srv.AmendBrief(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
}

// --- CloseTask --------------------------------------------------------

func TestCloseTask_MethodNotAllowed(t *testing.T) {
	srv := tcServer(&mocks.MockTaskRepository{}, &tcStubMessageRepo{})
	req := httptest.NewRequest(http.MethodGet, tcURL("/close"), nil)
	rec := httptest.NewRecorder()
	srv.CloseTask(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestCloseTask_LifecycleDisabled(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodPost, tcURL("/close"), nil)
	rec := httptest.NewRecorder()
	srv.CloseTask(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestCloseTask_InvalidStateForClose(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return tcTask(persistence.TaskStatusRunning), nil
		},
	}
	srv := tcServer(taskRepo, &tcStubMessageRepo{})
	req := httptest.NewRequest(http.MethodPost, tcURL("/close"), strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	srv.CloseTask(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409", rec.Code)
	}
}

func TestCloseTask_HappyPath(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return tcTask(persistence.TaskStatusCompleted), nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, to persistence.TaskStatus, opts persistence.TransitionOpts) (bool, error) {
			if to != persistence.TaskStatusClosed {
				t.Errorf("transition target: got %s, want CLOSED", to)
			}
			if !opts.SetClosedAtNow || !opts.ClearLease {
				t.Errorf("transition opts: %+v", opts)
			}
			return true, nil
		},
	}
	msgRepo := &tcStubMessageRepo{}
	srv := tcServer(taskRepo, msgRepo)
	req := httptest.NewRequest(http.MethodPost, tcURL("/close"),
		strings.NewReader(`{"reason":"obsolete","authorId":"alice"}`))
	rec := httptest.NewRecorder()
	srv.CloseTask(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&body)
	if body["closedBy"] != "alice" {
		t.Errorf("closedBy: got %v, want alice", body["closedBy"])
	}
}

func TestCloseTask_TransitionError(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return tcTask(persistence.TaskStatusCompleted), nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			return false, errors.New("db blew up")
		},
	}
	srv := tcServer(taskRepo, &tcStubMessageRepo{})
	req := httptest.NewRequest(http.MethodPost, tcURL("/close"), strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	srv.CloseTask(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
}

func TestCloseTask_TransitionDrift(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return tcTask(persistence.TaskStatusCompleted), nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			return false, nil
		},
	}
	srv := tcServer(taskRepo, &tcStubMessageRepo{})
	req := httptest.NewRequest(http.MethodPost, tcURL("/close"), strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	srv.CloseTask(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409", rec.Code)
	}
}

func TestCloseTask_InsertError(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return tcTask(persistence.TaskStatusCompleted), nil
		},
	}
	msgRepo := &tcStubMessageRepo{
		insertFn: func(_ context.Context, _ *persistence.TaskMessage) error {
			return errors.New("insert failed")
		},
	}
	srv := tcServer(taskRepo, msgRepo)
	req := httptest.NewRequest(http.MethodPost, tcURL("/close"), strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	srv.CloseTask(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d", rec.Code)
	}
}

// --- SummarizeThread --------------------------------------------------

func TestSummarizeThread_MethodNotAllowed(t *testing.T) {
	srv := tcServer(&mocks.MockTaskRepository{}, &tcStubMessageRepo{})
	req := httptest.NewRequest(http.MethodGet, tcURL("/summarize"), nil)
	rec := httptest.NewRecorder()
	srv.SummarizeThread(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestSummarizeThread_LifecycleDisabled(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodPost, tcURL("/summarize"), strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	srv.SummarizeThread(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestSummarizeThread_MissingSummary(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return tcTask(persistence.TaskStatusRunning), nil
		},
	}
	srv := tcServer(taskRepo, &tcStubMessageRepo{})
	req := httptest.NewRequest(http.MethodPost, tcURL("/summarize"),
		strings.NewReader(`{"messageIds":["m1"],"summary":""}`))
	rec := httptest.NewRecorder()
	srv.SummarizeThread(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestSummarizeThread_MissingMessageIDs(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return tcTask(persistence.TaskStatusRunning), nil
		},
	}
	srv := tcServer(taskRepo, &tcStubMessageRepo{})
	req := httptest.NewRequest(http.MethodPost, tcURL("/summarize"),
		strings.NewReader(`{"summary":"x"}`))
	rec := httptest.NewRecorder()
	srv.SummarizeThread(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestSummarizeThread_HappyPath_DefaultsLead(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return tcTask(persistence.TaskStatusRunning), nil
		},
	}
	msgRepo := &tcStubMessageRepo{}
	srv := tcServer(taskRepo, msgRepo)
	req := httptest.NewRequest(http.MethodPost, tcURL("/summarize"),
		strings.NewReader(`{"messageIds":["m1","m2"],"summary":"compressed text"}`))
	rec := httptest.NewRecorder()
	srv.SummarizeThread(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	if len(msgRepo.inserts) != 1 {
		t.Fatalf("inserts: %d", len(msgRepo.inserts))
	}
	m := msgRepo.inserts[0]
	if m.AuthorID == nil || *m.AuthorID != "lead" {
		t.Errorf("default authorID: got %v, want lead", m.AuthorID)
	}
	if m.MessageKind != persistence.TaskMessageKindNote {
		t.Errorf("kind: got %s, want note", m.MessageKind)
	}
}

func TestSummarizeThread_InsertError(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return tcTask(persistence.TaskStatusRunning), nil
		},
	}
	msgRepo := &tcStubMessageRepo{
		insertFn: func(_ context.Context, _ *persistence.TaskMessage) error {
			return errors.New("insert failed")
		},
	}
	srv := tcServer(taskRepo, msgRepo)
	req := httptest.NewRequest(http.MethodPost, tcURL("/summarize"),
		strings.NewReader(`{"messageIds":["m1"],"summary":"x"}`))
	rec := httptest.NewRecorder()
	srv.SummarizeThread(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d", rec.Code)
	}
}

// --- simpleStatusFlip (PauseTask / ResumeTask) -----------------------

func TestPauseTask_MethodNotAllowed(t *testing.T) {
	srv := tcServer(&mocks.MockTaskRepository{}, &tcStubMessageRepo{})
	req := httptest.NewRequest(http.MethodGet, tcURL("/pause"), nil)
	rec := httptest.NewRecorder()
	srv.PauseTask(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestPauseTask_LifecycleDisabled(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodPost, tcURL("/pause"), nil)
	rec := httptest.NewRecorder()
	srv.PauseTask(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestPauseTask_InvalidState(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return tcTask(persistence.TaskStatusClosed), nil
		},
	}
	srv := tcServer(taskRepo, &tcStubMessageRepo{})
	req := httptest.NewRequest(http.MethodPost, tcURL("/pause"), nil)
	rec := httptest.NewRecorder()
	srv.PauseTask(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestPauseTask_HappyPath_NoExecutor(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return tcTask(persistence.TaskStatusPending), nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, to persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			if to != persistence.TaskStatusPaused {
				t.Errorf("target: got %s", to)
			}
			return true, nil
		},
	}
	srv := tcServer(taskRepo, &tcStubMessageRepo{})
	req := httptest.NewRequest(http.MethodPost, tcURL("/pause"), nil)
	rec := httptest.NewRecorder()
	srv.PauseTask(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
}

func TestResumeTask_HappyPath(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return tcTask(persistence.TaskStatusPaused), nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, to persistence.TaskStatus, opts persistence.TransitionOpts) (bool, error) {
			if to != persistence.TaskStatusQueued {
				t.Errorf("target: got %s, want QUEUED", to)
			}
			if !opts.ClearLease {
				t.Errorf("expected ClearLease on resume")
			}
			return true, nil
		},
	}
	srv := tcServer(taskRepo, &tcStubMessageRepo{})
	req := httptest.NewRequest(http.MethodPost, tcURL("/resume"), nil)
	rec := httptest.NewRecorder()
	srv.ResumeTask(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestSimpleStatusFlip_InsertError(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return tcTask(persistence.TaskStatusPending), nil
		},
	}
	msgRepo := &tcStubMessageRepo{
		insertFn: func(_ context.Context, _ *persistence.TaskMessage) error {
			return errors.New("insert failed")
		},
	}
	srv := tcServer(taskRepo, msgRepo)
	req := httptest.NewRequest(http.MethodPost, tcURL("/pause"), nil)
	rec := httptest.NewRecorder()
	srv.PauseTask(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestSimpleStatusFlip_TransitionDrift(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return tcTask(persistence.TaskStatusPending), nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			return false, nil
		},
	}
	srv := tcServer(taskRepo, &tcStubMessageRepo{})
	req := httptest.NewRequest(http.MethodPost, tcURL("/pause"), nil)
	rec := httptest.NewRecorder()
	srv.PauseTask(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestSimpleStatusFlip_TransitionError(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return tcTask(persistence.TaskStatusPending), nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			return false, errors.New("boom")
		},
	}
	srv := tcServer(taskRepo, &tcStubMessageRepo{})
	req := httptest.NewRequest(http.MethodPost, tcURL("/pause"), nil)
	rec := httptest.NewRecorder()
	srv.PauseTask(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d", rec.Code)
	}
}

// --- extractCheckpointID coverage ------------------------------------

func TestExtractCheckpointID(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/api/v1/projects/p/tasks/t/messages/cp1/answer", "cp1"},
		{"/api/v1/projects/p/tasks/t/messages/abc", "abc"},
		{"/api/v1/projects/p/tasks/t/amend", ""},
		{"/messages", ""},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.path, nil)
			if got := extractCheckpointID(req); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAnswerCheckpoint_ChoiceOnlyGetsLabelContent — regression pin for
// task …a691c512ebd1c4fd (2026-06-06): a choice-only answer persisted
// Content:"" with the choice id only in metadata; the chat log showed
// an empty answer and the lead agent (which reads Content) re-asked
// the already-answered checkpoint. A choice-only answer must persist
// the chosen option's label (falling back to the raw id when the
// label doesn't resolve).
func TestAnswerCheckpoint_ChoiceOnlyGetsLabelContent(t *testing.T) {
	cp := "cp1"
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			t := tcTask(persistence.TaskStatusAwaitingInput)
			t.OpenCheckpointID = &cp
			return t, nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			return true, nil
		},
	}
	msgRepo := &tcStubMessageRepo{
		getOpenFn: func(_ context.Context, _ string) (*persistence.TaskMessage, error) {
			return &persistence.TaskMessage{
				ID:          "cp1",
				MessageKind: persistence.TaskMessageKindCheckpoint,
				Metadata: []byte(`{"kind":"decision","options":[
					{"id":"retry_research_pathfix","label":"Retry from researcher with an explicit corrective hint."},
					{"id":"abort","label":"Abort with explanation."}]}`),
			}, nil
		},
	}
	srv := tcServer(taskRepo, msgRepo)
	req := httptest.NewRequest(http.MethodPost, tcURL("/messages/cp1/answer"),
		strings.NewReader(`{"choice":"retry_research_pathfix"}`))
	rec := httptest.NewRecorder()
	srv.AnswerCheckpoint(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	if len(msgRepo.inserts) != 1 {
		t.Fatalf("inserts = %d, want 1", len(msgRepo.inserts))
	}
	if got := msgRepo.inserts[0].Content; got != "Retry from researcher with an explicit corrective hint." {
		t.Errorf("answer Content = %q, want the chosen option's label", got)
	}
}

// TestAnswerCheckpoint_ChoiceOnlyUnknownIDFallsBack — label lookup is
// best-effort; an unresolvable id still lands as Content.
func TestAnswerCheckpoint_ChoiceOnlyUnknownIDFallsBack(t *testing.T) {
	cp := "cp1"
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			t := tcTask(persistence.TaskStatusAwaitingInput)
			t.OpenCheckpointID = &cp
			return t, nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			return true, nil
		},
	}
	msgRepo := &tcStubMessageRepo{}
	srv := tcServer(taskRepo, msgRepo)
	req := httptest.NewRequest(http.MethodPost, tcURL("/messages/cp1/answer"),
		strings.NewReader(`{"choice":"option_a"}`))
	rec := httptest.NewRecorder()
	srv.AnswerCheckpoint(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	if len(msgRepo.inserts) != 1 || msgRepo.inserts[0].Content != "option_a" {
		t.Fatalf("answer Content = %q, want fallback to the choice id", msgRepo.inserts[0].Content)
	}
}
