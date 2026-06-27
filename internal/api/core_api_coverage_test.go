package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// errorEnvelope mirrors the shape respondError emits so tests can assert
// the JSON body's code (not just a substring) without re-typing the
// nested struct everywhere.
type errorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func decodeErrorEnvelope(t *testing.T, body []byte) errorEnvelope {
	t.Helper()
	var env errorEnvelope
	require.NoError(t, json.Unmarshal(body, &env), "error body must be valid JSON envelope: %s", body)
	return env
}

// ---------------------------------------------------------------------
// respondError — the canonical error response shape every handler uses.
// ---------------------------------------------------------------------

// TestCoreAPI_RespondErrorShape locks the {"error":{"code","message"}}
// envelope + Content-Type + status so a refactor of respondError can't
// silently change the contract every client parses.
func TestCoreAPI_RespondErrorShape(t *testing.T) {
	rec := httptest.NewRecorder()
	respondError(rec, http.StatusTeapot, "SOME_CODE", "a human message")

	require.Equal(t, http.StatusTeapot, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "SOME_CODE", env.Error.Code)
	assert.Equal(t, "a human message", env.Error.Message)
}

// ---------------------------------------------------------------------
// CreateTask — validation, project-scope, configuration and failure
// paths the existing suite doesn't already cover.
// ---------------------------------------------------------------------

// TestCoreAPI_CreateTask_MissingProjectID: the path carries no project
// segment, so the handler must 400 with VALIDATION_ERROR before it ever
// touches the body or the repo.
func TestCoreAPI_CreateTask_MissingProjectID(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{}
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(taskRepo))

	req := httptest.NewRequest(http.MethodPost, "/projects//tasks", bytes.NewBufferString(`{"taskType":"x"}`))
	rec := httptest.NewRecorder()
	server.CreateTask(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "VALIDATION_ERROR", env.Error.Code)
	assert.Equal(t, 0, taskRepo.CallCount.Create)
}

// TestCoreAPI_CreateTask_UnknownProjectReturns404: when a registry is
// wired, a project ID it doesn't know must 404 NOT_FOUND — the guard
// that stops a typo'd --project silently creating an orphan task.
func TestCoreAPI_CreateTask_UnknownProjectReturns404(t *testing.T) {
	reg := testWebhookRegistry(t) // knows only "project-1"
	taskRepo := &mocks.MockTaskRepository{}
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
		WithProjectRegistry(reg),
	)

	req := httptest.NewRequest(http.MethodPost, "/projects/no-such-project/tasks", bytes.NewBufferString(`{"taskType":"x"}`))
	rec := httptest.NewRecorder()
	server.CreateTask(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code)
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "NOT_FOUND", env.Error.Code)
	assert.Equal(t, 0, taskRepo.CallCount.Create)
}

// TestCoreAPI_CreateTask_InputArtifactsWithoutStore: requesting input
// artifacts on a daemon that has no artifact store wired must 503
// NOT_CONFIGURED — not a 500, and never a half-created task.
func TestCoreAPI_CreateTask_InputArtifactsWithoutStore(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{}
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(taskRepo))

	body := `{"taskType":"x","inputArtifacts":[{"name":"a.txt","content":"aGk="}]}`
	req := httptest.NewRequest(http.MethodPost, "/projects/project-1/tasks", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	server.CreateTask(rec, req)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "NOT_CONFIGURED", env.Error.Code)
	assert.Equal(t, 0, taskRepo.CallCount.Create)
}

// TestCoreAPI_CreateTask_RepoCreateErrorReturns500: a non-duplicate
// Create failure surfaces as INTERNAL_ERROR (the duplicate-key path is
// covered elsewhere; this is the generic write failure).
func TestCoreAPI_CreateTask_RepoCreateErrorReturns500(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		CreateFunc: func(ctx context.Context, task *persistence.Task) error {
			return errors.New("db down")
		},
	}
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(taskRepo))

	req := httptest.NewRequest(http.MethodPost, "/projects/project-1/tasks", bytes.NewBufferString(`{"taskType":"x"}`))
	rec := httptest.NewRecorder()
	server.CreateTask(rec, req)

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "INTERNAL_ERROR", env.Error.Code)
}

// TestCoreAPI_CreateTask_SuccessPersistsAndReturns202: the happy path
// with no registry/queue. Asserts 202 Accepted, the response carries
// the new task ID + QUEUED status, and the persisted row defaulted
// priority to 50 with a generated "task" ID and an Attempt=1 row.
func TestCoreAPI_CreateTask_SuccessPersistsAndReturns202(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{}
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(taskRepo))

	req := httptest.NewRequest(http.MethodPost, "/projects/project-1/tasks", bytes.NewBufferString(`{"taskType":"do-thing"}`))
	rec := httptest.NewRecorder()
	server.CreateTask(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	require.Equal(t, 1, taskRepo.CallCount.Create)

	created := taskRepo.LastCall.Task
	require.NotNil(t, created)
	assert.Equal(t, "project-1", created.ProjectID)
	assert.Equal(t, 50, created.Priority, "priority defaults to 50 when unset and no registry default")
	assert.Equal(t, persistence.TaskStatusQueued, created.Status)
	assert.Equal(t, 1, created.Attempt)
	assert.True(t, strings.HasPrefix(created.ID, "task"), "ID must be a generated task ID, got %q", created.ID)
	assert.Nil(t, created.IdempotencyKey, "no idempotency key supplied → nil, not empty pointer")

	var resp CreateTaskResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, created.ID, resp.TaskID)
	assert.Equal(t, string(persistence.TaskStatusQueued), resp.Status)
}

