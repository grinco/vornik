package ui

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// fakeJudgeVerdictRepo stubs TaskJudgeVerdictRepository for the
// verdict-panel render branch.
type fakeJudgeVerdictRepo struct {
	v *persistence.TaskJudgeVerdict
}

func (f *fakeJudgeVerdictRepo) Record(context.Context, *persistence.TaskJudgeVerdict) error {
	return nil
}
func (f *fakeJudgeVerdictRepo) GetByTask(context.Context, string) (*persistence.TaskJudgeVerdict, error) {
	return f.v, nil
}
func (f *fakeJudgeVerdictRepo) ListRecent(context.Context, string, int) ([]*persistence.TaskJudgeVerdict, error) {
	return nil, nil
}

// TestTaskDetail_BlankIDReturns404 — the path-parsing guard.
func TestTaskDetail_BlankIDReturns404(t *testing.T) {
	srv := NewServer(WithTaskRepository(&mocks.MockTaskRepository{}))
	req := httptest.NewRequest(http.MethodGet, "/tasks/", nil)
	rec := httptest.NewRecorder()
	srv.TaskDetail(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestTaskDetail_NoRepoReturns404 — without a task repo wired the
// handler 404s rather than rendering an empty page (operator gets
// a clear signal that the deployment is misconfigured).
func TestTaskDetail_NoRepoReturns404(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/tasks/t1", nil)
	rec := httptest.NewRecorder()
	srv.TaskDetail(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestTaskDetail_TaskMissingReturns404 — repo returns nil task →
// the handler must serve 404.
func TestTaskDetail_TaskMissingReturns404(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(context.Context, string) (*persistence.Task, error) {
			return nil, errors.New("not found")
		},
	}
	srv := NewServer(WithTaskRepository(repo))
	req := httptest.NewRequest(http.MethodGet, "/tasks/missing", nil)
	rec := httptest.NewRecorder()
	srv.TaskDetail(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestTaskDetail_RendersJudgeVerdictPanel — when judge verdict
// repo is wired and the task has one, the verdict panel renders.
func TestTaskDetail_RendersJudgeVerdictPanel(t *testing.T) {
	taskID := "task_abc"
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			if id == taskID {
				return &persistence.Task{ID: taskID, ProjectID: "p1", Status: persistence.TaskStatusCompleted}, nil
			}
			return nil, nil
		},
		ListFunc: func(context.Context, persistence.TaskFilter) ([]*persistence.Task, error) {
			return nil, nil
		},
	}
	verdict := &persistence.TaskJudgeVerdict{
		Verdict:    "pass",
		Confidence: 0.92,
		Summary:    "looked good",
		Model:      "judge-haiku",
		Role:       "judge",
		RecordedAt: time.Now(),
	}
	srv := NewServer(
		WithTaskRepository(taskRepo),
		WithJudgeVerdictRepository(&fakeJudgeVerdictRepo{v: verdict}),
	)
	req := httptest.NewRequest(http.MethodGet, "/tasks/"+taskID, nil)
	rec := httptest.NewRecorder()
	srv.TaskDetail(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "looked good", "verdict summary should render")
}

// TestTaskDetail_PostMortemError_QueryParamSurfaces — when the
// redirect from TaskPostMortemGenerate appended ?post_mortem_error=
// the body should show that error banner.
func TestTaskDetail_PostMortemError_QueryParamSurfaces(t *testing.T) {
	taskID := "task_pmerr"
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return &persistence.Task{ID: taskID, ProjectID: "p1", Status: persistence.TaskStatusFailed}, nil
		},
		ListFunc: func(context.Context, persistence.TaskFilter) ([]*persistence.Task, error) {
			return nil, nil
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo))
	req := httptest.NewRequest(http.MethodGet,
		"/tasks/"+taskID+"?post_mortem_error=LLM+timed+out", nil)
	rec := httptest.NewRecorder()
	srv.TaskDetail(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "LLM timed out", "post-mortem error must render in banner")
}

// TestLoadTaskSiblings_FindsPrevAndNext — same project, three tasks,
// the middle one should report both neighbours.
func TestLoadTaskSiblings_FindsPrevAndNext(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(context.Context, persistence.TaskFilter) ([]*persistence.Task, error) {
			return []*persistence.Task{
				{ID: "task_c", ProjectID: "p1"}, // newest
				{ID: "task_b", ProjectID: "p1"},
				{ID: "task_a", ProjectID: "p1"}, // oldest
			}, nil
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo))
	prev, next := srv.loadTaskSiblings(context.Background(),
		&persistence.Task{ID: "task_b", ProjectID: "p1"})
	assert.Equal(t, "task_c", prev, "newer task should be prev")
	assert.Equal(t, "task_a", next, "older task should be next")
}

