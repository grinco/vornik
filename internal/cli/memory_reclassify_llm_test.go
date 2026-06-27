package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// llmHandlerSpec drives a controllable httptest server for the LLM
// reclassify loop. Each test sets up batchResponses; the handler
// returns them in order.
type llmHandlerSpec struct {
	probeRemaining int
	probeErr       string
	batchResponses []llmReclassifyResponse
	batchErr       string
	batchCalls     atomic.Int32
	lastQuery      string
}

func newLLMTestServer(t *testing.T, spec *llmHandlerSpec) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		spec.lastQuery = r.URL.RawQuery
		if r.URL.Query().Get("count") == "true" {
			if spec.probeErr != "" {
				http.Error(w, spec.probeErr, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(llmReclassifyResponse{Remaining: spec.probeRemaining})
			return
		}
		idx := int(spec.batchCalls.Add(1)) - 1
		if spec.batchErr != "" {
			http.Error(w, spec.batchErr, http.StatusInternalServerError)
			return
		}
		if idx >= len(spec.batchResponses) {
			// Default: signal drained so the loop exits cleanly.
			_ = json.NewEncoder(w).Encode(llmReclassifyResponse{Remaining: 0})
			return
		}
		_ = json.NewEncoder(w).Encode(spec.batchResponses[idx])
	}))
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "test")
	return srv
}

func TestRunLLMReclassifyLoop_DryRunReportsRemaining(t *testing.T) {
	spec := &llmHandlerSpec{probeRemaining: 42}
	srv := newLLMTestServer(t, spec)
	defer srv.Close()

	w, read := captureStdout(t)
	err := runLLMReclassifyLoop("p", true, false, 10, w)
	if err != nil {
		t.Fatal(err)
	}
	got := read()
	if !strings.Contains(got, "would process 42") {
		t.Fatalf("missing remaining count: %s", got)
	}
	if spec.batchCalls.Load() != 0 {
		t.Fatalf("dry-run should not invoke batch endpoint: %d", spec.batchCalls.Load())
	}
}

func TestRunLLMReclassifyLoop_DryRunJSON(t *testing.T) {
	spec := &llmHandlerSpec{probeRemaining: 7}
	srv := newLLMTestServer(t, spec)
	defer srv.Close()

	w, read := captureStdout(t)
	if err := runLLMReclassifyLoop("p", true, true, 10, w); err != nil {
		t.Fatal(err)
	}
	got := read()
	var r llmReclassifyResponse
	if err := json.Unmarshal([]byte(got), &r); err != nil {
		t.Fatalf("not JSON: %s", got)
	}
	if r.Remaining != 7 {
		t.Fatalf("remaining: %d", r.Remaining)
	}
}

func TestRunLLMReclassifyLoop_NothingToDo(t *testing.T) {
	spec := &llmHandlerSpec{probeRemaining: 0}
	srv := newLLMTestServer(t, spec)
	defer srv.Close()

	w, read := captureStdout(t)
	if err := runLLMReclassifyLoop("p", false, false, 10, w); err != nil {
		t.Fatal(err)
	}
	got := read()
	if !strings.Contains(got, "no chunks left for the LLM") {
		t.Fatalf("missing empty-case message: %s", got)
	}
	if spec.batchCalls.Load() != 0 {
		t.Fatal("empty case must not POST")
	}
}

func TestRunLLMReclassifyLoop_DrainsAcrossBatches(t *testing.T) {
	spec := &llmHandlerSpec{
		probeRemaining: 20,
		batchResponses: []llmReclassifyResponse{
			{Processed: 10, Succeeded: 9, Skipped: 1, Remaining: 10},
			{Processed: 10, Succeeded: 8, Failed: 2, Remaining: 0},
		},
	}
	srv := newLLMTestServer(t, spec)
	defer srv.Close()

	w, read := captureStdout(t)
	if err := runLLMReclassifyLoop("p", false, false, 10, w); err != nil {
		t.Fatal(err)
	}
	got := read()
	if !strings.Contains(got, "17 chunks classified") {
		t.Fatalf("missing total summary (9+8=17): %s", got)
	}
	if spec.batchCalls.Load() != 2 {
		t.Fatalf("expected 2 batches, got %d", spec.batchCalls.Load())
	}
}

