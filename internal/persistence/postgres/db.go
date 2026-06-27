// Package postgres provides PostgreSQL-specific database connection and configuration.
package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	// Import lib/pq driver for PostgreSQL
	_ "github.com/lib/pq"

	"vornik.io/vornik/internal/persistence"
)

// DBTX is the canonical database abstraction defined in the
// persistence package. Repositories in this package keep referring to
// the unqualified `DBTX` identifier via this type alias so the existing
// constructors don't need a sweeping rename when the type moves.
type DBTX = persistence.DBTX

// Config holds PostgreSQL connection configuration.
type Config struct {
	// Host is the PostgreSQL server hostname.
	Host string

	// Port is the PostgreSQL server port.
	Port int

	// Database is the name of the database to connect to.
	Database string

	// User is the database user.
	User string

	// Password is the database password.
	Password string

	// SSLMode controls SSL/TLS connection settings.
	// Valid values: disable, require, verify-ca, verify-full
	SSLMode string

	// MaxOpenConns sets the maximum number of open connections.
	MaxOpenConns int

	// MaxIdleConns sets the maximum number of idle connections.
	MaxIdleConns int

	// ConnMaxLifetime sets the maximum lifetime of a connection.
	ConnMaxLifetime time.Duration

	// ConnMaxIdleTime sets the maximum idle time for a connection.
	ConnMaxIdleTime time.Duration

	// ConnectTimeout is the timeout for establishing connections.
	ConnectTimeout time.Duration
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		Host:            "localhost",
		Port:            5432,
		Database:        "vornik",
		User:            "vornik",
		SSLMode:         "disable",
		MaxOpenConns:    25,
		MaxIdleConns:    5,
		ConnMaxLifetime: 5 * time.Minute,
		ConnMaxIdleTime: 2 * time.Minute,
		ConnectTimeout:  10 * time.Second,
	}
}

// DSN returns the PostgreSQL connection string.
func (c Config) DSN() string {
	return fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		escapeDSNValue(c.Host),
		c.Port,
		escapeDSNValue(c.User),
		escapeDSNValue(c.Password),
		escapeDSNValue(c.Database),
		escapeDSNValue(c.SSLMode),
	)
}

func escapeDSNValue(value string) string {
	if value == "" {
		return "''"
	}
	if !strings.ContainsAny(value, " \t\r\n'\\") {
		return value
	}
	escaped := strings.ReplaceAll(value, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `'`, `\'`)
	return "'" + escaped + "'"
}

// DB wraps the PostgreSQL database connection with migration support.
type DB struct {
	*sql.DB
	config Config
}

// Connect opens a connection to PostgreSQL and verifies connectivity.
func Connect(ctx context.Context, cfg Config) (*DB, error) {
	db, err := sql.Open("postgres", cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Configure connection pool
	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	db.SetConnMaxIdleTime(cfg.ConnMaxIdleTime)

	// Verify connectivity with timeout
	pingCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()

	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	return &DB{DB: db, config: cfg}, nil
}

// Migrate runs all pending migrations.
func (db *DB) Migrate(ctx context.Context) error {
	runner := persistence.NewMigrationRunner(db.DB)
	return runner.Run(ctx)
}

// MigrationRunner returns a migration runner for this database.
func (db *DB) MigrationRunner() *persistence.MigrationRunner {
	return persistence.NewMigrationRunner(db.DB)
}

// IsReady checks if the database is connected and migrations are applied.
func (db *DB) IsReady(ctx context.Context) error {
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("database not connected: %w", err)
	}

	runner := db.MigrationRunner()
	status, err := runner.Status(ctx)
	if err != nil {
		return fmt.Errorf("failed to check migration status: %w", err)
	}

	if len(status.Pending) > 0 {
		return fmt.Errorf("database has %d pending migrations", len(status.Pending))
	}

	return nil
}

// Close closes the database connection.
func (db *DB) Close() error {
	return db.DB.Close()
}

// Stats returns database connection pool statistics.
func (db *DB) Stats() *sql.DBStats {
	stats := db.DB.Stats()
	return &stats
}
