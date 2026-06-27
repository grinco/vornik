package cli

// Coverage sweep for `vornikctl memory {search,stats}` (the HTTP-backed
// memory commands; reassign/wipe/scope/dlq are DB-backed and need
// Postgres). httptest harness; captureStdoutFunc from
// blackbox_triggers_test.go.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunMemorySearch_Table(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/projects/janka/memory/search" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.URL.Query().Get("q") != "rate limits" || r.URL.Query().Get("limit") != "5" {
			t.Errorf("query not forwarded: %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"results":[{"chunk_id":"c1","source_name":"doc.md","task_id":"t1","content":"some content","score":0.91}]}`))
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	memorySearchProject, memorySearchQuery, memorySearchLimit, memorySearchJSON = "janka", "rate limits", 5, false
	out, err := captureStdoutFunc(t, func() error { return runMemorySearch(memorySearchCmd, nil) })
	if err != nil {
		t.Fatalf("runMemorySearch: %v", err)
	}
	for _, want := range []string{"score=0.9100", "doc.md", "some content"} {
		if !strings.Contains(out, want) {
			t.Errorf("search output missing %q in:\n%s", want, out)
		}
	}
}

func TestRunMemorySearch_EmptyResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	memorySearchProject, memorySearchQuery, memorySearchLimit, memorySearchJSON = "p", "x", 10, false
	out, err := captureStdoutFunc(t, func() error { return runMemorySearch(memorySearchCmd, nil) })
	if err != nil {
		t.Fatalf("runMemorySearch: %v", err)
	}
	if !strings.Contains(out, "(no results)") {
		t.Errorf("expected no-results message, got %s", out)
	}
}

func TestRunMemorySearch_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "no project"})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	memorySearchProject, memorySearchQuery, memorySearchLimit = "p", "x", 10
	_, err := captureStdoutFunc(t, func() error { return runMemorySearch(memorySearchCmd, nil) })
	if err == nil {
		t.Fatal("expected error on 404")
	}
}

func TestRunMemoryStats_TableAndCoverage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/memory/stats" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"projects":[
			{"projectId":"beta","chunksTotal":10,"chunksEmbedded":5,"queueDepth":2},
			{"projectId":"alpha","chunksTotal":0,"chunksEmbedded":0,"queueDepth":0}
		],"total":2}`))
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	memoryStatsProject, memoryStatsJSON = "", false
	out, err := captureStdoutFunc(t, func() error { return runMemoryStats(memoryStatsCmd, nil) })
	if err != nil {
		t.Fatalf("runMemoryStats: %v", err)
	}
	// 5/10 = 50%; zero-total renders the em-dash.
	if !strings.Contains(out, "50.0%") || !strings.Contains(out, "—") {
		t.Errorf("stats coverage output: %s", out)
	}
	// alpha sorts before beta.
	if strings.Index(out, "alpha") > strings.Index(out, "beta") {
		t.Errorf("stats not sorted: %s", out)
	}
}

func TestRunMemoryStats_ProjectFilterJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"projects":[
			{"projectId":"keep","chunksTotal":4,"chunksEmbedded":4,"queueDepth":0},
			{"projectId":"drop","chunksTotal":1,"chunksEmbedded":0,"queueDepth":1}
		],"total":2}`))
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	memoryStatsProject, memoryStatsJSON = "keep", true
	defer func() { memoryStatsProject, memoryStatsJSON = "", false }()
	out, err := captureStdoutFunc(t, func() error { return runMemoryStats(memoryStatsCmd, nil) })
	if err != nil {
		t.Fatalf("runMemoryStats json: %v", err)
	}
	if strings.Contains(out, "drop") {
		t.Errorf("filter leaked other project: %s", out)
	}
	if !strings.Contains(out, "keep") {
		t.Errorf("filter dropped requested project: %s", out)
	}
}

func TestRunMemoryWipe_RequiresProject(t *testing.T) {
	// The --project guard runs before config.Load(), so this is
	// hermetic (no DB).
	memoryWipeProject = ""
	if err := runMemoryWipe(memoryWipeCmd, nil); err == nil || !strings.Contains(err.Error(), "--project is required") {
		t.Fatalf("expected --project guard, got %v", err)
	}
}

func TestRunMemoryReassign_ValidationBranches(t *testing.T) {
	// These two validation checks run before any config/DB access.
	memoryReassignFrom, memoryReassignTo = "", ""
	if err := runMemoryReassign(memoryReassignCmd, nil); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("expected required error, got %v", err)
	}
	memoryReassignFrom, memoryReassignTo = "same", "same"
	if err := runMemoryReassign(memoryReassignCmd, nil); err == nil || !strings.Contains(err.Error(), "must differ") {
		t.Fatalf("expected must-differ error, got %v", err)
	}
	memoryReassignFrom, memoryReassignTo = "", ""
}
