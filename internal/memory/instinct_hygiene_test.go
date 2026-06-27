package memory

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence"
)

// fakeInstinctLister is a table-driven stand-in for the instinct
// repository's List surface. It records the filter it was called with
// so the gate / filter-shape assertions can inspect it.
type fakeInstinctLister struct {
	rows       []*persistence.Instinct
	err        error
	lastFilter persistence.InstinctFilter
	calls      int
}

func (f *fakeInstinctLister) List(_ context.Context, filter persistence.InstinctFilter) ([]*persistence.Instinct, error) {
	f.calls++
	f.lastFilter = filter
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

// fakePolicyLoader returns canned policy rows by chunk ID.
type fakePolicyLoader struct {
	rows map[string]ChunkPolicyRow
	err  error
}

func (f *fakePolicyLoader) LoadChunkPolicies(_ context.Context, ids []string) (map[string]ChunkPolicyRow, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make(map[string]ChunkPolicyRow, len(ids))
	for _, id := range ids {
		if r, ok := f.rows[id]; ok {
			out[id] = r
		}
	}
	return out, nil
}

func pruneAction(chunkID string) string {
	return "chunk " + chunkID + " has not been retrieved in 60 days (prune candidate)"
}

func TestDeriveRetrievalHints(t *testing.T) {
	tests := []struct {
		name      string
		rows      []*persistence.Instinct
		wantBoost []string
		wantPrune []string
	}{
		{
			name:      "empty input yields empty sets",
			rows:      nil,
			wantBoost: nil,
			wantPrune: nil,
		},
		{
			name: "active scope-support becomes boost",
			rows: []*persistence.Instinct{
				{
					Domain:  persistence.InstinctDomainRetrieval,
					Status:  persistence.InstinctStatusActive,
					Action:  "searching scope docs during role lead correlated with the step outcome",
					Trigger: triggerJSONFor("lead", "", "docs"),
				},
			},
			wantBoost: []string{"docs"},
		},
		{
			name: "candidate scope-support is NOT acted on",
			rows: []*persistence.Instinct{
				{
					Domain:  persistence.InstinctDomainRetrieval,
					Status:  persistence.InstinctStatusCandidate,
					Action:  "searching scope docs during role lead correlated with the step outcome",
					Trigger: triggerJSONFor("lead", "", "docs"),
				},
			},
			wantBoost: nil,
		},
		{
			name: "promoted scope-support becomes boost",
			rows: []*persistence.Instinct{
				{
					Domain:  persistence.InstinctDomainRetrieval,
					Status:  persistence.InstinctStatusPromoted,
					Action:  "searching scope src during role coder correlated with the step outcome",
					Trigger: triggerJSONFor("coder", "", "src"),
				},
			},
			wantBoost: []string{"src"},
		},
		{
			name: "prune-candidate action becomes prune chunk",
			rows: []*persistence.Instinct{
				{
					Domain:  persistence.InstinctDomainRetrieval,
					Status:  persistence.InstinctStatusCandidate,
					Action:  pruneAction("chunk-dead-1"),
					Trigger: triggerJSONFor("", "chunk-dead-1", ""),
				},
			},
			wantPrune: []string{"chunk-dead-1"},
		},
		{
			name: "retired prune candidate is ignored",
			rows: []*persistence.Instinct{
				{
					Domain:  persistence.InstinctDomainRetrieval,
					Status:  persistence.InstinctStatusRetired,
					Action:  pruneAction("chunk-resolved"),
					Trigger: triggerJSONFor("", "chunk-resolved", ""),
				},
			},
			wantPrune: nil,
		},
		{
			name: "non-retrieval domain is ignored",
			rows: []*persistence.Instinct{
				{
					Domain:  persistence.InstinctDomainRecovery,
					Status:  persistence.InstinctStatusActive,
					Action:  pruneAction("chunk-x"),
					Trigger: triggerJSONFor("lead", "chunk-x", "docs"),
				},
			},
			wantBoost: nil,
			wantPrune: nil,
		},
		{
			name: "mixed rows partition correctly and dedup",
			rows: []*persistence.Instinct{
				{
					Domain:  persistence.InstinctDomainRetrieval,
					Status:  persistence.InstinctStatusActive,
					Action:  "searching scope docs during role lead correlated with the step outcome",
					Trigger: triggerJSONFor("lead", "", "docs"),
				},
				{
					Domain:  persistence.InstinctDomainRetrieval,
					Status:  persistence.InstinctStatusActive,
					Action:  "searching scope docs during role coder correlated with the step outcome",
					Trigger: triggerJSONFor("coder", "", "docs"), // same scope, dedup
				},
				{
					Domain:  persistence.InstinctDomainRetrieval,
					Status:  persistence.InstinctStatusCandidate,
					Action:  pruneAction("chunk-dead-1"),
					Trigger: triggerJSONFor("", "chunk-dead-1", ""),
				},
				{
					Domain:  persistence.InstinctDomainRetrieval,
					Status:  persistence.InstinctStatusActive,
					Action:  pruneAction("chunk-dead-2"),
					Trigger: triggerJSONFor("", "chunk-dead-2", ""),
				},
			},
			wantBoost: []string{"docs"},
			wantPrune: []string{"chunk-dead-1", "chunk-dead-2"},
		},
		{
			name: "nil row is skipped",
			rows: []*persistence.Instinct{
				nil,
				{
					Domain:  persistence.InstinctDomainRetrieval,
					Status:  persistence.InstinctStatusActive,
					Action:  "searching scope docs during role lead correlated with the step outcome",
					Trigger: triggerJSONFor("lead", "", "docs"),
				},
			},
			wantBoost: []string{"docs"},
		},
		{
			name: "prune action with empty step_id is skipped",
			rows: []*persistence.Instinct{
				{
					Domain:  persistence.InstinctDomainRetrieval,
					Status:  persistence.InstinctStatusCandidate,
					Action:  pruneAction(""),
					Trigger: triggerJSONFor("", "", ""),
				},
			},
			wantPrune: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DeriveRetrievalHints(tc.rows)
			assertSet(t, "boost", got.BoostScopes, tc.wantBoost)
			assertSet(t, "prune", got.PruneChunkIDs, tc.wantPrune)
		})
	}
}

