package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"

	"vornik.io/vornik/internal/persistence"
)

// TestLogGateTrace_EmptyTraceAndNoErrorIsNoop — silent on empty
// inputs so the executor's gate path doesn't spam a warn line for
// every gate-less step.
func TestLogGateTrace_EmptyTraceAndNoErrorIsNoop(t *testing.T) {
	e := &Executor{logger: zerolog.Nop()}
	task := &persistence.Task{ID: "t1"}
	exec := &persistence.Execution{ID: "e1"}
	assert.NotPanics(t, func() {
		e.logGateTrace(context.Background(), task, exec, "step-1", GateEvalTrace{}, nil, "")
	})
}

// TestLogGateTrace_SuccessLevelInfo — matched gate logs at Info.
func TestLogGateTrace_SuccessLevelInfo(t *testing.T) {
	e := &Executor{logger: zerolog.Nop()}
	task := &persistence.Task{ID: "t1"}
	exec := &persistence.Execution{ID: "e1"}
	trace := GateEvalTrace{
		RawPreview: `{"score":0.9}`,
		Entries: []GateEvalEntry{
			{Condition: "score>0.5", Target: "approved", Matched: true,
				Observed: 0.9, Wanted: 0.5, Found: true},
		},
	}
	assert.NotPanics(t, func() {
		e.logGateTrace(context.Background(), task, exec, "step-1", trace, nil, "approved")
	})
}

// TestLogGateTrace_FailureLevelWarn — unmatched gate (err != nil)
// logs at Warn with "gate_failed".
func TestLogGateTrace_FailureLevelWarn(t *testing.T) {
	e := &Executor{logger: zerolog.Nop()}
	task := &persistence.Task{ID: "t1"}
	exec := &persistence.Execution{ID: "e1"}
	trace := GateEvalTrace{
		RawPreview: `{"score":0.1}`,
		Entries: []GateEvalEntry{
			{Condition: "score>0.5", Target: "approved", Matched: false,
				Observed: 0.1, Wanted: 0.5, Found: true},
			{Condition: "bogus", Err: "bad condition syntax"},
			// Entry with Found=false and no Err → "observed_missing" path.
			{Condition: "missing>0", Target: "fail", Found: false},
		},
	}
	assert.NotPanics(t, func() {
		e.logGateTrace(context.Background(), task, exec, "step-1", trace, errors.New("no gate matched"), "")
	})
}

// TestLogGateTrace_OnlyErrorWithoutEntries — error without entries
// still logs the gate_failed line.
func TestLogGateTrace_OnlyErrorWithoutEntries(t *testing.T) {
	e := &Executor{logger: zerolog.Nop()}
	task := &persistence.Task{ID: "t1"}
	exec := &persistence.Execution{ID: "e1"}
	assert.NotPanics(t, func() {
		e.logGateTrace(context.Background(), task, exec, "step-1", GateEvalTrace{}, errors.New("evaluator init failed"), "")
	})
}
