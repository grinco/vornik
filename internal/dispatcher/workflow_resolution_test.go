package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/registry"
)

// loadDispatcherTestRegistry stages a minimal project + swarm + workflow
// triple on disk and returns the loaded registry. Mirrors the helper
// pattern used in autonomy/manager_test.go since neither package
// exposes a programmatic registry-set API. defaultPriority is
// included so tests covering the priority-resolution path can pin a
// non-50 value and assert it lands on the persisted task.
func loadDispatcherTestRegistry(t *testing.T, defaultWorkflowID string, defaultPriority int) *registry.Registry {
	t.Helper()
	configDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(configDir, "projects"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(configDir, "swarms"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(configDir, "workflows"), 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(configDir, "swarms", "s1.md"), []byte(`---
swarmId: "s1"
roles:
  - name: "coder"
    runtime:
      image: "test:latest"
---
`), 0o644))

	// Two workflows so we can prove the dispatcher picks the project
	// default rather than promoting a same-named task type.
	for _, wf := range []string{"dev-workflow", "dynamic"} {
		body := []byte(`---
workflowId: "` + wf + `"
entrypoint: "run"
steps:
  run:
    type: "agent"
    prompt: "do work"
    role: "coder"
    on_success: "done"
terminals:
  done:
    status: "COMPLETED"
---
`)
		require.NoError(t, os.WriteFile(filepath.Join(configDir, "workflows", wf+".md"), body, 0o644))
	}

	require.NoError(t, os.WriteFile(filepath.Join(configDir, "projects", "p1.yaml"), []byte(fmt.Sprintf(`
projectId: "p1"
displayName: "Test Project"
swarmId: "s1"
defaultWorkflowId: "%s"
defaultPriority: %d
`, defaultWorkflowID, defaultPriority)), 0o644))

	reg := registry.New()
	require.NoError(t, reg.Load(configDir))
	return reg
}

// TestDispatcher_CreateTask_ResolvesProjectDefaultWorkflow — the bug
// that motivated this test: project "p1" has defaultWorkflowId
// "dev-workflow" but a chat user requests `create_task` with
// type="dynamic" and no workflow_id. Older dispatcher behaviour
// matched args.Type against the workflow registry and silently
// promoted the matching name to the task's workflow_id, so the task
// landed pinned to "dynamic" and bypassed the operator-set project
// default. With the type-as-workflow inference removed, the task
// must persist with workflow_id = the project's default.
func TestDispatcher_CreateTask_ResolvesProjectDefaultWorkflow(t *testing.T) {
	reg := loadDispatcherTestRegistry(t, "dev-workflow", 50)
	taskRepo := &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}}
	te := &ToolExecutor{
		registry: reg,
		taskRepo: taskRepo,
		logger:   zerolog.Nop(),
	}

	args := map[string]any{
		"project_id": "p1",
		// "dynamic" matches a workflow name — the regression hook.
		"type":   "dynamic",
		"prompt": "do the thing",
	}
	argsJSON, _ := json.Marshal(args)

	res := te.createTask(context.Background(), string(argsJSON), "p1", []string{"p1"}, 0)
	require.NotEmpty(t, res.Content, "expected non-empty result content")
	require.NotNil(t, taskRepo.last, "task should have been persisted")

	require.NotNil(t, taskRepo.last.WorkflowID,
		"task.WorkflowID must not be nil — operator's project default must land on the row")
	assert.Equal(t, "dev-workflow", *taskRepo.last.WorkflowID,
		"chat-initiated task with no explicit workflow_id must inherit the project default, not be promoted from args.Type")
}

