package postgres

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"vornik.io/vornik/internal/persistence"
)

// TestAPIKeyRepository_Create — INSERT shape pins the column
// order + arg binding the migration depends on. A future schema
// change that drops a column would have to update both this test
// and the repo, surfacing the breakage.
func TestAPIKeyRepository_Create(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewAPIKeyRepository(db)

	created := time.Date(2026, 5, 16, 14, 0, 0, 0, time.UTC)
	expires := created.Add(30 * 24 * time.Hour)
	key := &persistence.APIKey{
		ID: "akey-1", ProjectID: "assistant", Name: "test",
		KeyHash: "deadbeef", KeyPrefix: "sk-vornik-as",
		CreatedAt: created, ExpiresAt: &expires, CreatedBy: "operator",
	}
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO api_keys")).
		WithArgs(key.ID, key.ProjectID, key.Name, key.KeyHash, key.KeyPrefix,
			key.CreatedAt, key.ExpiresAt, sql.NullString{String: "operator", Valid: true},
			// Rate-limit columns: NULL when unset.
			sql.NullInt64{}, sql.NullInt64{},
			// Companion-scope columns (LLD 21): all NULL for a key
			// minted via the legacy non-companion path.
			sql.NullString{}, sql.NullFloat64{}, sql.NullString{}, sql.NullString{},
			// Companion RAG capabilities (LLD 22): default false.
			// allow_push (LLD slice 2): default false.
			// default_repo_scope (migration 110): NULL when unset.
			false, false, false, sql.NullString{}).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Create(context.Background(), key); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestAPIKeyRepository_Create_PreservesRateLimits — the Postgres INSERT
// must carry rate_limit_rps / rate_limit_burst. A prior version omitted
// these columns, so RotateAPIKey (which copies them from the prior key)
// silently dropped rate limits on the production Postgres backend while
// the in-memory stub used by the handler test masked the regression.
func TestAPIKeyRepository_Create_PreservesRateLimits(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewAPIKeyRepository(db)

	rps, burst := 5, 11
	key := &persistence.APIKey{
		ID: "akey-rl", ProjectID: "assistant", Name: "rl",
		KeyHash: "h", KeyPrefix: "sk-vornik-as",
		CreatedAt:      time.Now(),
		RateLimitRPS:   &rps,
		RateLimitBurst: &burst,
	}
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO api_keys")).
		WithArgs(key.ID, key.ProjectID, key.Name, key.KeyHash, key.KeyPrefix,
			key.CreatedAt, key.ExpiresAt, sql.NullString{},
			sql.NullInt64{Int64: 5, Valid: true}, sql.NullInt64{Int64: 11, Valid: true},
			sql.NullString{}, sql.NullFloat64{}, sql.NullString{}, sql.NullString{},
			false, false, false, sql.NullString{}).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Create(context.Background(), key); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestAPIKeyRepository_Create_NullCreatedBy — empty CreatedBy must
// land as SQL NULL, not as the literal empty string. NULL preserves
// the "field never set" semantic which the UI relies on to render
// the column as blank rather than "<empty>".
func TestAPIKeyRepository_Create_NullCreatedBy(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewAPIKeyRepository(db)

	key := &persistence.APIKey{
		ID: "akey-2", ProjectID: "p", Name: "n",
		KeyHash: "h", KeyPrefix: "sk-vornik-p",
		CreatedAt: time.Now(),
	}
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO api_keys")).
		WithArgs(key.ID, key.ProjectID, key.Name, key.KeyHash, key.KeyPrefix,
			key.CreatedAt, key.ExpiresAt, sql.NullString{},
			sql.NullInt64{}, sql.NullInt64{},
			sql.NullString{}, sql.NullFloat64{}, sql.NullString{}, sql.NullString{},
			false, false, false, sql.NullString{}).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Create(context.Background(), key); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestAPIKeyRepository_LookupActiveByHash_Found — happy path. The
// query must filter on `revoked_at IS NULL AND (expires_at IS NULL
// OR expires_at > NOW())` so a revoked or expired row is invisible
// to the auth layer.
func TestAPIKeyRepository_LookupActiveByHash_Found(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewAPIKeyRepository(db)

	hash := "abc123"
	created := time.Date(2026, 5, 16, 14, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("WHERE key_hash = $1")).
		WithArgs(hash).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "project_id", "name", "key_hash", "key_prefix",
			"created_at", "last_used_at", "expires_at", "revoked_at", "created_by",
			"rate_limit_rps", "rate_limit_burst",
			"allowed_workflows", "budget_cap_usd", "client_kind", "session_label",
			"memory_read", "memory_write", "allow_push", "default_repo_scope",
		}).AddRow("akey-3", "assistant", "ha-key", hash, "sk-vornik-as",
			created, nil, nil, nil, "operator", nil, nil, nil, nil, nil, nil, false, false, false, nil))

	got, err := repo.LookupActiveByHash(context.Background(), hash)
	if err != nil {
		t.Fatalf("LookupActiveByHash: %v", err)
	}
	if got.ID != "akey-3" || got.ProjectID != "assistant" || got.CreatedBy != "operator" {
		t.Errorf("unexpected row: %+v", got)
	}
}

