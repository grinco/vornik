package sqlite_test

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// The residual failure bucket has to be diagnosable, and until 2026-08-26 it
// was not: `container_non_zero_exit` was 3,027 of 5,791 classified step
// failures, and the container's exit code appeared in error_detail for
// ELEVEN of them (0.4%). The code only ever reached error_detail through the
// `container exited with code %d` fallback, which fires when result.json did
// NOT say FAILED — the uncommon path. execution_step_outcomes had 23 columns
// and none held an exit code.
//
// A free-text field cannot be grouped. `GROUP BY container_exit_code` over
// the residual is how the next pattern gets found — 137 (OOM-killed) reading
// differently from 125 (podman refused to start) is the whole point — so this
// is a typed nullable column, not more prose.
//
// Design: https://docs.vornik.io (D2)
func TestExecutionStepOutcome_ContainerExitCodeRoundTrips(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := sqlite.NewExecutionStepOutcomeRepository(db.DB)

	code := 137
	if err := repo.Record(ctx, &persistence.ExecutionStepOutcome{
		ID:                "oc-exit",
		ProjectID:         "p",
		TaskID:            "t",
		ExecutionID:       "e1",
		StepID:            "s1",
		Role:              "worker",
		Model:             "m",
		Outcome:           "failed",
		ErrorClass:        "container_non_zero_exit",
		ErrorDetail:       "container exited with code 137",
		ContainerExitCode: &code,
		RecordedAt:        time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	got, err := repo.List(ctx, persistence.ExecutionStepOutcomeFilter{ExecutionIDs: []string{"e1"}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 row, got %d", len(got))
	}
	if got[0].ContainerExitCode == nil {
		t.Fatal("ContainerExitCode came back nil — the column did not round-trip")
	}
	if *got[0].ContainerExitCode != 137 {
		t.Fatalf("want exit code 137, got %d", *got[0].ContainerExitCode)
	}
}

// NULL is a real and common value: it means "this step did not fail in a
// container", not "it exited 0". Conflating the two would make every
// non-container step look like a clean container exit.
func TestExecutionStepOutcome_ContainerExitCodeNilStaysNil(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := sqlite.NewExecutionStepOutcomeRepository(db.DB)

	if err := repo.Record(ctx, &persistence.ExecutionStepOutcome{
		ID: "oc-nil", ProjectID: "p", TaskID: "t", ExecutionID: "e2",
		StepID: "s1", Role: "worker", Model: "m", Outcome: "ok",
		RecordedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	got, err := repo.List(ctx, persistence.ExecutionStepOutcomeFilter{ExecutionIDs: []string{"e2"}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 row, got %d", len(got))
	}
	if got[0].ContainerExitCode != nil {
		t.Fatalf("a step that never ran a container must keep a NULL exit code, got %d", *got[0].ContainerExitCode)
	}
}

// A zero exit code must survive as 0, not collapse to NULL. A container that
// exited 0 but failed the step (a verifier rejection, say) is a genuinely
// different row from one that never ran a container, and *int is the only
// shape that keeps them apart.
func TestExecutionStepOutcome_ContainerExitCodeZeroIsNotNull(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := sqlite.NewExecutionStepOutcomeRepository(db.DB)

	zero := 0
	if err := repo.Record(ctx, &persistence.ExecutionStepOutcome{
		ID: "oc-zero", ProjectID: "p", TaskID: "t", ExecutionID: "e3",
		StepID: "s1", Role: "worker", Model: "m", Outcome: "failed",
		ContainerExitCode: &zero,
		RecordedAt:        time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	got, err := repo.List(ctx, persistence.ExecutionStepOutcomeFilter{ExecutionIDs: []string{"e3"}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 row, got %d", len(got))
	}
	if got[0].ContainerExitCode == nil {
		t.Fatal("exit code 0 collapsed to NULL — 'exited 0' and 'no container' are different facts")
	}
	if *got[0].ContainerExitCode != 0 {
		t.Fatalf("want 0, got %d", *got[0].ContainerExitCode)
	}
}