// triggerJSONFor is the non-*testing.T marshaller used inside the table
// (DeriveRetrievalHints is pure; a marshal failure here is a test bug).
func triggerJSONFor(role, stepID, repoScope string) json.RawMessage {
	b, _ := json.Marshal(retrievalTrigger{Role: role, StepID: stepID, RepoScope: repoScope})
	return b
}

func assertSet(t *testing.T, label string, got map[string]struct{}, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s set: got %d entries %v, want %d %v", label, len(got), keys(got), len(want), want)
	}
	for _, w := range want {
		if _, ok := got[w]; !ok {
			t.Errorf("%s set: missing %q (have %v)", label, w, keys(got))
		}
	}
}

func keys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestRetrievalHints_Accessors(t *testing.T) {
	h := &RetrievalHints{
		BoostScopes:   map[string]struct{}{"docs": {}},
		PruneChunkIDs: map[string]struct{}{"dead": {}},
	}
	if !h.IsBoostScope("docs") {
		t.Error("IsBoostScope(docs) = false, want true")
	}
	if h.IsBoostScope("missing") {
		t.Error("IsBoostScope(missing) = true, want false")
	}
	if !h.IsPruneCandidate("dead") {
		t.Error("IsPruneCandidate(dead) = false, want true")
	}
	if h.IsPruneCandidate("alive") {
		t.Error("IsPruneCandidate(alive) = true, want false")
	}

	// Nil-safe: a nil/empty Hints reports false for everything (the
	// gate-off behaviour callers rely on).
	var nilHints *RetrievalHints
	if nilHints.IsBoostScope("docs") || nilHints.IsPruneCandidate("dead") {
		t.Error("nil Hints accessors must report false")
	}
	emptyHints := &RetrievalHints{}
	if emptyHints.IsBoostScope("docs") || emptyHints.IsPruneCandidate("dead") {
		t.Error("empty Hints accessors must report false")
	}
}

