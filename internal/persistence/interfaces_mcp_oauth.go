package persistence

import (
	"context"
	"time"
)

// Step 4 of the MCP server authentication design
// (https://docs.vornik.io §6) — the token store behind
// per-project OAuth service identity for MCP servers.
//
// One operator connects a server once per project; every task in that project then uses that
// token, which is what makes autonomous and cron runs work (design §3). The row therefore
// belongs to the (project, server) pair rather than to a UI user — a per-user token has nobody
// to borrow from when autonomy fires at 03:00.

// MCPOAuthToken is one project's (or the daemon's) OAuth grant for one MCP server.
type MCPOAuthToken struct {
	// ProjectID scopes the token. "" means a daemon-scope server (config.yaml's
	// mcp.servers), which is reachable from every project — see design §9's warning.
	ProjectID  string
	ServerName string

	// Resource is the RFC 8707 canonical resource URI the token was issued FOR, stored
	// rather than derived. F5: Trello and Jira share one authorization server but are
	// distinct resources with distinct scope sets, so a Jira token must never be presented
	// to Trello. The injection path checks this before attaching a header.
	Resource string

	// ClientID is the OAuth client this grant belongs to. Stored because a
	// dynamically-registered (RFC 7591) client is per-DEPLOYMENT and must survive a daemon
	// restart — re-registering on every boot would leave unbounded garbage clients at the
	// authorization server. Scoping rules are design §7.2a: one client per (deployment,
	// server), which is exactly this row's key minus the project.
	ClientID string

	AccessToken string
	// RefreshToken uses "" as the null-substitute rather than SQL NULL: not every flow
	// issues one, and an empty string keeps the conditional `UPDATE … WHERE refresh_token =
	// <the one we used>` comparison two-valued instead of three-valued. A row with no
	// refresh token and a past ExpiresAt is NeedsReconnect by definition.
	RefreshToken string

	// ExpiresAt is nil when the authorization server issued no expiry (rare, and treated as
	// "valid until it 401s").
	ExpiresAt *time.Time

	// Scopes is the space-separated GRANTED scope set, which is not necessarily what was
	// requested — an authorization server may narrow it, and the CLI compares the two so a
	// silently-reduced grant is visible rather than surfacing later as a puzzling 403.
	Scopes string

	// ConnectedBy is the identity that consented. With per-project service identity the
	// vendor's own audit log shows a single actor for all agent activity, so this is the
	// only place the human behind the grant is recorded (design §9).
	// RedirectURI is the callback the stored ClientID is registered under at the
	// authorization server (§7.2a). A DCR registration PINS its redirect_uris, so a
	// changed server.public_base_url makes the client unusable — the vendor rejects an
	// authorization request carrying a redirect_uri it never registered. Recorded so
	// that change is DETECTABLE: nothing could see it before, neither an origin change
	// nor a path change.
	//
	// "" means unknown (every row written before migration 151). Unknown is not a
	// mismatch: dropping a working client because we cannot prove its redirect URI
	// would break connections to punish our own missing data.
	RedirectURI string

	ConnectedBy string

	ConnectedAt time.Time
	UpdatedAt   time.Time

	// NeedsReconnect is set when a refresh failed unrecoverably (the grant was revoked, the
	// refresh token was rejected) or when the stored client's redirect URI no longer matches
	// public_base_url. It is the difference between "expired, will refresh itself" and
	// "a human must consent again", which the UI and the tool-call error path both need.
	NeedsReconnect bool
}

// Expired reports whether the access token is past its expiry, with a skew allowance so a token
// that will die mid-request is refreshed before use rather than after a 401.
func (t *MCPOAuthToken) Expired(now time.Time, skew time.Duration) bool {
	if t == nil {
		return true
	}
	if t.ExpiresAt == nil {
		return false
	}
	return !now.Add(skew).Before(*t.ExpiresAt)
}

// Usable reports whether this token may be presented to a server right now.
func (t *MCPOAuthToken) Usable(now time.Time, skew time.Duration) bool {
	return t != nil && !t.NeedsReconnect && t.AccessToken != "" && !t.Expired(now, skew)
}

