package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// encodeAllowedWorkflows mirrors the postgres helper of the same
// name. Kept duplicated rather than hoisted into the parent
// persistence package because both sql.NullString flavours
// (database/sql) are identical — the duplication is one-liners.
func encodeAllowedWorkflows(wfs []string) sql.NullString {
	if wfs == nil {
		return sql.NullString{}
	}
	b, err := json.Marshal(wfs)
	if err != nil {
		return sql.NullString{}
	}
	return sql.NullString{String: string(b), Valid: true}
}

func decodeAllowedWorkflows(raw sql.NullString) []string {
	if !raw.Valid {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw.String), &out); err != nil {
		return []string{}
	}
	return out
}

// APIKeyRepository is the SQLite persistence.APIKeyRepository.
type APIKeyRepository struct {
	db DBTX
}

func NewAPIKeyRepository(db DBTX) *APIKeyRepository { return &APIKeyRepository{db: db} }

// Create persists a new key row. Caller pre-hashes the key.
func (r *APIKeyRepository) Create(ctx context.Context, k *persistence.APIKey) error {
	if k.CreatedAt.IsZero() {
		k.CreatedAt = time.Now().UTC()
	}
	var budget sql.NullFloat64
	if k.BudgetCapUSD != nil {
		budget = sql.NullFloat64{Float64: *k.BudgetCapUSD, Valid: true}
	}
	clientKind := sql.NullString{}
	if k.ClientKind != "" {
		clientKind = sql.NullString{String: k.ClientKind, Valid: true}
	}
	sessionLabel := sql.NullString{}
	if k.SessionLabel != "" {
		sessionLabel = sql.NullString{String: k.SessionLabel, Valid: true}
	}
	defaultRepoScope := sql.NullString{}
	if k.DefaultRepoScope != "" {
		defaultRepoScope = sql.NullString{String: k.DefaultRepoScope, Valid: true}
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO api_keys (
			id, project_id, name, key_hash, key_prefix,
			created_at, last_used_at, expires_at, revoked_at, created_by,
			rate_limit_rps, rate_limit_burst,
			allowed_workflows, budget_cap_usd, client_kind, session_label,
			memory_read, memory_write, allow_push, default_repo_scope
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		k.ID, k.ProjectID, k.Name, k.KeyHash, k.KeyPrefix,
		sqliteTime(k.CreatedAt), sqliteTimePtr(k.LastUsedAt), sqliteTimePtr(k.ExpiresAt),
		sqliteTimePtr(k.RevokedAt), k.CreatedBy, k.RateLimitRPS, k.RateLimitBurst,
		encodeAllowedWorkflows(k.AllowedWorkflows), budget, clientKind, sessionLabel,
		boolToInt(k.MemoryRead), boolToInt(k.MemoryWrite), boolToInt(k.AllowPush), defaultRepoScope,
	)
	return err
}

// boolToInt maps Go bools to SQLite's 0/1 storage. INTEGER 0/1 keeps
// the column compatible with BOOLEAN-style reads via standard
// database/sql Scan into *bool.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// LookupActiveByHash returns the active row matching the supplied
// hash, ErrAPIKeyNotFound otherwise. Expired-or-revoked rows are
// treated as non-existent — defensive behaviour for an auth layer.
func (r *APIKeyRepository) LookupActiveByHash(ctx context.Context, keyHash string) (*persistence.APIKey, error) {
	now := sqliteTime(time.Now().UTC())
	row := r.db.QueryRowContext(ctx, `
		SELECT id, project_id, name, key_hash, key_prefix,
		       created_at, last_used_at, expires_at, revoked_at, created_by,
		       rate_limit_rps, rate_limit_burst,
		       allowed_workflows, budget_cap_usd, client_kind, session_label,
		       memory_read, memory_write, allow_push, default_repo_scope
		FROM api_keys
		WHERE key_hash = ?
		  AND revoked_at IS NULL
		  AND (expires_at IS NULL OR expires_at > ?)`,
		keyHash, now)
	k, err := scanAPIKey(row)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return nil, persistence.ErrAPIKeyNotFound
		}
		return nil, err
	}
	return k, nil
}