func TestRetrievalHygiene_Gate(t *testing.T) {
	rows := []*persistence.Instinct{
		{
			Domain:  persistence.InstinctDomainRetrieval,
			Status:  persistence.InstinctStatusActive,
			Action:  "searching scope docs during role lead correlated with the step outcome",
			Trigger: triggerJSONFor("lead", "", "docs"),
		},
	}

	t.Run("gate off returns empty without calling repo", func(t *testing.T) {
		lister := &fakeInstinctLister{rows: rows}
		h := &RetrievalHygiene{Enabled: false, Instincts: lister}
		hints, err := h.Hints(context.Background(), "proj-1")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if lister.calls != 0 {
			t.Errorf("gate off must not call repo; got %d calls", lister.calls)
		}
		if len(hints.BoostScopes) != 0 || len(hints.PruneChunkIDs) != 0 {
			t.Errorf("gate off must yield empty hints; got %+v", hints)
		}
		if hints.IsBoostScope("docs") {
			t.Error("gate off: IsBoostScope should be false")
		}
	})

	t.Run("nil instincts returns empty", func(t *testing.T) {
		h := &RetrievalHygiene{Enabled: true, Instincts: nil}
		hints, err := h.Hints(context.Background(), "proj-1")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if len(hints.BoostScopes) != 0 || len(hints.PruneChunkIDs) != 0 {
			t.Errorf("nil instincts must yield empty hints; got %+v", hints)
		}
	})

	t.Run("nil receiver is safe", func(t *testing.T) {
		var h *RetrievalHygiene
		hints, err := h.Hints(context.Background(), "proj-1")
		if err != nil || hints == nil {
			t.Fatalf("nil receiver: hints=%v err=%v", hints, err)
		}
	})

	t.Run("gate on lists with retrieval-domain + project filter", func(t *testing.T) {
		lister := &fakeInstinctLister{rows: rows}
		h := &RetrievalHygiene{Enabled: true, Instincts: lister}
		hints, err := h.Hints(context.Background(), "proj-1")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if lister.calls != 1 {
			t.Fatalf("expected 1 list call, got %d", lister.calls)
		}
		if lister.lastFilter.Domain == nil || *lister.lastFilter.Domain != persistence.InstinctDomainRetrieval {
			t.Errorf("filter domain = %v, want retrieval", lister.lastFilter.Domain)
		}
		if lister.lastFilter.ProjectID == nil || *lister.lastFilter.ProjectID != "proj-1" {
			t.Errorf("filter project = %v, want proj-1", lister.lastFilter.ProjectID)
		}
		if lister.lastFilter.PageSize != defaultRetrievalHygieneMaxHints {
			t.Errorf("page size = %d, want %d", lister.lastFilter.PageSize, defaultRetrievalHygieneMaxHints)
		}
		if !hints.IsBoostScope("docs") {
			t.Error("expected docs to be a boost scope")
		}
	})

	t.Run("empty projectID omits project filter", func(t *testing.T) {
		lister := &fakeInstinctLister{rows: rows}
		h := &RetrievalHygiene{Enabled: true, Instincts: lister}
		if _, err := h.Hints(context.Background(), ""); err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if lister.lastFilter.ProjectID != nil {
			t.Errorf("empty project must omit project filter; got %v", *lister.lastFilter.ProjectID)
		}
	})

	t.Run("list error propagates with empty hints", func(t *testing.T) {
		lister := &fakeInstinctLister{err: errors.New("boom")}
		h := &RetrievalHygiene{Enabled: true, Instincts: lister}
		hints, err := h.Hints(context.Background(), "proj-1")
		if err == nil {
			t.Fatal("expected error")
		}
		if len(hints.BoostScopes) != 0 || len(hints.PruneChunkIDs) != 0 {
			t.Errorf("error path must yield empty hints; got %+v", hints)
		}
	})

	t.Run("custom MaxHints honoured", func(t *testing.T) {
		lister := &fakeInstinctLister{rows: rows}
		h := &RetrievalHygiene{Enabled: true, Instincts: lister, MaxHints: 7}
		if _, err := h.Hints(context.Background(), "p"); err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if lister.lastFilter.PageSize != 7 {
			t.Errorf("page size = %d, want 7", lister.lastFilter.PageSize)
		}
	})
}