// TestLoadTaskSiblings_FirstTaskNoPrev — newest in the window has no
// "newer" sibling.
func TestLoadTaskSiblings_FirstTaskNoPrev(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(context.Context, persistence.TaskFilter) ([]*persistence.Task, error) {
			return []*persistence.Task{
				{ID: "task_c", ProjectID: "p1"},
				{ID: "task_b", ProjectID: "p1"},
			}, nil
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo))
	prev, next := srv.loadTaskSiblings(context.Background(),
		&persistence.Task{ID: "task_c", ProjectID: "p1"})
	assert.Equal(t, "", prev, "newest task has no prev")
	assert.Equal(t, "task_b", next)
}

// TestLoadTaskSiblings_LastTaskNoNext — symmetric to the prev case.
func TestLoadTaskSiblings_LastTaskNoNext(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(context.Context, persistence.TaskFilter) ([]*persistence.Task, error) {
			return []*persistence.Task{
				{ID: "task_b", ProjectID: "p1"},
				{ID: "task_a", ProjectID: "p1"},
			}, nil
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo))
	prev, next := srv.loadTaskSiblings(context.Background(),
		&persistence.Task{ID: "task_a", ProjectID: "p1"})
	assert.Equal(t, "task_b", prev)
	assert.Equal(t, "", next, "oldest task has no next")
}

// TestLoadTaskSiblings_ListErrorReturnsEmpty — DB read errors must
// not panic; they leave both pointers empty so arrows hide.
func TestLoadTaskSiblings_ListErrorReturnsEmpty(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(context.Context, persistence.TaskFilter) ([]*persistence.Task, error) {
			return nil, errors.New("db down")
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo))
	prev, next := srv.loadTaskSiblings(context.Background(),
		&persistence.Task{ID: "task_a", ProjectID: "p1"})
	assert.Equal(t, "", prev)
	assert.Equal(t, "", next)
}

// TestLoadTaskSiblings_NilTask — defensive nil-guard.
func TestLoadTaskSiblings_NilTask(t *testing.T) {
	srv := NewServer(WithTaskRepository(&mocks.MockTaskRepository{}))
	prev, next := srv.loadTaskSiblings(context.Background(), nil)
	assert.Equal(t, "", prev)
	assert.Equal(t, "", next)
}

// TestLoadTaskSiblings_NoRepo — without a task repo, returns empty.
func TestLoadTaskSiblings_NoRepo(t *testing.T) {
	srv := NewServer()
	prev, next := srv.loadTaskSiblings(context.Background(),
		&persistence.Task{ID: "task_a"})
	assert.Equal(t, "", prev)
	assert.Equal(t, "", next)
}

// TestLoadTaskSiblings_TaskNotInList — task not present in the
// window → empty neighbours.
func TestLoadTaskSiblings_TaskNotInList(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(context.Context, persistence.TaskFilter) ([]*persistence.Task, error) {
			return []*persistence.Task{
				{ID: "task_x", ProjectID: "p1"},
			}, nil
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo))
	prev, next := srv.loadTaskSiblings(context.Background(),
		&persistence.Task{ID: "task_y", ProjectID: "p1"})
	assert.Equal(t, "", prev)
	assert.Equal(t, "", next)
}

