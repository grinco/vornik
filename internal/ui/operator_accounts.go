package ui

// /ui/operator/accounts — the COMMUNITY operator shell's account page
// (2026-09-13 config-assistant review R1/R2). It is the Users page
// (admin_users.go) ported OUT of the Enterprise admin suite: same rows,
// same actions, same audit discipline, but mounted on its own route and
// gated by an explicit operator capability rather than by the EE admin
// router. Enterprise keeps /ui/admin/users as well; both read the one
// identity repository.
//
// Gate (R2): operator scope PLUS an explicit capability — auth disabled,
// an admin browser session, the admin middleware's IsAdmin stamp (EE), or
// an API key on admin.allowed_keys. An unrestricted key with no allowlist
// entry is refused.

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"vornik.io/vornik/internal/admin"
	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/authz"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
)

// WithAccountsService wires the CE account-management service behind
// /ui/operator/accounts. Nil → the page renders "not wired" and every
// POST is 503.
func WithAccountsService(acc *authz.Accounts) ServerOption {
	return func(s *Server) { s.accounts = acc }
}

// WithOperatorCapability supplies the admin.allowed_keys break-glass list
// the CE operator shell accepts as an explicit capability. Wired in both
// editions (the config block is Community; only the admin UI suite is
// Enterprise).
func WithOperatorCapability(cfg config.AdminConfig) ServerOption {
	return func(s *Server) { s.operatorCapability = cfg }
}

// operatorCapable reports whether the caller holds an explicit operator
// capability for the CE shell.
func (s *Server) operatorCapable(r *http.Request) bool {
	ctx := r.Context()
	if !api.IsAuthEnabledFromContext(ctx) {
		return true
	}
	if _, scoped := api.RequestScopedProjects(r); scoped {
		return false
	}
	if admin.IsAdminFromContext(ctx) || api.SessionRoleFromContext(ctx) == auth.RoleAdmin {
		return true
	}
	key := api.APIKeyFromContext(ctx)
	return key != "" && s.operatorCapability.IsAdminKey(key)
}

// OperatorAccountsData backs operator_accounts.html.
type OperatorAccountsData struct {
	adminCommonData
	Available     bool
	Users         []adminUserRow
	Projects      []string
	AwaitingCount int
	Notice        string
}

// OperatorAccounts handles GET /ui/operator/accounts.
func (s *Server) OperatorAccounts(w http.ResponseWriter, r *http.Request) {
	if !s.operatorCapable(r) {
		http.Error(w, "operator capability required", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	data := OperatorAccountsData{
		adminCommonData: adminCommonData{Title: "Accounts", CurrentPage: "operator", IsAdmin: true},
		Available:       s.accounts != nil,
		Notice:          r.URL.Query().Get("notice"),
	}
	if !data.Available {
		s.render(w, "operator_accounts.html", data)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	views, err := s.accounts.List(ctx)
	if err != nil {
		s.logger.Warn().Err(err).Msg("operator accounts: list failed")
		data.Notice = "failed to load accounts: " + err.Error()
		s.render(w, "operator_accounts.html", data)
		return
	}
	if s.projectReg != nil {
		for _, p := range s.projectReg.ListProjects() {
			data.Projects = append(data.Projects, p.ID)
		}
	}
	data.Users = buildAdminUserRows(views, data.Projects)
	data.AwaitingCount = countAwaiting(views)
	s.render(w, "operator_accounts.html", data)
}

// operatorAccountsRouter dispatches /ui/operator/accounts/{id}/{action}
// and POST /ui/operator/accounts (create).
func (s *Server) operatorAccountsRouter(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/operator/accounts")
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" {
		if r.Method == http.MethodPost {
			s.operatorAccountCreate(w, r)
			return
		}
		s.OperatorAccounts(w, r)
		return
	}
	id, action, _ := strings.Cut(rest, "/")
	if id == "" || action == "" || strings.Contains(action, "/") {
		http.NotFound(w, r)
		return
	}
	s.operatorAccountAction(w, r, id, action)
}

func accountsRedirect(w http.ResponseWriter, r *http.Request, notice string) {
	http.Redirect(w, r, "/ui/operator/accounts?notice="+url.QueryEscape(notice), http.StatusSeeOther)
}

// operatorPostPreamble enforces method + wiring + capability for a
// mutation and parses the form.
func (s *Server) operatorPostPreamble(w http.ResponseWriter, r *http.Request) (authz.Actor, bool) {
	if !s.operatorCapable(r) {
		http.Error(w, "operator capability required", http.StatusForbidden)
		return authz.Actor{}, false
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return authz.Actor{}, false
	}
	if s.accounts == nil {
		http.Error(w, "identity core not wired", http.StatusServiceUnavailable)
		return authz.Actor{}, false
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form: "+err.Error(), http.StatusBadRequest)
		return authz.Actor{}, false
	}
	return authz.Actor{Principal: adminPrincipal(r), Source: "ui", IP: clientIP(r), UserAgent: r.UserAgent()}, true
}

func (s *Server) operatorAccountCreate(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.operatorPostPreamble(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	req := authz.CreateRequest{DisplayName: r.FormValue("display_name"), Role: r.FormValue("role"), Projects: r.Form["projects"]}
	if req.Role == "user" && len(req.Projects) == 0 {
		accountsRedirect(w, r, "pick at least one project, or grant the admin role")
		return
	}
	u, err := s.accounts.Create(ctx, req, actor)
	if u == nil && err != nil {
		accountsRedirect(w, r, "create failed: "+err.Error())
		return
	}
	accountsRedirect(w, r, accountOutcome("account created", err))
}

func (s *Server) operatorAccountAction(w http.ResponseWriter, r *http.Request, id, action string) {
	actor, ok := s.operatorPostPreamble(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	var err error
	var done string
	switch action {
	case "grant":
		role := r.FormValue("role")
		projects := r.Form["projects"]
		if role == "user" && len(projects) == 0 {
			accountsRedirect(w, r, "pick at least one project, or grant the admin role")
			return
		}
		err = s.accounts.Grant(ctx, id, role, projects, actor)
		done = "access updated"
	case "revoke-access":
		err = s.accounts.Revoke(ctx, id, actor)
		done = "access revoked"
	case "disable":
		err = s.accounts.SetDisabled(ctx, id, true, actor)
		done = "account disabled"
	case "enable":
		err = s.accounts.SetDisabled(ctx, id, false, actor)
		done = "account enabled"
	case "revoke-identity":
		err = s.accounts.Unlink(ctx, id, r.FormValue("channel"), r.FormValue("external_id"), actor)
		done = "identity unlinked"
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil && !strings.Contains(err.Error(), "AUDIT WRITE FAILED") {
		s.logger.Warn().Err(err).Str("user_id", id).Str("action", action).Msg("operator accounts: action failed")
		accountsRedirect(w, r, accountFailure(action, err))
		return
	}
	accountsRedirect(w, r, accountOutcome(done, err))
}

func accountOutcome(done string, auditErr error) string {
	if auditErr != nil {
		return done + " — but the AUDIT WRITE FAILED; see daemon logs"
	}
	return done
}

func accountFailure(action string, err error) string {
	switch {
	case errors.Is(err, persistence.ErrLastAdmin):
		return "refused: that would remove the last enabled admin"
	case errors.Is(err, authz.ErrIdentityNotOwned):
		return "that identity does not belong to this account"
	case errors.Is(err, persistence.ErrUserNotFound):
		return "account not found"
	default:
		return action + " failed: " + err.Error()
	}
}
