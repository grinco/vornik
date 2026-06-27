// Package chat provides an OpenAI-compatible chat client for vornik.
package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/budget"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// BudgetGate carries the dependencies executeCreateTask needs to atomically
// reserve hard-cap budget for a chat-created task (trading-hardening §1). The
// chat path is otherwise a budget blind spot — the dispatcher does a
// read-only budget.Check upstream, but the actual taskRepo.Create happens
// here, so without this gate a chat/DM-created task reserves nothing and the
// hard cap stays best-effort. A nil gate (or any nil field) skips the
// reservation — callers without budget wiring keep the legacy behavior.
type BudgetGate struct {
	Reservations persistence.BudgetReservationRepository
	Usage        budget.Repo
	Project      *registry.Project
}

// Action represents a parsed action from the LLM response.
type Action struct {
	Type       string `json:"action"`
	Project    string `json:"project,omitempty"`
	TaskID     string `json:"task_id,omitempty"`
	Status     string `json:"status,omitempty"`
	Type_      string `json:"type,omitempty"`        // for create_task
	WorkflowID string `json:"workflow_id,omitempty"` // for create_task
	// Priority is the per-task scheduling weight on a 0-100 scale,
	// resolved by the caller from project.DefaultPriority before
	// executeCreateTask runs. Zero is treated as "use the
	// compiled-in default of 50" — the chat package can't reach
	// the registry directly, so the dispatcher / API caller is
	// responsible for honouring the project default and passing
	// the resolved value here.
	Priority int                    `json:"priority,omitempty"` // for create_task
	Input    map[string]interface{} `json:"input,omitempty"`
	// Confirm gates destructive actions (cancel_task, retry_task). The
	// server refuses to execute a destructive action unless Confirm is
	// true, returning a confirmation prompt instead — so an ambiguous or
	// accidental request can't cancel/retry in a single turn. The caller
	// (chat agent) sets this only after the user has explicitly agreed.
	Confirm bool `json:"confirm,omitempty"`
	// ChatTurnID, when non-empty, is stamped onto Task.ChatTurnID
	// during create_task so the dispatcher turn that spawned the task
	// can be queried later (in-conversation dedup, follow-up
	// coalescing, audit). Empty for API-initiated and autonomous
	// task creation. See dispatcher.WithChatTurnID for the
	// upstream propagation.
	ChatTurnID string `json:"-"`
}

// Action types supported by the parser.
const (
	ActionListTasks  = "list_tasks"
	ActionCreateTask = "create_task"
	ActionCancelTask = "cancel_task"
	ActionRetryTask  = "retry_task"
	ActionGetStatus  = "get_status"
)

// DefaultSystemPrompt is the default system prompt for the task assistant.
const DefaultSystemPrompt = `You are a task scheduling assistant for vornik.

Available actions:
- list_tasks(project, status?) → list tasks
- create_task(project, type, input?) → create task
- cancel_task(task_id) → cancel a task
- retry_task(task_id) → retry a failed task
- get_status(task_id) → get task status

Respond naturally. When executing an action, include a JSON block:
` + "```json\n" + `{"action": "create_task", "project": "alpha", "type": "backup"}` + "\n```\n"

