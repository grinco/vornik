package ui

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
)

// signalsBlob builds a JSONB-encoded slice of detector findings
// matching the wire shape parseHallucinationSignalsForUI
// expects. Lets each test specify only the fields that matter
// (severity, detector) without redeclaring the full row shape.
func signalsBlob(specs ...struct{ severity, detector string }) []byte {
	type sig struct {
		Severity string `json:"severity"`
		Detector string `json:"detector"`
	}
	out := make([]sig, 0, len(specs))
	for _, s := range specs {
		out = append(out, sig{Severity: s.severity, Detector: s.detector})
	}
	b, _ := json.Marshal(out)
	return b
}

// outc is a tiny constructor for one ExecutionStepOutcome —
// keeps tests terse without hiding role/model/signals.
func outc(role, model string, at time.Time, sigs []byte) *persistence.ExecutionStepOutcome {
	return &persistence.ExecutionStepOutcome{
		Role:                 role,
		Model:                model,
		RecordedAt:           at,
		HallucinationSignals: sigs,
	}
}

// TestAggregatePhase1Cohorts_GroupsByRoleModel — same (role,
// model) outcomes collapse into one row with summed step
// counts + summed per-severity counts. The headline shape
// the new tile renders.
func TestAggregatePhase1Cohorts_GroupsByRoleModel(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	in := []*persistence.ExecutionStepOutcome{
		outc("strategist", "glm-5", now.Add(-1*time.Hour), signalsBlob(
			struct{ severity, detector string }{"high", "url_not_fetched"},
		)),
		outc("strategist", "glm-5", now.Add(-2*time.Hour), signalsBlob(
			struct{ severity, detector string }{"warn", "schema_leakage"},
			struct{ severity, detector string }{"warn", "url_not_fetched"},
		)),
		outc("risk-officer", "glm-5", now.Add(-3*time.Hour), signalsBlob(
			struct{ severity, detector string }{"info", "numeric_claim_mismatch"},
		)),
	}
	out := aggregatePhase1Cohorts(in)
	require.Len(t, out, 2)

	// strategist row first (high signal sorts ahead).
	str := out[0]
	assert.Equal(t, "strategist", str.Role)
	assert.Equal(t, 2, str.StepsAffected)
	assert.Equal(t, 1, str.SignalsHigh)
	assert.Equal(t, 2, str.SignalsWarn)
	assert.Equal(t, 0, str.SignalsInfo)
	assert.Equal(t, 2, str.SignalsByDetector["url_not_fetched"])
	assert.Equal(t, 1, str.SignalsByDetector["schema_leakage"])
	assert.Equal(t, now.Add(-1*time.Hour), str.LastRecordedAt)

	rsk := out[1]
	assert.Equal(t, "risk-officer", rsk.Role)
	assert.Equal(t, 1, rsk.StepsAffected)
	assert.Equal(t, 0, rsk.SignalsHigh)
	assert.Equal(t, 1, rsk.SignalsInfo)
}

// TestAggregatePhase1Cohorts_SkipsEmptySignalRows — rows
// without signals (the common case for healthy steps) must
// NOT pollute the aggregation. Cohorts that ran cleanly
// don't appear in the table at all.
func TestAggregatePhase1Cohorts_SkipsEmptySignalRows(t *testing.T) {
	now := time.Now().UTC()
	in := []*persistence.ExecutionStepOutcome{
		outc("strategist", "glm-5", now, nil),
		outc("strategist", "glm-5", now, []byte{}),
		outc("strategist", "glm-5", now, signalsBlob(
			struct{ severity, detector string }{"high", "x"},
		)),
	}
	out := aggregatePhase1Cohorts(in)
	require.Len(t, out, 1)
	assert.Equal(t, 1, out[0].StepsAffected, "only the row with non-empty signals counts")
	assert.Equal(t, 1, out[0].SignalsHigh)
}

// TestAggregatePhase1Cohorts_SortsByHighThenTotal — the
// rollup tile lists the loudest cohorts first: highest High
// count, then total, then role name. Matches what the
// operator's eye looks for — "which cohort is failing hard?".
func TestAggregatePhase1Cohorts_SortsByHighThenTotal(t *testing.T) {
	now := time.Now().UTC()
	in := []*persistence.ExecutionStepOutcome{
		// Cohort A: 5 high signals.
		outc("a", "m", now, signalsBlob(
			struct{ severity, detector string }{"high", "x"},
			struct{ severity, detector string }{"high", "x"},
			struct{ severity, detector string }{"high", "x"},
			struct{ severity, detector string }{"high", "x"},
			struct{ severity, detector string }{"high", "x"},
		)),
		// Cohort B: 1 high + 100 warn (loud but with high < A).
		outc("b", "m", now, signalsBlob(
			struct{ severity, detector string }{"high", "x"},
		)),
		outc("b", "m", now, signalsBlob(
			struct{ severity, detector string }{"warn", "x"},
		)),
		// Cohort C: 0 high, 5 warn.
		outc("c", "m", now, signalsBlob(
			struct{ severity, detector string }{"warn", "x"},
			struct{ severity, detector string }{"warn", "x"},
			struct{ severity, detector string }{"warn", "x"},
			struct{ severity, detector string }{"warn", "x"},
			struct{ severity, detector string }{"warn", "x"},
		)),
	}
	out := aggregatePhase1Cohorts(in)
	require.Len(t, out, 3)
	assert.Equal(t, "a", out[0].Role, "highest High count sorts first")
	assert.Equal(t, "b", out[1].Role, "1 high beats 0 high regardless of warn count")
	assert.Equal(t, "c", out[2].Role, "no high signals sorts last")
}

// TestAggregatePhase1Cohorts_EmptyInput — defensive: nil
// input returns nil, not an empty slice (the template uses
// `{{if .Phase1Cohorts}}` to gate rendering).
func TestAggregatePhase1Cohorts_EmptyInput(t *testing.T) {
	out := aggregatePhase1Cohorts(nil)
	assert.Nil(t, out)
}

// TestAggregatePhase1Cohorts_HandlesNilEntries — defensive
// against a corrupt nil row in the slice (slice resize race,
// log replay error). Must not panic.
func TestAggregatePhase1Cohorts_HandlesNilEntries(t *testing.T) {
	now := time.Now().UTC()
	in := []*persistence.ExecutionStepOutcome{
		nil,
		outc("r", "m", now, signalsBlob(
			struct{ severity, detector string }{"high", "x"},
		)),
		nil,
	}
	out := aggregatePhase1Cohorts(in)
	require.Len(t, out, 1)
	assert.Equal(t, 1, out[0].SignalsHigh)
}
