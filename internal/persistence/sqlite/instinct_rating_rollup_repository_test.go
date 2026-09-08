package sqlite_test

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// sqliteInstinctRollupSeeder builds the three tables the reported-arms query
// reads. instinct_applications carries an FK to instincts on this backend, so
// the parent row is created on first use.
type sqliteInstinctRollupSeeder struct {
	db   *sqlite.DB
	seen map[string]bool
}

func (s sqliteInstinctRollupSeeder) SeedStepOutcome(ctx context.Context,
	executionID, stepID, projectID, role, errorClass, outcome string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO execution_step_outcomes
		  (id, project_id, task_id, execution_id, step_id, role, outcome, error_class, recorded_at)
		VALUES (?, ?, '', ?, ?, ?, ?, ?, ?)`,
		executionID+"/"+stepID, projectID, executionID, stepID, role, outcome, errorClass,
		at.UTC().Format(time.RFC3339Nano))
	return err
}

func (s sqliteInstinctRollupSeeder) SeedInstinctApplication(ctx context.Context,
	instinctID, executionID, stepID, result string, at time.Time) error {
	ts := at.UTC().Format(time.RFC3339Nano)
	if !s.seen[instinctID] {
		if _, err := s.db.ExecContext(ctx, `
			INSERT OR IGNORE INTO instincts
			  (id, scope, project_id, domain, trigger_key, action, status,
			   created_at, updated_at, last_seen_at)
			VALUES (?, 'project', 'proj-inst', 'recovery', ?, 'retry', 'active', ?, ?, ?)`,
			instinctID, "trigger-"+instinctID, ts, ts, ts); err != nil {
			return err
		}
		s.seen[instinctID] = true
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO instinct_applications
		  (id, instinct_id, task_id, surface, result, applied_at, execution_id, step_id)
		VALUES (?, ?, '', 'lead_recovery', ?, ?, ?, ?)`,
		instinctID+"/"+executionID+"/"+stepID, instinctID, result, ts, executionID, stepID)
	return err
}

func (s sqliteInstinctRollupSeeder) SeedRating(ctx context.Context, executionID, raterID, verdict string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO execution_ratings
		  (execution_id, rater_id, verdict, reason, created_at, updated_at)
		VALUES (?, ?, ?, '', ?, ?)`, executionID, raterID, verdict, now, now)
	return err
}

// The SQLite half of the reported-instinct-arms contract. The Postgres half
// runs the identical suite under -tags=integration.
func TestInstinctRatingRollupRepository_SQLite(t *testing.T) {
	db := newTestDB(t)
	repotest.RunInstinctRatingRollupSuite(t,
		persistence.InstinctRatingRollupRepository(sqlite.NewInstinctRatingRollupRepository(db)),
		sqliteInstinctRollupSeeder{db: db, seen: map[string]bool{}})
}
