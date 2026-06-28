//go:build integration

package memory

// Integration coverage for the Tier-3 "memory ingest → recall" journey
// (https://docs.vornik.io) with project scoping, exercised end-to-end against a
// real PostgreSQL: content ingested for project A through the real
// Pipeline (gates → Indexer → project_memory_chunks) is recalled for A
// by the real Searcher, but is NEVER returned for project B. The
// fail-closed tenant gate (memoryfirewall.Evaluator under strict
// isolation) is pinned alongside as a pure-Go characterization so the
// "blank/blank-scope request reads nothing" invariant is asserted on the
// same real surface the recall path delegates to.
//
// SEAM CHOICE: the recall arm runs with NO embedding endpoint configured,
// so Searcher.embedQueryWithTimeout returns nil and the search degrades to
// the keyword (FTS) arm — fully deterministic, no embedding backend or
// pgvector vectors required. The ingest arm runs the real gate stack
// (RunStandardGates) + Indexer.IngestText, so project scoping is enforced
// by the production WHERE project_id = $1 clause, not a test shim.
//
// GATING: skips cleanly (never fails) when TEST_DATABASE_URL is unset, so
// the unit lane and CI hosts without a throwaway Postgres stay green. Run
// with the package's standard integration recipe, e.g.:
//
//	TEST_DATABASE_URL=postgres://vornik:vornik@localhost:5433/vornik_integration_test?sslmode=disable \
//	  go test -tags=integration ./internal/memory/... -run TestIntegration_IngestRecall -race -count=1
//
// SAFETY: refuses to run against any live daemon database — "vornik"
// (default) or "vornik_test" — unless
// VORNIK_TEST_ALLOW_DAEMON=1 is set (see isProtectedDaemonDB). Each subtest
// uses unique, random-suffixed project IDs and deletes only its own rows on
// cleanup, so it never touches pre-existing data.

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/memoryfirewall"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/postgres"
)

var (
	ingestRecallMigrateOnce sync.Once
	ingestRecallMigrateErr  error
)

// openIngestRecallDB returns a live *sql.DB pointed at the integration
// Postgres described by TEST_DATABASE_URL, migrating the full schema
// exactly once per test process. It SKIPS (never fails) when the env is
// unset or the database is unreachable so the suite is green on hosts
// without the throwaway container — the package's established
// integration posture (see internal/cli/db_integration_harness_cov_test.go
// and internal/persistence/postgres/integration_helpers.go).
func openIngestRecallDB(t *testing.T) *sql.DB {
	t.Helper()

	rawURL := os.Getenv("TEST_DATABASE_URL")
	if rawURL == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping memory ingest→recall integration test")
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	dbName := strings.TrimPrefix(u.Path, "/")
	// Never run destructive per-test cleanup against a live daemon DB unless
	// the operator explicitly opts in. isProtectedDaemonDB shields all of
	// them — "vornik" (default) and "vornik_test" (see
	// integration_guard_test.go for why the old vornik_test-only check was a gap).
	if isProtectedDaemonDB(dbName) && os.Getenv("VORNIK_TEST_ALLOW_DAEMON") != "1" {
		t.Skipf("TEST_DATABASE_URL points at a live daemon DB (%q); refusing to run. Set VORNIK_TEST_ALLOW_DAEMON=1 to override.", dbName)
	}

	db, err := sql.Open("postgres", rawURL)
	if err != nil {
		t.Skipf("postgres open failed, skipping: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Skipf("postgres unreachable, skipping: %v", err)
	}

	ingestRecallMigrateOnce.Do(func() {
		pgCfg, cfgErr := pgConfigFromURL(u)
		if cfgErr != nil {
			ingestRecallMigrateErr = cfgErr
			return
		}
		mctx, mcancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer mcancel()
		pg, connErr := postgres.Connect(mctx, pgCfg)
		if connErr != nil {
			ingestRecallMigrateErr = connErr
			return
		}
		defer func() { _ = pg.Close() }()
		ingestRecallMigrateErr = pg.Migrate(mctx)
	})
	if ingestRecallMigrateErr != nil {
		_ = db.Close()
		t.Fatalf("schema migration failed: %v", ingestRecallMigrateErr)
	}

	t.Cleanup(func() { _ = db.Close() })
	return db
}

// pgConfigFromURL maps a postgres:// URL onto the postgres.Config the
// MigrationRunner needs. Defaults match the integration container.
func pgConfigFromURL(u *url.URL) (postgres.Config, error) {
	cfg := postgres.Config{
		Host:           u.Hostname(),
		Database:       strings.TrimPrefix(u.Path, "/"),
		SSLMode:        "disable",
		ConnectTimeout: 10 * time.Second,
	}
	if cfg.Host == "" {
		cfg.Host = "localhost"
	}
	cfg.Port = 5432
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return postgres.Config{}, err
		}
		cfg.Port = n
	}
	if u.User != nil {
		cfg.User = u.User.Username()
		if pw, ok := u.User.Password(); ok {
			cfg.Password = pw
		}
	}
	if mode := u.Query().Get("sslmode"); mode != "" {
		cfg.SSLMode = mode
	}
	return cfg, nil
}

