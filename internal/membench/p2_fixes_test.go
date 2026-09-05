package membench

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// --- Leak probe nonce (2026-08-21 backlog item) --------------------------------

// TestLeakProbePayload_DiffersBetweenRuns — regression for "the memory bench's
// leak probe makes every repeat run untrustworthy".
//
// The probe payload was `fmt.Sprintf("%s (probe %d)", leakageNeedle, i)` with no
// run nonce, so it was byte-identical on every run. On a persistent store the
// ingest gate rejected all four probes on dedup_hash from the second run onward,
// the harness counted those rejections as haystack loss, and every run after the
// first was stamped untrustworthy with a reason that named a dataset problem
// which did not exist. A judged 120-item pass is what that discards.
func TestLeakProbePayload_DiffersBetweenRuns(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		n := newRunNonce()
		if n == "" {
			t.Fatal("newRunNonce returned empty; the payload would be constant again")
		}
		if seen[n] {
			t.Fatalf("newRunNonce repeated %q within %d draws — a colliding nonce is the bug this fixes", n, i+1)
		}
		seen[n] = true
	}
}

// The probe payload must still CONTAIN the needle, or the leak assertion it
// exists to make stops working: parts 1-3 all search for leakageNeedle.
func TestLeakProbePayload_StillCarriesTheNeedle(t *testing.T) {
	r := &Runner{runNonce: newRunNonce()}
	sys := &recordingProbeSystem{}
	r.System = sys
	r.MaxTokens = 100

	_ = r.assertNoLeakage(context.Background())

	if len(sys.ingested) != leakageProbeScopes {
		t.Fatalf("ingested %d probes, want %d", len(sys.ingested), leakageProbeScopes)
	}
	for _, content := range sys.ingested {
		if !strings.Contains(content, leakageNeedle) {
			t.Errorf("probe payload %q lost the needle; the isolation check searches for it", content)
		}
		if !strings.Contains(content, r.runNonce) {
			t.Errorf("probe payload %q carries no run nonce; it will dedup against its own history", content)
		}
	}
}

// recordingProbeSystem captures what the leak probes ingest and reports the
// needle back in its own scope, so assertNoLeakage reaches its checks instead of
// bailing on the return-nothing guard.
type recordingProbeSystem struct {
	ingested []string
}

func (s *recordingProbeSystem) Name() string { return "recording-probe" }

func (s *recordingProbeSystem) Prepare(context.Context, string) error { return nil }

func (s *recordingProbeSystem) Ingest(_ context.Context, _ string, items []Item) (IngestStats, error) {
	for _, it := range items {
		s.ingested = append(s.ingested, it.Content)
	}
	return IngestStats{}, nil
}

// Recall answers only for the FIRST probe scope, which is the one part 3 checks.
// Every other scope returns nothing, which is what "no leak" looks like.
func (s *recordingProbeSystem) Recall(_ context.Context, scope string, _ Query) (Recalled, error) {
	if scope != "membench/leak-probe/0" || len(s.ingested) == 0 {
		return Recalled{}, nil
	}
	return Recalled{Hits: []Hit{{SourceID: scopeProbeID(scope), Text: s.ingested[0]}}}, nil
}

func (s *recordingProbeSystem) Teardown(context.Context, string) error { return nil }

func (s *recordingProbeSystem) Config(context.Context) (string, error) { return "", nil }

// --- Resume carries the haystack-loss evidence (2026-09-03 audit) --------------

