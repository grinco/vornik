package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// The rollup on the MCP surfaces
// (LLD 2026-09-08-execution-ratings-approval-paths-design §2.3). An agent
// holding skill_admin approves through this path, so it is a gate like any
// other and must not be blind.

type stubRatingArms struct {
	rows []persistence.SkillRatingArms
	sha  string
}

func (s *stubRatingArms) SkillRatingArms(_ context.Context, _ string, _ time.Time) ([]persistence.SkillRatingArms, error) {
	return s.rows, nil
}

func (s *stubRatingArms) SkillRatingArmsForBody(_ context.Context, _, sha string, _ time.Time) ([]persistence.SkillRatingArms, error) {
	s.sha = sha
	return s.rows, nil
}

type stubProvenance struct {
	prov persistence.InjectionProvenance
}

func (s stubProvenance) Record(_ context.Context, _, _, _ string) error { return nil }
func (s stubProvenance) ListByExecution(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}
func (s stubProvenance) SkillInjectionProvenance(_ context.Context, _, _ string, _ time.Time) (persistence.InjectionProvenance, error) {
	return s.prov, nil
}

func TestSkillRatingLine(t *testing.T) {
	sk := &persistence.Skill{ID: "s1", BodySHA256: "sha-under-review"}

	t.Run("a fresh draft says it was never injected, never nothing", func(t *testing.T) {
		arms := &stubRatingArms{}
		s := &Server{ratingRollupRepo: arms, execSkillRepo: stubProvenance{}}
		got := s.skillRatingLine(context.Background(), sk)
		if got == "" {
			t.Fatal("a wired daemon must never render an empty rollup field")
		}
		if arms.sha != "sha-under-review" {
			t.Errorf("the arms must be scoped to the body being approved, got sha %q", arms.sha)
		}
	})

	t.Run("ratings for an earlier body are named as such", func(t *testing.T) {
		s := &Server{
			ratingRollupRepo: &stubRatingArms{},
			execSkillRepo:    stubProvenance{prov: persistence.InjectionProvenance{OtherBodyN: 30}},
		}
		got := s.skillRatingLine(context.Background(), sk)
		if got == "" || !strings.Contains(got, "earlier body") {
			t.Errorf("got %q, want the superseded-body sentence", got)
		}
	})

	t.Run("nothing wired omits the field entirely", func(t *testing.T) {
		s := &Server{}
		if got := s.skillRatingLine(context.Background(), sk); got != "" {
			t.Errorf("got %q, want empty when no rollup is wired", got)
		}
	})
}

// skillSummaryMap must preserve every field of the summary it re-encodes —
// it exists only so the rollup can sit beside them.
func TestSkillSummaryMapPreservesTheSummary(t *testing.T) {
	sum := toSkillSummary(&persistence.Skill{
		ID: "s1", Name: "n", Description: "d", Maturity: "draft", Version: 2,
	})
	m, err := skillSummaryMap(sum)
	if err != nil {
		t.Fatalf("skillSummaryMap: %v", err)
	}
	direct, _ := json.Marshal(sum)
	viaMap, _ := json.Marshal(m)
	var a, b map[string]any
	_ = json.Unmarshal(direct, &a)
	_ = json.Unmarshal(viaMap, &b)
	if len(a) != len(b) {
		t.Errorf("round trip changed the field set: %v vs %v", a, b)
	}
	for k, v := range a {
		if fmt := b[k]; fmt != v {
			t.Errorf("field %q changed: %v -> %v", k, v, fmt)
		}
	}
}
