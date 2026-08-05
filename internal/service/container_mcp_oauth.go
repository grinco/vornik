package service

import (
	"context"
	"net/http"
	"time"

	"vornik.io/vornik/internal/mcpauth"
	"vornik.io/vornik/internal/mcpconnect"
	"vornik.io/vornik/internal/registry"
)

// MCP OAuth wiring (mcp-server-authentication-design.md steps 3-5). Owns the single Connector
// the UI callback, the CLI and the injection path all share — one instance because the in-flight
// authorization attempts (state → PKCE verifier) live in it, so a second instance would reject
// the callback for a consent the first one started.

// mcpOAuthHTTPTimeout bounds discovery and token requests. Generous relative to a token
// endpoint's normal latency, tight enough that a hung vendor cannot hold an operator's browser
// (or a task's tool call) indefinitely.
const mcpOAuthHTTPTimeout = 30 * time.Second

// mcpConnector returns the shared Connector, or nil when the deployment has no token store (no
// database wired at all).
//
// The connector is built even when public_base_url is unset: the precondition then fails at
// Begin with an actionable error, which is better than a missing route that fails AFTER the
// operator has consented at the vendor (§7.1).
func (c *Container) mcpConnector() *mcpconnect.Connector {
	c.mcpOAuthOnce.Do(func() {
		if c.repos == nil || c.repos.MCPOAuthTokens == nil {
			return
		}
		var audit mcpconnect.AuditSink
		if c.repos.AdminAudit != nil {
			audit = c.repos.AdminAudit
		}
		c.mcpOAuth = &mcpconnect.Connector{
			Tokens: c.repos.MCPOAuthTokens,
			// The secret store is the process environment, which is where the
			// daemon's secrets dir lands at boot and where projectdoctor's
			// EnvSecrets.Set writes a freshly-supplied value — so no second
			// store is introduced here.
			Secrets: mcpauth.EnvSecretSource{},
			Audit:   audit,
			HTTP:    &http.Client{Timeout: mcpOAuthHTTPTimeout},
			// Read LIVE rather than captured: the connector is built once at
			// boot, and setting public_base_url then reloading config must be
			// enough — an operator should not have to restart the daemon to
			// make Connect work. server.public_base_url is the same field the
			// git clone URL uses; OAuth 2.1 needs an origin the vendor can
			// redirect to.
			BaseURL:  func() string { return c.Config.Server.PublicBaseURL },
			Resolver: c.mcpServerRef,
			Logger:   c.Logger.With().Str("component", "mcp-oauth").Logger(),
		}
		// §7.2a boot sweep: a redirect URI that changed while the daemon was down
		// leaves stored DCR clients registered at the vendor under a callback this
		// deployment no longer serves. Drop them once, here, so the operator sees
		// "needs reconnect" on the tab instead of a vendor rejection mid-consent.
		//
		// Best-effort by construction: RedirectURI() errors when public_base_url is
		// unset or not https, and in that state there is nothing to compare against —
		// flagging every grant on a misconfiguration would be worse than waiting for
		// the next connect, which refuses with an actionable error anyway.
		ctx, cancel := context.WithTimeout(context.Background(), mcpOAuthHTTPTimeout)
		defer cancel()
		switch n, err := c.mcpOAuth.InvalidateStaleClients(ctx); {
		case err != nil:
			c.Logger.Debug().Err(err).
				Msg("MCP OAuth: skipped the stale-redirect-URI sweep (no usable public_base_url yet)")
		case n > 0:
			c.Logger.Warn().Int("grants", n).
				Msg("MCP OAuth: server.public_base_url changed — dropped stored client registrations; affected grants need reconnect")
		}
	})
	return c.mcpOAuth
}

// mcpServerRef resolves the (project, server) pair an operator named into the ServerRef the
// connector needs: the server's URL, its auth block, and the project's secret grants.
//
// projectID "" means the daemon-scope catalog (config.yaml's mcp.servers). A project entry that
// carries no transport of its own inherits the daemon server's connection details, exactly as
// mcpDesiredServers does at wiring time — otherwise `vornikctl mcp connect` would report "no such
// server" for the name-only subscription shape the project form emits.
func (c *Container) mcpServerRef(projectID, serverName string) (mcpconnect.ServerRef, bool) {
	// LIVE catalog, not c.Config: a server added by a config reload must be
	// resolvable by `vornikctl mcp connect`, which is the whole point of
	// adding it. c.Config is pinned to boot.
	daemonServers := indexMCPServersByName(c.daemonMCPServers())

	if projectID == "" {
		s, ok := daemonServers[serverName]
		if !ok {
			return mcpconnect.ServerRef{}, false
		}
		return mcpconnect.ServerRef{ServerName: s.Name, URL: s.URL, Auth: s.Auth}, true
	}

	var proj *registry.Project
	if c.Registry != nil {
		for _, p := range c.Registry.ListProjects() {
			if p.ID == projectID {
				proj = p
				break
			}
		}
	}
	if proj == nil {
		return mcpconnect.ServerRef{}, false
	}
	for _, s := range proj.MCP.Servers {
		if s.Name != serverName {
			continue
		}
		ref := mcpconnect.ServerRef{
			ProjectID:      proj.ID,
			ServerName:     s.Name,
			URL:            s.URL,
			Auth:           s.Auth,
			GrantedSecrets: proj.Permissions.Secrets,
		}
		if s.Transport == "" {
			if daemon, ok := daemonServers[s.Name]; ok {
				ref.URL = daemon.URL
				// The project's own auth block wins when it has one; the
				// ambiguous both-sides case is refused at wiring time
				// (mcpDesiredServers), not silently resolved here.
				if s.Auth.IsZero() {
					ref.Auth = daemon.Auth
				}
			}
		}
		return ref, true
	}
	return mcpconnect.ServerRef{}, false
}