// ---------------------------------------------------------------------
// ListTasks — filter validation, registry guard, scope, status
// normalisation.
// ---------------------------------------------------------------------

// TestCoreAPI_ListTasks_InvalidStatusFilter: an unknown status value
// must be rejected at the API with 400 VALIDATION_ERROR rather than
// reaching the DB and bubbling up as a 500 from the check constraint.
func TestCoreAPI_ListTasks_InvalidStatusFilter(t *testing.T) {
	listCalled := false
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			listCalled = true
			return nil, nil
		},
	}
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(taskRepo))

	req := httptest.NewRequest(http.MethodGet, "/projects/project-1/tasks?status=BOGUS", nil)
	rec := httptest.NewRecorder()
	server.ListTasks(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "VALIDATION_ERROR", env.Error.Code)
	assert.False(t, listCalled, "invalid status must short-circuit before hitting the repo")
}

// TestCoreAPI_ListTasks_StatusFilterCaseInsensitive: a lowercase status
// is accepted and normalised to the UPPERCASE enum before reaching the
// filter — operator convenience without a 400.
func TestCoreAPI_ListTasks_StatusFilterCaseInsensitive(t *testing.T) {
	var seenStatus *persistence.TaskStatus
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, filter persistence.TaskFilter) ([]*persistence.Task, error) {
			seenStatus = filter.Status
			return []*persistence.Task{}, nil
		},
		CountFunc: func(_ context.Context, _ persistence.TaskFilter) (int64, error) { return 0, nil },
	}
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(taskRepo))

	req := httptest.NewRequest(http.MethodGet, "/projects/project-1/tasks?status=running", nil)
	rec := httptest.NewRecorder()
	server.ListTasks(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, seenStatus)
	assert.Equal(t, persistence.TaskStatusRunning, *seenStatus, "lowercase status must normalise to the UPPERCASE enum")
}

// TestCoreAPI_ListTasks_UnknownProjectReturns404: with a registry
// wired, listing an unknown project must 404 rather than returning an
// empty list that masks a project-ID typo.
func TestCoreAPI_ListTasks_UnknownProjectReturns404(t *testing.T) {
	listCalled := false
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			listCalled = true
			return nil, nil
		},
	}
	reg := testWebhookRegistry(t) // knows only "project-1"
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
		WithProjectRegistry(reg),
	)

	req := httptest.NewRequest(http.MethodGet, "/projects/ghost/tasks", nil)
	rec := httptest.NewRecorder()
	server.ListTasks(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code)
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "NOT_FOUND", env.Error.Code)
	assert.False(t, listCalled)
}

// TestCoreAPI_ListTasks_ScopedKeyForbidden: a request whose auth scope
// only allows project-1 must be denied 403 when it lists project-2,
// before the repo is consulted.
func TestCoreAPI_ListTasks_ScopedKeyForbidden(t *testing.T) {
	listCalled := false
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			listCalled = true
			return nil, nil
		},
	}
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(taskRepo))

	req := httptest.NewRequest(http.MethodGet, "/projects/project-2/tasks", nil)
	req = req.WithContext(context.WithValue(req.Context(), projectIDKey, []string{"project-1"}))
	rec := httptest.NewRecorder()
	server.ListTasks(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code)
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "FORBIDDEN", env.Error.Code)
	assert.False(t, listCalled)
}

// ---------------------------------------------------------------------
// CancelTask — id validation, terminal-state guard, wrong project, and
// the conditional-transition race conflict.
// ---------------------------------------------------------------------

// TestCoreAPI_CancelTask_MissingIDs: a request with no task segment
// must 400 before touching the repo.
func TestCoreAPI_CancelTask_MissingIDs(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{}
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(taskRepo))

	req := httptest.NewRequest(http.MethodPost, "/projects/project-1/tasks//cancel", nil)
	rec := httptest.NewRecorder()
	server.CancelTask(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "VALIDATION_ERROR", env.Error.Code)
}

// TestCoreAPI_CancelTask_TerminalStateRejected: a COMPLETED task is not
// cancellable — the fast-path read guard returns 400 INVALID_STATE and
// the atomic transition is never attempted.
func TestCoreAPI_CancelTask_TerminalStateRejected(t *testing.T) {
	transitionCalled := false
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, ProjectID: "project-1", Status: persistence.TaskStatusCompleted}, nil
		},
		TransitionToCancelledFunc: func(_ context.Context, _ string) (bool, error) {
			transitionCalled = true
			return false, nil
		},
	}
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(taskRepo))

	req := httptest.NewRequest(http.MethodPost, "/projects/project-1/tasks/task-1/cancel", nil)
	rec := httptest.NewRecorder()
	server.CancelTask(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "INVALID_STATE", env.Error.Code)
	assert.False(t, transitionCalled, "terminal task must be rejected before the atomic transition")
}

