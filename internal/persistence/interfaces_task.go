// Package persistence — task + execution domain interfaces.
//
// Task lifecycle (Task, LeaseOptions, Execution, ExecutionStepOutcome) plus per-task analytics (TaskLLMUsage, TaskJudgeVerdict, TaskPostMortem, TaskScratchpad, ExecutionHint).
// Split from interfaces.go on 2026-05-28 to keep each domain in
// its own file. Same package; no API change — pure file-org.
package persistence

import (
	"context"
	"time"
)

// TaskRepository defines the interface for task persistence operations.
// Implementations must ensure thread-safety and handle transactions appropriately.
type TaskRepository interface {
	// Ping checks if the database is reachable.
	Ping(ctx context.Context) error

	// Create inserts a new task into the database.
	Create(ctx context.Context, task *Task) error

	// Get retrieves a task by ID.
	Get(ctx context.Context, id string) (*Task, error)

	// GetByIdempotencyKey retrieves a task by project-scoped idempotency key.
	GetByIdempotencyKey(ctx context.Context, projectID, idempotencyKey string) (*Task, error)

	// Update modifies an existing task.
	Update(ctx context.Context, task *Task) error

	// Delete removes a task by ID.
	Delete(ctx context.Context, id string) error

	// List retrieves tasks based on filter criteria.
	List(ctx context.Context, filter TaskFilter) ([]*Task, error)

	// Count returns the total number of tasks matching the filter,
	// ignoring PageSize and Offset. Powers correct pagination Total
	// in the API: pre-fix the handlers returned len(currentPage),
	// so a paginated client could not tell when to stop fetching.
	Count(ctx context.Context, filter TaskFilter) (int64, error)

	// UpdateStatus atomically updates a task's status.
	// Returns ErrOptimisticLock if the task was modified concurrently.
	UpdateStatus(ctx context.Context, id string, status TaskStatus) error

	// TransitionToCancelled atomically marks a task CANCELLED only
	// when its current status is one that may be cancelled (QUEUED,
	// LEASED, RUNNING, PENDING). Returns (true, nil) when the row
	// transitioned, (false, nil) when no row matched (terminal task,
	// or task missing). Closes a TOCTOU window where the legacy
	// CancelTask handler did read-status → check → write-CANCELLED
	// in three round trips: a task that COMPLETED between the read
	// and write would have its terminal state silently overwritten.
	TransitionToCancelled(ctx context.Context, id string) (bool, error)

	// RequeueTerminalTask atomically prepares a finished task for
	// retry: bumps attempt + max_attempts and resets status to
	// QUEUED, but only when the current status is FAILED, CANCELLED,
	// or COMPLETED. Returns (true, nil) when requeued; (false, nil)
	// when the task is in-flight (caller treats as 409 Conflict
	// rather than corrupting active state). Replaces the prior
	// ReleaseLease(taskID, "") misuse on terminal rows, where
	// passing an empty leaseID could re-queue a task that should
	// have stayed terminal.
	RequeueTerminalTask(ctx context.Context, id string, attempt, maxAttempts int) (bool, error)

	// TransitionConditional atomically updates a task's status
	// only when it currently sits in one of `from`. Returns
	// (true, nil) when the row transitioned, (false, nil) when no
	// row matched (status drifted concurrently, task missing).
	// Optional opts mutate companion columns in the same UPDATE so
	// the row never disagrees with itself — closed_at/closed_by
	// land on a CLOSED transition, expected_by lands on
	// AWAITING_EXTERNAL, etc.
	//
	// Phase 23+ of the conversational task lifecycle. See
	// https://docs.vornik.io §3.1.
	TransitionConditional(
		ctx context.Context,
		id string,
		from []TaskStatus,
		to TaskStatus,
		opts TransitionOpts,
	) (bool, error)

	// LeaseTask attempts to atomically claim a task for execution.
	// Returns the leased task or ErrNoTasksAvailable if no tasks are available.
	LeaseTask(ctx context.Context, opts LeaseOptions) (*Task, error)

	// RenewLease extends the lease on a currently leased task.
	// Returns ErrLeaseNotFound if the task is not leased or lease_id doesn't match.
	RenewLease(ctx context.Context, taskID, leaseID string, extendBySeconds int) error

	// ReleaseLease releases a task lease, returning it to the queue.
	// Sets status to the provided newStatus (typically QUEUED or a terminal state).
	ReleaseLease(ctx context.Context, taskID, leaseID string, newStatus TaskStatus, opts ReleaseOptions) error

	// FindExpiredLeases finds tasks with expired leases for recovery.
	FindExpiredLeases(ctx context.Context, limit int) ([]*Task, error)

	// CountByStatus returns the count of tasks in each status for a project.
	CountByStatus(ctx context.Context, projectID string) (map[TaskStatus]int64, error)

	// CountRecentFailures returns the number of FAILED tasks in a project
	// within the rolling window starting at `since`. If errorClasses is
	// non-empty, only tasks whose last_error_class is one of those values
	// are counted; an empty slice counts every FAILED task regardless of
	// class. Powers the per-project circuit breaker — when this count
	// crosses a threshold the executor pauses autonomy on the project so
	// a stuck loop can't burn through the budget.
	CountRecentFailures(ctx context.Context, projectID string, errorClasses []string, since time.Time) (int, error)

	// GetChildren retrieves all child tasks of a parent task.
	GetChildren(ctx context.Context, parentTaskID string) ([]*Task, error)

	// CountChildrenForParents returns the direct-child count keyed by
	// parent task ID. Parents with zero children are absent from the
	// result map (not zero-filled). Powers the UI list's "Subtasks (N)"
	// pill without an N+1 GetChildren call per row.
	CountChildrenForParents(ctx context.Context, parentTaskIDs []string) (map[string]int, error)

	// GetDependencies retrieves tasks that must complete before a given task.
	GetDependencies(ctx context.Context, taskID string) ([]*Task, error)

	// GetDependents retrieves tasks waiting on a given task to complete.
	GetDependents(ctx context.Context, taskID string) ([]*Task, error)
}

