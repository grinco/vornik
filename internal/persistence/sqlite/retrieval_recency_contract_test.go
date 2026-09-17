package sqlite_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/persistence/repotest"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// The SQLite half of the retrieval-recency contract
// (2026-09-16-retrieval-recency-design.md §7). Its Postgres twin is
// internal/persistence/postgres/retrieval_recency_contract_integration_test.go,
// and both delegate to the same suite: a result that differs between them is
// the finding.
//
// This lane is the one that runs on every `go test ./...`, which is exactly
// why it must not be the only one. The predicate is SQL, and the two drivers
// disagree about enough of it — boolean vs 0/1, timestamptz vs TEXT, row-value
// support — that "it works" on one lane is not a claim about the other.

// recencyHarnessSQLite wires the shared suite to an in-memory database.
func recencyHarnessSQLite(t *testing.T) repotest.RecencyHarness {
	t.Helper()
	db := newTestDB(t)
	return repotest.RecencyHarness{
		Driver: "sqlite",
		DB:     db.DB,
		Arg:    func(int) string { return "?" },
		Seed:   seedRecencyChunkSQLite(db),
		Rerank: rerankThroughMemory,
		Explain: func(ctx context.Context, query string, args ...any) (string, error) {
			return explainQueryPlanSQLite(ctx, db, query, args...)
		},
		// sqlite decomposes the row-value tuple: the created_at range goes
		// into the index and the id tiebreak is resolved above it. The design
		// measured "SEARCH n USING COVERING INDEX idx (project_id=? AND
		// series_key=? AND created_at>?)"; the observed plan is the same minus
		// COVERING, because this suite's SELECT reads artifact_id and
		// source_name, which the partial index does not carry. The push-down
		// — the part the decision rested on — is the "created_at>" term.
		PlanPushdown: "created_at>",
	}
}

// rerankThroughMemory adapts repotest.RecencyCandidate to the shipped re-rank.
//
// The adapter lives here, on the lane, rather than in repotest, because
// internal/memory's own tests import repotest — so repotest importing
// internal/memory is an import cycle, not a preference. That constraint is why
// the suite owns the SCENARIOS and each lane owns the two translations it
// needs (rows in, results out), exactly as each lane already owns its seeder.
//
// It is a pure data shuffle with no arithmetic: every number the assertions
// read is produced by memory.ApplyRecencyForTest, which is a pass-through to
// the shipped applyRecency. Nothing here can make a failing re-rank look green.
func rerankThroughMemory(in []repotest.RecencyCandidate, enabled bool, now time.Time, limit int) []repotest.RecencyCandidate {
	results := make([]memory.SearchResult, 0, len(in))
	for _, c := range in {
		results = append(results, memory.SearchResult{
			ChunkID:          c.ChunkID,
			Score:            c.Score,
			CreatedAt:        c.CreatedAt,
			ExpiresAt:        c.ExpiresAt,
			SeriesSuperseded: c.SeriesSuperseded,
		})
	}
	cfg := memory.DefaultRecencyConfig()
	// SetEnabled, not the bare field: applyDefaults distinguishes "explicitly
	// set false" from "unset" and would otherwise restore the default true,
	// turning the kill-switch test into a test of the enabled path.
	cfg.SetEnabled(enabled)
	out := memory.ApplyRecencyForTest(results, cfg, now, limit)
	back := make([]repotest.RecencyCandidate, 0, len(out))
	for _, r := range out {
		back = append(back, repotest.RecencyCandidate{
			ChunkID:          r.ChunkID,
			Score:            r.Score,
			CreatedAt:        r.CreatedAt,
			ExpiresAt:        r.ExpiresAt,
			SeriesSuperseded: r.SeriesSuperseded,
		})
	}
	return back
}

// seedRecencyChunkSQLite is the SQLite half of repotest.RecencySeed.
//
// The timestamp layout is the load-bearing part. created_at is TEXT here, so
// the predicate's row-value comparison is a STRING comparison — and
// time.RFC3339Nano, which the older seedChunkForSuite uses, trims trailing
// zeros. Under that layout a whole-second time serialises with no fraction at
// all and sorts ABOVE every fractional time in the same second, because 'Z'
// outranks '.'. The suite's fixture is deliberately microseconds apart, so
// writing it with RFC3339Nano would order the rows differently here than on
// Postgres and this lane would pass while asserting the opposite. Hence the
// fixed-width repotest.RecencyTimeLayout.
func seedRecencyChunkSQLite(db *sqlite.DB) repotest.RecencySeed {
	return func(ctx context.Context, c repotest.RecencyChunk) error {
		var expires any
		if c.ExpiresAt != nil {
			expires = c.ExpiresAt.UTC().Format(repotest.RecencyTimeLayout)
		}
		_, err := db.ExecContext(ctx, `INSERT INTO project_memory_chunks
			(id, project_id, content, content_hash, created_at, expires_at,
			 series_key, artifact_id, source_name)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			c.ID, c.ProjectID, c.Content, "h-"+c.ID,
			c.CreatedAt.UTC().Format(repotest.RecencyTimeLayout), expires,
			nullIfEmpty(c.SeriesKey), nullIfEmpty(c.ArtifactID), nullIfEmpty(c.SourceName))
		return err
	}
}

// nullIfEmpty keeps "" out of the store. NULL and ” are the same to a
// careless writer and opposite to this design: the partial index and the
// predicate's CASE both key on IS NOT NULL, so an empty-string series_key
// would enrol the row in one enormous series that supersedes itself.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func explainQueryPlanSQLite(ctx context.Context, db *sqlite.DB, query string, args ...any) (string, error) {
	rows, err := db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()
	var b strings.Builder
	for rows.Next() {
		var id, parent, notused sql.NullInt64
		var detail sql.NullString
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			return "", err
		}
		b.WriteString(detail.String)
		b.WriteString("\n")
	}
	return b.String(), rows.Err()
}

// TestRetrievalRecency_Contract — the §7 suite on the sqlite lane.
func TestRetrievalRecency_Contract(t *testing.T) {
	repotest.RunRetrievalRecencySuite(t, recencyHarnessSQLite(t))
}
