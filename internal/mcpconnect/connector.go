// Package mcpconnect orchestrates the OAuth consent flow for MCP servers: it holds the
// in-flight authorization attempts, drives internal/mcpauth's discovery + registration + token
// exchange, persists the resulting grant, and records who consented to what.
//
// Separate from internal/mcpauth because that package is a LEAF by design — it must not depend
// on persistence or the registry, so the layer that knows about repositories, config trees and
// secrets lives here. mcpauth is the protocol; this is the wiring.
//
// Design: https://docs.vornik.io steps 3-5.
package mcpconnect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/mcpauth"
	"vornik.io/vornik/internal/persistence"
)

// CallbackPath is the single redirect URI path for this deployment.
//
// `/auth/<purpose>/callback` is the standard for a surface a human's BROWSER reaches, matching the
// existing `/auth/<provider>/callback` login flow; `/api/v1/…` is for surfaces another MACHINE
// reaches (see the callback-and-webhook-surfaces design). It is deliberately NOT under `/ui`: a
// redirect target is not a page in the console's information architecture, and it is not inside the
// control-plane hub either, because an operator who hand-edits mcp.servers needs a browser callback
// whether or not they ever open the hub (MCP auth design §7.2a).
//
// Registered as an EXACT pattern on the root mux, so it carries session authentication while the
// `/auth/` prefix that serves login stays public — `http.ServeMux` resolves the longest matching
// pattern, and neither registration depends on the other existing.
const CallbackPath = "/auth/mcp/callback"

// pendingTTL bounds how long an unfinished authorization stays resumable. Authorization codes
// live seconds to minutes, so anything older is a dead attempt whose state must not be
// replayable.
const pendingTTL = 10 * time.Minute

var (
	// ErrNoPublicBaseURL reports the §7.1 precondition: OAuth 2.1 requires a localhost or
	// HTTPS redirect URI, and Vornik commonly runs on a LAN address behind a tunnel, so the
	// callback must be an absolute HTTPS URL the vendor can reach.
	ErrNoPublicBaseURL = errors.New("server.public_base_url must be set to an https:// origin before an MCP server can be connected")

	// ErrUnknownState reports a callback whose state does not match a pending authorization:
	// an expired attempt, a daemon restart mid-flow, or a forged callback.
	ErrUnknownState = errors.New("no pending MCP authorization matches this callback")

	// ErrNotOAuth reports that the named server's auth block is not mode: oauth, so there is
	// nothing to consent to.
	ErrNotOAuth = errors.New("server is not configured with auth mode: oauth")
)

// SecretSource resolves a secret:// name to its value — satisfied by mcpauth.EnvSecretSource.
type SecretSource interface {
	Get(name string) (string, bool)
}

// AuditSink records the consent grant. Narrowed to the one method used so tests need no
// repository.
type AuditSink interface {
	Insert(ctx context.Context, entry *persistence.AdminAuditEntry) error
}

// Connector drives consent for one daemon.
type Connector struct {
	// Tokens persists grants. Required.
	Tokens persistence.MCPOAuthTokenRepository
	// Secrets resolves auth.client_secret_from. Required for confidential clients.
	Secrets SecretSource
	// Audit records who consented to what. Optional — a nil sink degrades to a log line
	// rather than blocking a consent the operator is standing in front of.
	Audit AuditSink
	// HTTP is the client used for discovery and token requests. Nil = http.DefaultClient.
	HTTP *http.Client
	// BaseURL returns server.public_base_url. A FUNCTION, not a captured string:
	// this connector is built once at boot, and an operator who sets
	// public_base_url and reloads config must not have to restart the daemon to
	// make Connect work — nor have the callback keep pointing at a stale origin
	// after the value changes. Empty (or nil) fails the §7.1 precondition up front.
	BaseURL func() string
	// Resolver maps a (project, server) pair to its configuration. Injected by the wiring
	// layer, which owns the registry and the daemon catalog — keeping it a function means
	// this package needs no registry dependency and stays testable with a literal.
	Resolver ServerResolver
	// OnGranted is called after a grant is stored, with the project and server
	// it was stored for. Optional; nil is a no-op.
	//
	// It exists because storing the token is not the same as USING it. The
	// access token is injected into an MCP client's headers when that client is
	// wired, which happens at boot and on config reload — so before this hook,
	// a completed consent changed nothing until the operator separately
	// reloaded or restarted. The callback page said "Connected" while the tool
	// surface kept sending unauthenticated requests and the control-plane badge
	// kept reporting that authentication was required. Reported 2026-08-05
	// against the atlassian server: consent at 22:21:26 did nothing, and a
	// manual `vornikctl config reload` at 22:24:32 is what actually connected
	// it.
	//
	// The implementation is expected to re-wire and re-probe. It must not
	// block: this runs on the operator's callback request, and re-dialling
	// every MCP server can take tens of seconds.
	OnGranted func(projectID, serverName string)
	Logger    zerolog.Logger

	mu      sync.Mutex
	pending map[string]*pendingAuth
}

