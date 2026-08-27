//go:build integration

package postgres

import (
	"context"
	"fmt"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// Migration 170 reclassifies the historical residual bucket. It is exercised
// here against real Postgres by re-running its own Up SQL — which is also how
// its idempotency claim is tested, since the design promises a second run is a
// no-op.
//
// Measured on the production database 2026-08-26: 3,027 of 5,791 classified step failures
// carried `container_non_zero_exit`, and 87.6% of them were nameable. A rename
// alone would have left them continuous but WRONG, so the migration applies
// the same patterns the refiner does rather than only relabelling the residual.
//
// Design: https://docs.vornik.io (D1, D4)
func TestMigration170_ReclassifiesHistoricalResidual(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()

	up := migrationUpSQL(t, 170)

	cases := []struct {
		name   string
		detail string
		want   string
	}{
		{"llm provider failure",
			"agent reported FAILED status: LLM call failed: upstream provider returned an error",
			"llm_call_failed"},
		{"missing prerequisite",
			`agent reported FAILED status: Missing prerequisite: file_read of "/app/x.md"`,
			"missing_prerequisite"},
		{"container start failure",
			"failed to start container: podman run failed: exit status 125",
			"container_start_failed"},
		{"container wait failure",
			"failed waiting for container: podman wait failed: exit status 1",
			"container_wait_failed"},
		// Precedence: `signal: killed` is a strict SUBSET of `podman wait
		// failed`. Matched in the wrong order, all 47 production kills would
		// land in the generic wait bucket.
		{"killed container beats the generic wait match",
			"failed waiting for container: podman wait failed: signal: killed",
			"container_killed"},
		// Migration-only patterns: the live path routes these before the
		// refiner ever runs (classifyStepOutcome), so they need no refiner arm
		// — but the historical rows predate that routing and are here.
		{"schema violation (migration-only pattern)",
			`schema violation: role "analyst" result.json is missing required keys: [analysis]`,
			"verify_claims_failed"},
		{"iteration cap (migration-only, pre-refiner rows)",
			"agent reported FAILED status: Tool iteration limit (20) reached.",
			"iteration_cap"},
		// The log tail must NOT decide the class. This is the documented trap:
		// error_detail carries the container log, and a tail mentioning an LLM
		// retry would otherwise reclassify an unrelated failure.
		{"log tail must not decide the class",
			"agent reported FAILED status: some novel failure nobody has classified\n\n" +
				"--- Container Log (last 400 lines) ---\nLLM call failed: retrying\n",
			"unclassified"},
		{"genuinely unrecognised becomes the named residual",
			"agent reported FAILED status: some novel failure nobody has classified",
			"unclassified"},
	}

	seed := func(i int, detail string) string {
		id := fmt.Sprintf("m170-%d-%d", time.Now().UnixNano(), i)
		_, err := db.DB.ExecContext(ctx, `
			INSERT INTO execution_step_outcomes
				(id, project_id, task_id, execution_id, step_id, role, model,
				 outcome, error_class, error_detail, recorded_at)
			VALUES ($1,'p','t','e','s','worker','m','failed','container_non_zero_exit',$2, now())`,
			id, detail)
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		t.Cleanup(func() {
			_, _ = db.DB.Exec(`DELETE FROM execution_step_outcomes WHERE id = $1`, id)
		})
		return id
	}

	ids := make([]string, len(cases))
	for i, tc := range cases {
		ids[i] = seed(i, tc.detail)
	}

	if _, err := db.DB.ExecContext(ctx, up); err != nil {
		t.Fatalf("apply migration 170 Up: %v", err)
	}

	classOf := func(id string) string {
		var c string
		if err := db.DB.QueryRowContext(ctx,
			`SELECT error_class FROM execution_step_outcomes WHERE id = $1`, id).Scan(&c); err != nil {
			t.Fatalf("read back %s: %v", id, err)
		}
		return c
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classOf(ids[i]); got != tc.want {
				t.Errorf("error_class = %q, want %q\n  detail: %s", got, tc.want, tc.detail)
			}
		})
	}

	// Idempotency: a second run must change nothing. The design leans on this
	// — after the rewrite no row matches the old value, so a replay is inert.
	before := make([]string, len(ids))
	for i, id := range ids {
		before[i] = classOf(id)
	}
	if _, err := db.DB.ExecContext(ctx, up); err != nil {
		t.Fatalf("re-apply migration 170 Up: %v", err)
	}
	for i, id := range ids {
		if got := classOf(id); got != before[i] {
			t.Errorf("re-running the migration changed %s: %q -> %q", id, before[i], got)
		}
	}
}