// TestAPIKeyRepository_LookupActiveByHash_NotFound — no row maps to
// the sentinel error AuthMiddleware translates to UNAUTHORIZED.
// Distinct from a generic DB error so the caller can branch on
// "key was wrong" vs "DB is sick".
func TestAPIKeyRepository_LookupActiveByHash_NotFound(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewAPIKeyRepository(db)

	mock.ExpectQuery(regexp.QuoteMeta("WHERE key_hash = $1")).
		WithArgs("missing").
		WillReturnError(sql.ErrNoRows)

	_, err := repo.LookupActiveByHash(context.Background(), "missing")
	if !errors.Is(err, persistence.ErrAPIKeyNotFound) {
		t.Errorf("err = %v, want ErrAPIKeyNotFound", err)
	}
}

// TestAPIKeyRepository_ListByProject — newest-first; revoked rows
// included (the UI renders them dimmed); scan handles NULL
// optional fields without panicking.
func TestAPIKeyRepository_ListByProject(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewAPIKeyRepository(db)

	now := time.Date(2026, 5, 16, 14, 0, 0, 0, time.UTC)
	older := now.Add(-24 * time.Hour)
	revoked := now.Add(-1 * time.Hour)
	mock.ExpectQuery(regexp.QuoteMeta("WHERE project_id = $1")).
		WithArgs("assistant").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "project_id", "name", "key_hash", "key_prefix",
			"created_at", "last_used_at", "expires_at", "revoked_at", "created_by",
			"rate_limit_rps", "rate_limit_burst",
			"allowed_workflows", "budget_cap_usd", "client_kind", "session_label",
			"memory_read", "memory_write", "allow_push", "default_repo_scope",
		}).
			AddRow("akey-new", "assistant", "n1", "h1", "sk-vornik-as", now, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, false, false, false, nil).
			AddRow("akey-old", "assistant", "n2", "h2", "sk-vornik-as", older, nil, nil, &revoked, "ops", nil, nil, nil, nil, nil, nil, false, false, false, nil))

	got, err := repo.ListByProject(context.Background(), "assistant")
	if err != nil {
		t.Fatalf("ListByProject: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].ID != "akey-new" || got[1].ID != "akey-old" {
		t.Errorf("ordering: got [%s, %s]", got[0].ID, got[1].ID)
	}
	if got[1].RevokedAt == nil || *got[1].RevokedAt != revoked {
		t.Errorf("revoked row missing RevokedAt: %+v", got[1])
	}
}

// TestAPIKeyRepository_LookupActiveByHash_DecodesRateLimits —
// when the row has non-NULL rate_limit_rps / rate_limit_burst,
// the repo populates the pointer fields so AuthMiddleware's
// per-key token bucket can read them. NULL columns stay nil.
func TestAPIKeyRepository_LookupActiveByHash_DecodesRateLimits(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewAPIKeyRepository(db)

	hash := "abc"
	created := time.Date(2026, 5, 16, 14, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("WHERE key_hash = $1")).
		WithArgs(hash).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "project_id", "name", "key_hash", "key_prefix",
			"created_at", "last_used_at", "expires_at", "revoked_at", "created_by",
			"rate_limit_rps", "rate_limit_burst",
			"allowed_workflows", "budget_cap_usd", "client_kind", "session_label",
			"memory_read", "memory_write", "allow_push", "default_repo_scope",
		}).AddRow("akey-rate", "assistant", "throttled", hash, "sk-vornik-as",
			created, nil, nil, nil, nil, int64(50), int64(100), nil, nil, nil, nil, false, false, false, nil))

	got, err := repo.LookupActiveByHash(context.Background(), hash)
	if err != nil {
		t.Fatalf("LookupActiveByHash: %v", err)
	}
	if got.RateLimitRPS == nil || *got.RateLimitRPS != 50 {
		t.Errorf("rate_limit_rps = %v, want 50", got.RateLimitRPS)
	}
	if got.RateLimitBurst == nil || *got.RateLimitBurst != 100 {
		t.Errorf("rate_limit_burst = %v, want 100", got.RateLimitBurst)
	}
}

