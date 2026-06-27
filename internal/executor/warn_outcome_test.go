package executor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/stepoutcome"
)

func TestRecordWarnViolationsOutcome_PersistsRow(t *testing.T) {
	repo := newStubStepOutcomeRepo()
	e := &Executor{logger: zerolog.Nop(), outcomeRepo: repo}
	task := &persistence.Task{ID: "task-1", ProjectID: "p1"}
	execution := &persistence.Execution{ID: "exec-1"}

	warnings := []string{
		`[warn] verifier "tripadvisor_blocks" (no_status_429_in_audit): 1/30 blocked`,
		`[warn] verifier "long_research" (artifact_min_entries): 4 items < 5`,
	}
	e.recordWarnViolationsOutcome(context.Background(), task, execution, "research", warnings)

	if len(repo.rows) != 1 {
		t.Fatalf("expected 1 outcome row, got %d", len(repo.rows))
	}
	row := repo.rows[0]
	if row.Outcome != string(stepoutcome.VerifierWarn) {
		t.Fatalf("outcome: %q", row.Outcome)
	}
	if row.ErrorClass != "verifier_warn" {
		t.Fatalf("class: %q", row.ErrorClass)
	}
	for _, want := range []string{"tripadvisor_blocks", "long_research", "5", "30"} {
		if !strings.Contains(row.ErrorDetail, want) {
			t.Errorf("detail missing %q: %s", want, row.ErrorDetail)
		}
	}
	if row.FinalizedAt == nil {
		t.Fatalf("FinalizedAt must be set so dashboards filter on finalised rows")
	}
	if row.TaskID != "task-1" || row.ExecutionID != "exec-1" || row.StepID != "research" {
		t.Fatalf("attribution: %+v", row)
	}
}

func TestRecordWarnViolationsOutcome_NilGuards(t *testing.T) {
	// Nil receiver / nil repo / nil task / nil execution / empty warnings:
	// every branch is a no-op without panicking.
	var nilE *Executor
	nilE.recordWarnViolationsOutcome(context.Background(), nil, nil, "", nil)

	repo := newStubStepOutcomeRepo()
	e := &Executor{logger: zerolog.Nop(), outcomeRepo: repo}
	e.recordWarnViolationsOutcome(context.Background(), nil, nil, "", []string{"x"})
	if len(repo.rows) != 0 {
		t.Fatal("nil task/exec must not write")
	}

	task := &persistence.Task{ID: "t", ProjectID: "p"}
	exec := &persistence.Execution{ID: "e"}
	e.recordWarnViolationsOutcome(context.Background(), task, exec, "s", nil)
	if len(repo.rows) != 0 {
		t.Fatal("empty warnings must not write")
	}

	// No outcomeRepo wired → no-op.
	bare := &Executor{logger: zerolog.Nop()}
	bare.recordWarnViolationsOutcome(context.Background(), task, exec, "s", []string{"x"})
}

func TestRecordWarnViolationsOutcome_RepoErrorIsLoggedNotPropagated(t *testing.T) {
	repo := newStubStepOutcomeRepo()
	repo.recordErr = errors.New("disk full")
	e := &Executor{logger: zerolog.Nop(), outcomeRepo: repo}
	task := &persistence.Task{ID: "t", ProjectID: "p"}
	exec := &persistence.Execution{ID: "e"}
	// Must not panic; must not return error (signature is void).
	e.recordWarnViolationsOutcome(context.Background(), task, exec, "s", []string{"w"})
}

func TestRecordWarnViolationsOutcome_DetailTruncated(t *testing.T) {
	repo := newStubStepOutcomeRepo()
	e := &Executor{logger: zerolog.Nop(), outcomeRepo: repo}
	task := &persistence.Task{ID: "t", ProjectID: "p"}
	exec := &persistence.Execution{ID: "e"}
	big := strings.Repeat("x", 5000)
	e.recordWarnViolationsOutcome(context.Background(), task, exec, "s", []string{big})
	if len(repo.rows) != 1 {
		t.Fatal("row missing")
	}
	if len(repo.rows[0].ErrorDetail) > 2050 {
		t.Fatalf("detail not truncated: %d bytes", len(repo.rows[0].ErrorDetail))
	}
}
