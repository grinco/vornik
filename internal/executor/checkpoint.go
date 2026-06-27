package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// maxCheckpointDepth caps the number of consecutive CHECKPOINT-source
// tasks the executor will chain. A pathologically expensive prompt
// that hits the iteration cap on every attempt would otherwise loop
// forever — each link in the chain costs LLM dollars. Three is enough
// to break a large feature into 3× the iteration budget without
// turning a stuck task into an unbounded burner.
const maxCheckpointDepth = 3

// checkpointPromptTemplate is the prompt the executor injects into
// the follow-up task when an agent step hit the tool-iteration limit.
// The template is deliberately short and points at the project's
// commit log rather than embedding the previous prompt verbatim — the
// upstream task's prompt is already in the project's git history (the
// lead's plan output and any role's commits) and the agent that picks
// up the continuation is expected to read the recent commit messages
// to understand what's been done.
//
// `previousPrompt` is the original task's prompt as best the
// executor could extract it (CreateTaskRequest.Context.prompt). May
// be empty when the parent had no payload — the message is still
// useful in that case because the lead can read git log.
const checkpointPromptTemplate = `[checkpoint continuation]

The previous task %s hit the agent's tool-iteration limit (VORNIK_MAX_TOOL_ITERATIONS) on step %s. Partial work was committed and merged to the project repository — read the most recent commits ('cd project && git log --oneline -8') to see what was completed.

Continue from there toward the original goal:

%s

If the work appears already done, verify against the original acceptance criteria and finish; otherwise pick up the next concrete subtask. Prefer one tight focused step over re-doing what the previous attempt accomplished.`

// scheduleCheckpointFollowUp creates a follow-up Task that continues
// the work of `parent` after the agent hit its tool-iteration limit.
// Returns the new task's ID on success, or an empty string + error
// when the chain has reached maxCheckpointDepth (in which case the
// parent should stay FAILED with the iteration-limit class and the
// operator gets a hard signal that splitting hasn't worked).
//
// The follow-up task:
//   - inherits ProjectID, WorkflowID, Priority from the parent
//   - sets ParentTaskID = parent.ID and CreationSource = CHECKPOINT
//     so dashboards can render the chain
//   - gets a fresh attempt budget (Attempt=1, MaxAttempts inherited)
//   - prompt is the checkpoint template above; the previous prompt
//     is best-effort extracted from the parent's Payload
//
// Workspace state from the parent is assumed to already be merged to
// the project's main branch — the caller (executor.go terminal-
// failure path) calls cleanupWorktree(true) before invoking this.
func (e *Executor) scheduleCheckpointFollowUp(ctx context.Context, parent *persistence.Task, failedStepID, errorMsg string) (string, error) {
	if parent == nil {
		return "", fmt.Errorf("nil parent task")
	}
	depth, err := e.countCheckpointDepth(ctx, parent)
	if err != nil {
		// Best-effort: a DB error on the depth walk shouldn't stop
		// the continuation. Log and proceed; the worst case is one
		// extra link in the chain, which is bounded by the next
		// failure's depth check.
		e.logger.Warn().Err(err).Str("task_id", parent.ID).
			Msg("checkpoint: depth walk failed, scheduling follow-up anyway")
		depth = 0
	}
	if depth >= maxCheckpointDepth {
		return "", fmt.Errorf("checkpoint depth %d reached cap %d — refusing further continuation", depth, maxCheckpointDepth)
	}

	prevPrompt := extractPromptFromTask(parent)
	if prevPrompt == "" {
		prevPrompt = "(original prompt unavailable; consult parent task " + parent.ID + " in the UI)"
	}

	prompt := fmt.Sprintf(checkpointPromptTemplate, parent.ID, failedStepID, prevPrompt)
	payload, err := json.Marshal(map[string]any{
		"taskType": "continuation",
		"context": map[string]any{
			"prompt": prompt,
		},
	})
	if err != nil {
		return "", fmt.Errorf("marshal checkpoint payload: %w", err)
	}

	maxAttempts := parent.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 1
	}

	parentID := parent.ID
	child := &persistence.Task{
		ID:             persistence.GenerateID("task"),
		ProjectID:      parent.ProjectID,
		WorkflowID:     parent.WorkflowID,
		ParentTaskID:   &parentID,
		CreationSource: persistence.TaskCreationSourceCheckpoint,
		Status:         persistence.TaskStatusQueued,
		Priority:       parent.Priority,
		Payload:        payload,
		Attempt:        1,
		MaxAttempts:    maxAttempts,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}

	if err := e.taskRepo.Create(ctx, child); err != nil {
		return "", fmt.Errorf("create checkpoint task: %w", err)
	}

	e.logger.Info().
		Str("parent_task_id", parent.ID).
		Str("child_task_id", child.ID).
		Str("project_id", parent.ProjectID).
		Str("failed_step_id", failedStepID).
		Int("depth", depth+1).
		Str("error", errorMsg).
		Msg("checkpoint: scheduled follow-up after iteration-limit failure")
	return child.ID, nil
}

// countCheckpointDepth walks task.ParentTaskID upward and returns the
// number of consecutive CHECKPOINT-source ancestors. The walk stops
// at the first non-checkpoint task (typically the original USER /
// AUTONOMOUS / DELEGATION task) and at any chain longer than
// maxCheckpointDepth*2 to bound a malformed loop. A nil ParentTaskID
// terminates cleanly with depth=0.
func (e *Executor) countCheckpointDepth(ctx context.Context, t *persistence.Task) (int, error) {
	depth := 0
	cursor := t
	guard := maxCheckpointDepth*2 + 1
	for cursor != nil && cursor.ParentTaskID != nil && guard > 0 {
		guard--
		parent, err := e.taskRepo.Get(ctx, *cursor.ParentTaskID)
		if err != nil {
			return depth, err
		}
		if parent == nil {
			return depth, nil
		}
		if parent.CreationSource != persistence.TaskCreationSourceCheckpoint {
			return depth, nil
		}
		depth++
		cursor = parent
	}
	return depth, nil
}

// extractPromptFromTask returns the original prompt from a task's
// payload, or "" when the payload is missing / not JSON / has no
// prompt field. Mirrors the shape autonomy/manager.go writes
// (CreateTaskRequest.Context.prompt). Best-effort: an unparseable
// payload yields "" so the caller substitutes a placeholder.
func extractPromptFromTask(t *persistence.Task) string {
	if t == nil || len(t.Payload) == 0 {
		return ""
	}
	var typed struct {
		Context struct {
			Prompt string `json:"prompt"`
		} `json:"context"`
	}
	if err := json.Unmarshal(t.Payload, &typed); err != nil {
		return ""
	}
	return typed.Context.Prompt
}
