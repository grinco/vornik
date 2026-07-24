package sqlite_test

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence/sqlite"
)

// TestMigration137_TaintLineage_Idempotent verifies migration 137 (taint-lineage
// columns + partial index) applies cleanly, round-trips the three columns, and
// is idempotent. In-memory SQLite — never touches the prod DB.
func TestMigration137_TaintLineage_Idempotent(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Connect(ctx, sqlite.DefaultConfig())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}

	// The three columns exist and accept a tainted row.
	_, err = db.ExecContext(ctx, `INSERT INTO execution_step_outcomes
		(id, project_id, task_id, execution_id, step_id, outcome, recorded_at,
		 untrusted_content_used, untrusted_sources, requires_review)
		VALUES ('oc-m137', 'p', 't', 'e', 's', 'ok', datetime('now'),
		 1, '[{"tool":"web_fetch","ref":"https://a.example","severity":2}]', 1)`)
	if err != nil {
		t.Fatalf("insert with taint columns: %v", err)
	}

	var used, review int
	var sources string
	row := db.QueryRowContext(ctx, `SELECT untrusted_content_used, requires_review, untrusted_sources
		FROM execution_step_outcomes WHERE id = 'oc-m137'`)
	if err := row.Scan(&used, &review, &sources); err != nil {
		t.Fatalf("scan taint columns: %v", err)
	}
	if used != 1 || review != 1 || sources == "" {
		t.Errorf("taint columns = (used=%d, review=%d, sources=%q)", used, review, sources)
	}

	// The partial index exists (query planner uses it; existence check via
	// sqlite_master is enough for the parity guarantee).
	var idxName string
	row = db.QueryRowContext(ctx, `SELECT name FROM sqlite_master
		WHERE type='index' AND name='idx_step_outcomes_task_taint'`)
	if err := row.Scan(&idxName); err != nil {
		t.Fatalf("taint partial index missing: %v", err)
	}

	// Idempotency: re-migrate must not error.
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate (idempotency): %v", err)
	}
}

// TestMigration137_TaintLineage_DefaultsFalse verifies rows inserted without the
// taint columns get the false/NULL defaults (non-agent / pre-migration rows).
func TestMigration137_TaintLineage_DefaultsFalse(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Connect(ctx, sqlite.DefaultConfig())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO execution_step_outcomes
		(id, project_id, task_id, execution_id, step_id, outcome, recorded_at)
		VALUES ('oc-m137b', 'p', 't', 'e', 's', 'ok', datetime('now'))`)
	if err != nil {
		t.Fatalf("insert without taint columns: %v", err)
	}
	var used, review int
	row := db.QueryRowContext(ctx, `SELECT untrusted_content_used, requires_review
		FROM execution_step_outcomes WHERE id = 'oc-m137b'`)
	if err := row.Scan(&used, &review); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if used != 0 || review != 0 {
		t.Errorf("defaults = (used=%d, review=%d), want (0,0)", used, review)
	}
}
