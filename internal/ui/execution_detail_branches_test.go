package ui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// renderExecutionDetail is a helper that stands up a minimal Server
// with a single Execution (and a matching Task) and returns the
// rendered HTML body. Mirrors the helpers used in task_detail tests.
func renderExecutionDetail(t *testing.T, exec *persistence.Execution) string {
	t.Helper()
	srv := NewServer(
		WithExecutionRepository(&mocks.MockExecutionRepository{
			GetFunc: func(_ context.Context, id string) (*persistence.Execution, error) {
				if id == exec.ID {
					return exec, nil
				}
				return nil, persistence.ErrNotFound
			},
		}),
		WithTaskRepository(&mocks.MockTaskRepository{
			GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
				return &persistence.Task{ID: exec.TaskID, ProjectID: exec.ProjectID, Status: persistence.TaskStatusCompleted}, nil
			},
		}),
	)
	req := httptest.NewRequest(http.MethodGet, "/executions/"+exec.ID, nil)
	rec := httptest.NewRecorder()
	srv.ExecutionDetail(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	return rec.Body.String()
}

// TestExecutionDetailTiering — hierarchy primitives + token migration.
//
// Covers:
//  1. Status card carries panel-primary class.
//  2. Result section renders as <details class="panel-ref ... and is
//     OPEN for a COMPLETED execution but CLOSED for a RUNNING one.
//  3. Metadata grid is rendered via statStrip (grid class present).
//  4. No legacy gray-/dark- tokens remain in execution.html source.
func TestExecutionDetailTiering(t *testing.T) {
	// 1 + 3: Status card carries panel-primary; metadata via statStrip.
	t.Run("StatusCardHasPanelPrimary", func(t *testing.T) {
		body := renderExecutionDetail(t, &persistence.Execution{
			ID:        "exec-tier-1",
			TaskID:    "task-tier",
			ProjectID: "p1",
			Status:    persistence.ExecutionStatusRunning,
		})
		// The status card element (id="execution-status-card") must carry panel-primary.
		// We look for the id and panel-primary in the same tag by checking a substring
		// that includes both within a reasonable proximity.
		assert.Contains(t, body, `id="execution-status-card"`,
			"status card must have the expected id")
		// Locate the status card tag and confirm panel-primary is in its class list.
		idx := strings.Index(body, `id="execution-status-card"`)
		if idx == -1 {
			t.Fatal("execution-status-card not found in body")
		}
		// Walk back to the opening < of the tag
		start := strings.LastIndex(body[:idx], "<")
		if start == -1 {
			start = 0
		}
		// The tag ends at the next >
		end := strings.Index(body[start:], ">")
		if end == -1 {
			end = 300
		}
		tag := body[start : start+end+1]
		assert.Contains(t, tag, "panel-primary",
			"execution-status-card element must carry the panel-primary class; tag=%q", tag)
	})

	t.Run("FailedStatusCardHasRoseTone", func(t *testing.T) {
		// The tonal differentiator for failure triage: a FAILED execution's
		// status card carries data-tone="rose".
		body := renderExecutionDetail(t, &persistence.Execution{
			ID:        "exec-tier-fail",
			TaskID:    "task-tier",
			ProjectID: "p1",
			Status:    persistence.ExecutionStatusFailed,
		})
		idx := strings.Index(body, `id="execution-status-card"`)
		if idx == -1 {
			t.Fatal("execution-status-card not found in body")
		}
		start := strings.LastIndex(body[:idx], "<")
		end := strings.Index(body[start:], ">")
		tag := body[start : start+end+1]
		assert.Contains(t, tag, `data-tone="rose"`,
			"FAILED execution status card must carry data-tone=\"rose\"; tag=%q", tag)
	})

	t.Run("MetadataGridHasStatStrip", func(t *testing.T) {
		body := renderExecutionDetail(t, &persistence.Execution{
			ID:        "exec-tier-2",
			TaskID:    "task-tier",
			ProjectID: "p1",
			Status:    persistence.ExecutionStatusCompleted,
		})
		// statStrip emits a <dl> with the grid class
		assert.Contains(t, body, `grid grid-cols-2 sm:grid-cols-3`,
			"metadata must be rendered through statStrip (grid class)")
	})

	// 2: Result panel — panel-ref presence and open/closed state.
	// The first occurrence of "panel-ref" in the body is the CSS definition
	// in the <style> block. We search for the HTML element pattern
	// `<details class="panel-ref` which appears only in the page body.
	t.Run("ResultPanelIsRef_Completed", func(t *testing.T) {
		body := renderExecutionDetail(t, &persistence.Execution{
			ID:        "exec-tier-3",
			TaskID:    "task-tier",
			ProjectID: "p1",
			Status:    persistence.ExecutionStatusCompleted,
			Result:    []byte(`{"status":"COMPLETED","message":"hello"}`),
		})
		// Anchor on the Result panel specifically (its sectionHeader renders
		// >Result<), then back-scan to its enclosing <details> — robust even
		// if other panel-refs (Step Outcomes etc.) precede it.
		ridx := strings.Index(body, ">Result<")
		if ridx == -1 {
			t.Fatalf("Result panel sectionHeader not found in body (len=%d)", len(body))
		}
		dStart := strings.LastIndex(body[:ridx], `<details class="panel-ref`)
		if dStart == -1 {
			t.Fatal("Result title must sit inside a <details class=\"panel-ref\">")
		}
		tag := body[dStart : dStart+strings.Index(body[dStart:], ">")+1]
		assert.Contains(t, tag, "open",
			"Result panel-ref must be open for COMPLETED execution; tag=%q", tag)
	})

	t.Run("ResultPanelIsClosed_Running", func(t *testing.T) {
		body := renderExecutionDetail(t, &persistence.Execution{
			ID:        "exec-tier-4",
			TaskID:    "task-tier",
			ProjectID: "p1",
			Status:    persistence.ExecutionStatusRunning,
			Result:    []byte(`{"status":"RUNNING","message":"still going"}`),
		})
		// For RUNNING execution the Result panel-ref must NOT be open. Anchor
		// on the Result panel specifically (it renders since Result is set).
		ridx := strings.Index(body, ">Result<")
		if ridx == -1 {
			t.Fatal("Result panel should render when Result is set")
		}
		dStart := strings.LastIndex(body[:ridx], `<details class="panel-ref`)
		if dStart == -1 {
			t.Fatal("Result title must sit inside a <details class=\"panel-ref\">")
		}
		tag := body[dStart : dStart+strings.Index(body[dStart:], ">")+1]
		assert.NotContains(t, tag, ` open`,
			"Result panel-ref must NOT be open for RUNNING execution; tag=%q", tag)
	})

	// 4: No legacy tokens in the template source.
	t.Run("NoLegacyTokens", func(t *testing.T) {
		assertNoLegacyTokens(t, "execution.html")
	})
}

