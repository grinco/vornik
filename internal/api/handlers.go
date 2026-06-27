// Package api provides HTTP handlers for the vornik data plane API.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/budget"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/taskcreate"
)

const (
	maxTaskRequestBytes  = 1 << 20
	maxOptionalBodyBytes = 16 << 10
	// maxMCPArgsBytes caps the CallMCPTool request body. MCP tool
	// arguments can legitimately be larger than typical control-plane
	// bodies (file contents, structured payloads), so this is sized
	// above maxOptionalBodyBytes but still bounded to prevent an
	// unauthenticated/agent caller from forcing unbounded heap
	// allocation by streaming an arbitrarily large body.
	maxMCPArgsBytes = 1 << 20
)

// CreateTaskRequest represents the request body for creating a task.
type CreateTaskRequest struct {
	TaskType       string          `json:"taskType"`
	Priority       int             `json:"priority,omitempty"`
	WorkflowID     string          `json:"workflowId,omitempty"`
	InputArtifacts []InputArtifact `json:"inputArtifacts,omitempty"`
	IdempotencyKey string          `json:"idempotencyKey,omitempty"`
	// Context is a free-form payload that the agent runtime reads via
	// extractPrompt / extractResultMessage helpers. The established
	// convention (written by autonomy/manager.go:createAutonomousTask
	// and consumed by every researcher role) is {"prompt": "..."}. The
	// field is json.RawMessage so callers can extend it with additional
	// per-task shape without schema migrations — vornikctl task submit
	// uses this path to forward --prompt text from operators.
	Context json.RawMessage `json:"context,omitempty"`
}

// InputArtifact represents an input artifact in the request.
type InputArtifact struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

// CreateTaskResponse represents the response for task creation.
type CreateTaskResponse struct {
	TaskID        string            `json:"taskId"`
	Status        string            `json:"status"`
	QueuePosition int               `json:"queuePosition,omitempty"`
	Links         map[string]string `json:"links"`
}

// GetTaskResponse represents the response for task retrieval.
type GetTaskResponse struct {
	TaskID            string            `json:"taskId"`
	Status            string            `json:"status"`
	ProjectID         string            `json:"projectId"`
	TaskType          string            `json:"taskType"`
	Priority          int               `json:"priority"`
	CreatedAt         string            `json:"createdAt"`
	ActiveExecutionID string            `json:"activeExecutionId,omitempty"`
	LastError         string            `json:"lastError,omitempty"`
	LastErrorClass    string            `json:"lastErrorClass,omitempty"`
	Links             map[string]string `json:"links"`
}

// ListTasksResponse represents the response for listing tasks.
type ListTasksResponse struct {
	Tasks []GetTaskResponse `json:"tasks"`
	Total int               `json:"total"`
}

// Scope-helper convention
// ------------------------
// requestAllowsProject and requestAllowsOperator are the canonical
// scope primitives. Every place that filters rows by project or
// operator should call one of these (or their exported siblings
// RequestAllowsProject / RequestAllowsOperator for the UI subtree)
// rather than open-coding scope checks against context keys.
//
// Both primitives bake in:
//   - auth-disabled bypass (single-tenant local installs see everything),
//   - fail-closed default (no principal + auth on → no access).
//
// New handlers should reach for these helpers rather than calling
// requestOperatorID for visibility decisions (requestOperatorID is the
// audit/identity lookup; it intentionally returns empty for anonymous
// callers so audit rows record "no verified principal" rather than a
// fabricated one).
//
// These helpers are the single scope-filter convention for HTTP/UI
// handlers: filter rows by requestAllowsProject / requestAllowsOperator,
// gate admin handlers with requireAdminGate. (The auth-prep scope audit
// that inventoried these call sites is retired — its gaps are all closed
// and the convention now lives here.)

func requestAllowsProject(r *http.Request, projectID string) bool {
	if projectID == "" {
		return false
	}

	// Auth-disabled deployments are single-tenant; no scope to
	// enforce. Made explicit (rather than relying on the absence
	// of projectIDKey) so a future middleware change that stamps
	// the key on auth-off requests doesn't silently break this.
	if !IsAuthEnabledFromContext(r.Context()) {
		return true
	}

	allowedProjects, ok := r.Context().Value(projectIDKey).([]string)
	if !ok || len(allowedProjects) == 0 {
		return true
	}

	for _, allowed := range allowedProjects {
		if allowed == projectID {
			return true
		}
	}

	return false
}

func requestScopedProjectSet(r *http.Request) (map[string]bool, bool) {
	if r == nil || !IsAuthEnabledFromContext(r.Context()) {
		return nil, false
	}
	allowedProjects, ok := r.Context().Value(projectIDKey).([]string)
	if !ok || len(allowedProjects) == 0 {
		return nil, false
	}
	out := make(map[string]bool, len(allowedProjects))
	for _, p := range allowedProjects {
		if p != "" {
			out[p] = true
		}
	}
	return out, len(out) > 0
}

// RequestAllowsProject exposes the project-scope check to sibling
// packages that share the API auth middleware, notably the UI subtree.
func RequestAllowsProject(r *http.Request, projectID string) bool {
	return requestAllowsProject(r, projectID)
}

// ContextWithProjectScope stamps the auth-enabled flag and a project
// allowlist onto ctx, exactly as AuthMiddleware does for a project-
// scoped session. Exposed so sibling packages (the UI subtree) can
// build scoped requests in tests without reaching the unexported
// context keys. A lone "*" yields all-access (matching how admins are
// stamped — the middleware stamps nothing). Not used on production
// request paths; the middleware owns those.
func ContextWithProjectScope(ctx context.Context, projects ...string) context.Context {
	ctx = context.WithValue(ctx, authEnabledKey, true)
	if len(projects) == 1 && projects[0] == "*" {
		return ctx
	}
	return context.WithValue(ctx, projectIDKey, projects)
}

// ContextWithSessionID stamps an auth identity carrying the given
// ui_sessions.id so SessionIDFromContext returns it. Exposed for tests
// of session-aware handlers (the admin session viewer's current-session
// guard); production stamps this via the session auth backend.
func ContextWithSessionID(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, identityKey, &auth.Identity{
		Extra: map[string]any{auth.ExtraSessionID: sessionID},
	})
}

// RequestScopedProjects returns the explicit project allowlist a
// project-scoped session is limited to (sorted), and whether such a
// limit applies. (nil, false) means NO scoping — all-project access
// (admin, auth-off, or an unscoped key) — so the caller must not
// constrain its query. UI list pages use this to scope the DB query
// itself: a global "latest N then post-filter" page silently drops a
// scoped user's rows that fall past the page boundary (a scoped user
// would see only the few of their rows that land in the global page).
func RequestScopedProjects(r *http.Request) ([]string, bool) {
	set, ok := requestScopedProjectSet(r)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, true
}

// requestAllowsOperator reports whether the request is allowed to
// see / act on rows owned by operatorID. The operator-scope
// counterpart to requestAllowsProject; use this for visibility
// filters rather than open-coding `requestOperatorID(r) == row.opID`,
// which keeps failing closed when auth is off and there's no
// principal to compare against.
//
// Returns true when:
//   - auth is disabled (single-tenant local install), or
//   - the verified operator principal equals operatorID.
//
// Returns false otherwise, including when operatorID is empty
// (global / un-owned rows belong to admin-class callers; see the
// per-handler admin check that runs before this).
func requestAllowsOperator(r *http.Request, operatorID string) bool {
	if !IsAuthEnabledFromContext(r.Context()) {
		return true
	}
	if operatorID == "" {
		return false
	}
	op := requestOperatorID(r)
	return op != "" && op == operatorID
}

// RequestAllowsOperator exposes the operator-scope check to sibling
// packages (mirrors RequestAllowsProject).
func RequestAllowsOperator(r *http.Request, operatorID string) bool {
	return requestAllowsOperator(r, operatorID)
}

// RequestOperatorID exposes the verified operator identity derived
// from the auth middleware. It follows the same spoofing rules as API
// handlers: API key principal wins; X-Operator-Id is accepted only when
// auth is explicitly disabled.
func RequestOperatorID(r *http.Request) string {
	return requestOperatorID(r)
}

// defaultSingleTenantOperatorID is the fallback identity stamped on
// auth-off requests that need a non-empty operator. Configurable via
// APIConfig.SingleTenantOperatorID; the literal default keeps the
// out-of-the-box dev experience working — without it the wizard CLI
// + UI drafts banner would 401 on every fresh local install.
const defaultSingleTenantOperatorID = "local:dev"

// SingleTenantOperatorIDFromConfig resolves the auth-off operator
// fallback from the daemon config, applying the `local:dev` default
// when the operator hasn't set one. Exported so the UI subtree (which
// doesn't carry the full *config.Config) can be wired with the same
// resolved value the API uses.
func SingleTenantOperatorIDFromConfig(cfg *config.Config) string {
	if cfg != nil {
		if v := strings.TrimSpace(cfg.API.SingleTenantOperatorID); v != "" {
			return v
		}
	}
	return defaultSingleTenantOperatorID
}

// RequestOperatorIDOrSingleTenant returns the verified operator
// identity (same as RequestOperatorID) when present; otherwise, when
// auth is disabled, the configured single-tenant fallback. Returns
// empty when auth is enabled with no principal — fail-closed on the
// hot path remains intact.
//
// Use this from handlers that need a non-empty operator id to
// function (wizard sessions, UI drafts banner). Audit-stamping
// handlers should keep using RequestOperatorID so the audit row
// records "no verified principal" rather than the fallback string.
func RequestOperatorIDOrSingleTenant(r *http.Request, fallback string) string {
	if op := requestOperatorID(r); op != "" {
		return op
	}
	if IsAuthEnabledFromContext(r.Context()) {
		return ""
	}
	if strings.TrimSpace(fallback) == "" {
		return defaultSingleTenantOperatorID
	}
	return fallback
}

