package persistence

import (
	"context"
	"time"
)

// OperatorProfile is one persisted per-operator profile row.
// Structured is the JSONB blob the dispatcher reads on every
// turn (small set of well-known keys: tone, verbosity,
// time_zone, communication_style, preferred_channel); Notes is
// free-form context the assistant accumulates over time.
//
// The persistence layer holds Structured as []byte (the JSONB
// raw bytes) so this file doesn't need to import a structured
// dispatcher type — callers unmarshal at their boundary. Keeps
// the dependency graph one-directional (chat → persistence,
// never back).
type OperatorProfile struct {
	OperatorID string
	Structured []byte // JSONB-encoded structured preferences
	Notes      string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// OperatorProfileRepository persists per-operator profile rows.
// Implementations:
//   - Postgres: real persistence. Upsert uses ON CONFLICT DO
//     UPDATE so the dispatcher's per-turn write (when the
//     write tool eventually ships) is one round trip.
//   - SQLite: stub returning ErrNotFound + no-op Upsert/Delete.
//     Single-process deployments don't need cross-process
//     profile persistence — the in-memory dispatcher session
//     state covers the same surface for one daemon's lifetime.
//
// All methods are operator-id-scoped; cross-tenant scoping is
// deferred to 2026.8.0+ when the tenant model lands.
type OperatorProfileRepository interface {
	// Get returns the profile for operatorID. ErrNotFound when
	// no row exists — callers branch on this to decide whether
	// to inject the profile block into the dispatcher prompt
	// at all (omit when missing rather than emit an empty
	// block).
	Get(ctx context.Context, operatorID string) (*OperatorProfile, error)

	// Upsert inserts or updates the operator's profile row.
	// Empty Structured coerces to JSONB `{}` so the column's
	// NOT NULL constraint is satisfied. The dispatcher's
	// (future) update_operator_profile tool funnels through
	// this method.
	Upsert(ctx context.Context, profile *OperatorProfile) error

	// Delete removes the profile (privacy revocation via
	// `vornikctl operator forget <id>`). Idempotent — no error
	// when the row doesn't exist.
	Delete(ctx context.Context, operatorID string) error

	// List returns up to `limit` profiles ordered by
	// updated_at DESC so the UI ranks recently-active operators
	// first. limit <= 0 defaults to 50; values > 500 cap at
	// 500. Empty slice when no profiles exist.
	List(ctx context.Context, limit int) ([]*OperatorProfile, error)
}