// TestCoreAPI_CancelTask_RaceLostReturns409: the read showed a
// cancellable state but the atomic conditional UPDATE didn't transition
// (a racing terminal write won). The handler must re-fetch and report
// 409 INVALID_STATE rather than claim success.
func TestCoreAPI_CancelTask_RaceLostReturns409(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, ProjectID: "project-1", Status: persistence.TaskStatusRunning}, nil
		},
		TransitionToCancelledFunc: func(_ context.Context, _ string) (bool, error) {
			return false, nil // row didn't transition
		},
	}
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(taskRepo))

	req := httptest.NewRequest(http.MethodPost, "/projects/project-1/tasks/task-1/cancel", nil)
	rec := httptest.NewRecorder()
	server.CancelTask(rec, req)

	require.Equal(t, http.StatusConflict, rec.Code)
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "INVALID_STATE", env.Error.Code)
}

// TestCoreAPI_CancelTask_RepoErrorReturns500: a Get failure that isn't
// not-found surfaces as INTERNAL_ERROR, not a misleading 404.
func TestCoreAPI_CancelTask_RepoErrorReturns500(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return nil, errors.New("connection reset")
		},
	}
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(taskRepo))

	req := httptest.NewRequest(http.MethodPost, "/projects/project-1/tasks/task-1/cancel", nil)
	rec := httptest.NewRecorder()
	server.CancelTask(rec, req)

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "INTERNAL_ERROR", env.Error.Code)
}

// ---------------------------------------------------------------------
// GetExecution — id validation and repo-not-wired guard.
// ---------------------------------------------------------------------

// TestCoreAPI_GetExecution_MissingID: no execution segment → 400
// VALIDATION_ERROR before any repo lookup.
func TestCoreAPI_GetExecution_MissingID(t *testing.T) {
	server := NewServer(WithLogger(zerolog.Nop()))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/executions/", nil)
	rec := httptest.NewRecorder()
	server.GetExecution(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "VALIDATION_ERROR", env.Error.Code)
}

// TestCoreAPI_GetExecution_NoRepoReturns500: with no execution
// repository wired the handler must 500 INTERNAL_ERROR rather than
// panic on a nil deref.
func TestCoreAPI_GetExecution_NoRepoReturns500(t *testing.T) {
	server := NewServer(WithLogger(zerolog.Nop()))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/executions/exec-1", nil)
	rec := httptest.NewRecorder()
	server.GetExecution(rec, req)

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "INTERNAL_ERROR", env.Error.Code)
}

// ---------------------------------------------------------------------
// Pure helpers backing the list/cancel handlers.
// ---------------------------------------------------------------------

// TestCoreAPI_ParsePageSizeClamps locks the page-size guard: empty/zero/
// negative fall back to the default, values above maxPageSize clamp to
// maxPageSize, and valid values pass through. Without the clamp a 0 or
// huge value reaches the DB LIMIT clause with the failure modes the
// handler comment documents.
func TestCoreAPI_ParsePageSizeClamps(t *testing.T) {
	cases := []struct {
		in   string
		def  int
		want int
	}{
		{"", 20, 20},
		{"0", 20, 20},
		{"-5", 20, 20},
		{"abc", 20, 20},
		{"7", 20, 7},
		{"1000", 20, maxPageSize},
		{"200", 20, 200},
	}
	for _, tc := range cases {
		assert.Equalf(t, tc.want, parsePageSize(tc.in, tc.def), "parsePageSize(%q,%d)", tc.in, tc.def)
	}
}

// TestCoreAPI_ParseOffsetFloorsAtZero: negatives and garbage floor to 0;
// valid offsets pass through.
func TestCoreAPI_ParseOffsetFloorsAtZero(t *testing.T) {
	assert.Equal(t, 0, parseOffset(""))
	assert.Equal(t, 0, parseOffset("-3"))
	assert.Equal(t, 0, parseOffset("nope"))
	assert.Equal(t, 12, parseOffset("12"))
}

// TestCoreAPI_IsKnownTaskStatus: every enum value the list filter
// accepts is recognised, and an arbitrary string is not — this is the
// gate that turns a typo into a 400 instead of a DB 500.
func TestCoreAPI_IsKnownTaskStatus(t *testing.T) {
	known := []persistence.TaskStatus{
		persistence.TaskStatusPending,
		persistence.TaskStatusQueued,
		persistence.TaskStatusLeased,
		persistence.TaskStatusRunning,
		persistence.TaskStatusWaitingForChildren,
		persistence.TaskStatusCompleted,
		persistence.TaskStatusFailed,
		persistence.TaskStatusCancelled,
	}
	for _, s := range known {
		assert.Truef(t, isKnownTaskStatus(s), "status %q must be known", s)
	}
	assert.False(t, isKnownTaskStatus(persistence.TaskStatus("BOGUS")))
	assert.False(t, isKnownTaskStatus(persistence.TaskStatus("running")), "enum check is case-sensitive; callers uppercase first")
}