// TestTaskDetail_MobileBarHasLifecycleActions pins mobile/desktop
// feature parity for the action bar — operator reported 2026-05-23
// that phone view was missing Retry / Watch live / Cancel. The
// mobile bar lives inside a `md:hidden` block so the test asserts
// the action labels appear in the rendered HTML for a RUNNING
// task (Watch live + Cancel paths) and a FAILED task (Retry
// path). If a future template refactor drops one of these
// branches from the md:hidden bar, this test fails before the
// regression reaches mobile users.
func TestTaskDetail_MobileBarHasLifecycleActions(t *testing.T) {
	t.Run("running shows live + cancel in mobile bar", func(t *testing.T) {
		taskID := "task_mobile_running"
		taskRepo := &mocks.MockTaskRepository{
			GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
				return &persistence.Task{ID: taskID, ProjectID: "p1", Status: persistence.TaskStatusRunning}, nil
			},
			ListFunc: func(context.Context, persistence.TaskFilter) ([]*persistence.Task, error) { return nil, nil },
		}
		srv := NewServer(WithTaskRepository(taskRepo))
		req := httptest.NewRequest(http.MethodGet, "/tasks/"+taskID, nil)
		rec := httptest.NewRecorder()
		srv.TaskDetail(rec, req)
		require.Equal(t, http.StatusOK, rec.Code)
		body := rec.Body.String()
		// md:hidden bar must include both — desktop md:flex header
		// already includes them so plain Contains is enough to
		// catch absence (both desktop + mobile would have to drop
		// the action for this to false-negative, which is the
		// regression we want).
		assert.Contains(t, body, `/ui/tasks/`+taskID+`/live`, "mobile bar should link to live execution view")
		assert.Contains(t, body, `/ui/tasks/`+taskID+`/cancel`, "mobile bar should expose cancel for running tasks")
	})
	t.Run("failed shows retry in mobile bar", func(t *testing.T) {
		taskID := "task_mobile_failed"
		taskRepo := &mocks.MockTaskRepository{
			GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
				return &persistence.Task{ID: taskID, ProjectID: "p1", Status: persistence.TaskStatusFailed}, nil
			},
			ListFunc: func(context.Context, persistence.TaskFilter) ([]*persistence.Task, error) { return nil, nil },
		}
		srv := NewServer(WithTaskRepository(taskRepo))
		req := httptest.NewRequest(http.MethodGet, "/tasks/"+taskID, nil)
		rec := httptest.NewRecorder()
		srv.TaskDetail(rec, req)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, rec.Body.String(), `/ui/tasks/`+taskID+`/retry`, "mobile bar should expose retry for failed tasks")
	})
}

// renderTaskDetailBody renders task_detail.html directly from a
// TaskDetailData value. The page-level render path (TaskDetail
// handler) drives ChangelogContent / Execution / Conversation
// through repo loaders that are awkward to stub for a pure
// presentation assertion; rendering the template against an
// explicit view-model lets the tiering test pin the exact field
// combinations (open checkpoint, finished execution, changelog
// present) without wiring four repos. The handler-level branch
// tests above still cover the loaders.
func renderTaskDetailBody(t *testing.T, data TaskDetailData) string {
	t.Helper()
	srv := NewServer()
	var buf bytes.Buffer
	if err := srv.templates.ExecuteTemplate(&buf, "task_detail.html", data); err != nil {
		t.Fatalf("ExecuteTemplate(task_detail.html): %v", err)
	}
	return buf.String()
}

