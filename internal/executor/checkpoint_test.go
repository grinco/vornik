package executor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
)

// makeExecutorForCheckpointTests builds a minimal Executor wired only
// with the dependencies scheduleCheckpointFollowUp and
// countCheckpointDepth touch — taskRepo + logger. The runtime,
// runtime config, scheduler, etc. stay nil because the checkpoint
// helpers don't reach for them. Keeps test setup small enough that
// the assertion logic stays the focus of the file.
func makeExecutorForCheckpointTests(repo *MockTaskRepo) *Executor {
	return &Executor{
		taskRepo: repo,
		logger:   zerolog.Nop(),
	}
}

func TestExtractPromptFromTask(t *testing.T) {
	cases := []struct {
		name    string
		payload []byte
		want    string
	}{
		{name: "well-formed", payload: []byte(`{"context":{"prompt":"do the thing"}}`), want: "do the thing"},
		{name: "missing prompt", payload: []byte(`{"context":{}}`), want: ""},
		{name: "missing context", payload: []byte(`{"taskType":"x"}`), want: ""},
		{name: "not json", payload: []byte(`oops`), want: ""},
		{name: "empty payload", payload: nil, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task := &persistence.Task{Payload: tc.payload}
			assert.Equal(t, tc.want, extractPromptFromTask(task))
		})
	}
	assert.Equal(t, "", extractPromptFromTask(nil), "nil task is safe and returns empty prompt")
}

func TestScheduleCheckpointFollowUp_HappyPath(t *testing.T) {
	repo := NewMockTaskRepo()
	e := makeExecutorForCheckpointTests(repo)

	wfID := "adaptive"
	parent := &persistence.Task{
		ID:             "task_parent",
		ProjectID:      "proj-1",
		WorkflowID:     &wfID,
		CreationSource: persistence.TaskCreationSourceUser,
		Status:         persistence.TaskStatusFailed,
		Priority:       42,
		MaxAttempts:    3,
		Payload:        []byte(`{"taskType":"feature","context":{"prompt":"implement the X subsystem end-to-end"}}`),
	}
	repo.AddTask(parent)

	childID, err := e.scheduleCheckpointFollowUp(context.Background(), parent, "implement", "tool iteration cap reached (50 iterations)")
	require.NoError(t, err)
	require.NotEmpty(t, childID)

	child, err := repo.Get(context.Background(), childID)
	require.NoError(t, err)
	require.NotNil(t, child)

	assert.Equal(t, persistence.TaskCreationSourceCheckpoint, child.CreationSource)
	assert.Equal(t, persistence.TaskStatusQueued, child.Status)
	assert.Equal(t, parent.Priority, child.Priority)
	assert.Equal(t, parent.MaxAttempts, child.MaxAttempts)
	require.NotNil(t, child.ParentTaskID)
	assert.Equal(t, parent.ID, *child.ParentTaskID)
	require.NotNil(t, child.WorkflowID)
	assert.Equal(t, *parent.WorkflowID, *child.WorkflowID)

	// Prompt should reference the parent ID, the failed step, and the
	// previous prompt verbatim — that's what makes the continuation
	// workable for the lead.
	prompt := extractPromptFromTask(child)
	assert.Contains(t, prompt, parent.ID, "checkpoint prompt should reference parent task ID")
	assert.Contains(t, prompt, "implement", "checkpoint prompt should reference the failed step ID")
	assert.Contains(t, prompt, "implement the X subsystem end-to-end", "checkpoint prompt should embed the original goal")
}

func TestScheduleCheckpointFollowUp_NilParent(t *testing.T) {
	e := makeExecutorForCheckpointTests(NewMockTaskRepo())
	_, err := e.scheduleCheckpointFollowUp(context.Background(), nil, "x", "y")
	require.Error(t, err)
}

func TestScheduleCheckpointFollowUp_DepthCapStopsRunaway(t *testing.T) {
	repo := NewMockTaskRepo()
	e := makeExecutorForCheckpointTests(repo)

	// Build a chain: original USER task → 3 CHECKPOINT children
	// already in the DB. The 4th attempt (this scheduling call)
	// should refuse because depth has reached the cap.
	original := &persistence.Task{ID: "task_root", ProjectID: "p", CreationSource: persistence.TaskCreationSourceUser}
	repo.AddTask(original)
	previous := original.ID
	for i := 0; i < maxCheckpointDepth; i++ {
		t := &persistence.Task{
			ID:             "task_chk_" + string(rune('a'+i)),
			ProjectID:      "p",
			CreationSource: persistence.TaskCreationSourceCheckpoint,
			ParentTaskID:   &previous,
		}
		repo.AddTask(t)
		previous = t.ID
	}

	leaf, err := repo.Get(context.Background(), previous)
	require.NoError(t, err)

	_, err = e.scheduleCheckpointFollowUp(context.Background(), leaf, "implement", "iteration cap")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "checkpoint depth")
}