// LeaseOptions configures task leasing behavior.
type LeaseOptions struct {
	// ProjectID restricts leasing to a specific project (optional).
	ProjectID string

	// LeaseHolder identifies the entity acquiring the lease.
	LeaseHolder string

	// LeaseDurationSeconds specifies how long the lease is valid.
	LeaseDurationSeconds int

	// PriorityFloor sets the minimum numeric priority value for tasks to consider.
	// Lower numeric priorities are more urgent; this legacy filter excludes
	// tasks with numerically smaller priorities when set above zero.
	PriorityFloor int

	// ProjectConcurrencyLimits maps project IDs to their max concurrent task
	// count. Projects at or above their limit are excluded from leasing.
	// Nil or empty means no per-project limit enforcement.
	ProjectConcurrencyLimits map[string]int

	// ProjectPriorities maps project IDs to their DefaultPriority. The
	// lease query uses these as the PRIMARY sort key (ahead of task-
	// level Priority and CreatedAt) so a high-priority project's queue
	// drains before a low-priority project gets a turn — strict
	// priority across projects, FIFO within. LLD §4.2.
	//
	// Projects absent from this map fall back to ProjectPriorityDefault
	// at the SQL level (COALESCE in the candidate ORDER BY). Empty map
	// = uniform priority across projects, equivalent to pre-2026.5.4
	// behaviour.
	ProjectPriorities map[string]int

	// ProjectPriorityDefault is the priority value applied to projects
	// missing from ProjectPriorities. Defaults to 50 (the project
	// schema's documented default) when zero. Higher numeric values
	// = lower priority.
	ProjectPriorityDefault int

	// ExcludedProjects is the list of project IDs whose tasks the
	// lease query must skip entirely. Used by the archived-project
	// hard-guard so queued work for an archived project doesn't
	// dispatch even if its tasks were created before the archive
	// flipped. Empty / nil disables the filter.
	ExcludedProjects []string
}

// ReleaseOptions configures task lease release behavior.
type ReleaseOptions struct {
	// Attempt is the current attempt number (incremented on retry).
	Attempt int

	// MaxAttempts is updated if non-zero.
	MaxAttempts int

	// Error is recorded for failed tasks.
	Error string

	// ErrorClass is the typed classification that accompanies Error.
	// See persistence.TaskFailureClass* constants. Empty preserves the
	// existing column when a caller hasn't (yet) been updated to
	// classify its failures — enables progressive rollout across
	// release paths (executor, scheduler recovery, autonomy guards).
	ErrorClass string
}

