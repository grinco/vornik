package ui

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
)

// Absence must look the same here as it does in production. Each key below is
// MissErrNotFound; each double answered (nil, nil), which is looser and lets a
// caller that never handles ErrNotFound pass. See
// https://docs.vornik.io
func TestUIDoubles_MissContract(t *testing.T) {
	ctx := context.Background()

	t.Run("TaskMessageRepository.GetOpenCheckpoint", func(t *testing.T) {
		repotest.AssertMiss(t, "TaskMessageRepository.GetOpenCheckpoint", func() (*persistence.TaskMessage, error) {
			return (&fakeTaskMessageRepo{}).GetOpenCheckpoint(ctx, "task-absent")
		})
	})
	t.Run("TaskJudgeVerdictRepository.GetByTask", func(t *testing.T) {
		repotest.AssertMiss(t, "TaskJudgeVerdictRepository.GetByTask", func() (*persistence.TaskJudgeVerdict, error) {
			return (&stubVerdictRepo{}).GetByTask(ctx, "task-absent")
		})
	})
	t.Run("InstinctRepository.Get", func(t *testing.T) {
		repotest.AssertMiss(t, "InstinctRepository.Get", func() (*persistence.Instinct, error) {
			return (&uiStubInstinctRepo{}).Get(ctx, "instinct-absent")
		})
	})
}