// newIngestRecallStack wires a real Pipeline (gates → Indexer) and a
// real Searcher over the same Repository, with NO embedding endpoint so
// recall runs deterministically on the keyword (FTS) arm.
func newIngestRecallStack(db *sql.DB) (*Pipeline, *Searcher) {
	repo := NewRepository(db)
	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.EmbeddingEndpoint = "" // force keyword-only recall (deterministic)

	indexer := NewIndexer(cfg, repo, nil, zerolog.Nop())
	pipeline := NewPipeline(indexer, PipelineConfig{
		// Quarantine repo intentionally nil: the test only feeds
		// gate-passing content, so the quarantine branch is never hit.
		Logger: zerolog.Nop(),
	})
	searcher := NewSearcher(cfg, repo, nil)
	return pipeline, searcher
}

// seedArtifact inserts a minimal artifacts row so the chunk's
// artifact_id FK (project_memory_chunks_artifact_id_fkey) is satisfied
// and the SchemaMatchGate's source_artifact_id-required check passes.
// project_id carries no FK, so no projects/tasks rows are needed.
func seedArtifact(t *testing.T, db *sql.DB, projectID, artifactID, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := db.ExecContext(ctx, `
INSERT INTO artifacts (id, project_id, name, artifact_class, storage_path, size_bytes)
VALUES ($1, $2, $3, 'OUTPUT', $4, 0)
ON CONFLICT (id) DO NOTHING`,
		artifactID, projectID, name, "memory://"+artifactID)
	if err != nil {
		t.Fatalf("seed artifacts row %s: %v", artifactID, err)
	}
}

// ingestForProject drives one artifact through the REAL pipeline
// (gates → store). It first seeds the backing artifacts row so the
// SchemaMatchGate (source_artifact_id required) passes and the chunk's
// artifact_id FK resolves — matching the production agent-ingest path.
func ingestForProject(t *testing.T, db *sql.DB, p *Pipeline, projectID, sourceName, content string) (IngestStats, string) {
	t.Helper()
	artifactID := persistence.GenerateID("itest-art")
	seedArtifact(t, db, projectID, artifactID, sourceName)
	stats, err := p.IngestArtifact(
		context.Background(),
		projectID,
		"",         // taskID (NULL — no tasks row needed)
		artifactID, // artifactID (real artifacts row seeded above)
		sourceName, // sourceName
		"analyst",  // producerRole → ClassifyByRole; non-empty so the gate passes
		"",         // ingestExecutionID
		content,
		int64(len(content)),
		"", // epochID (legacy/NULL — searchable immediately)
	)
	if err != nil {
		t.Fatalf("IngestArtifact(%s): %v", projectID, err)
	}
	return stats, artifactID
}

