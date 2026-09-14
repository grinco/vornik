package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/authz"
	"vornik.io/vornik/internal/persistence"
)

// CE operator shell — account management (2026-09-13 config-assistant
// review R1/R2, plan §2). These routes are COMMUNITY: they live under
// /api/v1/operator/ (never /api/v1/admin/, whose prefix carries the EE
// admin-gate invariant) and gate on requireOperatorCapability — operator
// scope PLUS an explicit operator/admin capability, never merely an
// unrestricted project set or an X-Control-Plane header (R2).

// WithAccountsService wires the CE account-management service and the
// resolver the operator shell reads through. Nil leaves the routes at 503.
func WithAccountsService(acc *authz.Accounts) ServerOption {
	return func(s *Server) { s.accounts = acc }
}

// requireOperatorCapability is the CE operator shell's gate (review R2):
// operator scope (no project-scoped tenant key) AND an explicit
// capability — auth disabled (the trusted local operator), a browser
// session whose principal resolved to admin, or an API key on the
// admin.allowed_keys break-glass list. The allowlist is read from the
// daemon config directly rather than from the EE admin surface so the
// capability exists in Community; an unrestricted static key with no
// allowlist entry is refused with a named 403 rather than passing on the
// strength of its empty project list.
func (s *Server) requireOperatorCapability(w http.ResponseWriter, r *http.Request) bool {
	if !s.requireOperatorScope(w, r) {
		return false
	}
	if !IsAuthEnabledFromContext(r.Context()) {
		return true
	}
	if SessionRoleFromContext(r.Context()) == auth.RoleAdmin {
		return true
	}
	key := APIKeyFromContext(r.Context())
	if key != "" && (s.adminConfig.IsAdminKey(key) || (s.config != nil && s.config.Admin.IsAdminKey(key))) {
		return true
	}
	respondError(w, http.StatusForbidden, "OPERATOR_CAPABILITY_REQUIRED",
		"this route needs an explicit operator capability: an admin session, or an API key listed in admin.allowed_keys (admin.enabled: true)")
	return false
}

// operatorActor names the caller for the audit row: a stable id, never a
// bearer secret. A session caller is "session:<user id>"; a key caller is
// the key's id (or fingerprint for a legacy static key).
func operatorActor(r *http.Request) authz.Actor {
	a := authz.Actor{Source: "api", IP: clientIPFromRequest(r), UserAgent: r.UserAgent()}
	if id := IdentityFromContext(r.Context()); id != nil && id.Extra != nil {
		if uid, _ := id.Extra[auth.ExtraSessionUserID].(string); uid != "" {
			a.Principal = "session:" + uid
			return a
		}
	}
	if p := apiKeyPrincipalFromContext(r.Context()); p != "" {
		a.Principal = p
		return a
	}
	if !IsAuthEnabledFromContext(r.Context()) {
		a.Principal = "auth-disabled"
	}
	return a
}

type accountJSON struct {
	ID             string                `json:"id"`
	DisplayName    string                `json:"displayName"`
	Disabled       bool                  `json:"disabled"`
	Role           string                `json:"role"` // "" = awaiting access
	Projects       []string              `json:"projects"`
	Identities     []accountIdentityJSON `json:"identities"`
	ActiveSessions int                   `json:"activeSessions"`
	CreatedAt      time.Time             `json:"createdAt"`
}

type accountIdentityJSON struct {
	Channel    string `json:"channel"`
	ExternalID string `json:"externalId"`
	Display    string `json:"display,omitempty"`
}

func toAccountJSON(v persistence.UserAdminView) accountJSON {
	out := accountJSON{ID: v.UserID, DisplayName: v.DisplayName, Disabled: v.Disabled, Role: v.Role,
		Projects: v.Projects, ActiveSessions: v.ActiveSessions, CreatedAt: v.CreatedAt}
	if out.Projects == nil {
		out.Projects = []string{}
	}
	out.Identities = make([]accountIdentityJSON, 0, len(v.Identities))
	for _, i := range v.Identities {
		out.Identities = append(out.Identities, accountIdentityJSON{Channel: i.Channel, ExternalID: i.ExternalID, Display: i.Display})
	}
	return out
}