// ExecutionRepository defines the interface for execution persistence operations.
type ExecutionRepository interface {
	// Create inserts a new execution into the database.
	Create(ctx context.Context, execution *Execution) error

	// Get retrieves an execution by ID.
	Get(ctx context.Context, id string) (*Execution, error)

	// GetByTaskID retrieves the current (or most recent) execution for a task.
	GetByTaskID(ctx context.Context, taskID string) (*Execution, error)

	// GetByTaskIDs is the batch sibling of GetByTaskID. Returns a
	// map keyed by task_id; task IDs with no execution are absent
	// from the map (not zero-valued) so callers distinguish "no
	// execution" from "missing field". Used by the autonomy state-
	// context builder to replace N+1 GetByTaskID calls with one
	// round-trip.
	GetByTaskIDs(ctx context.Context, taskIDs []string) (map[string]*Execution, error)

	// Update modifies an existing execution.
	Update(ctx context.Context, execution *Execution) error

	// List retrieves executions based on filter criteria.
	List(ctx context.Context, filter ExecutionFilter) ([]*Execution, error)

	// Count returns the total number of executions matching the
	// filter, ignoring PageSize and Offset. Same pagination Total
	// motivation as TaskRepository.Count.
	Count(ctx context.Context, filter ExecutionFilter) (int64, error)

	// UpdateStatus atomically updates an execution's status.
	UpdateStatus(ctx context.Context, id string, status ExecutionStatus) error

	// SaveStateSnapshot updates the execution's state snapshot.
	SaveStateSnapshot(ctx context.Context, id string, snapshot []byte, currentStepID string, completedSteps []string) error

	// SetWorkflowSnapshot persists a JSON-marshaled workflow body
	// captured at execution start so the replay path uses the
	// snapshot rather than the live (possibly edited) workflow YAML.
	// Empty/zero-byte payload is a no-op — callers that detect a
	// snapshot already exists for an execution skip the write.
	// Eliminates WORKFLOW_DRIFT failures by pinning the step graph
	// to the version that started the execution.
	SetWorkflowSnapshot(ctx context.Context, id string, snapshot []byte) error

	// GetWorkflowSnapshot returns the snapshot bytes captured at
	// execution start, or nil when none was captured (legacy
	// executions, or fresh ones whose snapshot couldn't be persisted).
	// Used by the replay/resume path to reconstruct the workflow as
	// it was when the execution began.
	GetWorkflowSnapshot(ctx context.Context, id string) ([]byte, error)

	// RecordCompletion marks an execution as completed with the given result.
	RecordCompletion(ctx context.Context, id string, result []byte) error

	// RecordFailure marks an execution as failed with error details.
	RecordFailure(ctx context.Context, id string, errorMessage, errorCode string) error

	// SupersedeNonTerminalForTask sweeps every non-terminal execution
	// for the given task into a terminal state — used when the task
	// itself transitions to COMPLETED/FAILED/CANCELLED and any
	// orphan PAUSED/RUNNING/PENDING execution rows would otherwise
	// linger forever.
	//
	// Background: the adaptive-route flow creates two executions per
	// task — the first reaches PAUSED with pause_reason=
	// awaiting_children and never gets finalised, even after the
	// parent task completes via the second execution. Over time
	// these orphans accumulate (29 rows on one project, blocking
	// config reload).
	//
	// Marks affected rows status=CANCELLED, completed_at=NOW(),
	// error_code='superseded_by_terminal_task'. Returns the count
	// of rows updated. Idempotent — a second call after all rows
	// are terminal returns 0 without error.
	SupersedeNonTerminalForTask(ctx context.Context, taskID string) (int64, error)

	// SupersedeOrphanPausedExecutions is the GLOBAL backstop for the
	// orphan-PAUSED leak: it finalizes EVERY PAUSED execution whose
	// parent task has already reached a terminal status, regardless of
	// which terminal code path created it. The per-task cascade
	// (SupersedeNonTerminalForTask, called from the executor's common
	// terminal paths) handles the immediate case, but it only fires on
	// the paths that remember to call it — CLOSED transitions and some
	// cancel paths slip through. This sweep, run periodically by the
	// watchdog, catches all of them AND cleans pre-existing orphans
	// accumulated before the per-task cascade existed.
	//
	// Marks rows status=CANCELLED, completed_at=NOW(),
	// error_code='superseded_orphan_paused'. A legitimately-paused
	// execution whose task is still non-terminal is never touched.
	// Returns the count finalized. Idempotent.
	SupersedeOrphanPausedExecutions(ctx context.Context) (int64, error)

	// CountByStatus returns the count of executions in each status for a project.
	CountByStatus(ctx context.Context, projectID string) (map[ExecutionStatus]int64, error)

	// GetRoleQuality returns per-role output quality stats for a project
	// within the given time window. Keyed by execution_step_outcomes.role.
	// Roles with no terminal, non-cancelled outcomes in the window are
	// absent from the map — callers treat "missing" as "no data."
	GetRoleQuality(ctx context.Context, projectID string, since time.Duration) (map[string]*RoleQuality, error)
}