// TestIntegration_IngestRecall_ProjectScoping is the Tier-3 journey:
// ingest distinctive content for project A through the real pipeline,
// then recall it for A but assert project B (which never received it)
// gets nothing — the production project_id scoping in the search SQL.
func TestIntegration_IngestRecall_ProjectScoping(t *testing.T) {
	db := openIngestRecallDB(t)
	pipeline, searcher := newIngestRecallStack(db)
	ctx := context.Background()

	// Unique project IDs so the test never collides with existing data.
	projectA := persistence.GenerateID("itest-proj-a")
	projectB := persistence.GenerateID("itest-proj-b")
	t.Cleanup(func() { cleanupProjects(t, db, projectA, projectB) })

	// A distinctive, gate-passing token string (>= 64 chars, >= 10 words)
	// so the FTS keyword arm can match it unambiguously.
	const marker = "zarquon"
	content := "The " + marker + " telemetry subsystem records flux capacitor " +
		"readings across every sharded ingest worker pipeline node continuously."

	// --- Ingest into project A only -------------------------------------
	stats, _ := ingestForProject(t, db, pipeline, projectA, "notes.md", content)
	if stats.Admitted == 0 {
		t.Fatalf("expected content admitted by gates, got stats=%+v (gates failed: %v)", stats, stats.GatesFailed)
	}

	// --- Recall for A returns it ----------------------------------------
	resA, err := searcher.Search(ctx, projectA, marker, 10)
	if err != nil {
		t.Fatalf("Search(projectA): %v", err)
	}
	if len(resA) == 0 {
		t.Fatalf("recall for ingesting project A returned 0 hits; ingest→recall journey broken")
	}
	foundA := false
	for _, r := range resA {
		if r.ProjectID != projectA {
			t.Errorf("SECURITY: project A recall returned a chunk scoped to %q, want %q", r.ProjectID, projectA)
		}
		if strings.Contains(r.Content, marker) {
			foundA = true
		}
	}
	if !foundA {
		t.Fatalf("recall for project A did not return the ingested %q content; got %d hits", marker, len(resA))
	}

	// Multi-term recall regression (2026-06-28): a plain query where
	// one distinctive term matches and one term does not must still
	// retrieve through the relaxed OR keyword query. The strict
	// websearch query alone would AND the terms and return nothing.
	resRelaxed, err := searcher.Search(ctx, projectA, marker+" absentterm", 10)
	if err != nil {
		t.Fatalf("Search(projectA relaxed multi-term): %v", err)
	}
	foundRelaxed := false
	for _, r := range resRelaxed {
		if r.ProjectID != projectA {
			t.Errorf("SECURITY: relaxed recall returned a chunk scoped to %q, want %q", r.ProjectID, projectA)
		}
		if strings.Contains(r.Content, marker) {
			foundRelaxed = true
		}
	}
	if !foundRelaxed {
		t.Fatalf("relaxed multi-term recall did not return the ingested %q content; got %d hits", marker, len(resRelaxed))
	}

	// Explicit web-search syntax stays strict: quoted phrases are
	// passed through unchanged and handled by Postgres' parser.
	resPhrase, err := searcher.Search(ctx, projectA, `"flux capacitor"`, 10)
	if err != nil {
		t.Fatalf("Search(projectA quoted phrase): %v", err)
	}
	if len(resPhrase) == 0 {
		t.Fatal("quoted phrase recall returned 0 hits; websearch_to_tsquery phrase handling broken")
	}

	// Stopword-heavy natural text should still reach the distinctive
	// term; Postgres drops stopwords while the relaxed OR query keeps
	// non-stopword anchors available.
	resStopword, err := searcher.Search(ctx, projectA, "the "+marker+" and absentterm", 10)
	if err != nil {
		t.Fatalf("Search(projectA stopword-heavy): %v", err)
	}
	if len(resStopword) == 0 {
		t.Fatal("stopword-heavy relaxed recall returned 0 hits")
	}

	// --- Recall for B (never ingested) returns NOTHING ------------------
	// This is the cross-tenant/cross-project isolation assertion. A
	// non-empty result here is a real cross-project leak.
	resB, err := searcher.Search(ctx, projectB, marker, 10)
	if err != nil {
		t.Fatalf("Search(projectB): %v", err)
	}
	if len(resB) != 0 {
		t.Fatalf("SECURITY (cross-project leak): project B recalled %d hit(s) for content only ingested into project A: %+v", len(resB), resB)
	}
}

