package api

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite" // the CE default driver, and the one this file is about

	"github.com/stretchr/testify/require"
)

// The doctor runs against whatever database the daemon has — c.DB is set for
// SQLite as well as Postgres, and NewDoctorHandlers takes it with no driver
// check. Before 2026-09-04, four checks issued Postgres-only SQL and reported
// the resulting driver error as WARNING on every run of every SQLite
// deployment, forever; a fifth used a LIKE pattern that silently matched
// nothing there. See the doctor design's Extension 2026-09-04.
//
// This file is the guard that stops the next one. It is deliberately about the
// WHOLE report rather than the five sites that were fixed: a check written next
// year gets the same treatment without anyone remembering to add it here.

// sqlDriverErrorSignatures are the substrings a SQLite driver puts in an error
// when it is handed SQL it cannot parse or execute. A doctor message carrying
// one of these is a check that did not run, reported as though it had.
var sqlDriverErrorSignatures = []string{
	"SQL logic error",
	"no such function",
	"no such column",
	"syntax error",
	`near "`,
	"unrecognized token",
}

// seedPortabilityDB builds the subset of the schema the DB-backed doctor checks
// touch, on SQLite, with rows that make each of them do real work rather than
// return early on an empty table.
func seedPortabilityDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	db.SetMaxOpenConns(1) // :memory: is per-connection
	t.Cleanup(func() { _ = db.Close() })

	old := time.Now().UTC().Add(-48 * time.Hour)
	stmts := []string{
		`CREATE TABLE tasks (
			id TEXT PRIMARY KEY, project_id TEXT, status TEXT,
			lease_id TEXT, leased_at TIMESTAMP, leased_by TEXT,
			lease_expires_at TIMESTAMP, updated_at TIMESTAMP)`,
		`CREATE TABLE executions (
			id TEXT PRIMARY KEY, task_id TEXT, project_id TEXT, status TEXT,
			created_at TIMESTAMP, updated_at TIMESTAMP, error_message TEXT)`,
		`CREATE TABLE task_watchers (task_id TEXT, chat_id TEXT, created_at TIMESTAMP)`,
		`CREATE TABLE task_llm_usage (id TEXT PRIMARY KEY, task_id TEXT, source TEXT)`,
		`CREATE TABLE tool_audit_log (id TEXT PRIMARY KEY, task_id TEXT)`,
		`CREATE TABLE execution_step_outcomes (
			execution_id TEXT, step_id TEXT, role TEXT, model TEXT,
			outcome TEXT, error_class TEXT, recorded_at TIMESTAMP)`,
		// The breach ledger: present but empty. Absent, the check degrades to
		// SKIPPED with the driver's "no such table" in its message, which the
		// assertions below (rightly) refuse — the fixture's job is to let each
		// check reach its SQL, so the guard tests the QUERY and not the fixture.
		`CREATE TABLE security_incidents (
			id TEXT PRIMARY KEY, detected_at TIMESTAMP, severity TEXT,
			authority_notified_at TIMESTAMP, subjects_notified_at TIMESTAMP,
			status TEXT)`,
	}
	for _, stmt := range stmts {
		_, err := db.Exec(stmt)
		require.NoError(t, err, stmt)
	}

	// A task with an EXPIRED lease — stale_leases must find it.
	_, err = db.Exec(`INSERT INTO tasks (id, project_id, status, lease_id, lease_expires_at, updated_at)
		VALUES ('t-stale','p1','LEASED','lease-1',?,?)`, old, old)
	require.NoError(t, err)
	// An execution RUNNING for two days — stuck_executions must find it.
	_, err = db.Exec(`INSERT INTO executions (id, task_id, project_id, status, created_at, updated_at)
		VALUES ('e-stuck','t-stale','p1','RUNNING',?,?)`, old, old)
	require.NoError(t, err)
	// A fallback rung that has attempted and never reached inference —
	// fallback_rungs must find it. The step id carries the underscores the
	// LIKE pattern escapes.
	for i := 0; i < 5; i++ {
		_, err = db.Exec(`INSERT INTO execution_step_outcomes
			(execution_id, step_id, role, model, outcome, error_class, recorded_at)
			VALUES ('e-stuck','plan_model_fallback','scout','gemma4:26b','failed','model_unhealthy',?)`,
			time.Now().UTC().Add(-time.Duration(i+1)*time.Hour))
		require.NoError(t, err)
	}
	return db
}

