// Identity-core interfaces — users, groups, channel bindings, and
// the resolver query backing internal/authz
// (oidc-identity-permissions-design.md §3).
package persistence

import (
	"context"
	"time"
)

// Identity-core domain errors. Follow the RepositoryError pattern
// from interfaces.go.
const (
	// ErrIdentityNotFound is returned when no active
	// (channel, external_id) binding exists.
	ErrIdentityNotFound = RepositoryError("identity binding not found")
	// ErrUserNotFound is returned when the referenced user row
	// does not exist.
	ErrUserNotFound = RepositoryError("user not found")
	// ErrGroupNotFound is returned when the referenced group row
	// does not exist.
	ErrGroupNotFound = RepositoryError("group not found")
	// ErrSessionNotFound is returned when no active session row
	// matches the presented token hash or id.
	ErrSessionNotFound = RepositoryError("session not found")
	// ErrNoProjects is returned by SetUserAccess when role "user" is
	// granted with an empty project set — a user-role grant with no
	// projects resolves to no access, which is indistinguishable from
	// awaiting; callers must pass at least one project (use ["*"] for
	// all projects) or grant the "admin" role instead.
	ErrNoProjects = RepositoryError("user-role grant requires at least one project")
	// ErrLastAdmin is returned by an admin-removing mutation (disable,
	// demote, revoke-access) that would leave the instance with zero
	// enabled admin users. Enforced inside the mutation's transaction
	// with row-locking so concurrent sessions cannot both pass a
	// handler-level pre-check and lock everyone out (the
	// admin.allowed_keys break-glass path remains the recovery route).
	ErrLastAdmin = RepositoryError("cannot remove the last enabled admin")
)

// UserAdminView is the aggregated per-user row backing the
// /ui/admin/users page (admin-users-approval-ui-design.md §3.1).
// Role is "" when the user belongs to no role-bearing group —
// i.e. awaiting access.
type UserAdminView struct {
	UserID         string
	DisplayName    string
	Disabled       bool
	Role           string            // "admin" | "user" | "" (awaiting)
	Projects       []string          // sorted; ["*"] = all projects; empty for admin / awaiting
	Identities     []UserIdentityRef // active channel bindings
	ActiveSessions int
	CreatedAt      time.Time
}

// UserIdentityRef is one active channel binding shown on the admin
// Users page (a subset of UserIdentity).
type UserIdentityRef struct {
	Channel    string
	ExternalID string
	Display    string
}

// PrincipalRow is one row of the resolver's joined query. A user
// with N group-project pairs yields N rows; a user with zero
// groups yields one row with nil Role/ProjectID (LEFT JOINs).
type PrincipalRow struct {
	UserID      string
	DisplayName string
	Disabled    bool
	Role        *string // nil when the user has no groups
	ProjectID   *string // nil for admin groups / zero-group users
}

// IdentityRepository owns the identity-core tables (users, groups,
// memberships, channel bindings) plus the single-round-trip
// resolver query internal/authz consumes. Link-code methods land
// with Phase 4; the table ships in migration 90.
type IdentityRepository interface {
	CreateUser(ctx context.Context, u *User) error
	// SetUserDisabled flips users.disabled_at. Disabling (disabled=true)
	// ALSO revokes the user's active ui_sessions in the same transaction
	// so a kicked user loses access promptly rather than riding the
	// session backend's positive-principal cache (audit finding A2,
	// https://docs.vornik.io).
	SetUserDisabled(ctx context.Context, userID string, disabled bool) error
	// RevokeSessionsForUser revokes every active ui_sessions row for the
	// user. Zero active sessions is not an error. Used by the disable
	// path (A2) and available for a standalone out-of-band "kick now".
	RevokeSessionsForUser(ctx context.Context, userID string) error

	CreateGroup(ctx context.Context, g *Group) error
	GetGroupByName(ctx context.Context, name string) (*Group, error)
	// SetGroupProjects replaces the group's project set WHOLESALE —
	// it is not additive; absent entries are removed.
	SetGroupProjects(ctx context.Context, groupID string, projects []string) error
	// SetGroupRole updates a group's role ("admin" | "user"). Used to
	// re-sync an auto-managed group (e.g. org-members) to its configured
	// role. No-op-safe: zero rows (missing group) is not an error.
	SetGroupRole(ctx context.Context, groupID, role string) error
	AddGroupMember(ctx context.Context, groupID, userID string) error
	// RemoveGroupMember deletes a (groupID, userID) membership. No-op
	// (not an error) when the row is absent. Used for login-time
	// org-membership revocation symmetry (remove an ex-org-member from
	// the shared org-members group).
	RemoveGroupMember(ctx context.Context, groupID, userID string) error

	// BindIdentity upserts on (channel, external_id): inserts a new
	// binding or repoints a REVOKED row to id.UserID and clears
	// revoked_at. An active conflict is left attached to its existing
	// user so concurrent provisioning converges on one principal.
	BindIdentity(ctx context.Context, id *UserIdentity) error
	// MigrateIdentityExternalID repoints an ACTIVE binding from a legacy
	// external_id to a new one within the same channel. Used by the lazy
	// login-keyed→immutable-ID migration (review of a799e3f2, 2026-06-07):
	// no offline SQL migration is possible because login→numeric-ID needs
	// the provider. Conflict-safe against UNIQUE(channel, external_id): if
	// an active row already exists under newExternalID it is treated as the
	// canonical binding and the legacy row is revoked instead of repointed
	// (a plain UPDATE would otherwise violate the constraint). A no-op
	// (legacy binding absent / already revoked) is NOT an error.
	MigrateIdentityExternalID(ctx context.Context, channel, oldExternalID, newExternalID string) error
	RevokeIdentity(ctx context.Context, channel, externalID string) error
	// TouchIdentityLastUsed is fired async by the resolver's
	// callers; failures are non-fatal (column goes stale).
	TouchIdentityLastUsed(ctx context.Context, channel, externalID string) error

	// ResolvePrincipalRows returns the joined identity→user→groups
	// →projects rows for an ACTIVE binding. Empty slice = no active
	// binding (callers map to their own not-found semantics).
	ResolvePrincipalRows(ctx context.Context, channel, externalID string) ([]PrincipalRow, error)

	// ResolveUserPrincipalRows returns the same joined rows as
	// ResolvePrincipalRows but keyed by user id (the session path —
	// the binding was resolved at login). Zero rows = the user row
	// no longer exists.
	ResolveUserPrincipalRows(ctx context.Context, userID string) ([]PrincipalRow, error)

	// ListUsers returns every user with aggregated role, project scope,
	// active channel bindings and active-session count, for the admin
	// Users page (admin-users-approval-ui-design.md §3.1). Ordering is
	// the caller's concern. Role "" = awaiting access.
	ListUsers(ctx context.Context) ([]UserAdminView, error)

	// SetUserAccess grants or re-grants a user's access by managing
	// their single backing group (auto-user-<userID>) in one
	// transaction: create the group if absent, update its role if it
	// changed, replace its project set (ignored for the "admin" role),
	// and ensure membership. The user's active sessions are revoked in
	// the same transaction so the new principal is resolved fresh
	// (mirrors SetGroupProjects' access-change invariant). role must be
	// "admin" or "user"; "user" with empty projectIDs returns
	// ErrNoProjects (use ["*"] for all projects).
	SetUserAccess(ctx context.Context, userID, role string, projectIDs []string) error

	// RemoveUserAccess drops the user's backing-group membership,
	// returning them to awaiting access, and revokes their active
	// sessions. Other group memberships (e.g. bootstrap-admins) are
	// untouched. A no-op when the user has no backing group.
	RemoveUserAccess(ctx context.Context, userID string) error
}

