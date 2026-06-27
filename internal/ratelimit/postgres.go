package ratelimit

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// PostgresProjectLimiter is the durable counterpart to the
// in-process Limiter — sub-item 5 of the rate-limit hardening
// track. Counters live in the ratelimit_counters table so two
// daemons sharing a project enforce one combined cap instead of
// 2× the configured rate.
//
// Storage shape: one row per (scope_kind, scope_key, window_start).
// scope_kind is the limiter dimension ("project" today; future:
// "api_key", "ip", "tool"). scope_key is the dimension id. The
// window_start column is the truncated bucket boundary — we keep
// two granularities (minute + hour) so Check can answer both
// caps with two parameterised queries.
//
// Concurrency: row-level UPSERT under
// INSERT … ON CONFLICT DO UPDATE serialises concurrent Record
// calls from N daemons against the same project. Check is a
// straight aggregate read; under heavy contention the read can
// race ahead of a concurrent Record (under-count by one). That
// matches the in-process backend's same-millisecond race and is
// acceptable: caps are defensive, an over-allowance of 1 task
// is harmless.
type PostgresProjectLimiter struct {
	db persistence.DBTX
}

// NewPostgresProjectLimiter constructs a postgres-backed limiter.
// Caller is responsible for running migration 42
// (create_ratelimit_counters) before any Check or Record call —
// the schema lives in internal/persistence/migrations.go.
func NewPostgresProjectLimiter(db persistence.DBTX) *PostgresProjectLimiter {
	return &PostgresProjectLimiter{db: db}
}

// Check answers whether one more task for p would exceed either
// configured cap. Runs two aggregate queries (minute, hour) under
// the same scope_kind. Caps of zero disable that dimension —
// matching the in-process limiter's contract.
//
// Background-context wrapper kept for legacy call sites that
// don't yet carry a request context. New callers should prefer
// CheckCtx so DB calls cancel with their upstream HTTP request.
func (l *PostgresProjectLimiter) Check(p *registry.Project, now time.Time) Decision {
	return l.CheckCtx(context.Background(), p, now)
}

// CheckCtx is the context-aware Check. Returns a zero Decision
// when the DB call fails — better to fail-open on a transient
// hiccup than to refuse every task and surface a 429 the
// operator can't explain. The DB error is silently swallowed
// here; operators see it via the postgres logs.
func (l *PostgresProjectLimiter) CheckCtx(ctx context.Context, p *registry.Project, now time.Time) Decision {
	if l == nil || p == nil {
		return Decision{}
	}
	if p.RateLimit.TasksPerMinute == 0 && p.RateLimit.TasksPerHour == 0 {
		return Decision{}
	}
	minuteCount, hourCount, err := l.countWindows(ctx, p.ID, now)
	if err != nil {
		// Fail-open. A transient DB hiccup shouldn't black-hole
		// the daemon's task-creation path. 2026-05-29 audit fix:
		// log the fail-open at warn level so operators see it in
		// the daemon's structured logs (previously the error was
		// silently swallowed, leaving no signal that ratelimit
		// caps were briefly bypassed during a DB outage).
		log.Warn().
			Err(err).
			Str("project_id", p.ID).
			Msg("ratelimit: postgres backend fail-open on count query (caps temporarily bypassed for this project)")
		return Decision{}
	}
	d := Decision{MinuteCount: minuteCount, HourCount: hourCount}
	if p.RateLimit.TasksPerMinute > 0 && minuteCount >= p.RateLimit.TasksPerMinute {
		d.Blocked = true
		d.Reason = "per-minute task rate limit reached"
		return d
	}
	if p.RateLimit.TasksPerHour > 0 && hourCount >= p.RateLimit.TasksPerHour {
		d.Blocked = true
		d.Reason = "per-hour task rate limit reached"
		return d
	}
	return d
}

// Record persists one event into the minute + hour buckets for
// the project. Two UPSERTs in sequence — the row's window_start
// is the truncated bucket boundary, so multiple events in the
// same minute/hour increment the same row.
//
// Background-context wrapper. Same fail-open contract: a DB
// hiccup logs (TODO: structured logger pass-through) and
// continues so the caller's task isn't refused on transient
// failure.
func (l *PostgresProjectLimiter) Record(projectID string, now time.Time) {
	l.RecordCtx(context.Background(), projectID, now)
}

