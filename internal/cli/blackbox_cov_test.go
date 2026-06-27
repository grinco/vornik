package cli

// Coverage sweep for `vornikctl blackbox {trace,replay,scorecard,sideeffects}`
// plus the small map-accessor helpers. httptest harness; captureStdoutFunc
// from blackbox_triggers_test.go.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func blackboxCov_reset() {
	blackboxTraceJSON = false
	blackboxReplayVariable, blackboxReplayValue, blackboxReplayRole, blackboxReplayLabel = "", "", "", ""
	blackboxReplayJSON, blackboxScorecardJSON, blackboxSideJSON = false, false, false
}

func TestRunBlackBoxTrace_HumanReadable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/admin/blackbox/traces/") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"header": map[string]any{
				"task_id": "task-1", "project_id": "p", "workflow_id": "wf",
				"status": "COMPLETED", "total_cost_usd": 0.1234, "trace_digest": "abc",
			},
			"counts": map[string]any{
				"events": 10, "llm_calls": 4, "tool_calls": 3, "memory_reads": 2, "steps": 1, "judge_verdicts": 1,
			},
		})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	blackboxCov_reset()
	out, err := captureStdoutFunc(t, func() error { return runBlackBoxTrace(blackboxTraceCmd, []string{"task-1"}) })
	if err != nil {
		t.Fatalf("runBlackBoxTrace: %v", err)
	}
	for _, want := range []string{"task-1", "COMPLETED", "$0.1234", "LLM=4", "Tool=3"} {
		if !strings.Contains(out, want) {
			t.Errorf("trace output missing %q in:\n%s", want, out)
		}
	}
}

func TestRunBlackBoxTrace_JSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"header":{"task_id":"tj"}}`))
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	blackboxCov_reset()
	blackboxTraceJSON = true
	out, err := captureStdoutFunc(t, func() error { return runBlackBoxTrace(blackboxTraceCmd, []string{"tj"}) })
	if err != nil {
		t.Fatalf("trace json: %v", err)
	}
	if !strings.Contains(out, "tj") {
		t.Errorf("json output: %s", out)
	}
}

func TestRunBlackBoxTrace_403IsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "admin required"})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	blackboxCov_reset()
	_, err := captureStdoutFunc(t, func() error { return runBlackBoxTrace(blackboxTraceCmd, []string{"t"}) })
	if err == nil {
		t.Fatal("expected error on 403")
	}
}

func TestRunBlackBoxReplay_RequiresFlags(t *testing.T) {
	blackboxCov_reset()
	if err := runBlackBoxReplay(blackboxReplayCmd, []string{"t"}); err == nil || !strings.Contains(err.Error(), "--variable") {
		t.Fatalf("expected --variable error, got %v", err)
	}
	blackboxReplayVariable = "model"
	if err := runBlackBoxReplay(blackboxReplayCmd, []string{"t"}); err == nil || !strings.Contains(err.Error(), "--value") {
		t.Fatalf("expected --value error, got %v", err)
	}
	blackboxReplayValue = "claude-x"
	if err := runBlackBoxReplay(blackboxReplayCmd, []string{"t"}); err == nil || !strings.Contains(err.Error(), "--label") {
		t.Fatalf("expected --label error, got %v", err)
	}
}

func TestRunBlackBoxReplay_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/admin/blackbox/replay" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["variable"] != "model" || body["role"] != "researcher" {
			t.Errorf("body not forwarded: %+v", body)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(blackboxReplayResp{
			TaskID: "new-1", OriginalTaskID: "task-1", Variable: "model", Label: "try gpt",
			StampWarning: "stamp drift", SideEffectingToolsHint: "broker stubbed",
		})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	blackboxCov_reset()
	blackboxReplayVariable, blackboxReplayValue, blackboxReplayLabel, blackboxReplayRole = "model", "claude-x", "try gpt", "researcher"
	out, err := captureStdoutFunc(t, func() error { return runBlackBoxReplay(blackboxReplayCmd, []string{"task-1"}) })
	if err != nil {
		t.Fatalf("runBlackBoxReplay: %v", err)
	}
	for _, want := range []string{"new-1", "task-1", "stamp drift", "broker stubbed"} {
		if !strings.Contains(out, want) {
			t.Errorf("replay output missing %q in:\n%s", want, out)
		}
	}
}

func TestRunBlackBoxScorecard_HumanReadable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/admin/blackbox/scorecard/") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		sc := blackboxScorecardResp{StatusChanged: true, CostDeltaUSD: 0.05, CostDeltaPct: 25, Findings: []string{"cost rose 25%"}}
		sc.Trace1Header.TaskID = "a"
		sc.Trace1Header.Status = "COMPLETED"
		sc.Trace2Header.TaskID = "b"
		sc.Trace2Header.Status = "FAILED"
		_ = json.NewEncoder(w).Encode(sc)
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	blackboxCov_reset()
	out, err := captureStdoutFunc(t, func() error { return runBlackBoxScorecard(blackboxScorecardCmd, []string{"a", "b"}) })
	if err != nil {
		t.Fatalf("runBlackBoxScorecard: %v", err)
	}
	for _, want := range []string{"CHANGED", "cost rose 25%", "Findings"} {
		if !strings.Contains(out, want) {
			t.Errorf("scorecard output missing %q in:\n%s", want, out)
		}
	}
}

func TestRunBlackBoxScorecard_NoFindings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(blackboxScorecardResp{})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	blackboxCov_reset()
	out, err := captureStdoutFunc(t, func() error { return runBlackBoxScorecard(blackboxScorecardCmd, []string{"a", "b"}) })
	if err != nil {
		t.Fatalf("scorecard: %v", err)
	}
	if !strings.Contains(out, "(none)") || !strings.Contains(out, "same") {
		t.Errorf("scorecard empty output: %s", out)
	}
}

func TestRunBlackBoxSideEffects_HumanReadable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/admin/blackbox/sideeffects" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"replay_safe_tools": []any{"web_search", "memory_read"},
			"policy":            "deny-by-default",
			"enforcement":       "strict",
			"note":              "tune via config",
		})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	blackboxCov_reset()
	out, err := captureStdoutFunc(t, func() error { return runBlackBoxSideEffects(blackboxSideEffectsCmd, nil) })
	if err != nil {
		t.Fatalf("runBlackBoxSideEffects: %v", err)
	}
	for _, want := range []string{"web_search", "memory_read", "deny-by-default", "tune via config"} {
		if !strings.Contains(out, want) {
			t.Errorf("sideeffects output missing %q in:\n%s", want, out)
		}
	}
}

func TestBlackBoxMapHelpers(t *testing.T) {
	if statusFlag(true) != "CHANGED" || statusFlag(false) != "same" {
		t.Error("statusFlag wrong")
	}
	m := map[string]any{"f": float64(3), "i": 7}
	if intFromMap(m, "f") != 3 || intFromMap(m, "i") != 7 || intFromMap(m, "missing") != 0 {
		t.Error("intFromMap wrong")
	}
	if intFromMap(nil, "x") != 0 {
		t.Error("intFromMap nil should be 0")
	}
	if floatFromMap(m, "f") != 3 || floatFromMap(m, "missing") != 0 || floatFromMap(nil, "x") != 0 {
		t.Error("floatFromMap wrong")
	}
}
