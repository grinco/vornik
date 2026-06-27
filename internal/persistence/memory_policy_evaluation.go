package persistence

// MemoryPolicyEvaluationRepository persists per-retrieval
// firewall audit rows (migration 80). Implemented by the
// postgres backend; SQLite leaves it nil (the firewall is
// postgres-only in v1).
//
// The internal/memoryfirewall.AuditWriter takes the
// BatchInsert method (via the AuditSink interface there);
// admin surfaces use ListRecent.

import (
	"context"
	"time"

	"vornik.io/vornik/internal/memoryfirewall"
)

// MemoryPolicyEvaluationRepository is the persistence surface
// for the firewall's audit trail. Phase A schema lives in
// migration 80 (memory_policy_evaluations).
type MemoryPolicyEvaluationRepository interface {
	// BatchInsert writes evaluation rows in a single
	// multi-row INSERT. Implementations should keep failure
	// surface narrow — return one error for the batch, not
	// per-row, because the AuditWriter buffers a fixed-size
	// batch.
	BatchInsert(ctx context.Context, rows []memoryfirewall.EvaluationRow) error

	// ListRecent returns the most recent evaluation rows for
	// a project, newest first. decisionFilter is optional —
	// empty returns all decision classes; non-empty matches
	// the decision column exactly. limit is clamped to [1, 500].
	ListRecent(ctx context.Context, projectID, decisionFilter string, since time.Time, limit int) ([]memoryfirewall.EvaluationRow, error)

	// ListByDigest returns every evaluation row recorded under a
	// given policy_digest, newest first. Powers the downstream
	// proof-verifier endpoint ("show me everyone who saw chunk X
	// under policy revision A") — firewall LLD § REST endpoints /
	// drift-mitigation §8.3. Uses idx_policy_eval_trace's sibling
	// scan on policy_digest; limit clamped to [1, 500].
	ListByDigest(ctx context.Context, policyDigest string, limit int) ([]memoryfirewall.EvaluationRow, error)

	// ListByChunk returns evaluation rows for a single chunk, newest first.
	// Powers the admin chunk-detail page's "recent evaluations" panel.
	// Previously that page called ListRecent across ALL projects (200 rows
	// over 30 days) and filtered to the chunk in Go — a cross-project scan on
	// a 36k-row table that discarded ~75% of what it read. This pushes the
	// filter into SQL, backed by idx_policy_eval_chunk (migration 102). limit
	// clamped to [1, 500].
	ListByChunk(ctx context.Context, chunkID string, limit int) ([]memoryfirewall.EvaluationRow, error)
}
