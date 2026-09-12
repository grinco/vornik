package hallucination

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
)

// The gate tests below reuse fakeVerdictRepo (runner_test.go) for Verdicts
// rather than defining a second double: its GetByTask already returns
// (nil, persistence.ErrNotFound), which is exactly "nothing recorded yet"
// — the idempotency short-circuit in Run never fires, same as a purpose-
// built stub would give, with zero new code (brief: task-4-brief.md:128).

// TestFakeVerdictRepo_MissContract pins fakeVerdictRepo.GetByTask against
// the same absence contract production obeys (persistence/misscontract):
// a repository double that answers a miss differently than production
// would certify every test that exercises Run's miss path (the
// GetByTask idempotency check above, and the gate tests below that reuse
// this same double) without ever executing production's real branch. See
// internal/contractreg's repo-double-conformance lint.
func TestFakeVerdictRepo_MissContract(t *testing.T) {
	repotest.AssertMiss(t, "TaskJudgeVerdictRepository.GetByTask", func() (*persistence.TaskJudgeVerdict, error) {
		return (&fakeVerdictRepo{}).GetByTask(context.Background(), "task-absent")
	})
}

type stubChildLister struct {
	children []*persistence.Task
	calls    int
}

func (s *stubChildLister) GetChildren(_ context.Context, _ string) ([]*persistence.Task, error) {
	s.calls++
	return s.children, nil
}

type countingJudge struct{ calls int }

func (c *countingJudge) Evaluate(_ context.Context, _ JudgeInput) (*Verdict, *JudgeMetrics, error) {
	c.calls++
	return &Verdict{Decision: "pass"}, &JudgeMetrics{}, nil
}

func delegationMode(s string) *persistence.DelegationMode {
	m := persistence.DelegationMode(s)
	return &m
}

// Incident 2026-09-10: 40 adaptive parents scored 17 fail / 21 abstain /
// 2 pass because JudgeRunner filters artifacts by the PARENT's TaskID
// (runner.go:64-68) while the researcher's artifacts live in the child.
// A control that fails on everything reports nothing.
func TestRun_SkipsDelegatingParent(t *testing.T) {
	judge := &countingJudge{}
	children := &stubChildLister{children: []*persistence.Task{
		{ID: "child1", DelegationMode: delegationMode("JOIN")},
	}}
	r := &JudgeRunner{
		Judge:    judge,
		Verdicts: &fakeVerdictRepo{},
		Children: children,
		Logger:   zerolog.Nop(),
	}

	if err := r.Run(context.Background(), &persistence.Task{ID: "parent1", ProjectID: "assistant"}); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if judge.calls != 0 {
		t.Errorf("judge was invoked %d times on a delegating parent, want 0", judge.calls)
	}
}

// A parent that failed BEFORE creating a child (malformed delegatedTasks,
// a fanout/depth/cycle guard rejection, route_invalid_pick) has no
// children, so the gate must not match and it is judged exactly as today.
func TestRun_JudgesParentThatNeverDelegated(t *testing.T) {
	judge := &countingJudge{}
	r := &JudgeRunner{
		Judge:    judge,
		Verdicts: &fakeVerdictRepo{},
		Children: &stubChildLister{children: nil},
		Logger:   zerolog.Nop(),
	}

	if err := r.Run(context.Background(), &persistence.Task{ID: "parent2", ProjectID: "assistant"}); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if judge.calls != 1 {
		t.Errorf("judge calls = %d, want 1 -- a pre-delegation failure must still be judged", judge.calls)
	}
}

// Checkpoint retries and call_project/spawn_project callees leave
// DelegationMode nil, so they are NOT delegation-engine children and
// must not suppress judging.
func TestRun_NonDelegationChildrenDoNotSuppress(t *testing.T) {
	judge := &countingJudge{}
	r := &JudgeRunner{
		Judge:    judge,
		Verdicts: &fakeVerdictRepo{},
		Children: &stubChildLister{children: []*persistence.Task{{ID: "callee", DelegationMode: nil}}},
		Logger:   zerolog.Nop(),
	}

	if err := r.Run(context.Background(), &persistence.Task{ID: "parent3", ProjectID: "assistant"}); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if judge.calls != 1 {
		t.Errorf("judge calls = %d, want 1 -- a nil DelegationMode child is not a delegation", judge.calls)
	}
}

// An unwired Children field must not change behaviour: minimal
// deployments and existing tests construct JudgeRunner without it.
func TestRun_NilChildListerJudgesAsBefore(t *testing.T) {
	judge := &countingJudge{}
	r := &JudgeRunner{Judge: judge, Verdicts: &fakeVerdictRepo{}, Logger: zerolog.Nop()}

	if err := r.Run(context.Background(), &persistence.Task{ID: "p4", ProjectID: "assistant"}); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if judge.calls != 1 {
		t.Errorf("judge calls = %d, want 1 when Children is nil", judge.calls)
	}
}

// Gate PLACEMENT, not gate logic (review finding 9, 2026-09-11). The
// gate originally sat ABOVE the GetByTask idempotency short-circuit, so
// two things regressed that nothing asserted: every completed task paid a
// GetChildren round-trip even when a verdict was already on file, and a
// delegating parent that had already been judged recorded
// `skipped_delegated` instead of the `skipped_existing` it recorded
// before the gate existed. Both are observable here: with a verdict
// already recorded, Run must short-circuit before touching the child
// lister at all.
func TestRun_ExistingVerdictShortCircuitsBeforeChildLookup(t *testing.T) {
	judge := &countingJudge{}
	children := &stubChildLister{children: []*persistence.Task{
		{ID: "child1", DelegationMode: delegationMode("JOIN")},
	}}
	verdicts := &fakeVerdictRepo{
		existing: &persistence.TaskJudgeVerdict{TaskID: "parent5", Verdict: persistence.JudgeVerdictPass},
	}
	reg := prometheus.NewRegistry()
	r := &JudgeRunner{
		Judge:    judge,
		Verdicts: verdicts,
		Children: children,
		Metrics:  NewMetrics(reg),
		Logger:   zerolog.Nop(),
	}

	if err := r.Run(context.Background(), &persistence.Task{ID: "parent5", ProjectID: "assistant"}); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if judge.calls != 0 {
		t.Errorf("judge calls = %d, want 0 -- a recorded verdict is idempotent", judge.calls)
	}
	if children.calls != 0 {
		t.Errorf("GetChildren called %d times, want 0 -- the idempotency short-circuit must come first",
			children.calls)
	}
	if got := testutil.ToFloat64(
		r.Metrics.JudgeEvaluationsTotal.WithLabelValues("assistant", "skipped_existing")); got != 1 {
		t.Errorf("skipped_existing = %v, want 1", got)
	}
	if got := testutil.ToFloat64(
		r.Metrics.JudgeEvaluationsTotal.WithLabelValues("assistant", "skipped_delegated")); got != 0 {
		t.Errorf("skipped_delegated = %v, want 0 -- an already-judged parent is 'existing', not 'delegated'", got)
	}
}