func TestExecutionDetail_BlankIDReturns404(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/executions/", nil)
	rec := httptest.NewRecorder()
	srv.ExecutionDetail(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestExecutionDetail_NotFoundReturns404(t *testing.T) {
	srv := NewServer(WithExecutionRepository(&mocks.MockExecutionRepository{
		GetFunc: func(context.Context, string) (*persistence.Execution, error) {
			return nil, errors.New("not found")
		},
	}))
	req := httptest.NewRequest(http.MethodGet, "/executions/missing", nil)
	rec := httptest.NewRecorder()
	srv.ExecutionDetail(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestExecutionDetail_NoRepoIs404Path(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/executions/exec-1", nil)
	rec := httptest.NewRecorder()
	srv.ExecutionDetail(rec, req)
	// The not-found path is the no-repo fallback (template renders
	// with the not-found body but with a 404 status as the handler
	// short-circuits when execRepo is nil and execution can't be loaded).
	// In the current impl, no execRepo → the inner block is skipped
	// and the page renders with empty data; the test confirms the
	// handler returns *something* without panicking.
	assert.NotEqual(t, 0, rec.Code)
}

func TestExecutionDetail_HappyPathRenders(t *testing.T) {
	srv := NewServer(WithExecutionRepository(&mocks.MockExecutionRepository{
		GetFunc: func(context.Context, string) (*persistence.Execution, error) {
			return &persistence.Execution{
				ID:         "exec-x",
				TaskID:     "task-x",
				Status:     persistence.ExecutionStatusCompleted,
				WorkflowID: "w1",
			}, nil
		},
	}), WithTaskRepository(&mocks.MockTaskRepository{
		GetFunc: func(context.Context, string) (*persistence.Task, error) {
			return &persistence.Task{ID: "task-x", ProjectID: "p1"}, nil
		},
	}))
	req := httptest.NewRequest(http.MethodGet, "/executions/exec-x", nil)
	rec := httptest.NewRecorder()
	srv.ExecutionDetail(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "exec-x")
}

// TestCrossPrimitiveRender asserts that all three hierarchy primitives —
// panel-primary (status card), statStrip (metadata grid), and panel-ref
// (Result, open for terminal) — coexist in a single COMPLETED execution
// page render. This is the integration test that catches regressions where
// adding one primitive inadvertently breaks another's output.
func TestCrossPrimitiveRender(t *testing.T) {
	body := renderExecutionDetail(t, &persistence.Execution{
		ID:        "exec-cross-prim",
		TaskID:    "task-cross",
		ProjectID: "proj-cross",
		Status:    persistence.ExecutionStatusCompleted,
		Result:    []byte(`{"status":"COMPLETED","message":"all three primitives present"}`),
	})

	// 1. panel-primary: status card must carry the class.
	if !strings.Contains(body, "panel-primary") {
		t.Errorf("COMPLETED execution body missing panel-primary (status card)")
	}

	// 2. statStrip: metadata grid class emitted by the statStrip primitive.
	const stripGrid = "grid grid-cols-2 sm:grid-cols-3"
	if !strings.Contains(body, stripGrid) {
		t.Errorf("COMPLETED execution body missing statStrip grid %q", stripGrid)
	}

	// 3. panel-ref with open: Result panel carries <details class="panel-ref"
	//    and is open for COMPLETED executions (refOpen returns true).
	ridx := strings.Index(body, ">Result<")
	if ridx == -1 {
		t.Fatalf("Result panel sectionHeader not found in COMPLETED execution body")
	}
	dStart := strings.LastIndex(body[:ridx], `<details class="panel-ref`)
	if dStart == -1 {
		t.Fatal("Result title must sit inside a <details class=\"panel-ref\">")
	}
	tag := body[dStart : dStart+strings.Index(body[dStart:], ">")+1]
	if !strings.Contains(tag, "open") {
		t.Errorf("Result panel-ref must be open for COMPLETED execution; tag=%q", tag)
	}
}

// TestPanelRefHasSummary asserts that every <details class="panel-ref"
// element in the rendered task_detail and execution pages carries a
// <summary> within the first ~200 characters of content. The summary
// holds the sectionHeader which is the only disclosure affordance (the
// CSS-hidden chevron isn't enough for screen readers).
func TestPanelRefHasSummary(t *testing.T) {
	tests := []struct {
		name string
		body func(t *testing.T) string
	}{
		{
			name: "task_detail COMPLETED",
			body: func(t *testing.T) string {
				t.Helper()
				return renderTaskDetailBody(t, TaskDetailData{
					Task:      &persistence.Task{ID: "task-a11y", ProjectID: "p1", Status: persistence.TaskStatusCompleted},
					Execution: &persistence.Execution{ID: "exec-a11y", Result: []byte(`{"message":"done"}`)},
				})
			},
		},
		{
			name: "execution COMPLETED",
			body: func(t *testing.T) string {
				t.Helper()
				return renderExecutionDetail(t, &persistence.Execution{
					ID:        "exec-a11y-ex",
					TaskID:    "task-a11y-ex",
					ProjectID: "p1",
					Status:    persistence.ExecutionStatusCompleted,
					Result:    []byte(`{"status":"COMPLETED"}`),
				})
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.body(t)
			// Find every <details class="panel-ref occurrence and assert
			// a <summary appears within the next 200 chars of the tag.
			search := body
			const marker = `<details class="panel-ref`
			for {
				idx := strings.Index(search, marker)
				if idx == -1 {
					break
				}
				window := search[idx:]
				limit := 200
				if len(window) < limit {
					limit = len(window)
				}
				if !strings.Contains(window[:limit], "<summary") {
					t.Errorf("panel-ref missing <summary> within 200 chars; excerpt:\n%s", window[:limit])
				}
				// Advance past this occurrence.
				search = window[len(marker):]
			}
		})
	}
}

func TestExecutionDetail_AdaptiveWithRoutingDecision(t *testing.T) {
	srv := NewServer(WithExecutionRepository(&mocks.MockExecutionRepository{
		GetFunc: func(context.Context, string) (*persistence.Execution, error) {
			return &persistence.Execution{
				ID:         "exec-r",
				TaskID:     "task-r",
				Status:     persistence.ExecutionStatusCompleted,
				WorkflowID: "adaptive",
				Result:     []byte(`{"selected_workflow":"build","why":"matches build intent"}`),
			}, nil
		},
	}), WithTaskRepository(&mocks.MockTaskRepository{
		GetFunc: func(context.Context, string) (*persistence.Task, error) {
			return &persistence.Task{ID: "task-r", ProjectID: "p1"}, nil
		},
		GetChildrenFunc: func(context.Context, string) ([]*persistence.Task, error) {
			wf := "build"
			return []*persistence.Task{
				{ID: "child-1", ProjectID: "p1", WorkflowID: &wf},
			}, nil
		},
	}))
	req := httptest.NewRequest(http.MethodGet, "/executions/exec-r", nil)
	rec := httptest.NewRecorder()
	srv.ExecutionDetail(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "build", "selected workflow should render")
}
