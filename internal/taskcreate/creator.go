// Package taskcreate provides the shared task-creation core used by
// every operator surface (REST API, web UI form, future surfaces).
//
// Why a dedicated package: before this lived in two near-identical
// copies — one in internal/api/handlers.go's CreateTask and one in
// internal/ui/task_actions.go's ProjectCreateTask — and they had
// already drifted on rate-limit ordering, budget-check semantics,
// and queue enqueue. The drift was discovered after a real-world
// E2E test had to use curl because the UI form was missing —
// adding a second surface would have widened the divergence.
//
// Surface: callers build a Params struct, call Create(ctx, params),
// and get back the persisted task plus a structured error. The
// error type distinguishes validation, rate-limit, budget, and
// internal failures so each surface can map to its idiomatic
// response (HTTP 400/429/500 for REST, sticky-form re-render for UI).
package taskcreate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/budget"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/queue"
	"vornik.io/vornik/internal/ratelimit"
	"vornik.io/vornik/internal/registry"
)

// Reason is the typed error category Create() returns. Callers use
// it to pick the right HTTP status or UI re-render branch without
// string-matching on the message.
type Reason string

const (
	ReasonValidation       Reason = "VALIDATION_ERROR"
	ReasonProjectNotFound  Reason = "PROJECT_NOT_FOUND"
	ReasonWorkflowNotFound Reason = "WORKFLOW_NOT_FOUND"
	ReasonWorkflowIncompat Reason = "WORKFLOW_INCOMPATIBLE"
	ReasonRateLimited      Reason = "RATE_LIMITED"
	ReasonBudgetExceeded   Reason = "BUDGET_EXCEEDED"
	ReasonInternal         Reason = "INTERNAL_ERROR"
)

// Error wraps a Reason with a human-readable message. Callers
// branch on Reason for status mapping; the Message goes verbatim
// to the operator (HTTP body, sticky-form banner, telegram
// dispatcher reply).
type Error struct {
	Reason  Reason
	Message string
	// Cause is the underlying error when one exists (e.g.
	// taskRepo.Create returning a DB error). nil for purely
	// declarative errors (missing prompt, unknown workflow).
	Cause error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Reason, e.Message, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Reason, e.Message)
}

func (e *Error) Unwrap() error { return e.Cause }

// AsError returns err as *Error when it is one, or nil otherwise.
// Lets callers do `if cerr := taskcreate.AsError(err); cerr != nil`
// without an explicit errors.As ceremony at every call site.
func AsError(err error) *Error {
	var ce *Error
	if errors.As(err, &ce) {
		return ce
	}
	return nil
}

// Params describes one task-creation request. The caller fills
// these from whatever surface they implement (JSON body, HTML
// form, autonomy LLM output) and Create() normalises + validates.
type Params struct {
	// ProjectID is required. The Creator looks up the project in
	// the registry to derive defaults (workflow, priority) and
	// enforce rate-limit / budget caps.
	ProjectID string
	// TaskType is required; this is what the workflow's agent
	// runtime reads to specialise behaviour (e.g. "research" vs
	// "feature").
	TaskType string
	// Prompt is the human-readable description of the task. Optional
	// at this layer (some autonomy flows don't supply one), but the
	// UI form always requires it via its own validation. When set,
	// it's placed at context.prompt in the persisted task payload.
	Prompt string
	// Priority overrides the project's default. Zero means "use
	// project default"; clamped to 0..100 by the caller.
	Priority int
	// WorkflowID overrides the project's defaultWorkflowId. Empty
	// means "use project default". Must reference an existing
	// workflow that the project's swarm can satisfy.
	WorkflowID string
	// IdempotencyKey, when present, makes Create() return the
	// existing task instead of creating a new one. Empty disables.
	IdempotencyKey string
	// CreationSource lets the caller distinguish where the task
	// originated (user-driven via API/UI, webhook, autonomy).
	// Defaults to TaskCreationSourceUser when zero.
	CreationSource persistence.TaskCreationSource
	// ExtraContext is merged into the payload's `context` object
	// alongside `prompt`. Optional. Lets the webhook handler push
	// payload metadata without subclassing Params.
	ExtraContext map[string]any
	// RawContext is the verbatim json.RawMessage the REST API
	// callers pass in CreateTaskRequest.Context. When set, the
	// final payload's `context` field is this exact byte slice
	// rather than the prompt+ExtraContext merge — preserves the
	// API contract where callers ship arbitrary nested JSON. The
	// UI form leaves this empty and uses Prompt instead.
	RawContext json.RawMessage
}