// TestDoctor_EveryCheckRunsOnSQLite — the report-wide guard. No check may
// report a driver error as its verdict, and a check that opts out on this
// driver must say the driver is why.
func TestDoctor_EveryCheckRunsOnSQLite(t *testing.T) {
	h := NewDoctorHandlers(seedPortabilityDB(t))
	report := h.RunReportReadOnly(context.Background())
	require.NotEmpty(t, report.Checks, "the report must contain checks")

	for _, c := range report.Checks {
		for _, sig := range sqlDriverErrorSignatures {
			if strings.Contains(c.Message, sig) {
				t.Errorf("check %q reported a DRIVER ERROR as its verdict on SQLite: %s = %q\n"+
					"A check that cannot run must report SKIPPED and say the driver is why "+
					"(doctor design, Extension 2026-09-04 E2); a driver error is never a verdict.",
					c.Name, c.Status, c.Message)
				break
			}
		}
	}
}

// TestDoctor_TheDatabaseBackedChecksReachTheirSQL — the other half of the
// guard, and the one that stops it passing vacuously.
//
// A check that short-circuits on an unwired dependency returns SKIPPED without
// its SQL ever running, so a Postgres-only query behind that guard would never
// be exercised and the report-wide test above would go green over an unexamined
// surface — this design's own "unevaluated check reports OK" failure mode,
// inside its own test. So the four DB-backed checks are named here and must
// each return a REAL verdict against the seeded rows.
func TestDoctor_TheDatabaseBackedChecksReachTheirSQL(t *testing.T) {
	h := NewDoctorHandlers(seedPortabilityDB(t))
	ctx := context.Background()

	cases := []struct {
		name  string
		check DoctorCheck
		want  string // a substring the real verdict must contain
	}{
		{"stale_leases", h.checkStaleLeases(ctx, false), "expired lease"},
		{"stuck_executions", h.checkStuckExecutions(ctx, false), "stuck"},
		{"task_state_audit", h.checkTaskStateAudit(ctx, false), ""},
		{"fallback_rungs", h.checkFallbackRungs(ctx), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.check
			require.NotEqual(t, "SKIPPED", c.Status,
				"everything this check needs IS wired in the fixture, so a SKIPPED here "+
					"means the fixture is wrong or the check bailed before its SQL: %s", c.Message)
			for _, sig := range sqlDriverErrorSignatures {
				require.NotContains(t, c.Message, sig, "driver error instead of a verdict")
			}
			if tc.want != "" {
				require.Contains(t, strings.ToLower(c.Message), tc.want,
					"the check must have SEEN the seeded rows, not merely survived")
			}
		})
	}
}

// TestDoctor_FallbackRungsLikeEscapeIsPortable — the LIKE that failed
// silently, pinned at the site.
//
// Postgres treats `\` as LIKE's default escape character; SQLite has none, so
// `'%\_model\_fallback%'` without an explicit ESCAPE asked for a literal
// backslash and matched nothing — a check reporting a clean bill of health for
// a surface it never examined. Measured before the fix: 0 rows matched on
// SQLite. Both drivers accept the explicit clause.
func TestDoctor_FallbackRungsLikeEscapeIsPortable(t *testing.T) {
	db := seedPortabilityDB(t)
	var n int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM execution_step_outcomes WHERE step_id LIKE '%\_model\_fallback%' ESCAPE '\'`,
	).Scan(&n))
	require.Equal(t, 5, n, "the escaped pattern must match the seeded rung on SQLite")

	// And the check built on it finds the rung rather than reporting nothing.
	rungs, err := NewDoctorHandlers(db).queryDeadFallbackRungs(context.Background())
	require.NoError(t, err)
	require.Len(t, rungs, 1, "the dead rung must be found on SQLite, not silently missed")
	require.Equal(t, "plan_model_fallback", rungs[0].stepID)
	require.Equal(t, "model_unhealthy", rungs[0].lastClass,
		"the last-class subquery must return the newest row's class")
}
