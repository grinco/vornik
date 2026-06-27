package replay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ForkTargetPayloadKey is the JSON field name the executor looks
// up on the new task's payload to detect a fork. Stored on the
// task (not the execution) because the executor creates the
// execution row when it leases the task; the payload is the only
// channel that survives the queue/lease boundary.
const ForkTargetPayloadKey = "fork_target"

// ForkTarget is the payload envelope carrying the fork metadata.
// Marshalled at fork time, unmarshalled by the executor on lease.
//
// The three fields exactly mirror the future Execution columns
// (parent_execution_id, forked_from_step_id, forked_prompt_override)
// so the executor can copy them across without translation.
type ForkTarget struct {
	SourceExecutionID string `json:"source_execution_id"`
	StepID            string `json:"step_id"`
	PromptOverride    string `json:"prompt_override,omitempty"`
}

// ForkRequest is the operator-supplied shape arriving on
// POST /api/v1/executions/{id}/fork-from-step.
type ForkRequest struct {
	StepID         string `json:"step_id"`
	PromptOverride string `json:"prompt_override,omitempty"`
}

// ForkResult is the response shape the API returns: the new
// task's ID + the URL the operator should redirect to.
type ForkResult struct {
	TaskID string `json:"task_id"`
	URL    string `json:"url"`
}

// Common errors the API handler maps to specific HTTP statuses.
var (
	// ErrForkSourceNotFound — source execution doesn't exist (404).
	ErrForkSourceNotFound = errors.New("replay: fork source execution not found")
	// ErrForkStepMissing — the chosen step has no outcome row in the
	// source execution (400). v1 only allows forking from steps that
	// actually ran.
	ErrForkStepMissing = errors.New("replay: fork step has no recorded outcome")
	// ErrForkValidation — generic input-validation refusal (400).
	ErrForkValidation = errors.New("replay: fork request invalid")
)

// taskCreator is the narrow interface the Forker needs from the
// task repository. Mirrors the narrow-interface pattern used by
// the Builder so test fakes stay small.
type taskCreator interface {
	Create(ctx context.Context, task *persistence.Task) error
}

// auditInserter is the narrow interface the Forker uses to write
// one admin_audit row per fork. The full
// persistence.AdminAuditRepository carries List too; we only need
// Insert here. Nil disables audit logging (best-effort — never
// blocks the fork on audit write failure).
type auditInserter interface {
	Insert(ctx context.Context, entry *persistence.AdminAuditEntry) error
}

// Forker creates a new task pointing back at a source execution +
// step, with an optional operator-supplied prompt override. The
// new task goes through the normal queue/lease/executor flow; the
// executor reads the fork target from the payload and populates
// the new Execution's lineage columns.
//
// Forker is a thin coordinator — the heavy lifting (executor
// detecting fork in payload, applying override on the first
// iteration) lives in the executor.
type Forker struct {
	// Executions returns the source execution by ID. Same narrow
	// interface as the Builder's.
	Executions executionGetter
	// Outcomes lists step outcomes for the source so we can
	// validate the chosen step actually ran.
	Outcomes outcomeLister
	// Tasks creates the new (forked) task row.
	Tasks taskCreator
	// AuditAdmin records one admin_audit row per fork so the
	// /ui/admin/audit feed shows who forked what. Optional —
	// nil silently skips the write (the fork still succeeds).
	AuditAdmin auditInserter
	// Metrics counts fork outcomes for the spend dashboard /
	// alerting. Optional — nil is a no-op.
	Metrics *Metrics
	// IDGenerator produces the new task ID. Pluggable so tests can
	// inject deterministic IDs. Nil falls back to
	// persistence.GenerateID("task").
	IDGenerator func() string
	// Now returns the current time. Pluggable so tests can pin
	// timestamps. Nil falls back to time.Now().UTC.
	Now func() time.Time
}

// Fork outcome labels — kept as named constants so the Prometheus
// label set is one place to audit. Mirror the
// Metrics.ForksTotal comment block.
const (
	forkOutcomeCreated        = "created"
	forkOutcomeValidationFail = "validation_failed"
	forkOutcomeSourceNotFound = "source_not_found"
	forkOutcomeStepMissing    = "step_missing"
	forkOutcomeError          = "error"
)