// TestJournal_ResumeCarriesWorstHaystackLoss — regression for "a resumed
// membench run drops haystack-loss evidence, so Trust can pass a run a fresh run
// would fail".
//
// AssessTrust gates Trustworthy on TWO signals. Counts were seeded from the
// journal so a resumed run "reports over the whole population"; worstLoss
// restarted at 0.0 and every already-Completed item was skipped. So a resumed
// run could be stamped trustworthy on exactly the evidence that would have
// failed it in one pass, with nothing saying the bar had moved.
func TestJournal_ResumeCarriesWorstHaystackLoss(t *testing.T) {
	path := tempJournal(t)
	j, err := OpenJournal(path)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	for _, e := range []JournalEntry{
		{ItemID: "q1", Phase: PhaseJudged, Category: "c", Outcome: OutcomeCorrect, HaystackLoss: 0.10},
		// The item that would fail the run: past MaxHaystackLoss.
		{ItemID: "q2", Phase: PhaseJudged, Category: "c", Outcome: OutcomeCorrect, HaystackLoss: 0.90},
		{ItemID: "q3", Phase: PhaseJudged, Category: "c", Outcome: OutcomeCorrect, HaystackLoss: 0.20},
	} {
		if err := j.Record(e); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	if err := j.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	replay, err := LoadJournal(path)
	if err != nil {
		t.Fatalf("LoadJournal: %v", err)
	}
	if got := replay.WorstHaystackLoss(); got != 0.90 {
		t.Fatalf("WorstHaystackLoss = %v, want 0.90 — the resumed run cannot see the item that should fail it", got)
	}

	// And the trust verdict built from it must actually refuse.
	trust := AssessTrust(replay.CountsByCategory()["c"], replay.WorstHaystackLoss(), 0)
	if trust.Trustworthy {
		t.Fatal("a resumed run carrying a 90% haystack loss was stamped trustworthy; " +
			"the same work in one pass would have been refused")
	}
}

// A journal written before the field existed must still resume, and must not
// invent a loss. The zero value means "no loss", which is what an absent field
// also means for a maximum.
func TestJournal_OldEntriesWithoutHaystackLoss_StillResume(t *testing.T) {
	path := tempJournal(t)
	j, err := OpenJournal(path)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	if err := j.Record(JournalEntry{ItemID: "q1", Phase: PhaseJudged, Category: "c", Outcome: OutcomeCorrect}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	replay, err := LoadJournal(path)
	if err != nil {
		t.Fatalf("LoadJournal on a pre-field journal: %v", err)
	}
	if got := replay.WorstHaystackLoss(); got != 0 {
		t.Fatalf("WorstHaystackLoss = %v on a journal with no such field, want 0", got)
	}
	if !replay.Completed("q1") {
		t.Error("a pre-field journal stopped resuming")
	}
}

// --- The clear retries a deadlock (2026-08-22 backlog item) --------------------

// pqDeadlock mimics lib/pq's error: it carries SQLState, which is what the
// retry predicate reads before falling back to string matching.
type pqDeadlock struct{}

func (pqDeadlock) Error() string    { return "pq: deadlock detected" }
func (pqDeadlock) SQLState() string { return "40P01" }

// TestClearBenchmarkStore_RetriesADeadlock — regression for "the benchmark store
// clear deadlocks against the daemon's own ingest".
//
// The clear deletes mentions -> edges -> entities -> chunks; the graph pipeline
// inserts the entity first and then the mention referencing it and its chunk. So
// each holds what the other wants, Postgres kills one side, and it is usually
// the clear — which is the whole run. Measured 2026-08-21 with a 1,897-chunk
// backlog: the clear failed on its first attempt and the run aborted before
// ingesting anything.
func TestClearBenchmarkStore_RetriesADeadlock(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Attempt 1 deadlocks on the first statement.
	mock.ExpectBegin()
	mock.ExpectExec("DELETE FROM entity_mentions").WillReturnError(pqDeadlock{})
	mock.ExpectRollback()

	// Attempt 2 succeeds, which is what the extraction worker moving on looks like.
	mock.ExpectBegin()
	mock.ExpectExec("DELETE FROM entity_mentions").WillReturnResult(sqlmock.NewResult(0, 333))
	mock.ExpectExec("DELETE FROM knowledge_edges").WillReturnResult(sqlmock.NewResult(0, 43))
	mock.ExpectExec("DELETE FROM knowledge_entities").WillReturnResult(sqlmock.NewResult(0, 196))
	mock.ExpectExec("DELETE FROM project_memory_chunks").WillReturnResult(sqlmock.NewResult(0, 1897))
	mock.ExpectCommit()

	res, err := ClearBenchmarkStore(context.Background(), db, "bench", "membench/%")
	if err != nil {
		t.Fatalf("a deadlock aborted the clear instead of being retried: %v", err)
	}
	if res.Chunks != 1897 || res.Mentions != 333 {
		t.Errorf("ClearResult = %+v, want the SECOND attempt's counts", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// A deadlock that never clears must still fail — and must name the concurrent
// extraction, because "deadlock detected" alone tells an operator nothing about
// which two things collided.
func TestClearBenchmarkStore_PersistentDeadlock_FailsWithTheCause(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	for i := 0; i < clearDeadlockAttempts; i++ {
		mock.ExpectBegin()
		mock.ExpectExec("DELETE FROM entity_mentions").WillReturnError(pqDeadlock{})
		mock.ExpectRollback()
	}

	_, err = ClearBenchmarkStore(context.Background(), db, "bench", "membench/%")
	if err == nil {
		t.Fatal("a permanently deadlocking clear reported success")
	}
	for _, want := range []string{"extracting a graph backlog", "Quiesce the ingest worker", "bench"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q; an operator cannot act on a bare driver error", err, want)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// A NON-deadlock error must not be retried: retrying a constraint violation or a
// permissions failure three times just makes the same failure slower.
func TestClearBenchmarkStore_OtherErrors_AreNotRetried(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectBegin()
	mock.ExpectExec("DELETE FROM entity_mentions").WillReturnError(errors.New("permission denied for table entity_mentions"))
	mock.ExpectRollback()

	if _, err := ClearBenchmarkStore(context.Background(), db, "bench", "membench/%"); err == nil {
		t.Fatal("expected the error to propagate")
	}
	// Exactly one attempt: any unmet expectation here means it retried.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations (a non-deadlock error was retried): %v", err)
	}
}

func TestIsDeadlock(t *testing.T) {
	cases := map[error]bool{
		nil:                                 false,
		pqDeadlock{}:                        true,
		errors.New("pq: deadlock detected"): true,
		errors.New("SQLSTATE 40P01"):        true,
		errors.New("permission denied"):     false,
		errors.New("could not serialize"):   false,
		errors.New("deadline exceeded"):     false,
	}
	for err, want := range cases {
		if got := isDeadlock(err); got != want {
			t.Errorf("isDeadlock(%v) = %v, want %v", err, got, want)
		}
	}
}

// --- The key can tell a warm corpus from a cold one (2026-08-27 backlog item) ---

// TestComparabilityKey_DistinguishesWarmFromCold — regression for "the
// comparability key cannot tell a warm corpus from a cold one".
//
// Demonstrated 2026-08-27: bench-runs/reproduce-1..3 (warm, taken before
// 98037e34 made the clear real) and head-20260827-tr6-r1..3 (cold) both produced
// key e865104e9959. `aggregate` would merge them without complaint and `compare`
// would diff them clean — over the axis results.md calls decisive enough to CLOSE
// a published table.
func TestComparabilityKey_DistinguishesWarmFromCold(t *testing.T) {
	cold := baseFields()
	cold.CorpusRegime = CorpusRegimeCold
	warm := baseFields()
	warm.CorpusRegime = CorpusRegimeWarm

	if cold.Key() == warm.Key() {
		t.Fatal("a warm run and a cold run share a comparability key; aggregate will merge " +
			"them and compare will diff them clean")
	}
	if err := CheckComparable(cold, warm); err == nil {
		t.Fatal("CheckComparable admitted a warm/cold pair")
	} else if !strings.Contains(err.Error(), "corpus_regime") {
		t.Errorf("the refusal does not name corpus_regime: %v", err)
	}
}

// A run that cannot say which regime it used marks the key PARTIAL rather than
// asserting either — the same observed-not-declared rule as ObservedEmbedder.
func TestComparabilityFields_UnknownCorpusRegime_IsPartial(t *testing.T) {
	f := baseFields()
	f.CorpusRegime = CorpusRegimeUnknown
	if !f.Partial() {
		t.Fatal("a run that cannot say whether its store was cleared was reported fully comparable")
	}
}

// --- A run can say which build produced it (2026-08-27 backlog item) -----------

// TestComparabilityKey_DistinguishesReleases — regression for "a memory
// benchmark run cannot say which build produced it".
//
// Everything else in the key pins the EXPERIMENT and nothing pinned the CODE, so
// two runs of different releases with the same config compared clean. A release
// change must refuse comparison rather than silently produce one, which is what
// ArmFields.binary_sha256 already does on the agent side.
func TestComparabilityKey_DistinguishesReleases(t *testing.T) {
	before := baseFields()
	before.DaemonRevision = "0e450f9c"
	after := baseFields()
	after.DaemonRevision = "4b343821"

	if before.Key() == after.Key() {
		t.Fatal("two releases produced the same comparability key; a regression between them " +
			"would compare clean")
	}
	if err := CheckComparable(before, after); err == nil {
		t.Fatal("CheckComparable admitted two different builds")
	} else if !strings.Contains(err.Error(), "daemon_revision") {
		t.Errorf("the refusal does not name daemon_revision: %v", err)
	}
}

func TestComparabilityFields_UnknownDaemonRevision_IsPartial(t *testing.T) {
	f := baseFields()
	f.DaemonRevision = ""
	if !f.Partial() {
		t.Fatal("a run that cannot name its build was reported fully comparable")
	}
}

// The manifest must CARRY the revisions, not merely the key: attributing a run
// to a release by matching finished_at against the git log is what the absence
// cost on 2026-08-27.
func TestManifest_CarriesTheRevisions(t *testing.T) {
	dir := t.TempDir()
	r := &Runner{
		System:          &recordingProbeSystem{},
		Dataset:         nil,
		RunDir:          dir,
		DaemonRevision:  "4b343821",
		HarnessRevision: "abc1234",
	}
	m := manifest{
		System:          "vornik",
		DaemonRevision:  r.DaemonRevision,
		HarnessRevision: r.HarnessRevision,
	}
	if err := writeJSON(filepath.Join(dir, "manifest.json"), m); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}
	got, err := readFileString(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	for _, want := range []string{`"daemon_revision": "4b343821"`, `"harness_revision": "abc1234"`} {
		if !strings.Contains(got, want) {
			t.Errorf("manifest.json does not contain %s:\n%s", want, got)
		}
	}
}
