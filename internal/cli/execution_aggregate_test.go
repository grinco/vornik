package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestAggregateExecutionsByTask_SpecCase — the case from the
// finding spec: executions [(FAILED, t1), (COMPLETED, t1),
// (COMPLETED, t2)] aggregate to two rows: (t1, COMPLETED, 2
// attempts), (t2, COMPLETED, 1 attempt). The API returns
// newest-first, so the FAILED row LATER in time would be index 0
// — we model that by giving the COMPLETED a later startedAt to
// exercise the "latest wins" logic.
func TestAggregateExecutionsByTask_SpecCase(t *testing.T) {
	in := []executionResponse{
		{ExecutionID: "e1", TaskID: "t1", Status: "FAILED", WorkflowID: "wf", StartedAt: "2026-05-17T10:00:00Z"},
		{ExecutionID: "e2", TaskID: "t1", Status: "COMPLETED", WorkflowID: "wf", StartedAt: "2026-05-17T10:05:00Z"},
		{ExecutionID: "e3", TaskID: "t2", Status: "COMPLETED", WorkflowID: "wf", StartedAt: "2026-05-17T10:01:00Z"},
	}
	rows := aggregateExecutionsByTask(in)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	// t1 row: latest is the COMPLETED retry; attempts = 2.
	if rows[0].TaskID != "t1" {
		t.Errorf("rows[0].TaskID = %q, want t1", rows[0].TaskID)
	}
	if rows[0].LatestStatus != "COMPLETED" {
		t.Errorf("rows[0].LatestStatus = %q, want COMPLETED (the retry won)", rows[0].LatestStatus)
	}
	if rows[0].LatestExecID != "e2" {
		t.Errorf("rows[0].LatestExecID = %q, want e2", rows[0].LatestExecID)
	}
	if rows[0].Attempts != 2 {
		t.Errorf("rows[0].Attempts = %d, want 2", rows[0].Attempts)
	}
	// t2 row: single attempt, unchanged.
	if rows[1].TaskID != "t2" {
		t.Errorf("rows[1].TaskID = %q, want t2", rows[1].TaskID)
	}
	if rows[1].Attempts != 1 {
		t.Errorf("rows[1].Attempts = %d, want 1", rows[1].Attempts)
	}
}

// TestAggregateExecutionsByTask_Empty — no executions returns no
// rows; defensive against the API returning an empty list under a
// filter.
func TestAggregateExecutionsByTask_Empty(t *testing.T) {
	rows := aggregateExecutionsByTask(nil)
	if rows == nil {
		t.Error("want empty slice, got nil — caller may range over it")
	}
	if len(rows) != 0 {
		t.Errorf("rows = %d, want 0", len(rows))
	}
}

// TestAggregateExecutionsByTask_SingleAttemptUnchanged — one
// execution per task: latest = the only execution, attempts = 1.
// Exercises the simple-case branch.
func TestAggregateExecutionsByTask_SingleAttemptUnchanged(t *testing.T) {
	in := []executionResponse{
		{ExecutionID: "e1", TaskID: "t1", Status: "RUNNING", WorkflowID: "wf"},
	}
	rows := aggregateExecutionsByTask(in)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", rows[0].Attempts)
	}
	if rows[0].LatestStatus != "RUNNING" {
		t.Errorf("LatestStatus = %q, want RUNNING", rows[0].LatestStatus)
	}
}

// TestAggregateExecutionsByTask_MultipleTasksDeterministicOrder —
// the output order is first-appearance in the input. Repeated
// calls return the same order regardless of Go map iteration
// randomisation.
func TestAggregateExecutionsByTask_MultipleTasksDeterministicOrder(t *testing.T) {
	in := []executionResponse{
		{ExecutionID: "e1", TaskID: "alpha"},
		{ExecutionID: "e2", TaskID: "beta"},
		{ExecutionID: "e3", TaskID: "alpha"},
		{ExecutionID: "e4", TaskID: "gamma"},
		{ExecutionID: "e5", TaskID: "beta"},
	}
	// Run twice to confirm determinism.
	for i := 0; i < 2; i++ {
		rows := aggregateExecutionsByTask(in)
		if len(rows) != 3 {
			t.Fatalf("rows = %d, want 3", len(rows))
		}
		want := []string{"alpha", "beta", "gamma"}
		for j, w := range want {
			if rows[j].TaskID != w {
				t.Errorf("run %d rows[%d].TaskID = %q, want %q", i, j, rows[j].TaskID, w)
			}
		}
	}
}