// Creator bundles the dependencies the shared core needs. Both
// the REST API server and the UI server build one per process and
// keep it around for the life of the daemon.
type Creator struct {
	taskRepo        persistence.TaskRepository
	queue           *queue.Queue
	rateLimiter     ratelimit.ProjectLimiter
	projectRegistry *registry.Registry
	llmUsageRepo    persistence.TaskLLMUsageRepository
	reservRepo      persistence.BudgetReservationRepository
	budgetNotifier  budget.Notifier
	logger          zerolog.Logger
	now             func() time.Time
}

// Option configures a Creator.
type Option func(*Creator)

// WithTaskRepository wires the persistence backend. Required —
// Create() returns ReasonInternal when nil.
func WithTaskRepository(r persistence.TaskRepository) Option {
	return func(c *Creator) { c.taskRepo = r }
}

// WithQueue wires the queue. Optional — without one, the task is
// still persisted (the scheduler's poll loop picks it up), the
// only thing missed is the immediate enqueue-counter metric.
func WithQueue(q *queue.Queue) Option {
	return func(c *Creator) { c.queue = q }
}

// WithRateLimiter wires the per-project task-creation rate limiter.
// Optional — without one, every call proceeds (legacy behaviour).
func WithRateLimiter(l ratelimit.ProjectLimiter) Option {
	return func(c *Creator) { c.rateLimiter = l }
}

// WithProjectRegistry wires the registry. Required for project
// lookup; without one the Creator falls back to caller-supplied
// values and skips compatibility checks.
func WithProjectRegistry(r *registry.Registry) Option {
	return func(c *Creator) { c.projectRegistry = r }
}

// WithLLMUsageRepository wires the spend repo used by the budget
// gate. Optional — without one the budget gate is skipped.
func WithLLMUsageRepository(r persistence.TaskLLMUsageRepository) Option {
	return func(c *Creator) { c.llmUsageRepo = r }
}

// WithBudgetReservationRepository wires the reservation ledger so the
// creator atomically reserves hard-cap headroom for the task before
// inserting it (trading-hardening §1). Optional — without one the creator
// falls back to the read-only budget Check (best-effort, TOCTOU-prone).
func WithBudgetReservationRepository(r persistence.BudgetReservationRepository) Option {
	return func(c *Creator) { c.reservRepo = r }
}

// WithBudgetNotifier wires the breach-notification side channel
// (telegram alert today). Optional.
func WithBudgetNotifier(n budget.Notifier) Option {
	return func(c *Creator) { c.budgetNotifier = n }
}

// WithLogger sets the logger.
func WithLogger(l zerolog.Logger) Option {
	return func(c *Creator) { c.logger = l }
}

// WithNowFunc overrides the time source. Test-only convenience.
func WithNowFunc(f func() time.Time) Option {
	return func(c *Creator) { c.now = f }
}

