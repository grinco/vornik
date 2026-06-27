package ui

// /ui/admin/users/{id}/sessions — the admin session viewer
// (admin-session-viewer-design.md). Drill into one user's active
// browser sessions and terminate individual ones or all of them. The
// caller's own current session is marked and protected from
// termination. Rides the same admin gate + mandatory-audit-sink rules
// as the rest of the Users surface (usersPostPreamble).

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/persistence"
)

// AdminUserSessionsData backs the session viewer page.
type AdminUserSessionsData struct {
	adminCommonData
	Available   bool // identity + session repos wired
	UserID      string
	DisplayName string
	Sessions    []adminSessionRow
	Notice      string
}

// adminSessionRow is the per-session view model.
type adminSessionRow struct {
	ID        string
	Provider  string
	Created   string
	LastSeen  string
	Expires   string
	IP        string
	Agent     string // friendly "Chrome on macOS"
	RawAgent  string // full UA for the row title
	IsCurrent bool   // the caller's own session — not terminable
}

const sessionTimeFmt = "2006-01-02 15:04"

// dispatchAdminUserRoute routes /admin/users/{id}/... sub-paths (rest
// is the part after "/users/"). Returns true when it handled the
// request. Split from adminRouter to keep each step flat.
func (s *Server) dispatchAdminUserRoute(w http.ResponseWriter, r *http.Request, rest string) bool {
	if s.dispatchUserSessionRoute(w, r, rest) {
		return true
	}
	return s.dispatchUserActionRoute(w, r, rest)
}

// dispatchUserSessionRoute handles /{id}/sessions[...] (list,
// terminate-all, {sid}/terminate). Guard clauses keep it flat.
func (s *Server) dispatchUserSessionRoute(w http.ResponseWriter, r *http.Request, rest string) bool {
	i := strings.Index(rest, "/sessions")
	if i <= 0 {
		return false
	}
	uid := rest[:i]
	if uid == "" || strings.Contains(uid, "/") {
		return false
	}
	switch tail := rest[i+len("/sessions"):]; {
	case tail == "":
		s.AdminUserSessions(w, r, uid)
		return true
	case tail == "/terminate-all":
		s.AdminUserSessionsTerminateAll(w, r, uid)
		return true
	case strings.HasPrefix(tail, "/") && strings.HasSuffix(tail, "/terminate"):
		sid := strings.TrimSuffix(strings.TrimPrefix(tail, "/"), "/terminate")
		if sid != "" && !strings.Contains(sid, "/") {
			s.AdminUserSessionTerminate(w, r, uid, sid)
			return true
		}
	}
	return false
}

// dispatchUserActionRoute handles the single-action POSTs
// /{id}/<action> (grant/disable/enable/revoke-access/revoke-identity).
func (s *Server) dispatchUserActionRoute(w http.ResponseWriter, r *http.Request, rest string) bool {
	for action, handler := range map[string]func(http.ResponseWriter, *http.Request, string){
		"grant":           s.AdminUserGrant,
		"disable":         s.AdminUserDisable,
		"enable":          s.AdminUserEnable,
		"revoke-access":   s.AdminUserRevokeAccess,
		"revoke-identity": s.AdminUserRevokeIdentity,
	} {
		if !strings.HasSuffix(rest, "/"+action) {
			continue
		}
		uid := strings.TrimSuffix(rest, "/"+action)
		if uid != "" && !strings.Contains(uid, "/") {
			handler(w, r, uid)
			return true
		}
	}
	return false
}

