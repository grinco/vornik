package executor

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/hallucination"
	"vornik.io/vornik/internal/persistence"
)

// TestRunHallucinationDetector_NilDetector — when the detector
// isn't wired the helper returns (nil, "", nil) without
// inspecting the result. This is the production default for
// deployments that haven't opted into Phase 1 detection.
func TestRunHallucinationDetector_NilDetector(t *testing.T) {
	e := &Executor{logger: zerolog.Nop()}
	blob, detail, err := e.runHallucinationDetector(context.Background(),
		&persistence.Task{ID: "t"}, &persistence.Execution{ID: "x"}, "step",
		[]byte(`{"message":"some prose"}`))
	require.NoError(t, err)
	assert.Nil(t, blob)
	assert.Empty(t, detail)
}

// TestRunHallucinationDetector_EmptyResult — empty result bytes
// short-circuit before the detector runs. Used when a step
// produces no result.json (warm-pool error, container OOM
// before write).
func TestRunHallucinationDetector_EmptyResult(t *testing.T) {
	e := &Executor{
		hallucinationDetector: hallucination.NewDefault(),
		hallucinationMetrics:  hallucination.NewMetrics(prometheus.NewRegistry()),
		logger:                zerolog.Nop(),
	}
	blob, detail, err := e.runHallucinationDetector(context.Background(),
		&persistence.Task{ID: "t"}, &persistence.Execution{ID: "x"}, "step", nil)
	require.NoError(t, err)
	assert.Nil(t, blob)
	assert.Empty(t, detail)
}

// TestRunHallucinationDetector_NoProseFields — when the result
// is valid JSON but carries no recognised prose field
// (message / summary / output / final_answer), the detector
// degrades to no-op rather than scanning the whole envelope.
// This pins the 2026-04 fix that stopped scanning raw JSON
// to avoid double-escaped-string false positives.
func TestRunHallucinationDetector_NoProseFields(t *testing.T) {
	e := &Executor{
		hallucinationDetector: hallucination.NewDefault(),
		hallucinationMetrics:  hallucination.NewMetrics(prometheus.NewRegistry()),
		logger:                zerolog.Nop(),
	}
	blob, detail, err := e.runHallucinationDetector(context.Background(),
		&persistence.Task{ID: "t"}, &persistence.Execution{ID: "x"}, "step",
		[]byte(`{"status":"COMPLETED","extra":"nothing model-authored"}`))
	require.NoError(t, err)
	assert.Nil(t, blob)
	assert.Empty(t, detail)
}

// TestRunHallucinationDetector_WarnSignalsPersistedNotBlocking —
// using a custom Detector that fires warn-level signals (no High),
// the function persists the signal blob, returns no block error,
// and ObserveSignals fires for telemetry.
//
// We can't drive this with NewDefault() because crafting prose
// that triggers warn-only signals is brittle. Instead, build a
// Detector with a custom Rule that emits one Warn signal
// unconditionally — the test verifies the executor's handling
// of warn-tier output is correct, regardless of which rule
// fired it.
func TestRunHallucinationDetector_WarnLevelDoesNotBlock(t *testing.T) {
	warnRule := func(_ string, _ *hallucination.GroundingContext) []hallucination.Signal {
		return []hallucination.Signal{{
			Detector:   "test_warn_rule",
			Severity:   hallucination.SeverityWarn,
			ClaimType:  "url",
			ClaimValue: "https://example.com/x",
			Sentence:   "agent claimed something",
		}}
	}
	det := hallucination.New([]hallucination.Rule{warnRule})

	e := &Executor{
		hallucinationDetector: det,
		hallucinationMetrics:  hallucination.NewMetrics(prometheus.NewRegistry()),
		logger:                zerolog.Nop(),
	}
	blob, detail, err := e.runHallucinationDetector(context.Background(),
		&persistence.Task{ID: "t", ProjectID: "p"},
		&persistence.Execution{ID: "x"}, "step",
		[]byte(`{"message":"agent text with claim"}`))
	require.NoError(t, err, "warn-only signals must NOT fail the step")
	assert.Empty(t, detail, "warn path returns empty detail (block-level only sets it)")
	assert.NotNil(t, blob, "signals must be persisted as JSON for the outcome row")
	assert.Contains(t, string(blob), "test_warn_rule")
}

// TestRunHallucinationDetector_HighLevelBlocks — custom rule
// firing a High-severity signal triggers the block path: returns
// a non-nil error, persists the signal blob, builds a
// human-readable detail with the top claim(s) for the
// step_outcome row.
func TestRunHallucinationDetector_HighLevelBlocks(t *testing.T) {
	highRule := func(_ string, _ *hallucination.GroundingContext) []hallucination.Signal {
		return []hallucination.Signal{
			{
				Detector:   "test_high_rule",
				Severity:   hallucination.SeverityHigh,
				ClaimType:  "url",
				ClaimValue: "https://fictional.example/x",
				Sentence:   "claim sentence",
			},
		}
	}
	det := hallucination.New([]hallucination.Rule{highRule})

	e := &Executor{
		hallucinationDetector: det,
		hallucinationMetrics:  hallucination.NewMetrics(prometheus.NewRegistry()),
		logger:                zerolog.Nop(),
	}
	blob, detail, err := e.runHallucinationDetector(context.Background(),
		&persistence.Task{ID: "t", ProjectID: "p"},
		&persistence.Execution{ID: "x"}, "step",
		[]byte(`{"message":"agent text"}`))
	require.Error(t, err, "High-severity signals must fail the step")
	assert.NotEmpty(t, detail, "detail string must summarise top claims for the outcome row")
	assert.Contains(t, detail, "url=\"https://fictional.example/x\"",
		"detail must include the ClaimType=ClaimValue pair")
	assert.NotNil(t, blob, "signals blob must be persisted on the failing path")
}

// TestRunHallucinationDetector_NoSignals — prose field present
// but the detector returns no signals (e.g. plain greeting with
// no URLs, IDs, or numeric claims). Returns (nil, "", nil) —
// distinct from "warn-level signals recorded" which would
// return a blob.
func TestRunHallucinationDetector_NoSignals(t *testing.T) {
	e := &Executor{
		hallucinationDetector: hallucination.NewDefault(),
		hallucinationMetrics:  hallucination.NewMetrics(prometheus.NewRegistry()),
		// nil auditRepo + artifactRepo → BuildForStep returns an
		// empty grounding context; the default rules degrade
		// (no URL claims to compare against, etc).
		logger: zerolog.Nop(),
	}
	blob, detail, err := e.runHallucinationDetector(context.Background(),
		&persistence.Task{ID: "t", ProjectID: "p"},
		&persistence.Execution{ID: "x"}, "step",
		[]byte(`{"message":"Hello world. The task is complete."}`))
	require.NoError(t, err)
	assert.Nil(t, blob, "no signals means no blob")
	assert.Empty(t, detail)
}
