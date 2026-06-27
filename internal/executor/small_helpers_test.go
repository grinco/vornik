package executor

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// TestMarkRetryable_NilInputReturnsNil ensures the nil-passthrough
// contract: handlers can chain markRetryable(err) without a nil
// guard on every call site.
func TestMarkRetryable_NilInputReturnsNil(t *testing.T) {
	assert.Nil(t, markRetryable(nil))
}

// TestMarkRetryable_WrapsErrorPreservingMessageAndUnwrap pins the
// shape: inner err is preserved, errors.Is works, and Unwrap returns
// the original.
func TestMarkRetryable_WrapsErrorPreservingMessageAndUnwrap(t *testing.T) {
	inner := errors.New("network hiccup")
	wrapped := markRetryable(inner)
	require.NotNil(t, wrapped)
	assert.Equal(t, "network hiccup", wrapped.Error())
	assert.True(t, errors.Is(wrapped, inner))
}

// TestFindSwarmRole covers all three branches: nil swarm, present
// role, absent role.
func TestFindSwarmRole(t *testing.T) {
	t.Run("nil swarm errors", func(t *testing.T) {
		_, err := findSwarmRole(nil, "coder")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "swarm config")
	})
	t.Run("present role returns pointer", func(t *testing.T) {
		swarm := &registry.Swarm{
			ID: "swarm-1",
			Roles: []registry.SwarmRole{
				{Name: "coder"},
				{Name: "reviewer"},
			},
		}
		got, err := findSwarmRole(swarm, "reviewer")
		assert.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "reviewer", got.Name)
	})
	t.Run("absent role errors with swarm ID and role name", func(t *testing.T) {
		swarm := &registry.Swarm{
			ID:    "swarm-1",
			Roles: []registry.SwarmRole{{Name: "coder"}},
		}
		_, err := findSwarmRole(swarm, "absent")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "absent")
		assert.Contains(t, err.Error(), "swarm-1")
	})
}

// TestTaskWorkflowID covers nil task, empty workflow id, dash
// placeholder, and explicit value.
func TestTaskWorkflowID(t *testing.T) {
	t.Run("nil task → default", func(t *testing.T) {
		assert.Equal(t, "default-workflow", taskWorkflowID(nil))
	})
	t.Run("nil WorkflowID → default", func(t *testing.T) {
		assert.Equal(t, "default-workflow", taskWorkflowID(&persistence.Task{}))
	})
	t.Run("empty WorkflowID → default", func(t *testing.T) {
		empty := ""
		assert.Equal(t, "default-workflow", taskWorkflowID(&persistence.Task{WorkflowID: &empty}))
	})
	t.Run("dash WorkflowID → default", func(t *testing.T) {
		dash := "-"
		assert.Equal(t, "default-workflow", taskWorkflowID(&persistence.Task{WorkflowID: &dash}))
	})
	t.Run("explicit WorkflowID returned", func(t *testing.T) {
		wf := "custom-wf"
		assert.Equal(t, "custom-wf", taskWorkflowID(&persistence.Task{WorkflowID: &wf}))
	})
}

func TestSerializeCheckpointMetadata(t *testing.T) {
	t.Run("nil checkpoint errors", func(t *testing.T) {
		_, err := SerializeCheckpointMetadata(nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "nil checkpoint")
	})
	t.Run("populated checkpoint round-trips", func(t *testing.T) {
		cp := &CheckpointPayload{
			Question:     "approve plan?",
			TaskForHuman: "review and approve",
		}
		bytes, err := SerializeCheckpointMetadata(cp)
		require.NoError(t, err)
		var back CheckpointPayload
		require.NoError(t, json.Unmarshal(bytes, &back))
		assert.Equal(t, cp.Question, back.Question)
		assert.Equal(t, cp.TaskForHuman, back.TaskForHuman)
	})
}

// TestIsTaskCancelled_NotFoundIsFalse pins the documented contract:
// a missing task is treated as not-cancelled so a transient repo
// miss doesn't bleed into the cancellation check.
func TestIsTaskCancelled_NotFoundIsFalse(t *testing.T) {
	e, _, _, _, tr := setup()
	// Add no task → Get returns ErrNotFound → method returns false.
	assert.False(t, e.isTaskCancelled(context.Background(), "no-such-task"))
	_ = tr
}
