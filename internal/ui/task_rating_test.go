package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/persistence/repotest"
)

// stubUIRatingRepo records what the page and the action did.
type stubUIRatingRepo struct {
	rows    map[string]*persistence.ExecutionRating
	upserts []*persistence.ExecutionRating
	deletes []string
}

func newStubUIRatingRepo() *stubUIRatingRepo {
	return &stubUIRatingRepo{rows: map[string]*persistence.ExecutionRating{}}
}

func (s *stubUIRatingRepo) Upsert(_ context.Context, r *persistence.ExecutionRating) error {
	cp := *r
	s.upserts = append(s.upserts, &cp)
	s.rows[r.ExecutionID+"|"+r.RaterID] = &cp
	return nil
}

func (s *stubUIRatingRepo) Get(_ context.Context, executionID, raterID string) (*persistence.ExecutionRating, error) {
	if r, ok := s.rows[executionID+"|"+raterID]; ok {
		return r, nil
	}
	return nil, persistence.ErrNotFound
}

func (s *stubUIRatingRepo) ListByExecution(context.Context, string) ([]*persistence.ExecutionRating, error) {
	return nil, nil
}

func (s *stubUIRatingRepo) Delete(_ context.Context, executionID, raterID string) error {
	s.deletes = append(s.deletes, executionID+"|"+raterID)
	delete(s.rows, executionID+"|"+raterID)
	return nil
}

func (s *stubUIRatingRepo) DeleteOlderThan(context.Context, time.Time) (int64, error) { return 0, nil }

// ratingExecRepo returns one execution for the task, which is the run the page's
// control rates.
func ratingExecRepo(taskID string) *mocks.MockExecutionRepository {
	return &mocks.MockExecutionRepository{
		ListFunc: func(context.Context, persistence.ExecutionFilter) ([]*persistence.Execution, error) {
			return []*persistence.Execution{{ID: "exec_1", TaskID: taskID, ProjectID: "p1"}}, nil
		},
	}
}

// The double must agree with production about absence, or the page's "you have
// not rated this" branch is certified by a stub that never produces it.
func TestStubUIRatingRepo_HonoursTheMissContract(t *testing.T) {
	repo := newStubUIRatingRepo()
	repotest.AssertMiss(t, "ExecutionRatingRepository.Get",
		func() (*persistence.ExecutionRating, error) {
			return repo.Get(context.Background(), "exec-absent", "op-nobody")
		})
}

func ratingTaskRepo(taskID string) *mocks.MockTaskRepository {
	return &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return &persistence.Task{ID: taskID, ProjectID: "p1", Status: persistence.TaskStatusCompleted}, nil
		},
		ListFunc: func(context.Context, persistence.TaskFilter) ([]*persistence.Task, error) {
			return nil, nil
		},
	}
}