// A row that never held the old value must not be touched, and in particular a
// correctly-classified row must not be swept into the residual.
func TestMigration170_LeavesOtherClassesAlone(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()

	id := fmt.Sprintf("m170-untouched-%d", time.Now().UnixNano())
	if _, err := db.DB.ExecContext(ctx, `
		INSERT INTO execution_step_outcomes
			(id, project_id, task_id, execution_id, step_id, role, model,
			 outcome, error_class, error_detail, recorded_at)
		VALUES ($1,'p','t','e','s','worker','m','failed','hallucinated_claim',
		        'LLM call failed: this text would match a wave-2 pattern', now())`, id); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() { _, _ = db.DB.Exec(`DELETE FROM execution_step_outcomes WHERE id = $1`, id) })

	if _, err := db.DB.ExecContext(ctx, migrationUpSQL(t, 170)); err != nil {
		t.Fatalf("apply: %v", err)
	}

	var got string
	if err := db.DB.QueryRowContext(ctx,
		`SELECT error_class FROM execution_step_outcomes WHERE id = $1`, id).Scan(&got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got != "hallucinated_claim" {
		t.Errorf("migration touched a row it does not own: error_class = %q", got)
	}
}

func migrationUpSQL(t *testing.T, version int) string {
	t.Helper()
	for _, m := range persistence.DefaultMigrations {
		if m.Version == version {
			return m.Up
		}
	}
	t.Fatalf("migration %d not found", version)
	return ""
}

// Down must restore exactly the rows Up changed.
//
// The first cut bounded Down by a literal timestamp frozen when the migration
// was authored. That is wrong in a way a static reading hides: the bound has to
// mean "rows that existed when the migration RAN", and a migration authored on
// one day and deployed on another under-reverts everything recorded in between
// — including, as it happened, rows recorded later on the authoring day itself.
//
// The runner records `applied_at` when Up commits, and Rollback executes Down
// BEFORE deleting that row and in the same transaction, so the exact bound is
// readable from within Down.
//
// The seeded row is stamped BEFORE that applied_at, because that is what a
// historical row IS. An earlier cut of this test stamped it now() and so
// modelled a row written after the migration had already run — which Down is
// correct to leave alone, and which made the test fail against correct code.
func TestMigration170_DownRestoresWhatUpChanged(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()

	id := fmt.Sprintf("m170-down-%d", time.Now().UnixNano())
	if _, err := db.DB.ExecContext(ctx, `
		INSERT INTO execution_step_outcomes
			(id, project_id, task_id, execution_id, step_id, role, model,
			 outcome, error_class, error_detail, recorded_at)
		VALUES ($1,'p','t','e','s','worker','m','failed','container_non_zero_exit',
		        'agent reported FAILED status: LLM call failed: upstream provider returned an error',
		        COALESCE((SELECT applied_at FROM migrations WHERE version = 170), now())
		          - interval '1 day')`, id); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() { _, _ = db.DB.Exec(`DELETE FROM execution_step_outcomes WHERE id = $1`, id) })

	classOf := func() string {
		var c string
		if err := db.DB.QueryRowContext(ctx,
			`SELECT error_class FROM execution_step_outcomes WHERE id = $1`, id).Scan(&c); err != nil {
			t.Fatalf("read back: %v", err)
		}
		return c
	}

	if _, err := db.DB.ExecContext(ctx, migrationUpSQL(t, 170)); err != nil {
		t.Fatalf("up: %v", err)
	}
	if got := classOf(); got != "llm_call_failed" {
		t.Fatalf("after Up: error_class = %q, want llm_call_failed", got)
	}

	if _, err := db.DB.ExecContext(ctx, migrationDownSQL(t, 170)); err != nil {
		t.Fatalf("down: %v", err)
	}
	if got := classOf(); got != "container_non_zero_exit" {
		t.Errorf("after Down: error_class = %q, want container_non_zero_exit — "+
			"Down did not restore a row Up had changed", got)
	}
}

func migrationDownSQL(t *testing.T, version int) string {
	t.Helper()
	for _, m := range persistence.DefaultMigrations {
		if m.Version == version {
			return m.Down
		}
	}
	t.Fatalf("migration %d not found", version)
	return ""
}
