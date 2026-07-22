package quality

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

type fakeRepo struct {
	roles []RoleAggregate
	tasks []TaskAggregate
}

func (f *fakeRepo) RoleQualityAggregates(context.Context, time.Time) ([]RoleAggregate, error) {
	return f.roles, nil
}
func (f *fakeRepo) TaskQualityAggregates(context.Context, time.Time) ([]TaskAggregate, error) {
	return f.tasks, nil
}

// Refresh folds shared-swarm projects, scores both tiers, and publishes the
// gauges — the end-to-end Phase-1 observe path (design §A).
func TestServiceRefreshScoresBothTiersAndSetsGauges(t *testing.T) {
	repo := &fakeRepo{
		roles: []RoleAggregate{
			{ProjectID: "assistant", Role: "researcher", Total: 60, Passing: 48, PromptTokens: 480_000},
			{ProjectID: "janka", Role: "researcher", Total: 40, Passing: 32, PromptTokens: 320_000},
		},
		tasks: []TaskAggregate{
			{ProjectID: "assistant", WorkflowID: "research", Total: 100, Passing: 80, PromptTokens: 8_000_000},
		},
	}
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	swarmOf := func(p string) string {
		if p == "assistant" || p == "janka" {
			return "assistant-swarm"
		}
		return ""
	}
	svc := NewService(repo, swarmOf, m, Config{StepMinSample: 20, TaskMinSample: 10})

	rep, err := svc.Refresh(context.Background(), time.Now().Add(-720*time.Hour))
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	// A1 folded (assistant-swarm, researcher): 100 steps, 80 passing → 0.8, 800000/80=10000
	r := findRole(rep.Steps, "assistant-swarm", "researcher")
	if r == nil || r.QualityRate != 0.8 || r.EffectiveCostTokens != 10_000 || !r.Sufficient {
		t.Fatalf("A1 researcher = %+v, want rate 0.8 cost 10000 sufficient", r)
	}
	// A2 (assistant-swarm, research): 100 tasks, 80 passing → 0.8, 8000000/80=100000
	w := findWorkflow(rep.Tasks, "assistant-swarm", "research")
	if w == nil || w.QualityRate != 0.8 || w.EffectiveCostTokens != 100_000 {
		t.Fatalf("A2 research = %+v, want rate 0.8 cost 100000", w)
	}

	// gauges published with the folded values
	got := testutil.ToFloat64(m.QualityScore.WithLabelValues("step", "assistant-swarm", "researcher"))
	if got != 0.8 {
		t.Errorf("gauge quality_score{step,assistant-swarm,researcher} = %v, want 0.8", got)
	}
	gotCost := testutil.ToFloat64(m.EffectiveCostTokens.WithLabelValues("task", "assistant-swarm", "research"))
	if gotCost != 100_000 {
		t.Errorf("gauge effective_cost_tokens{task,assistant-swarm,research} = %v, want 100000", gotCost)
	}
}

// A below-floor series is still published (gauge continuity) but with
// Sufficient=0; its rate/cost are undefined and must not be consumed as real
// (design consumer contract, review-20260721-78d1 #10).
func TestServiceRefreshPublishesInsufficientWithSufficientZero(t *testing.T) {
	repo := &fakeRepo{
		roles: []RoleAggregate{
			{ProjectID: "assistant", Role: "vision", Total: 3, Passing: 3, PromptTokens: 9000},
		},
	}
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	svc := NewService(repo, func(string) string { return "assistant-swarm" }, m, Config{StepMinSample: 20, TaskMinSample: 20})

	rep, err := svc.Refresh(context.Background(), time.Now().Add(-720*time.Hour))
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	r := findRole(rep.Steps, "assistant-swarm", "vision")
	if r == nil || r.Sufficient {
		t.Fatalf("vision (3 samples < floor 20) should be insufficient, got %+v", r)
	}
	if got := testutil.ToFloat64(m.Sufficient.WithLabelValues("step", "assistant-swarm", "vision")); got != 0 {
		t.Errorf("gauge sufficient = %v, want 0 for below-floor series", got)
	}
	// Consumer contract (service.go godoc): the rate/cost are still PUBLISHED
	// (as 0.0) for the insufficient series — verify the series exist, not just
	// that ToFloat64 returns 0 (which it would for an absent series too).
	if n := testutil.CollectAndCount(m.QualityScore); n != 1 {
		t.Errorf("QualityScore series count = %d, want 1 (insufficient series still published)", n)
	}
	if n := testutil.CollectAndCount(m.EffectiveCostTokens); n != 1 {
		t.Errorf("EffectiveCostTokens series count = %d, want 1", n)
	}
}

func findRole(rs []ScoredSwarmRole, swarm, role string) *ScoredSwarmRole {
	for i := range rs {
		if rs[i].Swarm == swarm && rs[i].Role == role {
			return &rs[i]
		}
	}
	return nil
}
func findWorkflow(ws []ScoredSwarmWorkflow, swarm, workflow string) *ScoredSwarmWorkflow {
	for i := range ws {
		if ws[i].Swarm == swarm && ws[i].Workflow == workflow {
			return &ws[i]
		}
	}
	return nil
}
