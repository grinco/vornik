package postmortem

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

func TestLooksLikeVerifierViolation(t *testing.T) {
	cases := map[string]bool{
		`verifier "scan_min_entries" (artifact_min_entries): no artifact matched pattern "scan-*.md"`: true,
		`[warn] verifier "no_rate_limit_blocks" (no_status_429_in_audit): 429 detected`:               true,
		"some random error message":          false,
		"":                                   false,
		"verifier with no parens":            false,
		"verifier (parens) but no colon-end": false,
		`verifier "x" (t): payload`:          true,
	}
	for in, want := range cases {
		if got := looksLikeVerifierViolation(in); got != want {
			t.Errorf("looksLikeVerifierViolation(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestCollectVerifierViolations_DedupesAndSplits(t *testing.T) {
	rows := []*persistence.ExecutionStepOutcome{
		nil,
		{
			StepID:      "scan",
			Outcome:     "verifier_violation",
			ErrorDetail: `verifier "no_rate_limit_blocks" (no_status_429_in_audit): 429 detected; verifier "scan_min_entries" (artifact_min_entries): no artifact matched pattern "scan-*.md"`,
		},
		{
			StepID:  "scan",
			Outcome: "verifier_violation",
			// Duplicate of the second violation above → must dedup.
			ErrorDetail: `verifier "scan_min_entries" (artifact_min_entries): no artifact matched pattern "scan-*.md"`,
		},
		{
			StepID:      "scan",
			Outcome:     "ok",
			ErrorDetail: "random non-verifier error",
		},
	}
	got := collectVerifierViolations(rows)
	if len(got) != 2 {
		t.Fatalf("expected 2 unique violations, got %d: %v", len(got), got)
	}
	// Newest-first iteration means the rate_limit row comes from the
	// row with the semicolon-joined detail, which we iterate last in
	// reverse — let's just assert both are present.
	seen := map[string]bool{}
	for _, line := range got {
		seen[line] = true
	}
	if !seen[`verifier "no_rate_limit_blocks" (no_status_429_in_audit): 429 detected`] {
		t.Errorf("missing rate-limit violation: %v", got)
	}
	if !seen[`verifier "scan_min_entries" (artifact_min_entries): no artifact matched pattern "scan-*.md"`] {
		t.Errorf("missing scan_min_entries violation: %v", got)
	}
}

// ---- gatherEvidence integration: covers the audit + log-tail
// branches the existing test suite skipped, plus the new
// verifier-violations section. Single integration test covers the
// remaining gatherEvidence gap. ----

type fakeExecutions struct {
	persistence.ExecutionRepository
	exec *persistence.Execution
	err  error
}

func (f *fakeExecutions) List(_ context.Context, _ persistence.ExecutionFilter) ([]*persistence.Execution, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.exec == nil {
		return nil, nil
	}
	return []*persistence.Execution{f.exec}, nil
}

type fakeOutcomes struct {
	persistence.ExecutionStepOutcomeRepository
	rows []*persistence.ExecutionStepOutcome
	err  error
}

func (f *fakeOutcomes) List(_ context.Context, _ persistence.ExecutionStepOutcomeFilter) ([]*persistence.ExecutionStepOutcome, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

type fakeAudits struct {
	persistence.ToolAuditRepository
	rows []*persistence.ToolAuditEntry
	err  error
}

func (f *fakeAudits) List(context.Context, persistence.ToolAuditFilter) ([]*persistence.ToolAuditEntry, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

type fakeLogs struct {
	out string
	err error
}

func (f *fakeLogs) TaskLogs(_ context.Context, _ string, _ int) (string, error) {
	return f.out, f.err
}

func TestGatherEvidence_AssemblesAllSections(t *testing.T) {
	errMsg := "phase-2 verifier(s) failed: verifier \"scan_min_entries\" (artifact_min_entries): no artifact matched pattern \"scan-*.md\"; expected at least one with ≥1 items"
	task := &persistence.Task{
		ID:          "task-1",
		ProjectID:   "janka",
		Status:      persistence.TaskStatusFailed,
		Attempt:     1,
		MaxAttempts: 3,
		LastError:   &errMsg,
		Payload:     []byte(`{"taskType":"research","context":{"prompt":"Scan portal and write the top 10 to scan-*.md"}}`),
	}
	execTime := time.Now()
	errBody := "execution exploded"
	exp := &Explainer{
		Executions: &fakeExecutions{exec: &persistence.Execution{
			ID:             "exec-1",
			Status:         persistence.ExecutionStatusFailed,
			ErrorMessage:   &errBody,
			CompletedSteps: []string{"route", "scan"},
			StartedAt:      &execTime,
		}},
		Outcomes: &fakeOutcomes{rows: []*persistence.ExecutionStepOutcome{
			{StepID: "scan", Outcome: "verifier_violation", ErrorDetail: `verifier "scan_min_entries" (artifact_min_entries): no artifact matched pattern "scan-*.md"`},
		}},
		Audits: &fakeAudits{rows: []*persistence.ToolAuditEntry{
			{ToolName: "fetch", ToolInput: "u", ToolOutput: "429"},
		}},
		Logs: &fakeLogs{out: "stderr line one\nstderr line two\n"},
	}
	got, execID := exp.gatherEvidence(context.Background(), task)
	if execID != "exec-1" {
		t.Fatalf("execID: %q", execID)
	}
	for _, want := range []string{
		"task-1",
		"VERIFIER VIOLATIONS",
		`pattern "scan-*.md"`,
		"Step outcomes",
		"Tool audit",
		"fetch",
		"429",
		"Container log tail",
		"stderr line one",
		"Original prompt:",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in evidence:\n%s", want, got)
		}
	}
}

func TestGatherEvidence_HandlesEmptySources(t *testing.T) {
	task := &persistence.Task{ID: "t-empty", ProjectID: "p"}
	exp := &Explainer{
		Executions: &fakeExecutions{},                               // no execution
		Outcomes:   &fakeOutcomes{err: errors.New("outcomes down")}, // error swallowed
		Audits:     &fakeAudits{err: errors.New("audits down")},     // error swallowed
		Logs:       &fakeLogs{err: errors.New("logs down")},         // error swallowed
		Logger:     zerolog.Nop(),
	}
	got, execID := exp.gatherEvidence(context.Background(), task)
	if execID != "" {
		t.Fatalf("expected empty execID, got %q", execID)
	}
	if !strings.Contains(got, "t-empty") {
		t.Fatalf("at least task ID should appear: %s", got)
	}
}

func TestGatherEvidence_EvidenceCapTruncates(t *testing.T) {
	// Tiny cap forces the truncation path.
	task := &persistence.Task{
		ID:        "t",
		ProjectID: "p",
		Payload:   []byte(`{"taskType":"x","context":{"prompt":"` + strings.Repeat("A", 2000) + `"}}`),
	}
	exp := &Explainer{
		MaxEvidenceBytes: 200,
		Logger:           zerolog.Nop(),
	}
	got, _ := exp.gatherEvidence(context.Background(), task)
	if !strings.Contains(got, "evidence truncated") {
		t.Fatalf("expected truncation marker, got: %s", got)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 100); got != "short" {
		t.Fatalf("under: %q", got)
	}
	if got := truncate("alphabet soup", 5); got != "alpha…" {
		t.Fatalf("over: %q", got)
	}
}

func TestCollectVerifierViolations_EmptyInput(t *testing.T) {
	if got := collectVerifierViolations(nil); got != nil {
		t.Fatalf("nil input: %v", got)
	}
	if got := collectVerifierViolations([]*persistence.ExecutionStepOutcome{}); got != nil {
		t.Fatalf("empty input: %v", got)
	}
	// All-nil rows.
	rows := []*persistence.ExecutionStepOutcome{nil, nil}
	if got := collectVerifierViolations(rows); got != nil {
		t.Fatalf("all-nil: %v", got)
	}
	// Rows with empty ErrorDetail.
	rows = []*persistence.ExecutionStepOutcome{{ErrorDetail: ""}}
	if got := collectVerifierViolations(rows); got != nil {
		t.Fatalf("empty detail: %v", got)
	}
}
