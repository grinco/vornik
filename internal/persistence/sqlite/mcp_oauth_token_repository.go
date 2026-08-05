package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// Step 4 of the MCP server authentication design §6 — the SQLite token store.
//
// A real implementation rather than a stub: without it, OAuth-authenticated MCP servers would
// be dark on every SQLite deployment, which is most single-host installs. TIMESTAMPTZ drops to
// RFC3339Nano TEXT (sqliteTime) and BOOLEAN to INTEGER; the shared repotest contract suite runs
// against both backends so a dialect divergence fails a test rather than a customer.

// MCPOAuthTokenRepository is the SQLite implementation of the MCP OAuth token store.
type MCPOAuthTokenRepository struct {
	db *sql.DB

	// refreshMu serialises WithRefreshLock in-process. SQLite is single-daemon by
	// construction, so a mutex is the whole of the mutual exclusion the Postgres side gets
	// from a transaction-scoped advisory lock.
	refreshMu chan struct{}
}

// NewMCPOAuthTokenRepository builds the repository.
func NewMCPOAuthTokenRepository(db *sql.DB) *MCPOAuthTokenRepository {
	return &MCPOAuthTokenRepository{db: db, refreshMu: make(chan struct{}, 1)}
}

var _ persistence.MCPOAuthTokenRepository = (*MCPOAuthTokenRepository)(nil)

const mcpOAuthColumns = `project_id, server_name, resource, client_id, access_token,
	refresh_token, expires_at, scopes, redirect_uri, connected_by, needs_reconnect, connected_at, updated_at`

// Get returns the grant, or nil when there is none.
func (r *MCPOAuthTokenRepository) Get(ctx context.Context, projectID, serverName string) (*persistence.MCPOAuthToken, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+mcpOAuthColumns+`
		   FROM mcp_oauth_tokens
		  WHERE project_id = ? AND server_name = ?`,
		projectID, serverName)
	tok, err := scanMCPOAuthToken(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: mcp_oauth_tokens get: %w", err)
	}
	return tok, nil
}

// Upsert writes the grant, replacing any existing one for the pair.
func (r *MCPOAuthTokenRepository) Upsert(ctx context.Context, tok *persistence.MCPOAuthToken) error {
	if tok == nil {
		return errors.New("sqlite: mcp_oauth_tokens upsert: nil token")
	}
	now := time.Now().UTC()
	connectedAt := tok.ConnectedAt
	if connectedAt.IsZero() {
		connectedAt = now
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO mcp_oauth_tokens (`+mcpOAuthColumns+`)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT (project_id, server_name) DO UPDATE SET
		     resource        = excluded.resource,
		     client_id       = excluded.client_id,
		     access_token    = excluded.access_token,
		     refresh_token   = excluded.refresh_token,
		     expires_at      = excluded.expires_at,
		     scopes          = excluded.scopes,
		     redirect_uri    = excluded.redirect_uri,
		     connected_by    = excluded.connected_by,
		     needs_reconnect = excluded.needs_reconnect,
		     updated_at      = excluded.updated_at`,
		tok.ProjectID, tok.ServerName, tok.Resource, tok.ClientID, tok.AccessToken,
		tok.RefreshToken, sqliteTimePtr(tok.ExpiresAt), tok.Scopes, tok.RedirectURI, tok.ConnectedBy,
		boolToInt(tok.NeedsReconnect), sqliteTime(connectedAt), sqliteTime(now),
	)
	if err != nil {
		return fmt.Errorf("sqlite: mcp_oauth_tokens upsert: %w", err)
	}
	return nil
}

// SwapRefreshToken replaces the grant only if the stored refresh token is still the one the
// caller used. connected_at / connected_by are untouched — a refresh is not a new consent.
func (r *MCPOAuthTokenRepository) SwapRefreshToken(ctx context.Context, usedRefreshToken string, next *persistence.MCPOAuthToken) (bool, error) {
	if next == nil {
		return false, errors.New("sqlite: mcp_oauth_tokens swap: nil token")
	}
	res, err := r.db.ExecContext(ctx,
		`UPDATE mcp_oauth_tokens
		    SET access_token    = ?,
		        refresh_token   = ?,
		        expires_at      = ?,
		        scopes          = ?,
		        needs_reconnect = 0,
		        updated_at      = ?
		  WHERE project_id = ? AND server_name = ? AND refresh_token = ?`,
		next.AccessToken, next.RefreshToken, sqliteTimePtr(next.ExpiresAt), next.Scopes,
		sqliteTime(time.Now().UTC()), next.ProjectID, next.ServerName, usedRefreshToken,
	)
	if err != nil {
		return false, fmt.Errorf("sqlite: mcp_oauth_tokens swap: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("sqlite: mcp_oauth_tokens swap rows: %w", err)
	}
	return n > 0, nil
}

// MarkNeedsReconnect flags the grant as requiring human re-consent.
func (r *MCPOAuthTokenRepository) MarkNeedsReconnect(ctx context.Context, projectID, serverName string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE mcp_oauth_tokens
		    SET needs_reconnect = 1, updated_at = ?
		  WHERE project_id = ? AND server_name = ?`,
		sqliteTime(time.Now().UTC()), projectID, serverName)
	if err != nil {
		return fmt.Errorf("sqlite: mcp_oauth_tokens mark needs_reconnect: %w", err)
	}
	return nil
}