func TestCountCheckpointDepth_StopsAtNonCheckpointAncestor(t *testing.T) {
	repo := NewMockTaskRepo()
	e := makeExecutorForCheckpointTests(repo)

	// A user task that delegated to a child — the delegation chain is
	// NOT a checkpoint chain, so the counter should return 0 even
	// though there's a parent relationship.
	user := &persistence.Task{ID: "u", CreationSource: persistence.TaskCreationSourceUser}
	repo.AddTask(user)
	delegated := &persistence.Task{ID: "d", CreationSource: persistence.TaskCreationSourceDelegation, ParentTaskID: &user.ID}
	repo.AddTask(delegated)

	depth, err := e.countCheckpointDepth(context.Background(), delegated)
	require.NoError(t, err)
	assert.Equal(t, 0, depth, "delegation parent does not count as a checkpoint")
}

func TestClassifyExecutionFailure_IterationLimit(t *testing.T) {
	// The agent's hardcoded message must classify into
	// TOOL_ITERATION_LIMIT rather than the generic TOOL_ERROR — that's
	// what makes the executor's terminal-failure path divert into the
	// checkpoint+continuation branch.
	msg := "agent reported FAILED status: Tool iteration limit (50) reached. The task was too complex for the configured limit."
	got := ClassifyExecutionFailure(nil, msg)
	assert.Equal(t, persistence.TaskFailureClassToolIterationLimit, got)
}

func TestClassifyExecutionFailure_SecretLeak(t *testing.T) {
	// Phase 2: the executor surfaces secret-leak Block as
	// "secret_leak: N finding(s)" via ErrSecretLeakBlocked; the
	// classifier must map that to SECRET_LEAK rather than fall
	// through to UNKNOWN or LLM_ERROR. Match must come BEFORE the
	// generic LLM/tool fall-throughs.
	got := ClassifyExecutionFailure(nil, "secret_leak: 2 finding(s)")
	assert.Equal(t, persistence.TaskFailureClassSecretLeak, got)
}

func TestClassifyExecutionFailure_SecretLeakBeatsToolError(t *testing.T) {
	// Defensive: when the failure message mentions both "secret_leak"
	// and a tool-class word ("tool", "shell"), SECRET_LEAK must win
	// because it's the more specific class. The classifier orders the
	// secret-leak match before the tool branches; this test pins
	// that ordering.
	got := ClassifyExecutionFailure(nil, "tool run_shell: secret_leak: 1 finding(s)")
	assert.Equal(t, persistence.TaskFailureClassSecretLeak, got)
}

// TestCheckpointPayloadShapeIsConsumable: the payload the executor
// writes for a checkpoint task must be parseable by the same
// CreateTaskRequest shape the rest of the daemon uses, so the agent
// runtime's prompt-extraction helpers find the prompt without a
// dedicated parser. This is a contract test rather than a behaviour
// test — it'd catch a future drift where someone restructures the
// payload to {"prompt": "..."} at top level (which would make leads
// see no prompt and fall back to "(unspecified task)").
func TestCheckpointPayloadShapeIsConsumable(t *testing.T) {
	repo := NewMockTaskRepo()
	e := makeExecutorForCheckpointTests(repo)
	parent := &persistence.Task{
		ID:             "task_parent",
		ProjectID:      "p",
		CreationSource: persistence.TaskCreationSourceUser,
		Payload:        []byte(`{"context":{"prompt":"original goal"}}`),
	}
	repo.AddTask(parent)
	childID, err := e.scheduleCheckpointFollowUp(context.Background(), parent, "implement", "")
	require.NoError(t, err)
	child, _ := repo.Get(context.Background(), childID)

	var typed struct {
		TaskType string `json:"taskType"`
		Context  struct {
			Prompt string `json:"prompt"`
		} `json:"context"`
	}
	require.NoError(t, json.Unmarshal(child.Payload, &typed))
	assert.Equal(t, "continuation", typed.TaskType)
	assert.True(t, strings.Contains(typed.Context.Prompt, "original goal"))
}
