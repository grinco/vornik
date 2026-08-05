package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// Step 4 of the MCP server authentication design §6 — the Postgres token store.
//
// Two things here are load-bearing rather than incidental:
//
//   - SwapRefreshToken's conditional UPDATE is the rotation guard. Nearly every authorization
//     server surveyed rotates the refresh token on use, so a concurrent refresh must lose
//     cleanly instead of clobbering the winner's rotated value.
//   - WithRefreshLock takes a TRANSACTION-scoped advisory lock, so it is released by COMMIT /
//     ROLLBACK even if the process dies mid-refresh. A session-scoped lock would survive a
//     panic and wedge every future refresh for that grant until the connection was recycled.

// MCPOAuthTokenRepository is the Postgres implementation of the MCP OAuth token store.
type MCPOAuthTokenRepository struct {
	db persistence.DBTX
}

// NewMCPOAuthTokenRepository builds the repository.
func NewMCPOAuthTokenRepository(db persistence.DBTX) *MCPOAuthTokenRepository {
	return &MCPOAuthTokenRepository{db: db}
}

var _ persistence.MCPOAuthTokenRepository = (*MCPOAuthTokenRepository)(nil)

const mcpOAuthColumns = `project_id, server_name, resource, client_id, access_token,
	refresh_token, expires_at, scopes, redirect_uri, connected_by, needs_reconnect, connected_at, updated_at`

// Get returns the grant, or nil when there is none.
func (r *MCPOAuthTokenRepository) Get(ctx context.Context, projectID, serverName string) (*persistence.MCPOAuthToken, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+mcpOAuthColumns+`
		   FROM mcp_oauth_tokens
		  WHERE project_id = $1 AND server_name = $2`,
		projectID, serverName)
	tok, err := scanMCPOAuthToken(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: mcp_oauth_tokens get: %w", err)
	}
	return tok, nil
}

// Upsert writes the grant, replacing any existing one for the pair.
func (r *MCPOAuthTokenRepository) Upsert(ctx context.Context, tok *persistence.MCPOAuthToken) error {
	if tok == nil {
		return errors.New("postgres: mcp_oauth_tokens upsert: nil token")
	}
	now := time.Now().UTC()
	connectedAt := tok.ConnectedAt
	if connectedAt.IsZero() {
		connectedAt = now
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO mcp_oauth_tokens (`+mcpOAuthColumns+`)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		 ON CONFLICT (project_id, server_name) DO UPDATE SET
		     resource        = EXCLUDED.resource,
		     client_id       = EXCLUDED.client_id,
		     access_token    = EXCLUDED.access_token,
		     refresh_token   = EXCLUDED.refresh_token,
		     expires_at      = EXCLUDED.expires_at,
		     scopes          = EXCLUDED.scopes,
		     redirect_uri    = EXCLUDED.redirect_uri,
		     connected_by    = EXCLUDED.connected_by,
		     needs_reconnect = EXCLUDED.needs_reconnect,
		     updated_at      = EXCLUDED.updated_at`,
		tok.ProjectID, tok.ServerName, tok.Resource, tok.ClientID, tok.AccessToken,
		tok.RefreshToken, utcPtr(tok.ExpiresAt), tok.Scopes, tok.RedirectURI, tok.ConnectedBy,
		tok.NeedsReconnect, connectedAt, now,
	)
	if err != nil {
		return fmt.Errorf("postgres: mcp_oauth_tokens upsert: %w", err)
	}
	return nil
}

// SwapRefreshToken replaces the grant only if the stored refresh token is still the one the
// caller used. connected_at and connected_by are deliberately NOT touched: a refresh is not a
// new consent, and overwriting them would erase who granted access and when.
func (r *MCPOAuthTokenRepository) SwapRefreshToken(ctx context.Context, usedRefreshToken string, next *persistence.MCPOAuthToken) (bool, error) {
	if next == nil {
		return false, errors.New("postgres: mcp_oauth_tokens swap: nil token")
	}
	res, err := r.db.ExecContext(ctx,
		`UPDATE mcp_oauth_tokens
		    SET access_token    = $1,
		        refresh_token   = $2,
		        expires_at      = $3,
		        scopes          = $4,
		        needs_reconnect = FALSE,
		        updated_at      = $5
		  WHERE project_id = $6 AND server_name = $7 AND refresh_token = $8`,
		next.AccessToken, next.RefreshToken, utcPtr(next.ExpiresAt), next.Scopes,
		time.Now().UTC(), next.ProjectID, next.ServerName, usedRefreshToken,
	)
	if err != nil {
		return false, fmt.Errorf("postgres: mcp_oauth_tokens swap: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("postgres: mcp_oauth_tokens swap rows: %w", err)
	}
	return n > 0, nil
}

// MarkNeedsReconnect flags the grant as requiring human re-consent.
func (r *MCPOAuthTokenRepository) MarkNeedsReconnect(ctx context.Context, projectID, serverName string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE mcp_oauth_tokens
		    SET needs_reconnect = TRUE, updated_at = $1
		  WHERE project_id = $2 AND server_name = $3`,
		time.Now().UTC(), projectID, serverName)
	if err != nil {
		return fmt.Errorf("postgres: mcp_oauth_tokens mark needs_reconnect: %w", err)
	}
	return nil
}

