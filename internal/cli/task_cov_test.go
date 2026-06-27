package cli

// Coverage sweep for `vornikctl task {submit,get,list,cancel,retry,tail}`.
// httptest harness shared with the rest of the package; captureStdoutFunc
// from blackbox_triggers_test.go.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func taskCov_reset() {
	taskListProject, taskListStatus, taskListJSON = "", "", false
	taskCancelProject, taskRetryProject, taskRetryResetFlag = "", "", false
	taskTailProject, taskTailLines, taskTailFollow = "", 200, false
	taskSubmitProject, taskSubmitWorkflow, taskSubmitTaskType = "", "", "research"
	taskSubmitPriority, taskSubmitPrompt, taskSubmitContextJSON = 0, "", ""
	taskSubmitIdempotencyKey, taskSubmitJSON = "", false
	taskSubmitAttach = nil
	taskGetProject, taskGetJSON = "", false
}

func TestRunTaskSubmit_PromptShorthand(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/projects/janka/tasks" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !strings.Contains(string(body["context"]), "Summarise") {
			t.Errorf("prompt not wrapped into context: %s", body["context"])
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"taskId": "task-1", "status": "QUEUED"})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	taskCov_reset()
	taskSubmitProject, taskSubmitPrompt = "janka", "Summarise yesterday"
	out, err := captureStdoutFunc(t, func() error { return runTaskSubmit(taskSubmitCmd, nil) })
	if err != nil {
		t.Fatalf("runTaskSubmit: %v", err)
	}
	if !strings.Contains(out, "task submitted: task-1") {
		t.Errorf("submit output: %s", out)
	}
}

func TestRunTaskSubmit_PromptAndContextMutuallyExclusive(t *testing.T) {
	taskCov_reset()
	taskSubmitProject, taskSubmitPrompt, taskSubmitContextJSON = "p", "x", "{}"
	err := runTaskSubmit(taskSubmitCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutual-exclusion error, got %v", err)
	}
}

func TestRunTaskSubmit_BadContextJSON(t *testing.T) {
	taskCov_reset()
	taskSubmitProject, taskSubmitContextJSON = "p", "{not json}"
	err := runTaskSubmit(taskSubmitCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("expected invalid-JSON error, got %v", err)
	}
}

func TestRunTaskSubmit_AttachMissingFile(t *testing.T) {
	taskCov_reset()
	taskSubmitProject, taskSubmitAttach = "p", []string{"/no/such/file-xyz"}
	err := runTaskSubmit(taskSubmitCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "read --attach") {
		t.Fatalf("expected attach-read error, got %v", err)
	}
}

func TestRunTaskSubmit_AttachEncodedAndErrorBody(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "doc.txt")
	if err := os.WriteFile(fp, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		arts, _ := body["inputArtifacts"].([]any)
		if len(arts) != 1 {
			t.Errorf("inputArtifacts not forwarded: %+v", body)
		}
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "bad request"})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	taskCov_reset()
	taskSubmitProject, taskSubmitAttach = "p", []string{fp}
	_, err := captureStdoutFunc(t, func() error { return runTaskSubmit(taskSubmitCmd, nil) })
	if err == nil {
		t.Fatal("expected error on 400")
	}
}

func TestRunTaskGet_HumanFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(taskResponse{
			TaskID: "task-9", ProjectID: "p", Status: "FAILED", TaskType: "research",
			Priority: 5, CreatedAt: "2026-06-01", LastError: "boom",
		})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	taskCov_reset()
	taskGetProject = "p"
	out, err := captureStdoutFunc(t, func() error { return runTaskGet(taskGetCmd, []string{"task-9"}) })
	if err != nil {
		t.Fatalf("runTaskGet: %v", err)
	}
	for _, want := range []string{"task-9", "FAILED", "research", "boom"} {
		if !strings.Contains(out, want) {
			t.Errorf("get output missing %q in:\n%s", want, out)
		}
	}
}