// UISessionRepository owns browser login sessions (migration 91).
type UISessionRepository interface {
	CreateSession(ctx context.Context, s *UISession) error
	// GetActiveByTokenHash returns the non-revoked session for the
	// hash, or ErrSessionNotFound. Expiry/idle checks are the
	// session store's job (it owns the clock); revocation
	// filtering is SQL's.
	GetActiveByTokenHash(ctx context.Context, tokenHash string) (*UISession, error)
	// TouchSession updates last_seen_at; best-effort (async caller;
	// zero rows affected is not an error). When ip is non-empty it is
	// also refreshed so the admin session viewer shows where the session
	// is currently active rather than a frozen login-time IP (the "stale
	// IP" the operator reported 2026-06-23). Empty ip leaves the stored
	// value untouched (non-middleware/test paths have no resolved IP).
	TouchSession(ctx context.Context, id, ip string) error
	// RevokeSession soft-deletes an ACTIVE session (logout). Returns
	// ErrSessionNotFound when the session is already revoked or
	// absent — unlike TouchSession, zero rows here IS an error so
	// callers can distinguish a no-op logout.
	RevokeSession(ctx context.Context, id string) error
	// RevokeSessionForUser soft-deletes an active session ONLY when it
	// belongs to userID — the ownership invariant for the admin session
	// viewer enforced in SQL (WHERE id=$1 AND user_id=$2), so a handler
	// bug can't revoke another user's session. ErrSessionNotFound when
	// the session is absent, already revoked, or owned by someone else
	// (indistinguishable by design — don't leak cross-user existence).
	RevokeSessionForUser(ctx context.Context, userID, sessionID string) error
	// ListActiveByUser returns the user's non-revoked sessions, newest
	// last-seen first, for the admin session viewer. "Active" =
	// revoked_at IS NULL — the same predicate the Users-page session
	// count uses, so the list length matches the count.
	ListActiveByUser(ctx context.Context, userID string) ([]*UISession, error)
	// DeleteExpiredSessions hard-deletes sessions expired or
	// revoked before cutoff; returns rows deleted (retention).
	DeleteExpiredSessions(ctx context.Context, cutoff time.Time) (int64, error)
	// CountByStatus returns ui_sessions counts bucketed by lifecycle
	// status, powering the ui_sessions observability gauge. The
	// expired-but-not-revoked bucket is the leak class the operator
	// reported 2026-06-23 — rows counted "active" until retention deletes
	// them — so surfacing it lets accumulation be seen without manual SQL.
	CountByStatus(ctx context.Context) (UISessionStatusCounts, error)
}

// UISessionStatusCounts is the lifecycle breakdown of ui_sessions rows:
// Active = revoked_at IS NULL AND expires_at > now(); ExpiredNotRevoked =
// revoked_at IS NULL AND expires_at <= now(); Revoked = revoked_at IS NOT NULL.
type UISessionStatusCounts struct {
	Active            int64
	ExpiredNotRevoked int64
	Revoked           int64
}
