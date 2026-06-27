package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// IdentityRepository implements persistence.IdentityRepository on
// Postgres. Same conventions as APIKeyRepository: DBTX-injected,
// domain errors from interfaces_identity.go, mapDBError for
// transport errors.
//
// Provisioning (authz.Service.Provision) issues several of these
// calls in sequence WITHOUT a transaction. BindIdentity is the
// canonicalization point: active conflicts keep the existing user,
// and Provision re-resolves that winner before minting a session.
// A losing CreateUser may leave an inert orphan row, but never an
// authenticated duplicate principal.
type IdentityRepository struct {
	db DBTX
}

// NewIdentityRepository constructs the repository.
func NewIdentityRepository(db DBTX) *IdentityRepository {
	return &IdentityRepository{db: db}
}

// CreateUser inserts a user row.
func (r *IdentityRepository) CreateUser(ctx context.Context, u *persistence.User) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO users (id, display_name, created_at) VALUES ($1, $2, $3)`,
		u.ID, u.DisplayName, u.CreatedAt)
	return mapDBError(err)
}

// SetUserDisabled flips users.disabled_at (true → NOW(), false → NULL).
//
// A2 (https://docs.vornik.io): disabling a user
// MUST also revoke their active browser sessions. Without this a
// disabled (possibly admin) principal keeps access until the session
// backend's positive-principal cache expires (~60s) — "kick this user
// now" was not honored. The disable + session-revoke run inside ONE
// transaction so a crash cannot leave a disabled user with live
// sessions (or vice-versa). Re-enabling (disabled=false) does NOT
// resurrect sessions — the user logs in again.
func (r *IdentityRepository) SetUserDisabled(ctx context.Context, userID string, disabled bool) error {
	if !disabled {
		// Re-enable: single statement, no session work.
		res, err := r.db.ExecContext(ctx,
			`UPDATE users SET disabled_at = CASE WHEN $2 THEN NOW() ELSE NULL END WHERE id = $1`,
			userID, disabled)
		if err != nil {
			return mapDBError(err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return persistence.ErrUserNotFound
		}
		return nil
	}

	tx, ok, err := persistence.BeginTx(ctx, r.db, nil)
	if err != nil {
		return mapDBError(err)
	}
	if !ok {
		// Caller already owns a transaction; run inline on r.db.
		return r.setUserDisabledTrueWithExec(ctx, r.db)(userID)
	}
	defer func() { _ = tx.Rollback() }()
	if err := r.setUserDisabledTrueWithExec(ctx, tx)(userID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return mapDBError(err)
	}
	return nil
}

// setUserDisabledTrueWithExec disables the user and revokes their active
// sessions on the given executor (the disable=true path of
// SetUserDisabled). Returns ErrUserNotFound when the user row is absent.
func (r *IdentityRepository) setUserDisabledTrueWithExec(ctx context.Context, exec DBTX) func(userID string) error {
	return func(userID string) error {
		// Last-admin guard (review finding #1): refuse to disable the
		// sole enabled admin; the lock serializes concurrent attempts.
		if err := r.guardLastAdmin(ctx, exec, userID); err != nil {
			return err
		}
		res, err := exec.ExecContext(ctx,
			`UPDATE users SET disabled_at = CASE WHEN $2 THEN NOW() ELSE NULL END WHERE id = $1`,
			userID, true)
		if err != nil {
			return mapDBError(err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return persistence.ErrUserNotFound
		}
		return r.revokeSessionsForUserWithExec(ctx, exec, userID)
	}
}

// RevokeSessionsForUser soft-deletes (revokes) every active ui_sessions
// row for userID. Best-effort by count: zero active sessions is NOT an
// error (the user may simply have none). A2 disable path; also usable
// standalone for an out-of-band "kick now" without flipping disabled_at.
func (r *IdentityRepository) RevokeSessionsForUser(ctx context.Context, userID string) error {
	return r.revokeSessionsForUserWithExec(ctx, r.db, userID)
}

func (r *IdentityRepository) revokeSessionsForUserWithExec(ctx context.Context, exec DBTX, userID string) error {
	_, err := exec.ExecContext(ctx,
		`UPDATE ui_sessions SET revoked_at = NOW() WHERE user_id = $1 AND revoked_at IS NULL`,
		userID)
	return mapDBError(err)
}

// revokeSessionsForGroupWithExec revokes every active session belonging
// to a member of groupID. Used when a group's access is narrowed
// (SetGroupProjects) so the change takes effect immediately rather than
// at the ~60s principal-cache TTL — the same "kick now" guarantee the
// disable path gives (hardening 2026-06-15, auth LLD review batch 2).
func (r *IdentityRepository) revokeSessionsForGroupWithExec(ctx context.Context, exec DBTX, groupID string) error {
	_, err := exec.ExecContext(ctx,
		`UPDATE ui_sessions SET revoked_at = NOW()
		 WHERE revoked_at IS NULL
		   AND user_id IN (SELECT user_id FROM group_members WHERE group_id = $1)`,
		groupID)
	return mapDBError(err)
}

// CreateGroup inserts a group row.
func (r *IdentityRepository) CreateGroup(ctx context.Context, g *persistence.Group) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO groups (id, name, role, description, created_at) VALUES ($1, $2, $3, $4, $5)`,
		g.ID, g.Name, g.Role, g.Description, g.CreatedAt)
	return mapDBError(err)
}

