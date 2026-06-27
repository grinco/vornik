// Package mocks provides mock implementations of persistence interfaces for testing.
package mocks

import (
	"context"

	"vornik.io/vornik/internal/persistence"
)

// MockArtifactRepository is a mock implementation of ArtifactRepository for testing.
type MockArtifactRepository struct {
	// CreateFunc is the function called for Create.
	CreateFunc func(ctx context.Context, artifact *persistence.Artifact) error

	// GetFunc is the function called for Get.
	GetFunc func(ctx context.Context, id string) (*persistence.Artifact, error)

	// GetByHashFunc is the function called for GetByHash.
	GetByHashFunc func(ctx context.Context, hash string) (*persistence.Artifact, error)

	// ListFunc is the function called for List.
	ListFunc func(ctx context.Context, filter persistence.ArtifactFilter) ([]*persistence.Artifact, error)

	// DeleteFunc is the function called for Delete.
	DeleteFunc func(ctx context.Context, id string) error

	// DeleteByExecutionIDFunc is the function called for DeleteByExecutionID.
	DeleteByExecutionIDFunc func(ctx context.Context, executionID string) error

	// UpdateTaskIDFunc is the function called for UpdateTaskID.
	UpdateTaskIDFunc func(ctx context.Context, artifactID, taskID string) error

	// CallCount tracks how many times each method was called.
	CallCount struct {
		Create              int
		Get                 int
		GetByHash           int
		List                int
		Delete              int
		DeleteByExecutionID int
		UpdateTaskID        int
	}

	// LastCall contains arguments from the most recent call.
	LastCall struct {
		Artifact    *persistence.Artifact
		ID          string
		Hash        string
		Filter      persistence.ArtifactFilter
		ExecutionID string
		TaskID      string
	}
}

// Create implements ArtifactRepository.
func (m *MockArtifactRepository) Create(ctx context.Context, artifact *persistence.Artifact) error {
	m.CallCount.Create++
	m.LastCall.Artifact = artifact
	if m.CreateFunc != nil {
		return m.CreateFunc(ctx, artifact)
	}
	return nil
}

// Get implements ArtifactRepository.
func (m *MockArtifactRepository) Get(ctx context.Context, id string) (*persistence.Artifact, error) {
	m.CallCount.Get++
	m.LastCall.ID = id
	if m.GetFunc != nil {
		return m.GetFunc(ctx, id)
	}
	return nil, nil
}

// GetByHash implements ArtifactRepository.
func (m *MockArtifactRepository) GetByHash(ctx context.Context, hash string) (*persistence.Artifact, error) {
	m.CallCount.GetByHash++
	m.LastCall.Hash = hash
	if m.GetByHashFunc != nil {
		return m.GetByHashFunc(ctx, hash)
	}
	return nil, nil
}

// List implements ArtifactRepository.
func (m *MockArtifactRepository) List(ctx context.Context, filter persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
	m.CallCount.List++
	m.LastCall.Filter = filter
	if m.ListFunc != nil {
		return m.ListFunc(ctx, filter)
	}
	return nil, nil
}

// Delete implements ArtifactRepository.
func (m *MockArtifactRepository) Delete(ctx context.Context, id string) error {
	m.CallCount.Delete++
	m.LastCall.ID = id
	if m.DeleteFunc != nil {
		return m.DeleteFunc(ctx, id)
	}
	return nil
}

// DeleteByExecutionID implements ArtifactRepository.
func (m *MockArtifactRepository) DeleteByExecutionID(ctx context.Context, executionID string) error {
	m.CallCount.DeleteByExecutionID++
	m.LastCall.ExecutionID = executionID
	if m.DeleteByExecutionIDFunc != nil {
		return m.DeleteByExecutionIDFunc(ctx, executionID)
	}
	return nil
}

// UpdateTaskID implements ArtifactRepository.
func (m *MockArtifactRepository) UpdateTaskID(ctx context.Context, artifactID, taskID string) error {
	m.CallCount.UpdateTaskID++
	m.LastCall.ID = artifactID
	m.LastCall.TaskID = taskID
	if m.UpdateTaskIDFunc != nil {
		return m.UpdateTaskIDFunc(ctx, artifactID, taskID)
	}
	return nil
}

// Reset resets the call counts and last call data.
func (m *MockArtifactRepository) Reset() {
	m.CallCount = struct {
		Create              int
		Get                 int
		GetByHash           int
		List                int
		Delete              int
		DeleteByExecutionID int
		UpdateTaskID        int
	}{}
	m.LastCall = struct {
		Artifact    *persistence.Artifact
		ID          string
		Hash        string
		Filter      persistence.ArtifactFilter
		ExecutionID string
		TaskID      string
	}{}
}
