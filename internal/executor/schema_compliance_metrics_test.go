package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestClassifyStepOutcome_RoutesSchemaViolation — pre-fix, every
// non-timeout/cancel error landed in "failed", which meant schema
// violations (missing required keys) were invisible in the per-
// model outcome metrics. Verify the classifier now distinguishes
// them so the dashboard can attribute "lead emitting result.json
// without 'plan'" to GLM-5 specifically.
func TestClassifyStepOutcome_RoutesSchemaViolation(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"missing_required_keys", errors.New(`schema violation: role "lead" result.json is missing required keys: [plan]`), "schema_violation"},
		{"could_not_parse_plan", errors.New("could not parse plan from lead output: bad shape"), "parse_error"},
		{"invalid_json", errors.New("invalid JSON in agent output"), "parse_error"},
		{"generic_failure", errors.New("container exited with code 1"), "failed"},
		{"timeout_phrase", errors.New("container wait timeout: deadline exceeded"), "timeout"},
		{"happy_path", nil, "success"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyStepOutcome(ctx, tc.err)
			if got != tc.want {
				t.Errorf("classifyStepOutcome(%q) = %q, want %q", errMsg(tc.err), got, tc.want)
			}
		})
	}
}

func errMsg(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// TestRecordFinalOutcome_SchemaViolationDrivesGauge — the metrics
// pipeline must produce a non-zero ModelSchemaViolationRate for a
// (role, model) once a schema_violation outcome is recorded.
// Mirrors the existing parse-failure rate test pattern.
func TestRecordFinalOutcome_SchemaViolationDrivesGauge(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	// 8 outcomes: 2 schema_violation, 6 ok.
	m.RecordFinalOutcome("lead", "zai.glm-5", "schema_violation")
	m.RecordFinalOutcome("lead", "zai.glm-5", "schema_violation")
	for i := 0; i < 6; i++ {
		m.RecordFinalOutcome("lead", "zai.glm-5", "ok")
	}

	got := testutil.ToFloat64(m.ModelSchemaViolationRate.WithLabelValues("lead", "zai.glm-5"))
	want := 2.0 / 8.0
	if got != want {
		t.Errorf("ModelSchemaViolationRate = %v, want %v (2 schema_violation / 8 terminal)", got, want)
	}

	// Different model should be at zero — gauge cardinality must be
	// keyed on the (role, model) pair, not bleed across models.
	m.RecordFinalOutcome("lead", "moonshotai.kimi-k2.5", "ok")
	others := testutil.ToFloat64(m.ModelSchemaViolationRate.WithLabelValues("lead", "moonshotai.kimi-k2.5"))
	if others != 0 {
		t.Errorf("kimi rate must be 0 (no schema_violation recorded), got %v", others)
	}
}

// TestRecordShapeRetry_CountersIndependent — shape-retry total and
// recovered must increment independently per (role, model, kind),
// so the dashboard can compute a per-model salvage ratio:
// recovered / total. A model that responds well to corrective
// prompting will have a high ratio; a model that's structurally
// unable to follow the schema will have a low one.
func TestRecordShapeRetry_CountersIndependent(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	// 5 shape retries on GLM-5 (lead, schema_violation), 1 recovered.
	for i := 0; i < 5; i++ {
		m.RecordShapeRetry("lead", "zai.glm-5", "schema_violation")
	}
	m.RecordShapeRetryRecovered("lead", "zai.glm-5", "schema_violation")

	gotTotal := testutil.ToFloat64(m.ShapeRetryTotal.WithLabelValues("lead", "zai.glm-5", "schema_violation"))
	if gotTotal != 5 {
		t.Errorf("ShapeRetryTotal = %v, want 5", gotTotal)
	}
	gotRecovered := testutil.ToFloat64(m.ShapeRetryRecoveredTotal.WithLabelValues("lead", "zai.glm-5", "schema_violation"))
	if gotRecovered != 1 {
		t.Errorf("ShapeRetryRecoveredTotal = %v, want 1", gotRecovered)
	}

	// Different kind label must not bleed into the schema_violation
	// row — a model that parses-fails on plans is a different bug
	// from a model that misses required keys.
	parseRow := testutil.ToFloat64(m.ShapeRetryTotal.WithLabelValues("lead", "zai.glm-5", "parse_error"))
	if parseRow != 0 {
		t.Errorf("parse_error row must be 0, got %v", parseRow)
	}
}

// TestRecordShapeRetry_DefaultsKindToShapeFailure — defensive: an
// empty `kind` argument falls back to the lower-cardinality
// "shape_failure" bucket so a recorder bug doesn't silently drop
// the metric or pollute the label space.
func TestRecordShapeRetry_DefaultsKindToShapeFailure(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	m.RecordShapeRetry("coder", "qwen-coder", "")
	got := testutil.ToFloat64(m.ShapeRetryTotal.WithLabelValues("coder", "qwen-coder", "shape_failure"))
	if got != 1 {
		t.Errorf("empty kind must default to shape_failure, got %v", got)
	}
}

// TestRecordModelFallback_LabelTriple — the fallback counter is
// keyed by (role, primary_model, fallback_model) so dashboards
// can answer "which primary model needs replacing?" with a single
// PromQL aggregation. Verify the labels round-trip.
func TestRecordModelFallback_LabelTriple(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	m.RecordModelFallback("lead", "zai.glm-5", "moonshotai.kimi-k2.5")
	m.RecordModelFallback("lead", "zai.glm-5", "moonshotai.kimi-k2.5")
	got := testutil.ToFloat64(m.ModelFallbackTotal.WithLabelValues("lead", "zai.glm-5", "moonshotai.kimi-k2.5"))
	if got != 2 {
		t.Errorf("ModelFallbackTotal for the (lead, glm-5, kimi) triple = %v, want 2", got)
	}

	// Inverted triple (different fallback model) must be a different counter.
	other := testutil.ToFloat64(m.ModelFallbackTotal.WithLabelValues("lead", "zai.glm-5", "claude-haiku-4-5"))
	if other != 0 {
		t.Errorf("inverted fallback triple must be 0, got %v", other)
	}
}

// TestShapeFailureMetricKind_DerivesFromError — the kind label
// derivation is shared between the step-outcome classifier and
// the shape-retry metric so a single failure produces consistent
// labels across both. Pin the mapping here.
func TestShapeFailureMetricKind_DerivesFromError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		kind shapeFailureKind
		want string
	}{
		{"missing_keys", errors.New(`schema violation: role "lead" result.json is missing required keys: [plan]`), shapeFailureJSON, "schema_violation"},
		{"parse_plan", errors.New("could not parse plan from lead output"), shapeFailureJSON, "parse_error"},
		{"plausibility", errors.New("plausibility violation: empty feedback"), shapeFailurePlausibility, "plausibility"},
		{"nil_err", nil, shapeFailureJSON, "shape_failure"},
		{"unknown", errors.New("unrecognised failure"), shapeFailureJSON, "shape_failure"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shapeFailureMetricKind(tc.err, tc.kind)
			if got != tc.want {
				t.Errorf("shapeFailureMetricKind(%v, %v) = %q, want %q", tc.err, tc.kind, got, tc.want)
			}
		})
	}
}
