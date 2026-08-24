package api

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
)

// The doubles in this package must agree with production about what absence
// looks like. Every key below is MissErrNotFound in
// internal/persistence/misscontract, and every double here answered (nil, nil)
// — the LOOSER direction, which certifies a caller's miss path without
// executing it. In internal/executor the same cleanup exposed four lineage
// walks whose end-of-chain branch was dead against production; see
// https://docs.vornik.io §8.

func TestAPIDoubles_MissContract(t *testing.T) {
	ctx := context.Background()

	t.Run("TaskJudgeVerdictRepository.GetByTask", func(t *testing.T) {
		repotest.AssertMiss(t, "TaskJudgeVerdictRepository.GetByTask", func() (*persistence.TaskJudgeVerdict, error) {
			return fakeJudgeBuilder{}.GetByTask(ctx, "task-absent")
		})
	})
	t.Run("TaskPostMortemRepository.Get", func(t *testing.T) {
		repotest.AssertMiss(t, "TaskPostMortemRepository.Get", func() (*persistence.TaskPostMortem, error) {
			return fakePMBuilder{}.Get(ctx, "task-absent")
		})
	})
	t.Run("TaskMessageRepository.GetOpenCheckpoint", func(t *testing.T) {
		repotest.AssertMiss(t, "TaskMessageRepository.GetOpenCheckpoint", func() (*persistence.TaskMessage, error) {
			return (&fakeTaskMessageRepo{}).GetOpenCheckpoint(ctx, "task-absent")
		})
	})
	t.Run("TaskRepository.Get", func(t *testing.T) {
		repotest.AssertMiss(t, "TaskRepository.Get", func() (*persistence.Task, error) {
			return (&mockTaskRepository{}).Get(ctx, "task-absent")
		})
	})
	t.Run("TaskRepository.GetByIdempotencyKey", func(t *testing.T) {
		repotest.AssertMiss(t, "TaskRepository.GetByIdempotencyKey", func() (*persistence.Task, error) {
			return (&mockTaskRepository{}).GetByIdempotencyKey(ctx, "p1", "key-absent")
		})
	})
	t.Run("ArtifactRepository.GetByHash", func(t *testing.T) {
		repotest.AssertMiss(t, "ArtifactRepository.GetByHash", func() (*persistence.Artifact, error) {
			return (&stubArtifactRepo{}).GetByHash(ctx, "sha256:absent")
		})
	})
	t.Run("DaemonLeaderLockRepository.Get", func(t *testing.T) {
		repotest.AssertMiss(t, "DaemonLeaderLockRepository.Get", func() (*persistence.DaemonLeaderLock, error) {
			return stubLeaderLocks{}.Get(ctx, "subsystem-absent")
		})
	})
	// stubExecRepoForFork keeps a deliberate nilNoError branch — a defensive
	// case production cannot produce, kept because the handler is expected to
	// survive it. Its DEFAULT path is the contract, and that is what is pinned.
	t.Run("ExecutionRepository.Get", func(t *testing.T) {
		repotest.AssertMiss(t, "ExecutionRepository.Get", func() (*persistence.Execution, error) {
			return (&stubExecRepoForFork{}).Get(ctx, "exec-absent")
		})
	})
}
