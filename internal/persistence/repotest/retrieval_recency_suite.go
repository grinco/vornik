package repotest

// Retrieval recency — the cross-driver contract for the freshness/series
// re-rank (https://docs.vornik.io §7).
//
// WHY THIS IS A REPOTEST SUITE AND NOT A UNIT TEST. The incident was not a
// unit-level defect: the curve computed the right number, the store held the
// right rows, and the 11-day-old digest still won. The failure only exists
// where a flag DERIVED BY THE DATABASE meets the Go scorer, so that seam is
// where the test has to sit — and since the predicate is SQL, "it works" is a
// claim about a driver, not about the code. `go test ./...` is the sqlite
// lane; it has repeatedly stayed green while Postgres broke. A recency test
// that ran on one driver would be exactly the instrument that missed the bug.
//
// Both lanes point at this file: internal/persistence/sqlite and (behind
// -tags=integration) internal/persistence/postgres. A disagreement between
// them is the finding, not a reason to relax the suite.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// seriesSupersededPredicateSQL is the query-time supersession predicate,
// copied BYTE-FOR-BYTE out of the four hybrid SELECTs in
// internal/memory/repository.go — comments included, because the comments are
// what make the copy verifiable.
//
// It is a copy rather than a shared constant because the shipped SQL is built
// as one literal per search path and this slice is not licensed to refactor
// the search SQL. The copy is kept honest by the SuitePredicateMatchesShipped
// subtest below, which reads repository.go and fails if the two drift. Without
// that guard this suite would be testing a predicate nothing runs.
const seriesSupersededPredicateSQL = `       CASE WHEN c.series_key IS NOT NULL THEN EXISTS (
           SELECT 1 FROM project_memory_chunks n
            WHERE n.project_id = c.project_id
              AND n.series_key = c.series_key
              -- A series MEMBER is an artifact, not a chunk. One ingest
              -- produces many chunks whose created_at differ by microseconds,
              -- so without this clause every chunk of the CURRENT digest is
              -- superseded by its own later siblings and only the last one
              -- survives at full weight. Found in production, 2026-09-16.
              AND COALESCE(n.artifact_id, n.source_name) <> COALESCE(c.artifact_id, c.source_name)
              AND (n.created_at, n.id) > (c.created_at, c.id)
       ) ELSE false END AS series_superseded`

// seriesIndexName is the index the predicate must be served by. Named, not
// "some index": a future planner picking a different one is a regression the
// design explicitly wants to hear about (§7 test 10).
const seriesIndexName = "idx_memory_chunks_series"

// RecencyTimeLayout is the fixed-width UTC layout a text-column driver MUST
// write created_at/expires_at in.
//
// Not cosmetic, and the trap is subtle enough to have earned a constant:
// SQLite stores these as TEXT and the predicate's row-value comparison is
// therefore a STRING comparison. time.RFC3339Nano TRIMS trailing zeros, so
// ".220000" serialises as ".22" and a whole-second time serialises with no
// fraction at all — and "…:05Z" sorts ABOVE "…:05.999999Z" because 'Z' > '.'.
// A microsecond-apart fixture written with RFC3339Nano would thus order
// differently on the two drivers, and the suite would "pass" on sqlite while
// asserting the opposite of what Postgres does.
const RecencyTimeLayout = "2006-01-02T15:04:05.000000Z07:00"

// RecencyChunk is one seeded row. Only the columns the re-rank reads are
// here; everything else is the driver seeder's problem, because the two
// drivers' project_memory_chunks are genuinely different column sets (the
// sqlite one is a deliberately slim test schema).
type RecencyChunk struct {
	ID        string
	ProjectID string
	// ArtifactID is the member identity the predicate compares. Empty = NULL,
	// which is the pre-column case the COALESCE falls back from.
	ArtifactID string
	SourceName string
	// SeriesKey empty = NULL = "not part of a series", which is ~99% of the
	// store and the case that must never pay for the subquery.
	SeriesKey string
	Content   string
	CreatedAt time.Time
	// ExpiresAt nil = no TTL = the curve does not decay this chunk at all.
	ExpiresAt *time.Time
}

// RecencySeed inserts one row. Driver-supplied for the same reason SeedChunk
// is: required columns differ per backend.
type RecencySeed func(ctx context.Context, c RecencyChunk) error

