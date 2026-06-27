// Package queue provides task queue management for vornik.
package queue

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence"
)

// Queue manages the durable task queue.
//
// It owns enqueue accounting (vornik_queue_enqueued_total) and the queued
// depth gauge (vornik_queue_depth). Leasing is owned by the scheduler, which
// acquires leases directly off the task repository and emits the lease
// counters/latency under the vornik_scheduler_* namespace — so the queue does
// NOT count leases or dequeues (that would double-count the scheduler).
type Queue struct {
	taskRepo persistence.TaskRepository
	logger   zerolog.Logger
	metrics  *Metrics
}

// QueueOption is a functional option for configuring the Queue.
type QueueOption func(*Queue)

// WithTaskRepository sets the task repository.
func WithTaskRepository(repo persistence.TaskRepository) QueueOption {
	return func(q *Queue) {
		q.taskRepo = repo
	}
}

// WithLogger sets the logger.
func WithLogger(logger zerolog.Logger) QueueOption {
	return func(q *Queue) {
		q.logger = logger
	}
}

// WithMetrics sets the metrics instance.
func WithMetrics(metrics *Metrics) QueueOption {
	return func(q *Queue) {
		q.metrics = metrics
	}
}

// WithPrometheusRegistry creates metrics with the given Prometheus registry.
// This is a convenience option that creates a new Metrics instance.
func WithPrometheusRegistry(registry *prometheus.Registry) QueueOption {
	return func(q *Queue) {
		if registry != nil {
			q.metrics = NewMetrics(registry)
		}
	}
}

// New creates a new Queue instance.
func New(opts ...QueueOption) *Queue {
	q := &Queue{
		logger: zerolog.Nop(),
	}

	for _, opt := range opts {
		opt(q)
	}

	return q
}

// Enqueue adds a task to the queue.
// The task should already be persisted with status QUEUED.
// This method exists primarily for logging and metrics.
func (q *Queue) Enqueue(taskID string, projectID string, priority int) error {
	if q.logger.Debug().Enabled() {
		q.logger.Debug().
			Str("task_id", taskID).
			Str("project_id", projectID).
			Int("priority", priority).
			Msg("task enqueued")
	}

	// Record metrics
	if q.metrics != nil {
		q.metrics.EnqueuedTotal.WithLabelValues(projectID).Inc()
		q.updateQueueDepth(projectID)
	}

	return nil
}

// EnqueueWithTimestamp adds a task to the queue with a specific creation timestamp.
// This is useful for calculating accurate queue latency metrics.
func (q *Queue) EnqueueWithTimestamp(taskID string, projectID string, priority int, createdAt time.Time) error {
	if q.logger.Debug().Enabled() {
		q.logger.Debug().
			Str("task_id", taskID).
			Str("project_id", projectID).
			Int("priority", priority).
			Msg("task enqueued")
	}

	// Record metrics
	if q.metrics != nil {
		q.metrics.EnqueuedTotal.WithLabelValues(projectID).Inc()
		q.updateQueueDepth(projectID)
	}

	return nil
}

// Release releases a task lease.
func (q *Queue) Release(ctx context.Context, taskID, leaseID string, newStatus persistence.TaskStatus, opts persistence.ReleaseOptions) error {
	if q.taskRepo == nil {
		return ErrNoRepository
	}

	err := q.taskRepo.ReleaseLease(ctx, taskID, leaseID, newStatus, opts)
	if err != nil {
		return err
	}

	return nil
}

// FindExpiredLeases finds tasks with expired leases.
func (q *Queue) FindExpiredLeases(ctx context.Context, limit int) ([]*persistence.Task, error) {
	if q.taskRepo == nil {
		return nil, ErrNoRepository
	}

	return q.taskRepo.FindExpiredLeases(ctx, limit)
}

// MoveToDLQ moves a task to the dead letter queue.
// This is called when a task has exceeded its maximum retry attempts.
func (q *Queue) MoveToDLQ(ctx context.Context, taskID string, projectID string) error {
	if q.taskRepo == nil {
		return ErrNoRepository
	}

	return fmt.Errorf("move task %s to DLQ: %w", taskID, ErrDLQNotImplemented)
}

// Stats returns queue statistics.
func (q *Queue) Stats(ctx context.Context, projectID string) (*QueueStats, error) {
	if q.taskRepo == nil {
		return nil, ErrNoRepository
	}

	counts, err := q.taskRepo.CountByStatus(ctx, projectID)
	if err != nil {
		return nil, err
	}

	stats := &QueueStats{
		Queued:    counts[persistence.TaskStatusQueued],
		Running:   counts[persistence.TaskStatusRunning],
		Leased:    counts[persistence.TaskStatusLeased],
		Completed: counts[persistence.TaskStatusCompleted],
		Failed:    counts[persistence.TaskStatusFailed],
	}

	// Update depth metric
	if q.metrics != nil {
		q.metrics.Depth.WithLabelValues(projectID).Set(float64(stats.Queued))
	}

	return stats, nil
}

// updateQueueDepth updates the queue depth gauge by fetching current stats.
// This is called after enqueue/dequeue operations to keep the gauge accurate.
func (q *Queue) updateQueueDepth(projectID string) {
	if q.taskRepo == nil || q.metrics == nil {
		return
	}

	// Use a background context so the metric update isn't cancelled by the
	// caller's request context, but bound it so a slow COUNT can't block
	// the caller indefinitely if the DB is wedged.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	counts, err := q.taskRepo.CountByStatus(ctx, projectID)
	if err != nil {
		return
	}

	queued := counts[persistence.TaskStatusQueued]
	q.metrics.Depth.WithLabelValues(projectID).Set(float64(queued))
}

// QueueStats holds queue statistics.
type QueueStats struct {
	Queued    int64
	Running   int64
	Leased    int64
	Completed int64
	Failed    int64
}

// Errors
var (
	ErrNoRepository      = &QueueError{Message: "no task repository configured"}
	ErrDLQNotImplemented = &QueueError{Message: "DLQ persistence is not implemented"}
)

// QueueError represents a queue error.
type QueueError struct {
	Message string
}

func (e *QueueError) Error() string {
	return e.Message
}