// Refreshable reports whether a refresh attempt is worth making.
func (t *MCPOAuthToken) Refreshable() bool {
	return t != nil && !t.NeedsReconnect && t.RefreshToken != ""
}

// MCPOAuthTokenRepository persists MCP OAuth grants.
//
// Postgres and SQLite implementations diverge on dialect (TIMESTAMPTZ vs RFC3339 TEXT, ON
// CONFLICT syntax) and on locking, which is why the shared repotest contract suite runs on both
// backends — `go test ./...` is SQLite-only and would not catch a Postgres-side break.
type MCPOAuthTokenRepository interface {
	// Get returns the grant, or (nil, ErrNotFound) when the (project, server)
	// pair has none. See internal/persistence/misscontract.
	//
	// Absence is ordinary here — every wiring pass asks about every configured
	// oauth server — so callers translate ErrNotFound rather than propagating
	// it (mcpconnect.Connector.Grant, mcpconnect.AccessToken).
	Get(ctx context.Context, projectID, serverName string) (*MCPOAuthToken, error)

	// Upsert writes the grant, replacing any existing one for the pair. Used by the
	// callback (first consent) and by a refresh that had no prior refresh token to compare
	// against.
	Upsert(ctx context.Context, tok *MCPOAuthToken) error

	// SwapRefreshToken atomically replaces the grant ONLY IF the stored refresh token is
	// still the one the caller used, and reports whether it won.
	//
	// This is the rotation guard. Nearly every authorization server surveyed rotates the
	// refresh token on use, so two daemons refreshing concurrently would otherwise have the
	// loser clobber the winner's rotated token — losing the connection and forcing a
	// re-consent. With the conditional update the loser simply loses, reloads, and uses the
	// fresh token (design §6).
	SwapRefreshToken(ctx context.Context, usedRefreshToken string, next *MCPOAuthToken) (bool, error)

	// MarkNeedsReconnect flags the grant as requiring human re-consent, recording nothing
	// about why beyond the flag — the reason belongs in the log, not in a row an operator
	// might mistake for an audit record.
	MarkNeedsReconnect(ctx context.Context, projectID, serverName string) error

	// InvalidateStaleRedirectURIs drops the stored client_id and marks
	// needs_reconnect on every grant whose recorded redirect URI is neither empty
	// nor equal to current, returning how many rows changed (§7.2a).
	//
	// One statement rather than list-then-update: the sweep runs at boot and again
	// before each connect, and a read-modify-write would race two daemons behind one
	// public_base_url — the exact deployment shape §7.2a says must share one client.
	//
	// Empty recorded values are LEFT ALONE. They mean "written before migration 151",
	// not "registered under something else", and dropping a working client over
	// missing data of our own would break connections for no gain.
	InvalidateStaleRedirectURIs(ctx context.Context, current string) (int, error)

	// Delete removes the grant (Disconnect). Config is untouched: the `auth:` block stays,
	// so reconnecting needs no config change.
	Delete(ctx context.Context, projectID, serverName string) error

	// ListForProject returns every grant for a project, for the control-plane row status.
	// Pass "" for the daemon scope.
	ListForProject(ctx context.Context, projectID string) ([]*MCPOAuthToken, error)

	// WithRefreshLock runs fn while holding a cross-process lock for the (project, server)
	// pair, so only one refresh is in flight per grant across every daemon sharing the
	// database.
	//
	// SwapRefreshToken already makes a concurrent refresh SAFE; this makes it non-wasteful.
	// Without it both daemons burn a refresh and one consumes a single-use refresh token,
	// which some authorization servers treat as replay and answer by revoking the whole
	// grant. On Postgres this is a transaction-scoped advisory lock; on SQLite —
	// single-daemon by construction — it simply calls fn.
	//
	// fn must re-read the token INSIDE the callback: the classic double-check, where the
	// lock winner refreshes and the loser wakes to find a fresh token and uses it.
	WithRefreshLock(ctx context.Context, projectID, serverName string, fn func(context.Context) error) error
}
