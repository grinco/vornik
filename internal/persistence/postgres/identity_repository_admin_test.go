package postgres

import (
	"context"
	"database/sql"
	"regexp"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"vornik.io/vornik/internal/persistence"
)

// ts parses an RFC3339 timestamp for table rows; panics on a bad
// literal (test-only).
func ts(s string) time.Time {
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return tm
}

// sqlNoRows is the driver "no rows" sentinel, returned by sqlmock for
// an absent-group SELECT.
func sqlNoRows() error { return sql.ErrNoRows }

// SQL pins for the admin-users methods (admin-users-approval-ui-design.md
// §3). Kept as constants so every test matches the production SQL verbatim.
const listUsersBaseSQL = `SELECT u.id, u.display_name, (u.disabled_at IS NOT NULL) AS disabled, u.created_at, g.role, gp.project_id
FROM users u
LEFT JOIN group_members gm ON gm.user_id = u.id
LEFT JOIN groups g ON g.id = gm.group_id
LEFT JOIN group_projects gp ON gp.group_id = g.id AND g.role = 'user'
ORDER BY u.created_at`

const listUsersIdentSQL = `SELECT user_id, channel, external_id, display FROM user_identities WHERE revoked_at IS NULL ORDER BY created_at`

// Must carry `expires_at > now()` so the Users-page count and the
// drill-down list (ListActiveByUser) use the SAME active predicate.
// Without it the count over-reports expired-but-not-revoked rows —
// the ever-growing session count + stale login-time IPs the operator
// reported 2026-06-23 — and would diverge from the (already-fixed)
// drill-down list length.
const listUsersSessSQL = `SELECT user_id, COUNT(*) FROM ui_sessions WHERE revoked_at IS NULL AND expires_at > now() GROUP BY user_id`

const selectBackingGroupSQL = `SELECT id, role FROM groups WHERE name = $1`
const selectBackingGroupIDSQL = `SELECT id FROM groups WHERE name = $1`
const insertGroupSQL = `INSERT INTO groups (id, name, role, description, created_at) VALUES ($1, $2, $3, $4, $5)`
const updateGroupRoleSQL = `UPDATE groups SET role = $2 WHERE id = $1`
const deleteProjectsSQL = `DELETE FROM group_projects WHERE group_id = $1`
const insertProjectSQL = `INSERT INTO group_projects (group_id, project_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`
const insertMemberSQL = `INSERT INTO group_members (group_id, user_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`
const deleteMemberSQL = `DELETE FROM group_members WHERE group_id = $1 AND user_id = $2`
const revokeUserSessSQL = `UPDATE ui_sessions SET revoked_at = NOW() WHERE user_id = $1 AND revoked_at IS NULL`

