package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/registry"
)

// fakeRegistrar captures RegisterFollowup calls so the test can
// assert that the dispatcher hands the bot the right (chat, task,
// project) triple. Thread-safe because, in production, the call
// fires from the receiver-driven dispatcher goroutine while the
// executor notify path could land on another goroutine.
type fakeRegistrar struct {
	mu    sync.Mutex
	calls []followupCall
}

type followupCall struct {
	chatID    int64
	taskID    string
	projectID string
}

func (f *fakeRegistrar) RegisterFollowup(chatID int64, taskID, projectID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, followupCall{chatID: chatID, taskID: taskID, projectID: projectID})
}

func (f *fakeRegistrar) snapshot() []followupCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]followupCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// TestCreateTask_AwaitCompletionRegistersFollowup — the regression
// the entire FollowupRegistrar pattern exists for: when the LLM
// passes await_completion=true, the dispatcher must hand the bot
// the chat/task/project triple so the bot can auto-resume the
// conversation when the task completes. Without this hook the
// "schedule a task to refresh data" pattern silently leaves the
// user waiting forever, which is the bug that motivated the
// feature.
func TestCreateTask_AwaitCompletionRegistersFollowup(t *testing.T) {
	reg := loadDispatcherTestRegistry(t, "dev-workflow", 50)
	taskRepo := &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}}
	registrar := &fakeRegistrar{}
	te := &ToolExecutor{
		registry:          reg,
		taskRepo:          taskRepo,
		followupRegistrar: registrar,
		logger:            zerolog.Nop(),
	}

	args := map[string]any{
		"project_id":       "p1",
		"type":             "feature",
		"prompt":           "summarise the latest scrape",
		"await_completion": true,
	}
	argsJSON, _ := json.Marshal(args)

	const chatID = int64(7777)
	res := te.createTask(context.Background(), string(argsJSON), "p1", []string{"p1"}, chatID)
	require.NotEmpty(t, res.Content)
	require.NotNil(t, taskRepo.last, "task must persist for followup to mean anything")

	calls := registrar.snapshot()
	require.Len(t, calls, 1, "exactly one followup must be registered for await_completion=true")
	assert.Equal(t, chatID, calls[0].chatID)
	assert.Equal(t, taskRepo.last.ID, calls[0].taskID,
		"followup task_id must match the persisted task — otherwise the bot will never see the completion")
	assert.Equal(t, "p1", calls[0].projectID)
}

// TestCreateTask_OmittedAwaitCompletionStillRegisters — auto-resume
// is the DEFAULT for chat-initiated tasks because small dispatcher
// models routinely drop the await_completion field on tool-call
// retries. Opt-in semantics left users stranded waiting for an
// answer the bot would never auto-deliver. With opt-out semantics,
// even a model that forgets the flag does the right thing.
func TestCreateTask_OmittedAwaitCompletionStillRegisters(t *testing.T) {
	reg := loadDispatcherTestRegistry(t, "dev-workflow", 50)
	taskRepo := &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}}
	registrar := &fakeRegistrar{}
	te := &ToolExecutor{
		registry:          reg,
		taskRepo:          taskRepo,
		followupRegistrar: registrar,
		logger:            zerolog.Nop(),
	}

	args := map[string]any{
		"project_id": "p1",
		"type":       "feature",
		"prompt":     "small-model retry path: no await_completion in args",
		// await_completion deliberately omitted.
	}
	argsJSON, _ := json.Marshal(args)

	te.createTask(context.Background(), string(argsJSON), "p1", []string{"p1"}, 99)
	calls := registrar.snapshot()
	require.Len(t, calls, 1, "default-on auto-resume must register even when await_completion is missing")
	assert.Equal(t, int64(99), calls[0].chatID)
}

// TestCreateTask_ExplicitFalseSkipsFollowup — the explicit opt-out
// path: if the LLM (or operator) genuinely wants fire-and-forget,
// passing await_completion=false skips the registrar so the chat
// won't be interrupted with the result. Rare but the contract
// matters: the dispatcher prompt advises against using it casually.
func TestCreateTask_ExplicitFalseSkipsFollowup(t *testing.T) {
	reg := loadDispatcherTestRegistry(t, "dev-workflow", 50)
	taskRepo := &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}}
	registrar := &fakeRegistrar{}
	te := &ToolExecutor{
		registry:          reg,
		taskRepo:          taskRepo,
		followupRegistrar: registrar,
		logger:            zerolog.Nop(),
	}

	args := map[string]any{
		"project_id":       "p1",
		"type":             "feature",
		"prompt":           "explicit fire-and-forget refactor",
		"await_completion": false,
	}
	argsJSON, _ := json.Marshal(args)

	te.createTask(context.Background(), string(argsJSON), "p1", []string{"p1"}, 99)
	assert.Empty(t, registrar.snapshot(),
		"explicit await_completion=false must skip the registrar")
}

// TestCreateTask_AwaitCompletionWithoutChatIsNoop — the dispatcher
// is also driven from the API and CLI which lack a chat context
// (chatID=0). In those callers await_completion has no auto-resume
// destination; the registrar call must be skipped rather than
// delivering a phantom followup keyed to chat 0.
func TestCreateTask_AwaitCompletionWithoutChatIsNoop(t *testing.T) {
	reg := loadDispatcherTestRegistry(t, "dev-workflow", 50)
	taskRepo := &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}}
	registrar := &fakeRegistrar{}
	te := &ToolExecutor{
		registry:          reg,
		taskRepo:          taskRepo,
		followupRegistrar: registrar,
		logger:            zerolog.Nop(),
	}

	args := map[string]any{
		"project_id":       "p1",
		"type":             "feature",
		"prompt":           "API-driven, no chat",
		"await_completion": true,
	}
	argsJSON, _ := json.Marshal(args)

	te.createTask(context.Background(), string(argsJSON), "p1", []string{"p1"}, 0)
	assert.Empty(t, registrar.snapshot(),
		"chatID=0 means there's no chat to auto-resume; skipping the registrar is correct")
}