// GetGroupByName fetches a group by its unique name.
func (r *IdentityRepository) GetGroupByName(ctx context.Context, name string) (*persistence.Group, error) {
	var g persistence.Group
	var desc sql.NullString
	err := r.db.QueryRowContext(ctx,
		`SELECT id, name, role, description, created_at FROM groups WHERE name = $1`,
		name).Scan(&g.ID, &g.Name, &g.Role, &desc, &g.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrGroupNotFound
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	g.Description = desc.String
	return &g, nil
}

// SetGroupProjects replaces the group's project set WHOLESALE.
// DELETE + all INSERTs run inside one transaction so a concurrent
// resolver query never observes the half-replaced (zero-project) state
// that would exist between the committed DELETE and the first INSERT.
//
// The members' active sessions are revoked in the SAME transaction: a
// project-set change is an access change, and serving the stale (broader)
// project set from the principal cache for up to ~60s would let a member
// keep access that was just removed. Revocation forces re-login →
// re-resolution against the new project set. (Hardening 2026-06-15.)
func (r *IdentityRepository) SetGroupProjects(ctx context.Context, groupID string, projects []string) error {
	tx, ok, err := persistence.BeginTx(ctx, r.db, nil)
	if err != nil {
		return mapDBError(err)
	}
	if !ok {
		// DBTX is already an *sql.Tx (caller owns the transaction).
		return r.setGroupProjectsWithExec(ctx, r.db, groupID, projects)
	}
	defer func() { _ = tx.Rollback() }()
	if err := r.setGroupProjectsWithExec(ctx, tx, groupID, projects); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return mapDBError(err)
	}
	return nil
}

func (r *IdentityRepository) setGroupProjectsWithExec(ctx context.Context, exec DBTX, groupID string, projects []string) error {
	if _, err := exec.ExecContext(ctx,
		`DELETE FROM group_projects WHERE group_id = $1`, groupID); err != nil {
		return mapDBError(err)
	}
	for _, p := range projects {
		if _, err := exec.ExecContext(ctx,
			`INSERT INTO group_projects (group_id, project_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			groupID, p); err != nil {
			return mapDBError(err)
		}
	}
	return r.revokeSessionsForGroupWithExec(ctx, exec, groupID)
}

// SetGroupRole updates a group's role. Zero rows (missing group) is not
// an error — re-sync callers don't care to distinguish.
func (r *IdentityRepository) SetGroupRole(ctx context.Context, groupID, role string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE groups SET role = $2 WHERE id = $1`, groupID, role)
	return mapDBError(err)
}

// AddGroupMember inserts a membership; idempotent.
func (r *IdentityRepository) AddGroupMember(ctx context.Context, groupID, userID string) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO group_members (group_id, user_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		groupID, userID)
	return mapDBError(err)
}

// RemoveGroupMember deletes a membership; absent row is not an error.
func (r *IdentityRepository) RemoveGroupMember(ctx context.Context, groupID, userID string) error {
	_, err := r.db.ExecContext(ctx,
		`DELETE FROM group_members WHERE group_id = $1 AND user_id = $2`, groupID, userID)
	return mapDBError(err)
}