// TestInstinctChunkScreener pins the extraction-time firewall screen the
// instinct worker uses: sensitive chunk IDs are reported so the worker
// never learns them, and a policy-load error propagates so the worker can
// fail closed.
func TestInstinctChunkScreener(t *testing.T) {
	ctx := context.Background()

	t.Run("reports only sensitive chunks", func(t *testing.T) {
		loader := &fakePolicyLoader{rows: map[string]ChunkPolicyRow{
			"public-chunk":       {ChunkID: "public-chunk", SensitivityTier: "public"},
			"confidential-chunk": {ChunkID: "confidential-chunk", SensitivityTier: "confidential"},
			"restricted-chunk":   {ChunkID: "restricted-chunk", SensitivityTier: "restricted"},
		}}
		s := &InstinctChunkScreener{Policies: loader}
		got, err := s.SensitiveChunks(ctx, []string{"public-chunk", "confidential-chunk", "restricted-chunk"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got["public-chunk"] {
			t.Error("public chunk must not be flagged sensitive")
		}
		if !got["confidential-chunk"] || !got["restricted-chunk"] {
			t.Errorf("confidential/restricted chunks must be flagged: %+v", got)
		}
	})

	t.Run("propagates loader error so the worker fails closed", func(t *testing.T) {
		s := &InstinctChunkScreener{Policies: &fakePolicyLoader{err: errors.New("policy db down")}}
		if _, err := s.SensitiveChunks(ctx, []string{"c1"}); err == nil {
			t.Error("expected the policy-load error to propagate")
		}
	})

	t.Run("nil loader / empty input screens nothing", func(t *testing.T) {
		s := &InstinctChunkScreener{}
		got, err := s.SensitiveChunks(ctx, []string{"c1"})
		if err != nil || len(got) != 0 {
			t.Errorf("nil loader should screen nothing, got %+v err=%v", got, err)
		}
	})
}

func TestRetrievalHygiene_FirewallScreen(t *testing.T) {
	pruneRows := []*persistence.Instinct{
		{
			Domain:  persistence.InstinctDomainRetrieval,
			Status:  persistence.InstinctStatusCandidate,
			Action:  pruneAction("public-chunk"),
			Trigger: triggerJSONFor("", "public-chunk", ""),
		},
		{
			Domain:  persistence.InstinctDomainRetrieval,
			Status:  persistence.InstinctStatusCandidate,
			Action:  pruneAction("confidential-chunk"),
			Trigger: triggerJSONFor("", "confidential-chunk", ""),
		},
		{
			Domain:  persistence.InstinctDomainRetrieval,
			Status:  persistence.InstinctStatusCandidate,
			Action:  pruneAction("restricted-chunk"),
			Trigger: triggerJSONFor("", "restricted-chunk", ""),
		},
		{
			Domain:  persistence.InstinctDomainRetrieval,
			Status:  persistence.InstinctStatusCandidate,
			Action:  pruneAction("credential-chunk"),
			Trigger: triggerJSONFor("", "credential-chunk", ""),
		},
	}

	t.Run("sensitive chunks dropped from prune set", func(t *testing.T) {
		lister := &fakeInstinctLister{rows: pruneRows}
		loader := &fakePolicyLoader{rows: map[string]ChunkPolicyRow{
			"public-chunk":       {ChunkID: "public-chunk", SensitivityTier: "public"},
			"confidential-chunk": {ChunkID: "confidential-chunk", SensitivityTier: "confidential"},
			"restricted-chunk":   {ChunkID: "restricted-chunk", SensitivityTier: "restricted"},
			// credential content-class with no persisted tier: the
			// classifier bridge must bump it to restricted → dropped.
			"credential-chunk": {ChunkID: "credential-chunk", ContentClass: "credentials"},
		}}
		h := &RetrievalHygiene{Enabled: true, Instincts: lister, Policies: loader}
		hints, err := h.Hints(context.Background(), "p")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		assertSet(t, "prune", hints.PruneChunkIDs, []string{"public-chunk"})
	})

	t.Run("policy load error suppresses all prune candidates", func(t *testing.T) {
		lister := &fakeInstinctLister{rows: pruneRows}
		loader := &fakePolicyLoader{err: errors.New("db down")}
		h := &RetrievalHygiene{Enabled: true, Instincts: lister, Policies: loader}
		hints, err := h.Hints(context.Background(), "p")
		if err != nil {
			t.Fatalf("policy load error should be swallowed (fail-closed), got %v", err)
		}
		if len(hints.PruneChunkIDs) != 0 {
			t.Errorf("fail-closed: expected no prune candidates, got %v", keys(hints.PruneChunkIDs))
		}
	})

	t.Run("nil policy loader skips screen", func(t *testing.T) {
		lister := &fakeInstinctLister{rows: pruneRows}
		h := &RetrievalHygiene{Enabled: true, Instincts: lister, Policies: nil}
		hints, err := h.Hints(context.Background(), "p")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		// Without screening, all four prune chunks survive.
		if len(hints.PruneChunkIDs) != 4 {
			t.Errorf("no-screen: want 4 prune chunks, got %v", keys(hints.PruneChunkIDs))
		}
	})
}

func TestChunkIsSensitive(t *testing.T) {
	tests := []struct {
		name string
		row  ChunkPolicyRow
		want bool
	}{
		{"public tier", ChunkPolicyRow{SensitivityTier: "public"}, false},
		{"internal tier", ChunkPolicyRow{SensitivityTier: "internal"}, false},
		{"confidential tier", ChunkPolicyRow{SensitivityTier: "confidential"}, true},
		{"restricted tier", ChunkPolicyRow{SensitivityTier: "restricted"}, true},
		{"credential content-class bumps to restricted", ChunkPolicyRow{ContentClass: "credentials"}, true},
		{"empty row default non-sensitive", ChunkPolicyRow{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := chunkIsSensitive(tc.row); got != tc.want {
				t.Errorf("chunkIsSensitive(%+v) = %v, want %v", tc.row, got, tc.want)
			}
		})
	}
}

// TestConsolidateWorker_HygieneSurfacing verifies the worker counts +
// surfaces hints when the gate is on, and stays inert when off.
func TestConsolidateWorker_HygieneSurfacing(t *testing.T) {
	rows := []*persistence.Instinct{
		{
			Domain:  persistence.InstinctDomainRetrieval,
			Status:  persistence.InstinctStatusActive,
			Action:  "searching scope docs during role lead correlated with the step outcome",
			Trigger: triggerJSONFor("lead", "", "docs"),
		},
		{
			Domain:  persistence.InstinctDomainRetrieval,
			Status:  persistence.InstinctStatusCandidate,
			Action:  pruneAction("dead-chunk"),
			Trigger: triggerJSONFor("", "dead-chunk", ""),
		},
	}

	t.Run("gate on increments boost + prune counters", func(t *testing.T) {
		reg := prometheus.NewRegistry()
		m := NewMetrics(reg)
		w := &ConsolidateWorker{
			Logger:  zerolog.Nop(),
			Metrics: m,
			Hygiene: &RetrievalHygiene{
				Enabled:   true,
				Instincts: &fakeInstinctLister{rows: rows},
			},
		}
		w.surfaceHygieneHints(context.Background(), "proj-1")
		if got := testutil.ToFloat64(m.HygieneCandidates.WithLabelValues("proj-1", "boost")); got != 1 {
			t.Errorf("boost counter = %v, want 1", got)
		}
		if got := testutil.ToFloat64(m.HygieneCandidates.WithLabelValues("proj-1", "prune")); got != 1 {
			t.Errorf("prune counter = %v, want 1", got)
		}
	})

	t.Run("gate off is a no-op (no repo call, no counter)", func(t *testing.T) {
		reg := prometheus.NewRegistry()
		m := NewMetrics(reg)
		lister := &fakeInstinctLister{rows: rows}
		w := &ConsolidateWorker{
			Logger:  zerolog.Nop(),
			Metrics: m,
			Hygiene: &RetrievalHygiene{Enabled: false, Instincts: lister},
		}
		w.surfaceHygieneHints(context.Background(), "proj-1")
		if lister.calls != 0 {
			t.Errorf("gate off must not call repo; got %d", lister.calls)
		}
		if got := testutil.ToFloat64(m.HygieneCandidates.WithLabelValues("proj-1", "boost")); got != 0 {
			t.Errorf("gate off boost counter = %v, want 0", got)
		}
	})

	t.Run("nil hygiene is a no-op", func(t *testing.T) {
		w := &ConsolidateWorker{Logger: zerolog.Nop(), Hygiene: nil}
		// Must not panic.
		w.surfaceHygieneHints(context.Background(), "proj-1")
	})

	t.Run("hint fetch error is swallowed", func(t *testing.T) {
		reg := prometheus.NewRegistry()
		m := NewMetrics(reg)
		w := &ConsolidateWorker{
			Logger:  zerolog.Nop(),
			Metrics: m,
			Hygiene: &RetrievalHygiene{
				Enabled:   true,
				Instincts: &fakeInstinctLister{err: errors.New("boom")},
			},
		}
		// Must not panic / must not increment.
		w.surfaceHygieneHints(context.Background(), "proj-1")
		if got := testutil.ToFloat64(m.HygieneCandidates.WithLabelValues("proj-1", "boost")); got != 0 {
			t.Errorf("error path boost counter = %v, want 0", got)
		}
	})
}