// TaskScratchpadRepository persists the lead's running summary
// for one task. One row per task. See LLD §4.2.
type TaskScratchpadRepository interface {
	// Get returns the scratchpad for a task, or nil + nil error
	// when the task has none yet.
	Get(ctx context.Context, taskID string) (*TaskScratchpad, error)

	// Upsert writes or replaces the scratchpad row.
	Upsert(ctx context.Context, scratch *TaskScratchpad) error
}

// TaskLLMUsageRepository persists per-step LLM token usage and cost. Writes
// happen at agent step completion; reads power the UI spend panel and
// per-project budget enforcement.
type TaskLLMUsageRepository interface {
	// Record inserts one usage row.
	Record(ctx context.Context, u *TaskLLMUsage) error

	// Upsert inserts a usage row OR overwrites an existing row
	// with the same ID. Used by the per-iteration streaming path
	// so cancelled tasks still carry the latest cumulative cost
	// summary in the DB even though step finalize never ran.
	// Caller supplies a deterministic ID so the same step's
	// streaming updates collapse to a single row.
	Upsert(ctx context.Context, u *TaskLLMUsage) error

	// List returns usage rows matching the filter, newest first.
	List(ctx context.Context, filter TaskLLMUsageFilter) ([]*TaskLLMUsage, error)

	// SumCostByProject returns total cost for a project within an optional
	// time window. A zero time is treated as unbounded on that side. Used
	// by budget enforcement (daily/monthly ceilings).
	SumCostByProject(ctx context.Context, projectID string, since, until time.Time) (float64, error)

	// SumCost returns total LLM spend across all projects within the
	// window. Powers the dashboard's headline "spend in last 24h" card.
	SumCost(ctx context.Context, since, until time.Time) (float64, error)

	// SumCostByAPIKey returns total LLM spend attributable to a
	// companion API key, summed over every task that key created
	// (joined via tasks.payload->'companion'->>'api_key_id'). A zero
	// time is unbounded on that side. Powers the per-key budget cap
	// enforced in the companion delegate handler. See
	// https://docs.vornik.io finding #2 / mitigation §7.2.
	SumCostByAPIKey(ctx context.Context, apiKeyID string, since, until time.Time) (float64, error)

	// MeanCostByWorkflow returns the historical average LLM cost per
	// task for a (project, workflow) pair within an optional window,
	// plus the number of tasks the average was computed over. Powers
	// the companion catalog()/delegate() cost_estimate (LLD-21 §
	// "delegate returns cost_estimate"; drift-mitigation §8.2). A
	// sampleCount of 0 means "no prior runs to estimate from" — the
	// caller surfaces a null estimate rather than a misleading $0.
	// Joins task_llm_usage → tasks on workflow_id; no migration.
	MeanCostByWorkflow(ctx context.Context, projectID, workflowID string, since, until time.Time) (mean float64, sampleCount int, err error)

	// AggregateByRoleModel returns spend, token counts, and step counts
	// grouped by (role, model) within the window, ordered by total cost
	// descending. Powers the dashboard leaderboard.
	// projectID is optional — empty filters to all projects so the
	// dashboard's global leaderboard keeps working unchanged. The
	// /ui/spend deep-dive page passes the active project filter so
	// the per-role table reflects the same scope as the headline cards.
	AggregateByRoleModel(ctx context.Context, since, until time.Time, limit int, projectID string) ([]RoleModelSpend, error)

	// AggregateByProject groups spend by project_id within the window,
	// ordered by total cost descending. Powers the /ui/spend deep-dive
	// "top projects" table — answers "which project is consuming the
	// most LLM budget."
	AggregateByProject(ctx context.Context, since, until time.Time, limit int) ([]ProjectSpend, error)

	// AggregateBySource groups spend by the `source` column
	// (workflow_step vs dispatcher) within the window. Critical for
	// the deep-dive: dispatcher overhead (every chat round-trip) is
	// often invisible on per-task views, but it can dominate total
	// spend on heavy chat-driven deployments.
	// projectID is optional — empty filters to all projects (the
	// global behaviour the main dashboard relies on). The /ui/spend
	// deep-dive passes the active project filter so the source split
	// and the headline cards derived from it scope to that project.
	AggregateBySource(ctx context.Context, since, until time.Time, projectID string) ([]SourceSpend, error)

	// TimeSeriesByDay returns daily spend buckets within the window.
	// projectID is optional — empty filters to all projects. Used by
	// the deep-dive's time-series chart so operators see whether a
	// cost spike happened on a specific day.
	TimeSeriesByDay(ctx context.Context, since, until time.Time, projectID string) ([]DailySpend, error)

	// TopTasks returns the most expensive tasks within the window,
	// joined with their token + iteration shape. projectID is
	// optional. Operators drill from this list into a single task's
	// per-step breakdown via TaskCostBreakdown.
	TopTasks(ctx context.Context, since, until time.Time, limit int, projectID string) ([]TaskSpend, error)

	// TaskCostBreakdown returns per-step cost rows for a single task,
	// in execution order (recorded_at ASC). The drill-down endpoint
	// behind the per-task spend panel — one row per step shows
	// role, model, tokens, iterations, $ so the operator can see
	// exactly which step ran the bill up.
	TaskCostBreakdown(ctx context.Context, taskID string) ([]StepSpend, error)
}

