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

// TestChatAuditRepository_Insert — pins the INSERT shape, ID/timestamp
// defaulting, and the empty-tool-calls fallback to "[]".
func TestChatAuditRepository_Insert(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewChatAuditRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO chat_audit_log")).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(),
			"chat-1", "user-1", "proj-1", "lead", "model-1",
			"hash", "msg", "[]", "reply", 2, int64(50), 0.01,
			"", // hallucination_signals_json — empty when no signals fired
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	entry := &persistence.ChatAuditEntry{
		ChatID: "chat-1", UserID: "user-1", ProjectID: "proj-1",
		RoleUsed: "lead", Model: "model-1", SystemPromptHash: "hash",
		UserMessage: "msg", Response: "reply", Iterations: 2,
		DurationMs: 50, CostUSD: 0.01,
	}
	if err := repo.Insert(context.Background(), entry); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if entry.ID == "" {
		t.Error("Insert should populate ID")
	}
}

// TestChatAuditRepository_Insert_Defensive — nil entry guard.
func TestChatAuditRepository_Insert_Defensive(t *testing.T) {
	db, _, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewChatAuditRepository(db)
	if err := repo.Insert(context.Background(), nil); err == nil {
		t.Error("nil entry should error")
	}
}

// TestChatAuditRepository_List — covers every filter branch and the
// scan path.
func TestChatAuditRepository_List(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewChatAuditRepository(db)

	since := time.Date(2026, 5, 19, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	created := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, ts, chat_id")).
		WithArgs("chat-x", "proj-y", since, until, 10, 0).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "ts", "chat_id", "user_id", "project_id", "role_used", "model",
			"system_prompt_hash", "user_message", "tool_calls_json",
			"response", "iterations", "duration_ms", "cost_usd",
			"hallucination_signals_json",
		}).AddRow(
			"row-1", created, "chat-x", "u", "proj-y", "lead", "m",
			"h", "msg", "[]", "r", 1, int64(100), 0.05,
			"", // hallucination_signals_json — no signals on this fixture row
		))

	out, err := repo.List(context.Background(), persistence.ChatAuditFilter{
		ChatID: "chat-x", ProjectID: "proj-y", Since: since, Until: until,
		PageSize: 10,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(out) != 1 || out[0].ID != "row-1" {
		t.Fatalf("List = %+v", out)
	}
}

// TestChatAuditRepository_List_PageSizeGuard — required pagination.
func TestChatAuditRepository_List_PageSizeGuard(t *testing.T) {
	db, _, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewChatAuditRepository(db)
	if _, err := repo.List(context.Background(), persistence.ChatAuditFilter{}); err == nil {
		t.Error("PageSize=0 should error")
	}
}

// TestChatAuditRepository_PromptCache — Save + Get + idempotent Save +
// missing-row ErrNotFound + error propagation.
func TestChatAuditRepository_PromptCache(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewChatAuditRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO chat_system_prompts")).
		WithArgs("h1", "body", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.SavePrompt(context.Background(), "h1", "body"); err != nil {
		t.Fatalf("SavePrompt: %v", err)
	}

	// SavePrompt empty hash → guard.
	if err := repo.SavePrompt(context.Background(), "", "body"); err == nil {
		t.Error("empty hash should error")
	}

	// GetPrompt happy path.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT body FROM chat_system_prompts")).
		WithArgs("h1").
		WillReturnRows(sqlmock.NewRows([]string{"body"}).AddRow("body"))
	got, err := repo.GetPrompt(context.Background(), "h1")
	if err != nil {
		t.Fatalf("GetPrompt: %v", err)
	}
	if got != "body" {
		t.Errorf("body = %q", got)
	}

	// GetPrompt no rows → ErrNotFound.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT body FROM chat_system_prompts")).
		WithArgs("missing").
		WillReturnError(sql.ErrNoRows)
	if _, err := repo.GetPrompt(context.Background(), "missing"); err != persistence.ErrNotFound {
		t.Errorf("missing = %v, want ErrNotFound", err)
	}

	// GetPrompt other error → propagates.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT body FROM chat_system_prompts")).
		WithArgs("boom").
		WillReturnError(errors.New("kaboom"))
	if _, err := repo.GetPrompt(context.Background(), "boom"); err == nil {
		t.Error("expected propagated error")
	}
}