// TestDispatcher_CreateTask_HonoursExplicitWorkflowID — the operator
// /agent passing an explicit workflow_id should win over the project
// default. Guards the override path that the project-default fallback
// must not regress.
func TestDispatcher_CreateTask_HonoursExplicitWorkflowID(t *testing.T) {
	reg := loadDispatcherTestRegistry(t, "dev-workflow", 50)
	taskRepo := &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}}
	te := &ToolExecutor{
		registry: reg,
		taskRepo: taskRepo,
		logger:   zerolog.Nop(),
	}

	args := map[string]any{
		"project_id":  "p1",
		"type":        "feature",
		"workflow_id": "dynamic",
		"prompt":      "do the thing",
	}
	argsJSON, _ := json.Marshal(args)
	te.createTask(context.Background(), string(argsJSON), "p1", []string{"p1"}, 0)

	require.NotNil(t, taskRepo.last)
	require.NotNil(t, taskRepo.last.WorkflowID)
	assert.Equal(t, "dynamic", *taskRepo.last.WorkflowID,
		"explicit workflow_id must override the project default")
}

// TestDispatcher_CreateTask_PlaceholderWorkflowIDFallsBack — LLMs
// occasionally emit "-" as a placeholder when they're told the field
// is optional. That value is sanitized to empty so the project
// default fallback applies.
func TestDispatcher_CreateTask_PlaceholderWorkflowIDFallsBack(t *testing.T) {
	reg := loadDispatcherTestRegistry(t, "dev-workflow", 50)
	taskRepo := &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}}
	te := &ToolExecutor{
		registry: reg,
		taskRepo: taskRepo,
		logger:   zerolog.Nop(),
	}

	args := map[string]any{
		"project_id":  "p1",
		"type":        "feature",
		"workflow_id": "-",
		"prompt":      "do the thing",
	}
	argsJSON, _ := json.Marshal(args)
	te.createTask(context.Background(), string(argsJSON), "p1", []string{"p1"}, 0)

	require.NotNil(t, taskRepo.last)
	require.NotNil(t, taskRepo.last.WorkflowID)
	assert.Equal(t, "dev-workflow", *taskRepo.last.WorkflowID,
		`workflow_id="-" placeholder must be sanitized to empty so the project default applies`)
}

// TestDispatcher_CreateTask_UsesProjectDefaultPriority — the
// regression operators reported: every Telegram-created task
// landed at priority=50 regardless of project.defaultPriority,
// because chat/parser.go.executeCreateTask hardcoded 50. The fix
// resolves project.DefaultPriority in the dispatcher before
// calling ExecuteAction; this test pins the behaviour so a future
// refactor can't quietly regress it.
func TestDispatcher_CreateTask_UsesProjectDefaultPriority(t *testing.T) {
	reg := loadDispatcherTestRegistry(t, "dev-workflow", 25)
	taskRepo := &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}}
	te := &ToolExecutor{
		registry: reg,
		taskRepo: taskRepo,
		logger:   zerolog.Nop(),
	}

	args := map[string]any{
		"project_id": "p1",
		"type":       "feature",
		"prompt":     "do the thing",
	}
	argsJSON, _ := json.Marshal(args)
	te.createTask(context.Background(), string(argsJSON), "p1", []string{"p1"}, 0)

	require.NotNil(t, taskRepo.last, "task should have been persisted")
	assert.Equal(t, 25, taskRepo.last.Priority,
		"chat-initiated task must inherit project.defaultPriority — hardcoded 50 was the symptom operators reported")
}

// TestDispatcher_CreateTask_FallsBackToFifty_NoProject — defensive
// edge: if the registry lookup misses (project removed mid-flight,
// or the dispatcher booted without a registry), priority lands on
// the chat-layer fallback of 50 rather than 0. Guards against a
// regression where the dispatcher passes priority=0 through and
// the chat layer's fallback is later removed without a partner
// fix here.
func TestDispatcher_CreateTask_FallsBackToFifty_NoProject(t *testing.T) {
	reg := registry.New() // empty registry — no projects loaded
	taskRepo := &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}}
	te := &ToolExecutor{
		registry: reg,
		taskRepo: taskRepo,
		logger:   zerolog.Nop(),
	}

	args := map[string]any{
		"project_id": "p1",
		"type":       "feature",
		"prompt":     "do the thing",
	}
	argsJSON, _ := json.Marshal(args)
	te.createTask(context.Background(), string(argsJSON), "p1", nil, 0)

	require.NotNil(t, taskRepo.last)
	assert.Equal(t, 50, taskRepo.last.Priority,
		"missing project lookup must end with the chat-layer fallback of 50, not 0")
}
