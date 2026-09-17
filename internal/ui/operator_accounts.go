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
	"sort"
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
	// IssuedCode is a freshly minted link code, rendered inline exactly once
	// and never via a redirect — §5.5's "shown once" is a TRANSPORT rule, and
	// a one-time code in a URL reaches browser history, the Referer header
	// and every access log on the way. IssuedFor names the account it is for,
	// because an operator issuing codes for several people needs to know
	// which one they are about to send.
	IssuedCode string
	IssuedFor  string
	// SlackLinkCommands — see MyAccountData: named only when Slack is wired,
	// because the command is configurable per deployment, and ALL of them
	// because a deployment can answer several.
	SlackLinkCommands []string
	// LinkCodeTTLMinutes — see MyAccountData.
	LinkCodeTTLMinutes int
	// AssignableKeys backs the attribution picker. The operator knows a key
	// by its NAME ("slava/codex"); the id is an opaque akey_<ts>_<hex> they
	// have no way to discover, and §5.4 guarantees they do not have the
	// secret either. A free-text id box asked for the one identifier nobody
	// holds.
	AssignableKeys []assignableKey
}

// assignableKey is one row of the attribution picker: what an operator can
// recognise, plus the id the form actually submits.
type assignableKey struct {
	ID      string
	Name    string
	Project string
	Prefix  string
	Revoked bool
	OwnedBy string // display name of the account holding it, empty if unattributed
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
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	s.render(w, "operator_accounts.html", s.operatorAccountsData(ctx, r))
}

// operatorAccountsData assembles the page model, shared by the GET handler
// and the inline link-code render so the two cannot drift.
func (s *Server) operatorAccountsData(ctx context.Context, r *http.Request) OperatorAccountsData {
	data := OperatorAccountsData{
		adminCommonData:    adminCommonData{Title: "Accounts", CurrentPage: "operator", IsAdmin: true},
		Available:          s.accounts != nil,
		Notice:             r.URL.Query().Get("notice"),
		SlackLinkCommands:  s.slackLinkCommands,
		LinkCodeTTLMinutes: int(authz.LinkCodeTTL / time.Minute),
	}
	if !data.Available {
		return data
	}
	views, err := s.accounts.List(ctx)
	if err != nil {
		s.logger.Warn().Err(err).Msg("operator accounts: list failed")
		// Curated for the same reason as accountFailure: on the inline
		// link-code render this shares the handler's 5s budget with the
		// issuance that already ran, so the likeliest error here is a
		// deadline — and "context deadline exceeded" on an operator's screen
		// is a Go internal, not an answer (review-20260915-c6a0 F5).
		data.Notice = "failed to load accounts — see the daemon log"
		return data
	}
	if s.projectReg != nil {
		for _, p := range s.projectReg.ListProjects() {
			data.Projects = append(data.Projects, p.ID)
		}
	}
	data.AssignableKeys = s.assignableKeys(ctx, views)
	// Name each api_key identity the way the operator knows it, BEFORE the
	// rows are built from the views. Display is the field the row already
	// renders when set; the identity store leaves it empty for keys because
	// the name lives in api_keys, a different table, so the row fell back to
	// the opaque akey_<ts>_<hex> id — the one identifier §5.4 guarantees the
	// operator does not have (report 2026-09-16). Labelling here rather than
	// in the template keeps the lookup out of the view.
	labelKeyIdentities(views, keyLabels(data.AssignableKeys))
	data.Users = buildAdminUserRows(views, data.Projects)
	data.AwaitingCount = countAwaiting(views)
	return data
}

