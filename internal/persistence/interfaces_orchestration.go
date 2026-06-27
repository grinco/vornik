// Package persistence — inter-project + workflow-meta interfaces.
//
// CrossProjectCall + ProjectSpawn (the inter-project orchestration ledger), ProjectWizardSession (conversational project setup), WorkflowProposal (memetic-workflows architect emits).
// Split from interfaces.go on 2026-05-28 to keep each domain in
// its own file. Same package; no API change — pure file-org.
package persistence

import (
	"context"
	"time"
)

// ExecutionHintRepository persists operator-injected hints
// (Feature #3 Phase C). See https://docs.vornik.io
// observation-design.md. The executor's pre-step hook consumes
// pending rows and flips applied_at; the API hint endpoint
// inserts new rows on operator POSTs.
// CrossProjectCallRepository persists cross_project_calls
// rows — the durable ledger for inter-project task delegation
// (LLD: https://docs.vornik.io
// design.md §5.1).
//
// One row per call_project step execution. Caller-side handler
// creates the row + the callee task atomically; the resolve
// hook fired by the callee's terminal-status transition writes
// the result envelope back and flips the status.
type CrossProjectCallRepository interface {
	// Create inserts a new pending CPC row. ID is generated when
	// empty. Returns ErrDuplicateKey if the (caller_task_id,
	// caller_step_id) pair already has a row — though the
	// executor handler protects against this by checking before
	// insert.
	Create(ctx context.Context, c *CrossProjectCall) error

	// Get returns the row by ID. ErrNotFound when missing.
	Get(ctx context.Context, id string) (*CrossProjectCall, error)

	// GetByCalleeTaskID returns the CPC row whose
	// callee_task_id matches. Used by the executor's resolve
	// hook to map "this task just terminated" → "which CPC
	// owns it". ErrNotFound when the task isn't a CPC callee.
	GetByCalleeTaskID(ctx context.Context, calleeTaskID string) (*CrossProjectCall, error)

	// SetCalleeTaskID stamps the callee_task_id after the
	// callee task is created in the same transaction. Separate
	// method so the Create+task-insert pair can be wrapped in
	// one transaction at the caller side.
	SetCalleeTaskID(ctx context.Context, id, calleeTaskID string) error

	// MarkRunning flips status from pending → running once the
	// callee task is leased. Idempotent — re-calling for an
	// already-running row is a no-op (returns nil).
	MarkRunning(ctx context.Context, id string) error

	// MarkCompleted resolves the CPC with a validated envelope:
	// status=completed, result_envelope=body, resolved_at=NOW().
	MarkCompleted(ctx context.Context, id string, envelope []byte) error

	// MarkFailed resolves the CPC as failed (callee task ended
	// FAILED/CANCELLED): status=failed, error_message=reason,
	// resolved_at=NOW().
	MarkFailed(ctx context.Context, id, reason string) error

	// MarkRejected resolves the CPC as rejected (validation /
	// acceptCallsFrom): status=rejected, error_message=reason,
	// resolved_at=NOW(). Distinct from failed so callers can
	// take a different on_failure branch.
	MarkRejected(ctx context.Context, id, reason string) error

	// ClaimTimedOut atomically transitions pending/running rows
	// past their timeout_at into status=timed_out and returns
	// them. Used by the Phase D timeout scanner. Atomic via
	// UPDATE ... RETURNING so two scanners (HA, 2026.8+) can't
	// double-claim. now is the cutoff; rows whose
	// timeout_at < now and status not yet terminal are claimed.
	// limit caps the batch size so a backlog can't lock the
	// table.
	ClaimTimedOut(ctx context.Context, now time.Time, limit int) ([]*CrossProjectCall, error)

	// List returns rows matching the filter, newest-first, capped
	// by PageSize. Drives the admin `vornikctl cpc list` surface
	// and any future operator dashboards.
	List(ctx context.Context, filter CPCListFilter) ([]*CrossProjectCall, error)

	// AdminCancel is the operator-triggered force-resolve. Flips
	// a pending/running row to status=rejected with the supplied
	// reason and resolved_at=NOW(). Idempotent — re-calling on
	// an already-terminal row is a no-op (returns nil).
	// Distinct from MarkRejected so audit + metrics can label
	// operator-initiated cancellations differently from
	// automatic envelope-shape rejections.
	AdminCancel(ctx context.Context, id, reason string) error
}

// CPCListFilter drives CrossProjectCallRepository.List. All
// fields are optional; zero-value means "any". Pagination via
// PageSize (caller passes 0 for the default cap of 200).
type CPCListFilter struct {
	// Status restricts to rows with this status. Empty = any.
	Status CrossProjectCallStatus
	// CallerProject restricts by caller. Empty = any.
	CallerProject string
	// CalleeProject restricts by callee. Empty = any.
	CalleeProject string
	// CreatedSince restricts to rows created at or after this
	// timestamp. Zero-value = unbounded.
	CreatedSince time.Time
	// PageSize caps the result count. Zero defaults to 200; a
	// hard maximum of 1000 is applied by the impl so a buggy
	// admin client can't drain the table.
	PageSize int
}

