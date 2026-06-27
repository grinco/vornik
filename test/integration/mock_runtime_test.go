//go:build integration
// +build integration

package integration_test

import (
	"context"
	"fmt"
	"time"

	"vornik.io/vornik/internal/runtime"
)

type mockRuntimeManager struct {
	runs      map[string]*mockRun
	exitCode  int
	exitDelay time.Duration
}

type mockRun struct {
	done    chan struct{}
	code    int
	taskID  string
	started time.Time
}

func (m *mockRuntimeManager) StartContainer(ctx context.Context, cfg *runtime.ContainerConfig) (string, error) {
	run := &mockRun{
		done:    make(chan struct{}),
		code:    m.exitCode,
		taskID:  cfg.TaskID,
		started: time.Now(),
	}
	containerID := fmt.Sprintf("mock-container-%d", time.Now().UnixNano())
	m.runs[containerID] = run

	if m.exitDelay > 0 {
		go func() {
			time.Sleep(m.exitDelay)
			select {
			case <-run.done:
			default:
				close(run.done)
			}
		}()
	}

	return containerID, nil
}

func (m *mockRuntimeManager) StopContainer(ctx context.Context, containerID string, force bool) error {
	if run, ok := m.runs[containerID]; ok {
		select {
		case <-run.done:
		default:
			close(run.done)
		}
	}
	return nil
}

func (m *mockRuntimeManager) InspectContainer(ctx context.Context, containerID string) (*runtime.Container, error) {
	return nil, nil
}

func (m *mockRuntimeManager) WaitForExit(ctx context.Context, containerID string, timeout time.Duration) (int, error) {
	if run, ok := m.runs[containerID]; ok {
		if timeout <= 0 {
			select {
			case <-run.done:
				return run.code, nil
			case <-ctx.Done():
				return -1, ctx.Err()
			}
		}

		select {
		case <-run.done:
			return run.code, nil
		case <-ctx.Done():
			return -1, ctx.Err()
		case <-time.After(timeout):
			return -1, fmt.Errorf("timeout waiting for container exit")
		}
	}
	return 0, nil
}

func (m *mockRuntimeManager) GetContainerByTask(ctx context.Context, taskID string) (*runtime.Container, error) {
	for id, run := range m.runs {
		if run.taskID == taskID {
			return &runtime.Container{
				ID:     id,
				Status: "running",
			}, nil
		}
	}
	return nil, nil
}

func (m *mockRuntimeManager) RemoveContainer(ctx context.Context, containerID string, force bool) error {
	delete(m.runs, containerID)
	return nil
}

func (m *mockRuntimeManager) Logs(ctx context.Context, containerID string, tail int) (string, error) {
	return "", nil
}
