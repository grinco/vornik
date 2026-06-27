package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
)

// TestCheckpointCov_ScheduleFollowUp_MaxAttemptsFloor — a parent with
// MaxAttempts <= 0 yields a child with MaxAttempts floored to 1 (otherwise
// the continuation would refuse to ever run).
func TestCheckpointCov_ScheduleFollowUp_MaxAttemptsFloor(t *testing.T) {
	repo := NewMockTaskRepo()
	e := &Executor{taskRepo: repo, logger: zerolog.Nop()}
	parent := &persistence.Task{
		ID:             "task_parent",
		ProjectID:      "p",
		CreationSource: persistence.TaskCreationSourceUser,
		MaxAttempts:    0, // floor path
		Payload:        []byte(`{"context":{"prompt":"goal"}}`),
	}
	repo.AddTask(parent)

	childID, err := e.scheduleCheckpointFollowUp(context.Background(), parent, "implement", "iteration cap")
	require.NoError(t, err)
	child, _ := repo.Get(context.Background(), childID)
	require.NotNil(t, child)
	assert.Equal(t, 1, child.MaxAttempts, "max_attempts must be floored to 1")
}

// TestCheckpointCov_ScheduleFollowUp_NoPromptPlaceholder — a parent with an
// empty/unparseable payload yields the placeholder-prompt branch.
func TestCheckpointCov_ScheduleFollowUp_NoPromptPlaceholder(t *testing.T) {
	repo := NewMockTaskRepo()
	e := &Executor{taskRepo: repo, logger: zerolog.Nop()}
	parent := &persistence.Task{
		ID:             "task_noprompt",
		ProjectID:      "p",
		CreationSource: persistence.TaskCreationSourceUser,
		Payload:        nil, // extractPromptFromTask returns ""
	}
	repo.AddTask(parent)

	childID, err := e.scheduleCheckpointFollowUp(context.Background(), parent, "step", "err")
	require.NoError(t, err)
	child, _ := repo.Get(context.Background(), childID)
	prompt := extractPromptFromTask(child)
	assert.Contains(t, prompt, "original prompt unavailable",
		"missing parent prompt must surface the placeholder pointer")
	assert.Contains(t, prompt, parent.ID)
}

// TestCheckpointCov_ScheduleFollowUp_DepthWalkErrorProceeds — when the depth
// walk errors, the follow-up still gets scheduled (best-effort: depth is
// reset to 0 and the next failure's check will cap the chain).
func TestCheckpointCov_ScheduleFollowUp_DepthWalkErrorProceeds(t *testing.T) {
	repo := NewMockTaskRepo()
	repo.err = errors.New("db unavailable")
	e := &Executor{taskRepo: repo, logger: zerolog.Nop()}

	parentID := "ancestor"
	parent := &persistence.Task{
		ID:             "task_parent",
		ProjectID:      "p",
		CreationSource: persistence.TaskCreationSourceCheckpoint,
		ParentTaskID:   &parentID, // forces countCheckpointDepth to call Get → error
		Payload:        []byte(`{"context":{"prompt":"goal"}}`),
	}
	// AddTask bypasses the err field for storage; Get still trips repo.err.
	repo.AddTask(parent)

	childID, err := e.scheduleCheckpointFollowUp(context.Background(), parent, "implement", "err")
	require.NoError(t, err, "a depth-walk DB error must not abort the follow-up")
	assert.NotEmpty(t, childID)
}

// TestCheckpointCov_CountDepth_NilParentTerminatesAtZero — a parent with no
// ParentTaskID terminates the walk immediately at depth 0.
func TestCheckpointCov_CountDepth_NilParentTerminatesAtZero(t *testing.T) {
	repo := NewMockTaskRepo()
	e := &Executor{taskRepo: repo, logger: zerolog.Nop()}
	root := &persistence.Task{ID: "root", CreationSource: persistence.TaskCreationSourceUser}
	depth, err := e.countCheckpointDepth(context.Background(), root)
	require.NoError(t, err)
	assert.Equal(t, 0, depth)
}

// TestCheckpointCov_CountDepth_StopsAtNilAncestor — when Get returns
// (nil, nil) for a missing ancestor, the walk terminates cleanly.
func TestCheckpointCov_CountDepth_StopsAtNilAncestor(t *testing.T) {
	repo := NewMockTaskRepo()
	e := &Executor{taskRepo: repo, logger: zerolog.Nop()}
	missing := "gone"
	leaf := &persistence.Task{
		ID:             "leaf",
		CreationSource: persistence.TaskCreationSourceCheckpoint,
		ParentTaskID:   &missing, // not in the repo → Get returns (nil,nil)
	}
	depth, err := e.countCheckpointDepth(context.Background(), leaf)
	require.NoError(t, err)
	assert.Equal(t, 0, depth, "a missing ancestor terminates the walk at the current depth")
}

// TestCheckpointCov_CountDepth_PropagatesGetError — a DB error on the walk
// surfaces as an error with the depth-so-far.
func TestCheckpointCov_CountDepth_PropagatesGetError(t *testing.T) {
	repo := NewMockTaskRepo()
	repo.err = errors.New("boom")
	e := &Executor{taskRepo: repo, logger: zerolog.Nop()}
	parentID := "p"
	leaf := &persistence.Task{ID: "leaf", CreationSource: persistence.TaskCreationSourceCheckpoint, ParentTaskID: &parentID}
	_, err := e.countCheckpointDepth(context.Background(), leaf)
	require.Error(t, err)
}