// BindIdentity inserts a channel binding or reactivates a revoked one.
// An ACTIVE conflicting binding is never repointed: concurrent
// first-contact provisioners must converge on the winner's user rather
// than minting sessions for orphan duplicate users.
func (r *IdentityRepository) BindIdentity(ctx context.Context, id *persistence.UserIdentity) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO user_identities (id, user_id, channel, external_id, display, created_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (channel, external_id)
DO UPDATE SET user_id = EXCLUDED.user_id, display = EXCLUDED.display, revoked_at = NULL
WHERE user_identities.revoked_at IS NOT NULL`,
		id.ID, id.UserID, id.Channel, id.ExternalID, id.Display, id.CreatedAt)
	return mapDBError(err)
}

// MigrateIdentityExternalID repoints an ACTIVE binding from a legacy
// external_id to newExternalID within the same channel (lazy
// login-keyed→numeric-ID migration; review of a799e3f2, 2026-06-07).
//
// Conflict handling against UNIQUE(channel, external_id), which spans
// revoked rows: the repoint UPDATE is guarded by a NOT EXISTS over any
// row already holding newExternalID, so it fires ONLY when the target
// key is free — a bare UPDATE would otherwise raise a unique violation.
// When the target key IS taken (the user already provisioned/migrated
// under the numeric ID), that row is canonical; the now-superfluous
// legacy row is revoked so it can never shadow the resolver. Either way
// the call is idempotent and a missing/already-revoked legacy binding is
// a harmless no-op (NOT an error) — Provision only calls this after a
// successful Resolve of the legacy key, but the guard keeps it safe under
// concurrent logins racing the same migration.
func (r *IdentityRepository) MigrateIdentityExternalID(ctx context.Context, channel, oldExternalID, newExternalID string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE user_identities SET external_id = $3
WHERE channel = $1 AND external_id = $2 AND revoked_at IS NULL
  AND NOT EXISTS (
    SELECT 1 FROM user_identities existing
    WHERE existing.channel = $1 AND existing.external_id = $3
  )`,
		channel, oldExternalID, newExternalID)
	if err != nil {
		return mapDBError(err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		// Legacy row was repointed onto the (previously free) new key.
		return nil
	}
	// The new key was already taken (canonical row exists) OR the legacy
	// row was absent/revoked. Revoke any still-active legacy row so it
	// cannot shadow the canonical binding; zero rows here is fine.
	_, err = r.db.ExecContext(ctx,
		`UPDATE user_identities SET revoked_at = NOW()
WHERE channel = $1 AND external_id = $2 AND revoked_at IS NULL`,
		channel, oldExternalID)
	return mapDBError(err)
}

// RevokeIdentity soft-deletes an ACTIVE binding (unlink) and revokes the
// owning user's active sessions in the SAME transaction. Unlinking an
// identity is an access removal; leaving the user's live sessions intact
// would let them keep access until the ~60s principal-cache TTL. If the
// user retains another active identity they can simply log in again and
// mint a fresh session; if this was their only identity they are locked
// out immediately, which is the intent. (Hardening 2026-06-15, auth LLD
// review batch 2 — mirrors the SetUserDisabled in-tx revoke.)
func (r *IdentityRepository) RevokeIdentity(ctx context.Context, channel, externalID string) error {
	tx, ok, err := persistence.BeginTx(ctx, r.db, nil)
	if err != nil {
		return mapDBError(err)
	}
	if !ok {
		// Caller already owns a transaction; run inline on r.db.
		return r.revokeIdentityWithExec(ctx, r.db, channel, externalID)
	}
	defer func() { _ = tx.Rollback() }()
	if err := r.revokeIdentityWithExec(ctx, tx, channel, externalID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return mapDBError(err)
	}
	return nil
}