// RecordCtx is the context-aware Record. See Record's comment
// for the fail-open contract.
func (l *PostgresProjectLimiter) RecordCtx(ctx context.Context, projectID string, now time.Time) {
	if l == nil || projectID == "" {
		return
	}
	minuteBucket := now.UTC().Truncate(time.Minute)
	hourBucket := now.UTC().Truncate(time.Hour)
	// Two writes in sequence. We DON'T wrap them in a transaction:
	// each row is independent (different window_start), and a
	// half-applied write under crash recovery is recoverable
	// (the missed dimension just under-counts for that single
	// event, which the next Record fixes).
	_, _ = l.db.ExecContext(ctx, upsertCounterSQL,
		scopeProject, projectID, minuteBucket)
	_, _ = l.db.ExecContext(ctx, upsertCounterSQL,
		scopeProjectHour, projectID, hourBucket)
}

// countWindows returns the trailing-minute + trailing-hour
// event counts for projectID. Two range aggregates against the
// indexed window_start column.
func (l *PostgresProjectLimiter) countWindows(ctx context.Context, projectID string, now time.Time) (int, int, error) {
	utc := now.UTC()
	minuteSince := utc.Add(-1 * time.Minute)
	hourSince := utc.Add(-1 * time.Hour)
	var minuteCount, hourCount int
	if err := l.db.QueryRowContext(ctx, countSumSQL,
		scopeProject, projectID, minuteSince,
	).Scan(&minuteCount); err != nil && err != sql.ErrNoRows {
		return 0, 0, fmt.Errorf("ratelimit: minute aggregate: %w", err)
	}
	if err := l.db.QueryRowContext(ctx, countSumSQL,
		scopeProjectHour, projectID, hourSince,
	).Scan(&hourCount); err != nil && err != sql.ErrNoRows {
		return 0, 0, fmt.Errorf("ratelimit: hour aggregate: %w", err)
	}
	return minuteCount, hourCount, nil
}

// SweepExpired deletes rows whose window_start is older than
// retention. Called by a periodic janitor (service container);
// keeps the table from growing unbounded. Returns the number of
// rows deleted for the operator log.
func (l *PostgresProjectLimiter) SweepExpired(ctx context.Context, retention time.Duration) (int64, error) {
	if l == nil || retention <= 0 {
		return 0, nil
	}
	cutoff := time.Now().UTC().Add(-retention)
	res, err := l.db.ExecContext(ctx, sweepCounterSQL, cutoff)
	if err != nil {
		return 0, fmt.Errorf("ratelimit: sweep: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// scopeProject / scopeProjectHour are the scope_kind constants
// stored in ratelimit_counters for per-project task-creation rows.
// The minute and hour granularities MUST use DISTINCT scope_kind:
// the count query aggregates by (scope_kind, scope_key, window_start
// range) with no separate granularity column, so if both granularities
// shared a scope_kind the hour aggregate would sum the minute buckets
// too (double-counting every event), and a minute-0-of-the-hour event
// — whose minute bucket and hour bucket truncate to the same
// window_start — would collide into one double-incremented row.
// (Regression: 2026-06-25 rate-limit double-count / horizontal-scaling
// flake.) Kept package-private so tests can assert the column values.
const (
	scopeProject     = "project"      // minute-granularity rows
	scopeProjectHour = "project:hour" // hour-granularity rows
)

// upsertCounterSQL increments the bucket's row, creating it on
// first hit. Using ON CONFLICT … DO UPDATE for atomic increment;
// no explicit row-level lock needed.
const upsertCounterSQL = `
INSERT INTO ratelimit_counters (scope_kind, scope_key, window_start, count)
VALUES ($1, $2, $3, 1)
ON CONFLICT (scope_kind, scope_key, window_start)
DO UPDATE SET count = ratelimit_counters.count + 1
`

// countSumSQL aggregates the per-bucket counts inside the
// caller-supplied window. COALESCE so an absent project returns
// zero rather than NULL.
const countSumSQL = `
SELECT COALESCE(SUM(count), 0)
FROM ratelimit_counters
WHERE scope_kind = $1
  AND scope_key  = $2
  AND window_start >= $3
`

// sweepCounterSQL deletes rows older than the retention cutoff.
// Driven by the periodic janitor; keeps the table size bounded.
const sweepCounterSQL = `
DELETE FROM ratelimit_counters
WHERE window_start < $1
`

// Compile-time check: postgres limiter satisfies both interfaces.
var (
	_ ProjectLimiter    = (*PostgresProjectLimiter)(nil)
	_ ProjectLimiterCtx = (*PostgresProjectLimiter)(nil)
)
