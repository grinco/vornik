package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// IdentityRepository implements persistence.IdentityRepository on SQLite —
// the identity-core storage parity of the 2026-09-13 config-assistant plan
// (§2, review amendment R1). It is a statement-for-statement port of the
// Postgres repository, which stays the semantic source of truth: the
// last-admin guard (ErrLastAdmin), the per-user backing group
// `auto-user-<id>`, session revocation inside the mutating transaction,
// BindIdentity's upsert-on-revoked-only, MigrateIdentityExternalID's
// conflict rule, ListUsers' role-rank aggregation with the '*' collapse,
// and the R3 access_revoked_at marker. The shared suites
// (repotest.RunIdentityRepositorySuite / RunIdentityAdminSuite) run
// against both backends so a divergence is a test failure, not a
// Community-only surprise.
//
// Dialect notes:
//
//   - NOW() → the caller's clock, passed as sqliteTime(time.Now().UTC()).
//   - FOR UPDATE does not exist. The last-admin guard runs inside the
//     mutation's transaction (persistence.BeginTx); on SQLite a
//     transaction that read the admin set and then writes after another
//     writer committed gets SQLITE_BUSY rather than proceeding on a stale
//     read, so the TOCTOU the Postgres row lock closes fails CLOSED here.
//   - The DSN opens with foreign_keys(OFF): ON DELETE CASCADE never fires
//     and nothing below relies on it.
//   - Provisioning issues several of these calls in sequence without a
//     transaction (see the Postgres file header); BindIdentity is the
//     canonicalisation point on both backends.
type IdentityRepository struct {
	db DBTX
}

// NewIdentityRepository constructs the repository.
func NewIdentityRepository(db DBTX) *IdentityRepository {
	return &IdentityRepository{db: db}
}

// Compile-time interface pin.
var _ persistence.IdentityRepository = (*IdentityRepository)(nil)

// mapIdentityDBError mirrors the Postgres mapDBError for the identity
// tables: a UNIQUE violation is ErrDuplicateKey and no-rows is ErrNotFound;
// anything else passes through unchanged.
func mapIdentityDBError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return persistence.ErrNotFound
	}
	if strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return persistence.ErrDuplicateKey
	}
	return err
}

// nowText is the clock every NOW() in the Postgres statements becomes.
func nowText() string { return sqliteTime(time.Now().UTC()) }

