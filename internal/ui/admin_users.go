package ui

// /ui/admin/users — login-approval surface (fills the Users half of
// oidc-identity-permissions-design.md §4.5; see
// admin-users-approval-ui-design.md). Lists every user with awaiting-
// access ones first, and lets an admin approve / re-grant access,
// enable/disable, revoke access, and unlink channel identities. Access
// is modelled user-centric: each user has one auto-managed backing
// group (handled in the identity repo).
//
// Same admin gate matrix as the rest of /admin/* (the adminRouter
// wrapper enforces admin scope). Every mutation writes an admin_audit
// row. A last-admin lockout guard rejects any action that would leave
// the instance with zero enabled admins.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// AdminUsersData backs the list page.
type AdminUsersData struct {
	adminCommonData
	Available     bool // identity core (postgres) wired
	Users         []adminUserRow
	Projects      []string // all project IDs (for the grant checkboxes)
	AwaitingCount int
	Notice        string
}

// adminUserRow is the per-user view model the template renders.
type adminUserRow struct {
	persistence.UserAdminView
	Awaiting    bool // Role == ""
	AllProjects bool // Projects == ["*"]
	// ProjectChoices is the full project list paired with whether this
	// user currently has each — precomputed so the template ranges over
	// a slice rather than indexing a map (html/template's map-index in
	// an attribute context mis-typed the value).
	ProjectChoices []adminProjectChoice
}

// adminProjectChoice is one project checkbox row for the grant form.
type adminProjectChoice struct {
	ID      string
	Checked bool
}

// AdminUsers renders GET /ui/admin/users.
func (s *Server) AdminUsers(w http.ResponseWriter, r *http.Request) {
	data := AdminUsersData{
		adminCommonData: adminCommonData{Title: "Users", CurrentPage: "admin", IsAdmin: true},
		Available:       s.identityRepo != nil,
		Notice:          r.URL.Query().Get("notice"),
	}
	if !data.Available {
		s.render(w, "admin_users.html", data)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	views, err := s.identityRepo.ListUsers(ctx)
	if err != nil {
		s.logger.Warn().Err(err).Msg("admin users: list failed")
		data.Notice = "failed to load users: " + err.Error()
		s.render(w, "admin_users.html", data)
		return
	}
	if s.projectReg != nil {
		for _, p := range s.projectReg.ListProjects() {
			data.Projects = append(data.Projects, p.ID)
		}
	}
	data.Users = buildAdminUserRows(views, data.Projects)
	data.AwaitingCount = countAwaiting(views)
	s.render(w, "admin_users.html", data)
}

// buildAdminUserRows sorts awaiting-first (then enabled, then disabled;
// ties by display name) and decorates each view for the template,
// pairing the full project list with the user's current grants.
func buildAdminUserRows(views []persistence.UserAdminView, allProjects []string) []adminUserRow {
	rows := make([]adminUserRow, 0, len(views))
	for _, v := range views {
		granted := make(map[string]bool, len(v.Projects))
		all := false
		for _, p := range v.Projects {
			if p == "*" {
				all = true
			}
			granted[p] = true
		}
		choices := make([]adminProjectChoice, 0, len(allProjects))
		for _, p := range allProjects {
			choices = append(choices, adminProjectChoice{ID: p, Checked: granted[p]})
		}
		rows = append(rows, adminUserRow{
			UserAdminView:  v,
			Awaiting:       v.Role == "",
			AllProjects:    all,
			ProjectChoices: choices,
		})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		ri, rj := rows[i], rows[j]
		ki, kj := sortRank(ri), sortRank(rj)
		if ki != kj {
			return ki < kj
		}
		return ri.DisplayName < rj.DisplayName
	})
	return rows
}

// sortRank: awaiting (0) first, then enabled approved (1), then disabled (2).
func sortRank(r adminUserRow) int {
	switch {
	case r.Awaiting:
		return 0
	case r.Disabled:
		return 2
	default:
		return 1
	}
}

func countAwaiting(views []persistence.UserAdminView) int {
	n := 0
	for _, v := range views {
		if v.Role == "" {
			n++
		}
	}
	return n
}

func countEnabledAdmins(views []persistence.UserAdminView) int {
	n := 0
	for _, v := range views {
		if v.Role == "admin" && !v.Disabled {
			n++
		}
	}
	return n
}

func findUserView(views []persistence.UserAdminView, id string) *persistence.UserAdminView {
	for i := range views {
		if views[i].UserID == id {
			return &views[i]
		}
	}
	return nil
}

