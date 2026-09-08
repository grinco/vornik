package sqlite_test

import (
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// The SQLite half of the CI-outcome contract. The Postgres half runs the
// identical suite under -tags=integration.
func TestForgeCIOutcomeRepository_SQLite(t *testing.T) {
	db := newTestDB(t)
	repotest.RunForgeCIOutcomeSuite(t,
		persistence.ForgeCIOutcomeRepository(sqlite.NewForgeCIOutcomeRepository(db)))
}