// pendingAuth is one in-flight authorization attempt, keyed by its state parameter.
//
// In-memory rather than persisted, deliberately: the window is minutes, the operator is standing
// in front of the browser, and a restart mid-consent is recoverable by clicking Connect again.
// Persisting it would put a PKCE verifier — a short-lived secret — in the database for no gain.
type pendingAuth struct {
	ProjectID   string
	ServerName  string
	Metadata    mcpauth.Metadata
	Creds       mcpauth.ClientCredentials
	PKCE        mcpauth.PKCE
	Scopes      []string
	RedirectURI string
	ConnectedBy string
	CreatedAt   time.Time
}

// ServerRef identifies the server being connected and carries its auth block.
type ServerRef struct {
	// ProjectID is "" for a daemon-scope server.
	ProjectID  string
	ServerName string
	URL        string
	Auth       mcpauth.Auth
	// GrantedSecrets is the project's permissions.secrets allowlist. Ignored for
	// daemon-scope servers, which have no project allowlist (design §9).
	GrantedSecrets []string
	// InheritedFrom names the project whose request produced a ref that was
	// REDIRECTED to daemon scope — a name-only subscriber inheriting the daemon
	// server's credential (design §9). ProjectID is "" in that case, because
	// that is where the one shared grant lives; this field keeps the asking
	// project reportable, so a surface can say "connected for project X via the
	// daemon-scope grant" instead of having to choose between two half-truths.
	//
	// Empty for a project that owns its credential and for a direct
	// daemon-scope lookup. Never part of a storage key.
	InheritedFrom string
}

// ServerResolver resolves a (project, server) pair to its configuration. projectID "" means the
// daemon-scope catalog.
type ServerResolver func(projectID, serverName string) (ServerRef, bool)

// ResolveServer looks up a configured server. Returns false when the name is unknown in that
// scope, or when no resolver was wired.
func (c *Connector) ResolveServer(projectID, serverName string) (ServerRef, bool) {
	if c.Resolver == nil {
		return ServerRef{}, false
	}
	return c.Resolver(projectID, serverName)
}

// Grant returns the stored grant for a pair, or nil when there is none. Exposed for the status
// endpoint and the control-plane row: it carries the token, so callers must never render it.
func (c *Connector) Grant(ctx context.Context, projectID, serverName string) (*persistence.MCPOAuthToken, error) {
	return c.Tokens.Get(ctx, projectID, serverName)
}

// BeginResult is what the operator needs in order to consent.
type BeginResult struct {
	// AuthorizationURL is opened in a browser.
	AuthorizationURL string
	// Resource and Scopes are what the CLI DISPLAYS before the operator consents, so they
	// see the ask; after the callback it compares the recorded grant against them (§7.2a,
	// review round-2 N1 — the CLI is a verifier, not a success light).
	Resource string
	Scopes   []string
	State    string
}