// TestAPIKeyRepository_TouchLastUsed — async write of last_used_at
// must accept any time arg and target the right id; the auth
// hot-path goroutine fires this with `time.Now().UTC()` but the
// test pins only the column shape (we mock with AnyArg).
func TestAPIKeyRepository_TouchLastUsed(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewAPIKeyRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("UPDATE api_keys SET last_used_at")).
		WithArgs(sqlmock.AnyArg(), "akey-7").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.TouchLastUsed(context.Background(), "akey-7"); err != nil {
		t.Fatalf("TouchLastUsed: %v", err)
	}
}

// TestAPIKeyRepository_Create_CompanionScope — companion-scope
// columns round-trip through Create. The companion grant handler
// builds an APIKey with AllowedWorkflows + BudgetCapUSD +
// ClientKind + SessionLabel and expects this layer to land them.
func TestAPIKeyRepository_Create_CompanionScope(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewAPIKeyRepository(db)

	cap := 25.50
	key := &persistence.APIKey{
		ID: "akey-co", ProjectID: "companion-example", Name: "session-1",
		KeyHash: "h", KeyPrefix: "sk-vornik-co",
		CreatedAt:        time.Now(),
		AllowedWorkflows: []string{"companion-architectural-review", "companion-doc-review"},
		BudgetCapUSD:     &cap,
		ClientKind:       "claude-code",
		SessionLabel:     "vadim/laptop",
	}
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO api_keys")).
		WithArgs(key.ID, key.ProjectID, key.Name, key.KeyHash, key.KeyPrefix,
			key.CreatedAt, key.ExpiresAt, sql.NullString{},
			sql.NullInt64{}, sql.NullInt64{},
			// The encoded JSON form is what hits the column.
			sql.NullString{String: `["companion-architectural-review","companion-doc-review"]`, Valid: true},
			sql.NullFloat64{Float64: 25.50, Valid: true},
			sql.NullString{String: "claude-code", Valid: true},
			sql.NullString{String: "vadim/laptop", Valid: true},
			false, false, false, sql.NullString{}).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Create(context.Background(), key); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestAPIKeyRepository_LookupActiveByHash_DecodesCompanionScope —
// the lookup must populate AllowedWorkflows + BudgetCapUSD +
// ClientKind + SessionLabel from the row. The companion MCP server
// reads these on every delegate() call to enforce scope.
func TestAPIKeyRepository_LookupActiveByHash_DecodesCompanionScope(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewAPIKeyRepository(db)

	hash := "co-hash"
	created := time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("WHERE key_hash = $1")).
		WithArgs(hash).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "project_id", "name", "key_hash", "key_prefix",
			"created_at", "last_used_at", "expires_at", "revoked_at", "created_by",
			"rate_limit_rps", "rate_limit_burst",
			"allowed_workflows", "budget_cap_usd", "client_kind", "session_label",
			"memory_read", "memory_write", "allow_push", "default_repo_scope",
		}).AddRow("akey-co", "companion-example", "session-1", hash, "sk-vornik-co",
			created, nil, nil, nil, nil, nil, nil,
			`["companion-architectural-review"]`, 25.50, "claude-code", "vadim/laptop",
			false, false, false, nil))

	got, err := repo.LookupActiveByHash(context.Background(), hash)
	if err != nil {
		t.Fatalf("LookupActiveByHash: %v", err)
	}
	if len(got.AllowedWorkflows) != 1 || got.AllowedWorkflows[0] != "companion-architectural-review" {
		t.Errorf("AllowedWorkflows = %v, want [companion-architectural-review]", got.AllowedWorkflows)
	}
	if got.BudgetCapUSD == nil || *got.BudgetCapUSD != 25.50 {
		t.Errorf("BudgetCapUSD = %v, want 25.50", got.BudgetCapUSD)
	}
	if got.ClientKind != "claude-code" {
		t.Errorf("ClientKind = %q, want claude-code", got.ClientKind)
	}
	if got.SessionLabel != "vadim/laptop" {
		t.Errorf("SessionLabel = %q, want vadim/laptop", got.SessionLabel)
	}
}

