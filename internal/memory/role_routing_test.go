package memory

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestApplyRoleClassBoost_EmptyRolePassThrough(t *testing.T) {
	in := []SearchResult{{ChunkID: "a", Score: 0.5}, {ChunkID: "b", Score: 0.4}}
	out := applyRoleClassBoost(in, "", 0)
	if out[0].ChunkID != "a" || out[0].Score != 0.5 {
		t.Fatalf("empty role: %+v", out)
	}
}

func TestApplyRoleClassBoost_SingleResult(t *testing.T) {
	in := []SearchResult{{ChunkID: "a", Score: 0.5, ContentClass: string(ClassSpec)}}
	out := applyRoleClassBoost(in, "coder", 0)
	if out[0].Score != 0.5 {
		t.Fatalf("single result must not be boosted (no reranking happens): %v", out[0].Score)
	}
}

func TestApplyRoleClassBoost_UnknownRolePassThrough(t *testing.T) {
	in := []SearchResult{
		{ChunkID: "a", Score: 0.5},
		{ChunkID: "b", Score: 0.4},
	}
	out := applyRoleClassBoost(in, "not-a-real-role", 0)
	if out[0].ChunkID != "a" || out[1].ChunkID != "b" {
		t.Fatalf("unknown role: %+v", out)
	}
}

func TestApplyRoleClassBoost_LiftsPreferredClass(t *testing.T) {
	// Coder strongly prefers Spec. The Spec result starts below the
	// unclassified result and must overtake it after boost.
	in := []SearchResult{
		{ChunkID: "unclass", Score: 0.6, ContentClass: string(ClassUnclassified)},
		{ChunkID: "spec", Score: 0.4, ContentClass: string(ClassSpec)},
	}
	out := applyRoleClassBoost(in, "coder", 0)
	if out[0].ChunkID != "spec" {
		t.Fatalf("expected spec to lift: %+v", out)
	}
}

func TestApplyRoleClassBoost_PreservesStableOrderOnTies(t *testing.T) {
	in := []SearchResult{
		{ChunkID: "a", Score: 0.5, ContentClass: string(ClassDecision)},
		{ChunkID: "b", Score: 0.5, ContentClass: string(ClassDecision)},
	}
	out := applyRoleClassBoost(in, "reviewer", 0)
	if out[0].ChunkID != "a" || out[1].ChunkID != "b" {
		t.Fatalf("stable sort lost order: %+v", out)
	}
}

func TestApplyRoleClassBoost_AmplitudeOverride(t *testing.T) {
	in := []SearchResult{
		{ChunkID: "spec", Score: 0.4, ContentClass: string(ClassSpec)},
		{ChunkID: "unclass", Score: 0.5, ContentClass: string(ClassUnclassified)},
	}
	// With amplitude=2, the spec boost is doubled — should clearly
	// overtake the unclassified entry.
	out := applyRoleClassBoost(in, "coder", 2.0)
	if out[0].ChunkID != "spec" {
		t.Fatalf("amplitude not applied: %+v", out)
	}
}

func TestSearch_RoleBoostAppliedFromContext(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	s := NewSearcher(Config{}, r, nil)
	s.setMetrics(freshMetrics())

	// 7-column shape so the role boost has class info to act on.
	rows := sqlmock.NewRows([]string{
		"id", "project_id", "task_id", "source_name", "content", "score", "content_class",
	}).
		AddRow("a", "p", "t", "s", "alpha", 0.6, string(ClassUnclassified)).
		AddRow("b", "p", "t", "s", "beta", 0.4, string(ClassSpec))
	mock.ExpectQuery("ts_rank").WillReturnRows(rows)

	ctx := WithRetrievalContext(context.Background(), &RetrievalContext{Role: "coder"})
	out, err := s.Search(ctx, "p", "q", 5)
	if err != nil {
		t.Fatal(err)
	}
	if out[0].ChunkID != "b" {
		t.Fatalf("role boost not applied: %+v", out)
	}
}

func TestSearch_NullContentClassHandledCleanly(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	s := NewSearcher(Config{}, r, nil)

	rows := sqlmock.NewRows([]string{
		"id", "project_id", "task_id", "source_name", "content", "score", "content_class",
	}).
		AddRow("a", "p", "t", "s", "alpha", 0.6, nil). // NULL class
		AddRow("b", "p", "t", "s", "beta", 0.4, "spec")
	mock.ExpectQuery("ts_rank").WillReturnRows(rows)

	out, err := s.Search(context.Background(), "p", "q", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0].ContentClass != "" || out[1].ContentClass != "spec" {
		t.Fatalf("class scan: %+v", out)
	}
}

func TestSearch_ScanRowErrorPropagates(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	s := NewSearcher(Config{}, r, nil)

	// Type mismatch on the score column → Scan fails on row 1.
	rows := sqlmock.NewRows([]string{
		"id", "project_id", "task_id", "source_name", "content", "score", "content_class",
	}).AddRow("a", "p", "t", "s", "alpha", "not-a-float", "spec")
	mock.ExpectQuery("ts_rank").WillReturnRows(rows)

	if _, err := s.Search(context.Background(), "p", "q", 5); err == nil {
		t.Fatal("want scan err")
	}
}

// TestApplyRoleClassBoost_WriterPrefersDecision — B1 regression guard:
// with the raised ClassDecision=1.0 on "writer", an authoritative
// decision-class chunk (score 0.60) must outrank a higher-raw-score
// research chunk (score 0.70) after boost.
// writer: research 0.70×(1+1.0×0.7)=1.19, decision 0.60×(1+1.0×1.0)=1.20 → decision wins.
func TestApplyRoleClassBoost_WriterPrefersDecision(t *testing.T) {
	in := []SearchResult{
		{ChunkID: "other", ContentClass: "research", Score: 0.70},
		{ChunkID: "resume", ContentClass: "decision", Score: 0.60},
	}
	out := applyRoleClassBoost(in, "writer", 0)
	require.Equal(t, "resume", out[0].ChunkID) // decision boosted above the higher-raw research hit
}

func TestSearch_NoRoleContextNoBoost(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	s := NewSearcher(Config{}, r, nil)
	s.setMetrics(freshMetrics())

	rows := sqlmock.NewRows([]string{
		"id", "project_id", "task_id", "source_name", "content", "score", "content_class",
	}).
		AddRow("a", "p", "t", "s", "alpha", 0.6, string(ClassUnclassified)).
		AddRow("b", "p", "t", "s", "beta", 0.4, string(ClassSpec))
	mock.ExpectQuery("ts_rank").WillReturnRows(rows)

	out, _ := s.Search(context.Background(), "p", "q", 5)
	if out[0].ChunkID != "a" {
		t.Fatalf("expected original order with no role: %+v", out)
	}
}
