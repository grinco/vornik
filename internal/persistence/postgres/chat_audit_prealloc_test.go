package postgres

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"vornik.io/vornik/internal/persistence"
)

// TestChatAuditRepository_List_PreallocClamped is the postgres parity of the
// sqlite regression for the CodeQL go/uncontrolled-allocation-size finding at
// List. filter.PageSize arrives from an HTTP query parameter; the fix clamps
// the result-slice capacity hint with min(PageSize, maxChatAuditPrealloc).
//
// The clamp is observed via cap(out): a pathological PageSize with only a few
// returned rows leaves cap at the (clamped) initial capacity. Pre-fix cap
// would equal the attacker-controlled PageSize.
func TestChatAuditRepository_List_PreallocClamped(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewChatAuditRepository(db)

	cols := []string{
		"id", "ts", "chat_id", "user_id", "project_id", "role_used", "model",
		"system_prompt_hash", "user_message", "tool_calls_json", "response",
		"iterations", "duration_ms", "cost_usd", "hallucination_signals_json",
	}
	rows := sqlmock.NewRows(cols)
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		rows.AddRow("id", now, "c", "u", "p", "r", "m", "h", "msg", "[]", "resp", 2, int64(50), 0.01, "")
	}

	const huge = 1 << 30
	mock.ExpectQuery(regexp.QuoteMeta("FROM chat_audit_log")).
		WithArgs("p", huge, 0).
		WillReturnRows(rows)

	got, err := repo.List(context.Background(), persistence.ChatAuditFilter{ProjectID: "p", PageSize: huge})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("List returned %d rows, want 3", len(got))
	}
	if cap(got) > maxChatAuditPrealloc {
		t.Errorf("List pre-allocated cap=%d for PageSize=%d; want clamp to <= %d (uncontrolled-allocation-size regression)", cap(got), huge, maxChatAuditPrealloc)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}