// CreateTask handles POST /projects/{projectId}/tasks
func (s *Server) CreateTask(w http.ResponseWriter, r *http.Request) {
	projectID := extractProjectID(r)
	if projectID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "projectId is required")
		return
	}

	// Parse request body
	var req CreateTaskRequest
	if err := decodeJSONBody(w, r, maxTaskRequestBytes, &req); err != nil {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request body")
		return
	}

	// Validate required fields
	if req.TaskType == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "taskType is required")
		return
	}

	// Idempotency key: the JSON body field wins for backward-compat, but
	// 12-data-plane-api.md §4 documents an `Idempotency-Key:` HTTP header.
	// Accept the header as an alias when the body omits the field so both
	// surfaces resolve to the same de-dup key downstream (Creator + inline).
	if strings.TrimSpace(req.IdempotencyKey) == "" {
		if hdr := strings.TrimSpace(r.Header.Get("Idempotency-Key")); hdr != "" {
			req.IdempotencyKey = hdr
		}
	}

	// Validate project exists
	if s.projectRegistry != nil {
		if s.projectRegistry.GetProject(projectID) == nil {
			respondError(w, http.StatusNotFound, "NOT_FOUND", "Project not found")
			return
		}
	}

	// InputArtifacts handling: snapshot each inline payload via the
	// artifact store, fire auto-extraction, and fold the resulting
	// inputFiles / inputArtifactIDs / inputExtractions into the
	// caller's context JSON. Runs BEFORE task creation so the
	// persisted payload carries the references — the worker step
	// then sees the same memory-ready trailer the dispatcher's
	// chat-driven path produces.
	if len(req.InputArtifacts) > 0 {
		if s.inputArtifactStore == nil {
			respondError(w, http.StatusServiceUnavailable, "NOT_CONFIGURED",
				"input artifacts are not configured on this daemon")
			return
		}
		results, err := s.processInputArtifacts(r.Context(), projectID, req.InputArtifacts)
		if err != nil {
			respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
			return
		}
		merged, err := mergeInputsIntoContext(req.Context, results)
		if err != nil {
			respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
			return
		}
		req.Context = merged
	}

	// When the shared task-creation core is wired (production
	// wiring), delegate. The legacy inline path below stays so
	// existing tests that don't set up a Creator keep working
	// without further refactoring; production always wires the
	// Creator so the two surfaces (REST + UI form) share one
	// lifecycle.
	if s.taskCreator != nil {
		s.createTaskViaCreator(w, r, projectID, req)
		return
	}

	// Rate-limit gate: per-minute / per-hour caps on task creation. Runs
	// before the budget check because it's cheaper (in-memory, no DB sum).
	if s.rateLimiter != nil && s.projectRegistry != nil {
		if proj := s.projectRegistry.GetProject(projectID); proj != nil {
			d := s.rateLimiter.Check(proj, time.Now())
			if s.rateLimitMetrics != nil {
				s.rateLimitMetrics.ObserveProject(projectID, d)
			}
			if d.Blocked {
				respondError(w, http.StatusTooManyRequests, "RATE_LIMITED", d.Reason)
				return
			}
		}
	}

	// Budget gate: daily/monthly hard caps convert to 429. Soft breaches
	// log but still accept the task — the operator gets visibility via the
	// logs and the usage metrics without the caller seeing a failure.
	// Both soft and hard breaches also notify telegram (once per
	// project/period/level) so the operator isn't only watching logs.
	if s.llmUsageRepo != nil && s.projectRegistry != nil {
		if proj := s.projectRegistry.GetProject(projectID); proj != nil {
			decision, berr := budget.Check(r.Context(), s.llmUsageRepo, proj, time.Now().UTC())
			if berr != nil {
				s.logger.Warn().Err(berr).Str("project_id", projectID).Msg("api: budget check failed — proceeding")
			} else if decision.Blocked {
				if s.budgetNotifier != nil {
					period, level := decision.Period()
					s.budgetNotifier.NotifyBudgetBreach(r.Context(), projectID, level, period, decision)
				}
				respondError(w, http.StatusTooManyRequests, "BUDGET_EXCEEDED", decision.Reason)
				return
			} else if decision.SoftBreached {
				s.logger.Warn().
					Str("project_id", projectID).
					Str("reason", decision.Reason).
					Msg("api: create-task proceeding despite soft budget breach")
				if s.budgetNotifier != nil {
					period, level := decision.Period()
					s.budgetNotifier.NotifyBudgetBreach(r.Context(), projectID, level, period, decision)
				}
			}
		}
	}

	// Determine workflow
	workflowID := req.WorkflowID
	if workflowID == "" && s.projectRegistry != nil {
		if project := s.projectRegistry.GetProject(projectID); project != nil {
			workflowID = project.DefaultWorkflowID
		}
	}

	// Determine priority
	priority := req.Priority
	if priority == 0 {
		priority = 50 // default
		if s.projectRegistry != nil {
			if project := s.projectRegistry.GetProject(projectID); project != nil {
				priority = project.DefaultPriority
			}
		}
	}

	// Generate task ID
	taskID := persistence.GenerateID("task")
	idempotencyKey := strPtr(strings.TrimSpace(req.IdempotencyKey))

	if idempotencyKey != nil && s.taskRepo != nil {
		existing, err := s.taskRepo.GetByIdempotencyKey(r.Context(), projectID, *idempotencyKey)
		if err == nil && existing != nil {
			respondJSON(w, http.StatusOK, buildCreateTaskResponse(existing))
			return
		}
		if err != nil && !errors.Is(err, persistence.ErrNotFound) && !errors.Is(err, sql.ErrNoRows) {
			s.logger.Error().Err(err).Str("project_id", projectID).Msg("failed to check idempotency key")
			respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to create task")
			return
		}
	}

	// Create task model
	payload, perr := marshalTaskPayload(req)
	if perr != nil {
		s.logger.Error().Err(perr).Str("project_id", projectID).Msg("failed to marshal task payload")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to create task")
		return
	}
	task := &persistence.Task{
		ID:             taskID,
		ProjectID:      projectID,
		WorkflowID:     strPtr(workflowID),
		IdempotencyKey: idempotencyKey,
		CreationSource: persistence.TaskCreationSourceUser,
		Status:         persistence.TaskStatusQueued,
		Priority:       priority,
		Payload:        payload,
		Attempt:        1,
		MaxAttempts:    3,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}

	// Persist task
	if s.taskRepo != nil {
		if err := s.taskRepo.Create(r.Context(), task); err != nil {
			if errors.Is(err, persistence.ErrDuplicateKey) && idempotencyKey != nil {
				existing, getErr := s.taskRepo.GetByIdempotencyKey(r.Context(), projectID, *idempotencyKey)
				if getErr == nil && existing != nil {
					respondJSON(w, http.StatusOK, buildCreateTaskResponse(existing))
					return
				}
			}
			s.logger.Error().Err(err).Msg("failed to create task")
			respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to create task")
			return
		}

		// Enqueue task
		if s.queue != nil {
			if err := s.queue.Enqueue(taskID, projectID, priority); err != nil {
				s.logger.Error().Err(err).Msg("failed to enqueue task")
				respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to enqueue task")
				return
			}
		}
		if s.rateLimiter != nil {
			s.rateLimiter.Record(projectID, time.Now())
		}
	}

	// Build response
	resp := buildCreateTaskResponse(task)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		return
	}
}

// createTaskViaCreator routes the request through the shared
// taskcreate.Creator. Reasons map to the same HTTP statuses the
// inline path used so external clients (vornikctl, telegram
// dispatcher, automated callers) don't see status drift on
// migration.
func (s *Server) createTaskViaCreator(w http.ResponseWriter, r *http.Request, projectID string, req CreateTaskRequest) {
	startedAt := time.Now()
	if s.rateLimitMetrics != nil && s.projectRegistry != nil {
		if proj := s.projectRegistry.GetProject(projectID); proj != nil && s.rateLimiter != nil {
			// Observe the limiter decision the same way the inline
			// path did, before delegating — the Creator runs its
			// own Check() too, but the metric emission lives
			// here. Cheap (in-memory).
			d := s.rateLimiter.Check(proj, time.Now())
			s.rateLimitMetrics.ObserveProject(projectID, d)
		}
	}
	task, err := s.taskCreator.Create(r.Context(), taskcreate.Params{
		ProjectID:      projectID,
		TaskType:       req.TaskType,
		Priority:       req.Priority,
		WorkflowID:     req.WorkflowID,
		IdempotencyKey: strings.TrimSpace(req.IdempotencyKey),
		RawContext:     req.Context,
	})
	if err != nil {
		ce := taskcreate.AsError(err)
		if ce == nil {
			s.logger.Error().Err(err).Msg("api: taskcreate returned non-typed error")
			respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to create task")
			return
		}
		switch ce.Reason {
		case taskcreate.ReasonValidation:
			respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", ce.Message)
		case taskcreate.ReasonProjectNotFound:
			respondError(w, http.StatusNotFound, "NOT_FOUND", ce.Message)
		case taskcreate.ReasonWorkflowNotFound, taskcreate.ReasonWorkflowIncompat:
			respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", ce.Message)
		case taskcreate.ReasonRateLimited:
			respondError(w, http.StatusTooManyRequests, "RATE_LIMITED", ce.Message)
		case taskcreate.ReasonBudgetExceeded:
			respondError(w, http.StatusTooManyRequests, "BUDGET_EXCEEDED", ce.Message)
		default:
			respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to create task")
		}
		return
	}
	resp := buildCreateTaskResponse(task)
	w.Header().Set("Content-Type", "application/json")
	// If the request hit an existing idempotent task, the legacy
	// inline path returned 200 instead of 202. Preserve that by
	// keying the status on whether Create returned a task whose
	// CreatedAt predates this handler call. New rows created by
	// taskcreate use the same process clock after startedAt.
	status := http.StatusAccepted
	if strings.TrimSpace(req.IdempotencyKey) != "" && task.CreatedAt.Before(startedAt) {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}

// GetTask handles GET /projects/{projectId}/tasks/{taskId}
func (s *Server) GetTask(w http.ResponseWriter, r *http.Request) {
	projectID := extractProjectID(r)
	taskID := extractTaskID(r)

	if projectID == "" || taskID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "projectId and taskId are required")
		return
	}

	// Defence-in-depth: ProjectAuthMiddleware already enforces this on the
	// global router, but a sub-router wiring without the middleware would
	// otherwise expose cross-project reads. Mirrors the registry/swarm/
	// workflow tightenings landed in c10838a.
	if !requestAllowsProject(r, projectID) {
		respondError(w, http.StatusForbidden, "FORBIDDEN", "Access denied to project")
		return
	}

	// Retrieve task from repository
	if s.taskRepo == nil {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "Task not found")
		return
	}

	task, err := s.taskRepo.Get(r.Context(), taskID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
			respondError(w, http.StatusNotFound, "NOT_FOUND", "Task not found")
			return
		}
		s.logger.Error().Err(err).Msg("failed to get task")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to get task")
		return
	}

	// Verify project ID matches
	if task.ProjectID != projectID {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "Task not found in project")
		return
	}

	// Get active execution if running
	var activeExecutionID string
	if task.Status == persistence.TaskStatusRunning && s.executionRepo != nil {
		if exec, err := s.executionRepo.GetByTaskID(r.Context(), taskID); err == nil {
			activeExecutionID = exec.ID
		}
	}

	// Build response
	resp := GetTaskResponse{
		TaskID:            task.ID,
		Status:            string(task.Status),
		ProjectID:         task.ProjectID,
		Priority:          task.Priority,
		CreatedAt:         task.CreatedAt.Format(time.RFC3339),
		ActiveExecutionID: activeExecutionID,
		Links:             make(map[string]string),
	}
	if task.LastError != nil {
		resp.LastError = *task.LastError
	}
	if task.LastErrorClass != nil {
		resp.LastErrorClass = *task.LastErrorClass
	}

	if activeExecutionID != "" {
		resp.Links["execution"] = "/api/v1/executions/" + activeExecutionID
	}

	// Extract task type from payload
	if len(task.Payload) > 0 {
		var payload CreateTaskRequest
		if err := json.Unmarshal(task.Payload, &payload); err == nil {
			resp.TaskType = payload.TaskType
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		return
	}
}

