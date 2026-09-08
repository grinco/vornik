package sqlite_test

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// sqliteRollupSeeder writes the three tables the rollup reads. It inserts
// directly rather than through repositories because the suite needs executions
// in specific (project, workflow) contexts and at specific times, which the
// production create paths do not let a caller choose.
type sqliteRollupSeeder struct{ db *sqlite.DB }

func (s sqliteRollupSeeder) SeedExecution(ctx context.Context, id, projectID, workflowID string, createdAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO executions (id, task_id, project_id, workflow_id, workflow_revision,
		                        status, created_at, updated_at)
		VALUES (?, ?, ?, ?, '1', 'COMPLETED', ?, ?)`,
		id, "task-"+id, projectID, workflowID,
		createdAt.UTC().Format(time.RFC3339Nano), createdAt.UTC().Format(time.RFC3339Nano))
	return err
}

func (s sqliteRollupSeeder) SeedInjectedSkill(ctx context.Context, executionID, skillID, bodySHA256 string) error {
	var sha any
	if bodySHA256 != "" {
		sha = bodySHA256
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO execution_injected_skills (execution_id, skill_id, injected_at, body_sha256)
		 VALUES (?, ?, ?, ?)`,
		executionID, skillID, time.Now().UTC().Format(time.RFC3339Nano), sha)
	return err
}

func (s sqliteRollupSeeder) SeedRating(ctx context.Context, executionID, raterID, verdict string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO execution_ratings (execution_id, rater_id, verdict, reason, created_at, updated_at)
		VALUES (?, ?, ?, '', ?, ?)`, executionID, raterID, verdict, now, now)
	return err
}

// The SQLite half of the shared rollup contract. The Postgres half runs the
// identical suite under -tags=integration.
func TestRatingRollupRepository_SQLite(t *testing.T) {
	db := newTestDB(t)
	// executions.task_id has an FK to tasks, so the parent rows must exist.
	seeder := sqliteRollupSeeder{db: db}
	repotest.RunRatingRollupSuite(t,
		persistence.RatingRollupRepository(sqlite.NewRatingRollupRepository(db)),
		seedWithTasks{seeder, db})
}

// The SQLite half of the body-provenance contract (migration 180). The
// Postgres half runs the identical suite under -tags=integration.
func TestSkillInjectionProvenance_SQLite(t *testing.T) {
	db := newTestDB(t)
	seeder := sqliteRollupSeeder{db: db}
	repotest.RunSkillInjectionProvenanceSuite(t,
		sqlite.NewExecutionInjectedSkillRepository(db.DB),
		seedWithTasks{seeder, db})
}

// seedWithTasks creates the parent task before each execution, so the fixture
// satisfies the FK the schema declares rather than the suite having to know
// about it.
type seedWithTasks struct {
	sqliteRollupSeeder
	db *sqlite.DB
}

func (s seedWithTasks) SeedExecution(ctx context.Context, id, projectID, workflowID string, createdAt time.Time) error {
	if _, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO tasks (id, project_id, workflow_id, status, created_at, updated_at)
		VALUES (?, ?, ?, 'COMPLETED', ?, ?)`,
		"task-"+id, projectID, workflowID,
		createdAt.UTC().Format(time.RFC3339Nano), createdAt.UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return s.sqliteRollupSeeder.SeedExecution(ctx, id, projectID, workflowID, createdAt)
}
