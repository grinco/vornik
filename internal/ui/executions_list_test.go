package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// buildExecutionsData powers the cross-task Executions list (IA completion).
// Pure builder: formats rows (current step deref, started-ago, duration) so
// the template stays declarative. countExecutionStatuses derives the
// filter-palette counts from a page slice (the restricted-auth fallback,
// mirroring tasks.go).

func ptr[T any](v T) *T { return &v }

func TestBuildExecutionsData_FormatsRows(t *testing.T) {
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	step := "research"
	execs := []*persistence.Execution{
		{
			ID: "exec-running", TaskID: "task-1", ProjectID: "janka", WorkflowID: "research",
			Status: persistence.ExecutionStatusRunning, CurrentStepID: &step,
			StartedAt: ptr(now.Add(-90 * time.Second)), // running 90s
		},
		{
			ID: "exec-done", TaskID: "task-2", ProjectID: "janka", WorkflowID: "research",
			Status:    persistence.ExecutionStatusCompleted,
			StartedAt: ptr(now.Add(-10 * time.Minute)), CompletedAt: ptr(now.Add(-7 * time.Minute)), // ran 3m
		},
	}

	data := buildExecutionsData(execs, now)
	if data.CurrentPage != "executions" {
		t.Errorf("CurrentPage = %q; want executions", data.CurrentPage)
	}
	if len(data.Executions) != 2 {
		t.Fatalf("rows = %d; want 2", len(data.Executions))
	}

	run := data.Executions[0]
	if run.CurrentStep != "research" {
		t.Errorf("running current step = %q; want research", run.CurrentStep)
	}
	if run.Duration != "1m30s" {
		t.Errorf("running duration = %q; want 1m30s (now - started)", run.Duration)
	}

	done := data.Executions[1]
	if done.CurrentStep != "—" {
		t.Errorf("completed current step = %q; want — (nil CurrentStepID)", done.CurrentStep)
	}
	if done.Duration != "3m0s" {
		t.Errorf("completed duration = %q; want 3m0s (completed - started)", done.Duration)
	}
}

func TestBuildExecutionsData_HasLiveRows(t *testing.T) {
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)

	t.Run("any live row sets HasLiveRows", func(t *testing.T) {
		execs := []*persistence.Execution{
			{ID: "a", Status: persistence.ExecutionStatusCompleted},
			{ID: "b", Status: persistence.ExecutionStatusRunning},
		}
		if !buildExecutionsData(execs, now).HasLiveRows {
			t.Error("HasLiveRows = false; want true when a RUNNING row is present")
		}
	})

	t.Run("all-terminal rows clear HasLiveRows", func(t *testing.T) {
		execs := []*persistence.Execution{
			{ID: "a", Status: persistence.ExecutionStatusCompleted},
			{ID: "b", Status: persistence.ExecutionStatusFailed},
			{ID: "c", Status: persistence.ExecutionStatusCancelled},
		}
		if buildExecutionsData(execs, now).HasLiveRows {
			t.Error("HasLiveRows = true; want false when every row is terminal")
		}
	})

	t.Run("empty page has no live rows", func(t *testing.T) {
		if buildExecutionsData(nil, now).HasLiveRows {
			t.Error("HasLiveRows = true; want false for an empty page")
		}
	})
}

func TestExecutionsList_AutoRefreshGatedOnLiveRows(t *testing.T) {
	render := func(execs []*persistence.Execution) string {
		execRepo := &mocks.MockExecutionRepository{
			ListFunc: func(context.Context, persistence.ExecutionFilter) ([]*persistence.Execution, error) {
				return execs, nil
			},
			CountFunc: func(context.Context, persistence.ExecutionFilter) (int64, error) { return 0, nil },
		}
		srv := NewServer(WithExecutionRepository(execRepo))
		rec := httptest.NewRecorder()
		srv.ExecutionsList(rec, httptest.NewRequest(http.MethodGet, "/ui/executions", nil))
		return rec.Body.String()
	}

	t.Run("live rows keep the auto-refresh trigger", func(t *testing.T) {
		body := render([]*persistence.Execution{{ID: "e1", TaskID: "t1", Status: persistence.ExecutionStatusRunning}})
		if !strings.Contains(body, "hx-trigger") {
			t.Error("expected hx-trigger present while a live execution is listed")
		}
	})

	t.Run("all-terminal page drops the auto-refresh trigger", func(t *testing.T) {
		body := render([]*persistence.Execution{{ID: "e1", TaskID: "t1", Status: persistence.ExecutionStatusCompleted}})
		if strings.Contains(body, "hx-trigger") {
			t.Error("expected no hx-trigger once every listed execution is terminal (polling must stop)")
		}
	})
}

func TestBuildExecutionPalette_NonZeroInOrder(t *testing.T) {
	counts := map[persistence.ExecutionStatus]int64{
		persistence.ExecutionStatusRunning:   2,
		persistence.ExecutionStatusFailed:    1,
		persistence.ExecutionStatusCompleted: 0, // zero → omitted
	}
	pills := buildExecutionPalette(executionStatusOrder, counts)
	// Order follows executionStatusOrder (Running before Failed); zero dropped.
	if len(pills) != 2 {
		t.Fatalf("pills = %d; want 2 (zero-count Completed dropped)", len(pills))
	}
	if pills[0].Status != persistence.ExecutionStatusRunning || pills[0].Count != 2 {
		t.Errorf("pill[0] = %+v; want Running x2", pills[0])
	}
	if pills[1].Status != persistence.ExecutionStatusFailed || pills[1].Count != 1 {
		t.Errorf("pill[1] = %+v; want Failed x1", pills[1])
	}
}

func TestCountExecutionStatuses(t *testing.T) {
	execs := []*persistence.Execution{
		{Status: persistence.ExecutionStatusRunning},
		{Status: persistence.ExecutionStatusRunning},
		{Status: persistence.ExecutionStatusFailed},
	}
	counts := countExecutionStatuses(execs)
	if counts[persistence.ExecutionStatusRunning] != 2 {
		t.Errorf("running = %d; want 2", counts[persistence.ExecutionStatusRunning])
	}
	if counts[persistence.ExecutionStatusFailed] != 1 {
		t.Errorf("failed = %d; want 1", counts[persistence.ExecutionStatusFailed])
	}
}