// RecencyCandidate is one candidate as the ranker sees it: a base score from
// SQL, plus the three inputs the re-rank reads. It is repotest's own type, not
// internal/memory's, and that is a hard constraint rather than a style choice
// — internal/memory's own tests import repotest (miss_contract_test.go), so
// repotest naming a memory type is an import cycle that fails the whole
// package's build. The seam therefore points outward: repotest declares the
// shape, the caller adapts its SearchResult to it.
type RecencyCandidate struct {
	ChunkID string
	// Score is the fused RRF × (1 + utility) score as the SQL produced it,
	// BEFORE any recency factor.
	Score     float64
	CreatedAt time.Time
	// ExpiresAt nil = no TTL = no decay, whatever the age.
	ExpiresAt *time.Time
	// SeriesSuperseded is what the database's EXISTS said, not what the test
	// thinks it should be. That direction is the whole point of the suite.
	SeriesSuperseded bool
}

// RecencyRerank is the seam onto the shipped §5.5 re-rank: rescore by
// freshness^weight × seriesWeight, re-sort, THEN cut to limit.
//
// A function rather than an interface, matching this package's existing
// SeedChunk idiom, and deliberately narrow: `enabled` is the only knob,
// because the only config question the cross-driver contract asks is whether
// the kill switch is a true revert. Every other knob has unit coverage in
// internal/memory, where it belongs — those are properties of arithmetic, and
// arithmetic does not vary by driver.
//
// The adapter that satisfies this lives on each lane next to that lane's
// seeder, for the same reason the seeder does.
type RecencyRerank func(in []RecencyCandidate, enabled bool, now time.Time, limit int) []RecencyCandidate

// RecencyHarness is the per-driver wiring the shared suite runs on.
type RecencyHarness struct {
	// Driver names the lane in failure messages, so a cross-driver
	// disagreement is readable from the output alone.
	Driver string
	DB     *sql.DB
	// Arg renders the n-th bind placeholder ("$1" on Postgres, "?" on
	// sqlite). The suite builds the surrounding SQL once so both lanes
	// execute the same predicate text.
	Arg  func(n int) string
	Seed RecencySeed
	// Rerank drives the shipped re-rank. Supplied per lane because repotest
	// cannot name internal/memory's types (see RecencyCandidate).
	Rerank RecencyRerank
	// Explain returns the driver's plan for a query as text.
	Explain func(ctx context.Context, query string, args ...any) (string, error)
	// PlanPushdown is the driver's own spelling of "the (created_at, id)
	// comparison went INTO the index", as it appears in that driver's plan
	// output. Per-lane because the two planners describe the same push-down
	// completely differently — sqlite decomposes the tuple into an indexed
	// created_at range, Postgres keeps the whole ROW(...) > ROW(...) in the
	// index condition. Naming a marker is what makes §7 test 10 assert what
	// it claims to: without it, an index merely APPEARING in the plan while
	// the comparison fell back to a filter would pass.
	PlanPushdown string
}

// recencyRow is what the store says about one candidate: the two ranking
// inputs the curve reads, plus the derived supersession flag.
type recencyRow struct {
	id         string
	createdAt  time.Time
	expiresAt  *time.Time
	superseded bool
}

// RunRetrievalRecencySuite is the §7 test plan, on whichever driver the
// harness wires.
func RunRetrievalRecencySuite(t *testing.T, h RecencyHarness) {
	t.Helper()
	t.Run("SuitePredicateMatchesShipped", func(t *testing.T) { recencyPredicateMatchesShipped(t) })
	t.Run("SameArtifactChunksDoNotSupersedeEachOther", func(t *testing.T) { recencySameArtifact(t, h) })
	t.Run("RecencyDemotesAStaleSeriesMember", func(t *testing.T) { recencyDemotesStaleMember(t, h) })
	t.Run("SpecsDoNotDecay", func(t *testing.T) { recencySpecsDoNotDecay(t, h) })
	t.Run("SupersededSeriesMemberStaysRetrievable", func(t *testing.T) { recencyStaysRetrievable(t, h) })
	t.Run("DisabledConfigRestoresExactPreChangeOrder", func(t *testing.T) { recencyDisabledIsATrueRevert(t, h) })
	t.Run("SeriesLookupPlanStaysIndexed", func(t *testing.T) { recencyPlanStaysIndexed(t, h) })
}

// ---------------------------------------------------------------- the store

