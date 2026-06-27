package memetic

// Regression tests for architect decision logging. The 2026-06-06
// "confidence 0.00 < 0.60" incident was undiagnosable for days because
// this package logged nothing — a rejection looked identical whether
// the model omitted the field, emitted null, or honestly scored low.
// These pin that every propose outcome now leaves a structured trail.

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/workflowtelemetry"
)

func newArchitectWithLog(t *testing.T, p *seqProvider, evidence []string, log zerolog.Logger) *Architect {
	t.Helper()
	lookup := &stubExecLookup{validIDs: map[string]bool{}}
	for _, id := range evidence {
		lookup.validIDs[id] = true
	}
	return New(
		p,
		&stubTelemetry{rollup: &workflowtelemetry.Rollup{WorkflowID: "simple-workflow", RunCount: 9}},
		&stubWorkflowSource{yaml: []byte(fixtureWorkflowYAML)},
		lookup,
		&stubProposalSink{},
		DefaultConfig(),
		WithLogger(log),
	)
}

func logRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line not JSON: %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func hasMsg(records []map[string]any, msg string) map[string]any {
	for _, m := range records {
		if m["message"] == msg {
			return m
		}
	}
	return nil
}

// The headline regression: a below-floor confidence logs the
// diagnostic line carrying the actual confidence + threshold, so an
// operator can tell an honest low score from a malformed reply.
func TestArchitectLog_LowConfidenceEmitsDiagnosticLine(t *testing.T) {
	var buf bytes.Buffer
	log := zerolog.New(&buf).Level(zerolog.InfoLevel)
	evidence := []string{"e1", "e2", "e3"}
	low := buildOutput("simple-workflow", fixtureWorkflowYAML, "meh", evidence, 0.0)
	a := newArchitectWithLog(t, &seqProvider{contents: []string{low}}, evidence, log)

	if _, err := a.Propose(context.Background(), "simple-workflow"); err == nil {
		t.Fatal("expected ErrLowConfidence")
	}
	rec := hasMsg(logRecords(t, &buf), "memetic: proposal rejected — architect confidence below threshold")
	if rec == nil {
		t.Fatalf("missing low-confidence diagnostic line; got %v", buf.String())
	}
	if rec["confidence"].(float64) != 0 {
		t.Errorf("confidence field = %v, want 0", rec["confidence"])
	}
	if rec["threshold"].(float64) != 0.6 {
		t.Errorf("threshold field = %v, want 0.6", rec["threshold"])
	}
}

// A successful propose logs the created proposal — proof the architect
// queried the LLM and the proposal landed.
func TestArchitectLog_SuccessLogsProposalCreated(t *testing.T) {
	var buf bytes.Buffer
	log := zerolog.New(&buf).Level(zerolog.InfoLevel)
	evidence := []string{"e1", "e2", "e3"}
	good := buildOutput("simple-workflow", fixtureWorkflowYAML, "fix it", evidence, 0.9)
	a := newArchitectWithLog(t, &seqProvider{contents: []string{good}}, evidence, log)

	got, err := a.Propose(context.Background(), "simple-workflow")
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	rec := hasMsg(logRecords(t, &buf), "memetic: architect proposal created")
	if rec == nil {
		t.Fatalf("missing proposal-created line; got %v", buf.String())
	}
	if rec["proposal_id"] != got.ID {
		t.Errorf("logged proposal_id = %v, want %v", rec["proposal_id"], got.ID)
	}
}

// A malformed reply logs the corrective-retry decision.
func TestArchitectLog_MalformedLogsRetry(t *testing.T) {
	var buf bytes.Buffer
	log := zerolog.New(&buf).Level(zerolog.InfoLevel)
	a := newArchitectWithLog(t, &seqProvider{contents: []string{"not json", "still not"}}, nil, log)

	_, _ = a.Propose(context.Background(), "simple-workflow")
	if hasMsg(logRecords(t, &buf), "memetic: architect output malformed — issuing one corrective retry") == nil {
		t.Fatalf("missing corrective-retry line; got %v", buf.String())
	}
}
