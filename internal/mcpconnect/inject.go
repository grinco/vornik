package mcpconnect

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/mcpauth"
	"vornik.io/vornik/internal/persistence"
)

// Step 5 of the design: turning a stored grant into the header a tool call actually carries, and
// refreshing it before it expires.

// refreshSkew refreshes a token slightly before it expires, so a call that starts valid cannot
// finish 401. Chosen larger than any single MCP request's timeout.
const refreshSkew = 2 * time.Minute

// ErrNeedsReconnect reports that a server's grant can no longer be refreshed and a human must
// consent again. Non-retryable by construction: the caller must fail the tool call rather than
// loop (design §8's "never loop").
var ErrNeedsReconnect = errors.New("mcp oauth grant needs operator reconnect")

// AccessToken returns the bearer token to present to a server, refreshing it first when it is
// expired or about to be.
//
// Returns ("", nil) when the pair has no grant at all — the caller decides whether that is fatal
// (an oauth-mode server has nothing to present) or simply "not configured".
//
// The refresh path is the §6 design in three moves:
//  1. Take the cross-process lock for this grant, so two daemons do not both burn a refresh —
//     one of which would consume a single-use refresh token that some authorization servers treat
//     as replay and answer by revoking the whole grant.
//  2. Re-read INSIDE the lock (the classic double-check): the lock winner refreshes, the loser
//     wakes to find a fresh token and uses it.
//  3. Persist with the conditional swap, so even without the lock a concurrent refresh loses
//     cleanly instead of clobbering the winner's rotated token.
func (c *Connector) AccessToken(ctx context.Context, ref ServerRef) (string, error) {
	tok, err := c.Tokens.Get(ctx, ref.ProjectID, ref.ServerName)
	if errors.Is(err, persistence.ErrNotFound) {
		// Every wiring pass asks about every configured oauth server,
		// most of which the operator has never connected. That is not an
		// error, and must not be reported as one.
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("mcpconnect: read grant: %w", err)
	}
	if tok == nil {
		return "", nil
	}
	if tok.NeedsReconnect {
		return "", fmt.Errorf("%w: %q", ErrNeedsReconnect, ref.ServerName)
	}
	// F5, enforced at use rather than trusted from config: a token issued for
	// one resource must never be presented to another. Two products sharing an
	// authorization server have distinct resources, and a config edit that
	// repoints a server's URL must invalidate the grant rather than silently
	// reuse it against a new audience.
	if err := c.assertResourceMatches(ctx, ref, tok); err != nil {
		return "", err
	}
	if tok.Usable(time.Now(), refreshSkew) {
		return tok.AccessToken, nil
	}
	if !tok.Refreshable() {
		// Expired with no refresh token is needs_reconnect by definition
		// (§6). Recorded so the UI and the CLI can say which it is.
		if markErr := c.Tokens.MarkNeedsReconnect(ctx, ref.ProjectID, ref.ServerName); markErr != nil {
			c.Logger.Error().Err(markErr).Str("server", ref.ServerName).
				Msg("mcp oauth: could not flag the grant as needing reconnect")
		}
		return "", fmt.Errorf("%w: %q (expired with no refresh token)", ErrNeedsReconnect, ref.ServerName)
	}

	var refreshed string
	lockErr := c.Tokens.WithRefreshLock(ctx, ref.ProjectID, ref.ServerName, func(ctx context.Context) error {
		current, err := c.Tokens.Get(ctx, ref.ProjectID, ref.ServerName)
		if errors.Is(err, persistence.ErrNotFound) {
			return fmt.Errorf("%w: %q (grant disappeared)", ErrNeedsReconnect, ref.ServerName)
		}
		if err != nil {
			return fmt.Errorf("mcpconnect: re-read grant under lock: %w", err)
		}
		if current == nil {
			return fmt.Errorf("%w: %q (grant disappeared)", ErrNeedsReconnect, ref.ServerName)
		}
		// The double-check: somebody else may have refreshed while we
		// waited for the lock, in which case there is nothing to do.
		if current.Usable(time.Now(), refreshSkew) {
			refreshed = current.AccessToken
			return nil
		}
		if !current.Refreshable() {
			return fmt.Errorf("%w: %q", ErrNeedsReconnect, ref.ServerName)
		}
		next, err := c.refreshLocked(ctx, ref, current)
		if err != nil {
			return err
		}
		refreshed = next
		return nil
	})
	if lockErr != nil {
		return "", lockErr
	}
	return refreshed, nil
}