// loadRecency runs the shipped predicate over one project and returns every
// candidate's ranking inputs, ordered by id.
//
// The SELECT list is deliberately the ranking inputs and nothing else: the
// half-life is expires_at - created_at read PER ROW (§5.1 as-built), so a
// driver that hands back a zero time where the column is NULL would silently
// give every never-expiring chunk an instant half-life. That is a scan bug
// only a real driver can have, which is the whole reason these values are
// read back rather than taken from the fixture.
func loadRecency(ctx context.Context, t *testing.T, h RecencyHarness, projectID string) map[string]recencyRow {
	t.Helper()
	query := "SELECT c.id, c.created_at, c.expires_at,\n" + seriesSupersededPredicateSQL + `
FROM project_memory_chunks c
WHERE c.project_id = ` + h.Arg(1) + `
ORDER BY c.id`
	rows, err := h.DB.QueryContext(ctx, query, projectID)
	if err != nil {
		t.Fatalf("[%s] series predicate query: %v\n%s", h.Driver, err, query)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]recencyRow{}
	for rows.Next() {
		var id string
		var created, expires, superseded any
		if err := rows.Scan(&id, &created, &expires, &superseded); err != nil {
			t.Fatalf("[%s] scan: %v", h.Driver, err)
		}
		ct, ok := recencyTime(t, h, created)
		if !ok {
			t.Fatalf("[%s] chunk %s has a NULL created_at; the fixture is broken", h.Driver, id)
		}
		r := recencyRow{id: id, superseded: recencyBool(t, h, superseded)}
		r.createdAt = ct
		if et, ok := recencyTime(t, h, expires); ok {
			r.expiresAt = &et
		}
		out[id] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("[%s] rows: %v", h.Driver, err)
	}
	return out
}

// recencyBool normalises the predicate's result. EXISTS is a real boolean on
// Postgres and a 0/1 integer on sqlite; a suite that assumed either one would
// compile and then fail on the other lane for a reason that has nothing to do
// with the property under test.
func recencyBool(t *testing.T, h RecencyHarness, v any) bool {
	t.Helper()
	switch x := v.(type) {
	case bool:
		return x
	case int64:
		return x != 0
	case int:
		return x != 0
	case nil:
		return false
	case []byte:
		return string(x) == "t" || string(x) == "true" || string(x) == "1"
	case string:
		return x == "t" || x == "true" || x == "1"
	default:
		t.Fatalf("[%s] series_superseded came back as %T (%v); teach recencyBool about it", h.Driver, v, v)
		return false
	}
}

// recencyTime normalises a timestamp column. timestamptz arrives as time.Time
// from lib/pq and as a TEXT string from the sqlite driver. Reports false for
// SQL NULL, which for expires_at means "no TTL" and must not collapse into the
// zero time.
func recencyTime(t *testing.T, h RecencyHarness, v any) (time.Time, bool) {
	t.Helper()
	switch x := v.(type) {
	case nil:
		return time.Time{}, false
	case time.Time:
		return x.UTC(), true
	case string:
		return parseRecencyTime(t, h, x)
	case []byte:
		return parseRecencyTime(t, h, string(x))
	default:
		t.Fatalf("[%s] timestamp came back as %T (%v); teach recencyTime about it", h.Driver, v, v)
		return time.Time{}, false
	}
}

func parseRecencyTime(t *testing.T, h RecencyHarness, s string) (time.Time, bool) {
	t.Helper()
	for _, layout := range []string{RecencyTimeLayout, time.RFC3339Nano, "2006-01-02 15:04:05.999999-07:00"} {
		if parsed, err := time.Parse(layout, s); err == nil {
			return parsed.UTC(), true
		}
	}
	t.Fatalf("[%s] cannot parse timestamp %q — the seeder must write %s", h.Driver, s, RecencyTimeLayout)
	return time.Time{}, false
}

// seedAll is a fixture convenience; a failure here is a broken test, not a
// finding, so it is fatal rather than an assertion.
func seedAll(ctx context.Context, t *testing.T, h RecencyHarness, chunks []RecencyChunk) {
	t.Helper()
	for _, c := range chunks {
		if err := h.Seed(ctx, c); err != nil {
			t.Fatalf("[%s] seed %s: %v", h.Driver, c.ID, err)
		}
	}
}

// result builds the SearchResult the re-rank sees: base score from the
// caller, ranking inputs from the STORE.
func (r recencyRow) result(baseScore float64) RecencyCandidate {
	return RecencyCandidate{
		ChunkID:          r.id,
		Score:            baseScore,
		CreatedAt:        r.createdAt,
		ExpiresAt:        r.expiresAt,
		SeriesSuperseded: r.superseded,
	}
}

func rankOf(rs []RecencyCandidate, id string) int {
	for i, r := range rs {
		if r.ChunkID == id {
			return i
		}
	}
	return -1
}

func scoreOf(rs []RecencyCandidate, id string) float64 {
	for _, r := range rs {
		if r.ChunkID == id {
			return r.Score
		}
	}
	return 0
}

// ---------------------------------------------------------------- the tests

