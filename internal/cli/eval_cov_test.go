package cli

// Coverage sweep for `vornikctl eval run`. The all-pass path is the only
// safe one to exercise end-to-end because runEval calls os.Exit(1) when
// any case fails — so every case here ends COMPLETED with an empty
// expectation (which the predicate treats as "reached COMPLETED = pass").
// httptest harness; captureStdoutFunc from blackbox_triggers_test.go.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func evalCov_reset() {
	evalProject, evalFile = "", ""
	evalWait, evalJSON = true, false
	evalTimeout = 30 * time.Minute
}

func evalCov_writeSuite(t *testing.T, suite evalSuite) string {
	t.Helper()
	dir := t.TempDir()
	fp := filepath.Join(dir, "suite.json")
	data, _ := json.Marshal(suite)
	if err := os.WriteFile(fp, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return fp
}

func TestLoadEvalSuite_MissingFile(t *testing.T) {
	if _, err := loadEvalSuite("nope", "/no/such/eval-suite.json"); err == nil {
		t.Fatal("expected read error for missing suite")
	}
}

func TestLoadEvalSuite_Malformed(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(fp, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadEvalSuite("bad", fp); err == nil || !strings.Contains(err.Error(), "parse eval suite") {
		t.Fatalf("expected parse error, got %v", err)
	}
}

func TestRunEval_MissingProject(t *testing.T) {
	evalCov_reset()
	fp := evalCov_writeSuite(t, evalSuite{Cases: []evalCase{{Name: "c1", Prompt: "do x"}}})
	evalFile = fp
	if err := runEval(evalRunCmd, []string{"my-swarm"}); err == nil || !strings.Contains(err.Error(), "project_id is required") {
		t.Fatalf("expected project_id error, got %v", err)
	}
}

func TestRunEval_NoCases(t *testing.T) {
	evalCov_reset()
	fp := evalCov_writeSuite(t, evalSuite{ProjectID: "p", Cases: nil})
	evalFile = fp
	if err := runEval(evalRunCmd, []string{"my-swarm"}); err == nil || !strings.Contains(err.Error(), "no cases") {
		t.Fatalf("expected no-cases error, got %v", err)
	}
}

func TestRunEval_AllPassNoWaitScoreboard(t *testing.T) {
	// Isolate the last-run state file so the regression diff has no prior.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/tasks") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		// Submit returns COMPLETED immediately so the empty-expectation
		// predicate passes without the wait loop.
		_ = json.NewEncoder(w).Encode(evalCreateResponse{TaskID: "task-" + r.URL.Path, Status: "COMPLETED"})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	evalCov_reset()
	evalWait = false
	fp := evalCov_writeSuite(t, evalSuite{
		ProjectID: "janka",
		Cases:     []evalCase{{Name: "smoke-1", Prompt: "do x"}, {Name: "smoke-2", Prompt: "do y"}},
	})
	evalFile = fp
	out, err := captureStdoutFunc(t, func() error { return runEval(evalRunCmd, []string{"my-swarm"}) })
	if err != nil {
		t.Fatalf("runEval: %v", err)
	}
	if !strings.Contains(out, "2/2 passed") {
		t.Errorf("scoreboard output: %s", out)
	}
}

func TestRunEval_AllPassJSON(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(evalCreateResponse{TaskID: "t-1", Status: "COMPLETED"})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	evalCov_reset()
	evalWait, evalJSON = false, true
	evalProject = "janka"
	fp := evalCov_writeSuite(t, evalSuite{Cases: []evalCase{{Name: "c1", Prompt: "p"}}})
	evalFile = fp
	out, err := captureStdoutFunc(t, func() error { return runEval(evalRunCmd, []string{"sw"}) })
	if err != nil {
		t.Fatalf("runEval json: %v", err)
	}
	var decoded map[string]any
	if jerr := json.Unmarshal([]byte(out), &decoded); jerr != nil {
		t.Fatalf("output not JSON: %v\n%s", jerr, out)
	}
	if decoded["swarm"] != "sw" {
		t.Errorf("json swarm field: %+v", decoded)
	}
}

func TestRunEval_SubmitErrorBubbles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "bad task"})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	evalCov_reset()
	evalProject = "p"
	fp := evalCov_writeSuite(t, evalSuite{Cases: []evalCase{{Name: "c1", Prompt: "p"}}})
	evalFile = fp
	_, err := captureStdoutFunc(t, func() error { return runEval(evalRunCmd, []string{"sw"}) })
	if err == nil || !strings.Contains(err.Error(), "submit") {
		t.Fatalf("expected submit error, got %v", err)
	}
}

func TestFetchEvalTask_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "no task"})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	if _, err := fetchEvalTask(ClientFromEnv(), "p", "t"); err == nil {
		t.Fatal("expected error on 404")
	}
}

func TestFetchEvalExecutionResult_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/executions/exec-1" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(evalExecutionResponse{ID: "exec-1", Status: "COMPLETED", Result: json.RawMessage(`{"answer":42}`)})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	res, err := fetchEvalExecutionResult(ClientFromEnv(), "exec-1")
	if err != nil {
		t.Fatalf("fetchEvalExecutionResult: %v", err)
	}
	if !strings.Contains(string(res), "42") {
		t.Errorf("result: %s", res)
	}
}
