package retention

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// chatRetentionDB is a real (in-memory) database rather than sqlmock: what
// these tests are about is the SEMANTICS of the reference guard, and a mock
// that matches query text would pass whatever the WHERE clause said.
func chatRetentionDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`
CREATE TABLE chat_audit_log (
    id TEXT PRIMARY KEY,
    ts TIMESTAMP NOT NULL,
    project_id TEXT NOT NULL DEFAULT '',
    system_prompt_hash TEXT NOT NULL DEFAULT ''
);
CREATE TABLE chat_system_prompts (
    hash TEXT PRIMARY KEY,
    body TEXT NOT NULL
);
CREATE TABLE tasks (
    id TEXT PRIMARY KEY,
    chat_turn_id TEXT
);`)
	require.NoError(t, err)
	return db
}

func addChatTurn(t *testing.T, db *sql.DB, id string, age time.Duration, promptHash string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO chat_audit_log (id, ts, project_id, system_prompt_hash) VALUES (?, ?, 'p1', ?)`,
		id, time.Now().UTC().Add(-age), promptHash)
	require.NoError(t, err)
}

func chatRowIDs(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT id FROM chat_audit_log ORDER BY id`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		out = append(out, id)
	}
	require.NoError(t, rows.Err())
	return out
}

// The horizon prunes what is past it and leaves what is not.
func TestPruneChatAuditLog_PrunesPastTheHorizon(t *testing.T) {
	db := chatRetentionDB(t)
	s := New(db, zerolog.Nop())
	addChatTurn(t, db, "old", 120*24*time.Hour, "")
	addChatTurn(t, db, "recent", 3*24*time.Hour, "")

	n, err := s.pruneChatAuditLog(context.Background(), "p1",
		time.Now().UTC().AddDate(0, 0, -DefaultChatAuditDays), false)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, []string{"recent"}, chatRowIDs(t, db))
}

// The guard: a row a task still points at is the ONLY record of where that
// task's result gets delivered, so it survives however old it is.
func TestPruneChatAuditLog_KeepsARowALiveTaskReferences(t *testing.T) {
	db := chatRetentionDB(t)
	s := New(db, zerolog.Nop())
	addChatTurn(t, db, "origin-of-a-parked-task", 400*24*time.Hour, "")
	_, err := db.Exec(`INSERT INTO tasks (id, chat_turn_id) VALUES ('t-parked', 'origin-of-a-parked-task')`)
	require.NoError(t, err)

	n, err := s.pruneChatAuditLog(context.Background(), "p1",
		time.Now().UTC().AddDate(0, 0, -DefaultChatAuditDays), false)
	require.NoError(t, err)
	assert.Equal(t, 0, n, "a referenced origin row must never be pruned")
	assert.Equal(t, []string{"origin-of-a-parked-task"}, chatRowIDs(t, db))
}

// The other half of the guard, and the case review round 1 asked for (F6):
// once the task row is gone, the origin row protects nothing — there is
// nothing left to deliver from — so it IS collected. The guard and the thing
// it guards disappear together, in that order.
func TestPruneChatAuditLog_CollectsTheRowOnceItsTaskIsGone(t *testing.T) {
	db := chatRetentionDB(t)
	s := New(db, zerolog.Nop())
	addChatTurn(t, db, "origin", 400*24*time.Hour, "")
	_, err := db.Exec(`INSERT INTO tasks (id, chat_turn_id) VALUES ('t1', 'origin')`)
	require.NoError(t, err)

	horizon := time.Now().UTC().AddDate(0, 0, -DefaultChatAuditDays)
	n, err := s.pruneChatAuditLog(context.Background(), "p1", horizon, false)
	require.NoError(t, err)
	require.Equal(t, 0, n)

	// The task reaches a terminal status and its own window collects it.
	_, err = db.Exec(`DELETE FROM tasks WHERE id = 't1'`)
	require.NoError(t, err)

	n, err = s.pruneChatAuditLog(context.Background(), "p1", horizon, false)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Empty(t, chatRowIDs(t, db))
}

// Preview counts without deleting.
func TestPruneChatAuditLog_PreviewDoesNotDelete(t *testing.T) {
	db := chatRetentionDB(t)
	s := New(db, zerolog.Nop())
	addChatTurn(t, db, "old", 400*24*time.Hour, "")

	n, err := s.pruneChatAuditLog(context.Background(), "p1",
		time.Now().UTC().AddDate(0, 0, -DefaultChatAuditDays), true)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, []string{"old"}, chatRowIDs(t, db), "preview must not delete")
}

// A prompt body lives exactly as long as some turn references it — no horizon
// of its own, and shared across projects when the bytes are shared.
func TestPruneUnreferencedChatPrompts_KeepsWhatIsStillReferenced(t *testing.T) {
	db := chatRetentionDB(t)
	s := New(db, zerolog.Nop())
	_, err := db.Exec(`INSERT INTO chat_system_prompts (hash, body) VALUES ('h-live','body'),('h-orphan','body2')`)
	require.NoError(t, err)
	addChatTurn(t, db, "turn", time.Hour, "h-live")

	n, err := s.pruneUnreferencedChatPrompts(context.Background(), false)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "only the unreferenced body goes")

	var remaining string
	require.NoError(t, db.QueryRow(`SELECT hash FROM chat_system_prompts`).Scan(&remaining))
	assert.Equal(t, "h-live", remaining)

	// Once the last referring turn goes, so does the body.
	_, err = db.Exec(`DELETE FROM chat_audit_log WHERE id = 'turn'`)
	require.NoError(t, err)
	n, err = s.pruneUnreferencedChatPrompts(context.Background(), false)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

func TestPruneUnreferencedChatPrompts_PreviewDoesNotDelete(t *testing.T) {
	db := chatRetentionDB(t)
	s := New(db, zerolog.Nop())
	_, err := db.Exec(`INSERT INTO chat_system_prompts (hash, body) VALUES ('h-orphan','body')`)
	require.NoError(t, err)

	n, err := s.pruneUnreferencedChatPrompts(context.Background(), true)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM chat_system_prompts`).Scan(&count))
	assert.Equal(t, 1, count, "preview must not delete")
}

// The horizon is always-on and defaults to 90 — deliberately longer than the
// tasks window, so an origin row outlives the task that could still need it.
func TestResolve_ChatAuditDaysDefaultsAboveTheTaskWindow(t *testing.T) {
	got := Resolve("p", Policy{}, Policy{})
	assert.Equal(t, DefaultChatAuditDays, got.ChatAuditDays)
	assert.Greater(t, DefaultChatAuditDays, DefaultTasksDays,
		"an origin row must outlive the task that might still need it")

	// Per-project wins, and the floor still applies.
	assert.Equal(t, 7, Resolve("p", Policy{ChatAuditDays: 7}, Policy{}).ChatAuditDays)
	assert.GreaterOrEqual(t, Resolve("p", Policy{ChatAuditDays: -3}, Policy{}).ChatAuditDays, MinimumFloorDays)
}