// TestAPIKeyRepository_ListCompanionByProject — filters to
// client_kind IS NOT NULL rows; ordering matches ListByProject's
// newest-first contract. Backs the companion admin "list keys"
// endpoint.
func TestAPIKeyRepository_ListCompanionByProject(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewAPIKeyRepository(db)

	now := time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("AND client_kind IS NOT NULL")).
		WithArgs("companion-example").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "project_id", "name", "key_hash", "key_prefix",
			"created_at", "last_used_at", "expires_at", "revoked_at", "created_by",
			"rate_limit_rps", "rate_limit_burst",
			"allowed_workflows", "budget_cap_usd", "client_kind", "session_label",
			"memory_read", "memory_write", "allow_push", "default_repo_scope",
		}).
			AddRow("co-1", "companion-example", "claude-laptop", "h1", "sk-vornik-co",
				now, nil, nil, nil, nil, nil, nil, `[]`, nil, "claude-code", nil, false, false, false, nil).
			AddRow("co-2", "companion-example", "codex-laptop", "h2", "sk-vornik-co",
				now.Add(-time.Hour), nil, nil, nil, nil, nil, nil, nil, nil, "codex", nil, false, false, false, nil))

	got, err := repo.ListCompanionByProject(context.Background(), "companion-example")
	if err != nil {
		t.Fatalf("ListCompanionByProject: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].ClientKind != "claude-code" || got[1].ClientKind != "codex" {
		t.Errorf("client_kinds: got [%s, %s]", got[0].ClientKind, got[1].ClientKind)
	}
}

// TestAPIKeyRepository_MemoryCapabilities — LLD 22 adds two booleans
// to api_keys. They must round-trip through Create + LookupActiveByHash
// so the companion MCP server can gate the recall / remember tools
// on per-key grants. Default false keeps existing companion keys
// from quietly acquiring access after the daemon upgrade.
func TestAPIKeyRepository_MemoryCapabilities(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewAPIKeyRepository(db)

	key := &persistence.APIKey{
		ID: "akey-mem", ProjectID: "companion-example", Name: "rag-key",
		KeyHash: "h", KeyPrefix: "sk-vornik-co",
		CreatedAt:   time.Now(),
		ClientKind:  "claude-code",
		MemoryRead:  true,
		MemoryWrite: true,
	}
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO api_keys")).
		WithArgs(key.ID, key.ProjectID, key.Name, key.KeyHash, key.KeyPrefix,
			key.CreatedAt, key.ExpiresAt, sql.NullString{},
			sql.NullInt64{}, sql.NullInt64{},
			sql.NullString{}, sql.NullFloat64{},
			sql.NullString{String: "claude-code", Valid: true},
			sql.NullString{},
			true, true, false, sql.NullString{}).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.Create(context.Background(), key); err != nil {
		t.Fatalf("Create: %v", err)
	}

	hash := "h"
	mock.ExpectQuery(regexp.QuoteMeta("WHERE key_hash = $1")).
		WithArgs(hash).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "project_id", "name", "key_hash", "key_prefix",
			"created_at", "last_used_at", "expires_at", "revoked_at", "created_by",
			"rate_limit_rps", "rate_limit_burst",
			"allowed_workflows", "budget_cap_usd", "client_kind", "session_label",
			"memory_read", "memory_write", "allow_push", "default_repo_scope",
		}).AddRow("akey-mem", "companion-example", "rag-key", hash, "sk-vornik-co",
			key.CreatedAt, nil, nil, nil, nil, nil, nil,
			nil, nil, "claude-code", nil, true, true, false, nil))

	got, err := repo.LookupActiveByHash(context.Background(), hash)
	if err != nil {
		t.Fatalf("LookupActiveByHash: %v", err)
	}
	if !got.MemoryRead || !got.MemoryWrite {
		t.Errorf("memory caps not decoded: read=%v write=%v", got.MemoryRead, got.MemoryWrite)
	}
}

// TestAPIKeyRepository_Revoke — soft-delete is idempotent: the
// UPDATE statement has `WHERE revoked_at IS NULL` so a second
// revoke for the same id is a 0-row no-op. The test asserts the
// repo doesn't return an error in that case (idempotency is the
// contract the management UI relies on).
func TestAPIKeyRepository_Revoke(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewAPIKeyRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("SET revoked_at = NOW()")).
		WithArgs("akey-9").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Revoke(context.Background(), "akey-9"); err != nil {
		t.Fatalf("Revoke first time: %v", err)
	}

	mock.ExpectExec(regexp.QuoteMeta("SET revoked_at = NOW()")).
		WithArgs("akey-9").
		WillReturnResult(sqlmock.NewResult(0, 0)) // 0 rows affected — already revoked

	if err := repo.Revoke(context.Background(), "akey-9"); err != nil {
		t.Fatalf("Revoke second time (should be idempotent): %v", err)
	}
}