// Fork validates the request, looks up the source, and creates
// the forked task. Returns the new task's ID + redirect URL.
//
// The source execution's project + workflow are inherited
// verbatim. The source task's max-attempt budget is also
// inherited so the fork has the same retry headroom (we look it
// up lazily — Forker doesn't take a TaskRepository because the
// max-attempt field rarely matters and a sensible default
// suffices when the source task isn't readable).
func (f *Forker) Fork(ctx context.Context, sourceExecutionID string, req ForkRequest) (*ForkResult, error) {
	if f == nil || f.Executions == nil || f.Outcomes == nil || f.Tasks == nil {
		return nil, errors.New("replay: forker not fully wired")
	}
	stepID := strings.TrimSpace(req.StepID)
	if stepID == "" {
		f.Metrics.recordForkOutcome(forkOutcomeValidationFail)
		return nil, fmt.Errorf("%w: step_id required", ErrForkValidation)
	}

	source, err := f.Executions.Get(ctx, sourceExecutionID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			f.Metrics.recordForkOutcome(forkOutcomeSourceNotFound)
			return nil, ErrForkSourceNotFound
		}
		f.Metrics.recordForkOutcome(forkOutcomeError)
		return nil, fmt.Errorf("replay: load source execution: %w", err)
	}
	if source == nil {
		f.Metrics.recordForkOutcome(forkOutcomeSourceNotFound)
		return nil, ErrForkSourceNotFound
	}

	// Validate the chosen step actually ran in the source. v1's
	// scope decision was clean re-execution from a step that has a
	// recorded outcome — forking from a not-yet-reached step is a
	// v2 relaxation.
	outcomes, err := f.Outcomes.List(ctx, persistence.ExecutionStepOutcomeFilter{
		ExecutionID: &sourceExecutionID,
	})
	if err != nil {
		f.Metrics.recordForkOutcome(forkOutcomeError)
		return nil, fmt.Errorf("replay: list source outcomes: %w", err)
	}
	if !hasStepOutcome(outcomes, stepID) {
		f.Metrics.recordForkOutcome(forkOutcomeStepMissing)
		return nil, fmt.Errorf("%w: step %q in execution %s", ErrForkStepMissing, stepID, sourceExecutionID)
	}

	// Build the fork-target payload envelope. Marshalled into the
	// new task's Payload alongside any task-context fields the
	// executor needs (none for fork v1 — the fork is the whole
	// task definition).
	target := ForkTarget{
		SourceExecutionID: sourceExecutionID,
		StepID:            stepID,
		PromptOverride:    strings.TrimSpace(req.PromptOverride),
	}
	payload, err := json.Marshal(map[string]any{
		ForkTargetPayloadKey: target,
		"taskType":           "fork",
	})
	if err != nil {
		return nil, fmt.Errorf("replay: marshal fork payload: %w", err)
	}

	now := time.Now().UTC()
	if f.Now != nil {
		now = f.Now()
	}
	newID := persistence.GenerateID("task")
	if f.IDGenerator != nil {
		newID = f.IDGenerator()
	}

	parentTaskID := source.TaskID
	workflowID := source.WorkflowID
	task := &persistence.Task{
		ID:             newID,
		ProjectID:      source.ProjectID,
		WorkflowID:     &workflowID,
		ParentTaskID:   &parentTaskID,
		CreationSource: persistence.TaskCreationSourceFork,
		Status:         persistence.TaskStatusQueued,
		Payload:        payload,
		Attempt:        1,
		MaxAttempts:    1, // forks are operator-initiated; one shot. Operator can fork again.
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := f.Tasks.Create(ctx, task); err != nil {
		f.Metrics.recordForkOutcome(forkOutcomeError)
		return nil, fmt.Errorf("replay: create fork task: %w", err)
	}

	// Audit + metrics on success. Audit write is best-effort —
	// fork already succeeded; failing to log shouldn't unwind
	// task creation.
	f.Metrics.recordForkOutcome(forkOutcomeCreated)
	f.recordAudit(ctx, sourceExecutionID, stepID, newID, target.PromptOverride)

	return &ForkResult{
		TaskID: newID,
		URL:    "/ui/tasks/" + newID,
	}, nil
}

// recordAudit writes one admin_audit row capturing the fork's
// who/when/from-where. Source="api" since today's only fork path
// is the HTTP endpoint; future CLI / Telegram entry points add
// their own labels. Errors are swallowed — audit fidelity is a
// dashboard concern, not a correctness one.
func (f *Forker) recordAudit(ctx context.Context, sourceExecutionID, stepID, newTaskID, override string) {
	if f.AuditAdmin == nil {
		return
	}
	now := time.Now().UTC()
	if f.Now != nil {
		now = f.Now()
	}
	// Before: source execution + chosen step. After: new task id +
	// trimmed override preview. Operators reading the audit row
	// see exactly what produced what without joining tables.
	beforeBlob, _ := json.Marshal(map[string]string{
		"source_execution_id": sourceExecutionID,
		"step_id":             stepID,
	})
	afterBlob, _ := json.Marshal(map[string]string{
		"new_task_id":     newTaskID,
		"prompt_override": override,
	})
	entry := &persistence.AdminAuditEntry{
		ID:        persistence.GenerateID("admaud"),
		Timestamp: now,
		Source:    "api",
		Action:    "execution.fork",
		Target:    newTaskID,
		Before:    string(beforeBlob),
		After:     string(afterBlob),
	}
	_ = f.AuditAdmin.Insert(ctx, entry)
}

// hasStepOutcome returns true when any outcome row in `rows` has
// the given step_id. Used by the v1 validation gate that forking
// is only allowed from steps that ran.
func hasStepOutcome(rows []*persistence.ExecutionStepOutcome, stepID string) bool {
	for _, r := range rows {
		if r != nil && r.StepID == stepID {
			return true
		}
	}
	return false
}

// ExtractForkTarget pulls the ForkTarget envelope out of a task's
// payload bytes. Returns nil + nil error when the payload doesn't
// carry one (the common case for non-fork tasks). Errors are
// reserved for malformed JSON inside an apparent fork envelope.
//
// Called by the executor on task lease so the new execution row
// gets its lineage columns populated.
func ExtractForkTarget(payload []byte) (*ForkTarget, error) {
	if len(payload) == 0 {
		return nil, nil
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(payload, &probe); err != nil {
		// Malformed envelope — caller (executor) treats this as
		// "not a fork" rather than aborting, since the existing
		// payload field is operator-controlled and might be any
		// shape on legacy rows.
		return nil, nil
	}
	raw, ok := probe[ForkTargetPayloadKey]
	if !ok {
		return nil, nil
	}
	var target ForkTarget
	if err := json.Unmarshal(raw, &target); err != nil {
		return nil, fmt.Errorf("replay: parse fork target: %w", err)
	}
	if target.SourceExecutionID == "" || target.StepID == "" {
		return nil, errors.New("replay: fork target missing required fields")
	}
	return &target, nil
}
