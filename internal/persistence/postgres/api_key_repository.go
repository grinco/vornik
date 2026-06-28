package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// encodeAllowedWorkflows turns the slice form used in the APIKey
// struct into the TEXT JSON column form. A nil slice means "no
// scope filter" and serialises to a SQL NULL; an empty slice is
// intentionally distinguishable (encodes to "[]") so a future
// caller that means "explicitly none" doesn't get silently
// promoted to "all". The companion grant handler rejects empty
// slices to prevent operator footgun.
func encodeAllowedWorkflows(wfs []string) sql.NullString {
	if wfs == nil {
		return sql.NullString{}
	}
	b, err := json.Marshal(wfs)
	if err != nil {
		// Marshalling a []string is infallible in encoding/json;
		// the only failure mode is non-UTF-8 input, which our
		// IDs never are. Fall through to NULL on the impossible
		// path rather than introducing an error return.
		return sql.NullString{}
	}
	return sql.NullString{String: string(b), Valid: true}
}

// decodeAllowedWorkflows reverses encodeAllowedWorkflows.
// NULL → nil slice; "[]" → empty slice; anything else → decoded.
func decodeAllowedWorkflows(raw sql.NullString) []string {
	if !raw.Valid {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw.String), &out); err != nil {
		// Corrupt JSON in the column would indicate hand-editing;
		// surface as no-scope so the request fails closed (the
		// project-level allowlist still applies).
		return []string{}
	}
	return out
}

// APIKeyRepository is the PostgreSQL implementation of
// persistence.APIKeyRepository. LookupActiveByHash hits the
// partial index on `key_hash WHERE revoked_at IS NULL` so the
// auth hot path is a single index scan per request.
type APIKeyRepository struct {
	db DBTX
}

// NewAPIKeyRepository constructs the repo.
func NewAPIKeyRepository(db DBTX) *APIKeyRepository {
	return &APIKeyRepository{db: db}
}

// Create inserts one key row. KeyHash is the sha256 hex digest of
// the raw key; the raw key itself is never seen by this layer.
//
// Companion-scope columns (allowed_workflows, budget_cap_usd,
// client_kind, session_label) are inserted from the APIKey struct;
// pass nil/empty/zero values for non-companion keys and the columns
// stay NULL — preserving the pre-LLD-21 INSERT semantics.
func (r *APIKeyRepository) Create(ctx context.Context, key *persistence.APIKey) error {
	var budget sql.NullFloat64
	if key.BudgetCapUSD != nil {
		budget = sql.NullFloat64{Float64: *key.BudgetCapUSD, Valid: true}
	}
	var rps, burst sql.NullInt64
	if key.RateLimitRPS != nil {
		rps = sql.NullInt64{Int64: int64(*key.RateLimitRPS), Valid: true}
	}
	if key.RateLimitBurst != nil {
		burst = sql.NullInt64{Int64: int64(*key.RateLimitBurst), Valid: true}
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO api_keys (
		    id, project_id, name, key_hash, key_prefix,
		    created_at, expires_at, created_by,
		    rate_limit_rps, rate_limit_burst,
		    allowed_workflows, budget_cap_usd, client_kind, session_label,
		    memory_read, memory_write, allow_push, default_repo_scope
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)`,
		key.ID, key.ProjectID, key.Name, key.KeyHash, key.KeyPrefix,
		key.CreatedAt, key.ExpiresAt, nullable(key.CreatedBy),
		rps, burst,
		encodeAllowedWorkflows(key.AllowedWorkflows), budget,
		nullable(key.ClientKind), nullable(key.SessionLabel),
		key.MemoryRead, key.MemoryWrite, key.AllowPush, nullable(key.DefaultRepoScope),
	)
	return mapDBError(err)
}

// LookupActiveByHash returns the unique row whose key_hash matches.
// Filters revoked + expired rows out at query time so the caller
// can't mistake a stale row for an active one — defensive: an
// auth layer that ever returned a revoked key would be a P0.
func (r *APIKeyRepository) LookupActiveByHash(ctx context.Context, keyHash string) (*persistence.APIKey, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, project_id, name, key_hash, key_prefix,
		       created_at, last_used_at, expires_at, revoked_at, created_by,
		       rate_limit_rps, rate_limit_burst,
		       allowed_workflows, budget_cap_usd, client_kind, session_label,
		       memory_read, memory_write, allow_push, default_repo_scope
		FROM api_keys
		WHERE key_hash = $1
		  AND revoked_at IS NULL
		  AND (expires_at IS NULL OR expires_at > NOW())`,
		keyHash,
	)
	return scanAPIKeyRow(row)
}

