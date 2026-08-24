package executor

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
)

// The doubles in this package must agree with production about what absence
// looks like, or they certify their callers' miss paths without exercising them.
//
// All eleven answered a miss with (nil, nil) while every one of these keys is
// MissErrNotFound in internal/persistence/misscontract. That is the LOOSER
// direction: a caller that forgets to handle ErrNotFound passes here and fails
// against Postgres, and a double that swallows a real miss lets a broken caller
// go green. See https://docs.vornik.io

func TestExecutorDoubles_MissContract(t *testing.T) {
	ctx := context.Background()

	t.Run("MockArtifactRepo.GetByHash", func(t *testing.T) {
		repotest.AssertMiss(t, "ArtifactRepository.GetByHash", func() (*persistence.Artifact, error) {
			return NewMockArtifactRepo().GetByHash(ctx, "sha256:absent")
		})
	})
	t.Run("MockExecRepo.Get", func(t *testing.T) {
		repotest.AssertMiss(t, "ExecutionRepository.Get", func() (*persistence.Execution, error) {
			return NewMockExecRepo().Get(ctx, "exec-absent")
		})
	})
	t.Run("MockTaskRepo.Get", func(t *testing.T) {
		repotest.AssertMiss(t, "TaskRepository.Get", func() (*persistence.Task, error) {
			return NewMockTaskRepo().Get(ctx, "task-absent")
		})
	})
	t.Run("capturingArtifactRepo.GetByHash", func(t *testing.T) {
		repotest.AssertMiss(t, "ArtifactRepository.GetByHash", func() (*persistence.Artifact, error) {
			return (&capturingArtifactRepo{}).GetByHash(ctx, "sha256:absent")
		})
	})
	t.Run("fakeArtifactRepo.GetByHash", func(t *testing.T) {
		repotest.AssertMiss(t, "ArtifactRepository.GetByHash", func() (*persistence.Artifact, error) {
			return (&fakeArtifactRepo{}).GetByHash(ctx, "sha256:absent")
		})
	})
	t.Run("fakeMessageRepo.GetOpenCheckpoint", func(t *testing.T) {
		repotest.AssertMiss(t, "TaskMessageRepository.GetOpenCheckpoint", func() (*persistence.TaskMessage, error) {
			return (&fakeMessageRepo{}).GetOpenCheckpoint(ctx, "task-absent")
		})
	})
	t.Run("inMemArtifactRepo.GetByHash", func(t *testing.T) {
		repotest.AssertMiss(t, "ArtifactRepository.GetByHash", func() (*persistence.Artifact, error) {
			return (&inMemArtifactRepo{}).GetByHash(ctx, "sha256:absent")
		})
	})
	t.Run("listingArtifactRepo.GetByHash", func(t *testing.T) {
		repotest.AssertMiss(t, "ArtifactRepository.GetByHash", func() (*persistence.Artifact, error) {
			return (&listingArtifactRepo{}).GetByHash(ctx, "sha256:absent")
		})
	})
	t.Run("stubArtifactRepo.GetByHash", func(t *testing.T) {
		repotest.AssertMiss(t, "ArtifactRepository.GetByHash", func() (*persistence.Artifact, error) {
			return (&stubArtifactRepo{}).GetByHash(ctx, "sha256:absent")
		})
	})
	t.Run("stubInstinctRepo.Get", func(t *testing.T) {
		repotest.AssertMiss(t, "InstinctRepository.Get", func() (*persistence.Instinct, error) {
			return (&stubInstinctRepo{}).Get(ctx, "instinct-absent")
		})
	})
	t.Run("taintMsgRepo.GetOpenCheckpoint", func(t *testing.T) {
		repotest.AssertMiss(t, "TaskMessageRepository.GetOpenCheckpoint", func() (*persistence.TaskMessage, error) {
			return (&taintMsgRepo{}).GetOpenCheckpoint(ctx, "task-absent")
		})
	})
}