// ListTasks handles GET /projects/{projectId}/tasks
func (s *Server) ListTasks(w http.ResponseWriter, r *http.Request) {
	projectID := extractProjectID(r)
	if projectID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "projectId is required")
		return
	}

	// Defence-in-depth: see GetTask above for rationale.
	if !requestAllowsProject(r, projectID) {
		respondError(w, http.StatusForbidden, "FORBIDDEN", "Access denied to project")
		return
	}

	// Validate project exists in the registry. Without this guard, a
	// typo in --project silently returns an empty list and the operator
	// thinks the project has no tasks when in fact the project ID is wrong.
	if s.projectRegistry != nil {
		if s.projectRegistry.GetProject(projectID) == nil {
			respondError(w, http.StatusNotFound, "NOT_FOUND", "Project not found: "+projectID)
			return
		}
	}

	// Parse pagination params
	pageSize := parsePageSize(r.URL.Query().Get("pageSize"), 20)
	offset := parseOffset(r.URL.Query().Get("offset"))

	// Build filter
	filter := persistence.TaskFilter{
		ProjectID: &projectID,
		PageSize:  pageSize,
		Offset:    offset,
	}

	// Parse status filter — validate against the known enum. An invalid
	// value would otherwise reach the DB unchecked and bubble up as a
	// generic 500 because the enum constraint fires server-side.
	// Canonical enum values are UPPERCASE; we accept either case for
	// operator convenience and normalise before comparing.
	if statusStr := r.URL.Query().Get("status"); statusStr != "" {
		status := persistence.TaskStatus(strings.ToUpper(statusStr))
		if !isKnownTaskStatus(status) {
			respondError(w, http.StatusBadRequest, "VALIDATION_ERROR",
				"unknown status "+statusStr+"; valid: PENDING, QUEUED, LEASED, RUNNING, WAITING_FOR_CHILDREN, COMPLETED, FAILED, CANCELLED")
			return
		}
		filter.Status = &status
	}

	// Retrieve tasks from repository
	if s.taskRepo == nil {
		respondJSON(w, http.StatusOK, ListTasksResponse{Tasks: []GetTaskResponse{}, Total: 0})
		return
	}

	tasks, err := s.taskRepo.List(r.Context(), filter)
	if err != nil {
		s.logger.Error().Err(err).Msg("failed to list tasks")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to list tasks")
		return
	}

	// Total count ignores PageSize/Offset so paginated clients can
	// tell when to stop. A Count failure is logged but not fatal —
	// the page itself is still useful, and Total falls back to the
	// page length to preserve the legacy shape.
	total := len(tasks)
	if c, cerr := s.taskRepo.Count(r.Context(), filter); cerr != nil {
		s.logger.Warn().Err(cerr).Msg("task count failed; falling back to page length for Total")
	} else {
		total = int(c)
	}

	// Build response
	resp := ListTasksResponse{
		Tasks: make([]GetTaskResponse, 0, len(tasks)),
		Total: total,
	}

	for _, task := range tasks {
		taskResp := GetTaskResponse{
			TaskID:    task.ID,
			Status:    string(task.Status),
			ProjectID: task.ProjectID,
			Priority:  task.Priority,
			CreatedAt: task.CreatedAt.Format(time.RFC3339),
			Links:     make(map[string]string),
		}
		if task.LastError != nil {
			taskResp.LastError = *task.LastError
		}
		if task.LastErrorClass != nil {
			taskResp.LastErrorClass = *task.LastErrorClass
		}

		// Extract task type from payload
		if len(task.Payload) > 0 {
			var payload CreateTaskRequest
			if err := json.Unmarshal(task.Payload, &payload); err == nil {
				taskResp.TaskType = payload.TaskType
			}
		}

		resp.Tasks = append(resp.Tasks, taskResp)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		return
	}
}

// CancelTaskRequest represents the request body for cancelling a task.
type CancelTaskRequest struct {
	Reason string `json:"reason,omitempty"`
}

// CancelTaskResponse represents the response for task cancellation.
type CancelTaskResponse struct {
	TaskID      string `json:"taskId"`
	Status      string `json:"status"`
	WasRunning  bool   `json:"wasRunning"`
	CancelledAt string `json:"cancelledAt"`
}

// CancelTask handles POST /projects/{projectId}/tasks/{taskId}/cancel
func (s *Server) CancelTask(w http.ResponseWriter, r *http.Request) {
	projectID := extractProjectID(r)
	taskID := extractTaskID(r)

	if projectID == "" || taskID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "projectId and taskId are required")
		return
	}

	// Retrieve task from repository
	if s.taskRepo == nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Task repository not available")
		return
	}

	task, err := s.taskRepo.Get(r.Context(), taskID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
			respondError(w, http.StatusNotFound, "NOT_FOUND", "Task not found")
			return
		}
		s.logger.Error().Err(err).Msg("failed to get task")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to get task")
		return
	}

	// Verify project ID matches
	if task.ProjectID != projectID {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "Task not found in project")
		return
	}

	// Check if task can be cancelled — fast path on the read so a
	// terminal task gets a clear INVALID_STATE before we even try.
	cancellableStatuses := map[persistence.TaskStatus]bool{
		persistence.TaskStatusQueued:  true,
		persistence.TaskStatusLeased:  true,
		persistence.TaskStatusRunning: true,
		persistence.TaskStatusPending: true,
	}

	if !cancellableStatuses[task.Status] {
		respondError(w, http.StatusBadRequest, "INVALID_STATE",
			fmt.Sprintf("Task cannot be cancelled in status %s", task.Status))
		return
	}

	wasRunning := task.Status == persistence.TaskStatusRunning

	// Atomic conditional transition. The legacy code did
	// read-status → check → write-CANCELLED in three steps; if a
	// task COMPLETED between the read and write, the third step
	// silently overwrote the terminal state. The new
	// TransitionToCancelled gates the WRITE on the live status, so
	// a racing terminal write wins. When the row didn't transition
	// we re-fetch and report the actual current state.
	transitioned, err := s.taskRepo.TransitionToCancelled(r.Context(), taskID)
	if err != nil {
		s.logger.Error().Err(err).Str("taskId", taskID).Msg("failed to atomically cancel task")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to cancel task")
		return
	}
	if !transitioned {
		fresh, getErr := s.taskRepo.Get(r.Context(), taskID)
		if getErr != nil {
			respondError(w, http.StatusConflict, "INVALID_STATE", "Task is no longer in a cancellable state")
			return
		}
		respondError(w, http.StatusConflict, "INVALID_STATE",
			fmt.Sprintf("Task transitioned to %s before cancellation took effect", fresh.Status))
		return
	}

	// Transition succeeded — best-effort tear down the running
	// execution. A failure here is logged but not surfaced: the
	// canonical state is the DB row, which is now CANCELLED. The
	// scheduler/executor reaper picks up the orphaned process.
	if wasRunning && s.executor != nil {
		if err := s.executor.Cancel(taskID); err != nil {
			s.logger.Error().Err(err).Str("taskId", taskID).Msg("failed to cancel execution")
		}
	}

	// CANCELLED is terminal, so drive the parent-unblock sweep — for
	// a non-running child (QUEUED/LEASED/PENDING) the executor's
	// handleCancelled path never fires, so without this nudge a
	// WAITING_FOR_CHILDREN parent waited for the cancelled child
	// forever (regression 2026-06-07). Idempotent for the running
	// case: the unblock core is TransitionConditional-gated, so the
	// overlap with handleCancelled's own call is a clean no-op.
	if s.executor != nil {
		s.executor.NotifyChildTerminal(r.Context(), taskID)
	}

	// Build response
	resp := CancelTaskResponse{
		TaskID:      taskID,
		Status:      string(persistence.TaskStatusCancelled),
		WasRunning:  wasRunning,
		CancelledAt: time.Now().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		return
	}
}

// GetExecutionResponse represents the response for execution retrieval.
type GetExecutionResponse struct {
	ExecutionID    string            `json:"executionId"`
	TaskID         string            `json:"taskId"`
	ProjectID      string            `json:"projectId"`
	WorkflowID     string            `json:"workflowId"`
	Status         string            `json:"status"`
	CurrentStepID  string            `json:"currentStepId,omitempty"`
	CompletedSteps []string          `json:"completedSteps,omitempty"`
	ErrorMessage   string            `json:"errorMessage,omitempty"`
	ErrorCode      string            `json:"errorCode,omitempty"`
	StartedAt      string            `json:"startedAt,omitempty"`
	CompletedAt    string            `json:"completedAt,omitempty"`
	Duration       string            `json:"duration,omitempty"`
	Links          map[string]string `json:"links"`
	// Result is the raw result JSON the executor persisted for this
	// execution. Omitted on in-flight executions where it is empty;
	// populated after the plan step writes its envelope (message +
	// changes block for dev workflows, raw role JSON otherwise).
	Result json.RawMessage `json:"result,omitempty"`
}