func TestRunLLMReclassifyLoop_DrainStalledBailsOut(t *testing.T) {
	// Queue never shrinks across two batches → loop must exit.
	spec := &llmHandlerSpec{
		probeRemaining: 5,
		batchResponses: []llmReclassifyResponse{
			{Processed: 5, Failed: 5, Remaining: 5},
			{Processed: 5, Failed: 5, Remaining: 5},
			{Processed: 5, Failed: 5, Remaining: 5},
		},
	}
	srv := newLLMTestServer(t, spec)
	defer srv.Close()

	w, read := captureStdout(t)
	if err := runLLMReclassifyLoop("p", false, false, 10, w); err != nil {
		t.Fatal(err)
	}
	got := read()
	if !strings.Contains(got, "stalled") {
		t.Fatalf("missing stall message: %s", got)
	}
	if spec.batchCalls.Load() > 4 {
		t.Fatalf("loop did not bail out: %d batches", spec.batchCalls.Load())
	}
}

func TestRunLLMReclassifyLoop_BatchSizeClamping(t *testing.T) {
	spec := &llmHandlerSpec{
		probeRemaining: 1,
		batchResponses: []llmReclassifyResponse{
			{Processed: 1, Succeeded: 1, Remaining: 0},
		},
	}
	srv := newLLMTestServer(t, spec)
	defer srv.Close()

	w, _ := captureStdout(t)
	if err := runLLMReclassifyLoop("p", false, false, 999, w); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(spec.lastQuery, "batch_size=50") {
		t.Fatalf("batch_size not clamped to 50: %s", spec.lastQuery)
	}
	if err := runLLMReclassifyLoop("p", false, false, -1, w); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(spec.lastQuery, "batch_size=1") {
		t.Fatalf("batch_size not clamped to 1: %s", spec.lastQuery)
	}
}

func TestRunLLMReclassifyLoop_ProbeError(t *testing.T) {
	spec := &llmHandlerSpec{probeErr: "down"}
	srv := newLLMTestServer(t, spec)
	defer srv.Close()
	w, _ := captureStdout(t)
	if err := runLLMReclassifyLoop("p", true, false, 10, w); err == nil {
		t.Fatal("want err")
	}
}

func TestRunLLMReclassifyLoop_BatchError(t *testing.T) {
	spec := &llmHandlerSpec{probeRemaining: 5, batchErr: "boom"}
	srv := newLLMTestServer(t, spec)
	defer srv.Close()
	w, _ := captureStdout(t)
	if err := runLLMReclassifyLoop("p", false, false, 10, w); err == nil {
		t.Fatal("want err")
	}
}

func TestRunLLMReclassifyLoop_JSONOutput(t *testing.T) {
	spec := &llmHandlerSpec{
		probeRemaining: 3,
		batchResponses: []llmReclassifyResponse{
			{Processed: 3, Succeeded: 2, Skipped: 1, Remaining: 0},
		},
	}
	srv := newLLMTestServer(t, spec)
	defer srv.Close()

	var buf bytes.Buffer
	w, read := captureStdoutBuffered(t, &buf)
	if err := runLLMReclassifyLoop("p", false, true, 10, w); err != nil {
		t.Fatal(err)
	}
	got := read()
	// Output includes the per-batch human line then a final JSON
	// summary — the JSON is on its own line at the end. Look for the
	// totals envelope by splitting on the last newline group.
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) == 0 {
		t.Fatal("no output")
	}
	final := lines[len(lines)-1]
	var r llmReclassifyResponse
	if err := json.Unmarshal([]byte(final), &r); err != nil {
		t.Fatalf("final line not JSON: %q\nfull: %s", final, got)
	}
	if r.Succeeded != 2 || r.Skipped != 1 {
		t.Fatalf("totals: %+v", r)
	}
}