// recencyPredicateMatchesShipped is the guard on the copy above. The suite can
// only prove something about production if the SQL it runs is production's
// SQL; a predicate that drifted would leave both lanes green while the shipped
// one regressed. Whitespace-normalised, because gofmt and the Go string
// literal disagree about indentation, and nothing else.
func recencyPredicateMatchesShipped(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Skip("cannot locate this file; source-level guard unavailable")
	}
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	shipped, err := os.ReadFile(filepath.Join(repoRoot, "internal", "memory", "repository.go"))
	if err != nil {
		t.Fatalf("read the shipped search SQL: %v", err)
	}
	norm := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	want := norm(seriesSupersededPredicateSQL)
	got := norm(string(shipped))
	if !strings.Contains(got, want) {
		t.Fatalf("the suite's supersession predicate is no longer the one internal/memory/repository.go "+
			"runs. Re-copy it, and check whether the change was meant to be tested:\n%s", seriesSupersededPredicateSQL)
	}
	// Four hybrid SELECTs carry it (HybridSearch's sibling paths plus the
	// temporal and epoch variants). If one loses it, that path silently stops
	// demoting stale series members and no ranking test would notice, because
	// the flag simply arrives false.
	if n := strings.Count(got, norm("AS series_superseded")); n != 4 {
		t.Errorf("series_superseded is projected by %d search paths, want 4 — a path that stopped "+
			"deriving it reports every member un-superseded, which reads exactly like 'nothing is stale'", n)
	}
}

// recencySameArtifact is THE headline regression: the production bug five
// design-review rounds missed.
//
// The reviewed predicate said "a newer member of the same series exists", and
// that reads correctly right up until you know that a MEMBER IS AN ARTIFACT,
// not a row. One ingest chunks a digest into many rows whose created_at differ
// by microseconds — the live measurement was .213946, .218082, .221539,
// .224749 inside a single digest, and those four values are the fixture below
// rather than round numbers, so this test fails on the code the operator
// actually ran. Chunk-to-chunk, every chunk of the CURRENT digest is
// superseded by its own later siblings: only the last one keeps full weight,
// the rest are multiplied by 0.25, and the digest the operator asked for drops
// out of the results. The fix is the COALESCE(artifact_id, source_name)
// inequality; delete it and this test fails on the newest artifact's first
// three chunks.
//
// Asserted in BOTH directions on purpose. "No chunk is superseded" would also
// pass a predicate that had simply stopped working, so the older artifact's
// chunks must all still be demoted — that is what separates the fix from a
// mute.
func recencySameArtifact(t *testing.T, h RecencyHarness) {
	ctx := context.Background()
	project := uniqueID("proj")
	const series = "czech-news"

	base := time.Date(2026, 9, 16, 6, 0, 0, 0, time.UTC)
	// The newest digest: one artifact, four chunks, microseconds apart.
	newest := []string{"213946", "218082", "221539", "224749"}
	var chunks []RecencyChunk
	var newestIDs, olderIDs []string
	for i, micros := range newest {
		id := fmt.Sprintf("%s-new-%d", project, i)
		newestIDs = append(newestIDs, id)
		chunks = append(chunks, RecencyChunk{
			ID: id, ProjectID: project, ArtifactID: project + "-artifact-today",
			SourceName: "czech-news-2026-09-16-afternoon.md", SeriesKey: series,
			Content:   "todays headlines",
			CreatedAt: base.Add(time.Duration(atoiMicros(t, micros)) * time.Microsecond),
		})
	}
	// An OLDER artifact in the same series, also multi-chunk. Its chunks are
	// the ones that must be demoted.
	older := base.AddDate(0, 0, -11)
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("%s-old-%d", project, i)
		olderIDs = append(olderIDs, id)
		chunks = append(chunks, RecencyChunk{
			ID: id, ProjectID: project, ArtifactID: project + "-artifact-11-days-ago",
			SourceName: "czech-news-2026-09-05-afternoon.md", SeriesKey: series,
			Content:   "stale headlines",
			CreatedAt: older.Add(time.Duration(100000+i*4136) * time.Microsecond),
		})
	}
	seedAll(ctx, t, h, chunks)

	got := loadRecency(ctx, t, h, project)
	for i, id := range newestIDs {
		row, ok := got[id]
		if !ok {
			t.Fatalf("[%s] chunk %s vanished from the candidate set", h.Driver, id)
		}
		if row.superseded {
			t.Errorf("[%s] chunk %d of the NEWEST artifact is marked superseded — it was superseded by "+
				"its own sibling %d microseconds later. This is the 2026-09-16 production bug: a series "+
				"member is an artifact, not a chunk, and without the COALESCE(artifact_id, source_name) "+
				"inequality the current digest is demoted to 0.25 and drops out of results.",
				h.Driver, i, atoiMicros(t, newest[len(newest)-1])-atoiMicros(t, newest[i]))
		}
	}
	for i, id := range olderIDs {
		row, ok := got[id]
		if !ok {
			t.Fatalf("[%s] chunk %s vanished from the candidate set", h.Driver, id)
		}
		if !row.superseded {
			t.Errorf("[%s] chunk %d of the 11-day-old artifact is NOT superseded, though a whole newer "+
				"artifact exists in series %q. A predicate that demotes nothing passes the half of this "+
				"test above for the wrong reason.", h.Driver, i, series)
		}
	}

	// The microsecond spacing has to have survived the round trip, or the
	// assertions above were made against timestamps the driver flattened —
	// which is precisely how a text-column driver can pass a test about
	// ordering while ordering nothing.
	if a, b := got[newestIDs[0]].createdAt, got[newestIDs[3]].createdAt; !a.Before(b) {
		t.Errorf("[%s] created_at lost sub-second resolution in the store: %s vs %s", h.Driver,
			a.Format(time.RFC3339Nano), b.Format(time.RFC3339Nano))
	}
}