func (r *IdentityRepository) revokeIdentityWithExec(ctx context.Context, exec DBTX, channel, externalID string) error {
	var userID string
	err := exec.QueryRowContext(ctx,
		`UPDATE user_identities SET revoked_at = NOW()
		 WHERE channel = $1 AND external_id = $2 AND revoked_at IS NULL
		 RETURNING user_id`,
		channel, externalID).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return persistence.ErrIdentityNotFound
	}
	if err != nil {
		return mapDBError(err)
	}
	return r.revokeSessionsForUserWithExec(ctx, exec, userID)
}

// TouchIdentityLastUsed updates last_used_at. Best-effort: zero
// rows affected is NOT an error (binding may have been revoked
// between resolve and touch — the stale column is harmless).
func (r *IdentityRepository) TouchIdentityLastUsed(ctx context.Context, channel, externalID string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE user_identities SET last_used_at = NOW() WHERE channel = $1 AND external_id = $2 AND revoked_at IS NULL`,
		channel, externalID)
	return mapDBError(err)
}

// Compile-time interface pin — now that the full method set exists.
var _ persistence.IdentityRepository = (*IdentityRepository)(nil)

// ResolvePrincipalRows is the resolver's single round trip
// (design §3.2): active binding → user → groups → projects, all
// LEFT-joined so a zero-group user still yields one row (nil Role)
// and the caller can distinguish "unknown identity" (zero rows)
// from "authenticated, no access" (rows with nil Role).
func (r *IdentityRepository) ResolvePrincipalRows(ctx context.Context, channel, externalID string) ([]persistence.PrincipalRow, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT u.id, u.display_name, (u.disabled_at IS NOT NULL) AS disabled, g.role, gp.project_id
FROM user_identities ui
JOIN users u ON u.id = ui.user_id
LEFT JOIN group_members gm ON gm.user_id = u.id
LEFT JOIN groups g ON g.id = gm.group_id
LEFT JOIN group_projects gp ON gp.group_id = g.id
WHERE ui.channel = $1 AND ui.external_id = $2 AND ui.revoked_at IS NULL`,
		channel, externalID)
	if err != nil {
		return nil, mapDBError(err)
	}
	return scanPrincipalRows(rows)
}

