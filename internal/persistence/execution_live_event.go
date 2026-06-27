package persistence

import (
	"context"
	"time"
)

// ExecutionLiveEvent is one persisted live-event row. Mirrors the
// wire-format livepubsub.LiveEvent except payload is held as raw
// JSON bytes (the persistence layer doesn't import internal/
// executor/livepubsub — the wrapper marshals/unmarshals at the
// boundary).
type ExecutionLiveEvent struct {
	ID          int64
	ExecutionID string
	Seq         int64
	Kind        string
	Payload     []byte // JSON-encoded
	CreatedAt   time.Time
}

// ExecutionLiveEventRepository persists the live-event stream so
// a non-emitting replica can serve /executions/{id}/live, late
// subscribers can replay, and post-mortem audits can reconstruct
// "what events fired during this execution".
//
// Implementations:
//   - Postgres: real persistence + LISTEN/NOTIFY for cross-replica
//     fanout. The Append path is the one source of truth for
//     `seq` ordering — it computes the next seq atomically inside
//     the INSERT (SELECT MAX+1 with a unique index as the
//     backstop).
//   - SQLite: stub. Single-process deployments have no need for
//     cross-replica fanout; the in-memory livepubsub publisher
//     already covers them.
type ExecutionLiveEventRepository interface {
	// Append persists one live event under executionID, allocates
	// the next per-execution seq, and returns the assigned seq.
	// payload is the JSON-encoded LiveEvent.Payload (caller
	// marshals at the boundary). Errors mean "publish failed" —
	// callers log + drop (live-event flow must never block the
	// emitting goroutine).
	Append(ctx context.Context, executionID, kind string, payload []byte) (seq int64, err error)

	// ListSince returns events for executionID with seq >=
	// fromSeq, ordered ascending. Used by Subscribe when the
	// requested fromSeq is older than the in-memory ring still
	// retains. Empty slice when nothing matches.
	ListSince(ctx context.Context, executionID string, fromSeq int64, limit int) ([]*ExecutionLiveEvent, error)

	// LatestSeq returns the highest seq stored for executionID,
	// or -1 when no events exist yet. Used by the cross-replica
	// LISTEN goroutine to bootstrap "what events do we still need
	// to fetch?" after a NOTIFY fires but the row hasn't
	// committed yet in our snapshot.
	LatestSeq(ctx context.Context, executionID string) (int64, error)

	// DeleteOlderThan removes every event row with created_at <
	// cutoff. Returns rows removed. Drives the future stale-event
	// sweeper — not used in this commit but the contract is
	// pinned so the impl stays simple.
	DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error)
}