// AdminUserSessions renders GET /ui/admin/users/{id}/sessions.
func (s *Server) AdminUserSessions(w http.ResponseWriter, r *http.Request, id string) {
	data := AdminUserSessionsData{
		adminCommonData: adminCommonData{Title: "User sessions", CurrentPage: "admin", IsAdmin: true},
		Available:       s.identityRepo != nil && s.uiSessionRepo != nil,
		UserID:          id,
		Notice:          r.URL.Query().Get("notice"),
	}
	if !data.Available {
		s.render(w, "admin_user_sessions.html", data)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	views, err := s.identityRepo.ListUsers(ctx)
	if err != nil {
		s.logger.Warn().Err(err).Msg("admin user sessions: list users failed")
		data.Notice = "failed to load user"
		s.render(w, "admin_user_sessions.html", data)
		return
	}
	u := findUserView(views, id)
	if u == nil {
		http.NotFound(w, r)
		return
	}
	data.DisplayName = u.DisplayName

	sessions, err := s.uiSessionRepo.ListActiveByUser(ctx, id)
	if err != nil {
		s.logger.Warn().Err(err).Str("user_id", id).Msg("admin user sessions: list sessions failed")
		data.Notice = "failed to load sessions"
		s.render(w, "admin_user_sessions.html", data)
		return
	}
	current := api.SessionIDFromContext(r.Context())
	for _, ses := range sessions {
		data.Sessions = append(data.Sessions, adminSessionRow{
			ID:        ses.ID,
			Provider:  ses.Provider,
			Created:   ses.CreatedAt.Local().Format(sessionTimeFmt),
			LastSeen:  ses.LastSeenAt.Local().Format(sessionTimeFmt),
			Expires:   ses.ExpiresAt.Local().Format(sessionTimeFmt),
			IP:        ses.IP,
			Agent:     friendlyUserAgent(ses.UserAgent),
			RawAgent:  ses.UserAgent,
			IsCurrent: current != "" && ses.ID == current,
		})
	}
	s.render(w, "admin_user_sessions.html", data)
}

// sessionsRedirect returns the operator to the user's sessions page.
func sessionsRedirect(w http.ResponseWriter, r *http.Request, userID, notice string) {
	loc := "/ui/admin/users/" + url.PathEscape(userID) + "/sessions"
	if notice != "" {
		loc += "?notice=" + url.QueryEscape(notice)
	}
	http.Redirect(w, r, loc, http.StatusSeeOther)
}

// AdminUserSessionTerminate handles POST
// /ui/admin/users/{id}/sessions/{sid}/terminate.
func (s *Server) AdminUserSessionTerminate(w http.ResponseWriter, r *http.Request, id, sid string) {
	ctx, cancel, views, ok := s.usersPostPreamble(w, r)
	if !ok {
		return
	}
	defer cancel()
	if s.uiSessionRepo == nil {
		http.Error(w, "session store not wired", http.StatusServiceUnavailable)
		return
	}
	if findUserView(views, id) == nil {
		http.NotFound(w, r)
		return
	}
	// Ownership: the session must belong to this user (fail closed —
	// never revoke an arbitrary session id from a crafted form).
	sessions, err := s.uiSessionRepo.ListActiveByUser(ctx, id)
	if err != nil {
		sessionsRedirect(w, r, id, "failed to load sessions")
		return
	}
	if !sessionOwned(sessions, sid) {
		http.NotFound(w, r)
		return
	}
	if current := api.SessionIDFromContext(r.Context()); current != "" && sid == current {
		sessionsRedirect(w, r, id, "can't terminate your own current session")
		return
	}
	// RevokeSessionForUser re-enforces ownership in SQL (defense in depth
	// vs the pre-check above). ErrSessionNotFound here means it was
	// revoked between the list and now (TOCTOU) — benign, the session is
	// gone either way, so report success.
	if err := s.uiSessionRepo.RevokeSessionForUser(ctx, id, sid); err != nil && err != persistence.ErrSessionNotFound {
		s.logger.Warn().Err(err).Str("session_id", sid).Msg("admin user sessions: revoke failed")
		sessionsRedirect(w, r, id, "terminate failed: "+err.Error())
		return
	}
	audited := s.auditUserAction(ctx, r, "user.session_revoke", id, map[string]any{"session_id": sid})
	sessionsRedirect(w, r, id, accessNotice("session terminated", audited))
}

// AdminUserSessionsTerminateAll handles POST
// /ui/admin/users/{id}/sessions/terminate-all. Revokes every active
// session for the user EXCEPT the caller's own current session, so the
// admin is never logged out by this action.
func (s *Server) AdminUserSessionsTerminateAll(w http.ResponseWriter, r *http.Request, id string) {
	ctx, cancel, views, ok := s.usersPostPreamble(w, r)
	if !ok {
		return
	}
	defer cancel()
	if s.uiSessionRepo == nil {
		http.Error(w, "session store not wired", http.StatusServiceUnavailable)
		return
	}
	if findUserView(views, id) == nil {
		http.NotFound(w, r)
		return
	}
	sessions, err := s.uiSessionRepo.ListActiveByUser(ctx, id)
	if err != nil {
		sessionsRedirect(w, r, id, "failed to load sessions")
		return
	}
	current := api.SessionIDFromContext(r.Context())
	revokedIDs := make([]string, 0, len(sessions))
	for _, ses := range sessions {
		if current != "" && ses.ID == current {
			continue // never revoke the caller's own session
		}
		if err := s.uiSessionRepo.RevokeSessionForUser(ctx, id, ses.ID); err != nil && err != persistence.ErrSessionNotFound {
			s.logger.Warn().Err(err).Str("session_id", ses.ID).Msg("admin user sessions: revoke-all member failed")
			continue
		}
		revokedIDs = append(revokedIDs, ses.ID)
	}
	// Audit the actual ids (forensics), not just the count.
	audited := s.auditUserAction(ctx, r, "user.session_revoke_all", id,
		map[string]any{"revoked": len(revokedIDs), "revoked_ids": revokedIDs})
	sessionsRedirect(w, r, id, accessNotice("terminated "+itoa(len(revokedIDs))+" session(s)", audited))
}

// sessionOwned reports whether sid is in the user's active session set.
func sessionOwned(sessions []*persistence.UISession, sid string) bool {
	for _, ses := range sessions {
		if ses.ID == sid {
			return true
		}
	}
	return false
}

// friendlyUserAgent renders a short "Browser on OS" label from a raw
// user-agent string, the main "do I recognise this session?" signal.
// Heuristic + order-sensitive (Edge and Chrome UAs both contain
// "Chrome/"; Chrome and Safari UAs both contain "Safari/"). Falls back
// to a truncated raw UA when it can't classify.
func friendlyUserAgent(ua string) string {
	if strings.TrimSpace(ua) == "" {
		return "unknown"
	}
	var browser string
	switch {
	case strings.Contains(ua, "Edg/"):
		browser = "Edge"
	case strings.Contains(ua, "Chrome/"):
		browser = "Chrome"
	case strings.Contains(ua, "Firefox/"):
		browser = "Firefox"
	case strings.Contains(ua, "Version/") && strings.Contains(ua, "Safari/"):
		browser = "Safari"
	}
	var os string
	switch {
	case strings.Contains(ua, "Windows"):
		os = "Windows"
	case strings.Contains(ua, "Macintosh"), strings.Contains(ua, "Mac OS X"):
		os = "macOS"
	case strings.Contains(ua, "Android"):
		os = "Android"
	case strings.Contains(ua, "iPhone"), strings.Contains(ua, "iPad"):
		os = "iOS"
	case strings.Contains(ua, "Linux"):
		os = "Linux"
	}
	switch {
	case browser != "" && os != "":
		return browser + " on " + os
	case browser != "":
		return browser
	case os != "":
		return os
	default:
		if len(ua) > 40 {
			return ua[:40] + "…"
		}
		return ua
	}
}

// itoa is a tiny non-fmt int formatter for the audit/notice strings.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
