// Package api provides HTTP handlers for the vornik data plane API.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
)

// mockAutonomyEvaluationRepository is a test double for AutonomyEvaluationRepository
type mockAutonomyEvaluationRepository struct {
	recordErr error
	listFunc  func(ctx context.Context, filter persistence.AutonomyEvaluationFilter) ([]*persistence.AutonomyEvaluation, error)
	listCall  persistence.AutonomyEvaluationFilter
	recorded  []*persistence.AutonomyEvaluation
	countFunc func(ctx context.Context, projectID string, since, until time.Time) (map[string]int64, error)
}

func (m *mockAutonomyEvaluationRepository) Record(ctx context.Context, e *persistence.AutonomyEvaluation) error {
	if m.recordErr != nil {
		return m.recordErr
	}
	m.recorded = append(m.recorded, e)
	return nil
}

func (m *mockAutonomyEvaluationRepository) List(ctx context.Context, filter persistence.AutonomyEvaluationFilter) ([]*persistence.AutonomyEvaluation, error) {
	m.listCall = filter
	if m.listFunc != nil {
		return m.listFunc(ctx, filter)
	}
	return []*persistence.AutonomyEvaluation{}, nil
}

func (m *mockAutonomyEvaluationRepository) CountByOutcome(ctx context.Context, projectID string, since, until time.Time) (map[string]int64, error) {
	if m.countFunc != nil {
		return m.countFunc(ctx, projectID, since, until)
	}
	return map[string]int64{}, nil
}

