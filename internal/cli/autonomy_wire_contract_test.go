package cli

// API <-> CLI WIRE CONTRACT for GET /autonomy/health.
//
// internal/cli re-declares the endpoint's JSON shape (healthFeedRow,
// healthDeliveryRow, judgeBlock, healthPayload) mirroring the structs in
// internal/api/autonomy_handlers.go. Before this test, the two halves
// were only ever exercised apart: the CLI tests hand-build a
// healthPayload, and the API tests assert on a generic map[string]any.
// Nobody tested the JOIN.
//
// That gap fails silently and in the worst direction. A tag drift on
// `breach` makes every feed render "-" (no breach, all clear). A drift on
// `monotony` silently stops flagging a stuck loop. A drift on
// `noDelegation` turns every tick into "delegated". All three are the
// "examined and clean" vs "never examined" conflation this whole surface
// exists to prevent, arriving through a typo in a struct tag.
//
// So this test decodes a REAL handler response body into the CLI's own
// healthPayload and asserts the values survived the trip, then renders
// it. It imports internal/api, which production CLI code deliberately
// does not -- a test-only edge, the same one
// support_report_parity_integration_test.go already takes, and it puts
// no HTTP server in the vornikctl binary.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/persistence/repotest"
	"vornik.io/vornik/internal/registry"
)

// --- repository doubles -------------------------------------------------

type wireEvalRepo struct {
	counts map[string]int64
	rows   []*persistence.AutonomyEvaluation
}

func (w *wireEvalRepo) Record(context.Context, *persistence.AutonomyEvaluation) error { return nil }
func (w *wireEvalRepo) List(context.Context, persistence.AutonomyEvaluationFilter) ([]*persistence.AutonomyEvaluation, error) {
	return w.rows, nil
}
func (w *wireEvalRepo) LatestByProject(context.Context) (map[string]*persistence.AutonomyEvaluation, error) {
	return nil, nil
}
func (w *wireEvalRepo) CountByOutcome(context.Context, string, time.Time, time.Time) (map[string]int64, error) {
	return w.counts, nil
}

type wireVerdictRepo struct{ verdict *persistence.TaskJudgeVerdict }

func (w *wireVerdictRepo) Record(context.Context, *persistence.TaskJudgeVerdict) error { return nil }
func (w *wireVerdictRepo) GetByTask(_ context.Context, taskID string) (*persistence.TaskJudgeVerdict, error) {
	if w.verdict != nil && w.verdict.TaskID == taskID {
		return w.verdict, nil
	}
	return nil, persistence.ErrNotFound
}
func (w *wireVerdictRepo) ListRecent(context.Context, string, int) ([]*persistence.TaskJudgeVerdict, error) {
	return nil, nil
}
func (w *wireVerdictRepo) ListRecentSince(context.Context, string, time.Time, int) ([]*persistence.TaskJudgeVerdict, error) {
	return nil, nil
}

// TestWireVerdictRepo_MissContract pins wireVerdictRepo.GetByTask against
// the absence contract production obeys. A double that answers a miss
// differently from the real repository would certify the endpoint's
// "not judged yet" branch without ever executing production's -- and
// this test's whole subject is whether the two halves agree.
func TestWireVerdictRepo_MissContract(t *testing.T) {
	repotest.AssertMiss(t, "TaskJudgeVerdictRepository.GetByTask", func() (*persistence.TaskJudgeVerdict, error) {
		return (&wireVerdictRepo{}).GetByTask(context.Background(), "task-absent")
	})
}

type wireStepOutcomeRepo struct{ orphaned int64 }

