package artifacts

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
)

type MockArtifactRepo struct {
	artifacts map[string]*persistence.Artifact
	createErr error
	getErr    error
	deleteErr error
	listErr   error
}

func NewMockArtifactRepo() *MockArtifactRepo {
	return &MockArtifactRepo{artifacts: make(map[string]*persistence.Artifact)}
}

func (m *MockArtifactRepo) Create(ctx context.Context, a *persistence.Artifact) error {
	if m.createErr != nil {
		return m.createErr
	}
	m.artifacts[a.ID] = a
	return nil
}

func (m *MockArtifactRepo) Get(ctx context.Context, id string) (*persistence.Artifact, error) {
	if m.getErr != nil {
		return nil, m.getErr
	}
	return m.artifacts[id], nil
}

func (m *MockArtifactRepo) GetByHash(ctx context.Context, hash string) (*persistence.Artifact, error) {
	for _, a := range m.artifacts {
		if a.ContentHashSHA256 != nil && *a.ContentHashSHA256 == hash {
			return a, nil
		}
	}
	return nil, errors.New("not found")
}

func (m *MockArtifactRepo) Delete(ctx context.Context, id string) error {
	if m.deleteErr != nil {
		return m.deleteErr
	}
	delete(m.artifacts, id)
	return nil
}

func (m *MockArtifactRepo) DeleteByExecutionID(ctx context.Context, execID string) error {
	for id, a := range m.artifacts {
		if a.ExecutionID != nil && *a.ExecutionID == execID {
			delete(m.artifacts, id)
		}
	}
	return nil
}

func (m *MockArtifactRepo) UpdateTaskID(ctx context.Context, artifactID, taskID string) error {
	if a, ok := m.artifacts[artifactID]; ok {
		tid := taskID
		a.TaskID = &tid
	}
	return nil
}

func (m *MockArtifactRepo) List(ctx context.Context, f persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	var result []*persistence.Artifact
	for _, a := range m.artifacts {
		if f.ProjectID == nil || a.ProjectID == *f.ProjectID {
			result = append(result, a)
		}
	}
	return result, nil
}

func TestNew(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := New(WithBasePath(tmpDir))
	require.NoError(t, err)
	assert.Equal(t, tmpDir, store.basePath)
}

func TestNewFallsBackToRelativeArtifactsDirOnPermissionError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission fallback test is unix-specific")
	}

	root := t.TempDir()
	blocked := filepath.Join(root, "blocked")
	require.NoError(t, os.Mkdir(blocked, 0555))

	store, err := New(WithBasePath(filepath.Join(blocked, "nested")))
	require.NoError(t, err)
	assert.Equal(t, "./artifacts", store.basePath)

	_ = os.RemoveAll("./artifacts")
}

func TestWithRepository(t *testing.T) {
	repo := NewMockArtifactRepo()
	store := &Store{}
	WithRepository(repo)(store)
	assert.Equal(t, repo, store.repo)
}

func TestWithBasePath(t *testing.T) {
	store := &Store{}
	WithBasePath("/tmp/test")(store)
	assert.Equal(t, "/tmp/test", store.basePath)
}

func TestStore_Store(t *testing.T) {
	tmpDir := t.TempDir()
	repo := NewMockArtifactRepo()
	store, err := New(WithBasePath(tmpDir), WithRepository(repo))
	require.NoError(t, err)

	srcFile := filepath.Join(tmpDir, "source.txt")
	err = os.WriteFile(srcFile, []byte("test"), 0644)
	require.NoError(t, err)

	artifact, err := store.Store(context.Background(), "p1", "e1", "t1", "out.txt", srcFile)
	require.NoError(t, err)
	assert.Contains(t, artifact.ID, "artifact_")
	require.NotNil(t, artifact.TaskID)
	assert.Equal(t, "t1", *artifact.TaskID)
}

func TestStore_Store_MissingSource(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := New(WithBasePath(tmpDir))
	require.NoError(t, err)
	_, err = store.Store(context.Background(), "p1", "e1", "t1", "out.txt", "/nonexistent")
	assert.Error(t, err)
}

func TestStore_Store_RejectsPathTraversalNames(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := New(WithBasePath(tmpDir))
	require.NoError(t, err)

	srcFile := filepath.Join(tmpDir, "source.txt")
	require.NoError(t, os.WriteFile(srcFile, []byte("test"), 0o644))

	_, err = store.Store(context.Background(), "p1", "e1", "t1", "../escape.txt", srcFile)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid artifact name")
}

func TestStore_NoRepo_Errors(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := New(WithBasePath(tmpDir))
	require.NoError(t, err)

	_, err = store.Retrieve(context.Background(), "a1")
	assert.ErrorIs(t, err, ErrNoRepository)

	err = store.Delete(context.Background(), "a1")
	assert.ErrorIs(t, err, ErrNoRepository)

	_, err = store.List(context.Background(), "p1", "e1")
	assert.ErrorIs(t, err, ErrNoRepository)

	_, err = store.GetPath("a1")
	assert.ErrorIs(t, err, ErrNoRepository)
}

func TestDetectMimeType(t *testing.T) {
	assert.Equal(t, "text/plain", detectMimeType("f.txt"))
	assert.Equal(t, "application/json", detectMimeType("f.json"))
	assert.Equal(t, "application/octet-stream", detectMimeType("f.xyz"))
}

func TestStoreError(t *testing.T) {
	assert.Equal(t, "no artifact repository configured", ErrNoRepository.Error())
	assert.Equal(t, "artifact content hash mismatch", ErrHashMismatch.Error())
}
