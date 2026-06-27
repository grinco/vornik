package persistence

import (
	"context"
	"time"
)

// ProfileUseAudit is one row in profile_use_audit — recorded
// per turn whose dispatcher injected a non-empty
// <operator_profile> block. The row captures which keys
// influenced the prompt so operators can answer "when did the
// model start using my 'prefers Czech' preference, and is
// that the right call?" via `vornikctl operator audit`.
//
// The audit is intentionally per-turn (not per-citation) — the
// model may or may not actually cite a key in its reply, but
// the dispatcher only knows what it injected into the prompt.
// "Did the model SAY it used X?" is operator-eyeballed from the
// reply text via the citation-marker convention; "did the model
// SEE X?" is what this audit records.
type ProfileUseAudit struct {
	ID         int64
	OperatorID string
	// TaskID is the chat task the turn ran under. Empty for
	// dispatcher contexts that aren't task-scoped (rare —
	// most turns are).
	TaskID    string
	UsedKeys  []string
	UsedNotes bool
	CreatedAt time.Time
}

// ProfileUseAuditQuery filters audit reads. Empty fields default
// to "no filter on that axis". Used by `vornikctl operator audit
// <id> [--since X] [--until Y] [--limit N]`.
type ProfileUseAuditQuery struct {
	Since time.Time
	Until time.Time
	Limit int
}

// ProfileUseAuditRepository persists per-turn profile-use rows.
// Implementations:
//   - Postgres: real append-only insert; List paginates by
//     created_at DESC with optional time + limit filters.
//   - SQLite: no-op insert + empty list. Single-process
//     deployments rarely need the audit surface; profile use
//     is still visible in the live conversation.
type ProfileUseAuditRepository interface {
	// Insert appends one row. Best-effort path — callers
	// typically swallow errors so a transient DB blip doesn't
	// break a chat turn over its audit-trail side effect.
	Insert(ctx context.Context, row *ProfileUseAudit) error

	// ListForOperator returns rows for operatorID matching the
	// query, newest-first. Empty Limit defaults to 50;
	// values > 500 cap at 500.
	ListForOperator(ctx context.Context, operatorID string, q ProfileUseAuditQuery) ([]*ProfileUseAudit, error)

	// DeleteAllForOperator drops every row for one operator.
	// Called from `vornikctl operator forget <id>
	// --include-audit` so a privacy-revocation can wipe the
	// audit trail too. Default forget keeps the audit since
	// it's the receipt that the model wasn't using forgotten
	// data after the wipe.
	DeleteAllForOperator(ctx context.Context, operatorID string) error
}