// The control appears on the page, and says which way the caller already voted
// so a second click is a change rather than a guess.
func TestTaskDetail_RendersTheRatingControl(t *testing.T) {
	taskID := "task_rate_1"
	ratings := newStubUIRatingRepo()
	ratings.rows["exec_1|op_1"] = &persistence.ExecutionRating{
		ExecutionID: "exec_1", RaterID: "op_1", Verdict: persistence.VerdictDown,
		Reason: "thin", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	srv := NewServer(
		WithTaskRepository(ratingTaskRepo(taskID)),
		WithExecutionRepository(ratingExecRepo(taskID)),
		WithUIExecutionRatingRepository(ratings),
	)

	req := httptest.NewRequest(http.MethodGet, "/tasks/"+taskID, nil)
	req.Header.Set("X-Operator-Id", "op_1")
	req = authDisabledUIRequest(req)
	rec := httptest.NewRecorder()
	srv.TaskDetail(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "/tasks/"+taskID+"/rate", "no rating form on the page")
	assert.Contains(t, body, "thin", "the caller's own reason is not shown back to them")
}

// Without a repo wired the page must still render — a missing optional surface
// is not a broken task page.
func TestTaskDetail_RendersWithoutARatingRepo(t *testing.T) {
	taskID := "task_rate_2"
	srv := NewServer(WithTaskRepository(ratingTaskRepo(taskID)))
	req := httptest.NewRequest(http.MethodGet, "/tasks/"+taskID, nil)
	rec := httptest.NewRecorder()
	srv.TaskDetail(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestTaskRate_UpsertsAndRedirectsBack(t *testing.T) {
	taskID := "task_rate_3"
	ratings := newStubUIRatingRepo()
	srv := NewServer(
		WithTaskRepository(ratingTaskRepo(taskID)),
		WithExecutionRepository(ratingExecRepo(taskID)),
		WithUIExecutionRatingRepository(ratings),
	)

	form := url.Values{"verdict": {"up"}, "reason": {"clear and on format"}}
	req := httptest.NewRequest(http.MethodPost, "/tasks/"+taskID+"/rate",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Operator-Id", "op_1")
	req = authDisabledUIRequest(req)
	rec := httptest.NewRecorder()
	srv.TaskRate(rec, req, taskID)

	require.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, "/ui/tasks/"+taskID, rec.Header().Get("Location"))
	require.Len(t, ratings.upserts, 1)
	assert.Equal(t, "up", ratings.upserts[0].Verdict)
	assert.Equal(t, "op_1", ratings.upserts[0].RaterID)
	assert.Equal(t, "exec_1", ratings.upserts[0].ExecutionID,
		"the rating must attach to the execution, not the task")
}

// An unidentified caller cannot rate. The page-level equivalent of the API's
// 401 — and it must not write an anonymous row instead.
func TestTaskRate_RefusesAnUnidentifiedCaller(t *testing.T) {
	taskID := "task_rate_4"
	ratings := newStubUIRatingRepo()
	srv := NewServer(
		WithTaskRepository(ratingTaskRepo(taskID)),
		WithExecutionRepository(ratingExecRepo(taskID)),
		WithUIExecutionRatingRepository(ratings),
	)

	form := url.Values{"verdict": {"up"}}
	req := httptest.NewRequest(http.MethodPost, "/tasks/"+taskID+"/rate",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// Deliberately NOT authDisabledUIRequest: with auth off the resolver falls
	// back to the single-tenant operator id and always yields an identity. The
	// unidentified case is auth ENABLED with no Identity stamped, which is what
	// a bare request is (absence is treated as enabled, fail-closed).
	rec := httptest.NewRecorder()
	srv.TaskRate(rec, req, taskID)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Empty(t, ratings.upserts, "an unidentified caller wrote a rating")
}

// The clear button withdraws rather than posting an empty verdict.
func TestTaskRate_ClearWithdrawsTheRating(t *testing.T) {
	taskID := "task_rate_5"
	ratings := newStubUIRatingRepo()
	ratings.rows["exec_1|op_1"] = &persistence.ExecutionRating{
		ExecutionID: "exec_1", RaterID: "op_1", Verdict: persistence.VerdictUp,
	}
	srv := NewServer(
		WithTaskRepository(ratingTaskRepo(taskID)),
		WithExecutionRepository(ratingExecRepo(taskID)),
		WithUIExecutionRatingRepository(ratings),
	)

	form := url.Values{"clear": {"1"}}
	req := httptest.NewRequest(http.MethodPost, "/tasks/"+taskID+"/rate",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Operator-Id", "op_1")
	req = authDisabledUIRequest(req)
	rec := httptest.NewRecorder()
	srv.TaskRate(rec, req, taskID)

	require.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, []string{"exec_1|op_1"}, ratings.deletes)
	assert.Empty(t, ratings.upserts)
}

// A verdict outside the closed set is refused here too, so a hand-crafted form
// post cannot introduce a third value.
func TestTaskRate_RefusesAThirdVerdict(t *testing.T) {
	taskID := "task_rate_6"
	ratings := newStubUIRatingRepo()
	srv := NewServer(
		WithTaskRepository(ratingTaskRepo(taskID)),
		WithExecutionRepository(ratingExecRepo(taskID)),
		WithUIExecutionRatingRepository(ratings),
	)

	form := url.Values{"verdict": {"sideways"}}
	req := httptest.NewRequest(http.MethodPost, "/tasks/"+taskID+"/rate",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Operator-Id", "op_1")
	req = authDisabledUIRequest(req)
	rec := httptest.NewRecorder()
	srv.TaskRate(rec, req, taskID)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, ratings.upserts)
}
