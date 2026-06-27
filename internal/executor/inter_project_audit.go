package executor

import (
	"context"
	"encoding/json"

	"vornik.io/vornik/internal/persistence"
)

// Inter-project orchestration audit helpers. One row per CPC
// create, CPC resolve, and project spawn — gives the admin
// audit UI a single chronological view of all cross-project
// activity. See LLD §9.4.
//
// All helpers are nil-safe and best-effort: a logged error
// doesn't fail the originating step. The lineage rows in
// cross_project_calls + project_spawns are the durable record;
// the audit row is the cross-cutting "who did what when"
// surface.

const (
	// auditActionCPCCreate fires when a call_project step
	// inserts a cross_project_calls row.
	auditActionCPCCreate = "interproject.cpc.create"
	// auditActionCPCResolve fires when the resolve hook flips
	// a CPC to a terminal state (completed/failed/rejected).
	auditActionCPCResolve = "interproject.cpc.resolve"
	// auditActionProjectSpawn fires when a spawn_project step
	// materialises a new project (idempotent skips don't
	// write a row).
	auditActionProjectSpawn = "interproject.project.spawn"
	// auditActionCPCDepthExceeded fires when a call_project step
	// is refused because the proposed hop would push the chain
	// past the depth cap.
	auditActionCPCDepthExceeded = "interproject.cpc.depth_exceeded"
	// auditActionCPCCycleDetected fires when a call_project step
	// is refused because the proposed callee is an ancestor of
	// the caller (cycle would form).
	auditActionCPCCycleDetected = "interproject.cpc.cycle_detected"

	// auditSource carries the audit row's `source` column.
	// CPC + spawn rows originate from the executor running an
	// agent workflow — no http / cli / tg surface produced
	// them.
	auditSource = "executor"
	// auditPrincipal is the placeholder principal recorded
	// for executor-initiated audit rows. The "operator" view
	// reconstructs end-user identity by joining against the
	// parent task's caller chain when needed; the audit row
	// itself records the source as the executor.
	auditPrincipal = "system:executor"
)

// recordCPCAuditCreate writes the audit row for a fresh CPC.
// `task` carries the caller context; `cpc` the newly-persisted
// ledger row. After-state JSON captures the routing fields so
// /ui/admin/audit can answer "what did this call do?" without
// a join against cross_project_calls.
func (e *Executor) recordCPCAuditCreate(
	ctx context.Context,
	task *persistence.Task,
	stepID string,
	cpc *persistence.CrossProjectCall,
	calleeTaskID string,
) {
	if e == nil || e.adminAuditRepo == nil || cpc == nil {
		return
	}
	after := map[string]any{
		"cpc_id":          cpc.ID,
		"caller_task_id":  cpc.CallerTaskID,
		"caller_project":  cpc.CallerProject,
		"caller_step_id":  cpc.CallerStepID,
		"callee_project":  cpc.CalleeProject,
		"callee_workflow": cpc.CalleeWorkflow,
		"callee_task_id":  calleeTaskID,
		"expected_schema": cpc.ExpectedSchema,
	}
	e.writeAuditRow(ctx, auditActionCPCCreate, cpc.CalleeProject, after)
	_ = task   // task referenced for future "join principal from caller chain"
	_ = stepID // included in CPC row already; kept in signature for symmetry
}

// recordCPCAuditResolve writes the audit row for a CPC
// reaching a terminal state. The lineage row carries the full
// envelope; the audit row records the status + reason so
// operators can spot patterns like "the architect project
// rejected 8 calls today" without scanning result_envelope
// JSON.
func (e *Executor) recordCPCAuditResolve(
	ctx context.Context,
	cpc *persistence.CrossProjectCall,
	status string,
	errMsg string,
) {
	if e == nil || e.adminAuditRepo == nil || cpc == nil {
		return
	}
	after := map[string]any{
		"cpc_id":         cpc.ID,
		"caller_project": cpc.CallerProject,
		"callee_project": cpc.CalleeProject,
		"status":         status,
	}
	if errMsg != "" {
		after["error_message"] = errMsg
	}
	e.writeAuditRow(ctx, auditActionCPCResolve, cpc.CalleeProject, after)
}

// recordSpawnAudit writes the audit row for a freshly-
// materialised project. Skipped idempotent runs are NOT
// audited (no new row was created; nothing changed for an
// operator to review).
func (e *Executor) recordSpawnAudit(
	ctx context.Context,
	parentTask *persistence.Task,
	stepID string,
	spawn *persistence.ProjectSpawn,
	initialTaskID string,
) {
	if e == nil || e.adminAuditRepo == nil || spawn == nil {
		return
	}
	after := map[string]any{
		"spawn_id":        spawn.ID,
		"spawned_project": spawn.SpawnedProject,
		"parent_project":  spawn.ParentProject,
		"parent_task_id":  spawn.ParentTaskID,
		"parent_step_id":  spawn.ParentStepID,
		"template_slug":   spawn.TemplateSlug,
	}
	if initialTaskID != "" {
		after["initial_task_id"] = initialTaskID
	}
	e.writeAuditRow(ctx, auditActionProjectSpawn, spawn.SpawnedProject, after)
	_ = parentTask // signature symmetry; principal expansion lands later
	_ = stepID
}

// recordCPCRefusal writes an audit row for a call_project step
// that was refused before the CPC row was created (depth limit
// hit, cycle detected). Distinct from recordCPCAuditResolve
// because there's no cpc.ID yet — the refusal didn't write a
// lineage row.
func (e *Executor) recordCPCRefusal(
	ctx context.Context,
	task *persistence.Task,
	stepID, calleeProject, action, reason string,
	depth int,
	lineagePath []string,
) {
	if e == nil || e.adminAuditRepo == nil {
		return
	}
	after := map[string]any{
		"caller_task_id": task.ID,
		"caller_project": task.ProjectID,
		"caller_step_id": stepID,
		"callee_project": calleeProject,
		"depth":          depth,
		"lineage_path":   lineagePath,
		"reason":         reason,
	}
	e.writeAuditRow(ctx, action, calleeProject, after)
}

// writeAuditRow is the single insert helper the three
// inter-project audit functions share. Marshals after-state
// to JSON; logs + swallows any persistence error so a failed
// audit write never breaks the originating step.
func (e *Executor) writeAuditRow(ctx context.Context, action, target string, after map[string]any) {
	if e == nil || e.adminAuditRepo == nil {
		return
	}
	afterJSON, err := json.Marshal(after)
	if err != nil {
		e.logger.Warn().Err(err).Str("action", action).
			Msg("inter-project audit: marshal after-state failed; row skipped")
		return
	}
	entry := &persistence.AdminAuditEntry{
		Principal: auditPrincipal,
		Source:    auditSource,
		Action:    action,
		Target:    target,
		After:     string(afterJSON),
	}
	if err := e.adminAuditRepo.Insert(ctx, entry); err != nil {
		e.logger.Warn().Err(err).Str("action", action).
			Str("target", target).
			Msg("inter-project audit: insert failed; row lost (lineage tables retain durable record)")
	}
}
