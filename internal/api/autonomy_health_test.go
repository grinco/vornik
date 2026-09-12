// Package api provides HTTP handlers for the vornik data plane API.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/registry"
)

// autonomyHealthTestProjectID is the project every test in this file
// stages and then addresses in its request path. A constant rather than a
// parameter: unparam flags a helper argument that only ever receives one
// value, and a second project id would have to be threaded through the
// URL of every case to mean anything.
const autonomyHealthTestProjectID = "proj"

// buildAutonomyHealthTestRegistry stages a minimal project, optionally with
// autonomy.feeds declared, exactly the same file-based pattern
// loadAPIQueryTestRegistry uses — registry.Registry exposes no programmatic
// project-set API.
func buildAutonomyHealthTestRegistry(t *testing.T, feedsYAML string) *registry.Registry {
	t.Helper()
	projectID := autonomyHealthTestProjectID
	configDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(configDir, "projects"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(configDir, "swarms"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(configDir, "workflows"), 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(configDir, "swarms", "s1.md"), []byte(`---
swarmId: "s1"
roles:
  - name: "coder"
    runtime:
      image: "test:latest"
---
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "workflows", "wf.md"), []byte(`---
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

	autonomyYAML := ""
	if feedsYAML != "" {
		autonomyYAML = "\nautonomy:\n  enabled: true\n  feeds:\n" + feedsYAML
	}
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "projects", projectID+".yaml"), []byte(`
projectId: "`+projectID+`"
displayName: "Test Project"
swarmId: "s1"
defaultWorkflowId: "wf"
`+autonomyYAML), 0o644))

	reg := registry.New()
	require.NoError(t, reg.Load(configDir))
	return reg
}

// mockJudgeVerdictRepository is a minimal test double for
// TaskJudgeVerdictRepository. Defaults (nil funcs) mirror "nothing recorded
// yet" — GetByTask returns ErrNotFound, matching the real repos' contract.
type mockJudgeVerdictRepository struct {
	getByTaskFunc func(ctx context.Context, taskID string) (*persistence.TaskJudgeVerdict, error)
}

func (m *mockJudgeVerdictRepository) Record(context.Context, *persistence.TaskJudgeVerdict) error {
	return nil
}

func (m *mockJudgeVerdictRepository) GetByTask(ctx context.Context, taskID string) (*persistence.TaskJudgeVerdict, error) {
	if m.getByTaskFunc != nil {
		return m.getByTaskFunc(ctx, taskID)
	}
	return nil, persistence.ErrNotFound
}

func (m *mockJudgeVerdictRepository) ListRecent(context.Context, string, int) ([]*persistence.TaskJudgeVerdict, error) {
	return nil, nil
}

func (m *mockJudgeVerdictRepository) ListRecentSince(context.Context, string, time.Time, int) ([]*persistence.TaskJudgeVerdict, error) {
	return nil, nil
}

// decodeHealthResp decodes the handler's JSON body into a generic map so
// tests can assert on presence and value of specific honesty-rule fields
// without binding to a Go struct the handler doesn't itself expose.
func decodeHealthResp(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var resp map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	return resp
}