func TestListAutonomyEvaluations(t *testing.T) {
	t.Run("requires GET method", func(t *testing.T) {
		srv := &Server{}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/autonomy/evaluations", nil)
		rec := httptest.NewRecorder()

		srv.ListAutonomyEvaluations(rec, req)

		assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
		assert.Contains(t, rec.Body.String(), "METHOD_NOT_ALLOWED")
	})

	t.Run("validates project_id present", func(t *testing.T) {
		srv := &Server{}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/projects//autonomy/evaluations", nil)
		rec := httptest.NewRecorder()

		srv.ListAutonomyEvaluations(rec, req)

		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, rec.Body.String(), "VALIDATION_ERROR")
	})

	t.Run("returns empty array when repo is nil", func(t *testing.T) {
		srv := &Server{}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/autonomy/evaluations", nil)
		rec := httptest.NewRecorder()

		srv.ListAutonomyEvaluations(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)

		var resp map[string]any
		err := json.NewDecoder(rec.Body).Decode(&resp)
		require.NoError(t, err)
		evaluations, ok := resp["evaluations"].([]any)
		require.True(t, ok)
		assert.Empty(t, evaluations)
		total, ok := resp["total"].(float64)
		require.True(t, ok)
		assert.Equal(t, 0.0, total)
	})

	t.Run("honors limit parameter", func(t *testing.T) {
		mockRepo := &mockAutonomyEvaluationRepository{
			listFunc: func(ctx context.Context, filter persistence.AutonomyEvaluationFilter) ([]*persistence.AutonomyEvaluation, error) {
				assert.Equal(t, 100, filter.PageSize)
				return []*persistence.AutonomyEvaluation{}, nil
			},
		}
		srv := &Server{autonomyEvalRepo: mockRepo}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/autonomy/evaluations?limit=100", nil)
		rec := httptest.NewRecorder()

		srv.ListAutonomyEvaluations(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("caps limit at 500", func(t *testing.T) {
		mockRepo := &mockAutonomyEvaluationRepository{
			listFunc: func(ctx context.Context, filter persistence.AutonomyEvaluationFilter) ([]*persistence.AutonomyEvaluation, error) {
				assert.Equal(t, 500, filter.PageSize)
				return []*persistence.AutonomyEvaluation{}, nil
			},
		}
		srv := &Server{autonomyEvalRepo: mockRepo}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/autonomy/evaluations?limit=999", nil)
		rec := httptest.NewRecorder()

		srv.ListAutonomyEvaluations(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("default limit is 50", func(t *testing.T) {
		mockRepo := &mockAutonomyEvaluationRepository{
			listFunc: func(ctx context.Context, filter persistence.AutonomyEvaluationFilter) ([]*persistence.AutonomyEvaluation, error) {
				assert.Equal(t, 50, filter.PageSize)
				return []*persistence.AutonomyEvaluation{}, nil
			},
		}
		srv := &Server{autonomyEvalRepo: mockRepo}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/autonomy/evaluations", nil)
		rec := httptest.NewRecorder()

		srv.ListAutonomyEvaluations(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("invalid limit falls back to 50", func(t *testing.T) {
		mockRepo := &mockAutonomyEvaluationRepository{
			listFunc: func(ctx context.Context, filter persistence.AutonomyEvaluationFilter) ([]*persistence.AutonomyEvaluation, error) {
				assert.Equal(t, 50, filter.PageSize)
				return []*persistence.AutonomyEvaluation{}, nil
			},
		}
		srv := &Server{autonomyEvalRepo: mockRepo}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/autonomy/evaluations?limit=abc", nil)
		rec := httptest.NewRecorder()

		srv.ListAutonomyEvaluations(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("zero limit falls back to 50", func(t *testing.T) {
		mockRepo := &mockAutonomyEvaluationRepository{
			listFunc: func(ctx context.Context, filter persistence.AutonomyEvaluationFilter) ([]*persistence.AutonomyEvaluation, error) {
				assert.Equal(t, 50, filter.PageSize)
				return []*persistence.AutonomyEvaluation{}, nil
			},
		}
		srv := &Server{autonomyEvalRepo: mockRepo}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/autonomy/evaluations?limit=0", nil)
		rec := httptest.NewRecorder()

		srv.ListAutonomyEvaluations(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("filters by outcome", func(t *testing.T) {
		mockRepo := &mockAutonomyEvaluationRepository{
			listFunc: func(ctx context.Context, filter persistence.AutonomyEvaluationFilter) ([]*persistence.AutonomyEvaluation, error) {
				require.NotNil(t, filter.Outcome)
				assert.Equal(t, "PASSED", *filter.Outcome)
				return []*persistence.AutonomyEvaluation{}, nil
			},
		}
		srv := &Server{autonomyEvalRepo: mockRepo}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/autonomy/evaluations?outcome=PASSED", nil)
		rec := httptest.NewRecorder()

		srv.ListAutonomyEvaluations(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("repo error returns 500", func(t *testing.T) {
		mockRepo := &mockAutonomyEvaluationRepository{
			listFunc: func(ctx context.Context, filter persistence.AutonomyEvaluationFilter) ([]*persistence.AutonomyEvaluation, error) {
				return nil, assert.AnError
			},
		}
		srv := &Server{autonomyEvalRepo: mockRepo}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/autonomy/evaluations", nil)
		rec := httptest.NewRecorder()

		srv.ListAutonomyEvaluations(rec, req)

		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.Contains(t, rec.Body.String(), "INTERNAL_ERROR")
	})
}

func TestGetAutonomyEvaluationSummary(t *testing.T) {
	t.Run("requires GET method", func(t *testing.T) {
		srv := &Server{}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/autonomy/summary", nil)
		rec := httptest.NewRecorder()

		srv.GetAutonomyEvaluationSummary(rec, req)

		assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
		assert.Contains(t, rec.Body.String(), "METHOD_NOT_ALLOWED")
	})

	t.Run("validates project_id present", func(t *testing.T) {
		srv := &Server{}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/projects//autonomy/summary", nil)
		rec := httptest.NewRecorder()

		srv.GetAutonomyEvaluationSummary(rec, req)

		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, rec.Body.String(), "VALIDATION_ERROR")
	})

	t.Run("returns empty counts when repo is nil", func(t *testing.T) {
		srv := &Server{}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/autonomy/summary", nil)
		rec := httptest.NewRecorder()

		srv.GetAutonomyEvaluationSummary(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)

		var resp map[string]any
		err := json.NewDecoder(rec.Body).Decode(&resp)
		require.NoError(t, err)
		assert.Equal(t, "p1", resp["projectId"])
		assert.Equal(t, 24.0, resp["windowHrs"])
		counts, ok := resp["counts"].(map[string]any)
		require.True(t, ok)
		assert.Empty(t, counts)
	})

	t.Run("honors hours parameter", func(t *testing.T) {
		mockRepo := &mockAutonomyEvaluationRepository{
			countFunc: func(ctx context.Context, projectID string, since, until time.Time) (map[string]int64, error) {
				// window should be 12 hours
				expectedSince := time.Now().UTC().Add(-12 * time.Hour)
				assert.WithinDuration(t, expectedSince, since, 5*time.Second)
				assert.True(t, until.IsZero())
				return map[string]int64{"PASSED": 5}, nil
			},
		}
		srv := &Server{autonomyEvalRepo: mockRepo}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/autonomy/summary?hours=12", nil)
		rec := httptest.NewRecorder()

		srv.GetAutonomyEvaluationSummary(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)

		var resp map[string]any
		err := json.NewDecoder(rec.Body).Decode(&resp)
		require.NoError(t, err)
		assert.Equal(t, 12.0, resp["windowHrs"])
	})

	t.Run("caps hours at 30 days", func(t *testing.T) {
		mockRepo := &mockAutonomyEvaluationRepository{
			countFunc: func(ctx context.Context, projectID string, since, until time.Time) (map[string]int64, error) {
				// window should be capped at 30 days (720 hours)
				expectedSince := time.Now().UTC().Add(-720 * time.Hour)
				assert.WithinDuration(t, expectedSince, since, 5*time.Second)
				return map[string]int64{}, nil
			},
		}
		srv := &Server{autonomyEvalRepo: mockRepo}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/autonomy/summary?hours=800", nil)
		rec := httptest.NewRecorder()

		srv.GetAutonomyEvaluationSummary(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("default hours is 24", func(t *testing.T) {
		mockRepo := &mockAutonomyEvaluationRepository{
			countFunc: func(ctx context.Context, projectID string, since, until time.Time) (map[string]int64, error) {
				expectedSince := time.Now().UTC().Add(-24 * time.Hour)
				assert.WithinDuration(t, expectedSince, since, 5*time.Second)
				return map[string]int64{}, nil
			},
		}
		srv := &Server{autonomyEvalRepo: mockRepo}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/autonomy/summary", nil)
		rec := httptest.NewRecorder()

		srv.GetAutonomyEvaluationSummary(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("invalid hours falls back to 24", func(t *testing.T) {
		mockRepo := &mockAutonomyEvaluationRepository{
			countFunc: func(ctx context.Context, projectID string, since, until time.Time) (map[string]int64, error) {
				expectedSince := time.Now().UTC().Add(-24 * time.Hour)
				assert.WithinDuration(t, expectedSince, since, 5*time.Second)
				return map[string]int64{}, nil
			},
		}
		srv := &Server{autonomyEvalRepo: mockRepo}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/autonomy/summary?hours=abc", nil)
		rec := httptest.NewRecorder()

		srv.GetAutonomyEvaluationSummary(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("zero hours falls back to 24", func(t *testing.T) {
		mockRepo := &mockAutonomyEvaluationRepository{
			countFunc: func(ctx context.Context, projectID string, since, until time.Time) (map[string]int64, error) {
				expectedSince := time.Now().UTC().Add(-24 * time.Hour)
				assert.WithinDuration(t, expectedSince, since, 5*time.Second)
				return map[string]int64{}, nil
			},
		}
		srv := &Server{autonomyEvalRepo: mockRepo}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/autonomy/summary?hours=0", nil)
		rec := httptest.NewRecorder()

		srv.GetAutonomyEvaluationSummary(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("returns aggregated counts", func(t *testing.T) {
		mockRepo := &mockAutonomyEvaluationRepository{
			countFunc: func(ctx context.Context, projectID string, since, until time.Time) (map[string]int64, error) {
				return map[string]int64{"PASSED": 7, "FAILED": 2}, nil
			},
		}
		srv := &Server{autonomyEvalRepo: mockRepo}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/autonomy/summary", nil)
		rec := httptest.NewRecorder()

		srv.GetAutonomyEvaluationSummary(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)

		var resp map[string]any
		err := json.NewDecoder(rec.Body).Decode(&resp)
		require.NoError(t, err)
		counts, ok := resp["counts"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, 7.0, counts["PASSED"])
		assert.Equal(t, 2.0, counts["FAILED"])
	})

	t.Run("repo error returns 500", func(t *testing.T) {
		mockRepo := &mockAutonomyEvaluationRepository{
			countFunc: func(ctx context.Context, projectID string, since, until time.Time) (map[string]int64, error) {
				return nil, assert.AnError
			},
		}
		srv := &Server{autonomyEvalRepo: mockRepo}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/autonomy/summary", nil)
		rec := httptest.NewRecorder()

		srv.GetAutonomyEvaluationSummary(rec, req)

		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.Contains(t, rec.Body.String(), "INTERNAL_ERROR")
	})
}