// ExecutionStepOutcomeRepository persists per-step outcome classifications.
// Writes come from the executor at two points:
//   - Record: when a step completes, with Outcome="pending_validation".
//   - Finalize / FinalizePending: when the downstream consumer knows
//     whether the output was usable (Outcome="ok" or one of the
//     error classes).
//
// SweepPending runs at execution-terminal time to finalize anything the
// consumer chain never finalized (e.g. the last step has no consumer).
type ExecutionStepOutcomeRepository interface {
	// Record inserts one outcome row. Used on step completion.
	Record(ctx context.Context, o *ExecutionStepOutcome) error

	// Finalize sets the outcome and related fields by the row's primary
	// key. Used when the caller has a specific row ID.
	Finalize(ctx context.Context, id, outcome, errorClass, errorDetail string, attributedToStepID *string) error

	// FinalizePending finds the most recent pending_validation row for
	// (executionID, stepID) and finalizes it. Used by downstream consumers
	// that know the producer step by ID but not by outcome row ID.
	// Returns the finalized row's (role, model) so callers can emit
	// per-outcome metrics without a separate lookup. Returns ErrNotFound
	// if no pending row exists for that step — callers treat that as
	// non-fatal (pre-outcome-table executions, or a step that already
	// got finalized for another reason).
	FinalizePending(ctx context.Context, executionID, stepID, outcome, errorClass, errorDetail string, attributedToStepID *string) (role, model string, err error)

	// SweepPending finalizes all remaining pending_validation rows for
	// an execution. Used at execution-terminal time: on COMPLETED flip
	// them to "ok", on FAILED flip them to the fallback outcome (usually
	// "failed"). Returns the (role, model) of each swept row so callers
	// can emit per-outcome metrics.
	SweepPending(ctx context.Context, executionID, fallbackOutcome string) ([]SweepResult, error)

	// List returns rows matching the filter, newest first. Powers
	// investigation UIs and the per-execution outcome panel.
	List(ctx context.Context, filter ExecutionStepOutcomeFilter) ([]*ExecutionStepOutcome, error)

	// SupersedeAfter marks every outcome row for the given execution
	// whose recorded_at is strictly after `cutoff` as outcome=
	// "superseded" with finalized_at=NOW(). Used by the
	// retry-from-step path so the dashboard's quality stats don't
	// double-count the original run alongside the retry's fresh
	// outcomes — the audit trail is preserved (rows aren't deleted),
	// just relabelled. Returns the number of rows updated.
	SupersedeAfter(ctx context.Context, executionID string, cutoff time.Time) (int64, error)

	// CountByRoleModelOutcome aggregates outcome rows by (role, model)
	// for one outcome literal within the window. projectID is
	// optional — empty filters to all projects (the dashboard's
	// global behaviour). Drift queries pass the active project so
	// the per-project page only counts that project's successes.
	// Empty role/model are filtered out at the SQL layer — gate
	// steps (role="gate") are noise for spend-quality dashboards.
	CountByRoleModelOutcome(ctx context.Context, outcome string, since, until time.Time, projectID string) ([]RoleModelOutcomeCount, error)
}