// withTx runs fn inside a transaction on r.db when r.db is a pool, or
// inline when the caller already owns one (persistence.BeginTx semantics).
func (r *IdentityRepository) withTx(ctx context.Context, fn func(exec DBTX) error) error {
	tx, ok, err := persistence.BeginTx(ctx, r.db, nil)
	if err != nil {
		return mapIdentityDBError(err)
	}
	if !ok {
		return fn(r.db)
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	return mapIdentityDBError(tx.Commit())
}

// CreateUser inserts a user row.
func (r *IdentityRepository) CreateUser(ctx context.Context, u *persistence.User) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO users (id, display_name, created_at) VALUES (?, ?, ?)`,
		u.ID, u.DisplayName, sqliteTime(u.CreatedAt))
	return mapIdentityDBError(err)
}

// SetUserDisabled flips users.disabled_at (true → now, false → NULL).
// Disabling also revokes the user's active sessions in the same
// transaction (audit A2) and is refused for the sole enabled admin
// (ErrLastAdmin). Re-enabling does not resurrect sessions.
func (r *IdentityRepository) SetUserDisabled(ctx context.Context, userID string, disabled bool) error {
	if !disabled {
		res, err := r.db.ExecContext(ctx,
			`UPDATE users SET disabled_at = NULL WHERE id = ?`, userID)
		if err != nil {
			return mapIdentityDBError(err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return persistence.ErrUserNotFound
		}
		return nil
	}
	return r.withTx(ctx, func(exec DBTX) error {
		return r.disableUserWithExec(ctx, exec, userID)
	})
}

func (r *IdentityRepository) disableUserWithExec(ctx context.Context, exec DBTX, userID string) error {
	if err := guardLastAdmin(ctx, exec, userID); err != nil {
		return err
	}
	res, err := exec.ExecContext(ctx,
		`UPDATE users SET disabled_at = ? WHERE id = ?`, nowText(), userID)
	if err != nil {
		return mapIdentityDBError(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return persistence.ErrUserNotFound
	}
	return revokeSessionsForUserWithExec(ctx, exec, userID)
}

// RevokeSessionsForUser revokes every active ui_sessions row for userID.
// Zero active sessions is not an error.
func (r *IdentityRepository) RevokeSessionsForUser(ctx context.Context, userID string) error {
	return revokeSessionsForUserWithExec(ctx, r.db, userID)
}

func revokeSessionsForUserWithExec(ctx context.Context, exec DBTX, userID string) error {
	_, err := exec.ExecContext(ctx,
		`UPDATE ui_sessions SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL`,
		nowText(), userID)
	return mapIdentityDBError(err)
}

// revokeSessionsForGroupWithExec revokes every active session of a member
// of groupID — a narrowed project set must take effect now, not at the
// principal-cache TTL.
func revokeSessionsForGroupWithExec(ctx context.Context, exec DBTX, groupID string) error {
	_, err := exec.ExecContext(ctx,
		`UPDATE ui_sessions SET revoked_at = ?
		 WHERE revoked_at IS NULL
		   AND user_id IN (SELECT user_id FROM group_members WHERE group_id = ?)`,
		nowText(), groupID)
	return mapIdentityDBError(err)
}

// CreateGroup inserts a group row. An empty description lands as NULL.
func (r *IdentityRepository) CreateGroup(ctx context.Context, g *persistence.Group) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO groups (id, name, role, description, created_at) VALUES (?, ?, ?, ?, ?)`,
		g.ID, g.Name, g.Role, nullStr(g.Description), sqliteTime(g.CreatedAt))
	return mapIdentityDBError(err)
}

// GetGroupByName fetches a group by its unique name; ErrGroupNotFound on a
// miss (which also satisfies errors.Is(err, ErrNotFound)).
func (r *IdentityRepository) GetGroupByName(ctx context.Context, name string) (*persistence.Group, error) {
	var g persistence.Group
	var desc sql.NullString
	var created sqlTime
	err := r.db.QueryRowContext(ctx,
		`SELECT id, name, role, description, created_at FROM groups WHERE name = ?`,
		name).Scan(&g.ID, &g.Name, &g.Role, &desc, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrGroupNotFound
	}
	if err != nil {
		return nil, mapIdentityDBError(err)
	}
	g.Description = desc.String
	g.CreatedAt = created.Time
	return &g, nil
}

// SetGroupProjects replaces the group's project set WHOLESALE inside one
// transaction and revokes the members' sessions in the same transaction.
func (r *IdentityRepository) SetGroupProjects(ctx context.Context, groupID string, projects []string) error {
	return r.withTx(ctx, func(exec DBTX) error {
		if err := replaceGroupProjectsWithExec(ctx, exec, groupID, projects); err != nil {
			return err
		}
		return revokeSessionsForGroupWithExec(ctx, exec, groupID)
	})
}

// replaceGroupProjectsWithExec is the DELETE + INSERTs both
// SetGroupProjects and SetUserAccess perform.
func replaceGroupProjectsWithExec(ctx context.Context, exec DBTX, groupID string, projects []string) error {
	if _, err := exec.ExecContext(ctx,
		`DELETE FROM group_projects WHERE group_id = ?`, groupID); err != nil {
		return mapIdentityDBError(err)
	}
	for _, p := range projects {
		if _, err := exec.ExecContext(ctx,
			`INSERT INTO group_projects (group_id, project_id) VALUES (?, ?) ON CONFLICT DO NOTHING`,
			groupID, p); err != nil {
			return mapIdentityDBError(err)
		}
	}
	return nil
}

