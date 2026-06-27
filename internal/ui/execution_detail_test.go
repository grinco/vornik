package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// shuffleOutcomeRepo emits rows in a forced "newest-first then
// scrambled within ties" order — the worst case the postgres-side
// ORDER BY recorded_at DESC can produce when several outcomes share a
// recorded_at down to the microsecond. The UI handler is responsible
// for restoring execution order from this input, so the test rig
// exists to prove that gating is in place.
type shuffleOutcomeRepo struct {
	rows []*persistence.ExecutionStepOutcome
}

func (r *shuffleOutcomeRepo) Record(_ context.Context, _ *persistence.ExecutionStepOutcome) error {
	return nil
}
func (r *shuffleOutcomeRepo) Finalize(_ context.Context, _, _, _, _ string, _ *string) error {
	return nil
}
func (r *shuffleOutcomeRepo) FinalizePending(_ context.Context, _, _, _, _, _ string, _ *string) (string, string, error) {
	return "", "", nil
}
func (r *shuffleOutcomeRepo) SweepPending(_ context.Context, _, _ string) ([]persistence.SweepResult, error) {
	return nil, nil
}
func (r *shuffleOutcomeRepo) SupersedeAfter(_ context.Context, _ string, _ time.Time) (int64, error) {
	return 0, nil
}
func (r *shuffleOutcomeRepo) CountByRoleModelOutcome(_ context.Context, _ string, _ time.Time, _ time.Time, _ string) ([]persistence.RoleModelOutcomeCount, error) {
	return nil, nil
}
func (r *shuffleOutcomeRepo) List(_ context.Context, _ persistence.ExecutionStepOutcomeFilter) ([]*persistence.ExecutionStepOutcome, error) {
	// Return a copy in (recorded_at DESC, id DESC) order — matches the
	// repo's SQL contract. The handler then has to flip to (asc, asc)
	// to render execution order; the test asserts on the result of
	// that flip.
	out := make([]*persistence.ExecutionStepOutcome, len(r.rows))
	copy(out, r.rows)
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].RecordedAt.Equal(out[j].RecordedAt) {
			return out[i].RecordedAt.After(out[j].RecordedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

// TestParseRoutingDecision_HappyPath — strict-adaptive route step's
// result.json carries selected_workflow + reason. Parser must
// extract both so the dashboard panel can render them.
func TestParseRoutingDecision_HappyPath(t *testing.T) {
	body := []byte(`{"selected_workflow":"dev-pipeline","reason":"Task is a code change.","status":"COMPLETED"}`)
	got := parseRoutingDecision(body)
	require.NotNil(t, got)
	assert.Equal(t, "dev-pipeline", got.SelectedWorkflow)
	assert.Equal(t, "Task is a code change.", got.Reason)
	assert.Empty(t, got.ChildTaskID, "child task id is populated by the handler, not the parser")
}

// TestParseRoutingDecision_NoSelectionReturnsNil — non-routing
// executions have a result.json without selected_workflow. Parser
// must return nil so the handler skips rendering the panel rather
// than showing an empty "Lead chose: " row.
func TestParseRoutingDecision_NoSelectionReturnsNil(t *testing.T) {
	cases := [][]byte{
		nil,
		[]byte(""),
		[]byte("not-json"),
		[]byte(`{"status":"COMPLETED","message":"done"}`),                     // no selected_workflow
		[]byte(`{"selected_workflow":"","reason":"empty selection skipped"}`), // empty selection
	}
	for i, body := range cases {
		got := parseRoutingDecision(body)
		assert.Nil(t, got, "case %d (%q) must yield nil so the routing panel doesn't render", i, body)
	}
}

// TestOutcomeCSSClass — the UI's outcome → pill-class mapping is the
// only way operators can distinguish good from bad rows on the
// execution detail page at a glance. budget_tripwire is a recently
// added outcome that must land in the "bad" bucket (clean exit, but
// the step didn't produce usable output — the agent stopped early to
// stay within budget). A miss here would render tripwire rows as
// neutral grey alongside ok rows and the dashboard would lie.
func TestOutcomeCSSClass(t *testing.T) {
	cases := []struct {
		outcome string
		want    string
	}{
		{"ok", "outcome-ok"},
		{"pending_validation", "outcome-pending"},
		{"cancelled", "outcome-neutral"},
		{"superseded", "outcome-neutral"},
		{"parse_error", "outcome-bad"},
		{"schema_violation", "outcome-bad"},
		{"refused", "outcome-bad"},
		{"degenerate_loop", "outcome-bad"},
		{"downstream_rejected", "outcome-bad"},
		{"gate_failed", "outcome-bad"},
		{"failed", "outcome-bad"},
		{"timeout", "outcome-bad"},
		{"budget_tripwire", "outcome-bad"},
		{"", "outcome-neutral"},
		{"unknown_future_outcome", "outcome-neutral"},
	}
	for _, tc := range cases {
		t.Run(tc.outcome, func(t *testing.T) {
			got := outcomeCSSClass(tc.outcome)
			if got != tc.want {
				t.Errorf("outcomeCSSClass(%q) = %q, want %q", tc.outcome, got, tc.want)
			}
		})
	}
}

// TestExecutionDetail_StepOutcomesRenderInExecutionOrder — the bug:
// operators reported the Step Outcomes table appearing in the wrong
// order. Repro is two outcomes sharing a recorded_at (gate finalize
// + next agent step's pending row landing in the same Postgres
// microsecond, common when the workflow loop is tight and Postgres
// truncates Go's nanosecond timestamps). Old code did sort.Slice
// (unstable) on RecordedAt alone, so ties were randomized per page
// load. Fix asserts a deterministic execution order: primary key
// recorded_at, tiebreak by ID — and the rendered HTML reflects that.
func TestExecutionDetail_StepOutcomesRenderInExecutionOrder(t *testing.T) {
	t0 := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	tied := time.Date(2026, 4, 27, 12, 0, 5, 123000, time.UTC)
	durMs := int64(100)

	// Insert order intent (i.e. execution order):
	//   plan (t0) → review-gate (tied, id=outc_b) → implement (tied, id=outc_c) → done (t0+10s)
	// With the shuffleRepo emitting (DESC, DESC) the handler receives:
	//   done, implement, review-gate, plan
	// After the handler's stable ascending sort with id tiebreak it
	// must show: plan, review-gate, implement, done.
	rows := []*persistence.ExecutionStepOutcome{
		{
			ID: "outc_a", ProjectID: "p1", TaskID: "t1", ExecutionID: "e1",
			StepID: "step-alpha", Role: "lead", Outcome: "ok",
			RecordedAt: t0, DurationMS: &durMs,
		},
		{
			ID: "outc_b", ProjectID: "p1", TaskID: "t1", ExecutionID: "e1",
			StepID: "step-bravo", Role: "gate", Outcome: "ok",
			RecordedAt: tied, DurationMS: &durMs,
		},
		{
			ID: "outc_c", ProjectID: "p1", TaskID: "t1", ExecutionID: "e1",
			StepID: "step-charlie", Role: "coder", Outcome: "ok",
			RecordedAt: tied, DurationMS: &durMs,
		},
		{
			ID: "outc_d", ProjectID: "p1", TaskID: "t1", ExecutionID: "e1",
			StepID: "step-delta", Role: "coder", Outcome: "ok",
			RecordedAt: t0.Add(10 * time.Second), DurationMS: &durMs,
		},
	}

	task := &persistence.Task{
		ID: "t1", ProjectID: "p1", Status: persistence.TaskStatusCompleted,
		CreatedAt: t0,
	}
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			if id == task.ID {
				return task, nil
			}
			return nil, persistence.ErrNotFound
		},
	}
	exec := &persistence.Execution{
		ID: "e1", TaskID: "t1", ProjectID: "p1",
		Status: persistence.ExecutionStatusCompleted,
		// CompletedSteps in execution order. The Step Progress section
		// iterates this directly — the bug under test is the Outcomes
		// list (which feeds the Step Outcomes table and the Step
		// Timeline), so we don't need a workflow loaded.
		CompletedSteps: []string{"step-alpha", "step-bravo", "step-charlie", "step-delta"},
	}
	execRepo := &mocks.MockExecutionRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Execution, error) {
			if id == exec.ID {
				return exec, nil
			}
			return nil, persistence.ErrNotFound
		},
	}

	s := NewServer(
		WithTaskRepository(taskRepo),
		WithExecutionRepository(execRepo),
		WithStepOutcomeRepository(&shuffleOutcomeRepo{rows: rows}),
	)

	req := httptest.NewRequest(http.MethodGet, "/executions/e1", nil)
	rec := httptest.NewRecorder()
	s.ExecutionDetail(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	body := rec.Body.String()
	// All four step IDs must appear, in execution order. We assert by
	// finding the first index of each step ID in the rendered HTML.
	wantOrder := []string{"step-alpha", "step-bravo", "step-charlie", "step-delta"}
	gotPositions := make([]int, len(wantOrder))
	for i, step := range wantOrder {
		gotPositions[i] = strings.Index(body, step)
		if gotPositions[i] == -1 {
			t.Fatalf("step %q not found in rendered HTML", step)
		}
	}
	for i := 1; i < len(gotPositions); i++ {
		assert.Greater(t, gotPositions[i], gotPositions[i-1],
			"step %q must appear after step %q in execution order — got positions %v",
			wantOrder[i], wantOrder[i-1], gotPositions)
	}
}