// RoleModelOutcomeCount is one row of CountByRoleModelOutcome.
type RoleModelOutcomeCount struct {
	Role  string
	Model string
	Count int64
}

// SweepResult reports the role+model+step_id of one row finalized by
// SweepPending. Callers (executor metrics) use these to emit per-outcome
// Prometheus events that mirror the DB state.
type SweepResult struct {
	StepID string
	Role   string
	Model  string
}

// RoleModelSpend is one row of the dashboard leaderboard.
type RoleModelSpend struct {
	Role             string
	Model            string
	CostUSD          float64
	StepCount        int
	PromptTokens     int64
	CompletionTokens int64
	// CacheCreationTokens / CacheReadTokens carry the LLM-caching
	// observability fields aggregated across rows. Zero when no
	// caching-capable provider produced traffic in the window.
	CacheCreationTokens int64
	CacheReadTokens     int64
}

// ProjectSpend is one row of the per-project spend table.
type ProjectSpend struct {
	ProjectID        string
	CostUSD          float64
	StepCount        int
	PromptTokens     int64
	CompletionTokens int64
	// TaskCount is the count of distinct task_ids contributing to
	// this project's spend. Lets the dashboard show "$X over N
	// tasks" so a project with one expensive runaway task is
	// distinguishable from one with many small ones.
	TaskCount int
	// CacheCreationTokens / CacheReadTokens — aggregated cache
	// observability per project. Surface on the dashboard so
	// operators see which projects benefit most from prompt caching.
	CacheCreationTokens int64
	CacheReadTokens     int64
}

// SourceSpend breaks down spend by the `source` column —
// "workflow_step" for executor-driven steps vs "dispatcher" for
// in-bot chat round-trips. Surfacing this attribution answers
// "is the dispatcher itself a major cost driver?"
type SourceSpend struct {
	Source           string
	CostUSD          float64
	CallCount        int
	PromptTokens     int64
	CompletionTokens int64
	// CacheCreationTokens / CacheReadTokens — aggregated cache
	// observability per source. external_api callers (HA, OpenWebUI)
	// frequently benefit from caching; workflow_step less so.
	CacheCreationTokens int64
	CacheReadTokens     int64
}

// DailySpend is one bucket on the time-series chart. Day is the
// UTC midnight start of the bucket.
type DailySpend struct {
	Day     time.Time
	CostUSD float64
	// CallCount is the number of LLM calls (rows in task_llm_usage)
	// that landed in the bucket — operators use this alongside cost
	// to see whether a spike was "more calls" or "same calls but
	// pricier".
	CallCount int
}

// TaskSpend is one row of the top-tasks table on the deep-dive
// dashboard. Joins the usage table with the tasks table to get
// status + creation source so the row tells the full story.
type TaskSpend struct {
	TaskID           string
	ProjectID        string
	Status           string
	CreationSource   string
	CostUSD          float64
	PromptTokens     int64
	CompletionTokens int64
	StepCount        int
	Iterations       int
	// FirstStepAt + LastStepAt let the dashboard show wall-clock
	// duration. Useful when a task ran for many hours of LLM time
	// — high cost over long duration is different from high cost
	// over a short window.
	FirstStepAt time.Time
	LastStepAt  time.Time
}

// StepSpend is one per-step row in a single task's breakdown.
// Ordered by recorded_at so the row order matches the workflow
// execution order.
type StepSpend struct {
	StepID           string
	Role             string
	Model            string
	PromptTokens     int64
	CompletionTokens int64
	Iterations       int
	CostUSD          float64
	RecordedAt       time.Time
	// Source distinguishes workflow_step entries (one per agent
	// step) from dispatcher entries (one per chat round-trip);
	// the same task can carry both when an agent delegates from
	// a chat session.
	Source string
}

