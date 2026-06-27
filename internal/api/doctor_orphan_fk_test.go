package api

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite" // in-memory DB for the orphan-FK probe

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedOrphanFKDB builds a minimal schema mirroring the columns the
// orphan_fk_rows probe touches, then seeds:
//   - one real task (T1)
//   - task_llm_usage: a valid row (→T1), a NULL-task_id background row
//     (kg_extraction), and a true orphan (→deleted task "ghost")
//   - tool_audit_log: a valid row (→T1) and an empty-task_id row
func seedOrphanFKDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	// :memory: is per-connection; pin the pool to one conn so seed,
	// probe and delete all hit the same in-memory database.
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	stmts := []string{
		`CREATE TABLE tasks (id TEXT PRIMARY KEY)`,
		`CREATE TABLE task_llm_usage (id TEXT PRIMARY KEY, task_id TEXT, source TEXT)`,
		`CREATE TABLE tool_audit_log (id TEXT PRIMARY KEY, task_id TEXT)`,
		`CREATE TABLE task_watchers (task_id TEXT)`,
		`INSERT INTO tasks (id) VALUES ('T1')`,
		// valid + NULL-background + true-orphan
		`INSERT INTO task_llm_usage (id, task_id, source) VALUES ('u1','T1','workflow_step')`,
		`INSERT INTO task_llm_usage (id, task_id, source) VALUES ('u2',NULL,'kg_extraction')`,
		`INSERT INTO task_llm_usage (id, task_id, source) VALUES ('u3','ghost','workflow_step')`,
		// valid + empty-string task_id (must not count)
		`INSERT INTO tool_audit_log (id, task_id) VALUES ('a1','T1')`,
		`INSERT INTO tool_audit_log (id, task_id) VALUES ('a2','')`,
	}
	for _, s := range stmts {
		_, err := db.Exec(s)
		require.NoError(t, err, s)
	}
	return db
}

// TestCheckOrphanFKRows_IgnoresTaskLessRows — regression for the
// 2026-06-11 finding that orphan_fk_rows flagged (and --fix DELETED)
// task_llm_usage rows with a NULL task_id. Those are dispatcher /
// background-maintenance cost records that are task-less by design;
// only a row naming a task that no longer exists is a real orphan.
func TestCheckOrphanFKRows_IgnoresTaskLessRows(t *testing.T) {
	h := &DoctorHandlers{db: seedOrphanFKDB(t)}

	got := h.checkOrphanFKRows(t.Context(), false)
	assert.Equal(t, "orphan_fk_rows", got.Name)
	// Exactly one true orphan (u3 → "ghost"); the NULL-task_id and
	// empty-task_id rows must not be counted.
	assert.Equal(t, "WARNING", got.Status)
	assert.Contains(t, got.Message, "1 orphan")

	// --fix must delete only the true orphan and leave the task-less
	// rows intact.
	fixed := h.checkOrphanFKRows(t.Context(), true)
	assert.Equal(t, "OK", fixed.Status)
	assert.Equal(t, 1, fixed.Fixed)

	var llm, audit int
	require.NoError(t, h.db.QueryRow(`SELECT COUNT(*) FROM task_llm_usage`).Scan(&llm))
	require.NoError(t, h.db.QueryRow(`SELECT COUNT(*) FROM tool_audit_log`).Scan(&audit))
	assert.Equal(t, 2, llm, "NULL-task_id background row must survive --fix")
	assert.Equal(t, 2, audit, "empty-task_id audit row must survive --fix")
}
