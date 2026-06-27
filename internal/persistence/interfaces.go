// Package persistence provides database abstractions and repository
// implementations for the vornik daemon.
//
// Repository interfaces are split across multiple files by domain to
// keep each grouping reviewable in isolation. This file holds the
// shared error type + constants every other interface file uses; the
// per-domain interface files live alongside as interfaces_<domain>.go.
//
// File layout (2026-05-28 split — see CHANGELOG for the per-file
// rationale):
//
//   - interfaces.go                   — RepositoryError + Err* constants
//   - interfaces_task.go              — task / execution lifecycle + per-task analytics
//   - interfaces_artifact.go          — artifact + extracted-document storage
//   - interfaces_memory.go            — ingest queue + memory audit + quarantine + epochs
//   - interfaces_knowledge_graph.go   — KG entity / edge / mention
//   - interfaces_audit.go             — tool / admin / chat audit
//   - interfaces_messaging.go         — TaskMessage / TaskWatcher / Webhook / TelegramThread
//   - interfaces_governance.go        — APIKey / IntentVerdict / AutonomyEvaluation
//   - interfaces_orchestration.go     — CPC / ProjectSpawn / Wizard / WorkflowProposal
//   - interfaces_trading.go           — Trading order / safety / fill / snapshot
//
// All files share `package persistence` — splitting is purely
// file-organisation. Adding a new repo: pick the closest existing
// domain file, drop the interface there. Adding a new domain:
// create a new `interfaces_<name>.go` with the same package
// declaration.
package persistence

// RepositoryError defines common errors for repository operations.
type RepositoryError string

const (
	// ErrNotFound indicates the requested entity was not found.
	ErrNotFound RepositoryError = "not found"

	// ErrDuplicateKey indicates a unique constraint violation.
	ErrDuplicateKey RepositoryError = "duplicate key"

	// ErrOptimisticLock indicates a concurrent modification conflict.
	ErrOptimisticLock RepositoryError = "optimistic lock conflict"

	// ErrInvalidTransition indicates a write was refused because
	// the row is in a terminal state — used by the wizard's
	// CommitTo to reject a second commit attempt.
	ErrInvalidTransition RepositoryError = "invalid transition"

	// ErrNoTasksAvailable indicates no tasks were available for leasing.
	ErrNoTasksAvailable RepositoryError = "no tasks available"

	// ErrLeaseNotFound indicates the lease doesn't exist or doesn't match.
	ErrLeaseNotFound RepositoryError = "lease not found"

	// ErrLeaseExpired indicates the lease has already expired.
	ErrLeaseExpired RepositoryError = "lease expired"

	// ErrOrderIdentityMismatch indicates a TradingOrderRepository.Record
	// upsert hit a row whose (symbol, action, qty, limit_price) tuple
	// differs from the incoming payload. The same (project_id,
	// idempotency_key) pair must always describe the SAME order —
	// returning this rather than silently merging avoids the
	// 2026-05-15 NVDA bookkeeping corruption (a fresh 6-share buy
	// got attached to a stale 2.7614 fractional row because both
	// calls had passed sha256("") as the idempotency_key). The
	// broker audit writer surfaces this to operators as a hard
	// alert; the underlying IBKR order has already been placed by
	// the time this error fires, so the operator's job is to
	// reconcile the broker state.
	ErrOrderIdentityMismatch RepositoryError = "order identity mismatch on idempotency key"

	// ErrProposalRateLimited indicates a WorkflowProposalRepository.Insert
	// was rejected because the workflow already has a pending
	// proposal. The memetic-workflows design (see
	// https://docs.vornik.io)
	// caps to one open proposal per workflow at a time so operators
	// don't drown in churn; this surfaces as 429 in the admin API.
	ErrProposalRateLimited RepositoryError = "workflow already has a pending proposal"

	// ErrInvalidProposalTransition fires when WorkflowProposalRepository
	// is asked to advance a row out of its current state
	// non-sensically — e.g. Decide() on a row that's already
	// applied, or MarkRolledBack() on a row that was never applied.
	// Distinct from ErrInvalidTransition because the proposal
	// state machine is its own thing and the message helps the
	// API surface a clear 409 to the operator.
	ErrInvalidProposalTransition RepositoryError = "invalid workflow proposal state transition"
)

func (e RepositoryError) Error() string {
	return string(e)
}
