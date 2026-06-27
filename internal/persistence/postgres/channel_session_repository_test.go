package postgres

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"vornik.io/vornik/internal/persistence"
)

// TestChannelSession_Load_Found: a row exists; Load returns it
// with bytes intact for the caller to unmarshal.
func TestChannelSession_Load_Found(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewChannelSessionRepository(db)

	historyBytes := []byte(`[{"role":"user","content":"hi"}]`)
	now := time.Date(2026, 5, 23, 16, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT kind, session_id")).
		WithArgs("webchat", "sess-1").
		WillReturnRows(sqlmock.NewRows([]string{"kind", "session_id", "active_project", "history", "created_at", "updated_at", "expires_at"}).
			AddRow("webchat", "sess-1", "proj-x", historyBytes, now, now, nil))

	got, err := repo.Load(context.Background(), "webchat", "sess-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.SessionID != "sess-1" || got.Kind != "webchat" || got.ActiveProject != "proj-x" {
		t.Errorf("unexpected row: %+v", got)
	}
	if string(got.History) != string(historyBytes) {
		t.Errorf("history bytes mismatch: %q vs %q", got.History, historyBytes)
	}
}

// TestChannelSession_Load_NotFound returns persistence.ErrNotFound
// so callers can distinguish "fresh session" from "DB failure".
func TestChannelSession_Load_NotFound(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewChannelSessionRepository(db)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT kind, session_id")).
		WithArgs("webchat", "missing").
		WillReturnRows(sqlmock.NewRows([]string{"kind", "session_id", "active_project", "history", "created_at", "updated_at", "expires_at"}))

	_, err := repo.Load(context.Background(), "webchat", "missing")
	if !errors.Is(err, persistence.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestChannelSession_Load_RequiresKindSessionID: defensive guard
// at the boundary so a typo doesn't fire an unbounded query.
func TestChannelSession_Load_RequiresKindSessionID(t *testing.T) {
	db, _, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewChannelSessionRepository(db)

	if _, err := repo.Load(context.Background(), "", "sess"); err == nil {
		t.Errorf("empty kind should error")
	}
	if _, err := repo.Load(context.Background(), "webchat", ""); err == nil {
		t.Errorf("empty sessionID should error")
	}
}

// TestChannelSession_Save_UpsertsRow: the ON CONFLICT upsert is
// the post-turn write-through path. Bind args verify the exact
// kind / session_id / active_project / history payload land in
// the query as sent.
func TestChannelSession_Save_UpsertsRow(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewChannelSessionRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO channel_sessions")).
		WithArgs("email", "thread-A", sqlmock.AnyArg(), []byte(`[{"r":1}]`)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Save(context.Background(), "email", "thread-A", "proj-mail", []byte(`[{"r":1}]`)); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

// TestChannelSession_Save_EmptyHistoryWritesEmptyArray defends
// the Postgres NOT NULL constraint: an empty caller payload
// becomes the JSONB empty array, not a NULL or zero-length
// blob. Without this, the next Load would return a row with
// History=nil and downstream unmarshallers would panic.
func TestChannelSession_Save_EmptyHistoryWritesEmptyArray(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewChannelSessionRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO channel_sessions")).
		WithArgs("slack", "thread-empty", sqlmock.AnyArg(), []byte("[]")).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Save(context.Background(), "slack", "thread-empty", "proj", nil); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

// TestChannelSession_Delete_RemovesRow drives the "clear chat"
// + future-stale-sweeper paths.
func TestChannelSession_Delete_RemovesRow(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewChannelSessionRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM channel_sessions")).
		WithArgs("webchat", "sess-del").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Delete(context.Background(), "webchat", "sess-del"); err != nil {
		t.Errorf("Delete: %v", err)
	}
}