// InvalidateStaleRedirectURIs implements §7.2a: a stored client_id registered under a
// redirect URI the deployment no longer serves cannot be used, so it is dropped and the
// grant is flagged for reconnect.
//
// One UPDATE, so two daemons behind one public_base_url cannot race each other through a
// read-modify-write. Rows with an empty recorded value are left alone — that means
// "written before migration 151", not "registered elsewhere".
func (r *MCPOAuthTokenRepository) InvalidateStaleRedirectURIs(ctx context.Context, current string) (int, error) {
	res, err := r.db.ExecContext(ctx,
		`UPDATE mcp_oauth_tokens
		    SET client_id = '', redirect_uri = '', needs_reconnect = TRUE, updated_at = $1
		  WHERE redirect_uri <> '' AND redirect_uri <> $2`,
		time.Now().UTC(), current)
	if err != nil {
		return 0, fmt.Errorf("postgres: mcp_oauth_tokens invalidate stale redirect URIs: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("postgres: mcp_oauth_tokens invalidate stale redirect URIs: rows affected: %w", err)
	}
	return int(n), nil
}

// Delete removes the grant (Disconnect).
func (r *MCPOAuthTokenRepository) Delete(ctx context.Context, projectID, serverName string) error {
	_, err := r.db.ExecContext(ctx,
		`DELETE FROM mcp_oauth_tokens WHERE project_id = $1 AND server_name = $2`,
		projectID, serverName)
	if err != nil {
		return fmt.Errorf("postgres: mcp_oauth_tokens delete: %w", err)
	}
	return nil
}

// ListForProject returns every grant for a project, ordered by server name so the
// control-plane rows are stable across refreshes.
func (r *MCPOAuthTokenRepository) ListForProject(ctx context.Context, projectID string) ([]*persistence.MCPOAuthToken, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+mcpOAuthColumns+`
		   FROM mcp_oauth_tokens
		  WHERE project_id = $1
		  ORDER BY server_name`, projectID)
	if err != nil {
		return nil, fmt.Errorf("postgres: mcp_oauth_tokens list: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*persistence.MCPOAuthToken
	for rows.Next() {
		tok, err := scanMCPOAuthToken(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: mcp_oauth_tokens list scan: %w", err)
		}
		out = append(out, tok)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: mcp_oauth_tokens list rows: %w", err)
	}
	return out, nil
}

// WithRefreshLock runs fn holding a transaction-scoped advisory lock for the (project, server)
// pair, so only one daemon refreshes a given grant at a time.
//
// The lock is keyed by hashtext over a namespaced key rather than by the raw pair, because
// pg_advisory_xact_lock takes a bigint. A hash collision is harmless here — it can only cause
// two unrelated grants to serialise, never to skip the lock.
//
// fn runs on the POOL, not inside the transaction. That is deliberate: the lock is held by the
// open transaction for the whole callback, which is all the mutual exclusion we need, and it
// keeps fn free to use the ordinary repository methods (and to make a slow HTTP call to the
// authorization server) without threading a tx through the OAuth client.
func (r *MCPOAuthTokenRepository) WithRefreshLock(ctx context.Context, projectID, serverName string, fn func(context.Context) error) error {
	db, ok := r.db.(interface {
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	})
	if !ok {
		// Already inside a transaction (or a mock): the caller's own scope
		// provides whatever serialisation exists. Running fn unlocked is the
		// honest fallback — SwapRefreshToken still makes a race SAFE.
		return fn(ctx)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("postgres: mcp_oauth_tokens refresh lock begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	key := "mcp_oauth_refresh:" + projectID + "/" + serverName
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, key); err != nil {
		return fmt.Errorf("postgres: mcp_oauth_tokens advisory lock: %w", err)
	}
	if err := fn(ctx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("postgres: mcp_oauth_tokens refresh lock commit: %w", err)
	}
	return nil
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanMCPOAuthToken(row rowScanner) (*persistence.MCPOAuthToken, error) {
	var (
		tok       persistence.MCPOAuthToken
		expiresAt sql.NullTime
	)
	if err := row.Scan(
		&tok.ProjectID, &tok.ServerName, &tok.Resource, &tok.ClientID, &tok.AccessToken,
		&tok.RefreshToken, &expiresAt, &tok.Scopes, &tok.RedirectURI, &tok.ConnectedBy,
		&tok.NeedsReconnect, &tok.ConnectedAt, &tok.UpdatedAt,
	); err != nil {
		return nil, err
	}
	if expiresAt.Valid {
		t := expiresAt.Time.UTC()
		tok.ExpiresAt = &t
	}
	tok.ConnectedAt = tok.ConnectedAt.UTC()
	tok.UpdatedAt = tok.UpdatedAt.UTC()
	return &tok, nil
}

// utcPtr normalises a nullable timestamp for the driver.
func utcPtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
}
