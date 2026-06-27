//go:build integration
// +build integration

package integration_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/persistence/postgres"
)

func TestExecutor_RecoverRunningExecution(t *testing.T) {
	db := connectDB(t)

	projectID := fmt.Sprintf("recover-project-%d", time.Now().UnixNano())
	taskID := fmt.Sprintf("recover-task-%d", time.Now().UnixNano())
	execID := fmt.Sprintf("recover-exec-%d", time.Now().UnixNano())

	t.Cleanup(func() {
		db.Exec(`DELETE FROM executions WHERE id = $1`, execID)
		db.Exec(`DELETE FROM tasks WHERE id = $1`, taskID)
		db.Close()
	})

	taskRepo := postgres.NewTaskRepository(db)
	execRepo := postgres.NewExecutionRepository(db)

	_, err := db.Exec(`
		INSERT INTO tasks (
			id, project_id, status, priority, creation_source, attempt, max_attempts,
			lease_id, leased_by, leased_at, lease_expires_at, created_at, updated_at
		) VALUES (
			$1, $2, 'RUNNING', 50, 'USER', 1, 3,
			$3, 'executor-recovery-test', NOW(), NOW() + INTERVAL '5 minutes', NOW(), NOW()
		)
	`, taskID, projectID, "lease-"+taskID)
	require.NoError(t, err)

	currentStepID := "execute"
	snapshot := []byte(`{"currentStepId":"execute","completedSteps":[]}`)
	_, err = db.Exec(`
		INSERT INTO executions (
			id, task_id, project_id, workflow_id, workflow_revision, status,
			current_step_id, completed_steps, state_snapshot, created_at, updated_at
		) VALUES (
			$1, $2, $3, 'default-workflow', 'v1', 'RUNNING',
			$4, ARRAY[]::text[], $5, NOW(), NOW()
		)
	`, execID, taskID, projectID, currentStepID, string(snapshot))
	require.NoError(t, err)

	rt := &mockRuntimeManager{
		runs:      make(map[string]*mockRun),
		exitCode:  0,
		exitDelay: 25 * time.Millisecond,
	}
	// Pre-seed the mock with a live container for the task — the
	// Recover() path added an orphan-sweep (commit 55b30b8) that
	// marks RUNNING executions FAILED/ORPHANED when GetContainerByTask
	// reports no live container. That's correct production behavior
	// (a SIGKILL'd daemon would leave a dead container), but this
	// test is exercising the OPPOSITE scenario: a daemon restart
	// where the container is still alive and Recover should adopt
	// it. The seed satisfies the orphan check so the adoption path
	// runs and the exec can drive to COMPLETED via mock exitDelay.
	rt.runs["mock-container-pretest-"+taskID] = &mockRun{
		done:    make(chan struct{}),
		code:    0,
		taskID:  taskID,
		started: time.Now(),
	}
	exec := executor.NewWithOptions(rt, execRepo, nil, taskRepo, executor.DefaultConfig())

	require.NoError(t, exec.Recover(context.Background()))

	require.Eventually(t, func() bool {
		var taskStatus string
		if err := db.QueryRow(`SELECT status FROM tasks WHERE id = $1`, taskID).Scan(&taskStatus); err != nil {
			return false
		}
		var execStatus string
		if err := db.QueryRow(`SELECT status FROM executions WHERE id = $1`, execID).Scan(&execStatus); err != nil {
			return false
		}
		return taskStatus == "COMPLETED" && execStatus == "COMPLETED"
	}, 5*time.Second, 25*time.Millisecond)
}
