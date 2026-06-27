// Package mocks provides mock implementations of persistence interfaces for testing.
package mocks

import (
	"context"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// MockExecutionRepository is a mock implementation of ExecutionRepository for testing.
type MockExecutionRepository struct {
	// CreateFunc is the function called for Create.
	CreateFunc func(ctx context.Context, execution *persistence.Execution) error

	// GetFunc is the function called for Get.
	GetFunc func(ctx context.Context, id string) (*persistence.Execution, error)

	// GetByTaskIDFunc is the function called for GetByTaskID.
	GetByTaskIDFunc func(ctx context.Context, taskID string) (*persistence.Execution, error)

	// GetByTaskIDsFunc is the function called for the batch path.
	// Nil falls back to looping GetByTaskIDFunc for compatibility.
	GetByTaskIDsFunc func(ctx context.Context, taskIDs []string) (map[string]*persistence.Execution, error)

	// UpdateFunc is the function called for Update.
	UpdateFunc func(ctx context.Context, execution *persistence.Execution) error

	// ListFunc is the function called for List.
	ListFunc func(ctx context.Context, filter persistence.ExecutionFilter) ([]*persistence.Execution, error)

	// CountFunc lets tests script the total-count return.
	CountFunc func(ctx context.Context, filter persistence.ExecutionFilter) (int64, error)

	// UpdateStatusFunc is the function called for UpdateStatus.
	UpdateStatusFunc func(ctx context.Context, id string, status persistence.ExecutionStatus) error

	// SetWorkflowSnapshotFunc lets tests dictate the snapshot-write
	// behaviour for workflow-pinning exercises.
	SetWorkflowSnapshotFunc func(ctx context.Context, id string, snapshot []byte) error
	// GetWorkflowSnapshotFunc lets tests dictate the snapshot-read
	// behaviour (e.g. simulate a recovered execution that has a
	// pinned workflow on disk).
	GetWorkflowSnapshotFunc func(ctx context.Context, id string) ([]byte, error)

	// SaveStateSnapshotFunc is the function called for SaveStateSnapshot.
	SaveStateSnapshotFunc func(ctx context.Context, id string, snapshot []byte, currentStepID string, completedSteps []string) error

	// RecordCompletionFunc is the function called for RecordCompletion.
	RecordCompletionFunc func(ctx context.Context, id string, result []byte) error

	// RecordFailureFunc is the function called for RecordFailure.
	RecordFailureFunc func(ctx context.Context, id string, errorMessage, errorCode string) error

	// CountByStatusFunc is the function called for CountByStatus.
	CountByStatusFunc func(ctx context.Context, projectID string) (map[persistence.ExecutionStatus]int64, error)

	// GetRoleQualityFunc is the function called for GetRoleQuality.
	GetRoleQualityFunc func(ctx context.Context, projectID string, since time.Duration) (map[string]*persistence.RoleQuality, error)

	// CallCount tracks how many times each method was called.
	CallCount struct {
		Create            int
		Get               int
		GetByTaskID       int
		Update            int
		List              int
		UpdateStatus      int
		SaveStateSnapshot int
		RecordCompletion  int
		RecordFailure     int
		CountByStatus     int
		GetRoleQuality    int
	}

	// LastCall contains arguments from the most recent call.
	LastCall struct {
		Execution      *persistence.Execution
		ID             string
		TaskID         string
		Filter         persistence.ExecutionFilter
		Status         persistence.ExecutionStatus
		Snapshot       []byte
		CurrentStepID  string
		CompletedSteps []string
		Result         []byte
		ErrorMessage   string
		ErrorCode      string
	}
}

// Create implements ExecutionRepository.
func (m *MockExecutionRepository) Create(ctx context.Context, execution *persistence.Execution) error {
	m.CallCount.Create++
	m.LastCall.Execution = execution
	if m.CreateFunc != nil {
		return m.CreateFunc(ctx, execution)
	}
	return nil
}

// Get implements ExecutionRepository.
func (m *MockExecutionRepository) Get(ctx context.Context, id string) (*persistence.Execution, error) {
	m.CallCount.Get++
	m.LastCall.ID = id
	if m.GetFunc != nil {
		return m.GetFunc(ctx, id)
	}
	return nil, nil
}

// GetByTaskID implements ExecutionRepository.
func (m *MockExecutionRepository) GetByTaskID(ctx context.Context, taskID string) (*persistence.Execution, error) {
	m.CallCount.GetByTaskID++
	m.LastCall.TaskID = taskID
	if m.GetByTaskIDFunc != nil {
		return m.GetByTaskIDFunc(ctx, taskID)
	}
	return nil, nil
}

// GetByTaskIDs implements ExecutionRepository. Default behaviour
// fans out to GetByTaskID so existing tests that only set
// GetByTaskIDFunc keep working; override GetByTaskIDsFunc to exercise
// the batch path directly.
func (m *MockExecutionRepository) GetByTaskIDs(ctx context.Context, taskIDs []string) (map[string]*persistence.Execution, error) {
	if m.GetByTaskIDsFunc != nil {
		return m.GetByTaskIDsFunc(ctx, taskIDs)
	}
	out := make(map[string]*persistence.Execution, len(taskIDs))
	for _, id := range taskIDs {
		exec, err := m.GetByTaskID(ctx, id)
		if err != nil {
			return nil, err
		}
		if exec != nil {
			out[id] = exec
		}
	}
	return out, nil
}

// Update implements ExecutionRepository.
func (m *MockExecutionRepository) Update(ctx context.Context, execution *persistence.Execution) error {
	m.CallCount.Update++
	m.LastCall.Execution = execution
	if m.UpdateFunc != nil {
		return m.UpdateFunc(ctx, execution)
	}
	return nil
}

// List implements ExecutionRepository.
func (m *MockExecutionRepository) List(ctx context.Context, filter persistence.ExecutionFilter) ([]*persistence.Execution, error) {
	m.CallCount.List++
	m.LastCall.Filter = filter
	if m.ListFunc != nil {
		return m.ListFunc(ctx, filter)
	}
	return nil, nil
}

// Count implements ExecutionRepository.
func (m *MockExecutionRepository) Count(ctx context.Context, filter persistence.ExecutionFilter) (int64, error) {
	if m.CountFunc != nil {
		return m.CountFunc(ctx, filter)
	}
	return 0, nil
}

// UpdateStatus implements ExecutionRepository.
func (m *MockExecutionRepository) UpdateStatus(ctx context.Context, id string, status persistence.ExecutionStatus) error {
	m.CallCount.UpdateStatus++
	m.LastCall.ID = id
	m.LastCall.Status = status
	if m.UpdateStatusFunc != nil {
		return m.UpdateStatusFunc(ctx, id, status)
	}
	return nil
}

// SaveStateSnapshot implements ExecutionRepository.
func (m *MockExecutionRepository) SaveStateSnapshot(ctx context.Context, id string, snapshot []byte, currentStepID string, completedSteps []string) error {
	m.CallCount.SaveStateSnapshot++
	m.LastCall.ID = id
	m.LastCall.Snapshot = snapshot
	m.LastCall.CurrentStepID = currentStepID
	m.LastCall.CompletedSteps = completedSteps
	if m.SaveStateSnapshotFunc != nil {
		return m.SaveStateSnapshotFunc(ctx, id, snapshot, currentStepID, completedSteps)
	}
	return nil
}

// SetWorkflowSnapshot is a no-op stub for tests; assignments are
// captured through the workflow_snapshots field (when tests want to
// inspect them) rather than CallCount because the production breaker
// path doesn't read this. Tests that exercise the resume path can
// override SetWorkflowSnapshotFunc.
func (m *MockExecutionRepository) SetWorkflowSnapshot(ctx context.Context, id string, snapshot []byte) error {
	if m.SetWorkflowSnapshotFunc != nil {
		return m.SetWorkflowSnapshotFunc(ctx, id, snapshot)
	}
	return nil
}

// GetWorkflowSnapshot returns nil by default — the resume path
// treats nil as "no snapshot, fall back to live workflow." Tests
// that need to exercise the snapshot replay branch override
// GetWorkflowSnapshotFunc.
func (m *MockExecutionRepository) GetWorkflowSnapshot(ctx context.Context, id string) ([]byte, error) {
	if m.GetWorkflowSnapshotFunc != nil {
		return m.GetWorkflowSnapshotFunc(ctx, id)
	}
	return nil, nil
}

// RecordCompletion implements ExecutionRepository.
func (m *MockExecutionRepository) RecordCompletion(ctx context.Context, id string, result []byte) error {
	m.CallCount.RecordCompletion++
	m.LastCall.ID = id
	m.LastCall.Result = result
	if m.RecordCompletionFunc != nil {
		return m.RecordCompletionFunc(ctx, id, result)
	}
	return nil
}

// RecordFailure implements ExecutionRepository.
func (m *MockExecutionRepository) RecordFailure(ctx context.Context, id string, errorMessage, errorCode string) error {
	m.CallCount.RecordFailure++
	m.LastCall.ID = id
	m.LastCall.ErrorMessage = errorMessage
	m.LastCall.ErrorCode = errorCode
	if m.RecordFailureFunc != nil {
		return m.RecordFailureFunc(ctx, id, errorMessage, errorCode)
	}
	return nil
}

// SupersedeNonTerminalForTask implements ExecutionRepository.
// Mock returns 0 with no error by default — tests that need to
// verify the cascade fired should assert via the call count or
// override via a callback (added on demand).
func (m *MockExecutionRepository) SupersedeNonTerminalForTask(_ context.Context, _ string) (int64, error) {
	return 0, nil
}

// SupersedeOrphanPausedExecutions implements ExecutionRepository.
func (m *MockExecutionRepository) SupersedeOrphanPausedExecutions(_ context.Context) (int64, error) {
	return 0, nil
}

// CountByStatus implements ExecutionRepository.
func (m *MockExecutionRepository) CountByStatus(ctx context.Context, projectID string) (map[persistence.ExecutionStatus]int64, error) {
	m.CallCount.CountByStatus++
	if m.CountByStatusFunc != nil {
		return m.CountByStatusFunc(ctx, projectID)
	}
	return make(map[persistence.ExecutionStatus]int64), nil
}

// GetRoleQuality implements ExecutionRepository.
func (m *MockExecutionRepository) GetRoleQuality(ctx context.Context, projectID string, since time.Duration) (map[string]*persistence.RoleQuality, error) {
	m.CallCount.GetRoleQuality++
	if m.GetRoleQualityFunc != nil {
		return m.GetRoleQualityFunc(ctx, projectID, since)
	}
	return make(map[string]*persistence.RoleQuality), nil
}

// Reset resets the call counts and last call data.
func (m *MockExecutionRepository) Reset() {
	m.CallCount = struct {
		Create            int
		Get               int
		GetByTaskID       int
		Update            int
		List              int
		UpdateStatus      int
		SaveStateSnapshot int
		RecordCompletion  int
		RecordFailure     int
		CountByStatus     int
		GetRoleQuality    int
	}{}
	m.LastCall = struct {
		Execution      *persistence.Execution
		ID             string
		TaskID         string
		Filter         persistence.ExecutionFilter
		Status         persistence.ExecutionStatus
		Snapshot       []byte
		CurrentStepID  string
		CompletedSteps []string
		Result         []byte
		ErrorMessage   string
		ErrorCode      string
	}{}
}