// TestCreateTask_AwaitCompletionWithNilRegistrarIsSafe — keeps the
// dispatcher usable in deployments / tests where no bot is wired.
// Regression guard against a future refactor that drops the nil
// check and panics on the API code path.
func TestCreateTask_AwaitCompletionWithNilRegistrarIsSafe(t *testing.T) {
	reg := loadDispatcherTestRegistry(t, "dev-workflow", 50)
	taskRepo := &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}}
	te := &ToolExecutor{
		registry:          reg,
		taskRepo:          taskRepo,
		followupRegistrar: nil,
		logger:            zerolog.Nop(),
	}

	args := map[string]any{
		"project_id":       "p1",
		"type":             "feature",
		"prompt":           "no bot, no problem",
		"await_completion": true,
	}
	argsJSON, _ := json.Marshal(args)

	res := te.createTask(context.Background(), string(argsJSON), "p1", []string{"p1"}, 555)
	assert.NotEmpty(t, res.Content, "task creation must still succeed when registrar is nil")
}

// loadAllowedTypesTestRegistry stages a project that constrains
// task types via autonomy.allowedTaskTypes — the production knob
// the loop bug exercised. Returns the registry so the test
// asserts behaviour directly against the real project loader.
func loadAllowedTypesTestRegistry(t *testing.T, allowed []string) *registry.Registry {
	t.Helper()
	configDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(configDir, "projects"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(configDir, "swarms"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(configDir, "workflows"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "swarms", "s1.md"), []byte(`---
swarmId: "s1"
roles:
  - name: "researcher"
    runtime:
      image: "test:latest"
---
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "workflows", "research.md"), []byte(`---
workflowId: "research"
entrypoint: "run"
steps:
  run:
    type: "agent"
    prompt: "do work"
    role: "researcher"
    on_success: "done"
terminals:
  done:
    status: "COMPLETED"
---
`), 0o644))

	allowedYAML := ""
	for _, t := range allowed {
		allowedYAML += "\n    - " + t
	}
	body := fmt.Sprintf(`
projectId: "p1"
displayName: "Test Project"
swarmId: "s1"
defaultWorkflowId: "research"
autonomy:
  enabled: true
  allowedTaskTypes:%s
`, allowedYAML)
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "projects", "p1.yaml"), []byte(body), 0o644))
	reg := registry.New()
	require.NoError(t, reg.Load(configDir))
	return reg
}

// TestCreateTask_DefaultsTypeFromAllowedList — the small autonomy
// model loop bug: with allowedTaskTypes set, a missing `type` used
// to fall through to executeCreateTask which rejects with "type is
// required", and a wrong type was rejected by the dispatcher
// itself. Two simultaneous failure modes meant the LLM couldn't
// recover. The fix: when the project restricts types and the LLM
// omits one, default to the project's first allowed type rather
// than rejecting outright.
func TestCreateTask_DefaultsTypeFromAllowedList(t *testing.T) {
	reg := loadAllowedTypesTestRegistry(t, []string{"research", "planning", "writing"})
	taskRepo := &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}}
	te := &ToolExecutor{
		registry: reg,
		taskRepo: taskRepo,
		logger:   zerolog.Nop(),
	}

	args := map[string]any{
		"project_id": "p1",
		"prompt":     "small model dropped the type field on retry",
		// type omitted on purpose.
	}
	argsJSON, _ := json.Marshal(args)
	res := te.createTask(context.Background(), string(argsJSON), "p1", []string{"p1"}, 0)
	require.NotNil(t, taskRepo.last,
		"task must be created when type can be defaulted from allowedTaskTypes; got tool result: %s", res.Content)
	// The auto-default lands on the persisted payload via input.taskType.
	require.NotNil(t, taskRepo.last.Payload, "task payload must carry the resolved type")
}

// TestCreateTask_RejectsTypeNotInAllowedList — the explicit
// rejection still has to fire when the LLM picks a type the
// project forbids. The error text is the model's recovery prompt;
// it must list the allowed values directly so even a small model
// can copy one back.
func TestCreateTask_RejectsTypeNotInAllowedList(t *testing.T) {
	reg := loadAllowedTypesTestRegistry(t, []string{"research", "planning"})
	taskRepo := &capturingTaskRepo{MockTaskRepository: &mocks.MockTaskRepository{}}
	te := &ToolExecutor{
		registry: reg,
		taskRepo: taskRepo,
		logger:   zerolog.Nop(),
	}

	args := map[string]any{
		"project_id": "p1",
		"type":       "feature",
		"prompt":     "wrong type",
	}
	argsJSON, _ := json.Marshal(args)
	res := te.createTask(context.Background(), string(argsJSON), "p1", []string{"p1"}, 0)
	assert.Nil(t, taskRepo.last, "rejected task must NOT be persisted")
	assert.Contains(t, res.Content, `"feature"`,
		"error must echo the rejected value so the LLM sees what it picked")
	assert.Contains(t, res.Content, "research",
		"error must list the allowed values verbatim — the LLM's recovery prompt depends on this")
	assert.Contains(t, res.Content, "planning",
		"error must list ALL allowed values, not just one")
	assert.NotContains(t, res.Content, "Omit workflow_id",
		"error message must not give the misleading 'omit workflow_id' advice — the fix is to change type, not workflow")
}