// TestGetAutonomyHealth_UndeclaredFeedsNotOK is the honesty-rule regression
// this endpoint exists for: a project that never declares autonomy.feeds
// must render feedsDeclared:false and feeds:null, never a blank/zero shape
// that a caller could mistake for "declared and healthy". This is the
// "examined and clean" vs "never examined" distinction (project rule 4) --
// reporting the first while meaning the second is the failure this whole
// surface exists to prevent.
func TestGetAutonomyHealth_UndeclaredFeedsNotOK(t *testing.T) {
	reg := buildAutonomyHealthTestRegistry(t, "")
	srv := &Server{
		projectRegistry:  reg,
		autonomyEvalRepo: &mockAutonomyEvaluationRepository{},
		taskRepo:         &mocks.MockTaskRepository{},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/proj/autonomy/health", nil)
	rec := httptest.NewRecorder()
	srv.GetAutonomyHealth(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	resp := decodeHealthResp(t, rec)

	declared, ok := resp["feedsDeclared"]
	require.True(t, ok, "response must carry feedsDeclared")
	assert.Equal(t, false, declared)

	feeds, ok := resp["feeds"]
	require.True(t, ok, "response must carry a feeds key even when nil")
	assert.Nil(t, feeds, "undeclared feeds must render as null, never an empty-but-present list")
}

// TestGetAutonomyHealth_NoVerdictsIsNotZeroPercent: a window with zero
// resolved judge verdicts must report judge.declared:false, never a
// {"failRate": 0} that reads as "every task passed".
func TestGetAutonomyHealth_NoVerdictsIsNotZeroPercent(t *testing.T) {
	createdOutcome := persistence.AutonomyOutcomeCreated
	taskID := "task-1"
	srv := &Server{
		autonomyEvalRepo: &mockAutonomyEvaluationRepository{
			listFunc: func(_ context.Context, filter persistence.AutonomyEvaluationFilter) ([]*persistence.AutonomyEvaluation, error) {
				require.NotNil(t, filter.Outcome)
				assert.Equal(t, createdOutcome, *filter.Outcome)
				return []*persistence.AutonomyEvaluation{
					{ID: "e1", ProjectID: "proj", Outcome: createdOutcome, TaskID: &taskID, CreatedAt: time.Now()},
				}, nil
			},
		},
		taskRepo: &mocks.MockTaskRepository{
			GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
				return &persistence.Task{ID: id, Status: persistence.TaskStatusCompleted}, nil
			},
		},
		verdictRepo: &mockJudgeVerdictRepository{}, // no verdicts recorded for anything
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/proj/autonomy/health", nil)
	rec := httptest.NewRecorder()
	srv.GetAutonomyHealth(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	resp := decodeHealthResp(t, rec)

	judge, ok := resp["judge"].(map[string]any)
	require.True(t, ok, "response must carry a judge object")
	assert.Equal(t, false, judge["declared"])
	_, hasFailRate := judge["failRate"]
	assert.False(t, hasFailRate, "an empty verdict window must not carry a failRate at all, let alone a 0% one")
}

// TestGetAutonomyHealth_DeliveryFollowsDelegationEdge: a CREATED tick whose
// task delegated must report the CHILD's terminal status -- the router's
// own status (e.g. COMPLETED once it finishes routing) is not the delivery.
func TestGetAutonomyHealth_DeliveryFollowsDelegationEdge(t *testing.T) {
	createdOutcome := persistence.AutonomyOutcomeCreated
	parentID := "parent-1"
	childID := "child-1"
	seqMode := persistence.DelegationModeSequential

	srv := &Server{
		autonomyEvalRepo: &mockAutonomyEvaluationRepository{
			listFunc: func(_ context.Context, _ persistence.AutonomyEvaluationFilter) ([]*persistence.AutonomyEvaluation, error) {
				return []*persistence.AutonomyEvaluation{
					{ID: "e1", ProjectID: "proj", Outcome: createdOutcome, TaskID: &parentID, CreatedAt: time.Now()},
				}, nil
			},
		},
		taskRepo: &mocks.MockTaskRepository{
			GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
				require.Equal(t, parentID, id)
				// The router's own status: COMPLETED. If delivery ever
				// reports this instead of the child's, the assertion
				// below on "status" catches it.
				return &persistence.Task{ID: parentID, Status: persistence.TaskStatusCompleted}, nil
			},
			GetChildrenFunc: func(_ context.Context, parentTaskID string) ([]*persistence.Task, error) {
				require.Equal(t, parentID, parentTaskID)
				return []*persistence.Task{
					{ID: childID, Status: persistence.TaskStatusFailed, DelegationMode: &seqMode},
				}, nil
			},
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/proj/autonomy/health", nil)
	rec := httptest.NewRecorder()
	srv.GetAutonomyHealth(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	resp := decodeHealthResp(t, rec)

	delivery, ok := resp["delivery"].(map[string]any)
	require.True(t, ok)
	rows, ok := delivery["rows"].([]any)
	require.True(t, ok)
	require.Len(t, rows, 1)
	row := rows[0].(map[string]any)

	assert.Equal(t, string(persistence.TaskStatusFailed), row["status"],
		"delivery must follow the delegation edge to the child's terminal status, not the router's")
	assert.Equal(t, childID, row["childTaskId"])
	assert.Equal(t, false, row["noDelegation"])
}

// TestGetAutonomyHealth_ParentWithoutChildMarkedNoDelegation: a CREATED tick
// whose task concluded without ever creating a child must be marked
// noDelegation with the PARENT's own terminal status -- it must not hide
// inside the same CREATED->COMPLETED shape as a healthy, delegating tick.
func TestGetAutonomyHealth_ParentWithoutChildMarkedNoDelegation(t *testing.T) {
	createdOutcome := persistence.AutonomyOutcomeCreated
	parentID := "parent-2"

	srv := &Server{
		autonomyEvalRepo: &mockAutonomyEvaluationRepository{
			listFunc: func(_ context.Context, _ persistence.AutonomyEvaluationFilter) ([]*persistence.AutonomyEvaluation, error) {
				return []*persistence.AutonomyEvaluation{
					{ID: "e2", ProjectID: "proj", Outcome: createdOutcome, TaskID: &parentID, CreatedAt: time.Now()},
				}, nil
			},
		},
		taskRepo: &mocks.MockTaskRepository{
			GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
				return &persistence.Task{ID: parentID, Status: persistence.TaskStatusFailed}, nil
			},
			GetChildrenFunc: func(_ context.Context, _ string) ([]*persistence.Task, error) {
				return nil, nil // no children -- hit a routing guard or malformed plan
			},
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/proj/autonomy/health", nil)
	rec := httptest.NewRecorder()
	srv.GetAutonomyHealth(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	resp := decodeHealthResp(t, rec)

	delivery, ok := resp["delivery"].(map[string]any)
	require.True(t, ok)
	rows, ok := delivery["rows"].([]any)
	require.True(t, ok)
	require.Len(t, rows, 1)
	row := rows[0].(map[string]any)

	assert.Equal(t, string(persistence.TaskStatusFailed), row["status"])
	assert.Equal(t, true, row["noDelegation"])
	_, hasChild := row["childTaskId"]
	assert.False(t, hasChild, "a non-delegating parent must not carry a childTaskId")
}

// TestGetAutonomyHealth_ChildrenLookupFailureCountsUnresolved is the
// fix-round-1 regression for finding 1 (review-20260910): a GetChildren
// failure used to `continue` silently, so the tick vanished from
// delivery.rows with nothing distinguishing it from "there simply weren't
// that many CREATED ticks in the window". It must instead show up as a
// non-zero delivery.unresolved -- reproducing that exact "examined and
// clean" vs "never examined" conflation inside the detector itself would be
// the worst possible outcome for this endpoint.
func TestGetAutonomyHealth_ChildrenLookupFailureCountsUnresolved(t *testing.T) {
	createdOutcome := persistence.AutonomyOutcomeCreated
	taskID := "task-1"

	srv := &Server{
		autonomyEvalRepo: &mockAutonomyEvaluationRepository{
			listFunc: func(_ context.Context, _ persistence.AutonomyEvaluationFilter) ([]*persistence.AutonomyEvaluation, error) {
				return []*persistence.AutonomyEvaluation{
					{ID: "e1", ProjectID: "proj", Outcome: createdOutcome, TaskID: &taskID, CreatedAt: time.Now()},
				}, nil
			},
		},
		taskRepo: &mocks.MockTaskRepository{
			GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
				return &persistence.Task{ID: id, Status: persistence.TaskStatusCompleted}, nil
			},
			GetChildrenFunc: func(_ context.Context, _ string) ([]*persistence.Task, error) {
				return nil, errors.New("db timeout")
			},
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/proj/autonomy/health", nil)
	rec := httptest.NewRecorder()
	srv.GetAutonomyHealth(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	resp := decodeHealthResp(t, rec)

	delivery, ok := resp["delivery"].(map[string]any)
	require.True(t, ok)
	rows, ok := delivery["rows"].([]any)
	require.True(t, ok)
	assert.Empty(t, rows, "a tick whose children lookup failed cannot be resolved to a row")

	unresolved, ok := delivery["unresolved"]
	require.True(t, ok, "response must carry delivery.unresolved")
	assert.EqualValues(t, 1, unresolved,
		"a lookup failure must produce a non-zero unresolved count, not just a silently shorter row list")
}

// TestGetAutonomyHealth_VerdictLookupFailureCountsUnresolvedWithoutDroppingRow
// covers the second swallowed-error site from finding 1: a verdict-repo
// error (as opposed to persistence.ErrNotFound, the legitimate "not judged
// yet" case) must count toward delivery.unresolved even though the
// delivery row itself is still resolvable and present.
func TestGetAutonomyHealth_VerdictLookupFailureCountsUnresolvedWithoutDroppingRow(t *testing.T) {
	createdOutcome := persistence.AutonomyOutcomeCreated
	taskID := "task-2"

	srv := &Server{
		autonomyEvalRepo: &mockAutonomyEvaluationRepository{
			listFunc: func(_ context.Context, _ persistence.AutonomyEvaluationFilter) ([]*persistence.AutonomyEvaluation, error) {
				return []*persistence.AutonomyEvaluation{
					{ID: "e2", ProjectID: "proj", Outcome: createdOutcome, TaskID: &taskID, CreatedAt: time.Now()},
				}, nil
			},
		},
		taskRepo: &mocks.MockTaskRepository{
			GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
				return &persistence.Task{ID: id, Status: persistence.TaskStatusCompleted}, nil
			},
			GetChildrenFunc: func(_ context.Context, _ string) ([]*persistence.Task, error) {
				return nil, nil // no children -- a legitimate non-delegating tick
			},
		},
		verdictRepo: &mockJudgeVerdictRepository{
			getByTaskFunc: func(_ context.Context, _ string) (*persistence.TaskJudgeVerdict, error) {
				return nil, errors.New("verdict backend unavailable") // NOT persistence.ErrNotFound
			},
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/proj/autonomy/health", nil)
	rec := httptest.NewRecorder()
	srv.GetAutonomyHealth(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	resp := decodeHealthResp(t, rec)

	delivery, ok := resp["delivery"].(map[string]any)
	require.True(t, ok)
	rows, ok := delivery["rows"].([]any)
	require.True(t, ok)
	assert.Len(t, rows, 1, "the tick itself resolved fine -- only the verdict lookup failed")

	unresolved, ok := delivery["unresolved"]
	require.True(t, ok)
	assert.EqualValues(t, 1, unresolved,
		"a verdict-lookup error (not ErrNotFound) must count as unresolved, not fold silently into judge.declared:false")

	judge, ok := resp["judge"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, false, judge["declared"])
}

// TestBuildAutonomyOutcomesBlock_BelowMinTicksNoMonotony pins the
// fix-round-1 regression for finding 2 (review-20260910): a single
// CREATED tick (or any window under the 10-tick floor) must never read as
// "100% monotone" -- a small window is not evidence.
func TestBuildAutonomyOutcomesBlock_BelowMinTicksNoMonotony(t *testing.T) {
	block := buildAutonomyOutcomesBlock(map[string]int64{"CREATED": 9})
	_, hasMonotony := block["monotony"]
	assert.False(t, hasMonotony, "9 ticks, even all one outcome, is below the min-ticks floor")
}

// TestBuildAutonomyOutcomesBlock_AtMinTicksAllOneOutcomeTriggersMonotony
// pins the other side of the same boundary: exactly 10 ticks, the
// configured floor, all one outcome, must trigger. This is the shape of
// the six-day 100%-CREATED incident the endpoint exists to catch.
func TestBuildAutonomyOutcomesBlock_AtMinTicksAllOneOutcomeTriggersMonotony(t *testing.T) {
	block := buildAutonomyOutcomesBlock(map[string]int64{"CREATED": 10})
	assert.Equal(t, true, block["monotony"])
}

// TestBuildAutonomyOutcomesBlock_MixedBelowThresholdNoMonotony confirms the
// threshold gates independently of the min-ticks floor: 10 ticks (at the
// floor) with a 90% share must not trigger -- 90% is a real mix, not a
// stuck outcome.
func TestBuildAutonomyOutcomesBlock_MixedBelowThresholdNoMonotony(t *testing.T) {
	block := buildAutonomyOutcomesBlock(map[string]int64{"CREATED": 9, "NO_ACTION": 1})
	_, hasMonotony := block["monotony"]
	assert.False(t, hasMonotony, "90% of 10 is a real mix, not the >=95% monotony shape")
}

// TestBuildAutonomyOutcomesBlock_ExactThresholdTriggersMonotony pins the
// >= 95% boundary exactly: 19 of 20 (95.0% on the nose) must trigger.
// 0.95*20 == 19.0 exactly in float64, so this specific ratio is what
// distinguishes ">=" from ">" on the threshold comparison -- a ">"
// mutation makes this case (and only this kind of case) fail while the
// 100%-share tests above stay green, which is why both boundaries need
// their own test.
func TestBuildAutonomyOutcomesBlock_ExactThresholdTriggersMonotony(t *testing.T) {
	block := buildAutonomyOutcomesBlock(map[string]int64{"CREATED": 19, "NO_ACTION": 1})
	assert.Equal(t, true, block["monotony"])
}

// --------------------------------------------------------------------
// The DECLARED-feeds path. Until 2026-09-11 the only caller of
// buildAutonomyHealthTestRegistry passed "", so its feedsYAML branch was
// dead code and the entire declared path -- the task-list query,
// renderFeedObservations, autonomyFeedJSON's wire shape, and
// resolveAutonomyFeeds' error path -- had no handler-level coverage at
// all. The endpoint was tested only on the branch where it produces no
// number.
// --------------------------------------------------------------------

// healthFeedRows pulls the feeds array out of the generic response map
// as a list of per-row maps, failing the test if feeds is absent or not
// a list (both of which the undeclared path legitimately produces -- so
// a declared-path test that tolerated them would assert nothing).
func healthFeedRows(t *testing.T, resp map[string]any) []map[string]any {
	t.Helper()
	raw, ok := resp["feeds"]
	require.True(t, ok, "response must carry feeds")
	list, ok := raw.([]any)
	require.True(t, ok, "declared feeds must render as a JSON list, got %T", raw)
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		row, ok := item.(map[string]any)
		require.True(t, ok, "feed row must be an object, got %T", item)
		out = append(out, row)
	}
	return out
}

// TestGetAutonomyHealth_DeclaredFeedsCarryValuesNotJustKeys asserts the
// VALUES on the wire, not merely that the keys exist: a slug that ran
// 19.7h ago against its declared 4h cadence must come back as a measured
// `slow` breach with the real numbers (the incident's own czech-news
// row), and a second declared slug with no task anywhere in an
// unsaturated page must come back as a genuine never-ran with no breach.
func TestGetAutonomyHealth_DeclaredFeedsCarryValuesNotJustKeys(t *testing.T) {
	reg := buildAutonomyHealthTestRegistry(t, `    - slug: czech-news
      cadence: "4h"
    - slug: cultural-events
      cadence: "1440h"
`)
	lag := 19*time.Hour + 42*time.Minute // 19.7h: 4.9x its 4h cadence (design §1.1)
	ranAt := time.Now().UTC().Add(-lag)
	srv := &Server{
		projectRegistry:  reg,
		autonomyEvalRepo: &mockAutonomyEvaluationRepository{},
		taskRepo: &mocks.MockTaskRepository{
			ListFunc: func(context.Context, persistence.TaskFilter) ([]*persistence.Task, error) {
				return []*persistence.Task{{
					ID:        "t1",
					ProjectID: "proj",
					CreatedAt: ranAt,
					Payload:   []byte(`{"context":{"prompt":"czech-news: refresh"}}`),
				}}, nil
			},
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/proj/autonomy/health", nil)
	rec := httptest.NewRecorder()
	srv.GetAutonomyHealth(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	resp := decodeHealthResp(t, rec)
	assert.Equal(t, true, resp["feedsDeclared"])

	rows := healthFeedRows(t, resp)
	require.Len(t, rows, 2, "both declared feeds must produce a row")

	byS := map[string]map[string]any{}
	for _, r := range rows {
		slug, _ := r["slug"].(string)
		byS[slug] = r
	}

	news := byS["czech-news"]
	require.NotNil(t, news, "czech-news row missing; got %+v", rows)
	assert.Equal(t, float64((4 * time.Hour).Seconds()), news["cadenceSeconds"])
	assert.InDelta(t, lag.Seconds(), news["lagSeconds"], 5)
	assert.Equal(t, false, news["neverRan"])
	assert.Equal(t, false, news["unmeasured"])
	assert.Equal(t, "slow", news["breach"], "19.7h against a 4h cadence is the incident's slow class")
	assert.Equal(t, float64(1), news["horizonTasks"], "the scope of the claim travels with it")
	assert.NotEmpty(t, news["horizonOldest"])

	// The page returned 1 task against a 50 bound, so it is NOT
	// saturated: the whole history was examined and this really is a
	// never-ran, not an unmeasured feed.
	culture := byS["cultural-events"]
	require.NotNil(t, culture, "cultural-events row missing; got %+v", rows)
	assert.Equal(t, true, culture["neverRan"])
	assert.Equal(t, false, culture["unmeasured"])
	assert.Equal(t, float64(0), culture["lagSeconds"])
	assert.Equal(t, "", culture["breach"], "absent evidence is not evidence of a breach")
	assert.NotContains(t, culture, "lagAtLeastSeconds", "no bound to report when the whole history was examined")
}

// A feed absent from a SATURATED page is "not measured", not "never
// ran" -- and when the examined window is already longer than the
// cadence, overdue is PROVEN without a measurement. Before the horizon
// travelled on the wire, this feed rendered byte-identically to one that
// had genuinely never run, and `slow` was unreachable for it.
func TestGetAutonomyHealth_SaturatedPageReportsUnmeasuredNotNeverRan(t *testing.T) {
	reg := buildAutonomyHealthTestRegistry(t, `    - slug: czech-news
      cadence: "4h"
`)
	now := time.Now().UTC()
	span := 233 * time.Hour // the shipped 50-task window at ~5.16 tasks/day
	filler := make([]*persistence.Task, 0, autonomyHealthFeedTaskPageSize)
	for i := 0; i < autonomyHealthFeedTaskPageSize; i++ {
		age := time.Duration(float64(span) * float64(i) / float64(autonomyHealthFeedTaskPageSize-1))
		filler = append(filler, &persistence.Task{
			ID: "f", ProjectID: "proj", CreatedAt: now.Add(-age),
			Payload: []byte(`{"context":{"prompt":"other-feed: refresh"}}`),
		})
	}
	srv := &Server{
		projectRegistry:  reg,
		autonomyEvalRepo: &mockAutonomyEvaluationRepository{},
		taskRepo: &mocks.MockTaskRepository{
			ListFunc: func(context.Context, persistence.TaskFilter) ([]*persistence.Task, error) {
				return filler, nil
			},
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/proj/autonomy/health", nil)
	rec := httptest.NewRecorder()
	srv.GetAutonomyHealth(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	row := healthFeedRows(t, decodeHealthResp(t, rec))[0]
	assert.Equal(t, true, row["neverRan"])
	assert.Equal(t, true, row["unmeasured"], "a full page means older history exists and was not examined")
	assert.Equal(t, "slow", row["breach"], "no run in 233h of examined history against a 4h cadence is provably overdue")
	assert.InDelta(t, span.Seconds(), row["lagAtLeastSeconds"], 5)
	assert.Equal(t, float64(0), row["lagSeconds"], "a bound must never be published as a measurement")
	assert.Equal(t, float64(autonomyHealthFeedTaskPageSize), row["horizonTasks"])
}

// resolveAutonomyFeeds' error path, previously unreachable in any test:
// a task-list failure must 500 rather than render an empty-but-declared
// feeds list, which would read as "declared and nothing wrong".
func TestGetAutonomyHealth_DeclaredFeedsTaskListFailureIs500(t *testing.T) {
	reg := buildAutonomyHealthTestRegistry(t, `    - slug: czech-news
      cadence: "4h"
`)
	srv := &Server{
		projectRegistry:  reg,
		autonomyEvalRepo: &mockAutonomyEvaluationRepository{},
		taskRepo: &mocks.MockTaskRepository{
			ListFunc: func(context.Context, persistence.TaskFilter) ([]*persistence.Task, error) {
				return nil, errors.New("task list exploded")
			},
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/proj/autonomy/health", nil)
	rec := httptest.NewRecorder()
	srv.GetAutonomyHealth(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// Finding 8: a Server with an eval repo and no task repo used to panic on
// the bare s.taskRepo dereference. Every other optional repo in this
// handler family is nil-guarded; this pins that taskRepo now is too, on
// both the feeds path (which cannot answer at all, so it errors) and the
// delivery path (where each unexaminable tick becomes `unresolved`).
func TestGetAutonomyHealth_NilTaskRepoDoesNotPanic(t *testing.T) {
	reg := buildAutonomyHealthTestRegistry(t, `    - slug: czech-news
      cadence: "4h"
`)
	srv := &Server{projectRegistry: reg, autonomyEvalRepo: &mockAutonomyEvaluationRepository{}}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/proj/autonomy/health", nil)
	rec := httptest.NewRecorder()
	require.NotPanics(t, func() { srv.GetAutonomyHealth(rec, req) })
	assert.Equal(t, http.StatusInternalServerError, rec.Code,
		"no task repo means no cadence answer at all -- an error, never an empty declared list")
}

func TestGetAutonomyHealth_NilTaskRepoCountsDeliveryTicksUnresolved(t *testing.T) {
	reg := buildAutonomyHealthTestRegistry(t, "") // no feeds: the feeds block returns before taskRepo
	taskID := "t1"
	srv := &Server{
		projectRegistry: reg,
		autonomyEvalRepo: &mockAutonomyEvaluationRepository{
			listFunc: func(context.Context, persistence.AutonomyEvaluationFilter) ([]*persistence.AutonomyEvaluation, error) {
				return []*persistence.AutonomyEvaluation{
					{TaskID: &taskID, CreatedAt: time.Now().UTC()},
				}, nil
			},
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/proj/autonomy/health", nil)
	rec := httptest.NewRecorder()
	require.NotPanics(t, func() { srv.GetAutonomyHealth(rec, req) })
	require.Equal(t, http.StatusOK, rec.Code)

	delivery, ok := decodeHealthResp(t, rec)["delivery"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, float64(1), delivery["unresolved"],
		"an unexaminable tick must be counted, never dropped into a table that reads as a quiet window")
}

// failingStepOutcomeRepo is stubStepOutcomeRepo (healing_recipe_generate_
// test.go) with only the one aggregate this endpoint calls made to fail,
// rather than a second full double -- the prior art is reused on purpose
// (project rule 5).
type failingStepOutcomeRepo struct{ stubStepOutcomeRepo }

func (f *failingStepOutcomeRepo) CountByRoleModelOutcome(
	context.Context, string, time.Time, time.Time, string,
) ([]persistence.RoleModelOutcomeCount, error) {
	return nil, errors.New("aggregate exploded")
}

// Finding 6: CountByRoleModelOutcome's error used to be swallowed by an
// `if ...; err == nil` and the block rendered orphanedStepOutcomes: 0 --
// "examined and clean" meaning "never examined", inside the detector
// built to catch exactly that. The key must be ABSENT so the CLI's
// presence check renders "not measured".
func TestGetAutonomyHealth_OrphanedCountFailureIsNotAZero(t *testing.T) {
	reg := buildAutonomyHealthTestRegistry(t, "")
	srv := &Server{
		projectRegistry:  reg,
		autonomyEvalRepo: &mockAutonomyEvaluationRepository{},
		taskRepo:         &mocks.MockTaskRepository{},
		stepOutcomeRepo:  &failingStepOutcomeRepo{},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/proj/autonomy/health", nil)
	rec := httptest.NewRecorder()
	srv.GetAutonomyHealth(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	churn, ok := decodeHealthResp(t, rec)["routeChurn"].(map[string]any)
	require.True(t, ok)
	assert.NotContains(t, churn, "orphanedStepOutcomes",
		"a failed aggregate must render as not-measured (absent key), never as a clean zero")
	assert.Contains(t, churn, "tasksObserved", "the rest of the block still reports")
}

// The same rule for a Server with no step-outcome repo at all: nothing
// was examined, so nothing may be reported as zero.
func TestGetAutonomyHealth_NoStepOutcomeRepoIsNotAZero(t *testing.T) {
	reg := buildAutonomyHealthTestRegistry(t, "")
	srv := &Server{
		projectRegistry:  reg,
		autonomyEvalRepo: &mockAutonomyEvaluationRepository{},
		taskRepo:         &mocks.MockTaskRepository{},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/proj/autonomy/health", nil)
	rec := httptest.NewRecorder()
	srv.GetAutonomyHealth(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	churn, _ := decodeHealthResp(t, rec)["routeChurn"].(map[string]any)
	assert.NotContains(t, churn, "orphanedStepOutcomes")
}