// lockoutBlocks reports whether an admin-removing action against target
// would leave zero enabled admins. removesAdmin captures whether the
// specific action strips the target's admin access (disable, revoke,
// demote). Returns false when the target isn't a sole enabled admin.
func lockoutBlocks(views []persistence.UserAdminView, targetID string, removesAdmin bool) bool {
	if !removesAdmin {
		return false
	}
	t := findUserView(views, targetID)
	if t == nil || t.Role != "admin" || t.Disabled {
		return false
	}
	return countEnabledAdmins(views) <= 1
}

// usersRedirect sends the operator back to the list with a notice.
func usersRedirect(w http.ResponseWriter, r *http.Request, notice string) {
	loc := "/ui/admin/users"
	if notice != "" {
		loc += "?notice=" + url.QueryEscape(notice)
	}
	http.Redirect(w, r, loc, http.StatusSeeOther)
}

// auditUserAction writes one admin_audit row for an access change.
// `after` is marshalled to JSON for the jsonb after_state column (a raw
// non-JSON string would make the INSERT fail). Returns false if the row
// could NOT be written — access changes must be auditable, so the
// caller surfaces this rather than silently dropping it. Callers MUST
// have verified s.adminAuditRepo != nil (usersPostPreamble enforces it).
func (s *Server) auditUserAction(ctx context.Context, r *http.Request, action, target string, after any) bool {
	var afterJSON string
	if after != nil {
		if b, err := json.Marshal(after); err == nil {
			afterJSON = string(b)
		}
	}
	err := s.adminAuditRepo.Insert(ctx, &persistence.AdminAuditEntry{
		Timestamp: time.Now().UTC(),
		Principal: adminPrincipal(r),
		Source:    "ui",
		Action:    action,
		Target:    target,
		After:     afterJSON,
		IP:        clientIP(r),
		UserAgent: r.UserAgent(),
	})
	if err != nil {
		// Never silent: an unaudited access change is a compliance gap.
		s.logger.Error().Err(err).Str("action", action).Str("target", target).
			Msg("admin users: AUDIT WRITE FAILED for access change")
		return false
	}
	return true
}

// postPreamble validates method + wiring and loads the current user
// set (needed for both the lockout guard and identity-ownership checks).
// Returns nil views when it has already written a response.
func (s *Server) usersPostPreamble(w http.ResponseWriter, r *http.Request) (context.Context, context.CancelFunc, []persistence.UserAdminView, bool) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return nil, nil, nil, false
	}
	if s.identityRepo == nil {
		http.Error(w, "identity core not wired", http.StatusServiceUnavailable)
		return nil, nil, nil, false
	}
	// Access changes must be auditable: refuse the mutation outright if
	// no audit sink is wired (per operator requirement — never apply an
	// access change we cannot record).
	if s.adminAuditRepo == nil {
		http.Error(w, "admin audit log not wired; refusing unauditable access change", http.StatusServiceUnavailable)
		return nil, nil, nil, false
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form: "+err.Error(), http.StatusBadRequest)
		return nil, nil, nil, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	views, err := s.identityRepo.ListUsers(ctx)
	if err != nil {
		cancel()
		s.logger.Warn().Err(err).Msg("admin users: list failed in POST preamble")
		usersRedirect(w, r, "failed to load users")
		return nil, nil, nil, false
	}
	return ctx, cancel, views, true
}

// AdminUserGrant handles POST /ui/admin/users/{id}/grant — approve or
// re-grant access. Form: role (admin|user), projects[] (user role).
func (s *Server) AdminUserGrant(w http.ResponseWriter, r *http.Request, id string) {
	ctx, cancel, views, ok := s.usersPostPreamble(w, r)
	if !ok {
		return
	}
	defer cancel()
	if findUserView(views, id) == nil {
		http.NotFound(w, r)
		return
	}
	role := r.FormValue("role")
	if role != "admin" && role != "user" {
		usersRedirect(w, r, "role must be admin or user")
		return
	}
	var projects []string
	if role == "user" {
		projects = r.Form["projects"]
		if len(projects) == 0 {
			usersRedirect(w, r, "pick at least one project, or grant the admin role")
			return
		}
		// "*" (all projects) supersedes any specific selections.
		for _, p := range projects {
			if p == "*" {
				projects = []string{"*"}
				break
			}
		}
	}
	// Lockout guard: demoting the last enabled admin to user is refused.
	if lockoutBlocks(views, id, role != "admin") {
		usersRedirect(w, r, "cannot demote the last admin")
		return
	}
	if err := s.identityRepo.SetUserAccess(ctx, id, role, projects); err != nil {
		s.logger.Warn().Err(err).Str("user_id", id).Msg("admin users: grant failed")
		usersRedirect(w, r, "grant failed: "+err.Error())
		return
	}
	after := map[string]any{"role": role}
	if role == "user" {
		after["projects"] = projects
	}
	usersRedirect(w, r, accessNotice("access updated", s.auditUserAction(ctx, r, "user.grant", id, after)))
}