func (w *wireStepOutcomeRepo) Record(context.Context, *persistence.ExecutionStepOutcome) error {
	return nil
}
func (w *wireStepOutcomeRepo) FinalizePending(context.Context, string, string, string, string, string, *string) (string, string, error) {
	return "", "", nil
}
func (w *wireStepOutcomeRepo) SweepPending(context.Context, string, string) ([]persistence.SweepResult, error) {
	return nil, nil
}
func (w *wireStepOutcomeRepo) List(context.Context, persistence.ExecutionStepOutcomeFilter) ([]*persistence.ExecutionStepOutcome, error) {
	return nil, nil
}
func (w *wireStepOutcomeRepo) SupersedeAfter(context.Context, string, time.Time) (int64, error) {
	return 0, nil
}
func (w *wireStepOutcomeRepo) StepLatencyP95ByStep(context.Context, time.Time) ([]persistence.StepLatencyStat, error) {
	return nil, nil
}
func (w *wireStepOutcomeRepo) TaintedStepsForTasks(context.Context, []string) ([]persistence.TaintedStepRow, error) {
	return nil, nil
}
func (w *wireStepOutcomeRepo) CountByRoleModelOutcome(context.Context, string, time.Time, time.Time, string) ([]persistence.RoleModelOutcomeCount, error) {
	return []persistence.RoleModelOutcomeCount{{Count: w.orphaned}}, nil
}

// --- fixture ------------------------------------------------------------

// wireContractRegistry stages a project declaring one feed, using the
// file-based load path because registry.Registry exposes no programmatic
// project-set API.
func wireContractRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"projects", "swarms", "workflows"} {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, sub), 0o755))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "swarms", "s1.md"), []byte(`---
swarmId: "s1"
roles:
  - name: "coder"
    runtime:
      image: "test:latest"
---
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workflows", "wf.md"), []byte(`---
workflowId: "wf"
entrypoint: "run"
steps:
  run:
    type: "agent"
    prompt: "do work"
    role: "coder"
    on_success: "done"
terminals:
  done:
    status: "COMPLETED"
---
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "projects", "proj.yaml"), []byte(`
projectId: "proj"
displayName: "Test Project"
swarmId: "s1"
defaultWorkflowId: "wf"
autonomy:
  enabled: true
  feeds:
    - slug: czech-news
      cadence: "4h"
`), 0o644))

	reg := registry.New()
	require.NoError(t, reg.Load(dir))
	return reg
}