// ListExecutionsResponse represents the response for listing executions.
type ListExecutionsResponse struct {
	Executions []GetExecutionResponse `json:"executions"`
	Total      int                    `json:"total"`
}

// PauseExecutionRequest represents the request body for pausing an execution.
type PauseExecutionRequest struct {
	Reason string `json:"reason,omitempty"`
}

// PauseExecutionResponse represents the response for execution pause.
type PauseExecutionResponse struct {
	ExecutionID string `json:"executionId"`
	Status      string `json:"status"`
	PausedAt    string `json:"pausedAt"`
}

// ResumeExecutionResponse represents the response for execution resume.
type ResumeExecutionResponse struct {
	ExecutionID string `json:"executionId"`
	Status      string `json:"status"`
	ResumedAt   string `json:"resumedAt"`
}

// RetryTaskRequest represents the request body for retrying a task.
type RetryTaskRequest struct {
	ResetAttempts bool `json:"resetAttempts,omitempty"` // Reset attempt counter to 1
}

// RetryTaskResponse represents the response for task retry.
type RetryTaskResponse struct {
	TaskID    string `json:"taskId"`
	Status    string `json:"status"`
	Attempt   int    `json:"attempt"`
	RetriedAt string `json:"retriedAt"`
}

// RetryTask handles POST /projects/{projectId}/tasks/{taskId}/retry
func (s *Server) RetryTask(w http.ResponseWriter, r *http.Request) {
	projectID := extractProjectID(r)
	taskID := extractTaskID(r)

	if projectID == "" || taskID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "projectId and taskId are required")
		return
	}

	// Retrieve task from repository
	if s.taskRepo == nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Task repository not available")
		return
	}

	task, err := s.taskRepo.Get(r.Context(), taskID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
			respondError(w, http.StatusNotFound, "NOT_FOUND", "Task not found")
			return
		}
		s.logger.Error().Err(err).Msg("failed to get task")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to get task")
		return
	}

	// Verify project ID matches
	if task.ProjectID != projectID {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "Task not found in project")
		return
	}

	// Check if task can be retried (must be in terminal state)
	terminalStatuses := map[persistence.TaskStatus]bool{
		persistence.TaskStatusFailed:    true,
		persistence.TaskStatusCancelled: true,
		persistence.TaskStatusCompleted: true,
	}

	if !terminalStatuses[task.Status] {
		respondError(w, http.StatusBadRequest, "INVALID_STATE",
			fmt.Sprintf("Task cannot be retried in status %s", task.Status))
		return
	}

	// Parse optional request body. A present-but-malformed body must not be
	// silently treated as an empty request — that would drop flags like
	// ResetAttempts on the floor and change retry behavior.
	var req RetryTaskRequest
	if r.Body != nil && r.ContentLength > 0 {
		if err := decodeJSONBody(w, r, maxOptionalBodyBytes, &req); err != nil {
			respondError(w, http.StatusBadRequest, "VALIDATION_ERROR",
				fmt.Sprintf("invalid retry request body: %v", err))
			return
		}
	}

	// Update attempt counter
	if req.ResetAttempts {
		task.Attempt = 1
	} else {
		task.Attempt++
		if task.Attempt > task.MaxAttempts {
			task.MaxAttempts = task.Attempt
		}
	}

	// Atomic terminal-to-QUEUED transition. The legacy code called
	// ReleaseLease(taskID, "") to do this, repurposing a lease-
	// management primitive for a state-machine transition — passing
	// an empty leaseID could silently re-queue a task that was no
	// longer terminal by the time the call landed (race with a
	// scheduler reaper, or a duplicate retry click), corrupting an
	// in-flight task. RequeueTerminalTask gates the WRITE on the
	// live status set and reports whether the row transitioned, so
	// concurrent retry clicks resolve to "first one wins, second
	// gets 409".
	transitioned, err := s.taskRepo.RequeueTerminalTask(r.Context(), taskID, task.Attempt, task.MaxAttempts)
	if err != nil {
		s.logger.Error().Err(err).Str("taskId", taskID).Msg("failed to requeue task for retry")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to retry task")
		return
	}
	if !transitioned {
		fresh, getErr := s.taskRepo.Get(r.Context(), taskID)
		if getErr != nil {
			respondError(w, http.StatusConflict, "INVALID_STATE", "Task is no longer in a retryable terminal state")
			return
		}
		respondError(w, http.StatusConflict, "INVALID_STATE",
			fmt.Sprintf("Task is now %s and can no longer be retried from a terminal state", fresh.Status))
		return
	}

	// Re-enqueue task
	if s.queue != nil {
		if err := s.queue.Enqueue(taskID, projectID, task.Priority); err != nil {
			s.logger.Error().Err(err).Str("taskId", taskID).Msg("failed to enqueue task for retry")
			respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to enqueue task for retry")
			return
		}
	}

	// Build response
	resp := RetryTaskResponse{
		TaskID:    taskID,
		Status:    string(persistence.TaskStatusQueued),
		Attempt:   task.Attempt,
		RetriedAt: time.Now().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		return
	}
}

// GetExecution handles GET /executions/{executionId}
func (s *Server) GetExecution(w http.ResponseWriter, r *http.Request) {
	executionID := extractExecutionID(r)
	if executionID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "executionId is required")
		return
	}

	if s.executionRepo == nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Execution repository not available")
		return
	}

	exec, err := s.executionRepo.Get(r.Context(), executionID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
			respondError(w, http.StatusNotFound, "NOT_FOUND", "Execution not found")
			return
		}
		s.logger.Error().Err(err).Msg("failed to get execution")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to get execution")
		return
	}
	if !requestAllowsProject(r, exec.ProjectID) {
		respondError(w, http.StatusForbidden, "FORBIDDEN", "Access denied to project")
		return
	}

	resp := buildExecutionResponse(exec)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		return
	}
}

// PauseExecution handles POST /executions/{executionId}/pause
func (s *Server) PauseExecution(w http.ResponseWriter, r *http.Request) {
	executionID := extractExecutionID(r)
	if executionID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "executionId is required")
		return
	}

	if s.executionRepo == nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Execution repository not available")
		return
	}

	// Get execution record
	exec, err := s.executionRepo.Get(r.Context(), executionID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
			respondError(w, http.StatusNotFound, "NOT_FOUND", "Execution not found")
			return
		}
		s.logger.Error().Err(err).Msg("failed to get execution")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to get execution")
		return
	}
	if !requestAllowsProject(r, exec.ProjectID) {
		respondError(w, http.StatusForbidden, "FORBIDDEN", "Access denied to project")
		return
	}

	// Check if execution is running
	if exec.Status != persistence.ExecutionStatusRunning {
		respondError(w, http.StatusBadRequest, "INVALID_STATE",
			fmt.Sprintf("Execution cannot be paused in status %s", exec.Status))
		return
	}

	// Pause the execution via executor
	if s.executor == nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Executor not available")
		return
	}

	pauseStatus, err := s.executor.Pause(exec.TaskID)
	if err != nil {
		s.logger.Error().Err(err).Str("executionId", executionID).Msg("failed to pause execution")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to pause execution")
		return
	}

	// Feature #3 Phase C-2 — publish the paused event so any
	// open WebSocket subscribers flip their Pause→Resume button
	// without a page reload.
	s.emitPaused(r.Context(), executionID, "operator", requestOperatorID(r))

	resp := PauseExecutionResponse{
		ExecutionID: executionID,
		Status:      string(persistence.ExecutionStatusPaused),
		PausedAt:    pauseStatus.PausedAt.Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		return
	}
}

// ResumeExecution handles POST /executions/{executionId}/resume
func (s *Server) ResumeExecution(w http.ResponseWriter, r *http.Request) {
	executionID := extractExecutionID(r)
	if executionID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "executionId is required")
		return
	}

	if s.executionRepo == nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Execution repository not available")
		return
	}

	// Get execution record
	exec, err := s.executionRepo.Get(r.Context(), executionID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
			respondError(w, http.StatusNotFound, "NOT_FOUND", "Execution not found")
			return
		}
		s.logger.Error().Err(err).Msg("failed to get execution")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to get execution")
		return
	}
	if !requestAllowsProject(r, exec.ProjectID) {
		respondError(w, http.StatusForbidden, "FORBIDDEN", "Access denied to project")
		return
	}

	// Check if execution is paused
	if exec.Status != persistence.ExecutionStatusPaused {
		respondError(w, http.StatusBadRequest, "INVALID_STATE",
			fmt.Sprintf("Execution cannot be resumed in status %s", exec.Status))
		return
	}

	// Resume the execution via executor
	if s.executor == nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Executor not available")
		return
	}

	resumeStatus, err := s.executor.Resume(exec.TaskID)
	if err != nil {
		s.logger.Error().Err(err).Str("executionId", executionID).Msg("failed to resume execution")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to resume execution")
		return
	}

	// Feature #3 Phase C-2 — publish the resumed event so the
	// live view flips Resume→Pause without a page reload.
	s.emitResumed(r.Context(), executionID, requestOperatorID(r))

	resp := ResumeExecutionResponse{
		ExecutionID: executionID,
		Status:      string(persistence.ExecutionStatusRunning),
		ResumedAt:   resumeStatus.ResumedAt.Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		return
	}
}

