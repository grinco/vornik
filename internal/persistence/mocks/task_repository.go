// Package mocks provides mock implementations of persistence interfaces for testing.
package mocks

import (
	"context"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// MockTaskRepository is a mock implementation of TaskRepository for testing.
type MockTaskRepository struct {
	PingFunc func(ctx context.Context) error
	// CreateFunc is the function called for Create.
	CreateFunc func(ctx context.Context, task *persistence.Task) error

	// GetFunc is the function called for Get.
	GetFunc func(ctx context.Context, id string) (*persistence.Task, error)

	// GetByIdempotencyKeyFunc is the function called for GetByIdempotencyKey.
	GetByIdempotencyKeyFunc func(ctx context.Context, projectID, idempotencyKey string) (*persistence.Task, error)

	// UpdateFunc is the function called for Update.
	UpdateFunc func(ctx context.Context, task *persistence.Task) error

	// DeleteFunc is the function called for Delete.
	DeleteFunc func(ctx context.Context, id string) error

	// ListFunc is the function called for List.
	ListFunc func(ctx context.Context, filter persistence.TaskFilter) ([]*persistence.Task, error)

	// CountFunc lets tests script the total-count return separately
	// from the paged List result. Defaults to (0, nil).
	CountFunc func(ctx context.Context, filter persistence.TaskFilter) (int64, error)

	// UpdateStatusFunc is the function called for UpdateStatus.
	UpdateStatusFunc func(ctx context.Context, id string, status persistence.TaskStatus) error

	// TransitionToCancelledFunc lets tests script the atomic
	// CANCELLED transition. Defaults to (true, nil) when nil.
	TransitionToCancelledFunc func(ctx context.Context, id string) (bool, error)

	// RequeueTerminalTaskFunc scripts the atomic terminal-to-QUEUED
	// transition. Defaults to (true, nil) when nil.
	RequeueTerminalTaskFunc func(ctx context.Context, id string, attempt, maxAttempts int) (bool, error)

	// TransitionConditionalFunc scripts atomic status transitions
	// for the conversational task lifecycle. Defaults to (true, nil)
	// when nil.
	TransitionConditionalFunc func(ctx context.Context, id string, from []persistence.TaskStatus, to persistence.TaskStatus, opts persistence.TransitionOpts) (bool, error)

	// LeaseTaskFunc is the function called for LeaseTask.
	LeaseTaskFunc func(ctx context.Context, opts persistence.LeaseOptions) (*persistence.Task, error)

	// RenewLeaseFunc is the function called for RenewLease.
	RenewLeaseFunc func(ctx context.Context, taskID, leaseID string, extendBySeconds int) error

	// ReleaseLeaseFunc is the function called for ReleaseLease.
	ReleaseLeaseFunc func(ctx context.Context, taskID, leaseID string, newStatus persistence.TaskStatus, opts persistence.ReleaseOptions) error

	// FindExpiredLeasesFunc is the function called for FindExpiredLeases.
	FindExpiredLeasesFunc func(ctx context.Context, limit int) ([]*persistence.Task, error)

	// CountByStatusFunc is the function called for CountByStatus.
	CountByStatusFunc func(ctx context.Context, projectID string) (map[persistence.TaskStatus]int64, error)

	// CountRecentFailuresFunc lets tests dictate the rolling failure
	// count for circuit-breaker exercises.
	CountRecentFailuresFunc func(ctx context.Context, projectID string, errorClasses []string, since time.Time) (int, error)

	// GetChildrenFunc is the function called for GetChildren.
	GetChildrenFunc func(ctx context.Context, parentTaskID string) ([]*persistence.Task, error)

	// CountChildrenForParentsFunc scripts the bulk direct-child count.
	// Defaults to (nil, nil) — i.e. "no children for any parent".
	CountChildrenForParentsFunc func(ctx context.Context, parentTaskIDs []string) (map[string]int, error)

	// GetDependenciesFunc is the function called for GetDependencies.
	GetDependenciesFunc func(ctx context.Context, taskID string) ([]*persistence.Task, error)

	// GetDependentsFunc is the function called for GetDependents.
	GetDependentsFunc func(ctx context.Context, taskID string) ([]*persistence.Task, error)

	// CallCount tracks how many times each method was called.
	CallCount struct {
		Create                  int
		Get                     int
		GetByIdempotencyKey     int
		Update                  int
		Delete                  int
		List                    int
		UpdateStatus            int
		LeaseTask               int
		RenewLease              int
		ReleaseLease            int
		FindExpiredLeases       int
		CountByStatus           int
		GetChildren             int
		CountChildrenForParents int
		GetDependencies         int
		GetDependents           int
	}

	// LastCall contains arguments from the most recent call.
	LastCall struct {
		Task   *persistence.Task
		Filter persistence.TaskFilter
		ID     string
		Status persistence.TaskStatus
		Opts   persistence.LeaseOptions
	}
}

// Create implements TaskRepository.
func (m *MockTaskRepository) Create(ctx context.Context, task *persistence.Task) error {
	m.CallCount.Create++
	m.LastCall.Task = task
	if m.CreateFunc != nil {
		return m.CreateFunc(ctx, task)
	}
	return nil
}

// Get implements TaskRepository.
func (m *MockTaskRepository) Get(ctx context.Context, id string) (*persistence.Task, error) {
	m.CallCount.Get++
	m.LastCall.ID = id
	if m.GetFunc != nil {
		return m.GetFunc(ctx, id)
	}
	return nil, nil
}

// GetByIdempotencyKey implements TaskRepository.
func (m *MockTaskRepository) GetByIdempotencyKey(ctx context.Context, projectID, idempotencyKey string) (*persistence.Task, error) {
	m.CallCount.GetByIdempotencyKey++
	if m.GetByIdempotencyKeyFunc != nil {
		return m.GetByIdempotencyKeyFunc(ctx, projectID, idempotencyKey)
	}
	return nil, persistence.ErrNotFound
}

// Update implements TaskRepository.
func (m *MockTaskRepository) Update(ctx context.Context, task *persistence.Task) error {
	m.CallCount.Update++
	m.LastCall.Task = task
	if m.UpdateFunc != nil {
		return m.UpdateFunc(ctx, task)
	}
	return nil
}

// Delete implements TaskRepository.
func (m *MockTaskRepository) Delete(ctx context.Context, id string) error {
	m.CallCount.Delete++
	m.LastCall.ID = id
	if m.DeleteFunc != nil {
		return m.DeleteFunc(ctx, id)
	}
	return nil
}

// List implements TaskRepository.
func (m *MockTaskRepository) List(ctx context.Context, filter persistence.TaskFilter) ([]*persistence.Task, error) {
	m.CallCount.List++
	m.LastCall.Filter = filter
	if m.ListFunc != nil {
		return m.ListFunc(ctx, filter)
	}
	return nil, nil
}

// Count implements TaskRepository.
func (m *MockTaskRepository) Count(ctx context.Context, filter persistence.TaskFilter) (int64, error) {
	if m.CountFunc != nil {
		return m.CountFunc(ctx, filter)
	}
	return 0, nil
}

// UpdateStatus implements TaskRepository.
func (m *MockTaskRepository) UpdateStatus(ctx context.Context, id string, status persistence.TaskStatus) error {
	m.CallCount.UpdateStatus++
	m.LastCall.ID = id
	m.LastCall.Status = status
	if m.UpdateStatusFunc != nil {
		return m.UpdateStatusFunc(ctx, id, status)
	}
	return nil
}

// TransitionToCancelled implements TaskRepository.
func (m *MockTaskRepository) TransitionToCancelled(ctx context.Context, id string) (bool, error) {
	m.LastCall.ID = id
	if m.TransitionToCancelledFunc != nil {
		return m.TransitionToCancelledFunc(ctx, id)
	}
	return true, nil
}

// RequeueTerminalTask implements TaskRepository.
func (m *MockTaskRepository) RequeueTerminalTask(ctx context.Context, id string, attempt, maxAttempts int) (bool, error) {
	m.LastCall.ID = id
	if m.RequeueTerminalTaskFunc != nil {
		return m.RequeueTerminalTaskFunc(ctx, id, attempt, maxAttempts)
	}
	return true, nil
}

// TransitionConditional implements TaskRepository.
func (m *MockTaskRepository) TransitionConditional(
	ctx context.Context,
	id string,
	from []persistence.TaskStatus,
	to persistence.TaskStatus,
	opts persistence.TransitionOpts,
) (bool, error) {
	m.LastCall.ID = id
	if m.TransitionConditionalFunc != nil {
		return m.TransitionConditionalFunc(ctx, id, from, to, opts)
	}
	return true, nil
}

// LeaseTask implements TaskRepository.
func (m *MockTaskRepository) LeaseTask(ctx context.Context, opts persistence.LeaseOptions) (*persistence.Task, error) {
	m.CallCount.LeaseTask++
	m.LastCall.Opts = opts
	if m.LeaseTaskFunc != nil {
		return m.LeaseTaskFunc(ctx, opts)
	}
	return nil, nil
}

// RenewLease implements TaskRepository.
func (m *MockTaskRepository) RenewLease(ctx context.Context, taskID, leaseID string, extendBySeconds int) error {
	m.CallCount.RenewLease++
	m.LastCall.ID = taskID
	if m.RenewLeaseFunc != nil {
		return m.RenewLeaseFunc(ctx, taskID, leaseID, extendBySeconds)
	}
	return nil
}

// ReleaseLease implements TaskRepository.
func (m *MockTaskRepository) ReleaseLease(ctx context.Context, taskID, leaseID string, newStatus persistence.TaskStatus, opts persistence.ReleaseOptions) error {
	m.CallCount.ReleaseLease++
	m.LastCall.ID = taskID
	m.LastCall.Status = newStatus
	if m.ReleaseLeaseFunc != nil {
		return m.ReleaseLeaseFunc(ctx, taskID, leaseID, newStatus, opts)
	}
	return nil
}

// FindExpiredLeases implements TaskRepository.
func (m *MockTaskRepository) FindExpiredLeases(ctx context.Context, limit int) ([]*persistence.Task, error) {
	m.CallCount.FindExpiredLeases++
	if m.FindExpiredLeasesFunc != nil {
		return m.FindExpiredLeasesFunc(ctx, limit)
	}
	return nil, nil
}

// CountByStatus implements TaskRepository.
func (m *MockTaskRepository) CountByStatus(ctx context.Context, projectID string) (map[persistence.TaskStatus]int64, error) {
	m.CallCount.CountByStatus++
	if m.CountByStatusFunc != nil {
		return m.CountByStatusFunc(ctx, projectID)
	}
	return make(map[persistence.TaskStatus]int64), nil
}

// CountRecentFailures implements TaskRepository. The mock returns 0
// by default; tests that need to exercise the circuit-breaker path
// override CountRecentFailuresFunc to return a higher count.
func (m *MockTaskRepository) CountRecentFailures(ctx context.Context, projectID string, errorClasses []string, since time.Time) (int, error) {
	if m.CountRecentFailuresFunc != nil {
		return m.CountRecentFailuresFunc(ctx, projectID, errorClasses, since)
	}
	return 0, nil
}

// GetChildren implements TaskRepository.
func (m *MockTaskRepository) GetChildren(ctx context.Context, parentTaskID string) ([]*persistence.Task, error) {
	m.CallCount.GetChildren++
	m.LastCall.ID = parentTaskID
	if m.GetChildrenFunc != nil {
		return m.GetChildrenFunc(ctx, parentTaskID)
	}
	return nil, nil
}

// CountChildrenForParents implements TaskRepository.
func (m *MockTaskRepository) CountChildrenForParents(ctx context.Context, parentTaskIDs []string) (map[string]int, error) {
	m.CallCount.CountChildrenForParents++
	if m.CountChildrenForParentsFunc != nil {
		return m.CountChildrenForParentsFunc(ctx, parentTaskIDs)
	}
	return nil, nil
}

// GetDependencies implements TaskRepository.
func (m *MockTaskRepository) GetDependencies(ctx context.Context, taskID string) ([]*persistence.Task, error) {
	m.CallCount.GetDependencies++
	m.LastCall.ID = taskID
	if m.GetDependenciesFunc != nil {
		return m.GetDependenciesFunc(ctx, taskID)
	}
	return nil, nil
}

// GetDependents implements TaskRepository.
func (m *MockTaskRepository) GetDependents(ctx context.Context, taskID string) ([]*persistence.Task, error) {
	m.CallCount.GetDependents++
	m.LastCall.ID = taskID
	if m.GetDependentsFunc != nil {
		return m.GetDependentsFunc(ctx, taskID)
	}
	return nil, nil
}

// Reset resets the call counts and last call data.
func (m *MockTaskRepository) Reset() {
	m.CallCount = struct {
		Create                  int
		Get                     int
		GetByIdempotencyKey     int
		Update                  int
		Delete                  int
		List                    int
		UpdateStatus            int
		LeaseTask               int
		RenewLease              int
		ReleaseLease            int
		FindExpiredLeases       int
		CountByStatus           int
		GetChildren             int
		CountChildrenForParents int
		GetDependencies         int
		GetDependents           int
	}{}
	m.LastCall = struct {
		Task   *persistence.Task
		Filter persistence.TaskFilter
		ID     string
		Status persistence.TaskStatus
		Opts   persistence.LeaseOptions
	}{}
}

func (m *MockTaskRepository) Ping(ctx context.Context) error {
	if m.PingFunc != nil {
		return m.PingFunc(ctx)
	}
	return nil

}