func TestIdentityRepository_ListUsers_Aggregates(t *testing.T) {
	// One awaiting user (no groups), one admin (no projects), one
	// user-role member with two projects. Verifies role = max, projects
	// union for user-role only, identities + session counts merged.
	repo, mock, done := newIdentityMock(t)
	defer done()

	base := sqlmock.NewRows([]string{"id", "display_name", "disabled", "created_at", "role", "project_id"}).
		AddRow("user_await", "@janka-lang", false, ts("2026-06-22T18:18:41Z"), nil, nil).
		AddRow("user_admin", "Vadim", false, ts("2026-06-05T21:22:50Z"), "admin", nil).
		AddRow("user_member", "Bob", false, ts("2026-06-10T10:00:00Z"), "user", "janka").
		AddRow("user_member", "Bob", false, ts("2026-06-10T10:00:00Z"), "user", "snake")
	mock.ExpectQuery(regexp.QuoteMeta(listUsersBaseSQL)).WillReturnRows(base)

	idents := sqlmock.NewRows([]string{"user_id", "channel", "external_id", "display"}).
		AddRow("user_await", "github", "295941391", "@janka-lang").
		AddRow("user_admin", "github", "5553528", "Vadim Grinco (@grinco)")
	mock.ExpectQuery(regexp.QuoteMeta(listUsersIdentSQL)).WillReturnRows(idents)

	sess := sqlmock.NewRows([]string{"user_id", "count"}).
		AddRow("user_admin", 2)
	mock.ExpectQuery(regexp.QuoteMeta(listUsersSessSQL)).WillReturnRows(sess)

	got, err := repo.ListUsers(context.Background())
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	byID := map[string]persistence.UserAdminView{}
	for _, u := range got {
		byID[u.UserID] = u
	}

	if u := byID["user_await"]; u.Role != "" || len(u.Projects) != 0 {
		t.Errorf("awaiting: role=%q projects=%v, want empty/empty", u.Role, u.Projects)
	}
	if u := byID["user_await"]; len(u.Identities) != 1 || u.Identities[0].ExternalID != "295941391" {
		t.Errorf("awaiting identities = %+v, want one github 295941391", u.Identities)
	}
	if u := byID["user_admin"]; u.Role != "admin" || len(u.Projects) != 0 {
		t.Errorf("admin: role=%q projects=%v, want admin/empty", u.Role, u.Projects)
	}
	if u := byID["user_admin"]; u.ActiveSessions != 2 {
		t.Errorf("admin sessions = %d, want 2", u.ActiveSessions)
	}
	if u := byID["user_member"]; u.Role != "user" || len(u.Projects) != 2 ||
		u.Projects[0] != "janka" || u.Projects[1] != "snake" {
		t.Errorf("member: role=%q projects=%v, want user/[janka snake]", u.Role, u.Projects)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestIdentityRepository_ListUsers_StarCollapses(t *testing.T) {
	// A user-role group scoped to '*' collapses to exactly ["*"] even
	// if other project rows are present.
	repo, mock, done := newIdentityMock(t)
	defer done()
	base := sqlmock.NewRows([]string{"id", "display_name", "disabled", "created_at", "role", "project_id"}).
		AddRow("u1", "All", false, ts("2026-06-10T10:00:00Z"), "user", "janka").
		AddRow("u1", "All", false, ts("2026-06-10T10:00:00Z"), "user", "*")
	mock.ExpectQuery(regexp.QuoteMeta(listUsersBaseSQL)).WillReturnRows(base)
	mock.ExpectQuery(regexp.QuoteMeta(listUsersIdentSQL)).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "channel", "external_id", "display"}))
	mock.ExpectQuery(regexp.QuoteMeta(listUsersSessSQL)).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "count"}))
	got, err := repo.ListUsers(context.Background())
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(got) != 1 || len(got[0].Projects) != 1 || got[0].Projects[0] != "*" {
		t.Fatalf("projects = %v, want [*]", got[0].Projects)
	}
}