// RetryExecutionFromStep handles POST
// /executions/{executionId}/retry-from-step. Body: {"step_id": "..."}.
// Resets the execution row's state to before the named step and
// re-runs from there in the existing execution row. Idempotent in
// the sense that two operators clicking simultaneously will see one
// success and one 409 — the second caller hits the
// already-executing guard.
func (s *Server) RetryExecutionFromStep(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Method not allowed")
		return
	}
	executionID := extractExecutionID(r)
	if executionID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "executionId is required")
		return
	}

	var body struct {
		StepID string `json:"step_id"`
	}
	if err := decodeJSONBody(w, r, maxOptionalBodyBytes, &body); err != nil {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "request body must be JSON: "+err.Error())
		return
	}
	body.StepID = strings.TrimSpace(body.StepID)
	if body.StepID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "step_id is required")
		return
	}

	if s.executionRepo == nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Execution repository not available")
		return
	}

	// Project authorization first — operators should not be able to
	// retry executions in projects they can't see.
	exec, err := s.executionRepo.Get(r.Context(), executionID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
			respondError(w, http.StatusNotFound, "NOT_FOUND", "Execution not found")
			return
		}
		s.logger.Error().Err(err).Str("executionId", executionID).Msg("retry-from-step: failed to load execution")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to load execution")
		return
	}
	if !requestAllowsProject(r, exec.ProjectID) {
		respondError(w, http.StatusForbidden, "FORBIDDEN", "Access denied to project")
		return
	}

	if s.executor == nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Executor not available")
		return
	}

	if err := s.executor.RetryFromStep(r.Context(), executionID, body.StepID); err != nil {
		// Translate sentinel errors from the executor to HTTP-shaped
		// responses so the UI can surface a precise message.
		switch {
		case errors.Is(err, executor.ErrRetryNotTerminal):
			respondError(w, http.StatusConflict, "INVALID_STATE", err.Error())
			return
		case errors.Is(err, executor.ErrRetryStepNotInExecution):
			respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
			return
		case errors.Is(err, executor.ErrRetryAlreadyExecuting):
			respondError(w, http.StatusConflict, "INVALID_STATE", err.Error())
			return
		}
		s.logger.Error().Err(err).
			Str("executionId", executionID).
			Str("stepId", body.StepID).
			Msg("retry-from-step: failed")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to retry execution from step")
		return
	}

	resp := struct {
		ExecutionID string `json:"executionId"`
		StepID      string `json:"stepId"`
		Status      string `json:"status"`
	}{
		ExecutionID: executionID,
		StepID:      body.StepID,
		Status:      string(persistence.ExecutionStatusRunning),
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		return
	}
}

// ListExecutions handles GET /projects/{projectId}/executions
func (s *Server) ListExecutions(w http.ResponseWriter, r *http.Request) {
	projectID := extractProjectID(r)
	if projectID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "projectId is required")
		return
	}

	// Defence-in-depth: see GetTask above for rationale.
	if !requestAllowsProject(r, projectID) {
		respondError(w, http.StatusForbidden, "FORBIDDEN", "Access denied to project")
		return
	}

	// Validate project exists — same rationale as ListTasks above.
	if s.projectRegistry != nil {
		if s.projectRegistry.GetProject(projectID) == nil {
			respondError(w, http.StatusNotFound, "NOT_FOUND", "Project not found: "+projectID)
			return
		}
	}

	// Parse pagination params
	pageSize := parsePageSize(r.URL.Query().Get("pageSize"), 20)
	offset := parseOffset(r.URL.Query().Get("offset"))

	// Build filter
	filter := persistence.ExecutionFilter{
		ProjectID: &projectID,
		PageSize:  pageSize,
		Offset:    offset,
	}

	// Parse status filter — validate the enum so typos return 400 not 500.
	// Accept either case; normalise to upper for the DB comparison.
	if statusStr := r.URL.Query().Get("status"); statusStr != "" {
		status := persistence.ExecutionStatus(strings.ToUpper(statusStr))
		if !isKnownExecutionStatus(status) {
			respondError(w, http.StatusBadRequest, "VALIDATION_ERROR",
				"unknown status "+statusStr+"; valid: PENDING, RUNNING, PAUSED, COMPLETED, FAILED, CANCELLED")
			return
		}
		filter.Status = &status
	}

	// Parse taskId filter
	if taskID := r.URL.Query().Get("taskId"); taskID != "" {
		filter.TaskID = &taskID
	}

	if s.executionRepo == nil {
		respondJSON(w, http.StatusOK, ListExecutionsResponse{Executions: []GetExecutionResponse{}, Total: 0})
		return
	}

	executions, err := s.executionRepo.List(r.Context(), filter)
	if err != nil {
		s.logger.Error().Err(err).Msg("failed to list executions")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to list executions")
		return
	}

	// Same Count fallback as ListTasks — see comment there.
	total := len(executions)
	if c, cerr := s.executionRepo.Count(r.Context(), filter); cerr != nil {
		s.logger.Warn().Err(cerr).Msg("execution count failed; falling back to page length for Total")
	} else {
		total = int(c)
	}

	// Build response
	resp := ListExecutionsResponse{
		Executions: make([]GetExecutionResponse, 0, len(executions)),
		Total:      total,
	}

	for _, exec := range executions {
		resp.Executions = append(resp.Executions, buildExecutionResponse(exec))
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		return
	}
}

// buildExecutionResponse builds an execution response from an execution model.
func buildExecutionResponse(exec *persistence.Execution) GetExecutionResponse {
	resp := GetExecutionResponse{
		ExecutionID: exec.ID,
		TaskID:      exec.TaskID,
		ProjectID:   exec.ProjectID,
		WorkflowID:  exec.WorkflowID,
		Status:      string(exec.Status),
		Links:       make(map[string]string),
	}

	if exec.CurrentStepID != nil {
		resp.CurrentStepID = *exec.CurrentStepID
	}
	if len(exec.CompletedSteps) > 0 {
		resp.CompletedSteps = exec.CompletedSteps
	}
	if exec.ErrorMessage != nil {
		resp.ErrorMessage = *exec.ErrorMessage
	}
	if exec.ErrorCode != nil {
		resp.ErrorCode = *exec.ErrorCode
	}
	if exec.StartedAt != nil {
		resp.StartedAt = exec.StartedAt.Format(time.RFC3339)
	}
	if exec.CompletedAt != nil {
		resp.CompletedAt = exec.CompletedAt.Format(time.RFC3339)
	}

	// Calculate duration if we have start time
	if exec.StartedAt != nil {
		endTime := time.Now()
		if exec.CompletedAt != nil {
			endTime = *exec.CompletedAt
		}
		duration := endTime.Sub(*exec.StartedAt)
		resp.Duration = duration.String()
	}

	// Add links
	resp.Links["task"] = "/api/v1/projects/" + exec.ProjectID + "/tasks/" + exec.TaskID

	// Copy the persisted result blob as json.RawMessage so clients see
	// the already-JSON-encoded object rather than a re-escaped string.
	if len(exec.Result) > 0 {
		resp.Result = json.RawMessage(exec.Result)
	}

	return resp
}

// ListMCPTools handles GET /projects/{projectId}/mcp/tools
//
// Returns the OpenAI-format tool catalog that the project's MCP servers
// advertise, post-allowlist filter. Agents use this via mcp-bridge's
// HTTP mode so they don't need to spawn their own MCP subprocesses
// (which wouldn't work anyway — agent containers lack node/credentials).
func (s *Server) ListMCPTools(w http.ResponseWriter, r *http.Request) {
	projectID := extractProjectID(r)
	if projectID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "projectId is required")
		return
	}
	// Project scope is enforced by ProjectAuthMiddleware on routes
	// matching /api/v1/projects/{id}/... — the duplicate explicit
	// check that used to live here was an open invitation to drift
	// from the middleware (different error message, different code,
	// different logic). Removed in the 2026-05-03 audit cleanup;
	// execution-scoped handlers below KEEP their explicit check
	// because their URL has no /projects/{id} segment for the
	// middleware to gate on.
	if s.mcpExecutor == nil {
		respondJSON(w, http.StatusOK, map[string]any{"tools": []any{}})
		return
	}
	tools := s.mcpExecutor.Tools(projectID)
	respondJSON(w, http.StatusOK, map[string]any{"tools": tools})
}

// CallMCPToolRequest is the request body for POST /projects/{id}/mcp/tools/call.
type CallMCPToolRequest struct {
	// Name is the qualified tool name (mcp__{server}__{tool}) returned
	// by ListMCPTools.
	Name string `json:"name"`
	// Arguments is the tool-specific JSON payload, passed through to the
	// MCP server unchanged.
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// CallMCPToolResponse is the response from a successful tool call.
type CallMCPToolResponse struct {
	// Text is the concatenated text content from the MCP tool result.
	Text string `json:"text"`
}

// CallMCPTool handles POST /projects/{projectId}/mcp/tools/call
//
// Proxies a tool invocation to the project's MCP server. The server name
// is parsed from the qualified name; a call with a server that isn't
// part of this project's config is rejected with 404 (not a routing
// scope violation), so cross-project leakage is structurally impossible.
func (s *Server) CallMCPTool(w http.ResponseWriter, r *http.Request) {
	projectID := extractProjectID(r)
	if projectID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "projectId is required")
		return
	}
	// Project scope is gated by ProjectAuthMiddleware — see the
	// note on ListMCPTools above.
	if s.mcpExecutor == nil {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "MCP is not configured")
		return
	}

	var req CallMCPToolRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxMCPArgsBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request body")
		return
	}
	if req.Name == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "name is required")
		return
	}

	args := string(req.Arguments)
	if args == "" {
		args = "{}"
	}

	// Forward the agent's task / execution origin so the broker
	// MCP can stamp trading_orders.task_id / execution_id. The
	// mcp-bridge running inside the agent container sets these
	// headers from VORNIK_TASK_ID / VORNIK_EXECUTION_ID. The
	// mcp.Client.CallTool implementation reads them back from
	// ctx and re-attaches as outbound headers on the upstream
	// MCP server request. Empty values are a no-op.
	ctx := r.Context()
	taskID := r.Header.Get("X-Task-ID")
	if id := IdentityFromContext(ctx); id != nil {
		if row, ok := id.Extra[auth.ExtraDBKeyRow].(*persistence.APIKey); ok {
			if boundTaskID, isTaskKey := persistence.TaskIDFromKeyName(row.Name); isTaskKey {
				if taskID != "" && taskID != boundTaskID {
					respondError(w, http.StatusForbidden, "FORBIDDEN", "X-Task-ID does not match the task-scoped API key")
					return
				}
				taskID = boundTaskID
			}
		}
	}
	if taskID != "" {
		ctx = context.WithValue(ctx, mcp.TaskIDHeaderKey{}, taskID)
	}
	if v := r.Header.Get("X-Execution-ID"); v != "" {
		ctx = context.WithValue(ctx, mcp.ExecutionIDHeaderKey{}, v)
	}

	// Finding B2: enforce the calling role's allowedTools SERVER-SIDE.
	// SwarmRole.AllowedTools was previously applied only via the
	// container-side mcp.json, and per-task keys bind project (not
	// role), so a compromised narrow-role agent could invoke any tool
	// the project enables (e.g. broker place_order). Intersect the
	// role's allowlist here; refuse 403 when the tool isn't permitted.
	// Roles that declare no allowedTools are unrestricted (preserves
	// pre-B2 behavior so non-opted-in roles don't break).
	if !s.roleAllowsMCPTool(ctx, taskID, req.Name) {
		respondError(w, http.StatusForbidden, "FORBIDDEN", "tool not in the calling role's allowedTools")
		return
	}

	// Phase C v2 counterfactual gate (deny-by-default): if the
	// caller's task is a replay, intercept tool calls before they
	// reach the real MCP server. Operator-supplied stubs win;
	// otherwise only replay-safe (allow-listed) tools the original
	// trace called run live — everything else gets a synthesized
	// "skipped" response. Non-counterfactual tasks pass through
	// unchanged.
	if gate, gateErr := s.applyCounterfactualMCPGate(ctx, taskID, req.Name); gateErr == nil && gate.HandledLocally {
		respondJSON(w, http.StatusOK, CallMCPToolResponse{Text: gate.Text})
		return
	} else if gateErr != nil {
		// Never fail open: a task lookup outage must not turn a
		// counterfactual replay into a real-world side effect.
		s.logger.Warn().Err(gateErr).Str("task_id", taskID).Msg("counterfactual gate: lookup failed; blocking dispatch")
		respondError(w, http.StatusServiceUnavailable, "COUNTERFACTUAL_GATE_UNAVAILABLE", "counterfactual safety gate unavailable")
		return
	}

	text, err := s.mcpExecutor.Execute(ctx, projectID, req.Name, args)
	if err != nil {
		// Include err text so agents can surface the root cause (e.g.
		// "tool not in allowed_tools" or "MCP server not connected") —
		// these aren't sensitive and are essential for debugging.
		respondError(w, http.StatusBadGateway, "MCP_ERROR", err.Error())
		return
	}
	respondJSON(w, http.StatusOK, CallMCPToolResponse{Text: text})
}