func atoiMicros(t *testing.T, s string) int {
	t.Helper()
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		t.Fatalf("fixture micros %q: %v", s, err)
	}
	return n
}

// recencyDemotesAStaleSeriesMember is the incident itself, at the level it
// occurred (§7 test 1).
//
// 2026-09-16: an 11-day-old czech-news digest outranked the current one for
// "what's in the news" and was summarised to the operator as today's
// headlines. The feeds were healthy and the fresh digest was in the store —
// ranking simply had no reason to prefer it. So the fixture gives the STALE
// member the HIGHER base score (top of the RRF band, 2/61) and the fresh one
// the bottom (1/80): pre-change there is nothing that can reorder them, and
// the test fails. The series multiplier is 0.25 precisely because for this leg
// dominance IS the contract — a superseded digest must LOSE an undated query,
// not merely score a little less.
func recencyDemotesStaleMember(t *testing.T, h RecencyHarness) {
	ctx := context.Background()
	project := uniqueID("proj")
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	ttl := func(from time.Time, days int) *time.Time { e := from.AddDate(0, 0, days); return &e }

	staleAt := now.AddDate(0, 0, -11)
	stale := RecencyChunk{
		ID: project + "-stale", ProjectID: project, ArtifactID: project + "-digest-05",
		SourceName: "czech-news-2026-09-05.md", SeriesKey: "czech-news",
		Content: "11 day old headlines", CreatedAt: staleAt, ExpiresAt: ttl(staleAt, 90),
	}
	fresh := RecencyChunk{
		ID: project + "-fresh", ProjectID: project, ArtifactID: project + "-digest-16",
		SourceName: "czech-news-2026-09-16.md", SeriesKey: "czech-news",
		Content: "todays headlines", CreatedAt: now, ExpiresAt: ttl(now, 90),
	}
	seedAll(ctx, t, h, []RecencyChunk{stale, fresh})
	rows := loadRecency(ctx, t, h, project)

	// Top and bottom of the measured RRF band (§5.3a): rank 1 in both arms
	// against rank 20 in one. The stale member gets the better base score.
	const bandTop, bandBottom = 2.0 / 61.0, 1.0 / 80.0
	in := []RecencyCandidate{
		rows[stale.ID].result(bandTop),
		rows[fresh.ID].result(bandBottom),
	}
	if !rows[stale.ID].superseded {
		t.Fatalf("[%s] the store did not mark the 11-day-old member superseded; the ranking assertion "+
			"below would then be vacuous", h.Driver)
	}
	out := h.Rerank(in, true, now, 10)
	if rankOf(out, fresh.ID) != 0 {
		t.Errorf("[%s] the stale digest still wins: order %v. This is the 2026-09-16 incident — the "+
			"older member held a higher fused score and nothing in ranking could tell them apart by age.",
			h.Driver, idsOf(out))
	}
	if rankOf(out, stale.ID) != 1 {
		t.Errorf("[%s] the superseded member must rank second, not vanish: order %v", h.Driver, idsOf(out))
	}
}

func idsOf(rs []RecencyCandidate) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.ChunkID)
	}
	return out
}