func TestIdentityRepository_SetUserAccess_CreatesBackingGroup(t *testing.T) {
	// Approve an awaiting user as role "user" scoped to one project:
	// group absent → INSERT group, replace projects, add member, revoke
	// the user's stale (awaiting) sessions — all in one transaction.
	repo, mock, done := newIdentityMock(t)
	defer done()
	mock.ExpectBegin()
	// role=user → last-admin guard: lock + pre-check (target not admin → passes).
	mock.ExpectQuery(regexp.QuoteMeta(adminLockSQL)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("user_admin"))
	mock.ExpectQuery(regexp.QuoteMeta(adminGuardSQL)).
		WithArgs("user_await").
		WillReturnRows(sqlmock.NewRows([]string{"a", "b"}).AddRow(false, true))
	mock.ExpectQuery(regexp.QuoteMeta(selectBackingGroupSQL)).
		WithArgs("auto-user-user_await").
		WillReturnError(sqlNoRows()) // group absent
	mock.ExpectExec(regexp.QuoteMeta(insertGroupSQL)).
		WithArgs(sqlmock.AnyArg(), "auto-user-user_await", "user", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(deleteProjectsSQL)).
		WithArgs(sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(insertProjectSQL)).
		WithArgs(sqlmock.AnyArg(), "janka").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(insertMemberSQL)).
		WithArgs(sqlmock.AnyArg(), "user_await").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(revokeUserSessSQL)).
		WithArgs("user_await").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := repo.SetUserAccess(context.Background(), "user_await", "user", []string{"janka"}); err != nil {
		t.Fatalf("SetUserAccess: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestIdentityRepository_SetUserAccess_PromotesExistingToAdmin(t *testing.T) {
	// Existing backing group (role user) re-granted as admin: UPDATE the
	// role, clear projects (admin is instance-wide → no project inserts),
	// ensure membership, revoke sessions.
	repo, mock, done := newIdentityMock(t)
	defer done()
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(selectBackingGroupSQL)).
		WithArgs("auto-user-u1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "role"}).AddRow("grp_existing", "user"))
	mock.ExpectExec(regexp.QuoteMeta(updateGroupRoleSQL)).
		WithArgs("grp_existing", "admin").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(deleteProjectsSQL)).
		WithArgs("grp_existing").WillReturnResult(sqlmock.NewResult(0, 1))
	// No insertProject — admin role.
	mock.ExpectExec(regexp.QuoteMeta(insertMemberSQL)).
		WithArgs("grp_existing", "u1").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(revokeUserSessSQL)).
		WithArgs("u1").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()

	if err := repo.SetUserAccess(context.Background(), "u1", "admin", []string{"ignored"}); err != nil {
		t.Fatalf("SetUserAccess: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestIdentityRepository_SetUserAccess_UserRoleNoProjects(t *testing.T) {
	// role "user" with zero projects is rejected before any DB work.
	repo, mock, done := newIdentityMock(t)
	defer done()
	err := repo.SetUserAccess(context.Background(), "u1", "user", nil)
	if err != persistence.ErrNoProjects {
		t.Fatalf("err = %v, want ErrNoProjects", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err) // no statements expected
	}
}

func TestIdentityRepository_SetUserAccess_BadRole(t *testing.T) {
	repo, _, done := newIdentityMock(t)
	defer done()
	if err := repo.SetUserAccess(context.Background(), "u1", "superuser", []string{"janka"}); err == nil {
		t.Fatal("expected error for invalid role, got nil")
	}
}

func TestIdentityRepository_RemoveUserAccess_DropsMembership(t *testing.T) {
	// Backing group exists → delete the membership and revoke sessions.
	repo, mock, done := newIdentityMock(t)
	defer done()
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(adminLockSQL)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("user_admin"))
	mock.ExpectQuery(regexp.QuoteMeta(adminGuardSQL)).
		WithArgs("u1").
		WillReturnRows(sqlmock.NewRows([]string{"a", "b"}).AddRow(false, true))
	mock.ExpectQuery(regexp.QuoteMeta(selectBackingGroupIDSQL)).
		WithArgs("auto-user-u1").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("grp_existing"))
	mock.ExpectExec(regexp.QuoteMeta(deleteMemberSQL)).
		WithArgs("grp_existing", "u1").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(revokeUserSessSQL)).
		WithArgs("u1").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if err := repo.RemoveUserAccess(context.Background(), "u1"); err != nil {
		t.Fatalf("RemoveUserAccess: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestIdentityRepository_SetUserDisabled_LastAdminGuard(t *testing.T) {
	// Disabling the only enabled admin trips the in-transaction guard:
	// target is admin and no other admin exists → ErrLastAdmin, rollback
	// before the UPDATE ever runs.
	repo, mock, done := newIdentityMock(t)
	defer done()
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(adminLockSQL)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("user_admin"))
	mock.ExpectQuery(regexp.QuoteMeta(adminGuardSQL)).
		WithArgs("user_admin").
		WillReturnRows(sqlmock.NewRows([]string{"a", "b"}).AddRow(true, false)) // is admin, no others
	mock.ExpectRollback()
	err := repo.SetUserDisabled(context.Background(), "user_admin", true)
	if err != persistence.ErrLastAdmin {
		t.Fatalf("err = %v, want ErrLastAdmin", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestIdentityRepository_RemoveUserAccess_NoBackingGroupIsNoop(t *testing.T) {
	// No backing group → no-op (no delete, no revoke, no error).
	repo, mock, done := newIdentityMock(t)
	defer done()
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(adminLockSQL)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("user_admin"))
	mock.ExpectQuery(regexp.QuoteMeta(adminGuardSQL)).
		WithArgs("u1").
		WillReturnRows(sqlmock.NewRows([]string{"a", "b"}).AddRow(false, true))
	mock.ExpectQuery(regexp.QuoteMeta(selectBackingGroupIDSQL)).
		WithArgs("auto-user-u1").
		WillReturnError(sqlNoRows())
	mock.ExpectCommit()
	if err := repo.RemoveUserAccess(context.Background(), "u1"); err != nil {
		t.Fatalf("RemoveUserAccess: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestIdentityRepository_SetGroupRole(t *testing.T) {
	repo, mock, done := newIdentityMock(t)
	defer done()
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE groups SET role = $2 WHERE id = $1`)).
		WithArgs("grp_1", "admin").WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.SetGroupRole(context.Background(), "grp_1", "admin"); err != nil {
		t.Fatalf("SetGroupRole: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestIdentityRepository_RemoveGroupMember(t *testing.T) {
	repo, mock, done := newIdentityMock(t)
	defer done()
	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM group_members WHERE group_id = $1 AND user_id = $2`)).
		WithArgs("grp_1", "user_1").WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.RemoveGroupMember(context.Background(), "grp_1", "user_1"); err != nil {
		t.Fatalf("RemoveGroupMember: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}