// roleAllowsMCPTool reports whether the task's current role is permitted
// to invoke qualifiedName, enforcing SwarmRole.AllowedTools server-side
// (Finding B2). It resolves task → execution.CurrentStepID → workflow
// step → step.Role → swarm role, then checks the role's allowlist.
//
// FAIL-OPEN by design on every resolution gap (no taskID, no
// execution/workflow/registry wired, role not found, or role declares no
// allowedTools): the daemon's project gate (ProjectAuthMiddleware) and
// the container-side mcp.json filter remain in force, and B2 only adds a
// SECOND server-side intersection for roles that opt in. Returning true
// on a gap avoids breaking deployments that don't declare allowedTools
// or run without the registry/execution repos (sqlite/dev). The
// allowlist matches the fully-qualified tool name (mcp__server__tool) as
// authored in swarm config, falling back to the bare tool segment so an
// operator can allowlist either shape.
func (s *Server) roleAllowsMCPTool(ctx context.Context, taskID, qualifiedName string) bool {
	if taskID == "" || s.executionRepo == nil || s.projectRegistry == nil {
		return true
	}
	exec, err := s.executionRepo.GetByTaskID(ctx, taskID)
	if err != nil || exec == nil || exec.CurrentStepID == nil || *exec.CurrentStepID == "" {
		return true
	}
	project, workflow, err := s.projectRegistry.GetProjectWithWorkflow(exec.ProjectID)
	if err != nil || project == nil || workflow == nil {
		return true
	}
	step, ok := workflow.Steps[*exec.CurrentStepID]
	if !ok || step.Role == "" {
		return true
	}
	_, swarm, err := s.projectRegistry.GetProjectWithSwarm(exec.ProjectID)
	if err != nil || swarm == nil {
		return true
	}
	var allowed []string
	found := false
	for i := range swarm.Roles {
		if swarm.Roles[i].Name == step.Role {
			allowed = swarm.Roles[i].Permissions.AllowedTools
			found = true
			break
		}
	}
	// Role not declared in the swarm, or declared with no allowedTools
	// → unrestricted (preserve pre-B2 behavior).
	if !found || len(allowed) == 0 {
		return true
	}
	return mcpRoleToolAllowed(allowed, qualifiedName)
}

// mcpRoleToolAllowed applies a resolved (non-empty) role allowlist to one
// requested tool. Pure (no I/O) so the policy is unit-testable without
// standing up task → execution → workflow → role resolution.
//
// CLOSED-WORLD (B2 authorization gate): a role with a non-empty allowlist may
// invoke only the tools it lists. MCP-qualified tools (mcp__server__tool) must
// be granted EXPLICITLY — by exact name, by bare segment, or by a wildcard the
// operator writes deliberately:
//
//	mcp__*           → any MCP tool (defer MCP gating to the project layer:
//	                   permissions.allowedTools + the MCP server's allowed_tools)
//	mcp__server__*   → any tool of one MCP server
//
// There is NO fail-open by omission: listing only built-in tools does NOT
// grant MCP tools (that would let a deliberately-narrow role reach, e.g.,
// broker place_order whenever the project enables it). A role that should use
// project MCP tools declares that intent with mcp__* or the specific tools.
//
// Trading roles keep the strict intersection (listing mcp__broker__get_quote
// still denies mcp__broker__place_order). Regression context: the janka
// `researcher` listed only built-in tools and so could not call
// mcp__scraper__web_fetch — every portal scan got a daemon-level FORBIDDEN,
// starving the RAG (2026-06-20). The fix is to GRANT the tool in the role
// config (here + the distributed swarm presets), not to fail open.
func mcpRoleToolAllowed(allowed []string, qualifiedName string) bool {
	// Bare tool segment (after the last "__") so an allowlist authored as
	// either the qualified or bare name both match.
	bare := qualifiedName
	if idx := strings.LastIndex(qualifiedName, "__"); idx >= 0 {
		bare = qualifiedName[idx+2:]
	}
	isMCP := strings.HasPrefix(qualifiedName, "mcp__")
	for _, a := range allowed {
		if a == qualifiedName || a == bare {
			return true
		}
		if isMCP && mcpWildcardMatch(a, qualifiedName) {
			return true
		}
	}
	return false
}

// mcpWildcardMatch reports whether an allowlist entry is an MCP wildcard that
// covers qualifiedName (already known to start with "mcp__"). Supported:
// "mcp__*" (all MCP tools) and "mcp__<server>__*" (one server's tools). The
// operator must write the wildcard explicitly — absence never grants.
func mcpWildcardMatch(entry, qualifiedName string) bool {
	if entry == "mcp__*" {
		return true
	}
	if prefix, ok := strings.CutSuffix(entry, "*"); ok {
		// e.g. entry "mcp__broker__*" → prefix "mcp__broker__"
		if strings.HasPrefix(prefix, "mcp__") && strings.HasPrefix(qualifiedName, prefix) {
			return true
		}
	}
	return false
}

// Healthz handles GET /healthz
func (s *Server) Healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// Livez handles GET /livez. Always returns 200 while the process is up,
// even during graceful shutdown drain. This is the k8s liveness probe
// contract: "is the container alive enough to keep running, or should
// the orchestrator kill -9 me?" — distinct from /readyz which signals
// "should the load balancer keep routing new requests to me?".
//
// During drain, /livez stays 200 (we are still alive, just refusing new
// work) while /readyz flips to 503. That split lets a kubernetes
// Deployment finish in-flight requests before the pod is terminated:
// the LB stops routing, the readyz probe trips before the liveness one
// would, and the SIGTERM-initiated shutdown gets its full grace window.
func (s *Server) Livez(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"alive"}`))
}

// SetDraining flips the drain bit. Called from the SIGTERM handler in
// container.Run before invoking shutdown() — once true, /readyz returns
// 503 with {"status":"draining"} so load balancers stop sending new
// requests during the grace window. There is no setter for false:
// graceful shutdown is one-way (the next process start clears the bit
// because it's a fresh in-memory flag).
func (s *Server) SetDraining(v bool) {
	s.draining.Store(v)
}

// IsDraining reports whether the daemon has begun graceful shutdown.
// Exposed for tests + the admin UI's landing-page tile.
func (s *Server) IsDraining() bool {
	return s.draining.Load()
}

// ReadinessResult is one entry in the in-process Readiness probe
// output. Mirrors the public /readyz JSON shape so the admin UI's
// landing tile can render the same data without self-HTTP-calling
// the daemon. Error carries the public-safe "check failed" string
// — the verbose original lands in the daemon log only.
type ReadinessResult struct {
	Name   string
	Status string // "ok" | "error"
	Error  string
}

// RunReadiness executes the registered checks in-process and returns
// the same name/status/error triples /readyz emits over HTTP. The
// caller's context bounds the runtime — typically 3 s, the same
// deadline /readyz uses. Exported for the admin UI; the handler
// route still uses its own implementation so the public response
// stays byte-for-byte stable.
func (s *Server) RunReadiness(ctx context.Context) []ReadinessResult {
	out := make([]ReadinessResult, 0, 1+len(s.readinessChecks))
	if s.taskRepo != nil {
		res := ReadinessResult{Name: "database", Status: "ok"}
		if err := s.taskRepo.Ping(ctx); err != nil {
			res.Status = "error"
			res.Error = "check failed"
		}
		out = append(out, res)
	}
	for _, c := range s.readinessChecks {
		res := ReadinessResult{Name: c.Name, Status: "ok"}
		if err := c.Check(ctx); err != nil {
			res.Status = "error"
			res.Error = "check failed"
		}
		out = append(out, res)
	}
	return out
}

// Readyz handles GET /readyz. Runs every registered ReadinessCheck plus
// the always-on DB ping, under a 3 s overall deadline. Returns 503 the
// moment any check fails, with a per-check JSON report so operators and
// probes can see which dependency is wedged. Backwards-compatible body
// shape: the success path still writes `{"status":"ready"}` and the
// structured details land in a sibling "checks" field.
func (s *Server) Readyz(w http.ResponseWriter, r *http.Request) {
	// Drain short-circuit. Once SIGTERM flips the bit, every /readyz
	// returns 503 with {"status":"draining"} so the LB stops routing
	// new traffic before we tear down DB connections + leader locks.
	// We skip the registered checks entirely — they may already be
	// disconnecting, and the operator's question here is "should you
	// route to me?" (answer: no), not "is your DB up?".
	if s.draining.Load() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"draining"}`))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	type checkResult struct {
		Name   string `json:"name"`
		Status string `json:"status"` // "ok" | "error"
		Error  string `json:"error,omitempty"`
	}

	results := make([]checkResult, 0, 1+len(s.readinessChecks))
	// detailedFailures captures the verbatim err.Error() for the
	// internal log only — the public response carries a generic
	// "check failed" string so the unauthenticated /readyz endpoint
	// doesn't leak DB hostnames, auth-error text, or other internal
	// dependency detail to network probes.
	type detailedFailure struct {
		Name  string `json:"name"`
		Error string `json:"error"`
	}
	var failures []detailedFailure
	allOK := true

	// DB ping stays the baseline. If the repo isn't wired we skip the
	// ping but keep serving 200 — tests and direct-handler callers
	// don't always set it.
	if s.taskRepo != nil {
		res := checkResult{Name: "database", Status: "ok"}
		if err := s.taskRepo.Ping(ctx); err != nil {
			res.Status = "error"
			res.Error = "check failed"
			failures = append(failures, detailedFailure{Name: res.Name, Error: err.Error()})
			allOK = false
		}
		results = append(results, res)
	}

	for _, c := range s.readinessChecks {
		res := checkResult{Name: c.Name, Status: "ok"}
		if err := c.Check(ctx); err != nil {
			res.Status = "error"
			res.Error = "check failed"
			failures = append(failures, detailedFailure{Name: res.Name, Error: err.Error()})
			allOK = false
		}
		results = append(results, res)
	}

	status := http.StatusOK
	body := map[string]any{
		"status": "ready",
		"checks": results,
	}
	if !allOK {
		status = http.StatusServiceUnavailable
		body["status"] = "not-ready"
		s.logger.Error().Interface("failures", failures).Msg("readiness check failed")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// Helper functions

// validPathID is the charset every path-segment ID must match. Project,
// task, execution, and workflow IDs are operator- or daemon-generated
// identifiers — `task_<ts>_<hex>`, kebab-case project names, etc. —
// that never legitimately contain `..`, `/`, whitespace, or control
// chars. The path extractors below reject anything else as defense in
// depth: Go's ServeMux already cleans `..` before reaching us, but a
// future router swap (or a handler that builds filesystem / shell
// arguments from an ID) shouldn't have to re-derive that guarantee.
const maxPathIDLen = 128

func isValidPathID(s string) bool {
	if s == "" || len(s) > maxPathIDLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z':
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case c == '-' || c == '_':
		default:
			return false
		}
	}
	return true
}