// TestAggregateExecutionsByTask_MissingTimestampFallsBackToFirstSeen —
// when no execution carries a startedAt, the first-occurrence row
// is treated as the latest. Models the "in-flight execution, no
// timestamps yet" case.
func TestAggregateExecutionsByTask_MissingTimestampFallsBackToFirstSeen(t *testing.T) {
	in := []executionResponse{
		{ExecutionID: "first", TaskID: "t1", Status: "FAILED"},
		{ExecutionID: "second", TaskID: "t1", Status: "COMPLETED"},
	}
	rows := aggregateExecutionsByTask(in)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].Attempts != 2 {
		t.Errorf("Attempts = %d, want 2", rows[0].Attempts)
	}
	// Both timestamps empty → first-seen wins.
	if rows[0].LatestExecID != "first" {
		t.Errorf("LatestExecID = %q, want first (no timestamps, first-seen wins)", rows[0].LatestExecID)
	}
}

// TestAggregateExecutionsByTask_PopulatedBeatsEmptyTimestamp —
// a populated startedAt always wins over a missing one, so an
// in-progress retry visible in the API list (no startedAt yet)
// doesn't displace a real timestamped attempt.
func TestAggregateExecutionsByTask_PopulatedBeatsEmptyTimestamp(t *testing.T) {
	in := []executionResponse{
		{ExecutionID: "no-ts", TaskID: "t1", Status: "PENDING"},
		{ExecutionID: "has-ts", TaskID: "t1", Status: "COMPLETED", StartedAt: "2026-05-17T10:00:00Z"},
	}
	rows := aggregateExecutionsByTask(in)
	if rows[0].LatestExecID != "has-ts" {
		t.Errorf("LatestExecID = %q, want has-ts (timestamped row beats empty-timestamp row)", rows[0].LatestExecID)
	}
}

// withExecutionTestServer starts an httptest server that returns
// a fixed listExecutionsResponse, points the CLI client at it,
// and returns a cleanup. Pulled out so multiple integration-style
// tests can share the setup.
func withExecutionTestServer(t *testing.T, resp listExecutionsResponse) (string, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "test-key")
	return srv.URL, srv.Close
}

// captureRunOutput redirects os.Stdout for the duration of the call
// and returns whatever was written. The CLI uses fmt.Printf
// directly so this is the simplest way to assert on table output.
func captureRunOutput(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		buf, _ := io.ReadAll(r)
		done <- string(buf)
	}()
	fn()
	_ = w.Close()
	os.Stdout = orig
	return <-done
}

// TestRunExecutionList_AggregatesByDefault — end-to-end: the CLI
// fetches the list, aggregates, and the rendered table carries
// the attempts-count cue (↻) when a task has multiple attempts.
// Validates the integration of aggregateExecutionsByTask into the
// command's render path.
func TestRunExecutionList_AggregatesByDefault(t *testing.T) {
	_, cleanup := withExecutionTestServer(t, listExecutionsResponse{
		Executions: []executionResponse{
			{ExecutionID: "e2", TaskID: "t1", Status: "COMPLETED", WorkflowID: "wf", StartedAt: "2026-05-17T10:05:00Z"},
			{ExecutionID: "e1", TaskID: "t1", Status: "FAILED", WorkflowID: "wf", StartedAt: "2026-05-17T10:00:00Z"},
		},
		Total: 2,
	})
	defer cleanup()

	executionListProject = "p1"
	executionListTask = ""
	executionListStatus = ""
	executionListJSON = false
	executionListAll = false

	out := captureRunOutput(t, func() {
		if err := runExecutionList(nil, nil); err != nil {
			t.Fatalf("runExecutionList: %v", err)
		}
	})

	if !strings.Contains(out, "TASK ID") {
		t.Errorf("aggregated header missing from output:\n%s", out)
	}
	if !strings.Contains(out, "↻") {
		t.Errorf("multi-attempt cue missing — operator can't tell retries happened:\n%s", out)
	}
	if !strings.Contains(out, "use --all") {
		t.Errorf("footer must hint at --all for verbose view:\n%s", out)
	}
}

