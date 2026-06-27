package cli

// Coverage sweep for `vornikctl memory backfill-titles`. The command is
// HTTP-backed (postJSON), so the full probe → dry-run / batch-loop flow
// is exercisable with the httptest harness. captureStdoutFunc from
// blackbox_triggers_test.go.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func backfillCov_reset() {
	backfillTitlesBatchSize, backfillTitlesMax = 10, 0
	backfillTitlesDryRun, backfillTitlesJSON = false, false
}

func TestRunMemoryBackfillTitles_DryRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("count") != "true" {
			t.Errorf("dry-run should probe with count=true: %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(backfillBatchResponse{Remaining: 42})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	backfillCov_reset()
	backfillTitlesDryRun = true
	out, err := captureStdoutFunc(t, func() error { return runMemoryBackfillTitles(memoryBackfillTitlesCmd, nil) })
	if err != nil {
		t.Fatalf("backfill dry-run: %v", err)
	}
	if !strings.Contains(out, "42 chunks missing") || !strings.Contains(out, "dry-run") {
		t.Errorf("dry-run output: %s", out)
	}
}

func TestRunMemoryBackfillTitles_NothingToDo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(backfillBatchResponse{Remaining: 0})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	backfillCov_reset()
	out, err := captureStdoutFunc(t, func() error { return runMemoryBackfillTitles(memoryBackfillTitlesCmd, nil) })
	if err != nil {
		t.Fatalf("backfill nothing-to-do: %v", err)
	}
	if !strings.Contains(out, "nothing to do") {
		t.Errorf("nothing-to-do output: %s", out)
	}
}

func TestRunMemoryBackfillTitles_BatchLoopCompletes(t *testing.T) {
	var call int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&call, 1)
		if r.URL.Query().Get("count") == "true" {
			// Probe.
			_ = json.NewEncoder(w).Encode(backfillBatchResponse{Remaining: 5})
			return
		}
		// First batch processes 3 (remaining 2), second clears it.
		if n == 2 {
			_ = json.NewEncoder(w).Encode(backfillBatchResponse{Processed: 3, Succeeded: 3, Remaining: 2})
		} else {
			_ = json.NewEncoder(w).Encode(backfillBatchResponse{Processed: 2, Succeeded: 2, Remaining: 0})
		}
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	backfillCov_reset()
	backfillTitlesBatchSize = 3
	out, err := captureStdoutFunc(t, func() error { return runMemoryBackfillTitles(memoryBackfillTitlesCmd, nil) })
	if err != nil {
		t.Fatalf("backfill batch loop: %v", err)
	}
	if !strings.Contains(out, "done. processed=5") || !strings.Contains(out, "succeeded=5") {
		t.Errorf("batch loop output: %s", out)
	}
}

func TestRunMemoryBackfillTitles_StallDetection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("count") == "true" {
			_ = json.NewEncoder(w).Encode(backfillBatchResponse{Remaining: 4})
			return
		}
		// Every batch processes rows but makes zero forward progress
		// (all fail, remaining stays >= prev) → stall after 2 batches.
		_ = json.NewEncoder(w).Encode(backfillBatchResponse{Processed: 4, Failed: 4, Remaining: 4})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	backfillCov_reset()
	_, err := captureStdoutFunc(t, func() error { return runMemoryBackfillTitles(memoryBackfillTitlesCmd, nil) })
	if err == nil || !strings.Contains(err.Error(), "stalled") {
		t.Fatalf("expected stall error, got %v", err)
	}
}

func TestRunMemoryBackfillTitles_ProbeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "boom"})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	backfillCov_reset()
	_, err := captureStdoutFunc(t, func() error { return runMemoryBackfillTitles(memoryBackfillTitlesCmd, nil) })
	if err == nil || !strings.Contains(err.Error(), "probe remaining") {
		t.Fatalf("expected probe error, got %v", err)
	}
}

func TestRunMemoryBackfillTitles_MaxCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("count") == "true" {
			_ = json.NewEncoder(w).Encode(backfillBatchResponse{Remaining: 100})
			return
		}
		// One batch of 2 (the --max), still 98 remaining → loop exits on --max.
		_ = json.NewEncoder(w).Encode(backfillBatchResponse{Processed: 2, Succeeded: 2, Remaining: 98})
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	backfillCov_reset()
	backfillTitlesMax = 2
	out, err := captureStdoutFunc(t, func() error { return runMemoryBackfillTitles(memoryBackfillTitlesCmd, nil) })
	if err != nil {
		t.Fatalf("backfill max cap: %v", err)
	}
	if !strings.Contains(out, "stopping after 2") || !strings.Contains(out, "processed=2") {
		t.Errorf("max-cap output: %s", out)
	}
}