// ListByProject returns every key for a project (including revoked),
// newest-first.
func (r *APIKeyRepository) ListByProject(ctx context.Context, projectID string) ([]*persistence.APIKey, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, project_id, name, key_hash, key_prefix,
		       created_at, last_used_at, expires_at, revoked_at, created_by,
		       rate_limit_rps, rate_limit_burst,
		       allowed_workflows, budget_cap_usd, client_kind, session_label,
		       memory_read, memory_write, allow_push, default_repo_scope
		FROM api_keys WHERE project_id = ?
		ORDER BY created_at DESC`, projectID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.APIKey
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// ListCompanionByProject returns only the companion-scoped keys
// (client_kind IS NOT NULL) for a project, newest-first. Mirrors
// the postgres method of the same name.
func (r *APIKeyRepository) ListCompanionByProject(ctx context.Context, projectID string) ([]*persistence.APIKey, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, project_id, name, key_hash, key_prefix,
		       created_at, last_used_at, expires_at, revoked_at, created_by,
		       rate_limit_rps, rate_limit_burst,
		       allowed_workflows, budget_cap_usd, client_kind, session_label,
		       memory_read, memory_write, allow_push, default_repo_scope
		FROM api_keys
		WHERE project_id = ?
		  AND client_kind IS NOT NULL
		ORDER BY created_at DESC`, projectID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.APIKey
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// TouchLastUsed updates last_used_at to NOW. Best-effort.
func (r *APIKeyRepository) TouchLastUsed(ctx context.Context, keyID string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE api_keys SET last_used_at = ? WHERE id = ?`,
		sqliteTime(time.Now().UTC()), keyID)
	return err
}

// Revoke sets revoked_at to NOW. Idempotent.
func (r *APIKeyRepository) Revoke(ctx context.Context, keyID string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE api_keys SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`,
		sqliteTime(time.Now().UTC()), keyID)
	return err
}

// RevokeByName sets revoked_at to NOW for the key row whose name
// matches. Idempotent — zero rows affected is not an error. SQLite
// mirror of the postgres impl.
func (r *APIKeyRepository) RevokeByName(ctx context.Context, name string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE api_keys SET revoked_at = ? WHERE name = ? AND revoked_at IS NULL`,
		sqliteTime(time.Now().UTC()), name)
	return err
}

// UpdateAllowedWorkflows replaces the allowed_workflows JSON-encoded
// list on one key row. SQLite mirror of the postgres impl.
func (r *APIKeyRepository) UpdateAllowedWorkflows(ctx context.Context, keyID string, allowed []string) error {
	if keyID == "" {
		return fmt.Errorf("UpdateAllowedWorkflows: keyID is required")
	}
	res, err := r.db.ExecContext(ctx,
		`UPDATE api_keys SET allowed_workflows = ? WHERE id = ?`,
		encodeAllowedWorkflows(allowed), keyID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return persistence.ErrAPIKeyNotFound
	}
	return nil
}

func scanAPIKey(scanner interface{ Scan(dest ...any) error }) (*persistence.APIKey, error) {
	var (
		k                persistence.APIKey
		createdAt        sqlTime
		lastUsed         sqlNullTime
		expiresAt        sqlNullTime
		revokedAt        sqlNullTime
		createdBy        sql.NullString
		rps, burst       sql.NullInt64
		allowedWF        sql.NullString
		budget           sql.NullFloat64
		clientKind       sql.NullString
		sessionLabel     sql.NullString
		defaultRepoScope sql.NullString
	)
	var memRead, memWrite, allowPush sql.NullInt64
	err := scanner.Scan(
		&k.ID, &k.ProjectID, &k.Name, &k.KeyHash, &k.KeyPrefix,
		&createdAt, &lastUsed, &expiresAt, &revokedAt, &createdBy,
		&rps, &burst,
		&allowedWF, &budget, &clientKind, &sessionLabel,
		&memRead, &memWrite, &allowPush, &defaultRepoScope,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, persistence.ErrNotFound
		}
		return nil, err
	}
	k.CreatedAt = createdAt.Time
	if lastUsed.Valid {
		t := lastUsed.Time
		k.LastUsedAt = &t
	}
	if expiresAt.Valid {
		t := expiresAt.Time
		k.ExpiresAt = &t
	}
	if revokedAt.Valid {
		t := revokedAt.Time
		k.RevokedAt = &t
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
	k.MemoryRead = memRead.Valid && memRead.Int64 != 0
	k.MemoryWrite = memWrite.Valid && memWrite.Int64 != 0
	k.AllowPush = allowPush.Valid && allowPush.Int64 != 0
	return &k, nil
}

// UpdateAllowPush sets the allow_push flag on one key row. SQLite mirror of
// the postgres impl. Uses boolToInt to convert bool → 0/1 for SQLite's
// INTEGER column. Returns ErrAPIKeyNotFound when no row matches.
func (r *APIKeyRepository) UpdateAllowPush(ctx context.Context, keyID string, allowed bool) error {
	if keyID == "" {
		return fmt.Errorf("UpdateAllowPush: keyID is required")
	}
	res, err := r.db.ExecContext(ctx,
		`UPDATE api_keys SET allow_push = ? WHERE id = ?`,
		boolToInt(allowed), keyID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return persistence.ErrAPIKeyNotFound
	}
	return nil
}
