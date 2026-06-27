package executor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence"
)

// TestPriorShapeFailureCount_CountsSchemaAndParseOnly — the loop
// guard counts schema_violation and parse_error outcomes (the two
// flavors of shape failure) and ignores everything else. Pre-cap
// noise like "ok" and "timeout" must not increment the count.
func TestPriorShapeFailureCount_CountsSchemaAndParseOnly(t *testing.T) {
	repo := newStubStepOutcomeRepo()
	execID := "exec_loop_count"

	// Mix of outcomes for the same execution. Only schema_violation
	// and parse_error should be counted.
	for _, outcome := range []string{
		"schema_violation",
		"schema_violation",
		"parse_error",
		"ok",
		"timeout",
		"refused",
		"degenerate_loop",
	} {
		_ = repo.Record(context.Background(), &persistence.ExecutionStepOutcome{
			ExecutionID: execID,
			Outcome:     outcome,
		})
	}

	e := &Executor{outcomeRepo: repo, logger: zerolog.Nop()}
	got := e.priorShapeFailureCount(context.Background(), execID)
	want := 3 // 2 schema_violation + 1 parse_error
	if got != want {
		t.Errorf("priorShapeFailureCount = %d, want %d (only schema_violation + parse_error)", got, want)
	}
}

// TestPriorShapeFailureCount_SafeOnNilRepo — defensive: an Executor
// constructed without an outcomeRepo (legacy / partial bring-up)
// must return 0, not panic, and the caller's shape retry path
// proceeds as if the watchdog isn't wired. This preserves
// backwards-compatibility for tests and minimal Executor builds.
func TestPriorShapeFailureCount_SafeOnNilRepo(t *testing.T) {
	e := &Executor{outcomeRepo: nil, logger: zerolog.Nop()}
	got := e.priorShapeFailureCount(context.Background(), "any-exec")
	if got != 0 {
		t.Errorf("nil-repo count = %d, want 0", got)
	}
}

// TestPriorShapeFailureCount_EmptyExecutionID — the guard is also
// resilient to an empty execution ID (e.g. early-failure paths
// where the execution row hasn't been created yet). Returns 0
// without querying the DB.
func TestPriorShapeFailureCount_EmptyExecutionID(t *testing.T) {
	repo := newStubStepOutcomeRepo()
	e := &Executor{outcomeRepo: repo, logger: zerolog.Nop()}
	if got := e.priorShapeFailureCount(context.Background(), ""); got != 0 {
		t.Errorf("empty execID count = %d, want 0", got)
	}
}

// TestFailureClassifier_RoutesLoopCapToInvalidOutputLoop — the
// shape-retry-loop escalation must produce a typed
// INVALID_OUTPUT_LOOP class so dashboards can distinguish "this
// task hit one bad shape" from "this task is stuck in a model-
// schema loop the watchdog killed". Match comes BEFORE the
// generic schema-violation branch since the loop-cap message
// contains "schema violation" too.
func TestFailureClassifier_RoutesLoopCapToInvalidOutputLoop(t *testing.T) {
	err := errors.New(`shape retry loop cap hit (3 prior shape failures): schema violation: role "lead" result.json is missing required keys: [plan]`)
	got := ClassifyExecutionFailure(err, err.Error())
	if got != persistence.TaskFailureClassInvalidOutputLoop {
		t.Errorf("class for loop-cap error = %q, want %q", got, persistence.TaskFailureClassInvalidOutputLoop)
	}
	if !strings.Contains(got, "LOOP") {
		t.Errorf("class label must end in LOOP for grep'ability, got %q", got)
	}
}

// TestFailureClassifier_PreservesInvalidOutputForOneAttempt — the
// per-attempt schema-violation class is distinct from the loop-cap
// escalation. A single bad attempt must remain INVALID_OUTPUT so
// existing dashboards don't suddenly start showing every
// schema-violation as a loop event.
func TestFailureClassifier_PreservesInvalidOutputForOneAttempt(t *testing.T) {
	err := errors.New(`schema violation: role "lead" result.json is missing required keys: [plan]`)
	got := ClassifyExecutionFailure(err, err.Error())
	if got != persistence.TaskFailureClassInvalidOutput {
		t.Errorf("class for single schema violation = %q, want %q (loop class only on cap-hit)", got, persistence.TaskFailureClassInvalidOutput)
	}
}