// ProjectSpawnRepository persists project_spawns rows — the
// lineage ledger for spawn_project steps (LLD §5.2). One row
// per materialised spawn; UNIQUE on spawned_project enforces
// idempotence at the DB layer (a retried step that targets an
// existing spawn falls through to a no-op rather than
// double-create).
type ProjectSpawnRepository interface {
	// Create inserts one row. ID is generated when empty.
	// Returns ErrDuplicateKey on the spawned_project UNIQUE
	// collision — caller treats this as "spawn already
	// happened, skip" (idempotent on retry).
	Create(ctx context.Context, s *ProjectSpawn) error

	// GetBySpawnedProject returns the lineage row for a given
	// spawned project ID. Used by the executor's spawn handler
	// to short-circuit on re-execution and by audit / UI to
	// answer "what spawned this project". ErrNotFound when the
	// project ID isn't from a spawn.
	GetBySpawnedProject(ctx context.Context, spawnedProjectID string) (*ProjectSpawn, error)

	// CountForProjectSince returns the number of rows where
	// parent_project = $1 AND created_at >= $2. Drives the
	// maxSpawnsPerDay rate-limit (caller passes time.Now()
	// .Add(-24h)).
	CountForProjectSince(ctx context.Context, parentProjectID string, since time.Time) (int64, error)
}

// ProjectWizardSessionRepository persists conversational project-
// wizard transcripts. See https://docs.vornik.io
// design.md. One row per operator conversation; operator_id is
// the only filter axis the API uses today (list-my-drafts).
type ProjectWizardSessionRepository interface {
	// Insert creates a brand-new session. The caller stamps the
	// ID via persistence.GenerateID("pw"); ErrDuplicateKey on a
	// collision is treated as a client retry by the wizard
	// handler.
	Insert(ctx context.Context, s *ProjectWizardSession) error
	// Get returns the session by ID; ErrNotFound when missing.
	Get(ctx context.Context, id string) (*ProjectWizardSession, error)
	// Update rewrites mutable columns by ID (transcript,
	// current_proposal, suggested_template, ready_to_commit).
	// updated_at is bumped server-side. Other columns
	// (committed_project_id, committed_at) flow through
	// CommitTo.
	Update(ctx context.Context, s *ProjectWizardSession) error
	// CommitTo stamps committed_project_id + committed_at in
	// one statement so the session becomes read-only atomically.
	// Returns ErrNotFound when the session ID is missing or
	// ErrInvalidTransition when already committed.
	CommitTo(ctx context.Context, sessionID, projectID string) error
	// Cancel stamps cancelled_at on an uncommitted session owned by
	// operatorID, freeing the operator's active-session slot. The
	// operatorID predicate is an IDOR guard. Idempotent on an
	// already-cancelled session (returns nil); ErrNotFound when
	// missing or owned by another operator; ErrInvalidTransition
	// when already committed.
	Cancel(ctx context.Context, sessionID, operatorID string) error
	// ListByOperator returns the operator's sessions newest-
	// first, capped at pageSize. Used by the "you have
	// unfinished drafts" banner on /ui/projects.
	ListByOperator(ctx context.Context, operatorID string, pageSize int) ([]*ProjectWizardSession, error)
}

// WorkflowProposalFilter narrows a List() call. Status filters to
// rows matching any of the listed statuses (OR semantics); empty
// means all statuses. WorkflowID filters to one workflow; empty
// means all workflows. PageSize caps the result; pass 0 for the
// repo's default (50).
type WorkflowProposalFilter struct {
	WorkflowID string
	Statuses   []WorkflowProposalStatus
	// Kinds filters to rows matching any of the listed proposal
	// kinds (OR semantics); empty means all kinds. Migration 83.
	Kinds    []WorkflowProposalKind
	PageSize int
}

// WorkflowProposalRepository persists architect-emitted workflow
// proposals (Slice 2 of the memetic-workflows arc). The architect
// inserts; the operator (UI / CLI / future Slice 4 apply path)
// transitions the status field through the state machine.
//
// Insertion-time rate limit: at most one row per workflow with
// status='pending' at any time. Enforced by the partial unique
// index on (workflow_id) WHERE status='pending' (see migration 65).
// A second Insert against a workflow that already has a pending
// proposal returns ErrProposalRateLimited so the caller can surface
// a clear 429 to the operator.
type WorkflowProposalRepository interface {
	// Insert persists a new pending proposal. Returns
	// ErrProposalRateLimited if a pending proposal already
	// exists for the same workflow; the caller (admin endpoint)
	// surfaces this as 429 Too Many Requests with the existing
	// proposal's ID in the body.
	Insert(ctx context.Context, p *WorkflowProposal) error

	// Get returns the row by primary key. Returns ErrNotFound
	// when the proposal doesn't exist.
	Get(ctx context.Context, id string) (*WorkflowProposal, error)

	// List returns proposals matching the filter, newest first.
	// Bounded by filter.PageSize (default 50).
	List(ctx context.Context, filter WorkflowProposalFilter) ([]*WorkflowProposal, error)

	// Decide transitions a proposal from pending → approved
	// or pending → rejected. Stamps decided_at + decided_by;
	// optional notes for the operator's rationale. Returns
	// ErrInvalidProposalTransition if the row isn't pending
	// (idempotent on already-decided rows would mask a UI bug).
	Decide(ctx context.Context, id string, status WorkflowProposalStatus, decidedBy, notes string) error

	// MarkApplied flips approved → applied + stamps the git
	// commit. Called from the Slice 4 apply path.
	MarkApplied(ctx context.Context, id, appliedCommit string) error

	// MarkRolledBack flips applied → rolled_back + stamps the
	// revert commit. Called from the Slice 5 rollback button.
	MarkRolledBack(ctx context.Context, id, rollbackCommit string) error

	// UpdateProposalYAML replaces the proposal_yaml of a PENDING
	// proposal — backs the operator review UI's "Modify" button
	// (§8.5): tweak the architect's YAML before approving. Refuses
	// (ErrInvalidProposalTransition) when the row isn't pending so a
	// decided/applied proposal's recorded YAML can't be rewritten.
	// editedBy is stamped into notes for the audit trail.
	UpdateProposalYAML(ctx context.Context, id, newYAML, editedBy string) error
}