// accessNotice returns the success message, or an explicit failure
// banner when the audit write did not land — an access change must
// never be silently unrecorded.
func accessNotice(ok string, audited bool) string {
	if audited {
		return ok
	}
	return ok + " — but the AUDIT WRITE FAILED; see daemon logs"
}

// AdminUserDisable handles POST /ui/admin/users/{id}/disable.
func (s *Server) AdminUserDisable(w http.ResponseWriter, r *http.Request, id string) {
	ctx, cancel, views, ok := s.usersPostPreamble(w, r)
	if !ok {
		return
	}
	defer cancel()
	if findUserView(views, id) == nil {
		http.NotFound(w, r)
		return
	}
	if lockoutBlocks(views, id, true) {
		usersRedirect(w, r, "cannot disable the last admin")
		return
	}
	if err := s.identityRepo.SetUserDisabled(ctx, id, true); err != nil {
		s.logger.Warn().Err(err).Str("user_id", id).Msg("admin users: disable failed")
		usersRedirect(w, r, "disable failed: "+err.Error())
		return
	}
	audited := s.auditUserAction(ctx, r, "user.disable", id, map[string]any{"disabled": true})
	usersRedirect(w, r, accessNotice("user disabled", audited))
}

// AdminUserEnable handles POST /ui/admin/users/{id}/enable.
func (s *Server) AdminUserEnable(w http.ResponseWriter, r *http.Request, id string) {
	ctx, cancel, views, ok := s.usersPostPreamble(w, r)
	if !ok {
		return
	}
	defer cancel()
	if findUserView(views, id) == nil {
		http.NotFound(w, r)
		return
	}
	if err := s.identityRepo.SetUserDisabled(ctx, id, false); err != nil {
		s.logger.Warn().Err(err).Str("user_id", id).Msg("admin users: enable failed")
		usersRedirect(w, r, "enable failed: "+err.Error())
		return
	}
	audited := s.auditUserAction(ctx, r, "user.enable", id, map[string]any{"disabled": false})
	usersRedirect(w, r, accessNotice("user enabled", audited))
}

// AdminUserRevokeAccess handles POST /ui/admin/users/{id}/revoke-access
// — returns the user to awaiting access.
func (s *Server) AdminUserRevokeAccess(w http.ResponseWriter, r *http.Request, id string) {
	ctx, cancel, views, ok := s.usersPostPreamble(w, r)
	if !ok {
		return
	}
	defer cancel()
	if findUserView(views, id) == nil {
		http.NotFound(w, r)
		return
	}
	if lockoutBlocks(views, id, true) {
		usersRedirect(w, r, "cannot revoke the last admin")
		return
	}
	if err := s.identityRepo.RemoveUserAccess(ctx, id); err != nil {
		s.logger.Warn().Err(err).Str("user_id", id).Msg("admin users: revoke-access failed")
		usersRedirect(w, r, "revoke failed: "+err.Error())
		return
	}
	audited := s.auditUserAction(ctx, r, "user.revoke_access", id, map[string]any{"role": ""})
	usersRedirect(w, r, accessNotice("access revoked", audited))
}

// AdminUserRevokeIdentity handles POST /ui/admin/users/{id}/revoke-identity
// — unlinks one channel binding. Form: channel, external_id. The binding
// MUST belong to the target user (a crafted form must not unlink someone
// else's identity).
func (s *Server) AdminUserRevokeIdentity(w http.ResponseWriter, r *http.Request, id string) {
	ctx, cancel, views, ok := s.usersPostPreamble(w, r)
	if !ok {
		return
	}
	defer cancel()
	target := findUserView(views, id)
	if target == nil {
		http.NotFound(w, r)
		return
	}
	channel := r.FormValue("channel")
	externalID := r.FormValue("external_id")
	owned := false
	for _, idn := range target.Identities {
		if idn.Channel == channel && idn.ExternalID == externalID {
			owned = true
			break
		}
	}
	if !owned {
		usersRedirect(w, r, "that identity does not belong to this user")
		return
	}
	// Conservatively refuse to unlink the last enabled admin (could lock
	// out admin login if it was their only identity).
	if lockoutBlocks(views, id, true) {
		usersRedirect(w, r, "cannot unlink the last admin's identity")
		return
	}
	if err := s.identityRepo.RevokeIdentity(ctx, channel, externalID); err != nil {
		s.logger.Warn().Err(err).Str("user_id", id).Msg("admin users: revoke-identity failed")
		usersRedirect(w, r, "unlink failed: "+err.Error())
		return
	}
	audited := s.auditUserAction(ctx, r, "user.revoke_identity", id,
		map[string]any{"channel": channel, "external_id": externalID})
	usersRedirect(w, r, accessNotice("identity unlinked", audited))
}
