package supportbundle

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
)

// The bundle's collectors ask three repositories for rows that are routinely
// ABSENT — a task with no judge verdict, no post-mortem, or an id that no
// longer exists — and each collector's absent branch decides between "omit the
// section" and "record a section error". A double that answers (nil, nil) where
// production answers ErrNotFound certifies the wrong branch, which is exactly
// the failure https://docs.vornik.io §8
// records. These doubles moved here with the builder on 2026-09-04; the
// assertions moved with them.
func TestSupportBundleDoubles_MissContract(t *testing.T) {
	ctx := context.Background()

	t.Run("TaskRepository.Get", func(t *testing.T) {
		repotest.AssertMiss(t, "TaskRepository.Get", func() (*persistence.Task, error) {
			return (&fakeTaskReader{}).Get(ctx, "task-absent")
		})
	})
	t.Run("TaskJudgeVerdictRepository.GetByTask", func(t *testing.T) {
		repotest.AssertMiss(t, "TaskJudgeVerdictRepository.GetByTask", func() (*persistence.TaskJudgeVerdict, error) {
			return (&fakeJudgeReader{}).GetByTask(ctx, "task-absent")
		})
	})
	t.Run("TaskPostMortemRepository.Get", func(t *testing.T) {
		repotest.AssertMiss(t, "TaskPostMortemRepository.Get", func() (*persistence.TaskPostMortem, error) {
			return (&fakePostMortemReader{}).Get(ctx, "task-absent")
		})
	})
}