// refreshLocked performs the token refresh and persists it. Called with the grant's lock held.
func (c *Connector) refreshLocked(ctx context.Context, ref ServerRef, current *persistence.MCPOAuthToken) (string, error) {
	md, err := c.resolveMetadata(ctx, ref)
	if err != nil {
		return "", err
	}
	// The resource the token was ISSUED for, not whatever discovery says now:
	// refreshing must not silently re-audience a grant.
	md.Resource = current.Resource

	creds, err := c.configuredClient(ref)
	if err != nil {
		return "", err
	}
	if creds.ID == "" {
		creds.ID = current.ClientID
	}

	tr, err := mcpauth.Refresh(ctx, c.HTTP, md, creds, current.RefreshToken)
	if err != nil {
		// invalid_grant is terminal: the grant is gone at the vendor and no
		// amount of retrying brings it back, so flag it and stop. Any other
		// error may be transient (a 503 at the token endpoint), so the grant
		// is left alone to be retried on the next call.
		if errors.Is(err, mcpauth.ErrInvalidGrant) {
			if markErr := c.Tokens.MarkNeedsReconnect(ctx, ref.ProjectID, ref.ServerName); markErr != nil {
				c.Logger.Error().Err(markErr).Str("server", ref.ServerName).
					Msg("mcp oauth: could not flag the grant as needing reconnect")
			}
			c.Logger.Warn().Str("server", ref.ServerName).Str("project", ref.ProjectID).
				Msg("mcp oauth: the authorization server rejected the refresh token — a human must reconnect")
			return "", fmt.Errorf("%w: %q (the authorization server rejected the refresh token)",
				ErrNeedsReconnect, ref.ServerName)
		}
		return "", err
	}

	next := &persistence.MCPOAuthToken{
		ProjectID:    ref.ProjectID,
		ServerName:   ref.ServerName,
		Resource:     current.Resource,
		ClientID:     creds.ID,
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		ExpiresAt:    tr.ExpiresAt,
		Scopes:       current.Scopes,
	}
	if tr.RefreshToken == "" {
		// Not every server rotates. Keeping the old one is what lets the
		// NEXT refresh work at all.
		next.RefreshToken = current.RefreshToken
	}
	if tr.Scopes != "" {
		next.Scopes = tr.Scopes
	}

	won, err := c.Tokens.SwapRefreshToken(ctx, current.RefreshToken, next)
	if err != nil {
		return "", fmt.Errorf("mcpconnect: persist refreshed grant: %w", err)
	}
	if !won {
		// Another process rotated the token under us. Its value is the live
		// one; ours is already spent, so reload rather than overwrite.
		reloaded, err := c.Tokens.Get(ctx, ref.ProjectID, ref.ServerName)
		if err != nil || reloaded == nil {
			return "", fmt.Errorf("mcpconnect: lost the refresh race and could not reload the grant: %w", err)
		}
		return reloaded.AccessToken, nil
	}
	c.Logger.Debug().Str("server", ref.ServerName).Str("project", ref.ProjectID).
		Msg("mcp oauth: access token refreshed")
	return next.AccessToken, nil
}

// assertResourceMatches refuses to present a token whose audience no longer matches the server it
// would be sent to.
//
// Cheap and config-only: it compares the stored resource with the server's configured URL, and
// only when the configured URL is a prefix-incompatible different origin. Discovery is NOT re-run
// here — that would put an HTTP round trip on every tool call — so this catches the case that
// actually happens (an operator repoints a server's url and forgets to reconnect) without
// pretending to be a full audience check.
func (c *Connector) assertResourceMatches(ctx context.Context, ref ServerRef, tok *persistence.MCPOAuthToken) error {
	_ = ctx
	if tok.Resource == "" || ref.URL == "" {
		return nil
	}
	if sameOrigin(tok.Resource, ref.URL) {
		return nil
	}
	c.Logger.Warn().
		Str("server", ref.ServerName).
		Str("project", ref.ProjectID).
		Str("grant_resource", tok.Resource).
		Str("configured_url", ref.URL).
		Msg("mcp oauth: the stored grant was issued for a different origin than this server's configured url — refusing to present it")
	return fmt.Errorf("%w: %q was reconfigured to a different origin than its grant was issued for; reconnect it",
		ErrNeedsReconnect, ref.ServerName)
}

// sameOrigin compares scheme+host of two URLs, tolerating path differences: the RFC 8707 resource
// is often the server URL with a normalised path.
func sameOrigin(a, b string) bool {
	return originOf(a) == originOf(b)
}

func originOf(raw string) string {
	s := strings.TrimSpace(raw)
	scheme, rest, ok := strings.Cut(s, "://")
	if !ok {
		return s
	}
	host, _, _ := strings.Cut(rest, "/")
	return strings.ToLower(scheme + "://" + host)
}