func TestAutonomyHealth_APIResponseDecodesIntoCLIPayload(t *testing.T) {
	now := time.Now().UTC()
	lag := 19*time.Hour + 42*time.Minute // the incident's czech-news drift
	parentID := "task_20260910_parent01"
	childID := "task_20260910_child001"
	delegation := persistence.DelegationMode("JOIN")

	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(context.Context, persistence.TaskFilter) ([]*persistence.Task, error) {
			return []*persistence.Task{{
				ID: parentID, ProjectID: "proj", CreatedAt: now.Add(-lag),
				Payload: []byte(`{"context":{"prompt":"czech-news: refresh"}}`),
			}}, nil
		},
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, ProjectID: "proj", Status: persistence.TaskStatusCompleted}, nil
		},
		GetChildrenFunc: func(context.Context, string) ([]*persistence.Task, error) {
			return []*persistence.Task{{
				ID: childID, ProjectID: "proj",
				Status: persistence.TaskStatusFailed, DelegationMode: &delegation,
			}}, nil
		},
	}

	srv := api.NewServer(
		api.WithLogger(zerolog.Nop()),
		api.WithProjectRegistry(wireContractRegistry(t)),
		api.WithTaskRepository(taskRepo),
		api.WithAutonomyEvaluationRepository(&wireEvalRepo{
			// 12 ticks, all CREATED: over the 10-tick floor and at 100%,
			// so monotony must fire.
			counts: map[string]int64{persistence.AutonomyOutcomeCreated: 12},
			rows: []*persistence.AutonomyEvaluation{{
				ProjectID: "proj", Outcome: persistence.AutonomyOutcomeCreated,
				TaskID: &parentID, CreatedAt: now.Add(-time.Hour),
			}},
		}),
		api.WithTaskJudgeVerdictRepository(&wireVerdictRepo{
			// Keyed on the CHILD: the verdict belongs to whatever
			// actually delivered, not to the router.
			verdict: &persistence.TaskJudgeVerdict{TaskID: childID, Verdict: persistence.JudgeVerdictFail},
		}),
		api.WithExecutionRepository(&mocks.MockExecutionRepository{
			ListFunc: func(context.Context, persistence.ExecutionFilter) ([]*persistence.Execution, error) {
				return []*persistence.Execution{{ID: "e1"}, {ID: "e2"}, {ID: "e3"}, {ID: "e4"}}, nil
			},
		}),
		api.WithExecutionStepOutcomeRepository(&wireStepOutcomeRepo{orphaned: 7}),
	)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/proj/autonomy/health?hours=24", nil)
	rec := httptest.NewRecorder()
	srv.GetAutonomyHealth(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	// THE JOIN: the handler's own bytes, decoded by the CLI's own structs.
	var p healthPayload
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p),
		"the CLI could not decode the API's response: %s", rec.Body.String())

	assert.Equal(t, "proj", p.ProjectID)
	assert.Equal(t, 24, p.WindowHrs)
	assert.NotEmpty(t, p.Since)

	assert.Equal(t, int64(12), p.Outcomes.Total)
	assert.Equal(t, int64(12), p.Outcomes.Counts[persistence.AutonomyOutcomeCreated])
	assert.True(t, p.Outcomes.Monotony, "12/12 CREATED must arrive as monotony=true, not silently drop")

	require.True(t, p.FeedsDeclared)
	require.Len(t, p.Feeds, 1)
	feed := p.Feeds[0]
	assert.Equal(t, "czech-news", feed.Slug)
	assert.InDelta(t, (4 * time.Hour).Seconds(), feed.CadenceSeconds, 0.5)
	assert.InDelta(t, lag.Seconds(), feed.LagSeconds, 5)
	assert.False(t, feed.NeverRan)
	assert.False(t, feed.Unmeasured)
	assert.Equal(t, "slow", feed.Breach, "a breach that decodes as \"\" renders as \"-\": all clear")
	assert.Equal(t, 1, feed.HorizonTasks)
	assert.NotEmpty(t, feed.HorizonOldest)

	require.Len(t, p.Delivery.Rows, 1)
	row := p.Delivery.Rows[0]
	assert.Equal(t, parentID, row.TaskID)
	require.NotNil(t, row.ChildTaskID, "the delegation edge must survive the wire")
	assert.Equal(t, childID, *row.ChildTaskID)
	assert.Equal(t, string(persistence.TaskStatusFailed), row.Status,
		"delivery reports the CHILD's status -- the parent's router is not the delivery")
	assert.False(t, row.NoDelegation)
	assert.Equal(t, int64(0), p.Delivery.Unresolved)
	assert.False(t, p.Delivery.Truncated)

	require.True(t, p.Judge.Declared)
	assert.Equal(t, int64(1), p.Judge.Total)
	assert.InDelta(t, 1.0, p.Judge.FailRate, 0.001)
	assert.Equal(t, int64(1), p.Judge.Counts[persistence.JudgeVerdictFail])

	assert.EqualValues(t, 7, p.RouteChurn["orphanedStepOutcomes"])
	assert.EqualValues(t, 4, p.RouteChurn["maxExecutionsForTask"])

	// And the renderer over the SAME payload: the honesty strings must
	// reflect the real values, not the "nothing to see" defaults a tag
	// drift would produce.
	out := renderAutonomyHealth(p)
	for _, want := range []string{
		"czech-news", "slow", "monotony", "no delegation" /* absent below */, "orphaned step outcomes",
	} {
		if want == "no delegation" {
			assert.NotContains(t, out, want, "a delegating tick must not render as no delegation")
			continue
		}
		assert.Contains(t, out, want)
	}
	assert.NotContains(t, out, "not declared")
	assert.NotContains(t, out, "no verdicts")
	assert.NotContains(t, out, "not measured")
	assert.NotContains(t, out, "never ran")
	assert.False(t, strings.Contains(out, "orphaned step outcomes  not measured"))
}