// RedirectURI is the single redirect URI for this deployment.
func (c *Connector) RedirectURI() (string, error) {
	raw := ""
	if c.BaseURL != nil {
		raw = c.BaseURL()
	}
	base := strings.TrimRight(strings.TrimSpace(raw), "/")
	if base == "" {
		return "", ErrNoPublicBaseURL
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("%w: %q does not parse as a URL", ErrNoPublicBaseURL, base)
	}
	// OAuth 2.1 requires https, with loopback the only exception. Fail BEFORE the
	// operator is sent to the vendor's consent screen: failing after consent wastes
	// their time and strands an authorization code (§7.1).
	if u.Scheme != "https" && !isLoopback(u.Hostname()) {
		return "", fmt.Errorf("%w: %q is neither https nor loopback", ErrNoPublicBaseURL, base)
	}
	return base + CallbackPath, nil
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// Begin starts an authorization attempt: discover, resolve or register a client, and build the
// URL the operator opens. Nothing is persisted until Complete — a half-finished consent leaves
// no grant behind.
func (c *Connector) Begin(ctx context.Context, ref ServerRef, connectedBy string) (BeginResult, error) {
	if ref.Auth.EffectiveMode() != mcpauth.ModeOAuth {
		return BeginResult{}, fmt.Errorf("%w: %q is mode %q", ErrNotOAuth, ref.ServerName, ref.Auth.EffectiveMode())
	}
	redirectURI, err := c.RedirectURI()
	if err != nil {
		return BeginResult{}, err
	}
	// §7.2a: a DCR registration PINS its redirect_uris, so a client registered under a
	// redirect URI this deployment no longer serves is dead at the vendor — the
	// authorization request is rejected for a redirect_uri the client never registered.
	// Drop it here, BEFORE resolveClient can pick it up, so this connect re-registers
	// instead of walking the operator into a vendor error. Grants keep their tokens but
	// are flagged needs_reconnect rather than failing silently later.
	c.invalidateStaleClients(ctx, redirectURI)

	md, err := c.resolveMetadata(ctx, ref)
	if err != nil {
		return BeginResult{}, err
	}

	scopes := ref.Auth.Scopes
	if len(scopes) == 0 {
		// Design §12.3's implementation default: request what the PRM
		// advertises, and let the operator see the full list on the hand-off
		// so an over-broad grant is visible rather than implicit.
		scopes = md.ScopesSupported
	}
	if ref.ProjectID == "" {
		// §12.2, hardened from "warning only": a daemon-scope token is
		// reachable from EVERY project, so write scopes there must be named
		// explicitly in config rather than inherited from the PRM default.
		if len(ref.Auth.Scopes) == 0 {
			scopes = readOnlyScopes(md.ScopesSupported)
		}
	}

	creds, err := c.resolveClient(ctx, ref, md, redirectURI, scopes)
	if err != nil {
		return BeginResult{}, err
	}

	pkce, err := mcpauth.NewPKCE()
	if err != nil {
		return BeginResult{}, err
	}
	state, err := mcpauth.NewState()
	if err != nil {
		return BeginResult{}, err
	}
	authURL, err := mcpauth.AuthorizationURL(md, creds, redirectURI, scopes, state, pkce)
	if err != nil {
		return BeginResult{}, err
	}

	c.mu.Lock()
	if c.pending == nil {
		c.pending = make(map[string]*pendingAuth)
	}
	c.reapExpiredLocked()
	c.pending[state] = &pendingAuth{
		ProjectID: ref.ProjectID, ServerName: ref.ServerName,
		Metadata: md, Creds: creds, PKCE: pkce, Scopes: scopes,
		RedirectURI: redirectURI, ConnectedBy: connectedBy, CreatedAt: time.Now(),
	}
	c.mu.Unlock()

	return BeginResult{
		AuthorizationURL: authURL,
		Resource:         md.Resource,
		Scopes:           scopes,
		State:            state,
	}, nil
}

// Complete finishes an authorization from the callback: exchange the code, persist the grant,
// and record the consent.
func (c *Connector) Complete(ctx context.Context, state, code string) (*persistence.MCPOAuthToken, error) {
	c.mu.Lock()
	c.reapExpiredLocked()
	p := c.pending[state]
	// One-shot: the state is consumed whether or not the exchange succeeds, so a
	// leaked callback URL cannot be replayed.
	delete(c.pending, state)
	c.mu.Unlock()

	if p == nil {
		return nil, ErrUnknownState
	}
	if strings.TrimSpace(code) == "" {
		return nil, errors.New("mcpconnect: callback carried no authorization code")
	}

	tr, err := mcpauth.ExchangeCode(ctx, c.HTTP, p.Metadata, p.Creds, p.RedirectURI, code, p.PKCE)
	if err != nil {
		return nil, err
	}

	granted := tr.Scopes
	if granted == "" {
		// Not every server echoes `scope`; recording what we asked for is
		// better than recording nothing, and the CLI's comparison then
		// reports a match rather than a spurious mismatch.
		granted = strings.Join(p.Scopes, " ")
	}
	tok := &persistence.MCPOAuthToken{
		ProjectID:    p.ProjectID,
		ServerName:   p.ServerName,
		Resource:     p.Metadata.Resource,
		ClientID:     p.Creds.ID,
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		ExpiresAt:    tr.ExpiresAt,
		Scopes:       granted,
		// What the stored ClientID is registered under (§7.2a). Recorded from the
		// flow that just completed rather than recomputed, so it is the URI the
		// vendor actually saw.
		RedirectURI: p.RedirectURI,
		ConnectedBy: p.ConnectedBy,
		ConnectedAt: time.Now().UTC(),
	}
	if err := c.Tokens.Upsert(ctx, tok); err != nil {
		return nil, fmt.Errorf("mcpconnect: persist grant: %w", err)
	}
	c.recordGrant(ctx, "mcp.oauth.connect", tok)
	c.notifyGranted(tok.ProjectID, tok.ServerName)
	return tok, nil
}

// Disconnect deletes a grant and records it. Config is untouched, so reconnecting needs no
// config change.
func (c *Connector) Disconnect(ctx context.Context, projectID, serverName, actor string) error {
	existing, err := c.Tokens.Get(ctx, projectID, serverName)
	if err != nil {
		return err
	}
	if err := c.Tokens.Delete(ctx, projectID, serverName); err != nil {
		return err
	}
	if existing == nil {
		existing = &persistence.MCPOAuthToken{ProjectID: projectID, ServerName: serverName}
	}
	existing.ConnectedBy = actor
	c.recordGrant(ctx, "mcp.oauth.disconnect", existing)
	// Disconnect needs the re-wire just as much as connect does: without it the
	// client keeps its injected Authorization header and goes on using a grant
	// the operator just revoked.
	c.notifyGranted(projectID, serverName)
	return nil
}

// notifyGranted invokes OnGranted nil-safely. Panic-guarded because this runs
// on the operator's callback request and a wiring bug in the hook must not take
// the response — or the daemon — down after a consent that already succeeded.
func (c *Connector) notifyGranted(projectID, serverName string) {
	if c.OnGranted == nil {
		return
	}
	defer func() {
		if rec := recover(); rec != nil {
			c.Logger.Error().Interface("panic", rec).
				Str("project", projectID).Str("server", serverName).
				Msg("mcp oauth: OnGranted hook panicked; the grant is stored but the re-wire did not run")
		}
	}()
	c.OnGranted(projectID, serverName)
}

// GrantRecord is the §7.2 ledger payload — no token, no config diff, so it is safe to display,
// retain and export.
type GrantRecord struct {
	ProjectID  string `json:"project_id"`
	ServerName string `json:"server_name"`
	Resource   string `json:"resource"`
	Scopes     string `json:"scopes"`
	Actor      string `json:"actor"`
	// DaemonScope makes the §12.2 blast radius explicit in the record itself: this grant is
	// reachable from every project on the daemon.
	DaemonScope bool `json:"daemon_scope"`
}

// recordGrant writes the consent record.
//
// DEVIATION from design §7.2, which specified a proposal-shaped row in control_plane_proposals
// with a distinct operation type. Recorded in admin_audit instead — whose documented Action
// examples already include "mcp.refresh" and whose Target is documented as "typically a project
// ID, key ID, or MCP server name", and which has a UI at /ui/admin/audit today.
//
// The reason is that §7.2 also requires the record be written at COMPLETION and NOT gated on
// approval. A proposal row that is never approved or applied would sit in the control-plane
// inbox looking actionable forever, which is worse than no record. admin_audit satisfies every
// stated property — visible, retained, exportable, no token, no diff — and is the trail an
// operator already reads for "who did what, when".
func (c *Connector) recordGrant(ctx context.Context, action string, tok *persistence.MCPOAuthToken) {
	rec := GrantRecord{
		ProjectID:   tok.ProjectID,
		ServerName:  tok.ServerName,
		Resource:    tok.Resource,
		Scopes:      tok.Scopes,
		Actor:       tok.ConnectedBy,
		DaemonScope: tok.ProjectID == "",
	}
	payload, err := json.Marshal(rec)
	if err != nil {
		payload = []byte("{}")
	}
	log := c.Logger.Info().
		Str("action", action).
		Str("project", tok.ProjectID).
		Str("server", tok.ServerName).
		Str("resource", tok.Resource).
		Str("scopes", tok.Scopes).
		Str("actor", tok.ConnectedBy)

	if c.Audit == nil {
		log.Msg("mcp oauth: grant recorded in the log only (no audit sink wired)")
		return
	}
	entry := &persistence.AdminAuditEntry{
		ID:        persistence.GenerateID("admaud"),
		Timestamp: time.Now().UTC(),
		Principal: tok.ConnectedBy,
		Source:    "ui",
		Action:    action,
		Target:    grantTarget(tok),
		After:     string(payload),
	}
	if err := c.Audit.Insert(ctx, entry); err != nil {
		// The consent already happened; failing the operator's flow over an
		// audit write would be worse than a loud log line.
		c.Logger.Error().Err(err).
			Str("action", action).
			Str("server", tok.ServerName).
			Msg("mcp oauth: failed to record the consent grant")
		return
	}
	log.Msg("mcp oauth: grant recorded")
}

func grantTarget(tok *persistence.MCPOAuthToken) string {
	if tok.ProjectID == "" {
		return "daemon/" + tok.ServerName
	}
	return tok.ProjectID + "/" + tok.ServerName
}

// resolveMetadata returns the server's authorization metadata, preferring explicitly configured
// endpoints over discovery. F4: a server that publishes no PRM anywhere (Intercom) is only
// reachable this way.
func (c *Connector) resolveMetadata(ctx context.Context, ref ServerRef) (mcpauth.Metadata, error) {
	if ref.Auth.AuthorizationEndpoint != "" && ref.Auth.TokenEndpoint != "" {
		return mcpauth.Metadata{
			Resource:              ref.URL,
			AuthorizationEndpoint: ref.Auth.AuthorizationEndpoint,
			TokenEndpoint:         ref.Auth.TokenEndpoint,
			ScopesSupported:       ref.Auth.Scopes,
		}, nil
	}
	md, err := mcpauth.Discover(ctx, c.HTTP, ref.URL)
	if err != nil {
		return mcpauth.Metadata{}, err
	}
	return md, nil
}

// resolveClient returns the OAuth client to use: the configured one, the stored DCR one, or a
// freshly registered one.
//
// A stored client_id is reused across projects and daemon processes — one client per
// (deployment, server), which is why it lives on the token row (§7.2a). Re-registering per
// connect would leave unbounded garbage clients at the authorization server.
func (c *Connector) resolveClient(ctx context.Context, ref ServerRef, md mcpauth.Metadata, redirectURI string, scopes []string) (mcpauth.ClientCredentials, error) {
	if ref.Auth.ClientID != "" {
		return c.configuredClient(ref)
	}

	// Reuse a previously registered client for this server, from any scope on this
	// deployment: the registration pinned the redirect URI, which is shared.
	if stored, err := c.storedClient(ctx, ref); err == nil && stored.ID != "" {
		return stored, nil
	}

	if !md.SupportsDCR() {
		return mcpauth.ClientCredentials{}, fmt.Errorf("%w for %q — set auth.client_id (and auth.client_secret_from if the vendor issued a secret)",
			mcpauth.ErrNoDCR, ref.ServerName)
	}
	return mcpauth.Register(ctx, c.HTTP, md, redirectURI, scopes)
}

// configuredClient resolves an operator-configured client, including its secret when the vendor
// issued one. This is the F1 path: Slack, GitHub and Box offer no dynamic registration, and
// Slack's authorization server accepts only a confidential client.
func (c *Connector) configuredClient(ref ServerRef) (mcpauth.ClientCredentials, error) {
	creds := mcpauth.ClientCredentials{ID: ref.Auth.ClientID}
	if ref.Auth.ClientSecretFrom == "" {
		return creds, nil
	}
	name, ok := mcpauth.ParseSecretRef(ref.Auth.ClientSecretFrom)
	if !ok {
		return creds, errors.New("mcpconnect: auth.client_secret_from is not a secret:// reference")
	}
	// permissions.secrets gates the OAuth path exactly as it gates mode:
	// static — a project must not reach a client secret it was never granted.
	// Daemon-scope servers have no project allowlist to check against.
	if ref.ProjectID != "" {
		if err := ref.Auth.ValidateSecretGrants(ref.GrantedSecrets); err != nil {
			return creds, fmt.Errorf("mcpconnect: %w", err)
		}
	}
	secret, ok := c.secretValue(name)
	if !ok {
		return creds, fmt.Errorf("%w: client_secret_from names secret %q, which the secret store does not hold",
			mcpauth.ErrSecretUnresolved, name)
	}
	creds.Secret = secret
	return creds, nil
}

// storedClient looks for an already-registered client id for this server.
//
// A dynamically registered client's SECRET is not stored (the token row has no column for it,
// deliberately — see §6's column list), so a confidential DCR client cannot be reused. That is
// acceptable because DCR and confidential-client are disjoint in the survey: every AS offering
// DCR accepts a public client. A confidential client is always operator-configured, and that
// path is handled above.
func (c *Connector) storedClient(ctx context.Context, ref ServerRef) (mcpauth.ClientCredentials, error) {
	tok, err := c.Tokens.Get(ctx, ref.ProjectID, ref.ServerName)
	if err != nil {
		return mcpauth.ClientCredentials{}, err
	}
	if tok != nil && tok.ClientID != "" {
		return mcpauth.ClientCredentials{ID: tok.ClientID}, nil
	}
	if ref.ProjectID != "" {
		// Fall back to the daemon-scope row for the same server: same
		// deployment, same redirect URI, so the same client applies.
		if tok, err := c.Tokens.Get(ctx, "", ref.ServerName); err == nil && tok != nil && tok.ClientID != "" {
			return mcpauth.ClientCredentials{ID: tok.ClientID}, nil
		}
	}
	return mcpauth.ClientCredentials{}, nil
}

// InvalidateStaleClients drops stored clients registered under a redirect URI this
// deployment no longer serves, returning how many grants were flagged.
//
// Called at boot and before each connect. Exported for the boot path; Begin uses the
// unexported wrapper so a sweep failure cannot fail a connect that might still work.
func (c *Connector) InvalidateStaleClients(ctx context.Context) (int, error) {
	redirectURI, err := c.RedirectURI()
	if err != nil {
		// public_base_url unset or not https: there is no current URI to compare
		// against, so invalidating would flag every grant on a misconfiguration.
		// Connect already refuses in that state with an actionable error (§7.1).
		return 0, err
	}
	if c.Tokens == nil {
		return 0, nil
	}
	return c.Tokens.InvalidateStaleRedirectURIs(ctx, redirectURI)
}

// invalidateStaleClients is the best-effort form used on the connect path: a sweep that
// errors must not block a consent, because the stored client may well still be valid and
// the operator is standing there waiting.
func (c *Connector) invalidateStaleClients(ctx context.Context, redirectURI string) {
	if c.Tokens == nil {
		return
	}
	n, err := c.Tokens.InvalidateStaleRedirectURIs(ctx, redirectURI)
	if err != nil {
		c.Logger.Warn().Err(err).Msg("mcpconnect: could not sweep stale redirect URIs; continuing with the stored client")
		return
	}
	if n > 0 {
		c.Logger.Warn().Int("grants", n).Str("redirect_uri", redirectURI).
			Msg("mcpconnect: dropped stored OAuth client(s) registered under a previous redirect URI — affected grants need reconnect")
	}
}

func (c *Connector) secretValue(name string) (string, bool) {
	if c.Secrets == nil {
		return "", false
	}
	return c.Secrets.Get(name)
}

// reapExpiredLocked drops attempts past the TTL. Called on every Begin and Complete rather than
// on a timer: the map is tiny, and a background goroutine for a handful of entries is more
// machinery than the problem deserves.
func (c *Connector) reapExpiredLocked() {
	cutoff := time.Now().Add(-pendingTTL)
	for state, p := range c.pending {
		if p.CreatedAt.Before(cutoff) {
			delete(c.pending, state)
		}
	}
}

// readOnlyScopes filters a PRM's advertised scopes down to the ones that only read.
//
// §12.2's hardening for daemon-scope servers: a token there is reachable from every project, so
// a compromise in ANY project reaches that vendor account. Write scopes must be named explicitly
// in config rather than inherited from the PRM default.
//
// The filter is deliberately conservative — it keeps a scope only when it clearly reads — so an
// unrecognised scope is DROPPED rather than assumed harmless. An operator who needs it names it.
func readOnlyScopes(scopes []string) []string {
	var out []string
	for _, s := range scopes {
		lower := strings.ToLower(s)
		switch {
		case lower == "offline_access", lower == "openid", lower == "profile", lower == "email":
			// Not resource access at all; needed for refresh to work.
			out = append(out, s)
		case strings.HasPrefix(lower, "read:"), strings.HasPrefix(lower, "read."),
			strings.HasSuffix(lower, ":read"), strings.HasSuffix(lower, ".read"),
			strings.HasSuffix(lower, ".readonly"):
			out = append(out, s)
		}
	}
	return out
}
