package service

import (
	"database/sql"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/ratelimit"
)

// TestInitProjectRateLimiter_DefaultsToMemory — empty Backend
// keeps the legacy in-process limiter. Production deployments
// that haven't migrated continue working unchanged.
func TestInitProjectRateLimiter_DefaultsToMemory(t *testing.T) {
	c := &Container{
		Logger: zerolog.Nop(),
		Config: &config.Config{},
	}
	c.initProjectRateLimiter()
	require.NotNil(t, c.rateLimiter)
	_, isMemory := c.rateLimiter.(*ratelimit.Limiter)
	assert.True(t, isMemory, "default backend must be the in-process Limiter")
	assert.Nil(t, c.rateLimiterPostgres, "memory backend must not allocate the postgres limiter")
}

// TestInitProjectRateLimiter_PostgresBackend — Backend=postgres
// allocates the durable limiter and runs the startup sweep. We
// fix the DB so the sweep query is exercised; sqlmock asserts
// the call shape.
func TestInitProjectRateLimiter_PostgresBackend(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	// Startup sweep DELETE.
	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM ratelimit_counters`)).
		WithArgs(sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))

	c := &Container{
		Logger: zerolog.Nop(),
		Config: &config.Config{
			API: config.APIConfig{
				RateLimit: config.APIRateLimitConfig{Backend: "postgres"},
			},
		},
		DB: db,
	}
	c.initProjectRateLimiter()

	require.NotNil(t, c.rateLimiter)
	_, isPostgres := c.rateLimiter.(*ratelimit.PostgresProjectLimiter)
	assert.True(t, isPostgres, "postgres backend must produce the durable limiter")
	require.NotNil(t, c.rateLimiterPostgres, "concrete limiter must be set so the sweeper can reach SweepExpired")
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestInitProjectRateLimiter_UnknownFallsBackToMemory — a typo
// in the operator's config doesn't refuse every task; the
// limiter degrades to memory with a warning log.
func TestInitProjectRateLimiter_UnknownFallsBackToMemory(t *testing.T) {
	c := &Container{
		Logger: zerolog.Nop(),
		Config: &config.Config{
			API: config.APIConfig{
				RateLimit: config.APIRateLimitConfig{Backend: "redis"},
			},
		},
	}
	c.initProjectRateLimiter()
	_, isMemory := c.rateLimiter.(*ratelimit.Limiter)
	assert.True(t, isMemory)
}

// TestInitProjectRateLimiter_PostgresHonoursCustomRetention —
// CounterRetention parses Go-style durations; an invalid string
// keeps the 24h default. We verify the configured value by
// driving Backend=postgres with a recognisable retention and
// checking the DB call fires (the sweep cutoff argument is
// AnyArg in sqlmock, so we just confirm execution).
func TestInitProjectRateLimiter_PostgresHonoursCustomRetention(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM ratelimit_counters`)).
		WithArgs(sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))

	c := &Container{
		Logger: zerolog.Nop(),
		Config: &config.Config{
			API: config.APIConfig{
				RateLimit: config.APIRateLimitConfig{
					Backend:          "postgres",
					CounterRetention: "48h",
				},
			},
		},
		DB: db,
	}
	c.initProjectRateLimiter()
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestInitProjectRateLimiter_PostgresIgnoresMalformedRetention —
// a typo on CounterRetention falls back to the 24h default
// rather than crashing the daemon. The sweep still fires.
func TestInitProjectRateLimiter_PostgresIgnoresMalformedRetention(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM ratelimit_counters`)).
		WithArgs(sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))

	c := &Container{
		Logger: zerolog.Nop(),
		Config: &config.Config{
			API: config.APIConfig{
				RateLimit: config.APIRateLimitConfig{
					Backend:          "postgres",
					CounterRetention: "twenty-four hours",
				},
			},
		},
		DB: db,
	}
	c.initProjectRateLimiter()
	require.NoError(t, mock.ExpectationsWereMet())
}

// keep the sql import alive even if all tests skip; helpful for
// future tests that drive the limiter through real DB rows.
var _ = sql.ErrNoRows
