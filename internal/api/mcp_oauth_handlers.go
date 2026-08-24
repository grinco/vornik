package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/mcpauth"
	"vornik.io/vornik/internal/mcpconnect"
	"vornik.io/vornik/internal/persistence"
)

// MCPOAuthConnector is the connector surface these handlers use. An interface rather than the
// concrete *mcpconnect.Connector so the handler tests need no HTTP vendor and no database.
type MCPOAuthConnector interface {
	// ResolveServer maps a (project, server) pair to its config, resolving the
	// daemon-catalog inheritance a name-only project entry relies on.
	ResolveServer(projectID, serverName string) (mcpconnect.ServerRef, bool)
	Begin(ctx context.Context, ref mcpconnect.ServerRef, connectedBy string) (mcpconnect.BeginResult, error)
	Disconnect(ctx context.Context, projectID, serverName, actor string) error
	Grant(ctx context.Context, projectID, serverName string) (*persistence.MCPOAuthToken, error)
	RedirectURI() (string, error)
}

// MCP OAuth connect endpoints (mcp-server-authentication-design.md steps 4-5). These are what
// `vornikctl mcp connect` drives: the CLI never runs its own OAuth client, it asks the daemon to
// start a flow, prints the URL, and polls until the daemon's own callback has completed the
// exchange (§7.2a).

// requireAdminClassGate authorizes an admin-CLASS caller WITHOUT the edition check that
// requireAdminGate applies.
//
// The distinction matters and is deliberate (design §7.2's CE note, verified 2026-08-03): the
// control-plane hub is admin-gated, NOT edition-gated, and MCP authentication is a Community
// feature by operator decision. Routing these endpoints through requireAdminGate would answer
// 501 EDITION_UNSUPPORTED on Community — making OAuth connect Enterprise-only by accident of a
// shared helper, and repeating the mistake the backlog already records for CE support bundles
// ("the edition most likely to need help is the one that cannot ask for it").
//
// Everything else about the gate is identical: auth disabled (single-operator homelab) passes, a
// browser session with role=admin passes, and otherwise the caller needs an admin-allowlisted
// API key.
func (s *Server) requireAdminClassGate(w http.ResponseWriter, r *http.Request) bool {
	if !IsAuthEnabledFromContext(r.Context()) {
		return true
	}
	if SessionRoleFromContext(r.Context()) == "admin" {
		return true
	}
	key := APIKeyFromContext(r.Context())
	if key == "" {
		respondError(w, http.StatusUnauthorized, "UNAUTHORIZED", "admin authentication required")
		return false
	}
	if !s.adminConfig.IsAdminKey(key) {
		respondError(w, http.StatusForbidden, "ADMIN_SCOPE_REQUIRED", "admin scope required")
		return false
	}
	return true
}

// mcpOAuthBeginRequest is the POST body for starting a flow. ProjectID empty = daemon scope.
type mcpOAuthBeginRequest struct {
	ProjectID string `json:"project_id"`
	Server    string `json:"server"`
}

// mcpOAuthBeginResponse carries what the operator needs in order to consent, and what the CLI
// later compares the recorded grant against.
type mcpOAuthBeginResponse struct {
	AuthorizationURL string   `json:"authorization_url"`
	Resource         string   `json:"resource"`
	Scopes           []string `json:"scopes"`
	// RedirectURI is echoed so an operator debugging a vendor-side
	// "redirect_uri mismatch" can see exactly what was sent.
	RedirectURI string `json:"redirect_uri"`
	// DroppedScopes are advertised scopes this request deliberately did not ask
	// for (daemon-scope servers may not inherit write access — auth design
	// §12.2). Carried on the wire because the CLI shows the operator the ask,
	// and an ask that was narrowed without saying so is how a read-only grant
	// gets mistaken for a vendor that offers no writes.
	DroppedScopes []string `json:"dropped_scopes,omitempty"`
}

// mcpOAuthStatusResponse is the poll shape. It carries NO token — the CLI is a verifier of the
// recorded grant, never a holder of it (§7.2a, review round-2 N1).
type mcpOAuthStatusResponse struct {
	Connected      bool     `json:"connected"`
	Resource       string   `json:"resource,omitempty"`
	Scopes         []string `json:"scopes,omitempty"`
	ConnectedBy    string   `json:"connected_by,omitempty"`
	ConnectedAt    string   `json:"connected_at,omitempty"`
	ExpiresAt      string   `json:"expires_at,omitempty"`
	NeedsReconnect bool     `json:"needs_reconnect"`
	// InheritedFrom is set when the caller asked about a PROJECT but the grant
	// was read at daemon scope, because that project subscribes to a
	// daemon-scope server by name only and therefore inherits its credential
	// (design §9). Without this the answer had to be one of two half-truths:
	// "not connected" (the row is not under that project) or a bare "connected"
	// that hides which grant is doing the work and where to revoke it.
	InheritedFrom string `json:"inherited_from,omitempty"`
}

