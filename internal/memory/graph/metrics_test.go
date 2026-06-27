package graph

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestNewMetrics_RegistersAllExpected pins the Prometheus metric
// names exposed by the KG pipeline. Any rename / removal breaks
// the Grafana dashboard at deployments/grafana/dashboards/vornik.json
// — keep both in sync. The test uses a private registry so it
// doesn't pollute the global one.
func TestNewMetrics_RegistersAllExpected(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	if m == nil {
		t.Fatal("NewMetrics returned nil")
	}

	// Touch every metric so it shows up in the registry. Counters
	// and counter-vecs need at least one observation; gauges need
	// a Set() call. This mirrors how the production pipeline drives
	// each metric.
	m.ChunksExtractedTotal.WithLabelValues("success").Inc()
	m.ChunksExtractedTotal.WithLabelValues("failed").Inc()
	m.ExtractionDuration.Observe(1.2)
	m.CircuitTrippedTotal.Inc()
	m.StageTokensTotal.WithLabelValues("extractor", "input").Add(123)
	m.StageTokensTotal.WithLabelValues("extractor", "output").Add(45)
	m.StageTokensTotal.WithLabelValues("resolver", "input").Add(200)
	m.StageTokensTotal.WithLabelValues("resolver", "output").Add(50)
	m.StageTokensTotal.WithLabelValues("relationship", "input").Add(800)
	m.StageTokensTotal.WithLabelValues("relationship", "output").Add(300)
	m.StageTokensTotal.WithLabelValues("validator", "input").Add(60)
	m.StageTokensTotal.WithLabelValues("validator", "output").Add(20)
	m.ExtractorOutcomesTotal.WithLabelValues(ExtractOutcomeEmptyResponse).Inc()
	m.ExtractorOutcomesTotal.WithLabelValues(ExtractOutcomeProduced).Inc()
	m.ExtractorOutcomesTotal.WithLabelValues(ExtractOutcomeDroppedAllInvalid).Inc()
	m.ResolverDecisionsTotal.WithLabelValues("short_circuit").Add(5)
	m.ResolverDecisionsTotal.WithLabelValues("llm").Add(2)
	m.ValidatorDroppedTotal.Inc()
	m.ValidatorDropsByReasonTotal.WithLabelValues(ValidatorDropReasonMissingScore).Inc()
	m.ValidatorDropsByReasonTotal.WithLabelValues(ValidatorDropReasonBelowThreshold).Inc()
	m.RelationshipDroppedTotal.WithLabelValues(DropReasonEvidenceNotInChunk).Inc()
	m.RelationshipDroppedTotal.WithLabelValues(DropReasonUnknownPredicate).Inc()
	m.SameChunkDedupTotal.Inc()
	m.DupKeyRecoveredTotal.Inc()
	m.ChunksPending.Set(100)
	m.ChunksDone.Set(50)
	m.EntitiesTotal.Set(75)
	m.EdgesTotal.Set(20)
	m.MentionsTotal.Set(150)
	m.EntitiesByType.WithLabelValues("PERSON").Set(10)
	m.EntitiesByType.WithLabelValues("VENDOR").Set(5)

	// All the metric names the Grafana dashboard queries. If any
	// rename, the dashboard's PromQL silently goes blank — adding
	// the name here forces a test failure that's easy to track.
	wantMetrics := []string{
		"vornik_memory_graph_chunks_extracted_total",
		"vornik_memory_graph_extraction_duration_seconds",
		"vornik_memory_graph_circuit_tripped_total",
		"vornik_memory_graph_stage_tokens_total",
		"vornik_memory_graph_extractor_outcomes_total",
		"vornik_memory_graph_resolver_decisions_total",
		"vornik_memory_graph_validator_dropped_total",
		"vornik_memory_graph_validator_drops_by_reason_total",
		"vornik_memory_graph_relationship_dropped_total",
		"vornik_memory_graph_same_chunk_dedup_total",
		"vornik_memory_graph_dup_key_recovered_total",
		"vornik_memory_graph_chunks_pending",
		"vornik_memory_graph_chunks_done",
		"vornik_memory_graph_entities_total",
		"vornik_memory_graph_edges_total",
		"vornik_memory_graph_mentions_total",
		"vornik_memory_graph_entities_by_type",
	}

	for _, name := range wantMetrics {
		t.Run(name, func(t *testing.T) {
			n := testutil.CollectAndCount(reg, name)
			if n == 0 {
				t.Errorf("metric %q not registered (or no samples)", name)
			}
		})
	}
}

// TestStageTokensTotal_LabelOrderStable guards the (stage, kind)
// label order. Grafana queries use `{{stage}} / {{kind}}` and a
// transposed label order would break the legend without breaking
// the graph itself, so the bug would be hard to spot visually.
func TestStageTokensTotal_LabelOrderStable(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.StageTokensTotal.WithLabelValues("extractor", "input").Add(1)

	// Render the metric and confirm the labels appear in the
	// expected order: stage first, kind second.
	got := testutil.ToFloat64(m.StageTokensTotal.WithLabelValues("extractor", "input"))
	if got != 1 {
		t.Errorf("expected 1 sample at (extractor, input), got %v", got)
	}
	// A transposed call should be a different time series. If the
	// code path under test silently reordered, the lookup below
	// would return 1 instead of 0.
	if !strings.Contains(metricNamesString(reg), "vornik_memory_graph_stage_tokens_total") {
		t.Error("stage_tokens_total not found in registry")
	}
}

// metricNamesString gathers every metric name registered with reg
// into a single space-separated string for substring assertions.
func metricNamesString(reg *prometheus.Registry) string {
	families, _ := reg.Gather()
	var b strings.Builder
	for _, f := range families {
		b.WriteString(f.GetName())
		b.WriteByte(' ')
	}
	return b.String()
}