// TestIntegration_IngestRecall_TenantGateFailsClosed pins the tenant
// gate's fail-closed posture on the SAME real surface the recall path
// delegates to (Searcher.applyFirewall → memoryfirewall.Evaluator.Decide).
// Pure-Go + deterministic, so it runs even without the DB — but it lives
// here (under the integration tag) so the full ingest→recall→isolation
// story reads as one suite. Under strict tenant isolation a request with
// a blank/empty tenant scope must read NOTHING (it would otherwise sweep
// every untagged/legacy chunk). The DB-backed scoping above and this gate
// together form the isolation contract.
func TestIntegration_IngestRecall_TenantGateFailsClosed(t *testing.T) {
	// Reachable without a DB; still skip-clean under the integration tag's
	// expectations by not requiring any external resource.
	eStrict := memoryfirewall.NewEvaluator().WithStrictTenantIsolation(true)

	// Maximally permissive chunk in every OTHER dimension: only the
	// blank-scope tenant gate may block it.
	publicChunk := memoryfirewall.Chunk{
		Policy: memoryfirewall.Policy{Sensitivity: memoryfirewall.SensitivityPublic},
	}

	// Blank/empty request scope under strict isolation → must block.
	dec, reason := eStrict.Decide(publicChunk, memoryfirewall.RequestContext{})
	if dec != memoryfirewall.DecisionBlockTenantMismatch {
		t.Fatalf("SECURITY: strict tenant gate did not fail closed on a blank scope; got decision=%v reason=%q", dec, reason)
	}
	if !strings.Contains(reason, "strict tenant isolation") {
		t.Errorf("expected fail-closed reason to mention strict tenant isolation, got %q", reason)
	}

	// A tagged chunk reached by a blank request must block regardless of
	// the strict flag (cross-tenant leak vector) — assert on the default
	// (non-strict) evaluator so the property is shown to be intrinsic.
	eDefault := memoryfirewall.NewEvaluator()
	taggedChunk := memoryfirewall.Chunk{Policy: memoryfirewall.Policy{TenantID: "tenant-a"}}
	dec, _ = eDefault.Decide(taggedChunk, memoryfirewall.RequestContext{})
	if dec != memoryfirewall.DecisionBlockTenantMismatch {
		t.Fatalf("SECURITY: tagged chunk served to a blank-scope request (cross-tenant leak); got decision=%v", dec)
	}

	// Sanity: the legacy single-tenant blank-vs-blank match still allows
	// under the default evaluator (so the gate isn't trivially deny-all).
	if dec, _ := eDefault.Decide(memoryfirewall.Chunk{}, memoryfirewall.RequestContext{}); dec != memoryfirewall.DecisionAllow {
		t.Fatalf("default evaluator should allow blank-vs-blank (legacy single-tenant), got %v", dec)
	}
}

// cleanupProjects deletes only the rows this test created. Scoped by the
// random project IDs so it can never affect pre-existing data.
func cleanupProjects(t *testing.T, db *sql.DB, projectIDs ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, pid := range projectIDs {
		// Chunks first (their artifact_id FK is ON DELETE SET NULL, but
		// deleting them explicitly keeps the row count assertions sound),
		// then the seeded artifacts rows.
		if _, err := db.ExecContext(ctx, `DELETE FROM project_memory_chunks WHERE project_id = $1`, pid); err != nil {
			t.Logf("cleanup chunks project %s: %v", pid, err)
		}
		if _, err := db.ExecContext(ctx, `DELETE FROM artifacts WHERE project_id = $1`, pid); err != nil {
			t.Logf("cleanup artifacts project %s: %v", pid, err)
		}
	}
}