// recencySpecsDoNotDecay guards 51.5% of the store (§7 test 2), and is the
// test that would catch the WORSE regression.
//
// Demoting a stale digest one place is a ranking annoyance. Burying the design
// record, the rulings and the specs under whatever was written this morning
// would make the corpus actively misleading, and it would happen silently. The
// property that prevents it is not a class table: it is that the half-life IS
// the chunk's own retention window, so a chunk with no expires_at has no
// half-life and the curve returns a flat 1.0 at any age.
//
// The NULL is read back THROUGH THE DRIVER rather than taken from the fixture,
// because "no TTL" and "the zero time" are the same bits to a careless scan
// and mean opposite things to the curve — a two-thousand-year-old chunk floors
// instantly.
//
// The aged-with-a-TTL control is what gives the test teeth: without it, a
// recency term that had been switched off entirely would pass.
func recencySpecsDoNotDecay(t *testing.T, h RecencyHarness) {
	ctx := context.Background()
	project := uniqueID("proj")
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	twoYearsAgo := now.AddDate(-2, 0, 0)
	ttl := twoYearsAgo.AddDate(0, 0, 90)

	spec := RecencyChunk{
		ID: project + "-a-spec", ProjectID: project, ArtifactID: project + "-lld-1",
		SourceName: "some-design.md", Content: "the design says", CreatedAt: twoYearsAgo,
		// No ExpiresAt: never-expiring, which is what a spec is.
	}
	control := RecencyChunk{
		ID: project + "-b-aged-ttl", ProjectID: project, ArtifactID: project + "-research-1",
		SourceName: "some-research.md", Content: "a dated finding", CreatedAt: twoYearsAgo,
		ExpiresAt: &ttl,
	}
	freshAt := now
	freshSummary := RecencyChunk{
		ID: project + "-c-fresh", ProjectID: project, ArtifactID: project + "-summary-1",
		SourceName: "todays-summary.md", Content: "this morning", CreatedAt: freshAt,
		ExpiresAt: func() *time.Time { e := freshAt.AddDate(0, 0, 7); return &e }(),
	}
	seedAll(ctx, t, h, []RecencyChunk{spec, control, freshSummary})
	rows := loadRecency(ctx, t, h, project)

	if rows[spec.ID].expiresAt != nil {
		t.Fatalf("[%s] a chunk seeded with no TTL read back with expires_at = %v; the never-decays "+
			"property is a property of that NULL", h.Driver, rows[spec.ID].expiresAt)
	}
	if rows[control.ID].expiresAt == nil {
		t.Fatalf("[%s] the aged control lost its TTL in the store; it would then look like a spec "+
			"and the control would prove nothing", h.Driver)
	}

	const equalBase = 1.0 / 61.0
	in := []RecencyCandidate{
		rows[spec.ID].result(equalBase),
		rows[control.ID].result(equalBase),
		rows[freshSummary.ID].result(equalBase),
	}
	out := h.Rerank(in, true, now, 10)

	if got := scoreOf(out, spec.ID); got != equalBase {
		t.Errorf("[%s] a two-year-old chunk with NO TTL was rescored %.9f from %.9f. It must not decay "+
			"at all: 51.5%% of the store is specs, rulings and designs, and ranking must never bury them "+
			"under this morning's notes.", h.Driver, got, equalBase)
	}
	if got := scoreOf(out, freshSummary.ID); got != equalBase {
		t.Errorf("[%s] a zero-age chunk was rescored %.9f from %.9f; freshness at age 0 is 1.0",
			h.Driver, got, equalBase)
	}
	if got := scoreOf(out, control.ID); got >= equalBase {
		t.Errorf("[%s] the two-year-old chunk WITH a 90-day TTL was not demoted (%.9f). Without this "+
			"control, a recency term that had been switched off entirely would pass the assertion above.",
			h.Driver, got)
	}
	if rankOf(out, control.ID) != 2 {
		t.Errorf("[%s] the aged, expiring chunk must fall below both un-decayed ones: order %v",
			h.Driver, idsOf(out))
	}
}