// renderOperatorIssuedLinkCode issues a code for id and renders the page with it,
// WITHOUT a redirect, so the code never reaches a URL. The self-service panel
// does the same thing for the same reason; a refresh re-POSTs and mints a
// spare, which is harmless for a single-use ten-minute code and is strictly
// better than the post-redirect-get that leaked the first one.
func (s *Server) renderOperatorIssuedLinkCode(ctx context.Context, w http.ResponseWriter, r *http.Request, id string, actor authz.Actor) {
	code, err := s.accounts.IssueLinkCode(ctx, id, actor)
	// The page model is rebuilt AFTER issuance and shares this handler's
	// budget, so a slow account list can only degrade the surrounding table —
	// never the code, which is already minted and is the one thing this
	// response exists to carry.
	data := s.operatorAccountsData(ctx, r)
	if err != nil {
		data.Notice = accountFailure("link-code", err)
	} else {
		data.IssuedCode, data.IssuedFor = code, id
	}
	// A page displaying a one-time secret must not be cached, and must not
	// hand its own URL to anything it loads.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
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
	case "assign-key":
		// Operator AUTHORITY, no secret — §5.4's "correct a wrong mapping,
		// or attribute the key of someone who has left". Self-claim cannot
		// do either, which is why this lives only here.
		err = s.accounts.AssignKey(ctx, id, strings.TrimSpace(r.FormValue("key_id")), actor)
		done = "key assigned"
	case "unassign-key":
		err = s.accounts.UnassignKey(ctx, strings.TrimSpace(r.FormValue("key_id")), actor)
		done = "key unassigned"
	case "link-code":
		// Renders inline rather than redirecting: the response carries a
		// one-time code. Returns before the shared redirect below.
		s.renderOperatorIssuedLinkCode(ctx, w, r, id, actor)
		return
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
	case errors.Is(err, persistence.ErrAPIKeyNotFound):
		// The message an operator got for this was "assign-key failed — see
		// the daemon log", because no branch matched and the curated default
		// swallowed it. They had typed a key NAME into a field asking for an
		// opaque id, which was the only identifier they had.
		return "no key with that id — pick one from the list rather than typing it"
	default:
		// Curated, like the branches above. err.Error() here put whatever a
		// layer happened to wrap — "context deadline exceeded", a driver
		// message, a package prefix — straight into a notice on the operator's
		// screen (review-20260915-c6a0 F6). The cause goes to the log, where
		// the person who needs it is looking.
		return action + " failed — see the daemon log"
	}
}

// assignableKeys lists every key an operator could attribute, annotated with
// who holds it now.
//
// Empty when no key repository is wired, which renders the picker's
// "no keys" state rather than an empty dropdown that looks broken. A listing
// failure is logged and treated the same way: the rest of the page is still
// the answer to the question the operator asked.
func (s *Server) assignableKeys(ctx context.Context, views []persistence.UserAdminView) []assignableKey {
	if s.apiKeyRepo == nil {
		return nil
	}
	keys, err := s.apiKeyRepo.ListAttributable(ctx)
	if err != nil {
		s.logger.Warn().Err(err).Msg("operator accounts: key listing failed; the attribution picker will be empty")
		return nil
	}
	// Who holds what, from the account views already loaded — no second query.
	owner := map[string]string{}
	for _, v := range views {
		for _, id := range v.Identities {
			if id.Channel == "api_key" {
				owner[id.ExternalID] = v.DisplayName
			}
		}
	}
	out := make([]assignableKey, 0, len(keys))
	for _, k := range keys {
		out = append(out, assignableKey{
			ID: k.ID, Name: k.Name, Project: k.ProjectID, Prefix: k.KeyPrefix,
			Revoked: k.RevokedAt != nil, OwnedBy: owner[k.ID],
		})
	}
	// By project, then name. The repository returns newest-first, which is
	// right for an audit listing and wrong for a picker: it scatters one
	// project's keys through the list, so finding "the one called
	// slava/codex" means reading every entry. Sorting is what makes a short
	// list scannable; it is not a substitute for the per-task filter, which
	// is what made the list short.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Project != out[j].Project {
			return out[i].Project < out[j].Project
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// keyLabels indexes the already-loaded picker rows by key id, so a linked
// identity can be rendered as the operator knows it.
//
// Built from AssignableKeys rather than a fresh lookup: the page has the list
// in hand, and a second query to label a row the first query already
// described is how two sources of one fact start disagreeing. The label
// itself comes from keyIdentityLabel, shared with the personal page.
func keyLabels(keys []assignableKey) map[string]string {
	if len(keys) == 0 {
		return nil
	}
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		out[k.ID] = keyIdentityLabel(k.Name, k.Project, k.Revoked)
	}
	return out
}

// labelKeyIdentities sets Display on every api_key identity to the key's
// human name, leaving other channels untouched.
func labelKeyIdentities(rows []persistence.UserAdminView, labels map[string]string) {
	if len(labels) == 0 {
		return
	}
	for i := range rows {
		for j := range rows[i].Identities {
			id := &rows[i].Identities[j]
			// A display that merely repeats the id is not a display:
			// it is what authz stamped on every row it wrote until
			// 2026-09-16, and skipping those rows is why the first fix
			// changed nothing on a real deployment.
			if id.Channel != "api_key" || !keyDisplayIsOpaque(id.Display, id.ExternalID) {
				continue
			}
			if label, ok := labels[id.ExternalID]; ok {
				id.Display = label
			}
		}
	}
}