// TestRunExecutionList_AllFlagShowsEveryAttempt — --all opts back
// into the verbose per-execution table; the footer is the legacy
// "Total: N" line.
func TestRunExecutionList_AllFlagShowsEveryAttempt(t *testing.T) {
	_, cleanup := withExecutionTestServer(t, listExecutionsResponse{
		Executions: []executionResponse{
			{ExecutionID: "e1", TaskID: "t1", Status: "FAILED", WorkflowID: "wf"},
			{ExecutionID: "e2", TaskID: "t1", Status: "COMPLETED", WorkflowID: "wf"},
		},
		Total: 2,
	})
	defer cleanup()

	executionListProject = "p1"
	executionListTask = ""
	executionListStatus = ""
	executionListJSON = false
	executionListAll = true
	defer func() { executionListAll = false }()

	out := captureRunOutput(t, func() {
		if err := runExecutionList(nil, nil); err != nil {
			t.Fatalf("runExecutionList: %v", err)
		}
	})

	if !strings.Contains(out, "EXECUTION ID") {
		t.Errorf("--all should render the per-execution header, got:\n%s", out)
	}
	if !strings.Contains(out, "e1") || !strings.Contains(out, "e2") {
		t.Errorf("--all must show every execution row:\n%s", out)
	}
	if !strings.Contains(out, "Total: 2") {
		t.Errorf("--all footer should be the legacy Total: line:\n%s", out)
	}
}

// TestRunExecutionList_TaskFilterSkipsAggregation — --task scoping
// implies the operator wants every attempt visible; aggregation
// is suppressed even without --all.
func TestRunExecutionList_TaskFilterSkipsAggregation(t *testing.T) {
	_, cleanup := withExecutionTestServer(t, listExecutionsResponse{
		Executions: []executionResponse{
			{ExecutionID: "e1", TaskID: "t1", Status: "FAILED", WorkflowID: "wf"},
			{ExecutionID: "e2", TaskID: "t1", Status: "COMPLETED", WorkflowID: "wf"},
		},
		Total: 2,
	})
	defer cleanup()

	executionListProject = "p1"
	executionListTask = "t1"
	executionListStatus = ""
	executionListJSON = false
	executionListAll = false
	defer func() { executionListTask = "" }()

	out := captureRunOutput(t, func() {
		if err := runExecutionList(nil, nil); err != nil {
			t.Fatalf("runExecutionList: %v", err)
		}
	})

	if !strings.Contains(out, "EXECUTION ID") {
		t.Errorf("--task filter should render the verbose table, got:\n%s", out)
	}
	if strings.Contains(out, "↻") {
		t.Errorf("--task filter should not aggregate, but the multi-attempt cue rendered:\n%s", out)
	}
}

// TestRunExecutionList_JSONFlagSkipsRendering — --json returns the
// raw API payload unchanged. Aggregation is a display-layer
// concern only.
func TestRunExecutionList_JSONFlagSkipsRendering(t *testing.T) {
	_, cleanup := withExecutionTestServer(t, listExecutionsResponse{
		Executions: []executionResponse{
			{ExecutionID: "e1", TaskID: "t1", Status: "FAILED"},
			{ExecutionID: "e2", TaskID: "t1", Status: "COMPLETED"},
		},
		Total: 2,
	})
	defer cleanup()

	executionListProject = "p1"
	executionListTask = ""
	executionListStatus = ""
	executionListJSON = true
	defer func() { executionListJSON = false }()

	out := captureRunOutput(t, func() {
		if err := runExecutionList(nil, nil); err != nil {
			t.Fatalf("runExecutionList: %v", err)
		}
	})

	var got listExecutionsResponse
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json output not valid JSON: %v\n%s", err, out)
	}
	if len(got.Executions) != 2 {
		t.Errorf("JSON should preserve all executions, got %d", len(got.Executions))
	}
}

// TestAggregateExecutionsByTask_PreservesWorkflowAndDurationFromLatest —
// the latest execution's workflow + duration are surfaced, not
// the first one's. Operators looking at the row see the data
// associated with the LATEST attempt.
func TestAggregateExecutionsByTask_PreservesWorkflowAndDurationFromLatest(t *testing.T) {
	in := []executionResponse{
		{ExecutionID: "old", TaskID: "t1", Status: "FAILED", WorkflowID: "old-wf", Duration: "1m", StartedAt: "2026-05-17T10:00:00Z"},
		{ExecutionID: "new", TaskID: "t1", Status: "COMPLETED", WorkflowID: "new-wf", Duration: "2m", StartedAt: "2026-05-17T10:10:00Z"},
	}
	rows := aggregateExecutionsByTask(in)
	if rows[0].LatestWorkflow != "new-wf" {
		t.Errorf("LatestWorkflow = %q, want new-wf", rows[0].LatestWorkflow)
	}
	if rows[0].LatestDuration != "2m" {
		t.Errorf("LatestDuration = %q, want 2m", rows[0].LatestDuration)
	}
}
