package memory

import "testing"

// scriptedRunner returns a pre-programmed result set per call and records the
// SearchOptions each round was invoked with.
type scriptedRunner struct {
	rounds  [][]SearchResult
	errOn   int // 1-based round index to error on; 0 = never
	calls   int
	gotOpts []SearchOptions
}

func (r *scriptedRunner) run(opts SearchOptions) ([]SearchResult, error) {
	r.calls++
	r.gotOpts = append(r.gotOpts, opts)
	if r.errOn == r.calls {
		return nil, errTest
	}
	if r.calls-1 < len(r.rounds) {
		return r.rounds[r.calls-1], nil
	}
	return nil, nil
}

var errTest = &testErr{}

type testErr struct{}

func (*testErr) Error() string { return "scripted error" }

func hits(scores ...float64) []SearchResult {
	out := make([]SearchResult, len(scores))
	for i, s := range scores {
		out[i] = SearchResult{Score: s, ChunkID: string(rune('a' + i))}
	}
	return out
}

func TestSufficiencyLoop_GatedOff_SingleShot(t *testing.T) {
	cfg := SufficiencyConfig{Enabled: true, MinHighRel: 3, ScoreFloor: 0.6, MaxRounds: 3}
	// reranker inactive ⇒ exactly one round regardless of enabled.
	r := &scriptedRunner{rounds: [][]SearchResult{hits(0.9, 0.1)}}
	_, err := sufficiencyLoop(SearchOptions{Limit: 5}, cfg, false /*active*/, r.run)
	if err != nil {
		t.Fatal(err)
	}
	if r.calls != 1 {
		t.Fatalf("reranker-inactive must be single-shot; got %d calls", r.calls)
	}
}

func TestSufficiencyLoop_DisabledOrSingleRound_SingleShot(t *testing.T) {
	for _, cfg := range []SufficiencyConfig{
		{Enabled: false, MinHighRel: 3, ScoreFloor: 0.6, MaxRounds: 3},
		{Enabled: true, MinHighRel: 3, ScoreFloor: 0.6, MaxRounds: 1},
	} {
		r := &scriptedRunner{rounds: [][]SearchResult{hits(0.9)}}
		if _, err := sufficiencyLoop(SearchOptions{Limit: 5}, cfg, true, r.run); err != nil {
			t.Fatal(err)
		}
		if r.calls != 1 {
			t.Fatalf("cfg %+v must be single-shot; got %d calls", cfg, r.calls)
		}
	}
}

func TestSufficiencyLoop_Round1Sufficient_NoWiden(t *testing.T) {
	cfg := SufficiencyConfig{Enabled: true, MinHighRel: 2, ScoreFloor: 0.6, MaxRounds: 3}
	r := &scriptedRunner{rounds: [][]SearchResult{hits(0.9, 0.8, 0.1)}} // 2 ≥ floor
	got, err := sufficiencyLoop(SearchOptions{Limit: 5}, cfg, true, r.run)
	if err != nil {
		t.Fatal(err)
	}
	if r.calls != 1 {
		t.Fatalf("round 1 sufficient ⇒ zero extra cost; got %d calls", r.calls)
	}
	if len(got) != 3 {
		t.Fatalf("expected round-1 set, got %d", len(got))
	}
}

func TestSufficiencyLoop_WidensThenReturnsFirstSufficient(t *testing.T) {
	cfg := SufficiencyConfig{Enabled: true, MinHighRel: 2, ScoreFloor: 0.6, MaxRounds: 3}
	r := &scriptedRunner{rounds: [][]SearchResult{
		hits(0.9, 0.1),      // round 1: 1 ≥ floor — insufficient
		hits(0.9, 0.7, 0.1), // round 2: 2 ≥ floor — sufficient
	}}
	got, err := sufficiencyLoop(SearchOptions{Limit: 5}, cfg, true, r.run)
	if err != nil {
		t.Fatal(err)
	}
	if r.calls != 2 {
		t.Fatalf("expected 2 rounds, got %d", r.calls)
	}
	if len(got) != 3 {
		t.Fatalf("expected round-2 set returned (round-isolated), got %d", len(got))
	}
	// Widening increased Limit and dropped StrictScope; query never changes
	// (cache-hit invariant lives in RecallSufficient, not here).
	if r.gotOpts[1].Limit <= r.gotOpts[0].Limit {
		t.Errorf("round 2 should widen Limit: %d -> %d", r.gotOpts[0].Limit, r.gotOpts[1].Limit)
	}
	if r.gotOpts[1].StrictScope {
		t.Error("widening should relax StrictScope")
	}
}

func TestSufficiencyLoop_NoneSufficient_BestByCount_Round1WinsTie(t *testing.T) {
	cfg := SufficiencyConfig{Enabled: true, MinHighRel: 5, ScoreFloor: 0.6, MaxRounds: 3}
	r := &scriptedRunner{rounds: [][]SearchResult{
		hits(0.9, 0.1),  // round 1: 1 high-rel
		hits(0.9, 0.05), // round 2: 1 high-rel — tie, round 1 must win
		hits(0.2, 0.1),  // round 3: 0 high-rel
	}}
	got, err := sufficiencyLoop(SearchOptions{Limit: 5}, cfg, true, r.run)
	if err != nil {
		t.Fatal(err)
	}
	if r.calls != 3 {
		t.Fatalf("expected all 3 rounds, got %d", r.calls)
	}
	// Round 1 wins the tie → its second hit has score 0.1.
	if len(got) != 2 || got[1].Score != 0.1 {
		t.Fatalf("expected round-1 set on tie, got %+v", got)
	}
}

func TestSufficiencyLoop_RoundErrorReturnsBestSoFar(t *testing.T) {
	cfg := SufficiencyConfig{Enabled: true, MinHighRel: 5, ScoreFloor: 0.6, MaxRounds: 3}
	r := &scriptedRunner{
		rounds: [][]SearchResult{hits(0.9, 0.1)}, // round 1
		errOn:  2,                                // round 2 errors
	}
	got, err := sufficiencyLoop(SearchOptions{Limit: 5}, cfg, true, r.run)
	if err != nil {
		t.Fatalf("error mid-loop must not propagate: %v", err)
	}
	if len(got) != 2 || got[0].Score != 0.9 {
		t.Fatalf("must return round-1 best-so-far, got %+v", got)
	}
}

func TestSufficiencyLoop_Round1ErrorPropagates(t *testing.T) {
	cfg := SufficiencyConfig{Enabled: true, MinHighRel: 2, ScoreFloor: 0.6, MaxRounds: 3}
	r := &scriptedRunner{errOn: 1}
	if _, err := sufficiencyLoop(SearchOptions{Limit: 5}, cfg, true, r.run); err == nil {
		t.Fatal("a round-1 error is the single-shot error and must propagate")
	}
}

func TestSufficiencyLoop_TruncatesToLimit(t *testing.T) {
	cfg := SufficiencyConfig{Enabled: true, MinHighRel: 1, ScoreFloor: 0.6, MaxRounds: 3}
	r := &scriptedRunner{rounds: [][]SearchResult{hits(0.9, 0.8, 0.7, 0.6, 0.5)}}
	got, err := sufficiencyLoop(SearchOptions{Limit: 2}, cfg, true, r.run)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("result must truncate to caller Limit=2, got %d", len(got))
	}
}

func TestHighRelCount(t *testing.T) {
	if n := highRelCount(hits(0.9, 0.6, 0.59, 0.1), 0.6); n != 2 {
		t.Fatalf("highRelCount = %d, want 2", n)
	}
}