// mcpGrantScope returns the project_id a grant should be READ or DELETED at for
// a (project, server) pair, plus the asking project when the two differ.
//
// It defers to the resolver, which owns the inheritance rule — a name-only
// subscriber to a daemon-scope server inherits that server's credential, so the
// grant lives at "" no matter which project asks. Handlers that skip this and
// trust the raw project_id disagree with the wiring: status reports "not
// connected" for a project whose tools work, and disconnect deletes a row that
// was never the one in use, leaving the real grant in place.
func mcpGrantScope(conn MCPOAuthConnector, projectID, serverName string) (scope, inheritedFrom string) {
	ref, ok := conn.ResolveServer(projectID, serverName)
	if !ok {
		// Unknown name in that scope. Fall back to what was asked rather than
		// inventing a scope; the lookup below then reports nothing, which is
		// the truth for a server that is not configured there.
		return projectID, ""
	}
	return ref.ProjectID, ref.InheritedFrom
}

// MCPOAuthBegin starts an authorization attempt: POST /api/v1/mcp/oauth/begin.
func (s *Server) MCPOAuthBegin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "POST required")
		return
	}
	if !s.requireAdminClassGate(w, r) {
		return
	}
	if s.mcpOAuth == nil {
		respondError(w, http.StatusServiceUnavailable, "MCP_OAUTH_UNAVAILABLE",
			"MCP OAuth is not wired on this daemon (no token store)")
		return
	}

	var req mcpOAuthBeginRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "INVALID_BODY", "body must be JSON")
		return
	}
	if strings.TrimSpace(req.Server) == "" {
		respondError(w, http.StatusBadRequest, "INVALID_BODY", "server is required")
		return
	}

	ref, ok := s.mcpOAuth.ResolveServer(req.ProjectID, req.Server)
	if !ok {
		respondError(w, http.StatusNotFound, "MCP_SERVER_NOT_FOUND",
			"no MCP server by that name in the requested scope")
		return
	}

	actor := mcpOAuthActor(r)
	begun, err := s.mcpOAuth.Begin(r.Context(), ref, actor)
	if err != nil {
		s.respondMCPOAuthError(w, err)
		return
	}
	redirectURI, _ := s.mcpOAuth.RedirectURI()
	respondJSON(w, http.StatusOK, mcpOAuthBeginResponse{
		AuthorizationURL: begun.AuthorizationURL,
		Resource:         begun.Resource,
		Scopes:           begun.Scopes,
		RedirectURI:      redirectURI,
		DroppedScopes:    begun.DroppedScopes,
	})
}

// MCPOAuthStatus reports whether a (project, server) pair is connected:
// GET /api/v1/mcp/oauth/status?project_id=&server=
func (s *Server) MCPOAuthStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET required")
		return
	}
	if !s.requireAdminClassGate(w, r) {
		return
	}
	if s.mcpOAuth == nil {
		respondError(w, http.StatusServiceUnavailable, "MCP_OAUTH_UNAVAILABLE",
			"MCP OAuth is not wired on this daemon (no token store)")
		return
	}
	server := strings.TrimSpace(r.URL.Query().Get("server"))
	if server == "" {
		respondError(w, http.StatusBadRequest, "INVALID_REQUEST", "server is required")
		return
	}
	projectID := r.URL.Query().Get("project_id")
	scope, inheritedFrom := mcpGrantScope(s.mcpOAuth, projectID, server)

	tok, err := s.mcpOAuth.Grant(r.Context(), scope, server)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL", "could not read the grant")
		return
	}
	if tok == nil {
		respondJSON(w, http.StatusOK, mcpOAuthStatusResponse{Connected: false})
		return
	}
	resp := mcpOAuthStatusResponse{
		Connected:      true,
		InheritedFrom:  inheritedFrom,
		Resource:       tok.Resource,
		ConnectedBy:    tok.ConnectedBy,
		ConnectedAt:    tok.ConnectedAt.UTC().Format(time.RFC3339Nano),
		NeedsReconnect: tok.NeedsReconnect,
	}
	if tok.Scopes != "" {
		resp.Scopes = strings.Fields(tok.Scopes)
	}
	if tok.ExpiresAt != nil {
		resp.ExpiresAt = tok.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	respondJSON(w, http.StatusOK, resp)
}

