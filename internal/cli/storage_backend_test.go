package cli

import (
	"database/sql"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence/postgres"
	"vornik.io/vornik/internal/storage"
)

func TestRequirePostgresDBRejectsSQLiteBackend(t *testing.T) {
	_, err := requirePostgresDB(&storage.Backend{Driver: "sqlite"}, "memory reassign")
	if err == nil {
		t.Fatal("expected unsupported-backend error")
	}
	if !strings.Contains(err.Error(), "requires database.driver=postgres") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRequirePostgresDBReturnsPostgresHandle(t *testing.T) {
	db := &sql.DB{}
	got, err := requirePostgresDB(&storage.Backend{
		Driver: "postgres",
		PG:     &postgres.DB{DB: db},
	}, "retention")
	if err != nil {
		t.Fatalf("requirePostgresDB: %v", err)
	}
	if got != db {
		t.Fatalf("got %p, want %p", got, db)
	}
}