// TaskJudgeVerdictRepository persists Phase-3 LLM-as-judge
// verdicts. One verdict per task; the judge runs async after a
// task reaches its terminal status. The ListByTask form is what
// the task-detail UI uses to surface the verdict; ListRecent
// drives the per-role rollup tile.
type TaskJudgeVerdictRepository interface {
	// Record inserts one verdict row. Returns ErrDuplicateKey if
	// a verdict already exists for this task — the judge is
	// idempotent per task and re-running shouldn't quietly
	// overwrite the prior verdict.
	Record(ctx context.Context, v *TaskJudgeVerdict) error
	// GetByTask returns the verdict for a single task, or
	// ErrNotFound if none has been recorded yet.
	GetByTask(ctx context.Context, taskID string) (*TaskJudgeVerdict, error)
	// ListRecent returns recent verdicts for a project (or all
	// projects when projectID is empty), newest first, capped at
	// limit. Powers the per-role hallucination-score rollup.
	ListRecent(ctx context.Context, projectID string, limit int) ([]*TaskJudgeVerdict, error)
}

// TaskPostMortemRepository persists LLM-generated failure
// explainers. One row per task — Record overwrites only when
// the caller passes ForceRefresh; the default Get path returns
// the cached row so a re-render of the failed-task page
// doesn't burn another LLM call.
type TaskPostMortemRepository interface {
	// Record inserts or replaces the post-mortem for a task.
	// PRIMARY KEY on task_id makes the upsert atomic; the
	// caller doesn't need to pre-check for an existing row.
	Record(ctx context.Context, pm *TaskPostMortem) error
	// Get returns the cached post-mortem for a task, or
	// ErrNotFound if none has been generated.
	Get(ctx context.Context, taskID string) (*TaskPostMortem, error)
}

type ExecutionHintRepository interface {
	// Insert writes one hint row. Caller stamps the ID via
	// persistence.GenerateID("hint"); applied_at is left nil
	// (the row is created pending).
	//
	// Exactly one of h.TaskID / h.ExecutionID must be set:
	//   - ExecutionID-scoped: consumed at the first step boundary
	//     of THIS execution; orphaned if the task is requeued.
	//   - TaskID-scoped (2026-05-26): consumed at the first step
	//     boundary of ANY execution for the task, including
	//     post-retry executions. Use when steering a research /
	//     plan task that may bounce through recover loops.
	Insert(ctx context.Context, h *ExecutionHint) error
	// ConsumePending returns + atomically marks-applied all
	// pending hints whose scope matches (taskID, executionID, stepID).
	// Atomic so two concurrent step starts can't both consume the
	// same hint.
	//
	// stepID="" matches hints with step_id IS NULL OR step_id =
	// "" (the "next step, any" target). stepID set matches both
	// the targeted hints and the no-target hints.
	//
	// Scope precedence:
	//   - hints with TaskID = taskID AND ExecutionID IS NULL  → task-scoped
	//   - hints with ExecutionID = executionID                → execution-scoped
	// Both sets are returned together so the caller sees one stream.
	// taskID="" disables the task-scoped predicate (legacy callers).
	ConsumePending(ctx context.Context, taskID, executionID, stepID string) ([]*ExecutionHint, error)
	// ListByExecution returns all hints (applied + pending)
	// newest first. Powers the live-view "hint history" pane.
	ListByExecution(ctx context.Context, executionID string) ([]*ExecutionHint, error)
	// ListForExecution returns the hints relevant to ONE execution's
	// live view: the execution-scoped hints (execution_id = $1) AND
	// the task-scoped hints (task_id = $2 AND execution_id IS NULL)
	// that carry across retries. Newest-first. ListByExecution alone
	// misses task-scoped hints, so the live page's hint history under-
	// reported steering messages issued at the task level (2026-05-29
	// LLD-drift audit §8.6). taskID="" degrades to execution-only.
	ListForExecution(ctx context.Context, executionID, taskID string) ([]*ExecutionHint, error)
	// ListPendingForTask returns all unconsumed task-scoped hints
	// for the given task (ExecutionID IS NULL). Used by the API to
	// show operators which task-level hints are still queued for a
	// future execution.
	ListPendingForTask(ctx context.Context, taskID string) ([]*ExecutionHint, error)
	// ListByTask returns ALL hints scoped to a task (any state:
	// pending or applied), ordered oldest-first so the UI can
	// interleave them with task_messages by CreatedAt. Only
	// task-scoped hints (execution_id IS NULL) — execution-scoped
	// hints stay on the live page where their lifetime makes
	// sense (consumed within seconds of the next step starting).
	// Added 2026-05-26 for the unified-timeline refactor.
	ListByTask(ctx context.Context, taskID string) ([]*ExecutionHint, error)
}