// recencyStaysRetrievable pins that demotion is DEMOTION, not exclusion
// (§7 test 3, undated half).
//
// seriesSupersededWeight is 0.25 and not 0 for one reason: a question only the
// old digest answers must still be answerable. So both halves are asserted —
// the store FLAGS the superseded member rather than filtering it out of the
// candidate set, and the re-rank keeps it in the returned slice with a
// non-zero score, merely below the current member.
//
// (The dated half of §7 test 3 — the older member WINNING a query bounded by
// fromDate/toDate through hybridSearchTemporal — is not here; see the suite
// note in the design's §7 as-built stamp for why.)
func recencyStaysRetrievable(t *testing.T, h RecencyHarness) {
	ctx := context.Background()
	project := uniqueID("proj")
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	oldAt := now.AddDate(0, 0, -11)
	exp := func(from time.Time) *time.Time { e := from.AddDate(0, 0, 90); return &e }

	old := RecencyChunk{
		ID: project + "-old", ProjectID: project, ArtifactID: project + "-digest-05",
		SourceName: "czech-news-2026-09-05.md", SeriesKey: "czech-news",
		Content: "the only mention of the bridge closure", CreatedAt: oldAt, ExpiresAt: exp(oldAt),
	}
	current := RecencyChunk{
		ID: project + "-current", ProjectID: project, ArtifactID: project + "-digest-16",
		SourceName: "czech-news-2026-09-16.md", SeriesKey: "czech-news",
		Content: "todays headlines", CreatedAt: now, ExpiresAt: exp(now),
	}
	seedAll(ctx, t, h, []RecencyChunk{old, current})
	rows := loadRecency(ctx, t, h, project)

	if len(rows) != 2 {
		t.Fatalf("[%s] the candidate set has %d rows, want 2 — supersession is a FLAG on the row, "+
			"never a filter that removes it", h.Driver, len(rows))
	}
	if !rows[old.ID].superseded {
		t.Fatalf("[%s] the older member is not flagged; the demotion assertion would be vacuous", h.Driver)
	}

	const equalBase = 1.0 / 61.0
	out := h.Rerank([]RecencyCandidate{
		rows[old.ID].result(equalBase),
		rows[current.ID].result(equalBase),
	}, true, now, 10)

	if rankOf(out, old.ID) < 0 {
		t.Fatalf("[%s] the superseded member was dropped from the results. The weight is 0.25 and not 0 "+
			"exactly so a question only the old digest answers can still surface it.", h.Driver)
	}
	if rankOf(out, old.ID) != 1 || rankOf(out, current.ID) != 0 {
		t.Errorf("[%s] demoted, not reordered away: want current first and old second, got %v",
			h.Driver, idsOf(out))
	}
	if s := scoreOf(out, old.ID); s <= 0 {
		t.Errorf("[%s] the superseded member scored %.9f; 0 would make it unreachable by any query", h.Driver, s)
	}
}

// recencyDisabledIsATrueRevert pins the kill switch (§7 test 5).
//
// A kill switch that reverts ranking but keeps the over-fetch is not a revert:
// the operator who turns it off after a bad rollout gets a result SET he has
// never seen before, from a LIMIT he did not ask for, and the thing he was
// trying to undo is still half-applied. So the assertion is identity over the
// whole over-fetched slice — same rows, same order, same scores, same LENGTH,
// with no truncation to the caller's limit.
func recencyDisabledIsATrueRevert(t *testing.T, h RecencyHarness) {
	ctx := context.Background()
	project := uniqueID("proj")
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)

	// A pool with every shape that WOULD move under the enabled config: a
	// superseded series member, an aged expiring chunk, and never-expiring
	// ones. If any factor leaked through, this ordering would change.
	var chunks []RecencyChunk
	for i := 0; i < 12; i++ {
		c := RecencyChunk{
			ID:        fmt.Sprintf("%s-%02d", project, i),
			ProjectID: project, ArtifactID: fmt.Sprintf("%s-art-%02d", project, i),
			SourceName: fmt.Sprintf("doc-%02d.md", i),
			Content:    "body", CreatedAt: now.AddDate(0, 0, -i*40),
		}
		if i%3 == 0 {
			e := c.CreatedAt.AddDate(0, 0, 30)
			c.ExpiresAt = &e
		}
		if i < 2 {
			c.SeriesKey = "a-series"
		}
		chunks = append(chunks, c)
	}
	seedAll(ctx, t, h, chunks)
	rows := loadRecency(ctx, t, h, project)

	in := make([]RecencyCandidate, 0, len(chunks))
	for i, c := range chunks {
		in = append(in, rows[c.ID].result(1.0/float64(61+i)))
	}
	before := append([]RecencyCandidate(nil), in...)

	// A caller limit far below the over-fetched pool: enabled, the re-rank
	// would cut to 4. Disabled it must not cut at all, because the SQL LIMIT
	// reverts too and the caller's own paging is what the pre-change code did.
	out := h.Rerank(in, false, now, 4)

	if len(out) != len(before) {
		t.Fatalf("[%s] disabled re-rank returned %d of %d rows — it truncated an over-fetch that, with "+
			"the switch off, the SQL never made", h.Driver, len(out), len(before))
	}
	for i := range before {
		if out[i].ChunkID != before[i].ChunkID || out[i].Score != before[i].Score {
			t.Fatalf("[%s] disabled re-rank is not a no-op at position %d: got %s/%.12f, want %s/%.12f",
				h.Driver, i, out[i].ChunkID, out[i].Score, before[i].ChunkID, before[i].Score)
		}
	}

	// And with the switch ON the same pool DOES move — otherwise the identity
	// above is satisfied by a feature that never did anything.
	moved := h.Rerank(in, true, now, len(in))
	same := true
	for i := range moved {
		if moved[i].ChunkID != before[i].ChunkID {
			same = false
			break
		}
	}
	if same {
		t.Errorf("[%s] the enabled re-rank left this pool in exactly the pre-change order, so the "+
			"disabled-is-identity assertion above proves nothing", h.Driver)
	}
}