// InvalidateStaleRedirectURIs implements §7.2a — see the postgres copy for the reasoning.
func (r *MCPOAuthTokenRepository) InvalidateStaleRedirectURIs(ctx context.Context, current string) (int, error) {
	res, err := r.db.ExecContext(ctx,
		`UPDATE mcp_oauth_tokens
		    SET client_id = '', redirect_uri = '', needs_reconnect = 1, updated_at = ?
		  WHERE redirect_uri <> '' AND redirect_uri <> ?`,
		sqliteTime(time.Now().UTC()), current)
	if err != nil {
		return 0, fmt.Errorf("sqlite: mcp_oauth_tokens invalidate stale redirect URIs: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sqlite: mcp_oauth_tokens invalidate stale redirect URIs: rows: %w", err)
	}
	return int(n), nil
}

// Delete removes the grant (Disconnect).
func (r *MCPOAuthTokenRepository) Delete(ctx context.Context, projectID, serverName string) error {
	_, err := r.db.ExecContext(ctx,
		`DELETE FROM mcp_oauth_tokens WHERE project_id = ? AND server_name = ?`,
		projectID, serverName)
	if err != nil {
		return fmt.Errorf("sqlite: mcp_oauth_tokens delete: %w", err)
	}
	return nil
}

// ListForProject returns every grant for a project, ordered by server name.
func (r *MCPOAuthTokenRepository) ListForProject(ctx context.Context, projectID string) ([]*persistence.MCPOAuthToken, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+mcpOAuthColumns+`
		   FROM mcp_oauth_tokens
		  WHERE project_id = ?
		  ORDER BY server_name`, projectID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: mcp_oauth_tokens list: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*persistence.MCPOAuthToken
	for rows.Next() {
		tok, err := scanMCPOAuthToken(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: mcp_oauth_tokens list scan: %w", err)
		}
		out = append(out, tok)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: mcp_oauth_tokens list rows: %w", err)
	}
	return out, nil
}

// WithRefreshLock serialises refreshes in-process. Honours ctx cancellation while waiting so a
// shutdown does not block behind a slow token endpoint.
func (r *MCPOAuthTokenRepository) WithRefreshLock(ctx context.Context, _, _ string, fn func(context.Context) error) error {
	select {
	case r.refreshMu <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-r.refreshMu }()
	return fn(ctx)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanMCPOAuthToken(row rowScanner) (*persistence.MCPOAuthToken, error) {
	var (
		tok         persistence.MCPOAuthToken
		expiresAt   sqlNullTime
		connectedAt sqlTime
		updatedAt   sqlTime
		needs       int
	)
	if err := row.Scan(
		&tok.ProjectID, &tok.ServerName, &tok.Resource, &tok.ClientID, &tok.AccessToken,
		&tok.RefreshToken, &expiresAt, &tok.Scopes, &tok.RedirectURI, &tok.ConnectedBy,
		&needs, &connectedAt, &updatedAt,
	); err != nil {
		return nil, err
	}
	if expiresAt.Valid {
		t := expiresAt.Time.UTC()
		tok.ExpiresAt = &t
	}
	tok.NeedsReconnect = needs != 0
	tok.ConnectedAt = connectedAt.Time.UTC()
	tok.UpdatedAt = updatedAt.Time.UTC()
	return &tok, nil
}