// ListByProject returns every key for a project (including
// revoked ones) newest-first. Used by the management surface
// to render the per-project table.
func (r *APIKeyRepository) ListByProject(ctx context.Context, projectID string) ([]*persistence.APIKey, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, project_id, name, key_hash, key_prefix,
		       created_at, last_used_at, expires_at, revoked_at, created_by,
		       rate_limit_rps, rate_limit_burst,
		       allowed_workflows, budget_cap_usd, client_kind, session_label,
		       memory_read, memory_write, allow_push, default_repo_scope
		FROM api_keys
		WHERE project_id = $1
		ORDER BY created_at DESC`,
		projectID,
	)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.APIKey
	for rows.Next() {
		k, err := scanAPIKeyRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// ListCompanionByProject returns only the companion-scoped keys for
// a project, newest-first. Companion keys are identified by a
// non-NULL client_kind; the partial index idx_api_keys_client_kind
// keeps the lookup cheap as the api_keys table grows. Used by the
// companion admin "list keys" endpoint and the operator UI.
func (r *APIKeyRepository) ListCompanionByProject(ctx context.Context, projectID string) ([]*persistence.APIKey, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, project_id, name, key_hash, key_prefix,
		       created_at, last_used_at, expires_at, revoked_at, created_by,
		       rate_limit_rps, rate_limit_burst,
		       allowed_workflows, budget_cap_usd, client_kind, session_label,
		       memory_read, memory_write, allow_push, default_repo_scope
		FROM api_keys
		WHERE project_id = $1
		  AND client_kind IS NOT NULL
		ORDER BY created_at DESC`,
		projectID,
	)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.APIKey
	for rows.Next() {
		k, err := scanAPIKeyRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// scanAPIKeyRow is the common decode path for every SELECT shape on
// api_keys. The column order must match every callsite's SELECT
// statement — keeping it centralised means a new column added in
// the future is one edit, not eight. Accepts either *sql.Row or
// *sql.Rows via the small interface.
func scanAPIKeyRow(scanner interface{ Scan(dest ...any) error }) (*persistence.APIKey, error) {
	var k persistence.APIKey
	var createdBy, clientKind, sessionLabel, defaultRepoScope sql.NullString
	var rps, burst sql.NullInt64
	var allowedWF sql.NullString
	var budget sql.NullFloat64
	if err := scanner.Scan(
		&k.ID, &k.ProjectID, &k.Name, &k.KeyHash, &k.KeyPrefix,
		&k.CreatedAt, &k.LastUsedAt, &k.ExpiresAt, &k.RevokedAt, &createdBy,
		&rps, &burst,
		&allowedWF, &budget, &clientKind, &sessionLabel,
		&k.MemoryRead, &k.MemoryWrite, &k.AllowPush, &defaultRepoScope,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, persistence.ErrAPIKeyNotFound
		}
		return nil, mapDBError(err)
	}
	if createdBy.Valid {
		k.CreatedBy = createdBy.String
	}
	if rps.Valid {
		v := int(rps.Int64)
		k.RateLimitRPS = &v
	}
	if burst.Valid {
		v := int(burst.Int64)
		k.RateLimitBurst = &v
	}
	k.AllowedWorkflows = decodeAllowedWorkflows(allowedWF)
	if budget.Valid {
		v := budget.Float64
		k.BudgetCapUSD = &v
	}
	if clientKind.Valid {
		k.ClientKind = clientKind.String
	}
	if sessionLabel.Valid {
		k.SessionLabel = sessionLabel.String
	}
	if defaultRepoScope.Valid {
		k.DefaultRepoScope = defaultRepoScope.String
	}
	return &k, nil
}

// TouchLastUsed best-effort writes the current time to
// last_used_at. AuthMiddleware fires this asynchronously so a DB
// hiccup never delays the hot path.
func (r *APIKeyRepository) TouchLastUsed(ctx context.Context, keyID string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE api_keys SET last_used_at = $1 WHERE id = $2`,
		time.Now().UTC(), keyID,
	)
	return mapDBError(err)
}

// Revoke sets revoked_at = NOW() iff currently NULL — idempotent
// against repeated revocations of the same key.
func (r *APIKeyRepository) Revoke(ctx context.Context, keyID string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE api_keys
		SET revoked_at = NOW()
		WHERE id = $1 AND revoked_at IS NULL`,
		keyID,
	)
	return mapDBError(err)
}

// RevokeByName sets revoked_at = NOW() for the key row whose name
// matches. Idempotent — zero rows affected is not an error. Used by
// the executor's per-task key lifecycle where the name
// "agent:task_<taskID>" is known but the key ID is not.
func (r *APIKeyRepository) RevokeByName(ctx context.Context, name string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE api_keys
		SET revoked_at = NOW()
		WHERE name = $1 AND revoked_at IS NULL`,
		name,
	)
	return mapDBError(err)
}

// UpdateAllowedWorkflows replaces the allowed_workflows JSON-encoded
// list on one key row. Encodes through the same encodeAllowedWorkflows
// helper Create uses so the column stays in lockstep across paths.
// Returns ErrAPIKeyNotFound when no row matches.
func (r *APIKeyRepository) UpdateAllowedWorkflows(ctx context.Context, keyID string, allowed []string) error {
	if keyID == "" {
		return fmt.Errorf("UpdateAllowedWorkflows: keyID is required")
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE api_keys
		SET allowed_workflows = $2
		WHERE id = $1`,
		keyID, encodeAllowedWorkflows(allowed),
	)
	if err != nil {
		return mapDBError(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return mapDBError(err)
	}
	if n == 0 {
		return persistence.ErrAPIKeyNotFound
	}
	return nil
}

// UpdateAllowPush sets the allow_push flag on one key row. Default false;
// set true to grant git-push access (git-over-HTTPS design, LLD slice 2).
// Returns ErrAPIKeyNotFound when no row matches.
func (r *APIKeyRepository) UpdateAllowPush(ctx context.Context, keyID string, allowed bool) error {
	if keyID == "" {
		return fmt.Errorf("UpdateAllowPush: keyID is required")
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE api_keys
		SET allow_push = $2
		WHERE id = $1`,
		keyID, allowed,
	)
	if err != nil {
		return mapDBError(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return mapDBError(err)
	}
	if n == 0 {
		return persistence.ErrAPIKeyNotFound
	}
	return nil
}

// nullable converts an empty string to a sql.NullString so the
// column stays NULL rather than landing as the empty string. Both
// would technically work — every read path tolerates both — but
// NULL preserves the "field was never set" semantic cleanly.
func nullable(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
