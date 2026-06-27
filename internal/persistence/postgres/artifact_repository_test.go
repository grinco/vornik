package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"vornik.io/vornik/internal/persistence"
)

func TestArtifactRepository_Create_NilArtifact(t *testing.T) {
	repo := NewArtifactRepository(&stubDBTX{})
	ctx := context.Background()

	err := repo.Create(ctx, nil)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "artifact is nil")
}

func TestArtifactRepository_Create_SetsCreatedAtWhenZero(t *testing.T) {
	repo := NewArtifactRepository(&stubDBTX{})
	ctx := context.Background()

	artifact := &persistence.Artifact{
		ID:            "artifact-1",
		ProjectID:     "proj-1",
		Name:          "test.txt",
		ArtifactClass: "OUTPUT",
		StoragePath:   "/storage/test.txt",
		CreatedAt:     time.Time{}, // Zero value
	}

	err := repo.Create(ctx, artifact)

	assert.NoError(t, err)
	assert.False(t, artifact.CreatedAt.IsZero())
	assert.Equal(t, time.UTC, artifact.CreatedAt.Location())
}

func TestArtifactRepository_Create_DbError(t *testing.T) {
	dbErr := &pq.Error{Code: "23505"}
	db := &recordingDBTX{execErr: dbErr}
	repo := NewArtifactRepository(db)
	ctx := context.Background()

	artifact := &persistence.Artifact{
		ID:            "artifact-1",
		ProjectID:     "proj-1",
		Name:          "test.txt",
		ArtifactClass: "OUTPUT",
		StoragePath:   "/storage/test.txt",
		CreatedAt:     time.Now().UTC(),
	}

	err := repo.Create(ctx, artifact)

	assert.ErrorIs(t, err, persistence.ErrDuplicateKey)
}

func TestArtifactRepository_UpdateTaskID_EmptyArtifactID(t *testing.T) {
	repo := NewArtifactRepository(&stubDBTX{})
	ctx := context.Background()

	err := repo.UpdateTaskID(ctx, "", "task-1")

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "artifact id is empty")
}

func TestArtifactRepository_UpdateTaskID_EmptyTaskID(t *testing.T) {
	repo := NewArtifactRepository(&stubDBTX{})
	ctx := context.Background()

	err := repo.UpdateTaskID(ctx, "artifact-1", "")

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "task id is empty")
}

func TestArtifactRepository_UpdateTaskID_DbError(t *testing.T) {
	dbErr := &pq.Error{Code: "23503"}
	db := &recordingDBTX{execErr: dbErr}
	repo := NewArtifactRepository(db)
	ctx := context.Background()

	err := repo.UpdateTaskID(ctx, "artifact-1", "task-1")

	assert.ErrorIs(t, err, persistence.ErrNotFound)
}

func TestArtifactRepository_Delete_DbError(t *testing.T) {
	dbErr := errors.New("connection failed")
	db := &recordingDBTX{execErr: dbErr}
	repo := NewArtifactRepository(db)
	ctx := context.Background()

	err := repo.Delete(ctx, "artifact-1")

	assert.Same(t, dbErr, err)
}

func TestArtifactRepository_DeleteByExecutionID_DbError(t *testing.T) {
	dbErr := errors.New("connection failed")
	db := &recordingDBTX{execErr: dbErr}
	repo := NewArtifactRepository(db)
	ctx := context.Background()

	err := repo.DeleteByExecutionID(ctx, "exec-1")

	assert.Same(t, dbErr, err)
}