// SetGroupRole updates a group's role; a missing group is not an error.
func (r *IdentityRepository) SetGroupRole(ctx context.Context, groupID, role string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE groups SET role = ? WHERE id = ?`, role, groupID)
	return mapIdentityDBError(err)
}

// AddGroupMember inserts a membership; idempotent.
func (r *IdentityRepository) AddGroupMember(ctx context.Context, groupID, userID string) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO group_members (group_id, user_id) VALUES (?, ?) ON CONFLICT DO NOTHING`,
		groupID, userID)
	return mapIdentityDBError(err)
}

// RemoveGroupMember deletes a membership; an absent row is not an error.
func (r *IdentityRepository) RemoveGroupMember(ctx context.Context, groupID, userID string) error {
	_, err := r.db.ExecContext(ctx,
		`DELETE FROM group_members WHERE group_id = ? AND user_id = ?`, groupID, userID)
	return mapIdentityDBError(err)
}

// BindIdentity inserts a channel binding or reactivates a REVOKED one. An
// active conflicting binding is never repointed (the DO UPDATE ... WHERE
// guard), so concurrent first-contact provisioners converge on one user.
// BindIdentity — the DO UPDATE fires for a revoked row (repoint) and for a
// row already held by the SAME user (idempotent re-bind, which also keeps
// display fresh). An active row held by someone else matches neither, so
// zero rows are affected and bound is false. See the interface contract:
// that case used to be indistinguishable from success.
func (r *IdentityRepository) BindIdentity(ctx context.Context, id *persistence.UserIdentity) (bool, error) {
	res, err := r.db.ExecContext(ctx,
		`INSERT INTO user_identities (id, user_id, channel, external_id, display, created_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (channel, external_id)
DO UPDATE SET user_id = excluded.user_id, display = excluded.display, revoked_at = NULL
WHERE user_identities.revoked_at IS NOT NULL
   OR user_identities.user_id = excluded.user_id`,
		id.ID, id.UserID, id.Channel, id.ExternalID, nullStr(id.Display), sqliteTime(id.CreatedAt))
	if err != nil {
		return false, mapIdentityDBError(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, mapIdentityDBError(err)
	}
	return n > 0, nil
}

// RebindIdentity is BindIdentity without the guard: admin authority repoints
// an active binding held by another user.
func (r *IdentityRepository) RebindIdentity(ctx context.Context, id *persistence.UserIdentity) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO user_identities (id, user_id, channel, external_id, display, created_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (channel, external_id)
DO UPDATE SET user_id = excluded.user_id, display = excluded.display, revoked_at = NULL`,
		id.ID, id.UserID, id.Channel, id.ExternalID, nullStr(id.Display), sqliteTime(id.CreatedAt))
	return mapIdentityDBError(err)
}

// MigrateIdentityExternalID repoints an ACTIVE binding from a legacy
// external_id to newExternalID within the same channel, with the Postgres
// conflict rule: the repoint fires only when the new key is free
// (NOT EXISTS guard against UNIQUE(channel, external_id), which spans
// revoked rows); when the new key is taken that row is canonical and the
// legacy row is revoked instead. Absent/revoked legacy rows are a no-op.
func (r *IdentityRepository) MigrateIdentityExternalID(ctx context.Context, channel, oldExternalID, newExternalID string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE user_identities SET external_id = ?3
WHERE channel = ?1 AND external_id = ?2 AND revoked_at IS NULL
  AND NOT EXISTS (
    SELECT 1 FROM user_identities existing
    WHERE existing.channel = ?1 AND existing.external_id = ?3
  )`,
		channel, oldExternalID, newExternalID)
	if err != nil {
		return mapIdentityDBError(err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	_, err = r.db.ExecContext(ctx,
		`UPDATE user_identities SET revoked_at = ?
WHERE channel = ? AND external_id = ? AND revoked_at IS NULL`,
		nowText(), channel, oldExternalID)
	return mapIdentityDBError(err)
}

// RevokeIdentityOwnedBy is RevokeIdentity with the expected owner in the
// UPDATE's WHERE clause, making the ownership test and the write atomic
// (audit 2026-09-15 CA-10).
func (r *IdentityRepository) RevokeIdentityOwnedBy(ctx context.Context, channel, externalID, expectedUserID string) error {
	return r.withTx(ctx, func(exec DBTX) error {
		var userID string
		err := exec.QueryRowContext(ctx,
			`UPDATE user_identities SET revoked_at = ?
			 WHERE channel = ? AND external_id = ? AND revoked_at IS NULL AND user_id = ?
			 RETURNING user_id`,
			nowText(), channel, externalID, expectedUserID).Scan(&userID)
		if errors.Is(err, sql.ErrNoRows) {
			return persistence.ErrIdentityNotFound
		}
		if err != nil {
			return mapIdentityDBError(err)
		}
		return revokeSessionsForUserWithExec(ctx, exec, userID)
	})
}

// RevokeIdentity soft-deletes an ACTIVE binding and revokes the owning
// user's sessions in the same transaction; ErrIdentityNotFound when no
// active binding matches.
func (r *IdentityRepository) RevokeIdentity(ctx context.Context, channel, externalID string) error {
	return r.withTx(ctx, func(exec DBTX) error {
		var userID string
		err := exec.QueryRowContext(ctx,
			`UPDATE user_identities SET revoked_at = ?
			 WHERE channel = ? AND external_id = ? AND revoked_at IS NULL
			 RETURNING user_id`,
			nowText(), channel, externalID).Scan(&userID)
		if errors.Is(err, sql.ErrNoRows) {
			return persistence.ErrIdentityNotFound
		}
		if err != nil {
			return mapIdentityDBError(err)
		}
		return revokeSessionsForUserWithExec(ctx, exec, userID)
	})
}

// TouchIdentityLastUsed updates last_used_at; zero rows is not an error.
func (r *IdentityRepository) TouchIdentityLastUsed(ctx context.Context, channel, externalID string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE user_identities SET last_used_at = ? WHERE channel = ? AND external_id = ? AND revoked_at IS NULL`,
		nowText(), channel, externalID)
	return mapIdentityDBError(err)
}

// ResolvePrincipalRows is the resolver's single round trip: active binding
// → user → groups → projects, LEFT-joined so a zero-group user still
// yields one nil-Role row and zero rows means "unknown identity".
func (r *IdentityRepository) ResolvePrincipalRows(ctx context.Context, channel, externalID string) ([]persistence.PrincipalRow, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT u.id, u.display_name, (u.disabled_at IS NOT NULL) AS disabled, g.role, gp.project_id
FROM user_identities ui
JOIN users u ON u.id = ui.user_id
LEFT JOIN group_members gm ON gm.user_id = u.id
LEFT JOIN groups g ON g.id = gm.group_id
LEFT JOIN group_projects gp ON gp.group_id = g.id
WHERE ui.channel = ? AND ui.external_id = ? AND ui.revoked_at IS NULL`,
		channel, externalID)
	if err != nil {
		return nil, mapIdentityDBError(err)
	}
	return scanPrincipalRows(rows)
}

// ResolveUserPrincipalRows is the session-path resolver keyed on users.id;
// same row contract as ResolvePrincipalRows, zero rows = no such user.
func (r *IdentityRepository) ResolveUserPrincipalRows(ctx context.Context, userID string) ([]persistence.PrincipalRow, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT u.id, u.display_name, (u.disabled_at IS NOT NULL) AS disabled, g.role, gp.project_id
FROM users u
LEFT JOIN group_members gm ON gm.user_id = u.id
LEFT JOIN groups g ON g.id = gm.group_id
LEFT JOIN group_projects gp ON gp.group_id = g.id
WHERE u.id = ?`,
		userID)
	if err != nil {
		return nil, mapIdentityDBError(err)
	}
	return scanPrincipalRows(rows)
}

func scanPrincipalRows(rows *sql.Rows) ([]persistence.PrincipalRow, error) {
	defer func() { _ = rows.Close() }()
	var out []persistence.PrincipalRow
	for rows.Next() {
		var pr persistence.PrincipalRow
		if err := rows.Scan(&pr.UserID, &pr.DisplayName, &pr.Disabled, &pr.Role, &pr.ProjectID); err != nil {
			return nil, mapIdentityDBError(err)
		}
		out = append(out, pr)
	}
	if err := rows.Err(); err != nil {
		return nil, mapIdentityDBError(err)
	}
	return out, nil
}

// adminGuardSQL reports whether the target is currently an enabled admin
// and whether any OTHER enabled admin exists — one round trip, so the
// guard only fires for the last admin and never blocks a non-admin.
const adminGuardSQL = `SELECT
  EXISTS(SELECT 1 FROM users u JOIN group_members gm ON gm.user_id = u.id JOIN groups g ON g.id = gm.group_id
         WHERE u.id = ?1 AND g.role = 'admin' AND u.disabled_at IS NULL),
  EXISTS(SELECT 1 FROM users u JOIN group_members gm ON gm.user_id = u.id JOIN groups g ON g.id = gm.group_id
         WHERE u.id <> ?1 AND g.role = 'admin' AND u.disabled_at IS NULL)`

// guardLastAdmin refuses (ErrLastAdmin) an action whose target is the sole
// enabled admin. Must run inside the mutation's transaction, before the
// mutation: SQLite has no FOR UPDATE, but a deferred transaction that read
// here and then writes after a concurrent commit fails with SQLITE_BUSY
// instead of acting on the stale count (fails closed).
func guardLastAdmin(ctx context.Context, exec DBTX, targetUserID string) error {
	var targetIsAdmin, othersExist bool
	if err := exec.QueryRowContext(ctx, adminGuardSQL, targetUserID).Scan(&targetIsAdmin, &othersExist); err != nil {
		return mapIdentityDBError(err)
	}
	if targetIsAdmin && !othersExist {
		return persistence.ErrLastAdmin
	}
	return nil
}

// backingGroupName is the deterministic name of a user's single
// auto-managed backing group (admin-users-approval-ui-design.md §2).
func backingGroupName(userID string) string { return "auto-user-" + userID }

// roleRank orders roles for the "max across groups" aggregation.
var roleRank = map[string]int{"": 0, "user": 1, "admin": 2}

// listUsersState is the fold state ListUsers builds from its three
// queries before finalising the per-user project sets.
type listUsersState struct {
	views    map[string]*persistence.UserAdminView
	order    []string
	stars    map[string]bool
	projSets map[string]map[string]bool
}

// ListUsers aggregates every user's effective role, project scope, active
// channel bindings and active-session count for the admin Users page.
// Three small queries merged in Go, exactly as on Postgres.
func (r *IdentityRepository) ListUsers(ctx context.Context) ([]persistence.UserAdminView, error) {
	st, err := r.scanListUsersBase(ctx)
	if err != nil {
		return nil, err
	}
	if err := r.mergeListUsersIdentities(ctx, st); err != nil {
		return nil, err
	}
	if err := r.mergeListUsersSessionCounts(ctx, st); err != nil {
		return nil, err
	}
	out := make([]persistence.UserAdminView, 0, len(st.order))
	for _, uid := range st.order {
		v := st.views[uid]
		if st.stars[uid] {
			v.Projects = []string{"*"}
		} else if len(st.projSets[uid]) > 0 {
			ps := make([]string, 0, len(st.projSets[uid]))
			for p := range st.projSets[uid] {
				ps = append(ps, p)
			}
			sort.Strings(ps)
			v.Projects = ps
		}
		out = append(out, *v)
	}
	return out, nil
}

// scanListUsersBase runs query A: one row per (user, group, user-role
// project); admin-role groups contribute the role but no projects.
func (r *IdentityRepository) scanListUsersBase(ctx context.Context) (*listUsersState, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT u.id, u.display_name, (u.disabled_at IS NOT NULL) AS disabled, u.created_at, g.role, gp.project_id
FROM users u
LEFT JOIN group_members gm ON gm.user_id = u.id
LEFT JOIN groups g ON g.id = gm.group_id
LEFT JOIN group_projects gp ON gp.group_id = g.id AND g.role = 'user'
ORDER BY u.created_at`)
	if err != nil {
		return nil, mapIdentityDBError(err)
	}
	defer func() { _ = rows.Close() }()
	st := &listUsersState{
		views:    map[string]*persistence.UserAdminView{},
		stars:    map[string]bool{},
		projSets: map[string]map[string]bool{},
	}
	for rows.Next() {
		var id, name string
		var disabled bool
		var created sqlTime
		var role, proj sql.NullString
		if err := rows.Scan(&id, &name, &disabled, &created, &role, &proj); err != nil {
			return nil, mapIdentityDBError(err)
		}
		v := st.views[id]
		if v == nil {
			v = &persistence.UserAdminView{UserID: id, DisplayName: name, Disabled: disabled, CreatedAt: created.Time}
			st.views[id] = v
			st.projSets[id] = map[string]bool{}
			st.order = append(st.order, id)
		}
		if role.Valid && roleRank[role.String] > roleRank[v.Role] {
			v.Role = role.String
		}
		if proj.Valid {
			if proj.String == "*" {
				st.stars[id] = true
			} else {
				st.projSets[id][proj.String] = true
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, mapIdentityDBError(err)
	}
	return st, nil
}

// mergeListUsersIdentities runs query B: active channel bindings.
func (r *IdentityRepository) mergeListUsersIdentities(ctx context.Context, st *listUsersState) error {
	rows, err := r.db.QueryContext(ctx,
		`SELECT user_id, channel, external_id, display FROM user_identities WHERE revoked_at IS NULL ORDER BY created_at`)
	if err != nil {
		return mapIdentityDBError(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var uid, ch, ext string
		var disp sql.NullString
		if err := rows.Scan(&uid, &ch, &ext, &disp); err != nil {
			return mapIdentityDBError(err)
		}
		if v := st.views[uid]; v != nil {
			v.Identities = append(v.Identities, persistence.UserIdentityRef{
				Channel: ch, ExternalID: ext, Display: disp.String,
			})
		}
	}
	return mapIdentityDBError(rows.Err())
}

// mergeListUsersSessionCounts runs query C: active-session counts with the
// same predicate as ListActiveByUser (revoked_at IS NULL AND expires_at >
// now), so the count matches the drill-down list length.
func (r *IdentityRepository) mergeListUsersSessionCounts(ctx context.Context, st *listUsersState) error {
	rows, err := r.db.QueryContext(ctx,
		`SELECT user_id, COUNT(*) FROM ui_sessions WHERE revoked_at IS NULL AND expires_at > ? GROUP BY user_id`,
		nowText())
	if err != nil {
		return mapIdentityDBError(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var uid string
		var n int
		if err := rows.Scan(&uid, &n); err != nil {
			return mapIdentityDBError(err)
		}
		if v := st.views[uid]; v != nil {
			v.ActiveSessions = n
		}
	}
	return mapIdentityDBError(rows.Err())
}

// SetUserAccess grants or re-grants a user's access via their single
// backing group, transactionally. role must be "admin" or "user"; "user"
// requires at least one project (ErrNoProjects).
func (r *IdentityRepository) SetUserAccess(ctx context.Context, userID, role string, projectIDs []string) error {
	if role != "admin" && role != "user" {
		return fmt.Errorf("SetUserAccess: invalid role %q (want admin or user)", role)
	}
	if role == "user" && len(projectIDs) == 0 {
		return persistence.ErrNoProjects
	}
	return r.withTx(ctx, func(exec DBTX) error {
		return setUserAccessWithExec(ctx, exec, userID, role, projectIDs)
	})
}

func setUserAccessWithExec(ctx context.Context, exec DBTX, userID, role string, projectIDs []string) error {
	// A grant with role != admin can demote the sole admin; refuse it.
	if role != "admin" {
		if err := guardLastAdmin(ctx, exec, userID); err != nil {
			return err
		}
	}
	groupID, err := ensureBackingGroupWithExec(ctx, exec, userID, role)
	if err != nil {
		return err
	}
	// Replace the project set wholesale; admin keeps it empty.
	var projects []string
	if role == "user" {
		projects = projectIDs
	}
	if err := replaceGroupProjectsWithExec(ctx, exec, groupID, projects); err != nil {
		return err
	}
	if _, err := exec.ExecContext(ctx,
		`INSERT INTO group_members (group_id, user_id) VALUES (?, ?) ON CONFLICT DO NOTHING`,
		groupID, userID); err != nil {
		return mapIdentityDBError(err)
	}
	// A grant is the admin's deliberate decision: clear the R3 revocation
	// marker so provisioning may act on this user again.
	if _, err := exec.ExecContext(ctx, clearAccessRevokedSQL, userID); err != nil {
		return mapIdentityDBError(err)
	}
	// Access changed → revoke sessions so the new principal is resolved
	// fresh on the next request.
	return revokeSessionsForUserWithExec(ctx, exec, userID)
}

// ensureBackingGroupWithExec finds or creates the user's backing group and
// re-syncs its role, returning the group id.
func ensureBackingGroupWithExec(ctx context.Context, exec DBTX, userID, role string) (string, error) {
	name := backingGroupName(userID)
	var groupID, existingRole string
	err := exec.QueryRowContext(ctx,
		`SELECT id, role FROM groups WHERE name = ?`, name).Scan(&groupID, &existingRole)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		groupID = persistence.GenerateID("grp")
		if _, err := exec.ExecContext(ctx,
			`INSERT INTO groups (id, name, role, description, created_at) VALUES (?, ?, ?, ?, ?)`,
			groupID, name, role, "auto-managed backing group for "+userID, nowText()); err != nil {
			return "", mapIdentityDBError(err)
		}
	case err != nil:
		return "", mapIdentityDBError(err)
	default:
		if existingRole != role {
			if _, err := exec.ExecContext(ctx,
				`UPDATE groups SET role = ? WHERE id = ?`, role, groupID); err != nil {
				return "", mapIdentityDBError(err)
			}
		}
	}
	return groupID, nil
}

// stampAccessRevokedSQL / clearAccessRevokedSQL maintain
// users.access_revoked_at, the "bootstrap must not re-grant deliberately
// revoked access" marker (2026-09-13 config-assistant review R3; Postgres
// migration 183). RemoveUserAccess stamps it as soon as the last-admin
// guard passes — before the backing-group lookup, because the admin's
// decision is the same whether or not a backing group happened to exist;
// SetUserAccess clears it.
const (
	stampAccessRevokedSQL = `UPDATE users SET access_revoked_at = ? WHERE id = ?`
	clearAccessRevokedSQL = `UPDATE users SET access_revoked_at = NULL WHERE id = ?`
)

// AccessRevokedAt implements persistence.AccessRevocationReader.
func (r *IdentityRepository) AccessRevokedAt(ctx context.Context, userID string) (*time.Time, error) {
	var at sqlNullTime
	err := r.db.QueryRowContext(ctx, `SELECT access_revoked_at FROM users WHERE id = ?`, userID).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrUserNotFound
	}
	if err != nil {
		return nil, mapIdentityDBError(err)
	}
	if !at.Valid {
		return nil, nil
	}
	t := at.Time
	return &t, nil
}

var _ persistence.AccessRevocationReader = (*IdentityRepository)(nil)

// RemoveUserAccess drops the user's backing-group membership (→ awaiting
// access), stamps the R3 marker and revokes their sessions,
// transactionally. Without a backing group only the marker is written.
func (r *IdentityRepository) RemoveUserAccess(ctx context.Context, userID string) error {
	return r.withTx(ctx, func(exec DBTX) error {
		if err := guardLastAdmin(ctx, exec, userID); err != nil {
			return err
		}
		if _, err := exec.ExecContext(ctx, stampAccessRevokedSQL, nowText(), userID); err != nil {
			return mapIdentityDBError(err)
		}
		var groupID string
		err := exec.QueryRowContext(ctx,
			`SELECT id FROM groups WHERE name = ?`, backingGroupName(userID)).Scan(&groupID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // no backing group → no membership to remove
		}
		if err != nil {
			return mapIdentityDBError(err)
		}
		if _, err := exec.ExecContext(ctx,
			`DELETE FROM group_members WHERE group_id = ? AND user_id = ?`, groupID, userID); err != nil {
			return mapIdentityDBError(err)
		}
		return revokeSessionsForUserWithExec(ctx, exec, userID)
	})
}
