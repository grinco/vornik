package service

import (
	"context"
	"database/sql"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/postgres"
)

// newMockDB builds a test *postgres.APIKeyRepository backed by sqlmock.
func newMockAPIKeyRepo(t *testing.T) (*postgres.APIKeyRepository, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	repo := postgres.NewAPIKeyRepository(db)
	return repo, mock, func() { _ = db.Close() }
}

// TestTaskKeyMinter_MintTaskKey_CreatesKeyAndReturnsRaw — MintTaskKey
// must call repo.Create with the correct fields and return the raw key
// (never the hash). Per-task keys must have an empty ClientKind so
// they don't get routed into the companion path by the auth middleware.
func TestTaskKeyMinter_MintTaskKey_CreatesKeyAndReturnsRaw(t *testing.T) {
	repo, mock, cleanup := newMockAPIKeyRepo(t)
	defer cleanup()

	m := &taskKeyMinter{repo: repo}

	// Expect an INSERT on api_keys. We pin the name and key prefix
	// shape; the raw key is minted internally so we match on AnyArg
	// for the hash and prefix values.
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO api_keys")).
		WithArgs(
			sqlmock.AnyArg(),      // id
			"proj-1",              // project_id
			"agent:task_task-abc", // name — the binding the revoke path relies on
			sqlmock.AnyArg(),      // key_hash
			sqlmock.AnyArg(),      // key_prefix
			sqlmock.AnyArg(),      // created_at
			sqlmock.AnyArg(),      // expires_at (now+48h)
			// created_by as NullString
			sqlmock.AnyArg(),
			// rate limit cols (NULL)
			sqlmock.AnyArg(), sqlmock.AnyArg(),
			// companion-scope cols (NULL) — client_kind MUST be empty
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			// memory caps + allow_push (false, false, false), default_repo_scope (NULL)
			false, false, false, sql.NullString{},
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	raw, err := m.MintTaskKey(context.Background(), "proj-1", "task-abc")
	if err != nil {
		t.Fatalf("MintTaskKey: %v", err)
	}
	if raw == "" {
		t.Fatal("expected non-empty raw key")
	}
	// Raw key must have the sk-vornik prefix.
	if len(raw) < 10 || raw[:9] != "sk-vornik" {
		t.Errorf("raw key shape unexpected: %q", raw)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestTaskKeyMinter_MintTaskKey_KeyExpiry — the inserted key must
// have expires_at set approximately 48h in the future. This is the
// belt-and-braces mechanism: if revoke-at-teardown is somehow skipped,
// the key expires within 48h on its own.
func TestTaskKeyMinter_MintTaskKey_KeyExpiry(t *testing.T) {
	before := time.Now().UTC()

	// Use an in-memory stub instead of sqlmock so we can inspect the
	// inserted row directly.
	stub := &inMemAPIKeyRepo{}
	m := &taskKeyMinter{repo: stub}

	raw, err := m.MintTaskKey(context.Background(), "proj-2", "task-def")
	if err != nil {
		t.Fatalf("MintTaskKey: %v", err)
	}
	if raw == "" {
		t.Fatal("expected non-empty raw key")
	}
	if len(stub.rows) != 1 {
		t.Fatalf("expected 1 inserted row, got %d", len(stub.rows))
	}
	row := stub.rows[0]
	if row.ExpiresAt == nil {
		t.Fatal("expires_at must be set")
	}
	// expires_at must be ~48h from now.
	exp := *row.ExpiresAt
	minExp := before.Add(47 * time.Hour)
	maxExp := time.Now().UTC().Add(49 * time.Hour)
	if exp.Before(minExp) || exp.After(maxExp) {
		t.Errorf("expires_at = %v, want ~48h from now (window: %v – %v)", exp, minExp, maxExp)
	}
	// ClientKind must be empty — non-empty triggers companion path confinement.
	if row.ClientKind != "" {
		t.Errorf("ClientKind = %q, want empty (empty avoids companion middleware path)", row.ClientKind)
	}
	// Name must encode the taskID.
	if row.Name != "agent:task_task-def" {
		t.Errorf("Name = %q, want agent:task_task-def", row.Name)
	}
	// CreatedBy must identify the executor subsystem.
	if row.CreatedBy != "executor" {
		t.Errorf("CreatedBy = %q, want executor", row.CreatedBy)
	}
}

// TestTaskKeyMinter_MintProjectScopedKey_IsProjectScopedNotTaskScoped —
// Finding B1(b). Warm-pool keys must be PROJECT-scoped: the persisted
// key carries a name that does NOT match persistence.TaskIDFromKeyName,
// so the audit handlers / CallMCPTool never treat a warm key as bound to
// a bogus task. The raw key still embeds the project (apikey shape) so
// requestAllowsProject accepts it for the pool's project only.
func TestTaskKeyMinter_MintProjectScopedKey_IsProjectScopedNotTaskScoped(t *testing.T) {
	stub := &inMemAPIKeyRepo{}
	m := &taskKeyMinter{repo: stub}

	raw, err := m.MintProjectScopedKey(context.Background(), "proj-warm", "coder")
	if err != nil {
		t.Fatalf("MintProjectScopedKey: %v", err)
	}
	if raw == "" || raw[:9] != "sk-vornik" {
		t.Fatalf("raw key shape unexpected: %q", raw)
	}
	pid, _, perr := apikey.Parse(raw)
	if perr != nil || !apikey.MatchesProject(pid, "proj-warm") {
		t.Fatalf("raw key must resolve to the pool project; got segment=%q err=%v", pid, perr)
	}
	if len(stub.rows) != 1 {
		t.Fatalf("expected 1 inserted row, got %d", len(stub.rows))
	}
	row := stub.rows[0]
	// MUST NOT be task-scoped — otherwise audit/MCP paths would bind it
	// to a nonexistent task and reject every warm call.
	if _, isTask := persistence.TaskIDFromKeyName(row.Name); isTask {
		t.Errorf("warm key name %q must NOT match TaskIDFromKeyName (project-scoped, not task-scoped)", row.Name)
	}
	if row.ClientKind != "" {
		t.Errorf("ClientKind = %q, want empty (avoids companion middleware path)", row.ClientKind)
	}
	if row.ExpiresAt == nil {
		t.Error("expires_at must be set")
	}
}

// TestTaskKeyMinter_RevokeTaskKey_DelegatesToRevokeByName — RevokeTaskKey
// must call RevokeByName("agent:task_<taskID>"). Idempotent: a key
// that was already revoked or never created returns nil.
func TestTaskKeyMinter_RevokeTaskKey_DelegatesToRevokeByName(t *testing.T) {
	stub := &inMemAPIKeyRepo{}
	// Seed a key with the expected name.
	stub.rows = append(stub.rows, &persistence.APIKey{
		ID: "akey-1", ProjectID: "proj-3", Name: "agent:task_task-ghi",
	})
	m := &taskKeyMinter{repo: stub}

	if err := m.RevokeTaskKey(context.Background(), "task-ghi"); err != nil {
		t.Fatalf("RevokeTaskKey: %v", err)
	}
	if stub.rows[0].RevokedAt == nil {
		t.Error("expected row to be revoked after RevokeTaskKey")
	}

	// Second call — already revoked, must be idempotent.
	if err := m.RevokeTaskKey(context.Background(), "task-ghi"); err != nil {
		t.Fatalf("RevokeTaskKey idempotent: %v", err)
	}
}

// --- in-memory stub for taskKeyMinter tests --------------------------

// inMemAPIKeyRepo is a minimal persistence.APIKeyRepository stub that
// stores inserted rows so MintTaskKey can be verified without sqlmock.
// Only Create and RevokeByName need to be correct for these tests;
// the other methods panic to catch unexpected calls.
type inMemAPIKeyRepo struct {
	rows []*persistence.APIKey
}

func (r *inMemAPIKeyRepo) Create(_ context.Context, k *persistence.APIKey) error {
	cp := *k
	r.rows = append(r.rows, &cp)
	return nil
}
func (r *inMemAPIKeyRepo) RevokeByName(_ context.Context, name string) error {
	now := time.Now().UTC()
	for _, row := range r.rows {
		if row.Name == name && row.RevokedAt == nil {
			row.RevokedAt = &now
		}
	}
	return nil
}
func (r *inMemAPIKeyRepo) LookupActiveByHash(context.Context, string) (*persistence.APIKey, error) {
	panic("unexpected LookupActiveByHash")
}
func (r *inMemAPIKeyRepo) ListByProject(context.Context, string) ([]*persistence.APIKey, error) {
	panic("unexpected ListByProject")
}
func (r *inMemAPIKeyRepo) ListCompanionByProject(context.Context, string) ([]*persistence.APIKey, error) {
	panic("unexpected ListCompanionByProject")
}
func (r *inMemAPIKeyRepo) TouchLastUsed(context.Context, string) error { return nil }
func (r *inMemAPIKeyRepo) Revoke(context.Context, string) error        { return nil }
func (r *inMemAPIKeyRepo) UpdateAllowedWorkflows(context.Context, string, []string) error {
	return nil
}
func (r *inMemAPIKeyRepo) UpdateAllowPush(context.Context, string, bool) error { return nil }
