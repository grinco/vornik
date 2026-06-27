package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLifecyclePolicyConstants(t *testing.T) {
	assert.Equal(t, LifecyclePolicy("ephemeral"), PolicyEphemeral)
	assert.Equal(t, LifecyclePolicy("warm"), PolicyWarm)
}

func TestStatusConstants(t *testing.T) {
	assert.Equal(t, Status("running"), StatusRunning)
	assert.Equal(t, Status("stopped"), StatusStopped)
	assert.Equal(t, Status("exited"), StatusExited)
	assert.Equal(t, Status("paused"), StatusPaused)
	assert.Equal(t, Status("unknown"), StatusUnknown)
	assert.Equal(t, Status("not_found"), StatusNotFound)
}

func TestContainerName(t *testing.T) {
	tests := []struct {
		name      string
		projectID string
		role      string
		taskID    string
	}{
		{
			name:      "simple",
			projectID: "my-project",
			role:      "coder",
			taskID:    "task-123",
		},
		{
			name:      "with special chars",
			projectID: "My_Project-123!",
			role:      "tester",
			taskID:    "task@456",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name := ContainerName(tt.projectID, tt.role, tt.taskID)
			assert.Contains(t, name, "vornik-")
			assert.NotEmpty(t, name)
		})
	}
}

func TestContainerName_Empty(t *testing.T) {
	name := ContainerName("", "", "")
	assert.Contains(t, name, "vornik-")
}

func TestContainerConfig_Validate(t *testing.T) {
	t.Run("valid config", func(t *testing.T) {
		config := &ContainerConfig{
			Image:     "vornik/agent:latest",
			ProjectID: "project-1",
			Role:      "coder",
			TaskID:    "task-1",
		}
		err := config.Validate()
		assert.NoError(t, err)
	})

	t.Run("network host denied by default", func(t *testing.T) {
		t.Setenv("VORNIK_ALLOW_NETWORK_HOST", "")
		config := &ContainerConfig{
			Image: "vornik/agent:latest", ProjectID: "p", Role: "coder", TaskID: "t",
			Network: NetworkHost,
		}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "network: host is disabled by default")
	})

	t.Run("network host allowed with explicit opt-in", func(t *testing.T) {
		t.Setenv("VORNIK_ALLOW_NETWORK_HOST", "1")
		config := &ContainerConfig{
			Image: "vornik/agent:latest", ProjectID: "p", Role: "coder", TaskID: "t",
			Network: NetworkHost,
		}
		assert.NoError(t, config.Validate())
	})

	t.Run("network none always allowed", func(t *testing.T) {
		config := &ContainerConfig{
			Image: "vornik/agent:latest", ProjectID: "p", Role: "coder", TaskID: "t",
			Network: NetworkNone,
		}
		assert.NoError(t, config.Validate())
	})

	t.Run("missing image", func(t *testing.T) {
		config := &ContainerConfig{
			ProjectID: "project-1",
			Role:      "coder",
			TaskID:    "task-1",
		}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "image is required")
	})

	t.Run("image option injection", func(t *testing.T) {
		config := &ContainerConfig{
			Image:     "--privileged",
			ProjectID: "project-1",
			Role:      "coder",
			TaskID:    "task-1",
		}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "must not start")
	})

	t.Run("trims required fields", func(t *testing.T) {
		config := &ContainerConfig{
			Image:     " vornik/agent:latest ",
			ProjectID: " project-1 ",
			Role:      " coder ",
			TaskID:    " task-1 ",
		}
		require.NoError(t, config.Validate())
		assert.Equal(t, "vornik/agent:latest", config.Image)
		assert.Equal(t, "project-1", config.ProjectID)
		assert.Equal(t, "coder", config.Role)
		assert.Equal(t, "task-1", config.TaskID)
	})

	t.Run("missing projectID", func(t *testing.T) {
		config := &ContainerConfig{
			Image:  "vornik/agent:latest",
			Role:   "coder",
			TaskID: "task-1",
		}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "projectId is required")
	})

	t.Run("missing role", func(t *testing.T) {
		config := &ContainerConfig{
			Image:     "vornik/agent:latest",
			ProjectID: "project-1",
			TaskID:    "task-1",
		}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "role is required")
	})

	t.Run("missing taskID", func(t *testing.T) {
		config := &ContainerConfig{
			Image:     "vornik/agent:latest",
			ProjectID: "project-1",
			Role:      "coder",
		}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "taskId is required")
	})
}

func TestContextWithOptionalTimeout_ZeroDoesNotExpireImmediately(t *testing.T) {
	ctx, cancel := contextWithOptionalTimeout(context.Background(), 0)
	defer cancel()

	select {
	case <-ctx.Done():
		t.Fatal("zero timeout must not create an already-expired context")
	default:
	}
}

func TestContextWithOptionalTimeout_PositiveExpires(t *testing.T) {
	ctx, cancel := contextWithOptionalTimeout(context.Background(), time.Nanosecond)
	defer cancel()

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("positive timeout did not expire")
	}
}

func TestDefaultContainerConfig(t *testing.T) {
	config := DefaultContainerConfig()
	require.NotNil(t, config)
	assert.Equal(t, PolicyEphemeral, config.LifecyclePolicy)
}

func TestContainer_Fields(t *testing.T) {
	container := &Container{
		ID:        "container-123",
		Name:      "vornik-project-coder-task",
		Image:     "vornik/agent:latest",
		Status:    StatusRunning,
		ProjectID: "project-1",
		Role:      "coder",
		TaskID:    "task-1",
		ExitCode:  0,
	}

	assert.Equal(t, "container-123", container.ID)
	assert.Equal(t, StatusRunning, container.Status)
	assert.Equal(t, 0, container.ExitCode)
}

func TestContainerConfig_Fields(t *testing.T) {
	config := &ContainerConfig{
		Image:           "vornik/agent:latest",
		ProjectID:       "project-1",
		Role:            "coder",
		TaskID:          "task-1",
		LifecyclePolicy: PolicyWarm,
		EnvVars:         map[string]string{"FOO": "bar"},
		CPUQuota:        100000,
		MemoryLimit:     1024 * 1024 * 512,
		InputDir:        "/input",
		OutputDir:       "/output",
		WorkspaceDir:    "/workspace",
		TimeoutSeconds:  300,
	}

	assert.Equal(t, "vornik/agent:latest", config.Image)
	assert.Equal(t, PolicyWarm, config.LifecyclePolicy)
	assert.Equal(t, "bar", config.EnvVars["FOO"])
	assert.Equal(t, int64(100000), config.CPUQuota)
	assert.Equal(t, 300, config.TimeoutSeconds)
}