// ResolveUserPrincipalRows is the session-path resolver: the login
// flow already resolved the (channel, external_id) → user binding,
// so the request-time lookup keys directly on users.id and skips the
// user_identities join. Same LEFT-joined row contract as
// ResolvePrincipalRows (zero-group user → one nil-Role row); zero
// rows = the user row no longer exists.
func (r *IdentityRepository) ResolveUserPrincipalRows(ctx context.Context, userID string) ([]persistence.PrincipalRow, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT u.id, u.display_name, (u.disabled_at IS NOT NULL) AS disabled, g.role, gp.project_id
FROM users u
LEFT JOIN group_members gm ON gm.user_id = u.id
LEFT JOIN groups g ON g.id = gm.group_id
LEFT JOIN group_projects gp ON gp.group_id = g.id
WHERE u.id = $1`,
		userID)
	if err != nil {
		return nil, mapDBError(err)
	}
	return scanPrincipalRows(rows)
}

// adminLockSQL locks the current enabled-admin user rows so concurrent
// admin-removal transactions serialize — without it, two sessions can
// each pass a count check and disable/demote different admins, leaving
// zero (last-admin TOCTOU, companion review 2026-06-22 finding #1).
const adminLockSQL = `SELECT u.id FROM users u
JOIN group_members gm ON gm.user_id = u.id
JOIN groups g ON g.id = gm.group_id
WHERE g.role = 'admin' AND u.disabled_at IS NULL
FOR UPDATE`

// adminGuardSQL reports, in one round trip, whether the target user is
// currently an enabled admin and whether any OTHER enabled admin
// exists. The guard fires only when the target is an admin and is the
// last one — so disabling/removing a non-admin is never blocked, even
// in the (degenerate) zero-admin state.
const adminGuardSQL = `SELECT
  EXISTS(SELECT 1 FROM users u JOIN group_members gm ON gm.user_id = u.id JOIN groups g ON g.id = gm.group_id
         WHERE u.id = $1 AND g.role = 'admin' AND u.disabled_at IS NULL),
  EXISTS(SELECT 1 FROM users u JOIN group_members gm ON gm.user_id = u.id JOIN groups g ON g.id = gm.group_id
         WHERE u.id <> $1 AND g.role = 'admin' AND u.disabled_at IS NULL)`

// lockEnabledAdmins acquires row locks on the enabled-admin set so a
// concurrent admin-removal transaction blocks here until this one
// commits, then re-evaluates against the new state.
func lockEnabledAdmins(ctx context.Context, exec DBTX) error {
	rows, err := exec.QueryContext(ctx, adminLockSQL)
	if err != nil {
		return mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return mapDBError(err)
		}
	}
	return mapDBError(rows.Err())
}

// guardLastAdmin locks the enabled-admin set then refuses (ErrLastAdmin)
// an action whose target is the sole enabled admin. Must be called
// inside the mutation's transaction, before the mutation, so the lock
// serializes concurrent admin-removal attempts (review finding #1).
func (r *IdentityRepository) guardLastAdmin(ctx context.Context, exec DBTX, targetUserID string) error {
	if err := lockEnabledAdmins(ctx, exec); err != nil {
		return err
	}
	var targetIsAdmin, othersExist bool
	if err := exec.QueryRowContext(ctx, adminGuardSQL, targetUserID).Scan(&targetIsAdmin, &othersExist); err != nil {
		return mapDBError(err)
	}
	if targetIsAdmin && !othersExist {
		return persistence.ErrLastAdmin
	}
	return nil
}

// backingGroupName is the deterministic, server-derived name of a
// user's single auto-managed backing group
// (admin-users-approval-ui-design.md §2). Derived from the user id, so
// a request can never target an arbitrary group.
func backingGroupName(userID string) string { return "auto-user-" + userID }

// roleRank orders roles for the "max across groups" aggregation in
// ListUsers (admin beats user beats awaiting).
var roleRank = map[string]int{"": 0, "user": 1, "admin": 2}

// ListUsers aggregates every user's effective role, project scope,
// active channel bindings and active-session count for /ui/admin/users
// (admin-users-approval-ui-design.md §3.1). Three small queries
// (base+role+projects, identities, session counts) merged in Go — user
// counts are tiny, and this keeps each statement readable and portable.
func (r *IdentityRepository) ListUsers(ctx context.Context) ([]persistence.UserAdminView, error) {
	// Query A: one row per (user, group, user-role project). admin-role
	// groups contribute the role but no projects (join condition gates
	// gp on g.role = 'user'); zero-group users yield one nil-role row.
	views, order, stars, projSets, err := r.scanListUsersBase(ctx)
	if err != nil {
		return nil, err
	}

	// Query B: active channel bindings.
	identRows, err := r.db.QueryContext(ctx,
		`SELECT user_id, channel, external_id, display FROM user_identities WHERE revoked_at IS NULL ORDER BY created_at`)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := func() error {
		defer func() { _ = identRows.Close() }()
		for identRows.Next() {
			var uid, ch, ext string
			var disp sql.NullString
			if err := identRows.Scan(&uid, &ch, &ext, &disp); err != nil {
				return mapDBError(err)
			}
			if v := views[uid]; v != nil {
				v.Identities = append(v.Identities, persistence.UserIdentityRef{
					Channel: ch, ExternalID: ext, Display: disp.String,
				})
			}
		}
		return mapDBError(identRows.Err())
	}(); err != nil {
		return nil, err
	}

	// Query C: active-session counts. `expires_at > now()` keeps this
	// count in lock-step with ListActiveByUser's drill-down predicate
	// and the session store's hard-expiry check (store.Validate). Without
	// it the count over-reports expired-but-not-revoked rows — the
	// ever-growing count + stale login-time IPs the operator reported
	// 2026-06-23 (worse with retention's 7-day grace before hard-delete).
	sessRows, err := r.db.QueryContext(ctx,
		`SELECT user_id, COUNT(*) FROM ui_sessions WHERE revoked_at IS NULL AND expires_at > now() GROUP BY user_id`)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := func() error {
		defer func() { _ = sessRows.Close() }()
		for sessRows.Next() {
			var uid string
			var n int
			if err := sessRows.Scan(&uid, &n); err != nil {
				return mapDBError(err)
			}
			if v := views[uid]; v != nil {
				v.ActiveSessions = n
			}
		}
		return mapDBError(sessRows.Err())
	}(); err != nil {
		return nil, err
	}

	// Finalize project sets: '*' collapses to exactly ["*"]; otherwise
	// sorted union. admin/awaiting users keep an empty set.
	out := make([]persistence.UserAdminView, 0, len(order))
	for _, uid := range order {
		v := views[uid]
		if stars[uid] {
			v.Projects = []string{"*"}
		} else if len(projSets[uid]) > 0 {
			ps := make([]string, 0, len(projSets[uid]))
			for p := range projSets[uid] {
				ps = append(ps, p)
			}
			sort.Strings(ps)
			v.Projects = ps
		}
		out = append(out, *v)
	}
	return out, nil
}

// scanListUsersBase runs query A and folds its rows into per-user
// views plus the project-accumulation state ListUsers finalizes.
func (r *IdentityRepository) scanListUsersBase(ctx context.Context) (
	views map[string]*persistence.UserAdminView, order []string,
	stars map[string]bool, projSets map[string]map[string]bool, err error,
) {
	rows, qerr := r.db.QueryContext(ctx,
		`SELECT u.id, u.display_name, (u.disabled_at IS NOT NULL) AS disabled, u.created_at, g.role, gp.project_id
FROM users u
LEFT JOIN group_members gm ON gm.user_id = u.id
LEFT JOIN groups g ON g.id = gm.group_id
LEFT JOIN group_projects gp ON gp.group_id = g.id AND g.role = 'user'
ORDER BY u.created_at`)
	if qerr != nil {
		return nil, nil, nil, nil, mapDBError(qerr)
	}
	defer func() { _ = rows.Close() }()
	views = map[string]*persistence.UserAdminView{}
	stars = map[string]bool{}
	projSets = map[string]map[string]bool{}
	for rows.Next() {
		var id, name string
		var disabled bool
		var created time.Time
		var role, proj sql.NullString
		if serr := rows.Scan(&id, &name, &disabled, &created, &role, &proj); serr != nil {
			return nil, nil, nil, nil, mapDBError(serr)
		}
		v := views[id]
		if v == nil {
			v = &persistence.UserAdminView{UserID: id, DisplayName: name, Disabled: disabled, CreatedAt: created}
			views[id] = v
			projSets[id] = map[string]bool{}
			order = append(order, id)
		}
		if role.Valid && roleRank[role.String] > roleRank[v.Role] {
			v.Role = role.String
		}
		if proj.Valid {
			if proj.String == "*" {
				stars[id] = true
			} else {
				projSets[id][proj.String] = true
			}
		}
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, nil, nil, nil, mapDBError(rerr)
	}
	return views, order, stars, projSets, nil
}

// SetUserAccess grants or re-grants a user's access via their single
// backing group, transactionally (admin-users-approval-ui-design.md
// §3.2). role must be "admin" or "user"; "user" requires ≥1 project.
func (r *IdentityRepository) SetUserAccess(ctx context.Context, userID, role string, projectIDs []string) error {
	if role != "admin" && role != "user" {
		return fmt.Errorf("SetUserAccess: invalid role %q (want admin or user)", role)
	}
	if role == "user" && len(projectIDs) == 0 {
		return persistence.ErrNoProjects
	}
	tx, ok, err := persistence.BeginTx(ctx, r.db, nil)
	if err != nil {
		return mapDBError(err)
	}
	if !ok {
		return r.setUserAccessWithExec(ctx, r.db, userID, role, projectIDs)
	}
	defer func() { _ = tx.Rollback() }()
	if err := r.setUserAccessWithExec(ctx, tx, userID, role, projectIDs); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return mapDBError(err)
	}
	return nil
}

func (r *IdentityRepository) setUserAccessWithExec(ctx context.Context, exec DBTX, userID, role string, projectIDs []string) error {
	// Last-admin guard (review finding #1): a grant with role != admin
	// can demote the sole admin. Refuse it (the lock serializes
	// concurrent removals). Conservative: if the user is also a
	// bootstrap-admin this still blocks, but the break-glass key
	// recovers and demoting the last admin is never the intent.
	if role != "admin" {
		if err := r.guardLastAdmin(ctx, exec, userID); err != nil {
			return err
		}
	}
	name := backingGroupName(userID)
	var groupID, existingRole string
	err := exec.QueryRowContext(ctx,
		`SELECT id, role FROM groups WHERE name = $1`, name).Scan(&groupID, &existingRole)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		groupID = persistence.GenerateID("grp")
		if _, err := exec.ExecContext(ctx,
			`INSERT INTO groups (id, name, role, description, created_at) VALUES ($1, $2, $3, $4, $5)`,
			groupID, name, role, "auto-managed backing group for "+userID, time.Now().UTC()); err != nil {
			return mapDBError(err)
		}
	case err != nil:
		return mapDBError(err)
	default:
		if existingRole != role {
			if _, err := exec.ExecContext(ctx,
				`UPDATE groups SET role = $2 WHERE id = $1`, groupID, role); err != nil {
				return mapDBError(err)
			}
		}
	}
	// Replace the project set wholesale; admin role keeps it empty
	// (admin is instance-wide).
	if _, err := exec.ExecContext(ctx,
		`DELETE FROM group_projects WHERE group_id = $1`, groupID); err != nil {
		return mapDBError(err)
	}
	if role == "user" {
		for _, p := range projectIDs {
			if _, err := exec.ExecContext(ctx,
				`INSERT INTO group_projects (group_id, project_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
				groupID, p); err != nil {
				return mapDBError(err)
			}
		}
	}
	if _, err := exec.ExecContext(ctx,
		`INSERT INTO group_members (group_id, user_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		groupID, userID); err != nil {
		return mapDBError(err)
	}
	// Access changed → revoke the user's sessions so the new principal
	// is resolved fresh on next request (mirrors SetGroupProjects).
	return r.revokeSessionsForUserWithExec(ctx, exec, userID)
}

// RemoveUserAccess drops the user's backing-group membership (→ awaiting
// access) and revokes their sessions, transactionally. No-op when the
// user has no backing group. Other memberships are untouched.
func (r *IdentityRepository) RemoveUserAccess(ctx context.Context, userID string) error {
	tx, ok, err := persistence.BeginTx(ctx, r.db, nil)
	if err != nil {
		return mapDBError(err)
	}
	if !ok {
		return r.removeUserAccessWithExec(ctx, r.db, userID)
	}
	defer func() { _ = tx.Rollback() }()
	if err := r.removeUserAccessWithExec(ctx, tx, userID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return mapDBError(err)
	}
	return nil
}

func (r *IdentityRepository) removeUserAccessWithExec(ctx context.Context, exec DBTX, userID string) error {
	// Last-admin guard (review finding #1): removing a backing-group
	// membership can drop the sole admin. Refuse it (the lock serializes
	// concurrent removals).
	if err := r.guardLastAdmin(ctx, exec, userID); err != nil {
		return err
	}
	var groupID string
	err := exec.QueryRowContext(ctx,
		`SELECT id FROM groups WHERE name = $1`, backingGroupName(userID)).Scan(&groupID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // no backing group → nothing to remove
	}
	if err != nil {
		return mapDBError(err)
	}
	if _, err := exec.ExecContext(ctx,
		`DELETE FROM group_members WHERE group_id = $1 AND user_id = $2`, groupID, userID); err != nil {
		return mapDBError(err)
	}
	return r.revokeSessionsForUserWithExec(ctx, exec, userID)
}

// scanPrincipalRows drains a *sql.Rows whose columns match the
// PrincipalRow shape (user_id, display_name, disabled, role,
// project_id) into a slice. Shared by both resolver queries so the
// scan loop + error mapping lives in one place; the two public
// methods keep their SQL distinct (and individually pinned).
func scanPrincipalRows(rows *sql.Rows) ([]persistence.PrincipalRow, error) {
	defer func() { _ = rows.Close() }()
	var out []persistence.PrincipalRow
	for rows.Next() {
		var pr persistence.PrincipalRow
		if err := rows.Scan(&pr.UserID, &pr.DisplayName, &pr.Disabled, &pr.Role, &pr.ProjectID); err != nil {
			return nil, mapDBError(err)
		}
		out = append(out, pr)
	}
	if err := rows.Err(); err != nil {
		return nil, mapDBError(err)
	}
	return out, nil
}
