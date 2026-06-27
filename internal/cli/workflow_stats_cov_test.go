package cli

// Coverage sweep for `vornikctl workflow-stats` plus the pure render
// helpers (renderWorkflowStats, renderOutcomeDist, sortedKeys).
// httptest harness; captureStdoutFunc from blackbox_triggers_test.go.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func wfStatsCov_reset() {
	wfStatsWorkflow, wfStatsSince, wfStatsJSON = "", "7d", false
}

func TestRunWorkflowStats_RequiresWorkflow(t *testing.T) {
	wfStatsCov_reset()
	if err := runWorkflowStats(workflowStatsCmd, nil); err == nil || !strings.Contains(err.Error(), "--workflow is required") {
		t.Fatalf("expected --workflow error, got %v", err)
	}
}

func TestRunWorkflowStats_HumanReadable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/admin/workflow-stats" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.URL.Query().Get("workflow") != "dev-pipeline" || r.URL.Query().Get("since") != "24h" {
			t.Errorf("query not forwarded: %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(workflowStatsResponse{
			WorkflowID: "dev-pipeline", WindowStart: "2026-06-01T00:00:00Z", WindowEnd: "2026-06-02T00:00:00Z",
			RunCount: 10, SuccessCount: 8, FailureCount: 2, AvgCostUSD: 0.12, AvgDurationSeconds: 30,
			HallucinationRate: 0.1, OperatorInterventionRate: 0.05,
			JudgeVerdictDist: map[string]int{"pass": 8, "fail": 2},
			Steps: []workflowStatsStep{
				{StepID: "gather", Role: "researcher", Model: "claude", OutcomeDist: map[string]int{"ok": 8, "err": 2}, AvgDurationSeconds: 5, TopErrorClass: "timeout"},
			},
			TopFailureClasses: []workflowStatsFailureClass{{ErrorClass: "timeout", Count: 2}},
		})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	wfStatsCov_reset()
	wfStatsWorkflow, wfStatsSince = "dev-pipeline", "24h"
	out, err := captureStdoutFunc(t, func() error { return runWorkflowStats(workflowStatsCmd, nil) })
	if err != nil {
		t.Fatalf("runWorkflowStats: %v", err)
	}
	for _, want := range []string{"dev-pipeline", "10 runs", "8 completed", "gather", "researcher", "timeout", "Top failure classes"} {
		if !strings.Contains(out, want) {
			t.Errorf("stats output missing %q in:\n%s", want, out)
		}
	}
}

func TestRunWorkflowStats_JSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(workflowStatsResponse{WorkflowID: "wfj", RunCount: 1})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	wfStatsCov_reset()
	wfStatsWorkflow, wfStatsJSON = "wfj", true
	out, err := captureStdoutFunc(t, func() error { return runWorkflowStats(workflowStatsCmd, nil) })
	if err != nil {
		t.Fatalf("runWorkflowStats json: %v", err)
	}
	var decoded workflowStatsResponse
	if jerr := json.Unmarshal([]byte(out), &decoded); jerr != nil || decoded.WorkflowID != "wfj" {
		t.Fatalf("bad JSON output: %v %s", jerr, out)
	}
}

func TestRunWorkflowStats_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "admin required"})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	wfStatsCov_reset()
	wfStatsWorkflow = "wf"
	_, err := captureStdoutFunc(t, func() error { return runWorkflowStats(workflowStatsCmd, nil) })
	if err == nil {
		t.Fatal("expected error on 403")
	}
}

func TestRenderWorkflowStats_NoRuns(t *testing.T) {
	// renderWorkflowStats writes to an *os.File; use a pipe so we can
	// assert the no-runs branch collapses to one line.
	r, w, _ := os.Pipe()
	err := renderWorkflowStats(w, &workflowStatsResponse{WorkflowID: "quiet", RunCount: 0})
	_ = w.Close()
	if err != nil {
		t.Fatalf("renderWorkflowStats: %v", err)
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	if !strings.Contains(buf.String(), "no runs in window") {
		t.Errorf("no-runs output: %s", buf.String())
	}
}

func TestRenderOutcomeDist_OrderingAndEmpty(t *testing.T) {
	if got := renderOutcomeDist(nil); got != "—" {
		t.Errorf("empty dist = %q", got)
	}
	// ok=5 before failed=2 (count desc); equal counts tie-break alpha.
	got := renderOutcomeDist(map[string]int{"failed": 2, "ok": 5, "a": 2})
	if !strings.HasPrefix(got, "ok=5") {
		t.Errorf("ordering wrong: %q", got)
	}
	if strings.Index(got, "a=2") > strings.Index(got, "failed=2") {
		t.Errorf("tie-break not alphabetical: %q", got)
	}
}

func TestSortedKeys_Alphabetical(t *testing.T) {
	got := sortedKeys(map[string]int{"c": 1, "a": 1, "b": 1})
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("sortedKeys = %v", got)
	}
}
