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

func newIdentityMock(t *testing.T) (*IdentityRepository, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	return NewIdentityRepository(db), mock, func() { _ = db.Close() }
}

func TestIdentityRepository_CreateUser(t *testing.T) {
	repo, mock, done := newIdentityMock(t)
	defer done()
	mock.ExpectExec(regexp.QuoteMeta(
		`INSERT INTO users (id, display_name, created_at) VALUES ($1, $2, $3)`)).
		WithArgs("user_1", "Vadim", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	err := repo.CreateUser(context.Background(), &persistence.User{
		ID: "user_1", DisplayName: "Vadim", CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestIdentityRepository_SetUserDisabled_NotFound(t *testing.T) {
	// disabled=true runs inside a transaction (A2); a missing user row
	// errors and rolls back before any session revocation.
	repo, mock, done := newIdentityMock(t)
	defer done()
	mock.ExpectBegin()
	// Last-admin guard locks the enabled-admin set, then pre-checks.
	mock.ExpectQuery(regexp.QuoteMeta(adminLockSQL)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectQuery(regexp.QuoteMeta(adminGuardSQL)).
		WithArgs("user_missing").
		WillReturnRows(sqlmock.NewRows([]string{"a", "b"}).AddRow(false, true))
	mock.ExpectExec(regexp.QuoteMeta(
		`UPDATE users SET disabled_at = CASE WHEN $2 THEN NOW() ELSE NULL END WHERE id = $1`)).
		WithArgs("user_missing", true).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()
	err := repo.SetUserDisabled(context.Background(), "user_missing", true)
	if err != persistence.ErrUserNotFound {
		t.Fatalf("err = %v, want ErrUserNotFound", err)
	}
}

func TestIdentityRepository_BindIdentity_UpsertShape(t *testing.T) {
	// Pins the rebind-after-revoke upsert: ON CONFLICT repoints
	// user_id and clears revoked_at (design §3.1 notes).
	repo, mock, done := newIdentityMock(t)
	defer done()
	mock.ExpectExec(regexp.QuoteMeta(
		`INSERT INTO user_identities (id, user_id, channel, external_id, display, created_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (channel, external_id)
DO UPDATE SET user_id = EXCLUDED.user_id, display = EXCLUDED.display, revoked_at = NULL
WHERE user_identities.revoked_at IS NOT NULL`)).
		WithArgs("uident_1", "user_1", "google", "vadim@vornik.io", "Vadim", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	err := repo.BindIdentity(context.Background(), &persistence.UserIdentity{
		ID: "uident_1", UserID: "user_1", Channel: "google",
		ExternalID: "vadim@vornik.io", Display: "Vadim", CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("BindIdentity: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// migrateRepointSQL / migrateRevokeSQL pin the two statements
// MigrateIdentityExternalID issues. Kept as constants so every test
// case matches the production SQL verbatim.
const migrateRepointSQL = `UPDATE user_identities SET external_id = $3
WHERE channel = $1 AND external_id = $2 AND revoked_at IS NULL
  AND NOT EXISTS (
    SELECT 1 FROM user_identities existing
    WHERE existing.channel = $1 AND existing.external_id = $3
  )`

const migrateRevokeSQL = `UPDATE user_identities SET revoked_at = NOW()
WHERE channel = $1 AND external_id = $2 AND revoked_at IS NULL`

func TestIdentityRepository_MigrateIdentityExternalID_Repoints(t *testing.T) {
	// Regression: review of a799e3f2 (2026-06-07) — login→numeric-ID
	// switch orphaned existing login-keyed identities. When the numeric
	// key is free, the legacy active binding is repointed (1 row), and
	// the revoke fallback is NOT issued.
	repo, mock, done := newIdentityMock(t)
	defer done()
	mock.ExpectExec(regexp.QuoteMeta(migrateRepointSQL)).
		WithArgs("github", "vgrinco", "5553528").
		WillReturnResult(sqlmock.NewResult(0, 1))
	err := repo.MigrateIdentityExternalID(context.Background(), "github", "vgrinco", "5553528")
	if err != nil {
		t.Fatalf("MigrateIdentityExternalID: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestIdentityRepository_MigrateIdentityExternalID_TargetTakenRevokesLegacy(t *testing.T) {
	// Regression: review of a799e3f2 (2026-06-07). When a row already
	// holds the numeric key, the guarded repoint affects 0 rows; the
	// legacy row is then revoked so it cannot shadow the canonical one.
	repo, mock, done := newIdentityMock(t)
	defer done()
	mock.ExpectExec(regexp.QuoteMeta(migrateRepointSQL)).
		WithArgs("github", "vgrinco", "5553528").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(migrateRevokeSQL)).
		WithArgs("github", "vgrinco").
		WillReturnResult(sqlmock.NewResult(0, 1))
	err := repo.MigrateIdentityExternalID(context.Background(), "github", "vgrinco", "5553528")
	if err != nil {
		t.Fatalf("MigrateIdentityExternalID: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestIdentityRepository_MigrateIdentityExternalID_NoLegacyRowIsNoop(t *testing.T) {
	// No legacy active row: repoint affects 0 rows, revoke affects 0
	// rows, and the call is a harmless no-op (NOT an error).
	repo, mock, done := newIdentityMock(t)
	defer done()
	mock.ExpectExec(regexp.QuoteMeta(migrateRepointSQL)).
		WithArgs("github", "absent", "5553528").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(migrateRevokeSQL)).
		WithArgs("github", "absent").
		WillReturnResult(sqlmock.NewResult(0, 0))
	err := repo.MigrateIdentityExternalID(context.Background(), "github", "absent", "5553528")
	if err != nil {
		t.Fatalf("MigrateIdentityExternalID no-op: %v (want nil)", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestIdentityRepository_MigrateIdentityExternalID_RepointExecError(t *testing.T) {
	repo, mock, done := newIdentityMock(t)
	defer done()
	mock.ExpectExec(regexp.QuoteMeta(migrateRepointSQL)).
		WithArgs("github", "vgrinco", "5553528").
		WillReturnError(sql.ErrConnDone)
	err := repo.MigrateIdentityExternalID(context.Background(), "github", "vgrinco", "5553528")
	if err == nil {
		t.Fatal("want error from repoint UPDATE, got nil")
	}
}

func TestIdentityRepository_MigrateIdentityExternalID_RevokeExecError(t *testing.T) {
	repo, mock, done := newIdentityMock(t)
	defer done()
	mock.ExpectExec(regexp.QuoteMeta(migrateRepointSQL)).
		WithArgs("github", "vgrinco", "5553528").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(migrateRevokeSQL)).
		WithArgs("github", "vgrinco").
		WillReturnError(sql.ErrConnDone)
	err := repo.MigrateIdentityExternalID(context.Background(), "github", "vgrinco", "5553528")
	if err == nil {
		t.Fatal("want error from revoke UPDATE, got nil")
	}
}

func TestIdentityRepository_RevokeIdentity_NotFound(t *testing.T) {
	// Hardening 2026-06-15: RevokeIdentity now runs in a tx and the
	// unlink uses UPDATE ... RETURNING user_id. A no-row result means
	// the binding was absent → ErrIdentityNotFound, no session revoke,
	// rollback.
	repo, mock, done := newIdentityMock(t)
	defer done()
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`UPDATE user_identities SET revoked_at = NOW()`)).
		WithArgs("telegram", "12345").
		WillReturnRows(sqlmock.NewRows([]string{"user_id"}))
	mock.ExpectRollback()
	err := repo.RevokeIdentity(context.Background(), "telegram", "12345")
	if err != persistence.ErrIdentityNotFound {
		t.Fatalf("err = %v, want ErrIdentityNotFound", err)
	}
}

func TestIdentityRepository_SetGroupProjects_ReplacesSet(t *testing.T) {
	// DELETE-then-INSERT inside one transaction; pins all statements.
	repo, mock, done := newIdentityMock(t)
	defer done()
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(
		`DELETE FROM group_projects WHERE group_id = $1`)).
		WithArgs("grp_1").
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(regexp.QuoteMeta(
		`INSERT INTO group_projects (group_id, project_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`)).
		WithArgs("grp_1", "proj-a").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(
		`INSERT INTO group_projects (group_id, project_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`)).
		WithArgs("grp_1", "proj-b").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Hardening 2026-06-15: narrowing a group's project set must revoke
	// its members' active sessions in the same tx so the change is
	// honored immediately, not after the ~60s principal-cache TTL.
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE ui_sessions SET revoked_at = NOW()`)).
		WithArgs("grp_1").
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectCommit()
	err := repo.SetGroupProjects(context.Background(), "grp_1", []string{"proj-a", "proj-b"})
	if err != nil {
		t.Fatalf("SetGroupProjects: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestIdentityRepository_CreateGroup(t *testing.T) {
	repo, mock, done := newIdentityMock(t)
	defer done()
	mock.ExpectExec(regexp.QuoteMeta(
		`INSERT INTO groups (id, name, role, description, created_at) VALUES ($1, $2, $3, $4, $5)`)).
		WithArgs("grp_1", "admins", "admin", "desc", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	err := repo.CreateGroup(context.Background(), &persistence.Group{
		ID: "grp_1", Name: "admins", Role: "admin", Description: "desc", CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestIdentityRepository_GetGroupByName(t *testing.T) {
	repo, mock, done := newIdentityMock(t)
	defer done()
	now := time.Now().UTC()
	rows := sqlmock.NewRows([]string{"id", "name", "role", "description", "created_at"}).
		AddRow("grp_1", "admins", "admin", "some desc", now)
	mock.ExpectQuery(regexp.QuoteMeta(
		`SELECT id, name, role, description, created_at FROM groups WHERE name = $1`)).
		WithArgs("admins").
		WillReturnRows(rows)
	g, err := repo.GetGroupByName(context.Background(), "admins")
	if err != nil {
		t.Fatalf("GetGroupByName: %v", err)
	}
	if g.ID != "grp_1" || g.Name != "admins" || g.Role != "admin" || g.Description != "some desc" {
		t.Errorf("unexpected group fields: %+v", g)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestIdentityRepository_GetGroupByName_NotFound(t *testing.T) {
	repo, mock, done := newIdentityMock(t)
	defer done()
	rows := sqlmock.NewRows([]string{"id", "name", "role", "description", "created_at"})
	mock.ExpectQuery(regexp.QuoteMeta(
		`SELECT id, name, role, description, created_at FROM groups WHERE name = $1`)).
		WithArgs("missing").
		WillReturnRows(rows)
	_, err := repo.GetGroupByName(context.Background(), "missing")
	if err != persistence.ErrGroupNotFound {
		t.Fatalf("err = %v, want ErrGroupNotFound", err)
	}
}

func TestIdentityRepository_AddGroupMember(t *testing.T) {
	repo, mock, done := newIdentityMock(t)
	defer done()
	mock.ExpectExec(regexp.QuoteMeta(
		`INSERT INTO group_members (group_id, user_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`)).
		WithArgs("grp_1", "user_1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	err := repo.AddGroupMember(context.Background(), "grp_1", "user_1")
	if err != nil {
		t.Fatalf("AddGroupMember: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestIdentityRepository_TouchIdentityLastUsed_ZeroRowsOK(t *testing.T) {
	repo, mock, done := newIdentityMock(t)
	defer done()
	mock.ExpectExec(regexp.QuoteMeta(
		`UPDATE user_identities SET last_used_at = NOW() WHERE channel = $1 AND external_id = $2 AND revoked_at IS NULL`)).
		WithArgs("telegram", "99999").
		WillReturnResult(sqlmock.NewResult(0, 0))
	err := repo.TouchIdentityLastUsed(context.Background(), "telegram", "99999")
	if err != nil {
		t.Fatalf("TouchIdentityLastUsed with zero rows: %v (want nil)", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestIdentityRepository_SetUserDisabled_Success(t *testing.T) {
	repo, mock, done := newIdentityMock(t)
	defer done()
	mock.ExpectExec(regexp.QuoteMeta(
		`UPDATE users SET disabled_at = CASE WHEN $2 THEN NOW() ELSE NULL END WHERE id = $1`)).
		WithArgs("user_1", false).
		WillReturnResult(sqlmock.NewResult(0, 1))
	err := repo.SetUserDisabled(context.Background(), "user_1", false)
	if err != nil {
		t.Fatalf("SetUserDisabled: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TestIdentityRepository_RevokeIdentity_Success is the hardening
// regression (2026-06-15, auth LLD review batch 2): unlinking an identity
// must revoke the owning user's active sessions in the same transaction,
// so access ends immediately instead of at the ~60s principal-cache TTL.
func TestIdentityRepository_RevokeIdentity_Success(t *testing.T) {
	repo, mock, done := newIdentityMock(t)
	defer done()
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`UPDATE user_identities SET revoked_at = NOW()`)).
		WithArgs("google", "vadim@vornik.io").
		WillReturnRows(sqlmock.NewRows([]string{"user_id"}).AddRow("user_42"))
	mock.ExpectExec(regexp.QuoteMeta(
		`UPDATE ui_sessions SET revoked_at = NOW() WHERE user_id = $1 AND revoked_at IS NULL`)).
		WithArgs("user_42").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	err := repo.RevokeIdentity(context.Background(), "google", "vadim@vornik.io")
	if err != nil {
		t.Fatalf("RevokeIdentity: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestIdentityRepository_SetUserDisabled_ExecError(t *testing.T) {
	// disabled=true runs inside a transaction (A2: it also revokes
	// sessions). A failure on the users UPDATE rolls back.
	repo, mock, done := newIdentityMock(t)
	defer done()
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(
		`UPDATE users SET disabled_at = CASE WHEN $2 THEN NOW() ELSE NULL END WHERE id = $1`)).
		WithArgs("user_1", true).
		WillReturnError(sql.ErrConnDone)
	mock.ExpectRollback()
	err := repo.SetUserDisabled(context.Background(), "user_1", true)
	if err == nil {
		t.Fatal("want error, got nil")
	}
}

// TestIdentityRepository_SetUserDisabled_RevokesSessions is the A2
// regression (https://docs.vornik.io): disabling a
// user MUST revoke their active ui_sessions in the same transaction so
// the disable is honored immediately, not after the principal-cache TTL.
func TestIdentityRepository_SetUserDisabled_RevokesSessions(t *testing.T) {
	repo, mock, done := newIdentityMock(t)
	defer done()
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(adminLockSQL)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("user_admin"))
	mock.ExpectQuery(regexp.QuoteMeta(adminGuardSQL)).
		WithArgs("user_1").
		WillReturnRows(sqlmock.NewRows([]string{"a", "b"}).AddRow(false, true)) // target not admin → passes
	mock.ExpectExec(regexp.QuoteMeta(
		`UPDATE users SET disabled_at = CASE WHEN $2 THEN NOW() ELSE NULL END WHERE id = $1`)).
		WithArgs("user_1", true).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(
		`UPDATE ui_sessions SET revoked_at = NOW() WHERE user_id = $1 AND revoked_at IS NULL`)).
		WithArgs("user_1").
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectCommit()
	if err := repo.SetUserDisabled(context.Background(), "user_1", true); err != nil {
		t.Fatalf("SetUserDisabled(true): %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TestIdentityRepository_RevokeSessionsForUser_ZeroOK: revoking with no
// active sessions is a no-op, not an error.
func TestIdentityRepository_RevokeSessionsForUser_ZeroOK(t *testing.T) {
	repo, mock, done := newIdentityMock(t)
	defer done()
	mock.ExpectExec(regexp.QuoteMeta(
		`UPDATE ui_sessions SET revoked_at = NOW() WHERE user_id = $1 AND revoked_at IS NULL`)).
		WithArgs("user_1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := repo.RevokeSessionsForUser(context.Background(), "user_1"); err != nil {
		t.Fatalf("RevokeSessionsForUser: %v (want nil)", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestIdentityRepository_RevokeIdentity_ExecError(t *testing.T) {
	repo, mock, done := newIdentityMock(t)
	defer done()
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`UPDATE user_identities SET revoked_at = NOW()`)).
		WithArgs("telegram", "12345").
		WillReturnError(sql.ErrConnDone)
	mock.ExpectRollback()
	err := repo.RevokeIdentity(context.Background(), "telegram", "12345")
	if err == nil {
		t.Fatal("want error, got nil")
	}
}

func TestIdentityRepository_GetGroupByName_ScanError(t *testing.T) {
	// Non-ErrNoRows scan error propagates as-is (via mapDBError).
	repo, mock, done := newIdentityMock(t)
	defer done()
	mock.ExpectQuery(regexp.QuoteMeta(
		`SELECT id, name, role, description, created_at FROM groups WHERE name = $1`)).
		WithArgs("bad").
		WillReturnError(sql.ErrConnDone)
	_, err := repo.GetGroupByName(context.Background(), "bad")
	if err == nil {
		t.Fatal("want error, got nil")
	}
}

func TestIdentityRepository_SetGroupProjects_DeleteError(t *testing.T) {
	repo, mock, done := newIdentityMock(t)
	defer done()
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(
		`DELETE FROM group_projects WHERE group_id = $1`)).
		WithArgs("grp_1").
		WillReturnError(sql.ErrConnDone)
	mock.ExpectRollback()
	err := repo.SetGroupProjects(context.Background(), "grp_1", []string{"proj-a"})
	if err == nil {
		t.Fatal("want error from DELETE, got nil")
	}
}

func TestIdentityRepository_SetGroupProjects_InsertError(t *testing.T) {
	repo, mock, done := newIdentityMock(t)
	defer done()
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(
		`DELETE FROM group_projects WHERE group_id = $1`)).
		WithArgs("grp_1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(
		`INSERT INTO group_projects (group_id, project_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`)).
		WithArgs("grp_1", "proj-a").
		WillReturnError(sql.ErrConnDone)
	mock.ExpectRollback()
	err := repo.SetGroupProjects(context.Background(), "grp_1", []string{"proj-a"})
	if err == nil {
		t.Fatal("want error from INSERT, got nil")
	}
}

func TestIdentityRepository_ResolvePrincipalRows(t *testing.T) {
	repo, mock, done := newIdentityMock(t)
	defer done()
	// Nullable columns: plain values / nil (sqlmock rejects Go
	// pointers as driver values); database/sql scans NULL into the
	// repo's *string fields.
	rows := sqlmock.NewRows([]string{"user_id", "display_name", "disabled", "role", "project_id"}).
		AddRow("user_1", "Vadim", false, "admin", nil).
		AddRow("user_1", "Vadim", false, "user", "proj-a")
	mock.ExpectQuery(regexp.QuoteMeta(
		`SELECT u.id, u.display_name, (u.disabled_at IS NOT NULL) AS disabled, g.role, gp.project_id
FROM user_identities ui
JOIN users u ON u.id = ui.user_id
LEFT JOIN group_members gm ON gm.user_id = u.id
LEFT JOIN groups g ON g.id = gm.group_id
LEFT JOIN group_projects gp ON gp.group_id = g.id
WHERE ui.channel = $1 AND ui.external_id = $2 AND ui.revoked_at IS NULL`)).
		WithArgs("google", "vadim@vornik.io").
		WillReturnRows(rows)

	got, err := repo.ResolvePrincipalRows(context.Background(), "google", "vadim@vornik.io")
	if err != nil {
		t.Fatalf("ResolvePrincipalRows: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("rows = %d, want 2", len(got))
	}
	if got[0].Role == nil || *got[0].Role != "admin" || got[0].ProjectID != nil {
		t.Errorf("row 0 = %+v, want admin/nil-project", got[0])
	}
	if got[1].ProjectID == nil || *got[1].ProjectID != "proj-a" {
		t.Errorf("row 1 = %+v, want proj-a", got[1])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestIdentityRepository_ResolvePrincipalRows_Empty(t *testing.T) {
	repo, mock, done := newIdentityMock(t)
	defer done()
	mock.ExpectQuery("SELECT u.id").
		WithArgs("telegram", "999").
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "display_name", "disabled", "role", "project_id"}))
	got, err := repo.ResolvePrincipalRows(context.Background(), "telegram", "999")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("rows = %d, want 0 (unknown identity)", len(got))
	}
}

func TestIdentityRepository_ResolvePrincipalRows_QueryError(t *testing.T) {
	// Query-level transport error propagates; rows.Next is never called.
	repo, mock, done := newIdentityMock(t)
	defer done()
	mock.ExpectQuery("SELECT u.id").
		WithArgs("google", "vadim@vornik.io").
		WillReturnError(sql.ErrConnDone)
	_, err := repo.ResolvePrincipalRows(context.Background(), "google", "vadim@vornik.io")
	if err == nil {
		t.Fatal("want error, got nil")
	}
}

func TestIdentityRepository_ResolveUserPrincipalRows(t *testing.T) {
	repo, mock, done := newIdentityMock(t)
	defer done()
	// Mirrors ResolvePrincipalRows happy 2-row, but keyed on u.id —
	// the session path (binding already resolved at login).
	rows := sqlmock.NewRows([]string{"user_id", "display_name", "disabled", "role", "project_id"}).
		AddRow("user_1", "Vadim", false, "admin", nil).
		AddRow("user_1", "Vadim", false, "user", "proj-a")
	mock.ExpectQuery(regexp.QuoteMeta(
		`SELECT u.id, u.display_name, (u.disabled_at IS NOT NULL) AS disabled, g.role, gp.project_id
FROM users u
LEFT JOIN group_members gm ON gm.user_id = u.id
LEFT JOIN groups g ON g.id = gm.group_id
LEFT JOIN group_projects gp ON gp.group_id = g.id
WHERE u.id = $1`)).
		WithArgs("user_1").
		WillReturnRows(rows)

	got, err := repo.ResolveUserPrincipalRows(context.Background(), "user_1")
	if err != nil {
		t.Fatalf("ResolveUserPrincipalRows: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("rows = %d, want 2", len(got))
	}
	if got[0].Role == nil || *got[0].Role != "admin" || got[0].ProjectID != nil {
		t.Errorf("row 0 = %+v, want admin/nil-project", got[0])
	}
	if got[1].ProjectID == nil || *got[1].ProjectID != "proj-a" {
		t.Errorf("row 1 = %+v, want proj-a", got[1])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestIdentityRepository_ResolveUserPrincipalRows_Empty(t *testing.T) {
	// Zero rows = the user row no longer exists (session path).
	repo, mock, done := newIdentityMock(t)
	defer done()
	mock.ExpectQuery("SELECT u.id").
		WithArgs("user_gone").
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "display_name", "disabled", "role", "project_id"}))
	got, err := repo.ResolveUserPrincipalRows(context.Background(), "user_gone")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("rows = %d, want 0 (user gone)", len(got))
	}
}

func TestIdentityRepository_ResolveUserPrincipalRows_QueryError(t *testing.T) {
	repo, mock, done := newIdentityMock(t)
	defer done()
	mock.ExpectQuery("SELECT u.id").
		WithArgs("user_1").
		WillReturnError(sql.ErrConnDone)
	_, err := repo.ResolveUserPrincipalRows(context.Background(), "user_1")
	if err == nil {
		t.Fatal("want error, got nil")
	}
}

func TestIdentityRepository_ResolveUserPrincipalRows_RowError(t *testing.T) {
	// rows.Err() surfacing mid-iteration must propagate through the
	// shared scanPrincipalRows helper (covers the rows.Err branch).
	repo, mock, done := newIdentityMock(t)
	defer done()
	rows := sqlmock.NewRows([]string{"user_id", "display_name", "disabled", "role", "project_id"}).
		AddRow("user_1", "V", false, "user", "proj-a").
		RowError(0, sql.ErrConnDone)
	mock.ExpectQuery("SELECT u.id").
		WithArgs("user_1").
		WillReturnRows(rows)
	_, err := repo.ResolveUserPrincipalRows(context.Background(), "user_1")
	if err == nil {
		t.Fatal("want error from rows.Err(), got nil")
	}
}

func TestIdentityRepository_ResolveUserPrincipalRows_ScanError(t *testing.T) {
	// A driver value the scanner can't coerce into the PrincipalRow
	// fields surfaces via Scan → mapDBError (covers the Scan branch).
	repo, mock, done := newIdentityMock(t)
	defer done()
	rows := sqlmock.NewRows([]string{"user_id", "display_name", "disabled", "role", "project_id"}).
		// disabled column gets a non-bool string the bool scan rejects.
		AddRow("user_1", "V", "not-a-bool", "user", "proj-a")
	mock.ExpectQuery("SELECT u.id").
		WithArgs("user_1").
		WillReturnRows(rows)
	_, err := repo.ResolveUserPrincipalRows(context.Background(), "user_1")
	if err == nil {
		t.Fatal("want scan error, got nil")
	}
}

func TestIdentityRepository_ResolvePrincipalRows_ZeroGroups(t *testing.T) {
	// A user with no group memberships yields one row with nil Role
	// and nil ProjectID (LEFT JOINs produce a single NULL-padded row).
	repo, mock, done := newIdentityMock(t)
	defer done()
	rows := sqlmock.NewRows([]string{"user_id", "display_name", "disabled", "role", "project_id"}).
		AddRow("user_1", "V", false, nil, nil)
	mock.ExpectQuery(regexp.QuoteMeta(
		`SELECT u.id, u.display_name, (u.disabled_at IS NOT NULL) AS disabled, g.role, gp.project_id
FROM user_identities ui
JOIN users u ON u.id = ui.user_id
LEFT JOIN group_members gm ON gm.user_id = u.id
LEFT JOIN groups g ON g.id = gm.group_id
LEFT JOIN group_projects gp ON gp.group_id = g.id
WHERE ui.channel = $1 AND ui.external_id = $2 AND ui.revoked_at IS NULL`)).
		WithArgs("google", "vadim@vornik.io").
		WillReturnRows(rows)
	got, err := repo.ResolvePrincipalRows(context.Background(), "google", "vadim@vornik.io")
	if err != nil {
		t.Fatalf("ResolvePrincipalRows: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("rows = %d, want 1", len(got))
	}
	if got[0].Role != nil || got[0].ProjectID != nil {
		t.Errorf("row 0 = %+v, want nil Role and nil ProjectID", got[0])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}
