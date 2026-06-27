// Package persistence — auth + governance interfaces.
//
// APIKey (bearer auth + per-project scoping), IntentVerdict (two-tier intent judge), AutonomyEvaluation (per-tick autonomy decision audit).
// Split from interfaces.go on 2026-05-28 to keep each domain in
// its own file. Same package; no API change — pure file-org.
package persistence

import (
	"context"
	"errors"
	"time"
)

// APIKeyRepository persists per-project bearer tokens used by
// AuthMiddleware. The raw key is never stored — only the sha256
// hex digest. LookupActiveByHash is on the hot path of every
// authenticated request, so the underlying index must point at
// `key_hash` directly with a partial index on
// `revoked_at IS NULL` for speed.
type APIKeyRepository interface {
	// Create inserts one key row. Callers compute the hash and
	// pass it via key.KeyHash; the raw key is never seen by this
	// layer.
	Create(ctx context.Context, key *APIKey) error

	// LookupActiveByHash returns the active row matching the
	// supplied hash, or ErrAPIKeyNotFound. Rows with non-NULL
	// revoked_at or an expires_at in the past are treated as
	// non-existent — the caller can't tell "revoked" from
	// "never existed", which is the right defensive behaviour
	// for an auth layer.
	LookupActiveByHash(ctx context.Context, keyHash string) (*APIKey, error)

	// ListByProject returns every key row (including revoked ones)
	// scoped to a project, newest first. The UI / CLI consume this
	// to render the per-project keys table.
	ListByProject(ctx context.Context, projectID string) ([]*APIKey, error)

	// ListCompanionByProject returns only the companion-scoped keys
	// (client_kind IS NOT NULL) scoped to a project, newest first.
	// Backs the companion-plugin admin "list keys" surface (LLD 21)
	// — operators reviewing which host LLM sessions are still live
	// don't want every legacy bearer key in the table.
	ListCompanionByProject(ctx context.Context, projectID string) ([]*APIKey, error)

	// TouchLastUsed best-effort updates `last_used_at` to NOW().
	// AuthMiddleware fires this asynchronously so a DB hiccup
	// doesn't block the hot path. Errors are logged + dropped.
	TouchLastUsed(ctx context.Context, keyID string) error

	// Revoke sets `revoked_at` to NOW() for the supplied key.
	// Idempotent — revoking an already-revoked key is a no-op.
	Revoke(ctx context.Context, keyID string) error

	// UpdateAllowedWorkflows replaces the key's allowed_workflows
	// list wholesale. The slice is the new set; pass an empty slice
	// to mean "every workflow the project permits" (matches the
	// nullable column convention). The implementation re-encodes
	// the slice to the JSON-text representation the column stores.
	// Idempotent and safe to call concurrently — last writer wins.
	// Returns ErrAPIKeyNotFound when the key doesn't exist.
	UpdateAllowedWorkflows(ctx context.Context, keyID string, allowed []string) error

	// RevokeByName sets revoked_at for the key row whose name
	// matches. Used by the executor's per-task key lifecycle to
	// revoke "agent:task_<taskID>" without needing to look up the
	// key ID first. Idempotent — zero rows affected is not an error.
	RevokeByName(ctx context.Context, name string) error

	// UpdateAllowPush sets the allow_push flag on a key. Default false
	// (read-only); set true to grant git-push access over HTTPS
	// (git-over-HTTPS design, LLD slice 2). Returns ErrAPIKeyNotFound
	// when the key doesn't exist.
	UpdateAllowPush(ctx context.Context, keyID string, allowed bool) error
}

// ErrAPIKeyNotFound is returned by LookupActiveByHash when no
// active row matches. AuthMiddleware maps this to UNAUTHORIZED.
var ErrAPIKeyNotFound = errors.New("apikey: not found")

// IntentVerdictRepository persists the two-tier intent judge's
// decisions for calibration analyses and operator visibility.
type IntentVerdictRepository interface {
	// Insert writes one verdict row. Heuristic fields are
	// required; LLM fields are nil until the async refiner
	// upserts them via UpdateLLMRefinement.
	Insert(ctx context.Context, v *IntentVerdict) error

	// UpdateLLMRefinement fills in the LLM-tier columns on an
	// existing row. Idempotent: a second call replaces the
	// prior refinement (operators can re-run with a tuned
	// prompt without orphan rows).
	UpdateLLMRefinement(ctx context.Context, v *IntentVerdict) error

	// ListRecent returns the most recent N verdicts for a
	// project, newest-first. Powers the operator UI / CLI
	// "show me what the judge said today" surface.
	ListRecent(ctx context.Context, projectID string, limit int) ([]*IntentVerdict, error)
}

// AutonomyEvaluationRepository persists per-tick autonomy evaluation
// outcomes. One row per tick, regardless of whether a task was created,
// so operators can see REJECTED / DEDUPED / COOLDOWN / NO_ACTION decisions
// alongside CREATED ones without having to correlate by hand in the logs.
type AutonomyEvaluationRepository interface {
	// Record inserts one evaluation row.
	Record(ctx context.Context, e *AutonomyEvaluation) error

	// List returns evaluations matching the filter, newest first.
	List(ctx context.Context, filter AutonomyEvaluationFilter) ([]*AutonomyEvaluation, error)

	// CountByOutcome groups evaluations by outcome within an optional time
	// window. Zero time is unbounded on that side. Powers dashboards and
	// the autonomy health check in vornikctl doctor.
	CountByOutcome(ctx context.Context, projectID string, since, until time.Time) (map[string]int64, error)
}
