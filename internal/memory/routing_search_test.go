package memory

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/memoryfirewall"
	"vornik.io/vornik/internal/persistence"
)

// trustRows builds a 14-column search result row set (the trust-projecting
// shape from keywordSearchTemporal): a single fresh verified research chunk.
func trustRows(now time.Time) *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "project_id", "task_id", "source_name", "content", "score",
		"content_class", "is_alive", "last_checked_at", "repo_scope",
		"confidence", "validation_status", "created_at", "expires_at",
	}).AddRow("a", "p", "t", "s", "fresh verified", 0.9,
		string(ClassResearch), nil, nil, "",
		0.5, statusVerified, now, nil)
}

// RecallWithRouting Routing-ON: single-shot high verdict, trust fields
// populated. Uses FromDate so the query routes through the trust-projecting
// keywordSearchTemporal path (no query vector ⇒ keyword arm).
func TestRecallWithRouting_On_HighVerdict(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	s := NewSearcher(Config{RetrievalRouting: DefaultRetrievalRoutingConfig()}, r, nil)

	mock.ExpectQuery("ts_rank").WillReturnRows(trustRows(time.Now()))

	opts := SearchOptions{Routing: true, Limit: 5, FromDate: time.Now().AddDate(-1, 0, 0)}
	res, v, err := s.RecallWithRouting(context.Background(), "p", "q", opts, memoryfirewall.RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if v == nil {
		t.Fatal("Routing on: want a non-nil verdict")
	}
	if v.Verdict != VerdictHigh {
		t.Fatalf("want high, got %s (mean=%.3f)", v.Verdict, v.Basis.TrustMean)
	}
	if len(res) != 1 || res[0].Confidence != 0.5 || res[0].ValidationStatus != statusVerified {
		t.Fatalf("want trust fields projected onto result, got %+v", res)
	}
}

// RecallWithRouting Routing-OFF: nil verdict, delegates to RecallWithContext.
func TestRecallWithRouting_Off_NilVerdict(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	s := NewSearcher(Config{RetrievalRouting: DefaultRetrievalRoutingConfig()}, r, nil)

	mock.ExpectQuery("ts_rank").WillReturnRows(trustRows(time.Now()))

	opts := SearchOptions{Routing: false, Limit: 5, FromDate: time.Now().AddDate(-1, 0, 0)}
	res, v, err := s.RecallWithRouting(context.Background(), "p", "q", opts, memoryfirewall.RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if v != nil {
		t.Fatalf("Routing off: want nil verdict, got %+v", v)
	}
	if len(res) != 1 {
		t.Fatalf("want results returned, got %d", len(res))
	}
}

// Master kill-switch (review M-2): Routing requested at the call site but
// disabled in config ⇒ behaves exactly as routing-off (nil verdict), so the
// rollout is reversible config-only.
func TestRecallWithRouting_MasterDisabled_NilVerdict(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	cfg := DefaultRetrievalRoutingConfig()
	cfg.SetEnabled(false)
	s := NewSearcher(Config{RetrievalRouting: cfg}, r, nil)

	mock.ExpectQuery("ts_rank").WillReturnRows(trustRows(time.Now()))

	opts := SearchOptions{Routing: true, Limit: 5, FromDate: time.Now().AddDate(-1, 0, 0)}
	res, v, err := s.RecallWithRouting(context.Background(), "p", "q", opts, memoryfirewall.RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if v != nil {
		t.Fatalf("master-disabled: want nil verdict even with Routing=true, got %+v", v)
	}
	if len(res) != 1 {
		t.Fatalf("want results still returned, got %d", len(res))
	}
}

func TestRoutingConfig_AppliesDefaults(t *testing.T) {
	s := NewSearcher(Config{}, nil, nil) // zero RetrievalRouting
	cfg := s.routingConfig()
	if cfg.K != 5 || cfg.MinResults != 1 || !cfg.WidenEnabled {
		t.Fatalf("routingConfig must apply defaults, got %+v", cfg)
	}
}

// Confidence-based retrieval routing (P3) — widen loop + trace sink tests
// against the pure widenLoopCore (search injected as a func, so no DB).

func freshVerifiedSet(now time.Time, n int) []SearchResult {
	out := make([]SearchResult, n)
	for i := range out {
		out[i] = mkResult(string(ClassResearch), statusVerified, 0.5, 0, now) // trust 0.90 → high
	}
	return out
}

func weakLowSet(now time.Time) []SearchResult {
	// legacy stale research → trust 0.24 each → mean < lowThreshold → low.
	return []SearchResult{
		mkResult(string(ClassResearch), statusLegacy, 0.5, 200, now),
		mkResult(string(ClassResearch), statusLegacy, 0.5, 200, now),
	}
}

// widen fires only on low, and a low that becomes high stops early.
func TestWidenLoopCore_WidensOnLow_ImprovesToHigh(t *testing.T) {
	now := time.Now()
	cfg := defCfg()
	calls := 0
	run := func(_ SearchOptions) ([]SearchResult, error) {
		calls++
		if calls == 1 {
			return weakLowSet(now), nil // round 1: low
		}
		return freshVerifiedSet(now, 3), nil // widened: high
	}
	set, v, err := widenLoopCore(SearchOptions{Limit: 5, Routing: true}, cfg, now, run)
	if err != nil {
		t.Fatal(err)
	}
	if v.Verdict != VerdictHigh {
		t.Fatalf("want high after widen, got %s", v.Verdict)
	}
	if v.WidenRounds != 1 {
		t.Fatalf("want 1 widen round (stops when improved), got %d", v.WidenRounds)
	}
	if len(set) != 3 {
		t.Fatalf("want the widened (better) set returned, got %d results", len(set))
	}
}

// Widen only on low: an age-cap-only MEDIUM returns immediately (0 rounds),
// NOT MaxRounds — widening cannot fix staleness the DB can't resolve (F1).
func TestWidenLoopCore_AgeCapMedium_ZeroRounds(t *testing.T) {
	now := time.Now()
	cfg := defCfg()
	calls := 0
	run := func(_ SearchOptions) ([]SearchResult, error) {
		calls++
		return []SearchResult{mkResult(string(ClassDecision), statusVerified, 0.9, 200, now)}, nil // medium (age-capped)
	}
	_, v, err := widenLoopCore(SearchOptions{Limit: 5, Routing: true}, cfg, now, run)
	if err != nil {
		t.Fatal(err)
	}
	if v.Verdict != VerdictMedium {
		t.Fatalf("want medium, got %s", v.Verdict)
	}
	if v.WidenRounds != 0 {
		t.Fatalf("age-cap medium must NOT widen: want 0 rounds, got %d", v.WidenRounds)
	}
	if calls != 1 {
		t.Fatalf("want exactly 1 search (no widen), got %d", calls)
	}
}

// Widen bounded: a persistently-low verdict widens at most MaxRounds times.
func TestWidenLoopCore_BoundedByMaxRounds(t *testing.T) {
	now := time.Now()
	cfg := defCfg() // MaxRounds 3
	calls := 0
	run := func(_ SearchOptions) ([]SearchResult, error) {
		calls++
		return weakLowSet(now), nil // always low
	}
	_, v, err := widenLoopCore(SearchOptions{Limit: 5, Routing: true}, cfg, now, run)
	if err != nil {
		t.Fatal(err)
	}
	if v.Verdict != VerdictLow {
		t.Fatalf("want low, got %s", v.Verdict)
	}
	if v.WidenRounds != cfg.MaxRounds {
		t.Fatalf("want %d widen rounds, got %d", cfg.MaxRounds, v.WidenRounds)
	}
	if calls != cfg.MaxRounds+1 { // round 1 + MaxRounds widen rounds
		t.Fatalf("want %d searches, got %d", cfg.MaxRounds+1, calls)
	}
}

// Reranker-off + Routing-on still widens (the whole point): the widen is
// verdict-predicated, not reranker-gated — rounds > 1 are possible with no
// reranker involved. widenLoopCore has no reranker dependency at all.
func TestWidenLoopCore_WidensWithoutReranker(t *testing.T) {
	now := time.Now()
	cfg := defCfg()
	calls := 0
	run := func(_ SearchOptions) ([]SearchResult, error) {
		calls++
		return weakLowSet(now), nil
	}
	_, v, _ := widenLoopCore(SearchOptions{Limit: 5, Routing: true}, cfg, now, run)
	if v.WidenRounds < 1 {
		t.Fatal("widen must execute with the reranker off (not single-shot)")
	}
}

// best-so-far (F3): a later WORSE round never replaces a better earlier round.
func TestWidenLoopCore_BestSoFar_WorseRoundDoesNotReplace(t *testing.T) {
	now := time.Now()
	cfg := defCfg()
	// round1 low; round2 medium (best); round3 low again (worse) — must keep round2.
	mediumSet := []SearchResult{mkResult(string(ClassResearch), statusUnverified, 0.5, 0, now)} // 0.61 → medium
	calls := 0
	run := func(_ SearchOptions) ([]SearchResult, error) {
		calls++
		switch calls {
		case 1:
			return weakLowSet(now), nil
		case 2:
			return mediumSet, nil
		default:
			return weakLowSet(now), nil
		}
	}
	set, v, _ := widenLoopCore(SearchOptions{Limit: 5, Routing: true}, cfg, now, run)
	if v.Verdict != VerdictMedium {
		t.Fatalf("want medium retained (best-so-far), got %s", v.Verdict)
	}
	if len(set) != 1 {
		t.Fatalf("want the medium round's set retained, got %d results", len(set))
	}
	// It kept widening after round2 because best is still not high AND verdict
	// is evaluated on `best` (still medium ⇒ loop condition is best==low? No —
	// once best is medium the loop stops). So calls should be 2 (round1 low →
	// widen once to medium → loop condition best!=low → stop).
	if calls != 2 {
		t.Fatalf("loop should stop once best is non-low: want 2 calls, got %d", calls)
	}
}

// A round error returns the prior best and never errors the call; no further
// searches are attempted.
func TestWidenLoopCore_RoundErrorReturnsPriorBest(t *testing.T) {
	now := time.Now()
	cfg := defCfg()
	calls := 0
	run := func(_ SearchOptions) ([]SearchResult, error) {
		calls++
		if calls == 1 {
			return weakLowSet(now), nil // low → will widen
		}
		return nil, errors.New("db blip")
	}
	set, v, err := widenLoopCore(SearchOptions{Limit: 5, Routing: true}, cfg, now, run)
	if err != nil {
		t.Fatalf("round error must NOT error the call, got %v", err)
	}
	if v.Verdict != VerdictLow {
		t.Fatalf("want prior best (low) retained, got %s", v.Verdict)
	}
	if len(set) != 2 {
		t.Fatalf("want prior best set retained, got %d results", len(set))
	}
	if calls != 2 {
		t.Fatalf("want exactly 2 calls (round1 + errored widen), got %d", calls)
	}
}

// Round-1 error is the single-shot error (bubbles).
func TestWidenLoopCore_Round1ErrorBubbles(t *testing.T) {
	run := func(_ SearchOptions) ([]SearchResult, error) {
		return nil, errors.New("boom")
	}
	_, _, err := widenLoopCore(SearchOptions{Limit: 5, Routing: true}, defCfg(), time.Now(), run)
	if err == nil {
		t.Fatal("round-1 error must bubble")
	}
}

// WidenEnabled=false ⇒ verdict-only: no widen even on low.
func TestWidenLoopCore_WidenDisabled_NoWiden(t *testing.T) {
	now := time.Now()
	cfg := defCfg()
	cfg.SetWidenEnabled(false)
	calls := 0
	run := func(_ SearchOptions) ([]SearchResult, error) {
		calls++
		return weakLowSet(now), nil
	}
	_, v, _ := widenLoopCore(SearchOptions{Limit: 5, Routing: true}, cfg, now, run)
	if v.WidenRounds != 0 || calls != 1 {
		t.Fatalf("widen disabled: want 0 rounds / 1 call, got %d rounds / %d calls", v.WidenRounds, calls)
	}
}

// --- trace sink ---

type fakeStageRepo struct {
	rows []*persistence.MemorySearchStage
	err  error
}

func (f *fakeStageRepo) RecordStage(_ context.Context, s *persistence.MemorySearchStage) error {
	if f.err != nil {
		return f.err
	}
	f.rows = append(f.rows, s)
	return nil
}

func TestWriteTrustVerdictTrace_WritesRow(t *testing.T) {
	fake := &fakeStageRepo{}
	s := &Searcher{logger: zerolog.Nop(), traceSink: fake}
	cfg := defCfg()
	v := &RoutingVerdict{
		Verdict:     VerdictMedium,
		Basis:       VerdictBasis{ResultCount: 2, TrustMean: 0.55, AgeCapped: true, WeakestDim: WeakestAgedDecision},
		WidenRounds: 0,
	}
	s.writeTrustVerdictTrace(context.Background(), "proj-1", v, cfg)

	if len(fake.rows) != 1 {
		t.Fatalf("want 1 trace row, got %d", len(fake.rows))
	}
	row := fake.rows[0]
	if row.Stage != "trust_verdict" {
		t.Fatalf("want stage=trust_verdict, got %q", row.Stage)
	}
	if row.ProjectID != "proj-1" {
		t.Fatalf("want project proj-1, got %q", row.ProjectID)
	}
	var params map[string]any
	if err := json.Unmarshal(row.Parameters, &params); err != nil {
		t.Fatalf("parameters not valid JSON: %v", err)
	}
	for _, k := range []string{"verdict", "trust_mean", "result_count", "age_capped", "weakest_dim", "weights", "thresholds", "K", "minResults", "widen_rounds"} {
		if _, ok := params[k]; !ok {
			t.Fatalf("trace parameters missing %q: %v", k, params)
		}
	}
	if params["verdict"] != VerdictMedium {
		t.Fatalf("trace verdict mismatch: %v", params["verdict"])
	}
}

func TestWriteTrustVerdictTrace_NilSink_NoPanic(_ *testing.T) {
	s := &Searcher{logger: zerolog.Nop()}
	// A nil sink must be a no-op (no panic).
	s.writeTrustVerdictTrace(context.Background(), "p", &RoutingVerdict{Verdict: VerdictLow}, defCfg())
}

func TestWriteTrustVerdictTrace_WriteError_Swallowed(_ *testing.T) {
	fake := &fakeStageRepo{err: errors.New("db down")}
	s := &Searcher{logger: zerolog.Nop(), traceSink: fake}
	// Must not panic / must not surface — best-effort.
	s.writeTrustVerdictTrace(context.Background(), "p", &RoutingVerdict{Verdict: VerdictLow}, defCfg())
}
