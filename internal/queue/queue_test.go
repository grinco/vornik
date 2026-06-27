package queue

import (
	"context"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

func TestNew(t *testing.T) {
	t.Run("creates queue with default values", func(t *testing.T) {
		q := New()
		require.NotNil(t, q)
		assert.Nil(t, q.taskRepo)
	})

	t.Run("applies functional options", func(t *testing.T) {
		mockRepo := &mocks.MockTaskRepository{}
		logger := zerolog.Nop()

		q := New(
			WithTaskRepository(mockRepo),
			WithLogger(logger),
		)

		require.NotNil(t, q)
		assert.Equal(t, mockRepo, q.taskRepo)
		assert.Equal(t, logger, q.logger)
	})
}

func TestWithTaskRepository(t *testing.T) {
	mockRepo := &mocks.MockTaskRepository{}
	opt := WithTaskRepository(mockRepo)

	q := &Queue{}
	opt(q)

	assert.Equal(t, mockRepo, q.taskRepo)
}

func TestWithLogger(t *testing.T) {
	logger := zerolog.Nop()
	opt := WithLogger(logger)

	q := &Queue{}
	opt(q)

	assert.Equal(t, logger, q.logger)
}

func TestQueue_Enqueue(t *testing.T) {
	t.Run("enqueues task successfully", func(t *testing.T) {
		q := New()
		err := q.Enqueue("task-123", "project-1", 10)
		assert.NoError(t, err)
	})

	t.Run("enqueues with debug logging enabled", func(t *testing.T) {
		logger := zerolog.New(zerolog.Nop()).Level(zerolog.DebugLevel)
		q := New(WithLogger(logger))
		err := q.Enqueue("task-456", "project-2", 5)
		assert.NoError(t, err)
	})

	t.Run("enqueues multiple tasks", func(t *testing.T) {
		q := New()
		for i := 0; i < 10; i++ {
			err := q.Enqueue("task-%d", "project-1", i)
			assert.NoError(t, err)
		}
	})
}

func TestQueue_Release(t *testing.T) {
	t.Run("returns error when no repository configured", func(t *testing.T) {
		q := New()
		err := q.Release(context.Background(), "task-123", "lease-1", persistence.TaskStatusCompleted, persistence.ReleaseOptions{})
		assert.ErrorIs(t, err, ErrNoRepository)
	})

	t.Run("releases task successfully", func(t *testing.T) {
		mockRepo := &mocks.MockTaskRepository{
			ReleaseLeaseFunc: func(ctx context.Context, taskID, leaseID string, newStatus persistence.TaskStatus, opts persistence.ReleaseOptions) error {
				assert.Equal(t, "task-123", taskID)
				assert.Equal(t, persistence.TaskStatusCompleted, newStatus)
				return nil
			},
		}

		q := New(WithTaskRepository(mockRepo))
		err := q.Release(context.Background(), "task-123", "lease-1", persistence.TaskStatusCompleted, persistence.ReleaseOptions{})

		assert.NoError(t, err)
	})
}

func TestQueue_FindExpiredLeases(t *testing.T) {
	t.Run("returns error when no repository configured", func(t *testing.T) {
		q := New()
		tasks, err := q.FindExpiredLeases(context.Background(), 10)
		assert.Nil(t, tasks)
		assert.ErrorIs(t, err, ErrNoRepository)
	})

	t.Run("finds expired leases", func(t *testing.T) {
		mockRepo := &mocks.MockTaskRepository{
			FindExpiredLeasesFunc: func(ctx context.Context, limit int) ([]*persistence.Task, error) {
				return []*persistence.Task{
					{ID: "task-1", Status: persistence.TaskStatusLeased},
					{ID: "task-2", Status: persistence.TaskStatusLeased},
				}, nil
			},
		}

		q := New(WithTaskRepository(mockRepo))
		tasks, err := q.FindExpiredLeases(context.Background(), 10)

		require.NoError(t, err)
		assert.Len(t, tasks, 2)
	})
}

func TestQueue_MoveToDLQ(t *testing.T) {
	t.Run("returns error when no repository configured", func(t *testing.T) {
		q := New()
		err := q.MoveToDLQ(context.Background(), "task-1", "project-1")
		assert.ErrorIs(t, err, ErrNoRepository)
	})

	t.Run("returns explicit not implemented error", func(t *testing.T) {
		q := New(WithTaskRepository(&mocks.MockTaskRepository{}))
		err := q.MoveToDLQ(context.Background(), "task-1", "project-1")
		assert.ErrorIs(t, err, ErrDLQNotImplemented)
		assert.Contains(t, err.Error(), "task-1")
	})
}

func TestQueue_Stats(t *testing.T) {
	t.Run("returns error when no repository configured", func(t *testing.T) {
		q := New()
		stats, err := q.Stats(context.Background(), "project-1")
		assert.Nil(t, stats)
		assert.ErrorIs(t, err, ErrNoRepository)
	})

	t.Run("returns stats from repository", func(t *testing.T) {
		mockRepo := &mocks.MockTaskRepository{
			CountByStatusFunc: func(ctx context.Context, projectID string) (map[persistence.TaskStatus]int64, error) {
				assert.Equal(t, "project-1", projectID)
				return map[persistence.TaskStatus]int64{
					persistence.TaskStatusQueued:    5,
					persistence.TaskStatusRunning:   2,
					persistence.TaskStatusCompleted: 10,
				}, nil
			},
		}

		q := New(WithTaskRepository(mockRepo))
		stats, err := q.Stats(context.Background(), "project-1")

		require.NoError(t, err)
		require.NotNil(t, stats)
		assert.Equal(t, int64(5), stats.Queued)
		assert.Equal(t, int64(2), stats.Running)
		assert.Equal(t, int64(10), stats.Completed)
	})
}

func TestQueueError(t *testing.T) {
	err := ErrNoRepository
	assert.Equal(t, "no task repository configured", err.Error())
}
