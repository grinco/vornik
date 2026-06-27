package cli

// Coverage sweep for `vornikctl autonomy {evaluations,summary}` plus
// shortenID. httptest harness; captureStdoutFunc from
// blackbox_triggers_test.go.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func autonomyCov_reset() {
	autonomyProject, autonomyOutcome = "", ""
	autonomyLimit, autonomyHours = 50, 24
	autonomyJSON = false
}

func TestRunAutonomyEvaluations_Table(t *testing.T) {
	taskID := "task_20260601_abcd1234"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/projects/janka/autonomy/evaluations" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.URL.Query().Get("outcome") != "CREATED" || r.URL.Query().Get("limit") != "10" {
			t.Errorf("query not forwarded: %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"evaluations": []autonomyEvalRow{
				{ID: "e1", ProjectID: "janka", Outcome: "CREATED", Reason: "ok", TaskID: &taskID, TaskType: "research", WorkflowID: "wf", DurationMs: 123},
			},
			"total": 1,
		})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	autonomyCov_reset()
	autonomyProject, autonomyOutcome, autonomyLimit = "janka", "CREATED", 10
	out, err := captureStdoutFunc(t, func() error { return runAutonomyEvaluations(autonomyEvaluationsCmd, nil) })
	if err != nil {
		t.Fatalf("runAutonomyEvaluations: %v", err)
	}
	for _, want := range []string{"CREATED", "123ms", "abcd1234", "Total: 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("eval output missing %q in:\n%s", want, out)
		}
	}
}

func TestRunAutonomyEvaluations_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"evaluations":[],"total":0}`))
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	autonomyCov_reset()
	autonomyProject = "p"
	out, err := captureStdoutFunc(t, func() error { return runAutonomyEvaluations(autonomyEvaluationsCmd, nil) })
	if err != nil {
		t.Fatalf("runAutonomyEvaluations empty: %v", err)
	}
	if !strings.Contains(out, "(no evaluations)") {
		t.Errorf("expected empty message, got %s", out)
	}
}

func TestRunAutonomyEvaluations_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "denied"})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	autonomyCov_reset()
	autonomyProject = "p"
	_, err := captureStdoutFunc(t, func() error { return runAutonomyEvaluations(autonomyEvaluationsCmd, nil) })
	if err == nil {
		t.Fatal("expected error on 403")
	}
}

func TestRunAutonomySummary_SortedByCount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/projects/p/autonomy/summary" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.URL.Query().Get("hours") != "48" {
			t.Errorf("hours not forwarded: %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"projectId": "p", "windowHrs": 48, "since": "2026-06-01",
			"counts": map[string]int64{"NO_ACTION": 10, "CREATED": 3},
		})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	autonomyCov_reset()
	autonomyProject, autonomyHours = "p", 48
	out, err := captureStdoutFunc(t, func() error { return runAutonomySummary(autonomySummaryCmd, nil) })
	if err != nil {
		t.Fatalf("runAutonomySummary: %v", err)
	}
	// NO_ACTION (10) must sort before CREATED (3).
	if strings.Index(out, "NO_ACTION") > strings.Index(out, "CREATED") {
		t.Errorf("summary not sorted by count:\n%s", out)
	}
	if !strings.Contains(out, "Total: 13") {
		t.Errorf("summary total: %s", out)
	}
}

func TestRunAutonomySummary_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"projectId": "p", "counts": map[string]int64{}})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	autonomyCov_reset()
	autonomyProject = "p"
	out, err := captureStdoutFunc(t, func() error { return runAutonomySummary(autonomySummaryCmd, nil) })
	if err != nil {
		t.Fatalf("runAutonomySummary empty: %v", err)
	}
	if !strings.Contains(out, "(no evaluations in window)") {
		t.Errorf("expected empty message, got %s", out)
	}
}