// recencyPlanStaysIndexed is §7 test 10: the predicate must be SERVED by
// idx_memory_chunks_series, on both drivers, by name.
//
// This is what makes declining round 2's OR-rewrite safe rather than lucky.
// That rewrite was rejected on a measurement — the tuple form pushed the
// created_at range into the index on sqlite and the whole row-value comparison
// into the index condition on Postgres — and index selection is an optimiser
// decision that moves with driver version and table statistics. Without this
// test a driver bump could regress every EXISTS to a full walk of the series
// and the suite would stay green while retrieval got three orders of magnitude
// slower (measured: 46.7µs indexed, 172.7ms with the index dropped).
//
// Named index, not "an index": a future planner picking a different one is
// exactly the regression worth hearing about.
func recencyPlanStaysIndexed(t *testing.T, h RecencyHarness) {
	ctx := context.Background()
	project := uniqueID("proj")
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)

	// A daily series after a year, which is the scale the design measured and
	// the scale at which a degenerate plan actually hurts. Plus non-series
	// noise, because the partial index must still be chosen when most of the
	// table is not in it.
	var chunks []RecencyChunk
	for i := 0; i < 365; i++ {
		chunks = append(chunks, RecencyChunk{
			ID: fmt.Sprintf("%s-s%03d", project, i), ProjectID: project,
			ArtifactID: fmt.Sprintf("%s-digest-%03d", project, i), SourceName: fmt.Sprintf("d-%03d.md", i),
			SeriesKey: "czech-news", Content: "headlines",
			CreatedAt: now.AddDate(0, 0, -i),
		})
	}
	for i := 0; i < 400; i++ {
		chunks = append(chunks, RecencyChunk{
			ID: fmt.Sprintf("%s-n%03d", project, i), ProjectID: project,
			ArtifactID: fmt.Sprintf("%s-note-%03d", project, i), SourceName: fmt.Sprintf("n-%03d.md", i),
			Content: "an ordinary note", CreatedAt: now.AddDate(0, 0, -i),
		})
	}
	seedAll(ctx, t, h, chunks)
	// Table-scoped on purpose: a bare ANALYZE is the whole database on
	// Postgres, and this suite shares that database with every other
	// integration test.
	if _, err := h.DB.ExecContext(ctx, "ANALYZE project_memory_chunks"); err != nil {
		t.Logf("[%s] ANALYZE: %v (continuing; the plan assertion may be made on stale statistics)", h.Driver, err)
	}

	query := "SELECT c.id,\n" + seriesSupersededPredicateSQL + `
FROM project_memory_chunks c
WHERE c.project_id = ` + h.Arg(1) + ` AND c.series_key IS NOT NULL
ORDER BY c.id`
	plan, err := h.Explain(ctx, query, project)
	if err != nil {
		t.Fatalf("[%s] EXPLAIN: %v", h.Driver, err)
	}
	if !strings.Contains(plan, seriesIndexName) {
		t.Errorf("[%s] the supersession subquery is no longer served by %s. The tuple form was KEPT over "+
			"a review round's OR rewrite on a measurement of this exact plan; if the planner has stopped "+
			"using the index the measurement is void and every EXISTS now walks the whole series "+
			"(46.7µs → 172.7ms at this fixture's scale). Plan:\n%s", h.Driver, seriesIndexName, plan)
	}
	// And the comparison must be IN the index, not re-checked above it. An
	// index that is merely opened and then filtered row-by-row still names
	// itself in the plan, so the index assertion alone would go on passing
	// through exactly the regression the measurement was taken to rule out.
	if h.PlanPushdown != "" && !strings.Contains(plan, h.PlanPushdown) {
		t.Errorf("[%s] the (created_at, id) comparison is no longer pushed into %s — expected this "+
			"driver's spelling of it, %q, in the plan. The tuple form was kept over a review round's "+
			"OR rewrite precisely because it pushed down on BOTH drivers; if it stopped, the rewrite "+
			"decision needs re-measuring rather than re-arguing. Plan:\n%s",
			h.Driver, seriesIndexName, h.PlanPushdown, plan)
	}
}
