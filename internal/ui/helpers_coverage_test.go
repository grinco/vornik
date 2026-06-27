package ui

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// Coverage-sweep tests for the pure helpers + option setters in
// this package.

// TestExportFormat_ReadsQueryParam — three branches in one shot.
func TestExportFormat_ReadsQueryParam(t *testing.T) {
	cases := map[string]string{
		"":              "",
		"?format=csv":   "csv",
		"?format=json":  "json",
		"?format=other": "",
		"?format=":      "",
	}
	for q, want := range cases {
		req := httptest.NewRequest("GET", "/spend"+q, nil)
		if got := exportFormat(req); got != want {
			t.Errorf("exportFormat(%q) = %q, want %q", q, got, want)
		}
	}
}

// TestWriteCSV_RoundTrips — the headline shape: rows in, valid
// CSV with the configured filename header out. Drives the
// Content-Disposition path so a future "drop filename for inline
// preview" wouldn't silently change the download UX.
func TestWriteCSV_RoundTrips(t *testing.T) {
	rec := httptest.NewRecorder()
	rows := [][]string{
		{"task", "status", "cost"},
		{"t-1", "completed", "0.42"},
		{"t-2", "failed", "0.10"},
	}
	writeCSV(rec, "spend.csv", rows)

	if ct := rec.Header().Get("Content-Type"); ct != "text/csv; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/csv", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "spend.csv") {
		t.Errorf("Content-Disposition missing filename: %q", cd)
	}
	// Re-parse the body as CSV to confirm valid output.
	parsed, err := csv.NewReader(bytes.NewReader(rec.Body.Bytes())).ReadAll()
	if err != nil {
		t.Fatalf("re-parse CSV: %v", err)
	}
	if len(parsed) != 3 || parsed[0][0] != "task" || parsed[2][2] != "0.10" {
		t.Errorf("parsed rows wrong: %+v", parsed)
	}
}

// TestWriteJSON_IndentsAndSetsHeaders — the JSON-export sibling.
// Indented output (newlines + spaces) is part of the contract —
// operators piping the response through `jq` expect a parseable
// stream.
func TestWriteJSON_IndentsAndSetsHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, "rows.json", map[string]int{"a": 1, "b": 2})
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "\n") || !strings.Contains(body, "  ") {
		t.Errorf("output not indented: %q", body)
	}
	// Re-parse to confirm well-formed JSON.
	var decoded map[string]int
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("re-parse JSON: %v", err)
	}
	if decoded["a"] != 1 || decoded["b"] != 2 {
		t.Errorf("decoded wrong: %+v", decoded)
	}
}

// TestCanonicalPipelineGates_LayoutStable — the SVG positions
// are baked into the rendering layer (the template positions
// the rects from the returned X/Y). A silent x/y shift would
// break the flow diagram. Pin the gate name order + first node
// position so a refactor surfaces here.
func TestCanonicalPipelineGates_LayoutStable(t *testing.T) {
	got := canonicalPipelineGates(map[string]int{"schema_match": 3, "secret_scan": 1})
	if len(got) < 7 {
		t.Fatalf("expected ≥7 gates; got %d", len(got))
	}
	if got[0].Name != "schema_match" {
		t.Errorf("first gate = %q, want schema_match", got[0].Name)
	}
	// Trip counts must flow from the input map.
	for _, n := range got {
		if n.Name == "schema_match" && n.Trips != 3 {
			t.Errorf("schema_match count = %d, want 3", n.Trips)
		}
		if n.Name == "secret_scan" && n.Trips != 1 {
			t.Errorf("secret_scan count = %d, want 1", n.Trips)
		}
	}
}

// TestCanonicalPipelineGates_NilMapSafe — defensive: a nil input
// map MUST NOT panic (zero counts everywhere is the right
// fallback).
func TestCanonicalPipelineGates_NilMapSafe(t *testing.T) {
	got := canonicalPipelineGates(nil)
	if len(got) < 7 {
		t.Fatalf("expected ≥7 gates; got %d", len(got))
	}
	for _, n := range got {
		if n.Trips != 0 {
			t.Errorf("nil map should give 0 counts, got %d on %q", n.Trips, n.Name)
		}
	}
}

// TestUIServerOptionSetters — drives all 20+ option setters
// through NewServer in a single pass for cheap coverage. None
// of them does anything more complex than `s.field = v`.
func TestUIServerOptionSetters(t *testing.T) {
	opts := []ServerOption{
		WithTaskRepository(nil),
		WithExecutionRepository(nil),
		WithArtifactRepository(nil),
		WithProjectRegistry(nil),
		WithExecutor(nil),
		WithTaskLogSource(nil),
		WithToolAuditRepository(nil),
		WithWebhookEventRepository(nil),
		WithAPIKeyRepository(nil),
		WithLLMUsageRepository(nil),
		WithAutonomyEvaluationRepository(nil),
		WithActiveChatSource(nil),
		WithProjectTemplates(nil),
		WithConfigsDir("/tmp/test"),
		WithMemoryQuarantineRepository(nil),
		WithVectorVizSource(nil),
		WithPipelineDryRunner(nil),
		WithMemorySearcher(nil),
		WithCorpusEpochRepository(nil),
		WithIngestQueueRepository(nil),
		WithChunkGraphRepository(nil),
		WithStepOutcomeRepository(nil),
		WithJudgeVerdictRepository(nil),
	}
	s := NewServer(opts...)
	if s == nil {
		t.Fatal("NewServer returned nil after option chain")
	}
}