// ActionResult contains the result of an action execution.
type ActionResult struct {
	Success bool        `json:"success"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
	// NeedsConfirmation is true when a destructive action was refused
	// pending explicit confirmation (Message carries the prompt). The
	// action was NOT executed; nothing changed.
	NeedsConfirmation bool `json:"needs_confirmation,omitempty"`
}

// isDestructiveAction reports whether an action mutates task lifecycle in
// a way that warrants explicit user confirmation before it runs.
func isDestructiveAction(actionType string) bool {
	return actionType == ActionCancelTask || actionType == ActionRetryTask
}

// confirmationPrompt is the message returned when a destructive action is
// refused pending confirmation.
func confirmationPrompt(action Action) string {
	switch action.Type {
	case ActionCancelTask:
		return fmt.Sprintf("Confirmation required: cancelling task %s stops in-progress work and cannot be undone. "+
			"Confirm with the user, then re-issue cancel_task with confirm=true.", action.TaskID)
	case ActionRetryTask:
		return fmt.Sprintf("Confirmation required: retrying task %s re-runs it and spends additional budget. "+
			"Confirm with the user, then re-issue retry_task with confirm=true.", action.TaskID)
	default:
		return fmt.Sprintf("Confirmation required for %s; re-issue with confirm=true after the user agrees.", action.Type)
	}
}

// ParseActions extracts JSON action blocks from LLM response.
// It looks for JSON objects with an "action" field.
func ParseActions(response string) []Action {
	var actions []Action

	// Find JSON objects by bracket counting
	for i := 0; i < len(response); i++ {
		if response[i] != '{' {
			continue
		}

		// Find the matching closing brace
		depth := 0
		inString := false
		escape := false
		end := -1

		for j := i; j < len(response); j++ {
			ch := response[j]

			if escape {
				escape = false
				continue
			}

			switch ch {
			case '\\':
				escape = true
			case '"':
				inString = !inString
			case '{':
				if !inString {
					depth++
				}
			case '}':
				if !inString {
					depth--
					if depth == 0 {
						end = j + 1
						break
					}
				}
			}
			if end > 0 {
				break
			}
		}

		if end < 0 {
			continue
		}

		jsonStr := response[i:end]
		var action Action
		if err := json.Unmarshal([]byte(jsonStr), &action); err == nil {
			if action.Type != "" {
				actions = append(actions, action)
			}
		}
		i = end - 1 // Move past this JSON object
	}

	return actions
}

// ExecuteAction executes an action against the task API.
func ExecuteAction(ctx context.Context, action Action, taskRepo persistence.TaskRepository, execRepo persistence.ExecutionRepository, gate *BudgetGate) (ActionResult, error) {
	// Confirmation gate: a destructive action must carry Confirm=true or
	// it is refused (not executed) with a confirmation prompt, so an
	// ambiguous request can't cancel/retry in a single turn (security LLD
	// review batch 3). Authz is enforced separately at the tool layer.
	if isDestructiveAction(action.Type) && !action.Confirm {
		return ActionResult{
			Success:           false,
			NeedsConfirmation: true,
			Message:           confirmationPrompt(action),
		}, nil
	}
	switch action.Type {
	case ActionListTasks:
		return executeListTasks(ctx, action, taskRepo)
	case ActionCreateTask:
		return executeCreateTask(ctx, action, taskRepo, gate)
	case ActionCancelTask:
		return executeCancelTask(ctx, action, taskRepo)
	case ActionRetryTask:
		return executeRetryTask(ctx, action, taskRepo)
	case ActionGetStatus:
		return executeGetStatus(ctx, action, taskRepo, execRepo)
	default:
		return ActionResult{
			Success: false,
			Message: fmt.Sprintf("Unknown action: %s", action.Type),
		}, fmt.Errorf("unknown action: %s", action.Type)
	}
}

// executeListTasks handles the list_tasks action.
func executeListTasks(ctx context.Context, action Action, taskRepo persistence.TaskRepository) (ActionResult, error) {
	filter := persistence.TaskFilter{
		PageSize: 20,
	}

	if action.Project != "" {
		filter.ProjectID = &action.Project
	}

	if action.Status != "" {
		status := persistence.TaskStatus(strings.ToUpper(action.Status))
		filter.Status = &status
	}

	tasks, err := taskRepo.List(ctx, filter)
	if err != nil {
		return ActionResult{
			Success: false,
			Message: fmt.Sprintf("Failed to list tasks: %v", err),
		}, err
	}

	if len(tasks) == 0 {
		return ActionResult{
			Success: true,
			Message: "No tasks found.",
			Data:    []*persistence.Task{},
		}, nil
	}

	var summary strings.Builder
	fmt.Fprintf(&summary, "Found %d task(s):\n", len(tasks))
	for i, task := range tasks {
		fmt.Fprintf(&summary, "%d. %s (%s) - %s\n", i+1, task.ID, task.ProjectID, task.Status)
	}

	return ActionResult{
		Success: true,
		Message: summary.String(),
		Data:    tasks,
	}, nil
}

// executeCreateTask handles the create_task action.
func executeCreateTask(ctx context.Context, action Action, taskRepo persistence.TaskRepository, gate *BudgetGate) (ActionResult, error) {
	if action.Project == "" {
		return ActionResult{
			Success: false,
			Message: "Project is required for create_task action.",
		}, fmt.Errorf("project is required for create_task")
	}

	if action.Type_ == "" {
		return ActionResult{
			Success: false,
			Message: "Type is required for create_task action.",
		}, fmt.Errorf("type is required for create_task")
	}

	// Priority is resolved by the caller (dispatcher.createTask)
	// from project.DefaultPriority before this point. The fallback
	// to 50 applies only when the caller passed zero — usually that
	// means the chat path is being driven by something other than
	// the dispatcher (older API callers that haven't been updated
	// to pre-resolve priority).
	priority := action.Priority
	if priority == 0 {
		priority = 50
	}
	task := &persistence.Task{
		ID:             persistence.GenerateID("task"),
		ProjectID:      action.Project,
		CreationSource: persistence.TaskCreationSourceUser,
		Status:         persistence.TaskStatusQueued,
		Priority:       priority,
		Attempt:        1,
		MaxAttempts:    3,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	if action.WorkflowID != "" {
		task.WorkflowID = &action.WorkflowID
	}
	if action.ChatTurnID != "" {
		turn := action.ChatTurnID
		task.ChatTurnID = &turn
	}

	// Set payload from input if provided
	if action.Input != nil {
		payload, err := json.Marshal(action.Input)
		if err != nil {
			return ActionResult{
				Success: false,
				Message: fmt.Sprintf("Failed to marshal input: %v", err),
			}, err
		}
		task.Payload = payload
	}

	// Atomic hard-cap reservation (trading-hardening §1): claim this task's
	// estimated spend against the cap before inserting, so chat/DM-created
	// tasks count toward the budget like every other admission path. FAIL
	// OPEN — a reservation-ledger error must never block legitimate work
	// (the dispatcher's upstream budget.Check + the watchdog sweep are the
	// backstops); a Blocked decision refuses the create with the reason.
	if gate != nil && gate.Reservations != nil && gate.Usage != nil && gate.Project != nil {
		if decision, rerr := budget.Reserve(ctx, gate.Reservations, gate.Usage, gate.Project, task.ID, time.Now().UTC()); rerr == nil && decision.Blocked {
			return ActionResult{
				Success: false,
				Message: decision.Reason,
			}, nil
		}
	}

	if err := taskRepo.Create(ctx, task); err != nil {
		return ActionResult{
			Success: false,
			Message: fmt.Sprintf("Failed to create task: %v", err),
		}, err
	}

	return ActionResult{
		Success: true,
		Message: fmt.Sprintf("Task created successfully. ID: %s", task.ID),
		Data:    task,
	}, nil
}

// executeCancelTask handles the cancel_task action.
func executeCancelTask(ctx context.Context, action Action, taskRepo persistence.TaskRepository) (ActionResult, error) {
	if action.TaskID == "" {
		return ActionResult{
			Success: false,
			Message: "Task ID is required for cancel_task action.",
		}, fmt.Errorf("task_id is required for cancel_task")
	}

	task, err := taskRepo.Get(ctx, action.TaskID)
	if err != nil {
		return ActionResult{
			Success: false,
			Message: fmt.Sprintf("Failed to get task: %v", err),
		}, err
	}

	if task == nil {
		return ActionResult{
			Success: false,
			Message: fmt.Sprintf("Task %s not found.", action.TaskID),
		}, fmt.Errorf("task not found: %s", action.TaskID)
	}

	// Check if task can be cancelled
	if task.Status == persistence.TaskStatusCompleted || task.Status == persistence.TaskStatusFailed || task.Status == persistence.TaskStatusCancelled {
		return ActionResult{
			Success: false,
			Message: fmt.Sprintf("Task %s is already in terminal state: %s", task.ID, task.Status),
		}, fmt.Errorf("task is already in terminal state")
	}

	if err := taskRepo.UpdateStatus(ctx, action.TaskID, persistence.TaskStatusCancelled); err != nil {
		return ActionResult{
			Success: false,
			Message: fmt.Sprintf("Failed to cancel task: %v", err),
		}, err
	}

	return ActionResult{
		Success: true,
		Message: fmt.Sprintf("Task %s has been cancelled.", action.TaskID),
		Data:    action.TaskID,
	}, nil
}

// executeRetryTask handles the retry_task action.
func executeRetryTask(ctx context.Context, action Action, taskRepo persistence.TaskRepository) (ActionResult, error) {
	if action.TaskID == "" {
		return ActionResult{
			Success: false,
			Message: "Task ID is required for retry_task action.",
		}, fmt.Errorf("task_id is required for retry_task")
	}

	task, err := taskRepo.Get(ctx, action.TaskID)
	if err != nil {
		return ActionResult{
			Success: false,
			Message: fmt.Sprintf("Failed to get task: %v", err),
		}, err
	}

	if task == nil {
		return ActionResult{
			Success: false,
			Message: fmt.Sprintf("Task %s not found.", action.TaskID),
		}, fmt.Errorf("task not found: %s", action.TaskID)
	}

	// Check if task can be retried
	if task.Status != persistence.TaskStatusFailed {
		return ActionResult{
			Success: false,
			Message: fmt.Sprintf("Task %s cannot be retried. Current status: %s (must be FAILED)", task.ID, task.Status),
		}, fmt.Errorf("task is not in FAILED state")
	}

	// Reset task to queued
	task.Status = persistence.TaskStatusQueued
	task.Attempt = 0
	task.LastError = nil
	task.UpdatedAt = time.Now()

	if err := taskRepo.Update(ctx, task); err != nil {
		return ActionResult{
			Success: false,
			Message: fmt.Sprintf("Failed to retry task: %v", err),
		}, err
	}

	return ActionResult{
		Success: true,
		Message: fmt.Sprintf("Task %s has been queued for retry.", action.TaskID),
		Data:    task,
	}, nil
}

// executeGetStatus handles the get_status action.
func executeGetStatus(ctx context.Context, action Action, taskRepo persistence.TaskRepository, execRepo persistence.ExecutionRepository) (ActionResult, error) {
	if action.TaskID == "" {
		return ActionResult{
			Success: false,
			Message: "Task ID is required for get_status action.",
		}, fmt.Errorf("task_id is required for get_status")
	}

	task, err := taskRepo.Get(ctx, action.TaskID)
	if err != nil {
		return ActionResult{
			Success: false,
			Message: fmt.Sprintf("Failed to get task: %v", err),
		}, err
	}

	if task == nil {
		return ActionResult{
			Success: false,
			Message: fmt.Sprintf("Task %s not found.", action.TaskID),
		}, fmt.Errorf("task not found: %s", action.TaskID)
	}

	// Get the current execution if available
	var exec *persistence.Execution
	if execRepo != nil {
		exec, _ = execRepo.GetByTaskID(ctx, action.TaskID)
	}

	var summary strings.Builder
	fmt.Fprintf(&summary, "Task: %s\n", task.ID)
	fmt.Fprintf(&summary, "Project: %s\n", task.ProjectID)
	fmt.Fprintf(&summary, "Status: %s\n", task.Status)
	fmt.Fprintf(&summary, "Priority: %d\n", task.Priority)
	fmt.Fprintf(&summary, "Attempt: %d/%d\n", task.Attempt, task.MaxAttempts)

	// Show the task input prompt
	if len(task.Payload) > 0 {
		var payload struct {
			Context struct {
				Prompt string `json:"prompt"`
			} `json:"context"`
		}
		if json.Unmarshal(task.Payload, &payload) == nil && payload.Context.Prompt != "" {
			fmt.Fprintf(&summary, "\nInput prompt: %s\n", payload.Context.Prompt)
		}
	}

	if task.LastError != nil && *task.LastError != "" {
		fmt.Fprintf(&summary, "Last Error: %s\n", *task.LastError)
	}

	if exec != nil {
		fmt.Fprintf(&summary, "\nExecution: %s\n", exec.ID)
		fmt.Fprintf(&summary, "Execution Status: %s\n", exec.Status)
		if exec.ErrorMessage != nil && *exec.ErrorMessage != "" {
			fmt.Fprintf(&summary, "Error: %s\n", *exec.ErrorMessage)
		}
		// Show the agent's output message
		if len(exec.Result) > 0 {
			var result struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(exec.Result, &result) == nil && result.Message != "" {
				msg := result.Message
				if len(msg) > 500 {
					msg = msg[:500] + "... (truncated)"
				}
				fmt.Fprintf(&summary, "\nAgent output:\n%s\n", msg)
			}
		}
	}

	return ActionResult{
		Success: true,
		Message: summary.String(),
		Data:    task,
	}, nil
}
