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
func (f *fakeRepo) RoleQualityAggregatesBetween(context.Context, time.Time, time.Time) ([]RoleAggregate, error) {
	return f.roles, nil
}
func (f *fakeRepo) TaskQualityAggregatesBetween(context.Context, time.Time, time.Time) ([]TaskAggregate, error) {
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

// rawStep is one raw fixture step row with a timestamp, so the windowedRepo can
// filter by [from,to) exactly as the SQL would. Used to prove RefreshBetween's
// bound (design I4).
type rawStep struct {
	Project      string
	Role         string
	At           time.Time
	Passing      bool
	PromptTokens int64
}

// windowedRepo is a time-aware fake Repo that folds raw rows into aggregates,
// honouring the [from,to) bound in the *Between methods and [since, now] in the
// unbounded ones — the ground truth I4 checks RefreshBetween against.
type windowedRepo struct{ steps []rawStep }

func (r *windowedRepo) aggregate(from time.Time, hasTo bool, to time.Time) []RoleAggregate {
	type key struct{ p, role string }
	acc := map[key]*RoleAggregate{}
	for _, s := range r.steps {
		if s.At.Before(from) || (hasTo && !s.At.Before(to)) {
			continue
		}
		k := key{s.Project, s.Role}
		a := acc[k]
		if a == nil {
			a = &RoleAggregate{ProjectID: s.Project, Role: s.Role}
			acc[k] = a
		}
		a.Total++
		if s.Passing {
			a.Passing++
			a.PromptTokens += s.PromptTokens
		}
	}
	out := make([]RoleAggregate, 0, len(acc))
	for _, a := range acc {
		out = append(out, *a)
	}
	return out
}
func (r *windowedRepo) RoleQualityAggregates(_ context.Context, since time.Time) ([]RoleAggregate, error) {
	return r.aggregate(since, false, time.Time{}), nil
}
func (r *windowedRepo) TaskQualityAggregates(context.Context, time.Time) ([]TaskAggregate, error) {
	return nil, nil
}
func (r *windowedRepo) RoleQualityAggregatesBetween(_ context.Context, from, to time.Time) ([]RoleAggregate, error) {
	return r.aggregate(from, true, to), nil
}
func (r *windowedRepo) TaskQualityAggregatesBetween(context.Context, time.Time, time.Time) ([]TaskAggregate, error) {
	return nil, nil
}

// TestRefreshBetween_BoundedAndGroundTruth covers design I4: (a) RefreshBetween
// equals Refresh when the window spans all rows; (b) an independent recompute of
// the bounded score from the raw rows matches; (c) rows outside [from,to) are
// excluded.
func TestRefreshBetween_BoundedAndGroundTruth(t *testing.T) {
	t0 := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	repo := &windowedRepo{steps: []rawStep{
		// In [t0, t0+10h): 30 steps, 24 passing.
		{Project: "assistant", Role: "researcher", At: t0.Add(1 * time.Hour), Passing: true, PromptTokens: 1000},
		{Project: "assistant", Role: "researcher", At: t0.Add(2 * time.Hour), Passing: false},
		// A big batch to clear the min-sample floor deterministically.
	}}
	// Fill the in-window batch: 40 passing (tokens 1000 each) + 10 failing = 50 total.
	for i := 0; i < 40; i++ {
		repo.steps = append(repo.steps, rawStep{Project: "assistant", Role: "researcher", At: t0.Add(3 * time.Hour), Passing: true, PromptTokens: 1000})
	}
	for i := 0; i < 10; i++ {
		repo.steps = append(repo.steps, rawStep{Project: "assistant", Role: "researcher", At: t0.Add(4 * time.Hour), Passing: false})
	}
	// OUT of [t0, t0+10h): a burst AFTER the window that must be excluded.
	for i := 0; i < 100; i++ {
		repo.steps = append(repo.steps, rawStep{Project: "assistant", Role: "researcher", At: t0.Add(50 * time.Hour), Passing: false})
	}
	swarmOf := func(string) string { return "assistant-swarm" }
	svc := NewService(repo, swarmOf, nil, Config{StepMinSample: 20, TaskMinSample: 20})

	// (b)+(c): bounded window [t0, t0+10h) → 42 passing / 52 total.
	from, to := t0, t0.Add(10*time.Hour)
	rep, err := svc.RefreshBetween(context.Background(), from, to)
	if err != nil {
		t.Fatalf("RefreshBetween: %v", err)
	}
	r := findRole(rep.Steps, "assistant-swarm", "researcher")
	if r == nil {
		t.Fatal("researcher series missing")
	}
	// Independent ground-truth recompute from the raw in-range rows.
	var pass, total, tokens int64
	for _, s := range repo.steps {
		if s.At.Before(from) || !s.At.Before(to) {
			continue
		}
		total++
		if s.Passing {
			pass++
			tokens += s.PromptTokens
		}
	}
	wantRate := float64(pass) / float64(total)
	wantCost := float64(tokens) / float64(pass)
	if r.QualityRate != wantRate || r.EffectiveCostTokens != wantCost || r.SampleCount != total {
		t.Fatalf("bounded score = rate %v cost %v n %d; ground truth rate %v cost %v n %d",
			r.QualityRate, r.EffectiveCostTokens, r.SampleCount, wantRate, wantCost, total)
	}
	if total != 52 {
		t.Fatalf("out-of-range rows leaked: total=%d, want 52 (the 100 post-window rows excluded)", total)
	}

	// (a): a window spanning everything equals the unbounded Refresh.
	repFull, _ := svc.Refresh(context.Background(), t0)
	repBetween, _ := svc.RefreshBetween(context.Background(), t0, t0.Add(1000*time.Hour))
	rf := findRole(repFull.Steps, "assistant-swarm", "researcher")
	rb := findRole(repBetween.Steps, "assistant-swarm", "researcher")
	if rf == nil || rb == nil || rf.QualityRate != rb.QualityRate || rf.SampleCount != rb.SampleCount {
		t.Fatalf("RefreshBetween(all) != Refresh: full=%+v between=%+v", rf, rb)
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