func extractPathSegmentAfter(r *http.Request, marker string) string {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == marker && parts[i+1] != "" {
			id := parts[i+1]
			if !isValidPathID(id) {
				return ""
			}
			return id
		}
	}
	return ""
}

// extractProjectID resolves the project an incoming request
// scopes to, when the project ID is structurally encoded in the
// URL path. Used by ProjectAuthMiddleware to enforce scope at
// the URL layer for `/api/v1/projects/{id}/...` and the A2A
// agent surface.
//
// What this DOESN'T cover, intentionally:
//   - /ui/tasks/{id}, /ui/executions/{id}, /ui/artifacts/{id}.
//     The path carries an opaque task/exec/artifact ID, not a
//     project. Resolving project_id from those would require a
//     DB round-trip per request — too expensive on SSE streams +
//     status-pill polling.
//   - /api/v1/tasks/{id}, /api/v1/executions/{id}, etc. — same
//     reasoning.
//
// 2026-05-27 audit (issue #1): the gap above used to be the
// only line of defense, leaving every "by ID" handler exposed
// to IDOR. The fix is row-level: every cited handler now calls
// api.RequestAllowsProject(r, row.ProjectID) immediately after
// loading the row (task_detail / task_actions / task_logs /
// task_live / sse_handler / task_postmortem / execution_detail /
// execution_actions / artifact). Regression tests live in
// internal/ui/idor_regression_test.go. This middleware remains
// the URL-layer defense-in-depth for the surfaces it does
// cover; extending it to resolve IDs is parked unless the
// per-request DB cost is judged acceptable.
func extractProjectID(r *http.Request) string {
	if id := extractPathSegmentAfter(r, "projects"); id != "" {
		return id
	}
	if strings.HasPrefix(r.URL.Path, "/a2a/v1/agents/") {
		return extractPathSegmentAfter(r, "agents")
	}
	return ""
}

func extractTaskID(r *http.Request) string {
	return extractPathSegmentAfter(r, "tasks")
}

func extractExecutionID(r *http.Request) string {
	return extractPathSegmentAfter(r, "executions")
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func buildCreateTaskResponse(task *persistence.Task) CreateTaskResponse {
	return CreateTaskResponse{
		TaskID: task.ID,
		Status: string(task.Status),
		Links: map[string]string{
			"status": "/api/v1/projects/" + task.ProjectID + "/tasks/" + task.ID,
		},
	}
}

// isKnownExecutionStatus is the ExecutionStatus sibling of
// isKnownTaskStatus; see that comment for rationale.
func isKnownExecutionStatus(v persistence.ExecutionStatus) bool {
	switch v {
	case persistence.ExecutionStatusPending,
		persistence.ExecutionStatusRunning,
		persistence.ExecutionStatusPaused,
		persistence.ExecutionStatusCompleted,
		persistence.ExecutionStatusFailed,
		persistence.ExecutionStatusCancelled:
		return true
	}
	return false
}

// isKnownTaskStatus reports whether v is one of the TaskStatus enum
// values the persistence layer accepts. Used by handlers that take a
// status filter so a typo turns into a clean 400 instead of a 500 from
// the DB check-constraint.
func isKnownTaskStatus(v persistence.TaskStatus) bool {
	switch v {
	case persistence.TaskStatusPending,
		persistence.TaskStatusQueued,
		persistence.TaskStatusLeased,
		persistence.TaskStatusRunning,
		persistence.TaskStatusWaitingForChildren,
		persistence.TaskStatusCompleted,
		persistence.TaskStatusFailed,
		persistence.TaskStatusCancelled:
		return true
	}
	return false
}

// marshalTaskPayload serialises a request struct for the task's Payload
// column. Returns the error rather than panicking: while the call sites
// here only feed tagged structs (so Marshal can't fail today), promoting
// a programmer bug to an end-of-world panic — even one Go's net/http
// would catch and isolate to the failing connection — is the wrong
// default. A 500 with the marshalling error logged surfaces the bug
// without dropping every other in-flight request on the same connection.
func marshalTaskPayload(v interface{}) ([]byte, error) {
	return json.Marshal(v)
}

func parseIntParam(s string, def int) int {
	if s == "" {
		return def
	}
	var result int
	if _, err := fmt.Sscanf(s, "%d", &result); err != nil {
		return def
	}
	return result
}

// maxPageSize caps the per-page row count callers can request. The DB
// repositories pass PageSize straight through to LIMIT, and a 0/negative
// value would omit the clause entirely (returning every row), while a
// huge value would buffer an unbounded result set into memory. Both are
// real failure modes — clamp here so handlers don't have to repeat the
// guard.
const maxPageSize = 200

func parsePageSize(s string, def int) int {
	v := parseIntParam(s, def)
	if v <= 0 {
		return def
	}
	if v > maxPageSize {
		return maxPageSize
	}
	return v
}

func parseOffset(s string) int {
	v := parseIntParam(s, 0)
	if v < 0 {
		return 0
	}
	return v
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, maxBytes int64, dst interface{}) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("request body contains multiple JSON values")
		}
		return fmt.Errorf("request body contains multiple JSON values")
	}
	return nil
}

func respondJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		return
	}
}

// ForkExecutionFromStep handles POST
// /api/v1/executions/{executionId}/fork-from-step. Body:
//
//	{"step_id": "summarise", "prompt_override": "..."}
//
// Creates a NEW task that, when leased and executed, will start
// at the chosen step with the prompt override applied on the
// first iteration. The source execution is preserved untouched —
// fork is the "branch" to retry-from-step's "rewind".
//
// Refusals:
//   - executor / forker not wired → 503 INTERNAL_ERROR (feature off)
//   - missing step_id → 400 VALIDATION_ERROR
//   - source execution not found → 404 NOT_FOUND
//   - caller can't see the source's project → 403 FORBIDDEN
//   - chosen step has no recorded outcome → 400 VALIDATION_ERROR
func (s *Server) ForkExecutionFromStep(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Method not allowed")
		return
	}
	executionID := extractExecutionID(r)
	if executionID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "executionId is required")
		return
	}
	if s.forker == nil {
		respondError(w, http.StatusServiceUnavailable, "FORK_DISABLED",
			"fork-from-step is not wired on this deployment")
		return
	}
	if s.executionRepo == nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Execution repository not available")
		return
	}

	var body ForkExecutorRequest
	if err := decodeJSONBody(w, r, maxOptionalBodyBytes, &body); err != nil {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "request body must be JSON: "+err.Error())
		return
	}
	body.StepID = strings.TrimSpace(body.StepID)
	if body.StepID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "step_id is required")
		return
	}

	// Project authz: caller's API key must include the source's project.
	exec, err := s.executionRepo.Get(r.Context(), executionID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
			respondError(w, http.StatusNotFound, "NOT_FOUND", "Execution not found")
			return
		}
		s.logger.Error().Err(err).Str("executionId", executionID).Msg("fork-from-step: failed to load execution")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to load execution")
		return
	}
	if exec == nil {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "Execution not found")
		return
	}
	if !requestAllowsProject(r, exec.ProjectID) {
		respondError(w, http.StatusForbidden, "FORBIDDEN", "Access denied to project")
		return
	}

	result, err := s.forker.Fork(r.Context(), executionID, body)
	if err != nil {
		// The Forker exposes typed sentinels via the replay
		// package; the API package's narrow interface returns
		// plain errors. Use string-match on the sentinel-style
		// messages to translate. The replay package's text is
		// stable and tested.
		msg := err.Error()
		switch {
		case strings.Contains(msg, "fork source execution not found"):
			respondError(w, http.StatusNotFound, "NOT_FOUND", "Execution not found")
			return
		case strings.Contains(msg, "fork step has no recorded outcome"):
			respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
			return
		case strings.Contains(msg, "fork request invalid"):
			respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
			return
		}
		s.logger.Error().Err(err).
			Str("executionId", executionID).
			Str("stepId", body.StepID).
			Msg("fork-from-step: failed")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to fork execution from step")
		return
	}

	respondJSON(w, http.StatusCreated, result)
}