// TestTaskDetailTiering guards the Task-5 re-tier of task_detail.html:
//   - the awaiting-approval emphasis card carries .panel-primary,
//   - reference panels render as <details class="panel-ref …>,
//   - the context-aware fold is correct: a no-openWhen panel
//     (Changelog) is CLOSED for a RUNNING task while the Output
//     panel is OPEN for a COMPLETED task,
//   - the template carries no legacy gray-*/dark-* tokens.
func TestTaskDetailTiering(t *testing.T) {
	t.Run("awaiting-approval card is panel-primary", func(t *testing.T) {
		body := renderTaskDetailBody(t, TaskDetailData{
			Task:         &persistence.Task{ID: "task_appr", ProjectID: "p1", Status: persistence.TaskStatusAwaitingApproval},
			Conversation: TaskConversationView{Enabled: true},
		})
		// Scope to the card: panel-primary + data-tone also appear in the
		// pageHead CSS, so locate the card's outer <div> by its distinctive
		// tinted background and inspect that open tag.
		cIdx := strings.Index(body, `class="bg-sky-500/10`)
		require.GreaterOrEqual(t, cIdx, 0, "awaiting-approval card should render with its sky tint")
		end := strings.Index(body[cIdx:], ">")
		require.GreaterOrEqual(t, end, 0)
		cardTag := body[cIdx : cIdx+end]
		assert.Contains(t, cardTag, "panel-primary", "awaiting-approval emphasis card should use the panel-primary primitive")
		assert.Contains(t, cardTag, `data-tone="sky"`, "awaiting-approval card should carry the sky tone")
		assert.Contains(t, body, "Awaiting your approval", "awaiting-approval copy must survive the re-tier")
	})

	t.Run("changelog reference panel is CLOSED for a running task", func(t *testing.T) {
		body := renderTaskDetailBody(t, TaskDetailData{
			Task:             &persistence.Task{ID: "task_run", ProjectID: "p1", Status: persistence.TaskStatusRunning},
			ChangelogContent: "## v1\n- did a thing",
		})
		// The changelog panel takes no openWhen args → always closed.
		// Match the rendered <h2> text node (>Changelog<), not bare
		// "Changelog" — that's comment-independent (an HTML comment can't
		// produce >Changelog<) and mirrors the Output sub-test below.
		idx := strings.Index(body, ">Changelog<")
		require.GreaterOrEqual(t, idx, 0, "changelog panel should render when ChangelogContent is set")
		// Find the <details ...> that opens the changelog block. The
		// summary holds the title, so scan backwards from the title to
		// the enclosing details tag and assert it is not force-open.
		pre := body[:idx]
		dIdx := strings.LastIndex(pre, "<details")
		require.GreaterOrEqual(t, dIdx, 0, "changelog title should sit inside a <details>")
		openTag := body[dIdx:idx]
		assert.NotContains(t, openTag, " open", "changelog reference panel must be CLOSED for a RUNNING task")
		assert.Contains(t, openTag, "panel-ref", "changelog should render as a panel-ref reference panel")
	})

	t.Run("output reference panel is OPEN for a completed task", func(t *testing.T) {
		body := renderTaskDetailBody(t, TaskDetailData{
			Task:      &persistence.Task{ID: "task_done", ProjectID: "p1", Status: persistence.TaskStatusCompleted},
			Execution: &persistence.Execution{ID: "exec1", Result: []byte(`{"message":"all done"}`)},
		})
		idx := strings.Index(body, ">Output<")
		require.GreaterOrEqual(t, idx, 0, "output panel should render when the execution has a result")
		pre := body[:idx]
		dIdx := strings.LastIndex(pre, "<details")
		require.GreaterOrEqual(t, dIdx, 0, "output title should sit inside a <details>")
		openTag := body[dIdx:idx]
		assert.Contains(t, openTag, "panel-ref", "output should render as a panel-ref reference panel")
		assert.Contains(t, openTag, " open", "output reference panel must be OPEN for a COMPLETED task")
	})

	t.Run("no legacy tokens remain", func(t *testing.T) {
		assertNoLegacyTokens(t, "task_detail.html")
	})
}
