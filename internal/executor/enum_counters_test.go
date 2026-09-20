package executor

import (
	"encoding/json"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"vornik.io/vornik/internal/quality"
	"vornik.io/vornik/internal/registry"
)

// D6.4: the pair. A violation WITH an install says something on the provider
// side of the wire did not do what was asked; a violation WITHOUT one says the
// daemon never pinned and the bug is ours. Nothing WITHIN violation+install is
// distinguishable — provider-ignored, provider-coerced and a daemon validator
// false positive all land there identically, which is why the design says the
// pair isolates the never-pinned confound and stops.

func counterValue(t *testing.T, c *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	m := &dto.Metric{}
	counter, err := c.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("counter lookup %v: %v", labels, err)
	}
	if err := counter.Write(m); err != nil {
		t.Fatalf("counter write: %v", err)
	}
	return m.GetCounter().GetValue()
}

func TestEnumViolationCounterIncrementsOnAFatalViolation(t *testing.T) {
	// Test 56.
	e := &Executor{metrics: NewMetrics(prometheus.NewRegistry())}
	e.checkOutputContract([]byte(`{"testing":{"cases":[{"id":"case_1","status":"passed"}]}}`),
		"tester", nil, testerSchema("s1_case_1"))
	if got := counterValue(t, e.metrics.OutputEnumViolationTotal, "tester", "testing.cases[0].id"); got != 1 {
		t.Fatalf("violation counter = %v, want 1 — this is the provider-enforcement signal", got)
	}
}

func TestEnumViolationCounterAlsoCountsWarnMode(t *testing.T) {
	// A warn-mode violation does not fail the step, but it MUST still count:
	// a control that cannot distinguish "examined and clean" from "never
	// examined" reports the first and means the second.
	e := &Executor{metrics: NewMetrics(prometheus.NewRegistry())}
	schema := &registry.OutputSchema{
		Type: "object",
		Properties: map[string]*registry.OutputSchema{
			"analysis": {
				Type: "object",
				Properties: map[string]*registry.OutputSchema{
					"complexity": {Type: "string", Enum: []any{"trivial"}, OnViolation: "warn"},
				},
			},
		},
	}
	if msg := e.checkOutputContract([]byte(`{"analysis":{"complexity":"medium"}}`), "analyst", nil, schema); msg != "" {
		t.Fatalf("warn mode failed the step: %q", msg)
	}
	if got := counterValue(t, e.metrics.OutputEnumViolationTotal, "analyst", "analysis.complexity"); got != 1 {
		t.Fatalf("warn-mode violation counter = %v, want 1", got)
	}
}

func TestEnumViolationCounterDoesNotIncrementOnACleanResult(t *testing.T) {
	// Test 56a — the false-positive guard. A signal that fires on clean input
	// cannot indict anyone.
	e := &Executor{metrics: NewMetrics(prometheus.NewRegistry())}
	e.checkOutputContract([]byte(`{"testing":{"cases":[{"id":"s1_case_1","status":"passed"}]}}`),
		"tester", nil, testerSchema("s1_case_1"))
	if got := counterValue(t, e.metrics.OutputEnumViolationTotal, "tester", "testing.cases[0].id"); got != 0 {
		t.Fatalf("violation counter = %v on a conforming report, want 0", got)
	}
}

func pinnedPlanAndState(ids string) (*executionPlan, *executionState) {
	plan := &executionPlan{workflow: &registry.Workflow{
		QualityScoring: &quality.ScoringPolicy{
			Kind:         quality.ScoreKindPinnedCaseValidation,
			ProducerStep: "analyze",
			VerifierStep: "test",
		},
	}}
	state := &executionState{StepResults: map[string]json.RawMessage{
		"analyze": json.RawMessage(ids),
	}}
	return plan, state
}

func TestPinnedInstallCounterDistinguishesTheDaemonNeverPinnedCase(t *testing.T) {
	// Tests 56b and 56c together: the install counter is what separates
	// "provider side" from "our own pin never happened", and the two readings
	// are distinguishable in the recorded series.
	role := &registry.SwarmRole{Name: "tester", OutputSchema: testerSchema()}

	// Case A: producer evidence present -> pin installs.
	plan, state := pinnedPlanAndState(`{"analysis":{"test_case_ids":["s1_case_1"]}}`)
	optsA := &agentInputOpts{}
	applyRoleSchemaOpts(optsA, role)
	if n := applyPinnedCaseEnum(optsA, plan, "test", role, state); n != 1 {
		t.Fatalf("installed %d ids, want 1", n)
	}

	// Case B: producer evidence unreadable -> no pin, and the effective schema
	// keeps the role's declaration, which constrains no id.
	planB, stateB := pinnedPlanAndState(`{"analysis":{}}`)
	optsB := &agentInputOpts{}
	applyRoleSchemaOpts(optsB, role)
	if n := applyPinnedCaseEnum(optsB, planB, "test", role, stateB); n != 0 {
		t.Fatalf("installed %d ids on unreadable producer evidence, want 0", n)
	}

	// The distinguishing consequence: in case B a wrong id produces NO
	// violation at all, so violation-without-install is the readable state and
	// cannot be confused with the provider ignoring an enum that was emitted.
	bad := []byte(`{"testing":{"cases":[{"id":"invented","status":"passed"}]}}`)
	if got := validateEnums(bad, optsA.EffectiveSchema); len(got) != 1 {
		t.Fatalf("installed pin did not catch a wrong id: %+v", got)
	}
	if got := validateEnums(bad, optsB.EffectiveSchema); len(got) != 0 {
		t.Fatalf("un-pinned schema produced %d violations; the never-pinned case must be silent here "+
			"so the counter pair stays readable: %+v", len(got), got)
	}
}
