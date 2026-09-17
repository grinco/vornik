//go:build integration

package postgres

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/persistence/repotest"
)

// The Postgres half of the retrieval-recency contract
// (2026-09-16-retrieval-recency-design.md §7) — the lane a production
// deployment actually runs, and the one `go test ./...` never touches.
//
// Same suite as internal/persistence/sqlite/retrieval_recency_contract_test.go.
// The two drivers implement the predicate's ingredients differently enough
// that agreement is worth asserting rather than assuming: EXISTS is a boolean
// here and a 0/1 integer there, created_at is timestamptz here and sortable
// TEXT there, and the row-value comparison (created_at, id) > (…) is pushed
// into the index condition here but decomposed into an indexed range plus a
// tiebreak there. A disagreement is the finding.
//
// Run with: make test-integration, or
// go test -tags=integration ./internal/persistence/postgres/... -run RetrievalRecency

func recencyHarnessPostgres(t *testing.T) repotest.RecencyHarness {
	t.Helper()
	db := newIntegrationDB(t)
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return repotest.RecencyHarness{
		Driver: "postgres",
		DB:     db.DB,
		Arg:    func(n int) string { return "$" + strconv.Itoa(n) },
		Seed:   seedRecencyChunkPostgres(db),
		Rerank: rerankThroughMemory,
		Explain: func(ctx context.Context, q string, args ...any) (string, error) {
			return explainPostgres(ctx, db, q, args...)
		},
	}
}

// rerankThroughMemory adapts repotest.RecencyCandidate to the shipped re-rank.
//
// Duplicated from the sqlite lane's copy on purpose, and the duplication is
// forced rather than chosen: internal/memory's own tests import repotest, so
// repotest cannot import internal/memory and the adapter cannot live in the
// shared suite. Each lane already owns its seeder for the same reason — the
// suite owns the scenarios, the lane owns the translation. There is no
// arithmetic here to diverge: both copies are a field-for-field shuffle around
// memory.ApplyRecencyForTest, which passes straight through to the shipped
// applyRecency.
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

// seedRecencyChunkPostgres is the Postgres half of repotest.RecencySeed.
//
// content_hash is derived from the id rather than the content: the table
// carries UNIQUE (project_id, content_hash), and the suite's fixtures
// deliberately repeat content across rows (365 digests all saying
// "headlines"), so hashing the content would collide on the second insert.
// source_name is NOT NULL here and absent from the sqlite slim schema — the
// kind of "same table, two column sets" difference that makes a single-lane
// contract test worthless.
func seedRecencyChunkPostgres(db *DB) repotest.RecencySeed {
	return func(ctx context.Context, c repotest.RecencyChunk) error {
		var artifactID, seriesKey any
		if c.ArtifactID != "" {
			artifactID = c.ArtifactID
		}
		if c.SeriesKey != "" {
			seriesKey = c.SeriesKey
		}
		var expires any
		if c.ExpiresAt != nil {
			expires = c.ExpiresAt.UTC()
		}
		// project_memory_chunks.artifact_id carries a FOREIGN KEY to
		// artifacts(id) on this driver, and the sqlite slim schema has no such
		// constraint — a divergence this suite discovered by failing on it
		// rather than by anyone reading the two schemas. The seeder therefore
		// materialises the artifact the chunk claims to belong to. It is not
		// decoration: the predicate compares COALESCE(artifact_id,
		// source_name), so a lane that quietly wrote NULL here to dodge the FK
		// would be testing the source_name fallback and never the column the
		// production bug was about.
		if artifactID != nil {
			if _, err := db.DB.ExecContext(ctx, `INSERT INTO artifacts
				(id, project_id, name, artifact_class, storage_path)
				VALUES ($1, $2, $3, 'OUTPUT', $4)
				ON CONFLICT (id) DO NOTHING`,
				c.ArtifactID, c.ProjectID, c.SourceName, "/dev/null/"+c.ArtifactID); err != nil {
				return err
			}
		}
		_, err := db.DB.ExecContext(ctx, `INSERT INTO project_memory_chunks
			(id, project_id, source_name, artifact_id, content, content_hash,
			 created_at, expires_at, series_key)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			c.ID, c.ProjectID, c.SourceName, artifactID, c.Content, "h-"+c.ID,
			c.CreatedAt.UTC(), expires, seriesKey)
		return err
	}
}

func explainPostgres(ctx context.Context, db *DB, query string, args ...any) (string, error) {
	rows, err := db.DB.QueryContext(ctx, "EXPLAIN "+query, args...)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()
	var b strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "", err
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String(), rows.Err()
}

// TestRetrievalRecency_PostgresContract — the §7 suite on the pgvector lane.
func TestRetrievalRecency_PostgresContract(t *testing.T) {
	repotest.RunRetrievalRecencySuite(t, recencyHarnessPostgres(t))
}
