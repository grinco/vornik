package sqlite_test

import (
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// The SQLite half of the shared execution-rating contract. The Postgres half
// runs the identical suite under -tags=integration; that they are the same
// function is what keeps the two backends from disagreeing about upsert
// semantics, which is exactly where an INSERT OR REPLACE would have diverged.
func TestExecutionRatingRepository_SQLite(t *testing.T) {
	db := newTestDB(t)
	repotest.RunExecutionRatingSuite(t, persistence.ExecutionRatingRepository(
		sqlite.NewExecutionRatingRepository(db)))
}