func TestRunTaskList_TableAndStatusFilter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("status") != "RUNNING" {
			t.Errorf("status filter not forwarded: %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(listTasksResponse{
			Tasks: []taskResponse{{TaskID: "t-1", Status: "RUNNING", TaskType: "x", Priority: 1, CreatedAt: "2026"}},
			Total: 1,
		})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	taskCov_reset()
	taskListProject, taskListStatus = "p", "RUNNING"
	out, err := captureStdoutFunc(t, func() error { return runTaskList(taskListCmd, nil) })
	if err != nil {
		t.Fatalf("runTaskList: %v", err)
	}
	if !strings.Contains(out, "t-1") || !strings.Contains(out, "Total: 1") {
		t.Errorf("list output: %s", out)
	}
}

func TestRunTaskList_JSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(listTasksResponse{Tasks: []taskResponse{{TaskID: "tj"}}, Total: 1})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	taskCov_reset()
	taskListProject, taskListJSON = "p", true
	out, err := captureStdoutFunc(t, func() error { return runTaskList(taskListCmd, nil) })
	if err != nil {
		t.Fatalf("runTaskList: %v", err)
	}
	var decoded listTasksResponse
	if jerr := json.Unmarshal([]byte(out), &decoded); jerr != nil || decoded.Tasks[0].TaskID != "tj" {
		t.Fatalf("bad JSON output: %v %s", jerr, out)
	}
}

func TestRunTaskCancel_PrintsResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/cancel") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(cancelTaskResponse{TaskID: "t-c", Status: "CANCELLED", WasRunning: true, CancelledAt: "now"})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	taskCov_reset()
	taskCancelProject = "p"
	out, err := captureStdoutFunc(t, func() error { return runTaskCancel(taskCancelCmd, []string{"t-c"}) })
	if err != nil {
		t.Fatalf("runTaskCancel: %v", err)
	}
	if !strings.Contains(out, "t-c cancelled") || !strings.Contains(out, "Was running: true") {
		t.Errorf("cancel output: %s", out)
	}
}

func TestRunTaskCancel_Non200IsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "wrong state"})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	taskCov_reset()
	taskCancelProject = "p"
	_, err := captureStdoutFunc(t, func() error { return runTaskCancel(taskCancelCmd, []string{"t"}) })
	if err == nil {
		t.Fatal("expected error on 409")
	}
}

func TestRunTaskRetry_WithResetAttempts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]bool
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !body["resetAttempts"] {
			t.Errorf("resetAttempts not forwarded: %+v", body)
		}
		_ = json.NewEncoder(w).Encode(retryTaskResponse{TaskID: "t-r", Status: "QUEUED", Attempt: 1, RetriedAt: "now"})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	taskCov_reset()
	taskRetryProject, taskRetryResetFlag = "p", true
	out, err := captureStdoutFunc(t, func() error { return runTaskRetry(taskRetryCmd, []string{"t-r"}) })
	if err != nil {
		t.Fatalf("runTaskRetry: %v", err)
	}
	if !strings.Contains(out, "t-r retried") || !strings.Contains(out, "Attempt: 1") {
		t.Errorf("retry output: %s", out)
	}
}

func TestRunTaskTail_NoFollowPrintsLogs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "tail=") {
			t.Errorf("tail param missing: %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte("log line one\nlog line two"))
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	taskCov_reset()
	taskTailProject, taskTailFollow = "p", false
	out, err := captureStdoutFunc(t, func() error { return runTaskTail(taskTailCmd, []string{"t-1"}) })
	if err != nil {
		t.Fatalf("runTaskTail: %v", err)
	}
	if !strings.Contains(out, "log line one") || !strings.Contains(out, "log line two") {
		t.Errorf("tail output: %s", out)
	}
}

func TestRunTaskTail_Non200IsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "no task"})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	taskCov_reset()
	taskTailProject = "p"
	_, err := captureStdoutFunc(t, func() error { return runTaskTail(taskTailCmd, []string{"t"}) })
	if err == nil {
		t.Fatal("expected error on 404")
	}
}
