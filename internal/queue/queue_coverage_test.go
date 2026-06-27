package queue

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

func newQueueWithMetrics(t *testing.T, repo persistence.TaskRepository) (*Queue, *Metrics) {
	t.Helper()
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	opts := []QueueOption{WithMetrics(m)}
	if repo != nil {
		opts = append(opts, WithTaskRepository(repo))
	}
	return New(opts...), m
}

func TestEnqueueWithTimestamp_RecordsMetricsAndDepth(t *testing.T) {
	called := false
	repo := &mocks.MockTaskRepository{
		CountByStatusFunc: func(ctx context.Context, projectID string) (map[persistence.TaskStatus]int64, error) {
			called = true
			return map[persistence.TaskStatus]int64{persistence.TaskStatusQueued: 5}, nil
		},
	}
	q, m := newQueueWithMetrics(t, repo)

	err := q.EnqueueWithTimestamp("task-1", "proj-1", 10, time.Now().Add(-1*time.Second))
	if err != nil {
		t.Fatalf("EnqueueWithTimestamp: %v", err)
	}
	if !called {
		t.Error("expected updateQueueDepth → CountByStatus to be called")
	}
	if got := testutil.ToFloat64(m.EnqueuedTotal.WithLabelValues("proj-1")); got != 1 {
		t.Errorf("EnqueuedTotal[proj-1] = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.Depth.WithLabelValues("proj-1")); got != 5 {
		t.Errorf("Depth[proj-1] = %v, want 5 (from CountByStatus stub)", got)
	}
}

func TestEnqueueWithTimestamp_NilMetricsIsNoop(t *testing.T) {
	q := New()
	if err := q.EnqueueWithTimestamp("t", "p", 0, time.Now()); err != nil {
		t.Errorf("EnqueueWithTimestamp without metrics returned %v", err)
	}
}

func TestStats_DepthMetricUpdatedAndCountsPopulated(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		CountByStatusFunc: func(ctx context.Context, projectID string) (map[persistence.TaskStatus]int64, error) {
			return map[persistence.TaskStatus]int64{
				persistence.TaskStatusQueued:    4,
				persistence.TaskStatusRunning:   2,
				persistence.TaskStatusLeased:    1,
				persistence.TaskStatusCompleted: 10,
				persistence.TaskStatusFailed:    3,
			}, nil
		},
	}
	q, m := newQueueWithMetrics(t, repo)

	stats, err := q.Stats(context.Background(), "proj-S")
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Queued != 4 || stats.Running != 2 || stats.Leased != 1 || stats.Completed != 10 || stats.Failed != 3 {
		t.Errorf("stats mismatch: %+v", stats)
	}
	if v := testutil.ToFloat64(m.Depth.WithLabelValues("proj-S")); v != 4 {
		t.Errorf("Depth = %v, want 4", v)
	}
}

func TestStats_PropagatesRepoError(t *testing.T) {
	want := errors.New("count broken")
	repo := &mocks.MockTaskRepository{
		CountByStatusFunc: func(ctx context.Context, projectID string) (map[persistence.TaskStatus]int64, error) {
			return nil, want
		},
	}
	q, _ := newQueueWithMetrics(t, repo)
	if _, err := q.Stats(context.Background(), "p"); !errors.Is(err, want) {
		t.Errorf("got %v, want %v", err, want)
	}
}

func TestStats_NoRepoReturnsErrNoRepository(t *testing.T) {
	q := New()
	if _, err := q.Stats(context.Background(), "p"); !errors.Is(err, ErrNoRepository) {
		t.Errorf("got %v, want ErrNoRepository", err)
	}
}

func TestUpdateQueueDepth_RepoErrorIsSilentlySkipped(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		CountByStatusFunc: func(ctx context.Context, projectID string) (map[persistence.TaskStatus]int64, error) {
			return nil, errors.New("transient")
		},
	}
	q, m := newQueueWithMetrics(t, repo)
	// Drive through Enqueue which calls updateQueueDepth.
	if err := q.Enqueue("t", "proj-X", 0); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if v := testutil.ToFloat64(m.Depth.WithLabelValues("proj-X")); v != 0 {
		t.Errorf("Depth = %v on repo error, want 0 (no update)", v)
	}
}

func TestUpdateQueueDepth_NoMetricsIsNoop(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		CountByStatusFunc: func(ctx context.Context, projectID string) (map[persistence.TaskStatus]int64, error) {
			t.Fatal("CountByStatus should NOT be called when metrics is nil")
			return nil, nil
		},
	}
	q := New(WithTaskRepository(repo))
	// Enqueue path skips updateQueueDepth body via q.metrics nil guard.
	if err := q.Enqueue("t", "p", 0); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
}
