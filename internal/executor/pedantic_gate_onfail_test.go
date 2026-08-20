package executor

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// Regression, measured live on 2026-08-19 during the 2026.8.8 profiling arm.
//
// pedantic routes on_fail straight to the FAILED terminal — that is what the
// daemon announces at every pedantic execution start, and `effectiveOnFail` is
// documented as "the uniform on_fail resolver". It was not uniform. Seven
// ordinary failure transitions went through it; the two GATE transitions
// assigned `step.OnFail` raw:
//
//	workflow.go:580  gate evaluation failed (classifyGateEvalError path)
//	workflow.go:591  no on_success and no gate matched
//
// So a gate failure still took the recovery hop under pedantic. On dev-pipeline
// that hop reaches `recover-checkpoint` and then the `checkpoint` terminal,
// whose status is COMPLETED BY DESIGN — so a task whose review satisfied no
// gate reported COMPLETED. The agentbench project sets pedantic: true precisely
// to stop that; its config comment records an earlier sweep scoring dev-pipeline
// 3/3 on task status with all three contracts unmet.
//
// Measured in the abandoned arm: `review` was downstream_rejected in 14 of 16
// attempts (error_class gate_eval_failed = "no gate condition matched"), 16
// recover-checkpoint steps executed, and 15 tasks reported COMPLETED.
//
// The existing pedantic tests all exercise `pedanticOnFail` directly, which was
// never the broken part — it works. Nothing asserted that the gate paths CALL
// it, which is why this shipped.

// gatedResolver builds a workflow whose only step carries a gate the agent's
// output will not satisfy, and whose on_fail is a recovery STEP rather than a
// terminal — the shape pedantic is supposed to rewrite.
func gatedResolver(pedantic *bool) *MockWorkflowResolver {
	return &MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "gated-test", Pedantic: pedantic},
		},
		swarms: map[string]*registry.Swarm{
			"s1": {ID: "s1", Roles: []registry.SwarmRole{
				{Name: "researcher", Runtime: registry.SwarmRoleRuntime{Image: "fake-agent:latest"}},
				{Name: "lead", Runtime: registry.SwarmRoleRuntime{Image: "fake-agent:latest"}},
			}},
		},
		workflows: map[string]*registry.Workflow{
			"gated-test": {
				ID:         "gated-test",
				Entrypoint: "work",
				Steps: map[string]registry.WorkflowStep{
					"work": {
						Type: "agent", Role: "researcher",
						// No OnSuccess: the gate is the only way forward, so
						// output matching no condition is a gate-eval failure.
						Gates:  []registry.WorkflowGate{{Condition: "review.approved == true", Target: "done"}},
						OnFail: "recover",
					},
					"recover": {Type: "agent", Role: "lead", OnSuccess: "done", OnFail: "failed"},
				},
				Terminals: map[string]registry.WorkflowTerminal{
					"done":   {Status: "COMPLETED"},
					"failed": {Status: "FAILED"},
				},
			},
		},
	}
}

func runGatedTask(t *testing.T, pedantic *bool) (*MockRuntime, *MockTaskRepo, string) {
	t.Helper()
	rt := NewMockRuntime()
	// The step SUCCEEDS and emits output that satisfies no gate condition.
	// That is a gate-evaluation failure, not a step failure — the distinction
	// the two uncovered transitions turn on.
	rt.outputJSON = `{"review":{"approved":false}}`
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, ar, tr, nil)
	e.config.RetryDelay = 0
	e.SetWorkflowResolver(gatedResolver(pedantic))

	id := "t-gated"
	tr.AddTask(&persistence.Task{
		ID: id, ProjectID: "p1", Status: persistence.TaskStatusLeased,
		Attempt: 1, MaxAttempts: 1,
		Payload:   []byte(`{"context":{"prompt":"do the thing"}}`),
		CreatedAt: time.Now(),
	})
	require.NoError(t, e.Execute(id))
	return rt, tr, id
}

// Under pedantic, a gate-evaluation failure must reach the FAILED terminal
// WITHOUT running the recovery step.
func TestPedantic_GateFailureSkipsTheRecoveryHop(t *testing.T) {
	pedanticTrue := true
	rt, tr, id := runGatedTask(t, &pedanticTrue)

	assert.Eventually(t, func() bool {
		task, _ := tr.Get(context.Background(), id)
		return task != nil && task.Status == persistence.TaskStatusFailed
	}, 3*time.Second, 10*time.Millisecond,
		"a pedantic project must FAIL on a gate-evaluation failure, not route through recovery")

	rt.mu.Lock()
	calls := rt.startCalls
	rt.mu.Unlock()
	assert.Equal(t, 1, calls,
		"only the gated step may run: pedantic routes its gate failure straight to the "+
			"failed terminal, so the recovery step must never start. Two calls means the "+
			"recovery hop ran and — on dev-pipeline — would have reached a COMPLETED terminal")
}

// The same workflow WITHOUT pedantic still takes the recovery hop. Without
// this, the fix could satisfy the test above by breaking recovery for everyone.
//
// Waits on the count rather than reading it straight after Execute: the
// recovery step starts asynchronously, and sampling too early reports 1 for
// both the fixed and broken builds.
func TestNonPedantic_GateFailureStillTakesTheRecoveryHop(t *testing.T) {
	rt, tr, id := runGatedTask(t, nil)

	assert.Eventually(t, func() bool {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		return rt.startCalls == 2
	}, 3*time.Second, 10*time.Millisecond,
		"a non-pedantic project must still route a gate failure to its recovery step")

	task, _ := tr.Get(context.Background(), id)
	require.NotNil(t, task)
	assert.Equal(t, persistence.TaskStatusCompleted, task.Status,
		"the recovery step succeeds in this fixture, so the non-pedantic path ends COMPLETED")
}

// Guard against the whole class, not just the two sites fixed on 2026-08-19.
//
// This bug existed because someone added a transition that assigned
// `step.OnFail` straight to the next-step variable, bypassing the resolver.
// Nothing objected: the pure function had tests, the ordinary paths used it,
// and the new path simply did not. A behavioural test catches the gate
// transitions; only a structural one catches the NEXT transition somebody adds.
//
// Two sites legitimately read step.OnFail into a local first, because they need
// the destination before the assignment (see the comment on effectiveOnFail).
// They resolve pedantic themselves. Assigning it directly to the step variable
// is what must not reappear.
func TestNoRawOnFailTransitionBypassesTheResolver(t *testing.T) {
	src, err := os.ReadFile("workflow.go")
	require.NoError(t, err)

	raw := regexp.MustCompile(`(?m)^\s*(nextStepID|currentStepID)\s*=\s*step\.OnFail\s*$`)
	if hits := raw.FindAllString(string(src), -1); len(hits) > 0 {
		t.Errorf("%d on_fail transition(s) assign step.OnFail directly instead of calling "+
			"e.effectiveOnFail(...):\n%s\n"+
			"Every failure transition must go through the resolver, or pedantic silently "+
			"stops applying to it — which is how a pedantic dev-pipeline ran "+
			"recover-checkpoint and reported COMPLETED with its contract unmet.",
			len(hits), strings.Join(hits, "\n"))
	}
}
