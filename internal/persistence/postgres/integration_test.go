//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"
)

// Integration tests that require a PostgreSQL instance.
// Run with: go test -tags=integration ./internal/persistence/postgres/...
// Or use: make test-int

// TestIntegrationConnect tests real PostgreSQL connection.
func TestIntegrationConnect(t *testing.T) {
	cfg := Config{
		Host:            getEnvOrDefault("POSTGRES_HOST", "localhost"),
		Port:            integrationPort(),
		Database:        getEnvOrDefault("POSTGRES_DB", integrationDBName),
		User:            getEnvOrDefault("POSTGRES_USER", "vornik"),
		Password:        getEnvOrDefault("POSTGRES_PASSWORD", "vornik"),
		SSLMode:         "disable",
		MaxOpenConns:    5,
		MaxIdleConns:    2,
		ConnMaxLifetime: 5 * time.Minute,
		ConnMaxIdleTime: 2 * time.Minute,
		ConnectTimeout:  10 * time.Second,
	}

	ctx := context.Background()
	db, err := Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer db.Close()

	// Verify connection is valid
	if err := db.PingContext(ctx); err != nil {
		t.Errorf("failed to ping: %v", err)
	}
}

// TestIntegrationMigrate tests migration execution.
func TestIntegrationMigrate(t *testing.T) {
	cfg := Config{
		Host:           getEnvOrDefault("POSTGRES_HOST", "localhost"),
		Port:           integrationPort(),
		Database:       getEnvOrDefault("POSTGRES_DB", integrationDBName),
		User:           getEnvOrDefault("POSTGRES_USER", "vornik"),
		Password:       getEnvOrDefault("POSTGRES_PASSWORD", "vornik"),
		SSLMode:        "disable",
		ConnectTimeout: 10 * time.Second,
	}

	ctx := context.Background()
	db, err := Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer db.Close()

	// Run migrations
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("failed to migrate: %v", err)
	}

	// Verify migrations were applied
	runner := db.MigrationRunner()
	status, err := runner.Status(ctx)
	if err != nil {
		t.Fatalf("failed to get migration status: %v", err)
	}

	if len(status.Pending) > 0 {
		t.Errorf("expected no pending migrations, got %d", len(status.Pending))
	}

	if status.CurrentVersion < 1 {
		t.Errorf("expected current version >= 1, got %d", status.CurrentVersion)
	}
}

// TestIntegrationIsReady tests readiness check.
func TestIntegrationIsReady(t *testing.T) {
	cfg := Config{
		Host:           getEnvOrDefault("POSTGRES_HOST", "localhost"),
		Port:           integrationPort(),
		Database:       getEnvOrDefault("POSTGRES_DB", integrationDBName),
		User:           getEnvOrDefault("POSTGRES_USER", "vornik"),
		Password:       getEnvOrDefault("POSTGRES_PASSWORD", "vornik"),
		SSLMode:        "disable",
		ConnectTimeout: 10 * time.Second,
	}

	ctx := context.Background()
	db, err := Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer db.Close()

	// Run migrations to make DB ready
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("failed to migrate: %v", err)
	}

	// Check readiness
	if err := db.IsReady(ctx); err != nil {
		t.Errorf("expected database to be ready: %v", err)
	}
}

// TestIntegrationStats tests connection pool statistics.
func TestIntegrationStats(t *testing.T) {
	cfg := Config{
		Host:           getEnvOrDefault("POSTGRES_HOST", "localhost"),
		Port:           integrationPort(),
		Database:       getEnvOrDefault("POSTGRES_DB", integrationDBName),
		User:           getEnvOrDefault("POSTGRES_USER", "vornik"),
		Password:       getEnvOrDefault("POSTGRES_PASSWORD", "vornik"),
		SSLMode:        "disable",
		MaxOpenConns:   10,
		MaxIdleConns:   2,
		ConnectTimeout: 10 * time.Second,
	}

	ctx := context.Background()
	db, err := Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer db.Close()

	stats := db.Stats()
	if stats == nil {
		t.Fatal("expected non-nil stats")
	}

	// Basic sanity checks
	if stats.MaxOpenConnections != 10 {
		t.Errorf("expected MaxOpenConnections 10, got %d", stats.MaxOpenConnections)
	}
}
