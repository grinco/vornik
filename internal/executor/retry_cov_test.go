package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"vornik.io/vornik/internal/persistence"
)

// retryCov_listErrOutcomeRepo wraps the package stub and forces List
// to error, so priorShapeFailureCount's best-effort "return 0 on DB
// error" arm is exercised. Only List is overridden; everything else
// delegates to the embedded stub.
type retryCov_listErrOutcomeRepo struct {
	*stubStepOutcomeRepo
}

func (r *retryCov_listErrOutcomeRepo) List(_ context.Context, _ persistence.ExecutionStepOutcomeFilter) ([]*persistence.ExecutionStepOutcome, error) {
	return nil, errors.New("outcomes table unreachable")
}

// TestRetryCov_PriorShapeFailureCountGuards — returns 0 for a nil
// executor, a nil outcomeRepo, and an empty execution ID. These are
// the three short-circuit guards before the DB query.
func TestRetryCov_PriorShapeFailureCountGuards(t *testing.T) {
	var nilExec *Executor
	assert.Equal(t, 0, nilExec.priorShapeFailureCount(context.Background(), "e1"))

	e := &Executor{} // outcomeRepo nil
	assert.Equal(t, 0, e.priorShapeFailureCount(context.Background(), "e1"))

	e2 := &Executor{outcomeRepo: newStubStepOutcomeRepo()}
	assert.Equal(t, 0, e2.priorShapeFailureCount(context.Background(), ""), "empty execution ID is a guard")
}

// TestRetryCov_PriorShapeFailureCountListError — a List error is
// swallowed (returns 0) so the watchdog never blocks executions when
// the outcomes table is unreachable.
func TestRetryCov_PriorShapeFailureCountListError(t *testing.T) {
	e := &Executor{outcomeRepo: &retryCov_listErrOutcomeRepo{stubStepOutcomeRepo: newStubStepOutcomeRepo()}}
	assert.Equal(t, 0, e.priorShapeFailureCount(context.Background(), "e1"))
}

// TestRetryCov_PriorShapeFailureCountCountsShapeOutcomes — only
// schema_violation and parse_error rows for the execution count; other
// outcomes (and nil rows) are skipped. Drives both switch arms plus
// the default skip.
func TestRetryCov_PriorShapeFailureCountCountsShapeOutcomes(t *testing.T) {
	stub := newStubStepOutcomeRepo()
	now := time.Now()
	mustRecord := func(o string) {
		_ = stub.Record(context.Background(), &persistence.ExecutionStepOutcome{
			ExecutionID: "e1",
			StepID:      "s",
			Outcome:     o,
			RecordedAt:  now,
		})
	}
	mustRecord("schema_violation")
	mustRecord("parse_error")
	mustRecord("success")         // not a shape failure → skipped
	mustRecord("content_failure") // default arm → skipped

	e := &Executor{outcomeRepo: stub}
	assert.Equal(t, 2, e.priorShapeFailureCount(context.Background(), "e1"),
		"only schema_violation + parse_error count toward the shape loop guard")
}

// TestRetryCov_ExtractMissingKeysBlankInner — a match whose captured
// bracket contents are blank/whitespace returns nil (the
// trimmed-empty arm), so the caller falls back to the raw error text.
func TestRetryCov_ExtractMissingKeysBlankInner(t *testing.T) {
	err := errors.New(`schema violation: role "x" result.json is missing required keys: [   ]`)
	assert.Nil(t, extractMissingKeysFromError(err),
		"a whitespace-only key list must yield nil, not a slice of empties")
}

// TestRetryCov_ExtractMissingKeysCommaSeparated — tolerate the
// comma-separated formatter drift the parser is hardened against.
func TestRetryCov_ExtractMissingKeysCommaSeparated(t *testing.T) {
	err := errors.New(`is missing required keys: [alpha, beta, gamma]`)
	got := extractMissingKeysFromError(err)
	assert.Equal(t, []string{"alpha", "beta", "gamma"}, got)
}

// TestRetryCov_ExtractMissingKeysNilAndNoMatch — nil error and a
// non-matching message both yield nil.
func TestRetryCov_ExtractMissingKeysNilAndNoMatch(t *testing.T) {
	assert.Nil(t, extractMissingKeysFromError(nil))
	assert.Nil(t, extractMissingKeysFromError(errors.New("some unrelated error")))
}