// ProjectWizardConverseRequest is the operator-supplied shape on
// POST /api/v1/projects/wizard/converse. session_id is empty on
// the first turn; subsequent turns include the value returned by
// the previous call.
type ProjectWizardConverseRequest struct {
	SessionID string `json:"session_id,omitempty"`
	Message   string `json:"message"`
}

// ProjectWizardConverse handles the conversational project-setup
// wizard. Each call is one turn — the operator's message is
// appended to the session transcript, the LLM is invoked with the
// envelope schema, and the resulting envelope is returned + the
// session row updated.
//
// Refusals:
//   - wizard unwired → 503 WIZARD_DISABLED
//   - missing message → 400 VALIDATION_ERROR
//   - committed session → 410 GONE (session is read-only)
//   - turn cap hit → 429 RATE_LIMITED
//   - LLM / persistence error → 502/500 with detail
func (s *Server) ProjectWizardConverse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Method not allowed")
		return
	}
	if s.projectWizard == nil {
		respondError(w, http.StatusServiceUnavailable, "WIZARD_DISABLED",
			"project wizard not wired on this deployment")
		return
	}

	var body ProjectWizardConverseRequest
	if err := decodeJSONBody(w, r, maxOptionalBodyBytes, &body); err != nil {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR",
			"request body must be JSON: "+err.Error())
		return
	}
	body.Message = strings.TrimSpace(body.Message)
	if body.Message == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "message is required")
		return
	}

	operatorID := RequestOperatorIDOrSingleTenant(r, SingleTenantOperatorIDFromConfig(s.config))
	if operatorID == "" {
		respondError(w, http.StatusUnauthorized, "UNAUTHORIZED",
			"operator identity required (provide an API key or admin Telegram session)")
		return
	}

	result, err := s.projectWizard.Converse(r.Context(), body.SessionID, operatorID, body.Message)
	if err != nil {
		msg := err.Error()
		switch {
		case strings.Contains(msg, "session already committed"):
			respondError(w, http.StatusGone, "SESSION_COMMITTED",
				"session already committed; start a new wizard session")
			return
		case strings.Contains(msg, "turn limit reached"):
			respondError(w, http.StatusTooManyRequests, "TURN_LIMIT",
				"wizard session reached its turn cap; start a new session")
			return
		case strings.Contains(msg, "empty user message"),
			strings.Contains(msg, "operator id required"):
			respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
			return
		case strings.Contains(msg, "not found"):
			respondError(w, http.StatusNotFound, "NOT_FOUND", "session not found")
			return
		}
		s.logger.Error().Err(err).
			Str("session_id", body.SessionID).
			Msg("project-wizard converse failed")
		respondError(w, http.StatusBadGateway, "WIZARD_ERROR", "wizard turn failed; see server logs")
		return
	}

	respondJSON(w, http.StatusOK, result)
}

// requestOperatorID returns the caller's identity for use as the
// operator_id on wizard sessions and any other surface that gates
// access by caller identity. Sources in priority order:
//   - the matched API key's hash (from auth middleware context).
//     This is the only path that produces a *verified* identity.
//   - the X-Operator-Id header — but ONLY when the deployment is
//     running with auth disabled (single-operator dev mode). When
//     auth is enabled, the header is ignored: an attacker who
//     reaches a handler past auth has no business overriding
//     their identity with a self-asserted value.
//   - empty string — the handler decides whether to reject (401)
//     or treat as anonymous depending on context.
//
// SECURITY: the header path historically allowed any caller to
// impersonate any operator by sending `X-Operator-Id: <victim>`.
// On a deployment with auth enabled the matched API key is the
// canonical identity, so allowing the header to override it lets
// any authenticated caller take over another operator's wizard
// sessions. The auth-enabled gate below closes that path.
func requestOperatorID(r *http.Request) string {
	if p := apiKeyPrincipalFromContext(r.Context()); p != "" {
		return p
	}
	// Header fallback only when auth is explicitly disabled. The
	// auth middleware stamps the flag on context for every
	// request; absence is treated as enabled (fail-closed) so a
	// path that bypasses the middleware can't accidentally
	// resurrect the spoofable header path.
	enabled, ok := r.Context().Value(authEnabledKey).(bool)
	if !ok || enabled {
		return ""
	}
	if h := strings.TrimSpace(r.Header.Get("X-Operator-Id")); h != "" {
		return h
	}
	return ""
}

// ProjectWizardCommit handles POST
// /api/v1/projects/wizard/{session_id}/commit. Finalises the
// session: re-validates the proposal, writes it as a real project
// via the existing ingestion path, stamps the session terminal,
// returns the new project ID + redirect URL.
//
// Refusals:
//   - wizard unwired → 503 WIZARD_DISABLED
//   - missing session_id in path → 400
//   - session not found → 404
//   - cross-operator (caller not the owner) → 404 (no leak)
//   - not ready to commit → 409 NOT_READY
//   - no proposal yet → 409 NO_PROPOSAL
//   - validation failure on re-check → 422 VALIDATION_ERROR
//   - writer unwired → 503 WRITER_DISABLED
//   - writer error (e.g. project ID collision) → 409 CONFLICT
//
// Idempotent on a re-click: a session that's already committed
// returns 200 with the existing project's URL.
func (s *Server) ProjectWizardCommit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Method not allowed")
		return
	}
	if s.projectWizard == nil {
		respondError(w, http.StatusServiceUnavailable, "WIZARD_DISABLED",
			"project wizard not wired on this deployment")
		return
	}
	sessionID := extractPathSegmentAfter(r, "wizard")
	sessionID = strings.TrimSuffix(sessionID, "/commit")
	if sessionID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "session_id is required")
		return
	}
	operatorID := RequestOperatorIDOrSingleTenant(r, SingleTenantOperatorIDFromConfig(s.config))
	if operatorID == "" {
		respondError(w, http.StatusUnauthorized, "UNAUTHORIZED",
			"operator identity required (provide an API key or admin Telegram session)")
		return
	}

	result, err := s.projectWizard.Commit(r.Context(), sessionID, operatorID)
	if err != nil {
		msg := err.Error()
		switch {
		case strings.Contains(msg, "not ready to commit"):
			respondError(w, http.StatusConflict, "NOT_READY",
				"session is not ready to commit; keep chatting until the wizard signals ready")
			return
		case strings.Contains(msg, "session has no proposal"):
			respondError(w, http.StatusConflict, "NO_PROPOSAL",
				"session has no proposal yet")
			return
		case strings.Contains(msg, "re-validation failed"):
			respondError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", err.Error())
			return
		case strings.Contains(msg, "project writer not wired"):
			respondError(w, http.StatusServiceUnavailable, "WRITER_DISABLED",
				"project writer not wired on this deployment")
			return
		case strings.Contains(msg, "not found"):
			respondError(w, http.StatusNotFound, "NOT_FOUND", "session not found")
			return
		case strings.Contains(msg, "write project") && strings.Contains(msg, "already exists"):
			respondError(w, http.StatusConflict, "PROJECT_EXISTS",
				"a project with that id already exists; ask the wizard to pick a different id")
			return
		}
		s.logger.Error().Err(err).Str("session_id", sessionID).Msg("project-wizard commit failed")
		respondError(w, http.StatusInternalServerError, "WIZARD_ERROR", "commit failed; see server logs")
		return
	}

	respondJSON(w, http.StatusOK, result)
}

// ProjectWizardCancel terminally cancels an in-progress wizard
// session, freeing the operator's active-session slot. POST-only;
// mirrors ProjectWizardCommit's operator resolution + error shape.
// Cancelling an already-cancelled session is an idempotent 200.
func (s *Server) ProjectWizardCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Method not allowed")
		return
	}
	if s.projectWizard == nil {
		respondError(w, http.StatusServiceUnavailable, "WIZARD_DISABLED",
			"project wizard not wired on this deployment")
		return
	}
	sessionID := extractPathSegmentAfter(r, "wizard")
	sessionID = strings.TrimSuffix(sessionID, "/cancel")
	if sessionID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "session_id is required")
		return
	}
	operatorID := RequestOperatorIDOrSingleTenant(r, SingleTenantOperatorIDFromConfig(s.config))
	if operatorID == "" {
		respondError(w, http.StatusUnauthorized, "UNAUTHORIZED",
			"operator identity required (provide an API key or admin Telegram session)")
		return
	}

	if err := s.projectWizard.Cancel(r.Context(), sessionID, operatorID); err != nil {
		msg := err.Error()
		switch {
		case strings.Contains(msg, "already committed") || strings.Contains(msg, "invalid transition"):
			respondError(w, http.StatusConflict, "ALREADY_COMMITTED",
				"session already committed; cannot cancel")
			return
		case strings.Contains(msg, "not found"):
			respondError(w, http.StatusNotFound, "NOT_FOUND", "session not found")
			return
		}
		s.logger.Error().Err(err).Str("session_id", sessionID).Msg("project-wizard cancel failed")
		respondError(w, http.StatusInternalServerError, "WIZARD_ERROR", "cancel failed; see server logs")
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{
		"session_id": sessionID,
		"status":     "cancelled",
	})
}

// projectWizardRouter dispatches /api/v1/projects/wizard/{...}
// paths to the appropriate wizard handler. /converse is exact-
// matched separately on the mux; this prefix router handles
// /{session_id}/commit and /{session_id}/cancel (the wildcard
// paths under wizard/).
//
// Unknown paths under /wizard/ return 404 so a future bad URL
// doesn't accidentally bind to /converse.
func (s *Server) projectWizardRouter(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/projects/wizard/")
	// /converse — handled by the exact-match route above, but a
	// caller hitting the trailing-slash variant lands here.
	if path == "converse" || path == "converse/" {
		s.ProjectWizardConverse(w, r)
		return
	}
	if strings.HasSuffix(path, "/commit") || strings.HasSuffix(path, "/commit/") {
		s.ProjectWizardCommit(w, r)
		return
	}
	if strings.HasSuffix(path, "/cancel") || strings.HasSuffix(path, "/cancel/") {
		s.ProjectWizardCancel(w, r)
		return
	}
	http.NotFound(w, r)
}