// TestAPIKeyRepository_RevokeByName — per-task agent keys are revoked
// by name ("agent:task_<taskID>") so the executor doesn't need to
// look up the key ID first. Zero rows affected is not an error (the
// key may have already expired or was double-revoked via the 48h belt-
// and-braces expires_at path).
func TestAPIKeyRepository_RevokeByName(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewAPIKeyRepository(db)

	// First call: row exists, revoked successfully.
	mock.ExpectExec(regexp.QuoteMeta("WHERE name = $1 AND revoked_at IS NULL")).
		WithArgs("agent:task_task-abc123").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.RevokeByName(context.Background(), "agent:task_task-abc123"); err != nil {
		t.Fatalf("RevokeByName first time: %v", err)
	}

	// Second call: already revoked (0 rows) — must be idempotent.
	mock.ExpectExec(regexp.QuoteMeta("WHERE name = $1 AND revoked_at IS NULL")).
		WithArgs("agent:task_task-abc123").
		WillReturnResult(sqlmock.NewResult(0, 0))

	if err := repo.RevokeByName(context.Background(), "agent:task_task-abc123"); err != nil {
		t.Fatalf("RevokeByName second time (should be idempotent): %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestAPIKeyRepository_AllowPush_UpdateShape — the UPDATE SQL must target
// exactly allow_push = $2 WHERE id = $1. Mirrors the sqlite round-trip in
// round1_smoke_test.go; uses sqlmock so no live Postgres is needed.
func TestAPIKeyRepository_AllowPush_UpdateShape(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewAPIKeyRepository(db)

	// Successful update.
	mock.ExpectExec(regexp.QuoteMeta("SET allow_push = $2")).
		WithArgs("akey-ap", true).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.UpdateAllowPush(context.Background(), "akey-ap", true); err != nil {
		t.Fatalf("UpdateAllowPush: %v", err)
	}

	// 0 rows → ErrAPIKeyNotFound.
	mock.ExpectExec(regexp.QuoteMeta("SET allow_push = $2")).
		WithArgs("nope", true).
		WillReturnResult(sqlmock.NewResult(0, 0))

	if err := repo.UpdateAllowPush(context.Background(), "nope", true); !errors.Is(err, persistence.ErrAPIKeyNotFound) {
		t.Fatalf("want ErrAPIKeyNotFound, got %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestAPIKeyRepository_AllowPush_EmptyKeyID — the empty-keyID guard returns an
// error before any SQL is executed; no mock expectations are registered.
func TestAPIKeyRepository_AllowPush_EmptyKeyID(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewAPIKeyRepository(db)

	if err := repo.UpdateAllowPush(context.Background(), "", true); err == nil {
		t.Fatal("UpdateAllowPush with empty keyID must return an error")
	}

	// No SQL must have been issued.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected sql call: %v", err)
	}
}

// TestAPIKeyRepository_AllowPush_ExecError — when ExecContext returns an error
// UpdateAllowPush must propagate it (covers the exec-error branch).
func TestAPIKeyRepository_AllowPush_ExecError(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewAPIKeyRepository(db)

	execErr := errors.New("connection reset")
	mock.ExpectExec(regexp.QuoteMeta("SET allow_push = $2")).
		WithArgs("akey-exec-err", true).
		WillReturnError(execErr)

	err := repo.UpdateAllowPush(context.Background(), "akey-exec-err", true)
	if err == nil {
		t.Fatal("expected error from ExecContext, got nil")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestAPIKeyRepository_Create_AllowPush — the INSERT must include allow_push
// as the 17th argument (after memory_write), default false.
func TestAPIKeyRepository_Create_AllowPush(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewAPIKeyRepository(db)

	key := &persistence.APIKey{
		ID: "akey-ap2", ProjectID: "proj", Name: "n", KeyHash: "h2", KeyPrefix: "pre",
		CreatedAt: time.Now(),
	}
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO api_keys")).
		WithArgs(key.ID, key.ProjectID, key.Name, key.KeyHash, key.KeyPrefix,
			key.CreatedAt, key.ExpiresAt, sql.NullString{},
			sql.NullInt64{}, sql.NullInt64{},
			sql.NullString{}, sql.NullFloat64{}, sql.NullString{}, sql.NullString{},
			false, false, false, sql.NullString{}). // memory_read, memory_write, allow_push, default_repo_scope
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Create(context.Background(), key); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}