// MCPOAuthDisconnect deletes a grant: POST /api/v1/mcp/oauth/disconnect.
func (s *Server) MCPOAuthDisconnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "POST required")
		return
	}
	if !s.requireAdminClassGate(w, r) {
		return
	}
	if s.mcpOAuth == nil {
		respondError(w, http.StatusServiceUnavailable, "MCP_OAUTH_UNAVAILABLE",
			"MCP OAuth is not wired on this daemon (no token store)")
		return
	}
	var req mcpOAuthBeginRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "INVALID_BODY", "body must be JSON")
		return
	}
	if strings.TrimSpace(req.Server) == "" {
		respondError(w, http.StatusBadRequest, "INVALID_BODY", "server is required")
		return
	}
	// Delete the grant the wiring READS, not the one the raw project_id names —
	// otherwise disconnecting an inherited server reports success while leaving
	// the credential in use.
	scope, _ := mcpGrantScope(s.mcpOAuth, req.ProjectID, req.Server)
	if err := s.mcpOAuth.Disconnect(r.Context(), scope, req.Server, mcpOAuthActor(r)); err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL", "could not disconnect")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"disconnected": true})
}

// respondMCPOAuthError maps a connector error to a status and a code the CLI can branch on.
// Every message is ours: a vendor's error body never reaches the caller, because on a token
// request it can echo the client secret.
func (s *Server) respondMCPOAuthError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, mcpconnect.ErrNoPublicBaseURL):
		respondError(w, http.StatusPreconditionFailed, "PUBLIC_BASE_URL_REQUIRED",
			"server.public_base_url must be set to an https:// origin before an MCP server can be connected — OAuth 2.1 requires a redirect URI the vendor can reach")
	case errors.Is(err, mcpconnect.ErrNotOAuth):
		respondError(w, http.StatusBadRequest, "NOT_OAUTH_MODE",
			"this server's auth block is not mode: oauth, so there is nothing to consent to")
	case errors.Is(err, mcpauth.ErrNoDCR):
		respondError(w, http.StatusBadRequest, "CLIENT_REGISTRATION_REQUIRED",
			"this authorization server offers no dynamic client registration — set auth.client_id (and auth.client_secret_from if the vendor issued a secret)")
	case errors.Is(err, mcpauth.ErrNoDiscovery):
		respondError(w, http.StatusBadRequest, "DISCOVERY_UNSUPPORTED",
			"this server publishes no OAuth metadata — set auth.authorization_endpoint and auth.token_endpoint manually")
	case errors.Is(err, mcpauth.ErrServerRefused):
		respondError(w, http.StatusBadGateway, "SERVER_REFUSED",
			"the MCP server refused the connection with no authorization challenge — this is a vendor-side refusal (often a WAF), not an auth failure")
	case errors.Is(err, mcpauth.ErrNotProtected):
		respondError(w, http.StatusBadRequest, "SERVER_NOT_PROTECTED",
			"this server answered an unauthenticated request normally, so it needs no auth block")
	case errors.Is(err, mcpauth.ErrSecretUnresolved):
		respondError(w, http.StatusPreconditionFailed, "SECRET_UNRESOLVED",
			"the configured client secret could not be resolved from the secret store; the daemon log names it")
	default:
		s.logger.Error().Err(err).Msg("mcp oauth: begin failed")
		respondError(w, http.StatusBadGateway, "MCP_OAUTH_FAILED",
			"could not start the authorization flow; the daemon log carries the reason")
	}
}

// mcpOAuthActor identifies who is consenting, for the grant record. Best-effort by design: the
// value is evidence, not authorization — the gate above already decided that.
func mcpOAuthActor(r *http.Request) string {
	if id := SessionIDFromContext(r.Context()); id != "" {
		// The browser session id, not a user handle: it is the strongest
		// stable identifier this layer has, and the identity behind it is
		// resolvable from ui_sessions.
		return "session:" + id
	}
	if key := APIKeyFromContext(r.Context()); key != "" {
		// Never the key itself: this string lands in an audit row.
		return "api-key:" + redactedKeyLabel(key)
	}
	return "operator"
}

// redactedKeyLabel renders a stable, non-reversible label for an API key.
func redactedKeyLabel(key string) string {
	if len(key) <= 6 {
		return "…"
	}
	return "…" + key[len(key)-4:]
}