// OperatorAccounts handles GET (list) and POST (create) on
// /api/v1/operator/accounts.
func (s *Server) OperatorAccounts(w http.ResponseWriter, r *http.Request) {
	if !s.requireOperatorCapability(w, r) {
		return
	}
	if s.accounts == nil {
		respondError(w, http.StatusServiceUnavailable, "IDENTITY_UNAVAILABLE", "identity core not wired on this daemon")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	switch r.Method {
	case http.MethodGet:
		views, err := s.accounts.List(ctx)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "INTERNAL", "list failed: "+err.Error())
			return
		}
		out := make([]accountJSON, 0, len(views))
		for _, v := range views {
			out = append(out, toAccountJSON(v))
		}
		respondJSON(w, http.StatusOK, map[string]any{"accounts": out, "count": len(out)})
	case http.MethodPost:
		var req struct {
			DisplayName string   `json:"displayName"`
			Role        string   `json:"role"`
			Projects    []string `json:"projects"`
		}
		limitJSONBody(w, r)
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, http.StatusBadRequest, "INVALID_BODY", "invalid JSON: "+err.Error())
			return
		}
		u, err := s.accounts.Create(ctx, authz.CreateRequest{DisplayName: req.DisplayName, Role: req.Role, Projects: req.Projects}, operatorActor(r))
		if u == nil && err != nil {
			respondAccountError(w, err)
			return
		}
		v, gerr := s.accounts.Get(ctx, u.ID)
		if gerr != nil {
			respondError(w, http.StatusInternalServerError, "INTERNAL", gerr.Error())
			return
		}
		body := map[string]any{"account": toAccountJSON(*v)}
		if err != nil {
			body["warning"] = err.Error() // applied, but the audit write failed
		}
		respondJSON(w, http.StatusCreated, body)
	default:
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET or POST")
	}
}

// OperatorAccountItem routes /api/v1/operator/accounts/{id}[/grant|/revoke|/disable|/enable|/unlink].
func (s *Server) OperatorAccountItem(w http.ResponseWriter, r *http.Request) {
	if !s.requireOperatorCapability(w, r) {
		return
	}
	if s.accounts == nil {
		respondError(w, http.StatusServiceUnavailable, "IDENTITY_UNAVAILABLE", "identity core not wired on this daemon")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/operator/accounts/")
	if rest == "" || rest == r.URL.Path {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "expected /api/v1/operator/accounts/{id}[/action]")
		return
	}
	id, action, _ := strings.Cut(rest, "/")
	if id == "" || strings.Contains(action, "/") {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "unknown account sub-route")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if action == "" {
		if r.Method != http.MethodGet {
			respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use GET")
			return
		}
		v, err := s.accounts.Get(ctx, id)
		if err != nil {
			respondAccountError(w, err)
			return
		}
		respondJSON(w, http.StatusOK, map[string]any{"account": toAccountJSON(*v)})
		return
	}
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use POST")
		return
	}
	var req struct {
		Role       string   `json:"role"`
		Projects   []string `json:"projects"`
		Channel    string   `json:"channel"`
		ExternalID string   `json:"externalId"`
	}
	limitJSONBody(w, r)
	if r.Body != nil {
		// revoke/disable/enable need no body; io.EOF on an empty reader is
		// not a client error.
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			respondError(w, http.StatusBadRequest, "INVALID_BODY", "invalid JSON: "+err.Error())
			return
		}
	}
	actor := operatorActor(r)
	known, err := s.applyOperatorAccountAction(ctx, id, action, req.Role, req.Projects, req.Channel, req.ExternalID, actor)
	if !known {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "unknown account action "+action)
		return
	}
	if err != nil && !strings.Contains(err.Error(), "AUDIT WRITE FAILED") {
		respondAccountError(w, err)
		return
	}
	v, gerr := s.accounts.Get(ctx, id)
	if gerr != nil {
		respondAccountError(w, gerr)
		return
	}
	body := map[string]any{"account": toAccountJSON(*v)}
	if err != nil {
		body["warning"] = err.Error()
	}
	respondJSON(w, http.StatusOK, body)
}

func (s *Server) applyOperatorAccountAction(ctx context.Context, id, action, role string, projects []string, channel, externalID string, actor authz.Actor) (bool, error) {
	switch action {
	case "grant":
		return true, s.accounts.Grant(ctx, id, role, projects, actor)
	case "revoke":
		return true, s.accounts.Revoke(ctx, id, actor)
	case "disable":
		return true, s.accounts.SetDisabled(ctx, id, true, actor)
	case "enable":
		return true, s.accounts.SetDisabled(ctx, id, false, actor)
	case "unlink":
		return true, s.accounts.Unlink(ctx, id, channel, externalID, actor)
	default:
		return false, nil
	}
}

func respondAccountError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, persistence.ErrUserNotFound):
		respondError(w, http.StatusNotFound, "NOT_FOUND", "account not found")
	case errors.Is(err, persistence.ErrLastAdmin):
		respondError(w, http.StatusConflict, "LAST_ADMIN", err.Error())
	case errors.Is(err, persistence.ErrNoProjects), errors.Is(err, authz.ErrInvalidRole), errors.Is(err, authz.ErrIdentityNotOwned):
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
	case errors.Is(err, authz.ErrUnauditable), errors.Is(err, authz.ErrResolverUnavailable):
		respondError(w, http.StatusServiceUnavailable, "IDENTITY_UNAVAILABLE", err.Error())
	default:
		respondError(w, http.StatusBadRequest, "ACCOUNT_ERROR", err.Error())
	}
}