// New builds a Creator from the given options.
func New(opts ...Option) *Creator {
	c := &Creator{
		logger: zerolog.Nop(),
		now:    time.Now,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Create runs the full task-creation lifecycle for a single
// request:
//
//  1. Validate ProjectID + TaskType (and look up the project).
//  2. Resolve the workflow (Params.WorkflowID falls back to the
//     project's DefaultWorkflowID) and confirm the project's
//     swarm can run it.
//  3. Honour an existing idempotency key (return the prior task).
//  4. Run the rate-limit gate (returns ReasonRateLimited on
//     block). Skip when no limiter is wired.
//  5. Run the budget gate (returns ReasonBudgetExceeded on hard
//     breach; soft breach is logged + notified but allowed).
//  6. Build + persist the task. Enqueue when a queue is wired.
//  7. Record the rate-limiter sample so the next call sees it.
//
// On success Create returns the persisted *persistence.Task. On
// failure it returns nil + an *Error whose Reason tells the
// caller which step failed.
func (c *Creator) Create(ctx context.Context, p Params) (*persistence.Task, error) {
	if c == nil || c.taskRepo == nil {
		return nil, &Error{Reason: ReasonInternal, Message: "task repository not configured"}
	}
	if p.ProjectID == "" {
		return nil, &Error{Reason: ReasonValidation, Message: "projectId is required"}
	}
	if p.TaskType == "" {
		return nil, &Error{Reason: ReasonValidation, Message: "taskType is required"}
	}

	var project *registry.Project
	if c.projectRegistry != nil {
		project = c.projectRegistry.GetProject(p.ProjectID)
		if project == nil {
			return nil, &Error{Reason: ReasonProjectNotFound, Message: "project not found: " + p.ProjectID}
		}
	}

	// Resolve workflow + verify compatibility.
	workflowID := p.WorkflowID
	if workflowID == "" && project != nil {
		workflowID = project.DefaultWorkflowID
	}
	if workflowID != "" && c.projectRegistry != nil {
		wf := c.projectRegistry.GetWorkflow(workflowID)
		if wf == nil {
			return nil, &Error{
				Reason:  ReasonWorkflowNotFound,
				Message: "workflow not found: " + workflowID,
			}
		}
		if project != nil {
			if missing := missingRoles(wf, c.projectRegistry.GetSwarm(project.SwarmID)); len(missing) > 0 {
				return nil, &Error{
					Reason: ReasonWorkflowIncompat,
					Message: fmt.Sprintf(
						"workflow %q cannot run on swarm %q (missing role(s): %v)",
						workflowID, project.SwarmID, missing,
					),
				}
			}
		}
	}

	// Resolve priority.
	priority := p.Priority
	if priority == 0 && project != nil {
		priority = project.DefaultPriority
	}
	if priority == 0 {
		priority = 50 // legacy default — matches api.CreateTask
	}

	// Idempotency — return the prior row instead of creating a dup.
	idempotencyKey := p.IdempotencyKey
	if idempotencyKey != "" {
		existing, err := c.taskRepo.GetByIdempotencyKey(ctx, p.ProjectID, idempotencyKey)
		if err == nil && existing != nil {
			return existing, nil
		}
		if err != nil && !errors.Is(err, persistence.ErrNotFound) {
			c.logger.Error().Err(err).Str("project_id", p.ProjectID).Msg("taskcreate: idempotency lookup failed")
			return nil, &Error{Reason: ReasonInternal, Message: "idempotency lookup failed", Cause: err}
		}
	}

	now := c.now()

	// Rate-limit gate.
	if c.rateLimiter != nil && project != nil {
		if d := c.rateLimiter.Check(project, now); d.Blocked {
			return nil, &Error{Reason: ReasonRateLimited, Message: d.Reason}
		}
	}

	// Budget gate. Soft breaches log + notify but allow.
	if c.llmUsageRepo != nil && project != nil {
		decision, berr := budget.Check(ctx, c.llmUsageRepo, project, now.UTC())
		if berr != nil {
			c.logger.Warn().Err(berr).Str("project_id", p.ProjectID).Msg("taskcreate: budget check failed — proceeding")
		} else if decision.Blocked {
			if c.budgetNotifier != nil {
				period, level := decision.Period()
				c.budgetNotifier.NotifyBudgetBreach(ctx, p.ProjectID, level, period, decision)
			}
			return nil, &Error{Reason: ReasonBudgetExceeded, Message: decision.Reason}
		} else if decision.SoftBreached {
			c.logger.Warn().
				Str("project_id", p.ProjectID).
				Str("reason", decision.Reason).
				Msg("taskcreate: proceeding despite soft budget breach")
			if c.budgetNotifier != nil {
				period, level := decision.Period()
				c.budgetNotifier.NotifyBudgetBreach(ctx, p.ProjectID, level, period, decision)
			}
		}
	}

	// Build payload. The agent runtime reads context.prompt — see
	// the comment on api.CreateTaskRequest.Context for history.
	payload, err := buildPayload(p)
	if err != nil {
		return nil, &Error{Reason: ReasonInternal, Message: "failed to marshal task payload", Cause: err}
	}

	creationSource := p.CreationSource
	if creationSource == "" {
		creationSource = persistence.TaskCreationSourceUser
	}

	task := &persistence.Task{
		ID:             persistence.GenerateID("task"),
		ProjectID:      p.ProjectID,
		WorkflowID:     ptrIfNonEmpty(workflowID),
		IdempotencyKey: ptrIfNonEmpty(idempotencyKey),
		CreationSource: creationSource,
		Status:         persistence.TaskStatusQueued,
		Priority:       priority,
		Payload:        payload,
		Attempt:        1,
		MaxAttempts:    3,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	// Atomic hard-cap reservation (trading-hardening §1): claim this task's
	// estimated spend against the cap before inserting, so N concurrent
	// admissions can't all pass the read-only Check above and overshoot.
	// FAIL OPEN on error — a reservation-ledger problem must never block
	// legitimate work; the committed-spend Check is the backstop.
	if c.reservRepo != nil && c.llmUsageRepo != nil && project != nil {
		decision, rerr := budget.Reserve(ctx, c.reservRepo, c.llmUsageRepo, project, task.ID, now.UTC())
		if rerr != nil {
			c.logger.Warn().Err(rerr).Str("project_id", p.ProjectID).Msg("taskcreate: budget reserve failed — proceeding")
		} else if decision.Blocked {
			if c.budgetNotifier != nil {
				period, level := decision.Period()
				c.budgetNotifier.NotifyBudgetBreach(ctx, p.ProjectID, level, period, decision)
			}
			return nil, &Error{Reason: ReasonBudgetExceeded, Message: decision.Reason}
		}
	}

	if err := c.taskRepo.Create(ctx, task); err != nil {
		// Lost-race idempotency recovery: a parallel call may have
		// just inserted the same key. Return its task.
		if errors.Is(err, persistence.ErrDuplicateKey) && idempotencyKey != "" {
			existing, getErr := c.taskRepo.GetByIdempotencyKey(ctx, p.ProjectID, idempotencyKey)
			if getErr == nil && existing != nil {
				return existing, nil
			}
		}
		c.logger.Error().Err(err).Str("project_id", p.ProjectID).Msg("taskcreate: persistence Create failed")
		return nil, &Error{Reason: ReasonInternal, Message: "failed to persist task", Cause: err}
	}

	if c.queue != nil {
		if err := c.queue.Enqueue(task.ID, p.ProjectID, priority); err != nil {
			c.logger.Error().Err(err).Str("project_id", p.ProjectID).Msg("taskcreate: queue enqueue failed")
			return nil, &Error{Reason: ReasonInternal, Message: "failed to enqueue task", Cause: err}
		}
	}
	if c.rateLimiter != nil {
		c.rateLimiter.Record(p.ProjectID, now)
	}

	return task, nil
}

// buildPayload assembles the JSON payload persisted on the task
// row. Mirrors api.CreateTaskRequest's shape (taskType, priority,
// workflowId, idempotencyKey, context) so legacy consumers — the
// agent runtime's extractPrompt, the post-mortem rendering — keep
// reading the same fields.
//
// When p.RawContext is set, that exact byte slice becomes the
// payload's `context` field — preserves the REST API's free-form
// shape where callers ship arbitrary nested JSON. Otherwise the
// helper merges p.ExtraContext + p.Prompt into a structured
// context object (the UI form path).
func buildPayload(p Params) ([]byte, error) {
	out := map[string]any{
		"taskType": p.TaskType,
	}
	if p.Priority != 0 {
		out["priority"] = p.Priority
	}
	if p.WorkflowID != "" {
		out["workflowId"] = p.WorkflowID
	}
	if p.IdempotencyKey != "" {
		out["idempotencyKey"] = p.IdempotencyKey
	}
	if len(p.RawContext) > 0 {
		// Round-trip through json.RawMessage so json.Marshal
		// emits the bytes verbatim instead of base64-encoding.
		out["context"] = p.RawContext
	} else if p.Prompt != "" || len(p.ExtraContext) > 0 {
		ctxBlock := make(map[string]any, len(p.ExtraContext)+1)
		for k, v := range p.ExtraContext {
			ctxBlock[k] = v
		}
		if p.Prompt != "" {
			ctxBlock["prompt"] = p.Prompt
		}
		out["context"] = ctxBlock
	}
	return json.Marshal(out)
}

// missingRoles returns the set of roles the workflow requires that
// the swarm doesn't supply. Mirrors the doctor check's
// missingRolesForWorkflow exactly so the form's compatibility
// gate matches what doctor reports. Returns nil when the swarm or
// workflow is missing — registry validation catches those at load
// time; we don't want to double-report here.
func missingRoles(wf *registry.Workflow, swarm *registry.Swarm) []string {
	if wf == nil || swarm == nil {
		return nil
	}
	have := make(map[string]bool, len(swarm.Roles))
	for _, r := range swarm.Roles {
		have[r.Name] = true
	}
	seen := map[string]bool{}
	var missing []string
	for _, step := range wf.Steps {
		if step.Type != "agent" && step.Type != "plan" {
			continue
		}
		if step.Role == "" || have[step.Role] || seen[step.Role] {
			continue
		}
		seen[step.Role] = true
		missing = append(missing, step.Role)
	}
	return missing
}

// ptrIfNonEmpty returns &s when s is non-empty, nil otherwise.
// Mirrors api.strPtr's contract so persisted rows have NULL on
// the matching columns when the caller didn't supply a value.
func ptrIfNonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
